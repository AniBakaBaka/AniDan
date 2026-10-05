package job

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/AniBakaBaka/AniDan/internal/store"
	"github.com/robfig/cron/v3"
)

type execution struct {
	info            ExecutionMetadata
	manager         *Manager
	task            Task
	fn              Func
	ctx             context.Context
	cancel          context.CancelFunc
	pause           atomic.Pointer[chan struct{}]
	started         bool
	cancelRequested bool
	lastProgress    time.Time
	startedAt       time.Time
}

type progressUpdate struct {
	execution *execution
	progress  int
	message   string
}

type Manager struct {
	lifecycle         *lifecycleRuntime
	recoveryReview    bool
	progressUpdates   chan progressUpdate
	store             *store.Store
	mu                sync.Mutex
	cond              *sync.Cond
	ctx               context.Context
	cancel            context.CancelFunc
	workers           int
	capacity          int
	executions        map[string]*execution
	queue             []string
	handlers          map[string]Handler
	queuesPaused      map[string]bool
	paused            bool
	closed            bool
	location          *time.Location
	wg                sync.WaitGroup
	closeOnce         sync.Once
	closeDone         chan struct{}
	heartbeatDone     chan struct{}
	owner, leaseValue string
	lastErr           error
	scheduler         *cron.Cron
	scheduleEntries   map[string]cron.EntryID
	scheduleErrors    map[string]string
}

func New(ctx context.Context, s *store.Store, workers, queue int) (*Manager, error) {
	return NewWithTimezone(ctx, s, workers, queue, "UTC")
}

// NewWithTimezone sets the legacy wall-clock timezone before restart recovery.
func NewWithTimezone(ctx context.Context, s *store.Store, workers, queue int, timezone string) (*Manager, error) {
	location, err := time.LoadLocation(timezone)
	if err != nil {
		return nil, err
	}

	if ctx == nil || s == nil || s.DB == nil {
		return nil, errors.New("context and initialized store are required")
	}
	if workers < 1 || workers > 256 || queue < 1 || queue > 100000 {
		return nil, errors.New("workers must be 1..256 and queue capacity 1..100000")
	}
	c, cancel := context.WithCancel(ctx)
	m := &Manager{lifecycle: newLifecycleRuntime(), progressUpdates: make(chan progressUpdate, workers*4), store: s, ctx: c, cancel: cancel, workers: workers, capacity: queue, executions: make(map[string]*execution), handlers: make(map[string]Handler), queuesPaused: make(map[string]bool), location: location, closeDone: make(chan struct{}), heartbeatDone: make(chan struct{}), scheduleEntries: make(map[string]cron.EntryID), scheduleErrors: make(map[string]string)}
	m.cond = sync.NewCond(&m.mu)
	if err := m.acquireLease(ctx); err != nil {
		cancel()
		return nil, err
	}
	if err := m.loadRecoveryReview(ctx); err != nil {
		_ = m.releaseLease(context.Background())
		cancel()
		return nil, err
	}
	if err := m.recoverInterrupted(ctx); err != nil {
		_ = m.releaseLease(context.Background())
		cancel()
		return nil, err
	}
	for i := 0; i < workers; i++ {
		m.wg.Add(1)
		go m.worker()
	}
	m.wg.Add(2)
	go m.lifecycleLoop()
	go m.progressLoop()
	go m.heartbeat()
	go func() {
		select {
		case <-ctx.Done():
			m.beginClose()
		case <-m.closeDone:
		}
	}()
	return m, nil
}

func (m *Manager) Register(kind string, h Handler) error {
	if strings.TrimSpace(kind) == "" || len(kind) > 500 || h == nil {
		return errors.New("valid job kind and handler are required")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return ErrClosed
	}
	if _, exists := m.handlers[kind]; exists {
		return fmt.Errorf("handler %q already registered", kind)
	}
	m.handlers[kind] = h
	if err := m.refillLocked(); err != nil {
		m.lastErr = err
		return err
	}
	m.cond.Broadcast()
	return nil
}
func (m *Manager) HandlerKinds() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	a := make([]string, 0, len(m.handlers))
	for k := range m.handlers {
		a = append(a, k)
	}
	sort.Strings(a)
	return a
}

func (m *Manager) Submit(kind string, params any, fn func(context.Context, func(int, string)) (any, error)) (string, error) {
	return m.SubmitWithOptions(kind, params, fn, SubmitOptions{})
}
func (m *Manager) SubmitRegistered(kind string, params any) (string, error) {
	return m.SubmitWithOptions(kind, params, nil, SubmitOptions{})
}
func (m *Manager) SubmitWithOptions(kind string, params any, fn Func, opts SubmitOptions) (string, error) {
	raw, err := marshalTaskParams(kind, params)
	if err != nil {
		return "", err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.submitLocked(kind, raw, fn, opts, nil)
}
func marshalTaskParams(kind string, params any) (json.RawMessage, error) {
	raw, err := json.Marshal(params)
	if err != nil {
		return nil, err
	}
	if len(raw) > MaxParamsBytes {
		return nil, errors.New("task parameters exceed 1 MiB")
	}
	if strings.TrimSpace(kind) == "" || len(kind) > 500 {
		return nil, errors.New("invalid job kind")
	}
	return raw, nil
}
func (m *Manager) submitLocked(kind string, raw json.RawMessage, fn Func, opts SubmitOptions, schedule *ScheduledTask) (string, error) {
	return m.submitLinkedLocked(m.ctx, kind, raw, fn, opts, schedule, nil)
}
func (m *Manager) submitLinkedLocked(caller context.Context, kind string, raw json.RawMessage, fn Func, opts SubmitOptions, schedule *ScheduledTask, parent *execution) (string, error) {
	return m.submitAdmissionLocked(caller, kind, raw, fn, opts, schedule, parent, nil, nil)
}

func (m *Manager) submitAdmissionLocked(caller context.Context, kind string, raw json.RawMessage, fn Func, opts SubmitOptions, schedule *ScheduledTask, parent *execution, receipt *admissionReceipt, ancestry *ParentReference) (string, error) {
	if m.closed {
		return "", ErrClosed
	}
	if m.ctx.Err() != nil {
		return "", m.ctx.Err()
	}
	if err := caller.Err(); err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(caller, 5*time.Second)
	defer cancel()
	var tx *sql.Tx
	var err error
	if receipt != nil {
		tx, err = m.store.DB.BeginTx(ctx, nil)
		if err != nil {
			return "", err
		}
		defer tx.Rollback()
		id, duplicate, err := m.readAdmissionReceipt(ctx, tx, receipt.key)
		if err != nil {
			return "", err
		}
		if duplicate {
			receipt.duplicate = true
			return id, nil
		}
	}
	if m.queuedLocked() >= m.capacity {
		return "", ErrQueueFull
	}
	if fn == nil {
		h, ok := m.handlers[kind]
		if !ok {
			return "", ErrUnknownHandler
		}
		params := append(json.RawMessage(nil), raw...)
		fn = func(ctx context.Context, p func(int, string)) (any, error) { return h(ctx, params, p) }
	}
	if opts.Title == "" {
		opts.Title = kind
	}
	if opts.QueueType == "" {
		opts.QueueType = "download"
	}
	if len(opts.Title) > 500 || len(opts.QueueType) > 500 || len(opts.UniqueKey) > 500 {
		return "", errors.New("task title, queue type or unique key is too long")
	}
	id, err := newID()
	if err != nil {
		return "", err
	}
	now := time.Now().In(m.location)
	t := Task{ID: id, Title: opts.Title, Kind: kind, Status: Pending, Description: "Queued", QueueType: opts.QueueType, Params: append(json.RawMessage(nil), raw...), CreatedAt: now, UpdatedAt: now, ScheduledTaskID: opts.ScheduledTaskID, UniqueKey: opts.UniqueKey, IsSystemTask: strings.HasPrefix(opts.ScheduledTaskID, "system_")}
	if tx == nil {
		tx, err = m.store.DB.BeginTx(ctx, nil)
		if err != nil {
			return "", err
		}
		defer tx.Rollback()
	}
	if opts.UniqueKey != "" {
		var count int
		if err = tx.QueryRowContext(ctx, m.bind("SELECT COUNT(*) FROM task_history WHERE unique_key = ? AND status IN (?, ?, ?)"), opts.UniqueKey, Pending, Running, Paused).Scan(&count); err != nil {
			return "", err
		}
		if count > 0 {
			return "", ErrAlreadyRunning
		}
	}
	if parent != nil {
		t.Parent = &ParentReference{TaskID: parent.task.ID, Kind: parent.task.Kind, ScheduledTaskID: parent.task.ScheduledTaskID}
	} else {
		t.Parent = cloneParentReference(ancestry)
	}
	if err = m.insertTask(ctx, tx, t); err != nil {
		return "", err
	}
	if t.Parent != nil {
		if err = m.writeParentReference(ctx, tx, t.ID, t.Parent); err != nil {
			return "", err
		}
	}
	if receipt != nil {
		if err = m.writeAdmissionReceipt(ctx, tx, receipt, id); err != nil {
			return "", err
		}
	}
	if schedule != nil {
		var next any
		if schedule.NextRunAt != nil {
			next = m.dbTime(*schedule.NextRunAt)
		}
		if _, err = tx.ExecContext(ctx, m.bind("UPDATE scheduled_tasks SET last_run_at = ?, next_run_at = ? WHERE id = ?"), m.dbTime(now), next, schedule.ID); err != nil {
			return "", err
		}
	}
	var parentResult json.RawMessage
	if parent != nil {
		parentResult, err = json.Marshal(map[string]string{"executionTaskId": id})
		if err != nil {
			return "", err
		}
		if err = m.putConfig(ctx, tx, resultPrefix+parent.task.ID, string(parentResult)); err != nil {
			return "", err
		}
	}
	if err = tx.Commit(); err != nil {
		return "", err
	}
	if parent != nil {
		parent.task.Result = parentResult
	}
	ectx, ecancel := context.WithCancel(m.ctx)
	e := &execution{info: executionMetadata(t), manager: m, task: t, fn: fn, ctx: ectx, cancel: ecancel}
	e.ctx = context.WithValue(e.ctx, checkpointKey{}, e)
	m.executions[id] = e
	m.queue = append(m.queue, id)
	m.cond.Broadcast()
	return id, nil
}

func (m *Manager) queuedLocked() int {
	n := 0
	for _, e := range m.executions {
		if !e.started {
			n++
		}
	}
	return n
}
func (m *Manager) refillLocked() error {
	if m.closed || m.recoveryReview || m.ctx.Err() != nil || len(m.handlers) == 0 {
		return nil
	}
	spare := m.capacity - m.queuedLocked()
	if spare <= 0 {
		return nil
	}
	kinds := make([]string, 0, len(m.handlers))
	for k := range m.handlers {
		kinds = append(kinds, k)
	}
	sort.Strings(kinds)
	qs := make([]string, len(kinds))
	args := make([]any, 0, len(kinds)+2)
	args = append(args, Pending)
	for i, k := range kinds {
		qs[i] = "?"
		args = append(args, k)
	}
	args = append(args, m.capacity+m.workers)
	ctx, cancel := context.WithTimeout(m.ctx, 5*time.Second)
	defer cancel()
	rows, err := m.store.DB.QueryContext(ctx, m.bind("SELECT "+taskColumns+" FROM task_history WHERE status = ? AND task_type IN ("+strings.Join(qs, ",")+") ORDER BY created_at, id LIMIT ?"), args...)
	if err != nil {
		return err
	}
	var tasks []Task
	for rows.Next() {
		t, e := m.scanTask(rows)
		if e != nil {
			rows.Close()
			return e
		}
		tasks = append(tasks, t)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, t := range tasks {
		if spare == 0 {
			break
		}
		if _, ok := m.executions[t.ID]; ok {
			continue
		}
		t.Parent, err = m.readParentReference(ctx, t.ID)
		if err != nil {
			return err
		}
		if len(t.Params) > MaxParamsBytes || !json.Valid(t.Params) {
			if err = m.setStatus(ctx, t.ID, Failed, "Invalid persisted parameters; explicit repair required", t.Progress, true); err != nil {
				return err
			}
			continue
		}
		h := m.handlers[t.Kind]
		params := append(json.RawMessage(nil), t.Params...)
		ectx, ecancel := context.WithCancel(m.ctx)
		e := &execution{info: executionMetadata(t), manager: m, task: t, ctx: ectx, cancel: ecancel, fn: func(ctx context.Context, p func(int, string)) (any, error) { return h(ctx, params, p) }}
		e.ctx = context.WithValue(e.ctx, checkpointKey{}, e)
		m.executions[t.ID] = e
		m.queue = append(m.queue, t.ID)
		spare--
	}
	return nil
}

func (m *Manager) worker() {
	defer m.wg.Done()
	for {
		m.mu.Lock()
		var e *execution
		for e == nil && !m.closed {
			if !m.paused {
				for i, id := range m.queue {
					candidate := m.executions[id]
					if candidate == nil {
						continue
					}
					if candidate.task.Status != Pending || m.queuesPaused[candidate.task.QueueType] {
						continue
					}
					e = candidate
					m.queue = append(m.queue[:i], m.queue[i+1:]...)
					break
				}
			}
			if e == nil {
				m.cond.Wait()
			}
		}
		if m.closed {
			m.mu.Unlock()
			return
		}
		ctx, cancel := context.WithTimeout(m.ctx, 5*time.Second)
		startedAt := time.Now().In(m.location)
		res, err := m.store.DB.ExecContext(ctx, m.bind("UPDATE task_history SET status = ?, description = ?, updated_at = ? WHERE id = ? AND status = ?"), Running, "Running", m.dbTime(startedAt), e.task.ID, Pending)
		cancel()
		var n int64
		if err == nil {
			n, err = res.RowsAffected()
		}
		if err != nil || n != 1 {
			if err != nil && m.ctx.Err() == nil {
				m.lastErr = fmt.Errorf("claim task: %w", err)
			}
			delete(m.executions, e.task.ID)
			e.cancel()
			m.mu.Unlock()
			continue
		}
		e.started = true
		e.startedAt = time.Now()
		e.task.Status = Running
		e.task.Description = "Running"
		e.task.UpdatedAt = startedAt
		m.queueLifecycle(LifecycleEvent{Phase: LifecycleStarted, Task: e.task, OccurredAt: startedAt})
		if err = m.refillLocked(); err != nil && m.ctx.Err() == nil {
			m.lastErr = err
		}
		m.cond.Broadcast()
		m.mu.Unlock()
		result, runErr := invoke(e)
		runDuration := time.Since(e.startedAt)
		m.mu.Lock()
		status, description := Completed, "Task completed"
		if runErr != nil {
			status = Failed
			description = truncate(runErr.Error(), 16384)
		}
		cancelled := e.ctx.Err() != nil || e.cancelRequested || m.closed
		if cancelled {
			status = Failed
			description = "Task cancelled; partial effects may exist"
			if m.closed {
				description = "Interrupted by shutdown; inspect effects before retrying"
			}
		}
		var encoded json.RawMessage
		_, diagnostic := result.(DiagnosticResult)
		publish := status == Completed || (diagnostic && runErr != nil && !cancelled && !errors.Is(runErr, context.Canceled) && !errors.Is(runErr, context.DeadlineExceeded))
		if publish && result != nil {
			encoded, err = json.Marshal(result)
			if err != nil || len(encoded) > MaxResultBytes {
				status = Failed
				description = "Task returned an invalid or oversized result"
				encoded = nil
			}
		}
		encoded, err = preserveChildResult(e.task.Result, encoded)
		if err != nil || len(encoded) > MaxResultBytes {
			status = Failed
			description = "Task returned an invalid or oversized linked result"
			encoded = e.task.Result
		}
		fctx, fcancel := context.WithTimeout(context.Background(), 5*time.Second)
		finishedAt := time.Now().In(m.location)
		finishedTask, persisted, err := m.finish(fctx, e.task, status, description, encoded, finishedAt)
		fcancel()
		if err != nil {
			m.lastErr = fmt.Errorf("persist terminal task %s: %w", e.task.ID, err)
		}
		if persisted {
			e.task = finishedTask
			m.queueLifecycle(LifecycleEvent{Phase: LifecycleFinished, Task: e.task, Cancelled: cancelled, OccurredAt: finishedAt})
		}
		e.cancel()
		delete(m.executions, e.task.ID)
		if err = m.refillLocked(); err != nil && m.ctx.Err() == nil {
			m.lastErr = err
		}
		m.cond.Broadcast()
		m.mu.Unlock()
		if persisted {
			metricCtx, metricCancel := context.WithTimeout(context.Background(), 2*time.Second)
			metricErr := m.recordExecutionMetric(metricCtx, e.task.ID, e.task.Kind, runDuration, status, description)
			metricCancel()
			if metricErr != nil {
				m.rememberError(fmt.Errorf("task execution telemetry: %w", metricErr))
			}
		}
	}
}
func invoke(e *execution) (result any, err error) {
	defer func() {
		if p := recover(); p != nil {
			result = nil
			err = fmt.Errorf("task panicked: %v", p)
		}
	}()
	if err = Checkpoint(e.ctx); err != nil {
		return nil, err
	}
	return e.fn(e.ctx, func(n int, s string) { e.manager.progress(e, n, s) })
}
func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		return string(r[:n])
	}
	return s
}
func (e *execution) unpause() {
	if gate := e.pause.Swap(nil); gate != nil {
		close(*gate)
	}
}
func (m *Manager) progress(e *execution, n int, message string) {
	if err := Checkpoint(e.ctx); err != nil {
		return
	}
	// Reporting must never acquire the manager mutex or a database connection:
	// handlers may call it while holding the only SQLite transaction connection.
	update := progressUpdate{e, n, truncate(message, 16384)}
	select {
	case m.progressUpdates <- update:
	default:
	}
}
func (m *Manager) progressLoop() {
	defer m.wg.Done()
	for {
		select {
		case <-m.ctx.Done():
			return
		case update := <-m.progressUpdates:
			e, n, message := update.execution, update.progress, update.message
			m.mu.Lock()
			if m.closed || e.cancelRequested || m.executions[e.task.ID] != e {
				m.mu.Unlock()
				continue
			}
			if n < 0 {
				n = 0
			}
			if n > 99 {
				n = 99
			}
			if n < e.task.Progress {
				n = e.task.Progress
			}
			if time.Since(e.lastProgress) < 100*time.Millisecond && n == e.task.Progress {
				m.mu.Unlock()
				continue
			}
			at := time.Now().In(m.location)
			now := m.dbTime(at)
			id := e.task.ID
			m.mu.Unlock()
			ctx, cancel := context.WithTimeout(e.ctx, 5*time.Second)
			// Never change status here: a delayed update must not resurrect a completed,
			// cancelled or paused task, nor race a lifecycle transition.
			res, err := m.store.DB.ExecContext(ctx, m.bind("UPDATE task_history SET progress = ?, description = ?, updated_at = ? WHERE id = ? AND status IN (?, ?)"), n, message, now, id, Running, Paused)
			cancel()
			var affected int64
			if err == nil {
				affected, err = res.RowsAffected()
			}
			m.mu.Lock()
			if err != nil && e.ctx.Err() == nil {
				m.lastErr = fmt.Errorf("persist task progress: %w", err)
			}
			// SQL was deliberately outside m.mu. A finish may have committed while
			// it ran; only a still-live execution can publish a progress snapshot.
			if err == nil && affected == 1 && !m.closed && !e.cancelRequested && m.executions[id] == e {
				e.task.Progress = n
				e.task.Description = message
				e.lastProgress = at
				e.task.UpdatedAt = at
				m.queueLifecycle(LifecycleEvent{Phase: LifecycleProgress, Task: e.task, OccurredAt: at})
			}
			m.mu.Unlock()
		}
	}
}

func (m *Manager) Get(id string) (Task, bool) {
	t, err := m.GetContext(context.Background(), id)
	if err != nil {
		m.rememberError(err)
		return Task{}, false
	}
	return t, true
}
func (m *Manager) GetContext(ctx context.Context, id string) (Task, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	m.mu.Lock()
	reader := &Manager{location: m.location, recoveryReview: m.recoveryReview}
	m.mu.Unlock()
	t, err := reader.scanTask(m.store.DB.QueryRowContext(ctx, m.bind("SELECT "+taskColumns+" FROM task_history WHERE id = ?"), id))
	if errors.Is(err, sql.ErrNoRows) {
		return t, ErrNotFound
	}
	if err != nil {
		return t, err
	}
	var result string
	err = m.store.DB.QueryRowContext(ctx, m.bind("SELECT config_value FROM config WHERE config_key = ?"), resultPrefix+id).Scan(&result)
	if err == nil {
		t.Result = json.RawMessage(result)
	} else if !errors.Is(err, sql.ErrNoRows) {
		return t, err
	}
	t.Parent, err = m.readParentReference(ctx, id)
	return t, err
}
func (m *Manager) List() []Task {
	ts, err := m.ListContext(context.Background(), MaxListItems, 0)
	if err != nil {
		m.rememberError(err)
	}
	return ts
}
func (m *Manager) ListContext(ctx context.Context, limit, offset int) ([]Task, error) {
	if limit < 1 || limit > MaxListItems || offset < 0 {
		return nil, errors.New("invalid task page")
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	m.mu.Lock()
	reader := &Manager{location: m.location, recoveryReview: m.recoveryReview}
	m.mu.Unlock()
	rows, err := m.store.DB.QueryContext(ctx, m.bind("SELECT "+taskColumns+" FROM task_history ORDER BY created_at DESC, id DESC LIMIT ? OFFSET ?"), limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	tasks := make([]Task, 0)
	for rows.Next() {
		t, e := reader.scanTask(rows)
		if e != nil {
			return nil, e
		}
		tasks = append(tasks, t)
	}
	return tasks, rows.Err()
}
func (m *Manager) rememberError(err error) {
	if err == nil || errors.Is(err, ErrNotFound) {
		return
	}
	m.mu.Lock()
	m.lastErr = err
	m.mu.Unlock()
}
func (m *Manager) LastError() error { m.mu.Lock(); defer m.mu.Unlock(); return m.lastErr }
func (m *Manager) Stats() Stats {
	m.mu.Lock()
	defer m.mu.Unlock()
	q := m.queuedLocked()
	return Stats{RecoveryReviewRequired: m.recoveryReview, Workers: m.workers, QueueCapacity: m.capacity, Queued: q, Running: len(m.executions) - q, Paused: m.paused, Closed: m.closed}
}

func (m *Manager) Cancel(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return ErrClosed
	}
	ctx, cancel := context.WithTimeout(m.ctx, 5*time.Second)
	defer cancel()
	e := m.executions[id]
	if e != nil && e.started {
		if e.cancelRequested {
			return nil
		}
		e.cancelRequested = true
		e.cancel()
		e.unpause()
		if err := m.setStatus(ctx, id, e.task.Status, "Cancellation requested; waiting for handler", e.task.Progress, false); err != nil {
			return err
		}
		return nil
	}
	t, err := m.scanTask(m.store.DB.QueryRowContext(ctx, m.bind("SELECT "+taskColumns+" FROM task_history WHERE id = ?"), id))
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if Terminal(t.Status) {
		return nil
	}
	if t.Status != Pending && t.Status != Paused {
		return ErrInvalidState
	}
	t.Parent, err = m.readParentReference(ctx, id)
	if err != nil {
		return err
	}
	tx, err := m.store.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	cancelledAt := time.Now().In(m.location)
	res, err := tx.ExecContext(ctx, m.bind("UPDATE task_history SET status = ?, description = ?, updated_at = ?, finished_at = ? WHERE id = ? AND status IN (?, ?)"), Failed, "Task cancelled before execution", m.dbTime(cancelledAt), m.dbTime(cancelledAt), id, Pending, Paused)
	if err != nil {
		return err
	}
	affected, err := res.RowsAffected()
	if err != nil || affected != 1 {
		return err
	}
	if _, err = tx.ExecContext(ctx, m.bind("DELETE FROM task_state_cache WHERE task_id = ?"), id); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	t.Status = Failed
	t.Description = "Task cancelled before execution"
	t.UpdatedAt = cancelledAt
	t.FinishedAt = &cancelledAt
	m.queueLifecycle(LifecycleEvent{Phase: LifecycleFinished, Task: t, Cancelled: true, OccurredAt: cancelledAt})
	if e != nil {
		e.cancel()
		delete(m.executions, id)
	}
	m.removeQueuedLocked(id)
	if err = m.refillLocked(); err != nil {
		m.lastErr = err
	}
	m.cond.Broadcast()
	return nil
}
func (m *Manager) removeQueuedLocked(id string) {
	for i, x := range m.queue {
		if x == id {
			m.queue = append(m.queue[:i], m.queue[i+1:]...)
			return
		}
	}
}
func (m *Manager) Pause(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return ErrClosed
	}
	e := m.executions[id]
	if e == nil {
		return ErrNotFound
	}
	if e.cancelRequested {
		return ErrInvalidState
	}
	if e.task.Status == Paused {
		return nil
	}
	if e.task.Status != Pending && e.task.Status != Running {
		return ErrInvalidState
	}
	ctx, cancel := context.WithTimeout(m.ctx, 5*time.Second)
	defer cancel()
	if err := m.setStatus(ctx, id, Paused, "Paused; running handlers stop at next checkpoint", e.task.Progress, false); err != nil {
		return err
	}
	e.task.Status = Paused
	gate := make(chan struct{})
	e.pause.Store(&gate)
	return nil
}
func (m *Manager) Resume(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return ErrClosed
	}
	e := m.executions[id]
	if e != nil {
		if e.task.Status != Paused || e.cancelRequested {
			return ErrInvalidState
		}
		status := Pending
		if e.started {
			status = Running
		}
		ctx, cancel := context.WithTimeout(m.ctx, 5*time.Second)
		defer cancel()
		e.unpause()
		e.task.Status = status
		if err := m.setStatus(ctx, id, status, "Resumed", e.task.Progress, false); err != nil {
			return err
		}
		m.cond.Broadcast()
		return nil
	}
	// A persisted paused task can have executed effects before a crash. Explicit
	// Retry is required, rather than silently resuming it from the beginning.
	return fmt.Errorf("%w: paused task has no live execution; inspect and explicitly retry", ErrInvalidState)
}
func (m *Manager) Retry(id string) (string, error) {
	t, err := m.GetContext(context.Background(), id)
	if err != nil {
		return "", err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, live := m.executions[id]; live {
		return "", ErrNotRetryable
	}
	if t.Status == Pending && m.recoveryReview {
		if m.closed {
			return "", ErrClosed
		}
		if m.queuedLocked() >= m.capacity {
			return "", ErrQueueFull
		}
		h, ok := m.handlers[t.Kind]
		if !ok {
			return "", ErrUnknownHandler
		}
		if !json.Valid(t.Params) || len(t.Params) > MaxParamsBytes {
			return "", ErrNotRetryable
		}
		params := append(json.RawMessage(nil), t.Params...)
		ectx, cancel := context.WithCancel(m.ctx)
		e := &execution{info: executionMetadata(t), manager: m, task: t, ctx: ectx, cancel: cancel, fn: func(ctx context.Context, p func(int, string)) (any, error) { return h(ctx, params, p) }}
		e.ctx = context.WithValue(e.ctx, checkpointKey{}, e)
		m.executions[t.ID] = e
		m.queue = append(m.queue, t.ID)
		m.cond.Broadcast()
		return t.ID, nil
	}
	if !Terminal(t.Status) && t.Status != Paused {
		return "", ErrNotRetryable
	}
	if _, ok := m.handlers[t.Kind]; !ok {
		return "", ErrUnknownHandler
	}
	if !json.Valid(t.Params) || len(t.Params) > MaxParamsBytes {
		return "", ErrNotRetryable
	}
	unique := t.UniqueKey
	if unique == "" {
		unique = "retry:" + id
	}
	return m.submitAdmissionLocked(m.ctx, t.Kind, t.Params, nil, SubmitOptions{Title: t.Title, QueueType: t.QueueType, UniqueKey: unique, ScheduledTaskID: t.ScheduledTaskID}, nil, nil, nil, t.Parent)
}

// PauseQueue/ResumeQueue with no argument affect all queues; an argument affects
// one named logical queue. Running jobs are not suspended by a queue pause.
func (m *Manager) PauseQueue(names ...string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(names) == 0 {
		m.paused = true
	} else {
		for _, n := range names {
			m.queuesPaused[n] = true
		}
	}
}
func (m *Manager) ResumeQueue(names ...string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(names) == 0 {
		m.paused = false
	} else {
		for _, n := range names {
			delete(m.queuesPaused, n)
		}
	}
	m.cond.Broadcast()
}

func (m *Manager) heartbeat() {
	defer close(m.heartbeatDone)
	ticker := time.NewTicker(leaseDuration / 3)
	defer ticker.Stop()
	for {
		select {
		case <-m.closeDone:
			return
		case <-ticker.C:
			m.mu.Lock()
			select {
			case <-m.closeDone:
				m.mu.Unlock()
				return
			default:
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			err := m.renewLease(ctx)
			cancel()
			if err != nil {
				m.lastErr = fmt.Errorf("job manager lease lost: %w", err)
			} else if !m.closed {
				if refillErr := m.refillLocked(); refillErr != nil && m.ctx.Err() == nil {
					m.lastErr = refillErr
				}
				m.cond.Broadcast()
			}
			m.mu.Unlock()
			if err != nil {
				m.beginClose()
				return
			}
		}
	}
}
func (m *Manager) beginClose() {
	m.closeOnce.Do(func() {
		// Signal contexts before acquiring a mutex which may be waiting on SQL.
		m.cancel()
		m.mu.Lock()
		m.closed = true
		m.cancel()
		for _, e := range m.executions {
			e.cancel()
			e.unpause()
		}
		m.cond.Broadcast()
		var stop context.Context
		if m.scheduler != nil {
			stop = m.scheduler.Stop()
		}
		m.mu.Unlock()
		go func() {
			if stop != nil {
				<-stop.Done()
			}
			m.wg.Wait()
			m.mu.Lock()
			select {
			case <-m.closeDone:
				m.mu.Unlock()
				return
			default:
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			err := m.releaseLease(ctx)
			cancel()
			if err != nil {
				m.lastErr = err
			}
			close(m.closeDone)
			m.mu.Unlock()
		}()
	})
}
func (m *Manager) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return m.CloseContext(ctx)
}
func (m *Manager) CloseContext(ctx context.Context) error {
	m.beginClose()
	select {
	case <-m.closeDone:
		return m.LastError()
	case <-ctx.Done():
		return fmt.Errorf("job handlers or lifecycle observer did not stop cooperatively: %w", ctx.Err())
	}
}
