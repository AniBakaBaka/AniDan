package job

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/robfig/cron/v3"
)

var cronParser = cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)

const scheduleColumns = "id, name, job_type, cron_expression, is_enabled, task_config, last_run_at, next_run_at"

func (m *Manager) scanSchedule(row scanner) (ScheduledTask, error) {
	var s ScheduledTask
	var raw sql.NullString
	var enabled, last, next any
	if err := row.Scan(&s.ID, &s.Name, &s.Kind, &s.CronExpression, &enabled, &raw, &last, &next); err != nil {
		return s, err
	}
	switch v := enabled.(type) {
	case bool:
		s.IsEnabled = v
	case int64:
		s.IsEnabled = v != 0
	default:
		s.IsEnabled = asString(v) == "1" || strings.EqualFold(asString(v), "true")
	}
	s.TaskConfig = json.RawMessage(raw.String)
	if len(s.TaskConfig) == 0 {
		s.TaskConfig = json.RawMessage(`{}`)
	}
	var err error
	s.LastRunAt, err = m.parseTime(last)
	if err != nil {
		return s, err
	}
	s.NextRunAt, err = m.parseTime(next)
	if err != nil {
		return s, err
	}
	s.IsSystemTask = strings.HasPrefix(s.ID, "system_")
	s.RecoveryReviewRequired = m.recoveryReview
	return s, nil
}
func (m *Manager) schedulesLocked(ctx context.Context) ([]ScheduledTask, error) {
	rows, err := m.store.DB.QueryContext(ctx, "SELECT "+scheduleColumns+" FROM scheduled_tasks ORDER BY name, id LIMIT 10001")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]ScheduledTask, 0)
	for rows.Next() {
		s, e := m.scanSchedule(rows)
		if e != nil {
			return nil, e
		}
		result = append(result, s)
	}
	if len(result) > 10000 {
		return nil, errors.New("scheduled task count exceeds 10000 safety limit")
	}
	return result, rows.Err()
}
func (m *Manager) Schedules() ([]ScheduledTask, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return m.schedulesLocked(ctx)
}
func (m *Manager) Schedule(id string) (ScheduledTask, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s, err := m.scanSchedule(m.store.DB.QueryRowContext(ctx, m.bind("SELECT "+scheduleColumns+" FROM scheduled_tasks WHERE id = ?"), id))
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrNotFound
	}
	return s, err
}

func parseSchedule(expression string, location *time.Location) (cron.Schedule, error) {
	// Existing Misaka cron strings are five-field local-time expressions. An
	// explicit CRON_TZ= prefix is accepted by robfig and overrides the app zone.
	schedule, err := cronParser.Parse(expression)
	if err != nil {
		return nil, err
	}
	next := schedule.Next(time.Now().In(location))
	if next.IsZero() {
		return nil, errors.New("cron has no future occurrence")
	}
	return schedule, nil
}
func (m *Manager) attachScheduleLocked(s ScheduledTask) error {
	return m.activateScheduleLocked(s, true)
}

func (m *Manager) activateScheduleLocked(s ScheduledTask, persistNext bool) error {
	if m.scheduler == nil || !s.IsEnabled {
		return nil
	}
	if m.recoveryReview {
		return ErrRecoveryReview
	}
	if _, ok := m.handlers[s.Kind]; !ok {
		return fmt.Errorf("%w: %s", ErrUnknownHandler, s.Kind)
	}
	if len(s.TaskConfig) > MaxParamsBytes || !json.Valid(s.TaskConfig) {
		return errors.New("invalid persisted scheduled task config")
	}
	schedule, err := parseSchedule(s.CronExpression, m.location)
	if err != nil {
		return err
	}
	id := s.ID
	if persistNext {
		next := schedule.Next(time.Now().In(m.location))
		ctx, cancel := context.WithTimeout(m.ctx, 5*time.Second)
		_, err = m.store.DB.ExecContext(ctx, m.bind("UPDATE scheduled_tasks SET next_run_at = ? WHERE id = ?"), m.dbTime(next), id)
		cancel()
		if err != nil {
			return err
		}
	}
	var entry cron.EntryID
	entry = m.scheduler.Schedule(schedule, cron.FuncJob(func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		// A callback already queued during edit/delete must not run a stale
		// schedule or a newly disabled schedule.
		if m.closed || m.scheduleEntries[id] != entry {
			return
		}
		if _, err := m.runScheduleLocked(id, true); err != nil {
			m.scheduleErrors[id] = err.Error()
		}
	}))
	m.scheduleEntries[id] = entry
	return nil
}

// ReloadSchedule activates an already committed schedule without changing any
// database row or timestamp. Missing and disabled rows remove the registration.
// An invalid row or activation failure also removes the old callback, preventing
// an obsolete cadence from continuing after a persisted configuration change.
// Enabled schedules require a started scheduler and explicit recovery approval.
func (m *Manager) ReloadSchedule(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return ErrClosed
	}
	if entry, ok := m.scheduleEntries[id]; ok && m.scheduler != nil {
		m.scheduler.Remove(entry)
	}
	delete(m.scheduleEntries, id)
	delete(m.scheduleErrors, id)
	ctx, cancel := context.WithTimeout(m.ctx, 5*time.Second)
	defer cancel()
	s, err := m.scanSchedule(m.store.DB.QueryRowContext(ctx, m.bind("SELECT "+scheduleColumns+" FROM scheduled_tasks WHERE id = ?"), id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err == nil && !s.IsEnabled {
		return nil
	}
	if err == nil {
		switch {
		case m.recoveryReview:
			err = ErrRecoveryReview
		case m.scheduler == nil:
			err = errors.New("scheduler is not started")
		default:
			err = m.activateScheduleLocked(s, false)
		}
	}
	if err != nil {
		m.scheduleErrors[id] = err.Error()
	}
	return err
}

// StartScheduler must be called after registering supported handlers. Invalid or
// unsupported imported schedules remain stored and are exposed in SchedulerErrors,
// but are never represented as successful scheduled work. Missed runs are not
// replayed on startup. New submissions use this same application timezone.
func (m *Manager) StartScheduler(timezone string) error {
	location, err := time.LoadLocation(timezone)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return ErrClosed
	}
	if m.scheduler != nil {
		return errors.New("scheduler already started")
	}
	m.location = location
	ctx, cancel := context.WithTimeout(m.ctx, 5*time.Second)
	defer cancel()
	schedules, err := m.schedulesLocked(ctx)
	if err != nil {
		return err
	}
	m.scheduler = cron.New(cron.WithLocation(location), cron.WithParser(cronParser), cron.WithChain(cron.Recover(cron.DiscardLogger), cron.SkipIfStillRunning(cron.DiscardLogger)))
	for _, s := range schedules {
		if e := m.attachScheduleLocked(s); e != nil {
			m.scheduleErrors[s.ID] = e.Error()
		}
	}
	m.scheduler.Start()
	return nil
}
func (m *Manager) SchedulerErrors() map[string]string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[string]string, len(m.scheduleErrors))
	for k, v := range m.scheduleErrors {
		out[k] = v
	}
	return out
}
func (m *Manager) SaveSchedule(s ScheduledTask) (ScheduledTask, error) {
	if strings.TrimSpace(s.Name) == "" || len(s.Name) > 500 || strings.TrimSpace(s.Kind) == "" || len(s.Kind) > 500 || len(s.ID) > 500 {
		return s, errors.New("invalid schedule name, id or job type")
	}
	if len(s.TaskConfig) == 0 {
		s.TaskConfig = json.RawMessage(`{}`)
	}
	if len(s.TaskConfig) > MaxParamsBytes || !json.Valid(s.TaskConfig) || strings.TrimSpace(string(s.TaskConfig))[0] != '{' {
		return s, errors.New("taskConfig must be a JSON object of at most 1 MiB")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return s, ErrClosed
	}
	parsed, err := parseSchedule(s.CronExpression, m.location)
	if err != nil {
		return s, err
	}
	if s.IsEnabled {
		if m.recoveryReview {
			return s, ErrRecoveryReview
		}
		if _, ok := m.handlers[s.Kind]; !ok {
			return s, ErrUnknownHandler
		}
	}
	if s.ID == "" {
		s.ID, err = newID()
		if err != nil {
			return s, err
		}
	}
	s.IsSystemTask = strings.HasPrefix(s.ID, "system_")
	s.RecoveryReviewRequired = m.recoveryReview
	next := parsed.Next(time.Now().In(m.location))
	s.NextRunAt = nil
	if s.IsEnabled {
		s.NextRunAt = &next
	}
	ctx, cancel := context.WithTimeout(m.ctx, 5*time.Second)
	defer cancel()
	existing, err := m.scanSchedule(m.store.DB.QueryRowContext(ctx, m.bind("SELECT "+scheduleColumns+" FROM scheduled_tasks WHERE id = ?"), s.ID))
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return s, err
	}
	if err == nil {
		s.LastRunAt = existing.LastRunAt
	}
	var nextArg any
	if s.NextRunAt != nil {
		nextArg = m.dbTime(*s.NextRunAt)
	}
	if errors.Is(err, sql.ErrNoRows) {
		var count int
		if err = m.store.DB.QueryRowContext(ctx, "SELECT COUNT(*) FROM scheduled_tasks").Scan(&count); err != nil {
			return s, err
		}
		if count >= 10000 {
			return s, errors.New("scheduled task count exceeds safety limit")
		}
		_, err = m.store.DB.ExecContext(ctx, m.bind("INSERT INTO scheduled_tasks ("+scheduleColumns+") VALUES (?, ?, ?, ?, ?, ?, ?, ?)"), s.ID, s.Name, s.Kind, s.CronExpression, s.IsEnabled, string(s.TaskConfig), nil, nextArg)
	} else {
		_, err = m.store.DB.ExecContext(ctx, m.bind("UPDATE scheduled_tasks SET name = ?, job_type = ?, cron_expression = ?, is_enabled = ?, task_config = ?, next_run_at = ? WHERE id = ?"), s.Name, s.Kind, s.CronExpression, s.IsEnabled, string(s.TaskConfig), nextArg, s.ID)
	}
	if err != nil {
		return s, err
	}
	if entry, ok := m.scheduleEntries[s.ID]; ok && m.scheduler != nil {
		m.scheduler.Remove(entry)
		delete(m.scheduleEntries, s.ID)
	}
	delete(m.scheduleErrors, s.ID)
	if err = m.attachScheduleLocked(s); err != nil {
		m.scheduleErrors[s.ID] = err.Error()
		return s, err
	}
	return s, nil
}
func (m *Manager) DeleteSchedule(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return ErrClosed
	}
	ctx, cancel := context.WithTimeout(m.ctx, 5*time.Second)
	defer cancel()
	// task_history has ON DELETE SET NULL. Completed history is retained.
	if _, err := m.store.DB.ExecContext(ctx, m.bind("DELETE FROM scheduled_tasks WHERE id = ?"), id); err != nil {
		return err
	}
	if entry, ok := m.scheduleEntries[id]; ok && m.scheduler != nil {
		m.scheduler.Remove(entry)
	}
	delete(m.scheduleEntries, id)
	delete(m.scheduleErrors, id)
	return nil
}
func (m *Manager) RunSchedule(id string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.runScheduleLocked(id, false)
}
func (m *Manager) runScheduleLocked(id string, requireEnabled bool) (string, error) {
	if m.closed {
		return "", ErrClosed
	}
	ctx, cancel := context.WithTimeout(m.ctx, 5*time.Second)
	defer cancel()
	s, err := m.scanSchedule(m.store.DB.QueryRowContext(ctx, m.bind("SELECT "+scheduleColumns+" FROM scheduled_tasks WHERE id = ?"), id))
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", err
	}
	if requireEnabled && !s.IsEnabled {
		return "", ErrInvalidState
	}
	if len(s.TaskConfig) > MaxParamsBytes || !json.Valid(s.TaskConfig) {
		return "", errors.New("invalid scheduled task parameters")
	}
	parsed, err := parseSchedule(s.CronExpression, m.location)
	if err != nil {
		return "", err
	}
	s.NextRunAt = nil
	if s.IsEnabled {
		next := parsed.Next(time.Now().In(m.location))
		s.NextRunAt = &next
	}
	taskID, err := m.submitLocked(s.Kind, s.TaskConfig, nil, SubmitOptions{Title: s.Name, QueueType: "management", UniqueKey: "scheduled:" + id, ScheduledTaskID: id}, &s)
	if err != nil {
		m.scheduleErrors[id] = err.Error()
		return "", err
	}
	delete(m.scheduleErrors, id)
	return taskID, nil
}

func (m *Manager) RecoveryReviewRequired() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.recoveryReview
}

// ApproveRecovery is an explicit operator decision permitting replay of pending
// historical tasks and enabled schedules. It must never be called on startup.
func (m *Manager) ApproveRecovery(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return ErrClosed
	}
	if !m.recoveryReview {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	tx, err := m.store.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = m.putConfig(ctx, tx, RecoveryGateKey, "false"); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	m.recoveryReview = false
	if err = m.refillLocked(); err != nil {
		return err
	}
	if m.scheduler != nil {
		schedules, err := m.schedulesLocked(ctx)
		if err != nil {
			return err
		}
		for _, item := range schedules {
			if _, exists := m.scheduleEntries[item.ID]; exists {
				continue
			}
			delete(m.scheduleErrors, item.ID)
			if err = m.attachScheduleLocked(item); err != nil {
				m.scheduleErrors[item.ID] = err.Error()
			}
		}
	}
	m.cond.Broadcast()
	return nil
}
