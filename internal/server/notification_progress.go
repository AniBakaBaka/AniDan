// SPDX-License-Identifier: AGPL-3.0-only
package server

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/AniBakaBaka/AniDan/internal/job"
	"github.com/AniBakaBaka/AniDan/internal/notify"
)

const progressMaxTasks = 128
const progressMaxDestinations = 512
const progressMaxCooldowns = 1024
const progressMemoryTTL = 10 * time.Minute

type notificationProgressView struct {
	Kind, Status   string
	Event, Summary string
	Percent        int
	Terminal       bool
	Cancelled      bool
	Ready          bool
	At             time.Time
}
type notificationProgressState struct {
	record    notificationProgressRecord
	view      notificationProgressView
	next      []time.Time
	busy      []bool
	blocked   []bool          // local preflight/storage failures; never fabricate protocol acknowledgments
	firstSend []atomic.Uint32 // 0=open,1=cancelled,2=initial HTTP admitted
}
type notificationProgressRuntime struct {
	ctx                                                                                   context.Context
	cancel                                                                                context.CancelFunc
	done                                                                                  chan struct{}
	wake                                                                                  chan struct{}
	mu                                                                                    sync.Mutex
	closed                                                                                bool
	states                                                                                map[string]*notificationProgressState
	channels                                                                              map[int64]time.Time
	globalNext                                                                            time.Time
	interval, channelInterval, globalInterval                                             time.Duration
	observed, suppressed, rejected, expired, errors, uncertain, sent, edited, invalidated atomic.Uint64
	active                                                                                atomic.Bool
}

func (s *Server) initNotificationProgress(m *http.ServeMux) {
	ctx, cancel := context.WithCancel(s.ctx)
	s.notificationProgress = &notificationProgressRuntime{ctx: ctx, cancel: cancel, done: make(chan struct{}), wake: make(chan struct{}, 1), states: map[string]*notificationProgressState{}, channels: map[int64]time.Time{}, interval: 2 * time.Second, channelInterval: time.Second, globalInterval: 250 * time.Millisecond}
	m.HandleFunc("GET /api/ui/notification/progress/status", s.operator(s.notificationProgressStatus))
	go s.notificationProgressLoop()
}
func (s *Server) closeNotificationProgress() {
	n := s.notificationProgress
	if n == nil {
		return
	}
	n.mu.Lock()
	n.closed = true
	n.mu.Unlock()
	n.cancel()
	select {
	case <-n.done:
	case <-time.After(4 * time.Second):
		n.errors.Add(1)
	}
}
func progressEligible(t job.Task) bool {
	if !notificationProgressTaskID(t.ID) || t.Kind == "" || len(t.Kind) > 128 || strings.HasPrefix(t.Kind, "notification_") {
		return false
	}
	// The ordinary policy excludes fallback operations. Their explicit text-mode
	// completion-subscription policy is handled by notificationProgressEvent.
	if t.QueueType == "fallback" {
		return false
	}
	switch t.Kind {
	case "player_search", "player_match", "predownload", "control_auto_import":
		return false
	}
	return true
}

// Fallback progress reuses the pinned completion subscription names. Native
// job kinds, not title text or queue labels, identify these two operations.
func notificationProgressEvent(t job.Task) string {
	if !notificationProgressTaskID(t.ID) {
		return ""
	}
	if t.Kind == "player_search" || t.Kind == "player_match" {
		if t.ScheduledTaskID != "" || (t.Parent != nil && t.Parent.ScheduledTaskID != "") {
			return ""
		}
		if t.Kind == "player_search" {
			return "fallback_search_complete"
		}
		return "match_fallback_complete"
	}
	if progressEligible(t) {
		return "task_progress"
	}
	return ""
}
func notificationProgressChannel(c notify.Channel, event string) bool {
	if !c.Enabled || c.Type != "telegram" || event == "" || !notify.Bool(c.EventsConfig[event]) {
		return false
	}
	return event == "task_progress" || notificationImageMode(c) == "text"
}
func progressView(e job.LifecycleEvent) notificationProgressView {
	status := e.Task.Status
	terminal := e.Phase == job.LifecycleFinished
	if e.Cancelled {
		status = "已取消"
	} else if !terminal && status != job.Paused {
		status = job.Running
	}
	ready := notificationProgressEvent(e.Task) == "task_progress" || terminal || (e.Phase == job.LifecycleProgress && e.Task.Progress < 100)
	return notificationProgressView{Ready: ready, Kind: e.Task.Kind, Status: status, Event: notificationProgressEvent(e.Task), Summary: notificationFallbackProgressSummary(e), Percent: max(0, min(100, e.Task.Progress)), Terminal: terminal, Cancelled: e.Cancelled, At: e.OccurredAt}
}
func progressMessage(id string, v notificationProgressView) notify.Message {
	label := "后台任务"
	switch v.Kind {
	case "player_search":
		label = "后备搜索"
	case "player_match":
		label = "匹配后备"
	case "generic_import", "library_import", "import_media_items", "import_local_items", "calendar_import":
		label = "导入弹幕"
	case "fetch_comments", "library_refresh", "library_source_refresh":
		label = "刷新弹幕"
	case "scan_media_server":
		label = "扫描媒体库"
	case "subscription_target", "subscriptionScan", "incrementalRefresh":
		label = "增量追更"
	case "backupDatabase":
		label = "数据库备份"
	}
	b := notificationTextBuilder{}
	b.append(label, "bold", "")
	b.append("\n", "", "")
	b.append("状态：", "bold", "")
	b.append(v.Status+"\n", "", "")
	b.append("进度：", "bold", "")
	b.append(fmt.Sprintf("%d%%", v.Percent), "", "")
	if v.Summary != "" {
		b.append("\n"+v.Summary, "", "")
	}
	b.append("\n", "", "")
	b.append("任务ID：", "bold", "")
	b.append(id, "code", "")
	b.append("\n详情请查看已登录管理界面。", "", "")
	return notify.Message{Title: "AniDan 任务进度", Text: b.text.String(), TextSpans: b.spans}
}
func progressTextHash(m notify.Message) (string, error) {
	return notify.TelegramTextFingerprint(m)
}
func progressTargets(r notificationProgressRecord) []notify.DeliveryTarget {
	out := make([]notify.DeliveryTarget, 0, len(r.Targets))
	for _, d := range r.Targets {
		out = append(out, d.Target)
	}
	return out
}
func (s *Server) newNotificationProgressRecord(ctx context.Context, t job.Task) (notificationProgressRecord, error) {
	r := notificationProgressRecord{Version: 1, TaskID: t.ID, CreatedAt: t.CreatedAt.UTC(), ExpiresAt: t.CreatedAt.UTC().Add(7 * 24 * time.Hour), Targets: []notificationProgressDestination{}}
	if r.CreatedAt.IsZero() || r.CreatedAt.After(time.Now().Add(5*time.Minute)) || !r.ExpiresAt.After(time.Now().Add(time.Minute)) {
		return r, errNotificationProgressExpired
	}
	rows, err := s.notificationEnabledRows(ctx)
	if err != nil {
		return r, err
	}
	for _, row := range rows {
		c, e := notificationDecode(row)
		if e != nil {
			return r, e
		}
		if !notificationProgressChannel(c, notificationProgressEvent(t)) {
			continue
		}
		r.Targets = append(r.Targets, notificationProgressDestination{Target: notify.DeliveryTarget{ID: c.ID, Fingerprint: notificationTargetFingerprint(row, c)}})
		if len(r.Targets) > 64 {
			return r, notify.ErrLimit
		}
	}
	return r, nil
}
func (n *notificationProgressRuntime) pruneLocked(now time.Time) {
	for id, st := range n.states {
		busy := false
		allDone := true
		for i, d := range st.record.Targets {
			busy = busy || st.busy[i]
			allDone = allDone && (d.Terminal || d.Uncertain || st.blocked[i])
		}
		stale := now.Sub(st.view.At) > progressMemoryTTL
		if !busy && (stale || (st.view.Terminal && allDone)) {
			delete(n.states, id)
			if stale && !allDone {
				n.expired.Add(1)
			}
		}
	}
	used := map[int64]bool{}
	for _, st := range n.states {
		for _, d := range st.record.Targets {
			used[d.Target.ID] = true
		}
	}
	for id := range n.channels {
		if !used[id] && !now.Before(n.channels[id]) {
			delete(n.channels, id)
		}
	}
}

// observeNotificationProgress returns exact audiences whose terminal notice is
// owned by the editable progress message. This prevents a second completion
// send while edits are pending or delivery is uncertain.
func (s *Server) observeNotificationProgress(ctx context.Context, e job.LifecycleEvent) ([]notify.DeliveryTarget, bool) {
	n := s.notificationProgress
	if n == nil || notificationProgressEvent(e.Task) == "" || (e.Phase != job.LifecycleStarted && e.Phase != job.LifecycleProgress && e.Phase != job.LifecycleFinished) {
		return nil, false
	}
	if e.OccurredAt.IsZero() || e.OccurredAt.After(time.Now().Add(time.Minute)) || time.Since(e.OccurredAt) > progressMemoryTTL {
		// Staleness bounds speculative work, not receipt ownership. A delayed
		// terminal observation must not create a replacement completion send.
		if e.Phase == job.LifecycleFinished {
			record, err := s.readNotificationProgressRecord(ctx, e.Task.ID)
			if err == nil {
				return progressTargets(*record), false
			}
			if !errors.Is(err, sql.ErrNoRows) {
				n.errors.Add(1)
				return nil, true
			}
		}
		return nil, false
	}
	n.observed.Add(1)
	n.mu.Lock()
	st := n.states[e.Task.ID]
	var owned []notify.DeliveryTarget
	if st != nil {
		owned = progressTargets(st.record)
		if e.Cancelled {
			for i := range st.firstSend {
				st.firstSend[i].CompareAndSwap(0, 1)
			}
		}
	}
	closed := n.closed
	n.mu.Unlock()
	if st == nil {
		record, err := s.readNotificationProgressRecord(ctx, e.Task.ID)
		if errors.Is(err, sql.ErrNoRows) {
			if closed || e.Cancelled {
				return nil, false
			}
			initial, e2 := s.newNotificationProgressRecord(ctx, e.Task)
			if e2 != nil {
				n.errors.Add(1)
				return nil, true
			}
			// Persist even an empty first audience before memory admission. A restart
			// or later opt-in must not add recipients to this already observed task.
			record, err = s.mutateNotificationProgressRecord(ctx, e.Task.ID, &initial, func(*notificationProgressRecord) error { return nil })
		}
		if err != nil {
			n.errors.Add(1)
			return nil, true
		}
		owned = progressTargets(*record)
		if len(record.Targets) == 0 {
			return nil, false
		}
		n.mu.Lock()
		st = n.states[e.Task.ID]
		if st == nil {
			if n.closed {
				n.mu.Unlock()
				if e.Phase == job.LifecycleFinished {
					return progressTargets(*record), false
				}
				return nil, false
			}
			n.pruneLocked(time.Now())
			pairs := 0
			for _, x := range n.states {
				pairs += len(x.record.Targets)
			}
			if len(n.states) >= progressMaxTasks || pairs+len(record.Targets) > progressMaxDestinations {
				n.rejected.Add(1)
				n.mu.Unlock()
				if e.Phase == job.LifecycleFinished {
					return owned, false
				}
				return nil, false
			}
			st = &notificationProgressState{record: *record, view: progressView(e), next: make([]time.Time, len(record.Targets)), busy: make([]bool, len(record.Targets)), blocked: make([]bool, len(record.Targets)), firstSend: make([]atomic.Uint32, len(record.Targets))}
			if e.Cancelled {
				for i := range st.firstSend {
					st.firstSend[i].CompareAndSwap(0, 1)
				}
			}
			n.states[e.Task.ID] = st
		}
		n.mu.Unlock()
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	st = n.states[e.Task.ID]
	if st == nil {
		if e.Phase == job.LifecycleFinished {
			return owned, false
		}
		return nil, false
	}
	if st.view.Event != notificationProgressEvent(e.Task) || st.view.Kind != e.Task.Kind {
		n.errors.Add(1)
		return nil, true
	}
	if !n.closed && !st.view.Terminal && !e.OccurredAt.Before(st.view.At) {
		v := progressView(e)
		v.Percent = max(v.Percent, st.view.Percent)
		st.view = v
		select {
		case n.wake <- struct{}{}:
		default:
		}
	} else {
		n.suppressed.Add(1)
	}
	if e.Phase == job.LifecycleFinished {
		return progressTargets(st.record), false
	}
	return nil, false
}

type notificationProgressWork struct {
	taskID    string
	index     int
	record    notificationProgressRecord
	target    notify.DeliveryTarget
	view      notificationProgressView
	message   notify.Message
	hash      string
	firstSend *atomic.Uint32
}

func (n *notificationProgressRuntime) nextWork(now time.Time) *notificationProgressWork {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.closed || now.Before(n.globalNext) {
		return nil
	}
	n.pruneLocked(now)
	for id, st := range n.states {
		if !st.view.Ready {
			continue
		}
		for i, d := range st.record.Targets {
			if st.busy[i] || st.blocked[i] || d.Terminal || d.Uncertain || now.Before(st.next[i]) || now.Before(n.channels[d.Target.ID]) {
				continue
			}
			if _, tracked := n.channels[d.Target.ID]; !tracked && len(n.channels) >= progressMaxCooldowns {
				continue
			}
			view := st.view
			view.Percent = max(view.Percent, d.LastProgress)
			message := progressMessage(id, view)
			hash, err := progressTextHash(message)
			if err != nil {
				st.blocked[i] = true
				n.errors.Add(1)
				continue
			}
			// Legacy plain hashes differ once from the canonical text/entities
			// fingerprint. Existing cooldown and durable receipt gates still apply:
			// an active receipt may get one edit, never a replacement send.
			if !d.Pending && d.Attempted && hash == d.LastHash {
				continue
			}
			st.busy[i] = true
			st.next[i] = now.Add(n.interval)
			n.channels[d.Target.ID] = now.Add(n.channelInterval)
			n.globalNext = now.Add(n.globalInterval)
			record := st.record
			record.Targets = append([]notificationProgressDestination(nil), record.Targets...)
			return &notificationProgressWork{taskID: id, index: i, record: record, target: d.Target, view: view, message: message, hash: hash, firstSend: &st.firstSend[i]}
		}
	}
	return nil
}
func (s *Server) notificationProgressLoop() {
	n := s.notificationProgress
	defer close(n.done)
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-n.ctx.Done():
			return
		case <-n.wake:
		case <-tick.C:
		}
		if w := n.nextWork(time.Now()); w != nil {
			s.executeNotificationProgress(n, w)
		}
	}
}
func progressDestination(r *notificationProgressRecord, target notify.DeliveryTarget) (*notificationProgressDestination, error) {
	for i := range r.Targets {
		if r.Targets[i].Target == target {
			return &r.Targets[i], nil
		}
	}
	return nil, errNotificationProgressState
}
func (s *Server) executeNotificationProgress(n *notificationProgressRuntime, w *notificationProgressWork) {
	n.active.Store(true)
	defer n.active.Store(false)
	var final *notificationProgressRecord
	failed := false
	defer func() {
		n.mu.Lock()
		if st := n.states[w.taskID]; st != nil {
			st.busy[w.index] = false
			if final != nil {
				st.record = *final
			} else if failed {
				st.blocked[w.index] = true
			}
			select {
			case n.wake <- struct{}{}:
			default:
			}
		}
		n.mu.Unlock()
	}()
	ctx, cancel := context.WithTimeout(n.ctx, 3*time.Second)
	defer cancel()
	record, err := s.readNotificationProgressRecord(ctx, w.taskID)
	if err == nil {
		d, e := progressDestination(record, w.target)
		if e != nil {
			n.errors.Add(1)
			failed = true
			return
		}
		if d.Pending || (d.Attempted && d.Receipt == nil && !d.Terminal && !d.Uncertain) {
			final, err = s.mutateNotificationProgressRecord(ctx, w.taskID, nil, func(r *notificationProgressRecord) error {
				x, e := progressDestination(r, w.target)
				if e != nil {
					return e
				}
				x.Pending = false
				x.Uncertain = true
				return nil
			})
			n.uncertain.Add(1)
			if err != nil {
				n.errors.Add(1)
				failed = true
			}
			return
		}
		if d.Terminal || d.Uncertain {
			final = record
			return
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		n.errors.Add(1)
		failed = true
		return
	}
	// Cancellation may update an existing fallback receipt, but must not
	// manufacture its first message when the send has not yet been attempted.
	if w.view.Cancelled && w.view.Event != "task_progress" {
		if record == nil {
			n.errors.Add(1)
			failed = true
			return
		}
		d, e := progressDestination(record, w.target)
		if e == nil && !d.Attempted && d.Receipt == nil {
			final, err = s.mutateNotificationProgressRecord(ctx, w.taskID, nil, func(r *notificationProgressRecord) error {
				d, e := progressDestination(r, w.target)
				if e != nil {
					return e
				}
				if d.Attempted || d.Receipt != nil {
					return errNotificationProgressState
				}
				d.Terminal = true
				return nil
			})
			if err != nil {
				n.errors.Add(1)
				failed = true
			}
			return
		}
	}
	rows, err := s.notificationEnabledRows(ctx)
	if err != nil {
		n.errors.Add(1)
		failed = true
		return
	}
	var channel notify.Channel
	valid := false
	for _, row := range rows {
		if number(row["id"]) != w.target.ID {
			continue
		}
		c, e := notificationDecode(row)
		if e == nil && notificationProgressChannel(c, w.view.Event) && notificationTargetFingerprint(row, c) == w.target.Fingerprint {
			channel = c
			valid = true
		}
		break
	}
	if !valid {
		final, err = s.mutateNotificationProgressRecord(ctx, w.taskID, &w.record, func(r *notificationProgressRecord) error {
			d, e := progressDestination(r, w.target)
			if e == nil {
				d.Terminal = true
			}
			return e
		})
		n.invalidated.Add(1)
		if err != nil {
			n.errors.Add(1)
			failed = true
		}
		return
	}
	if err = notify.ValidateMessageShape(channel, w.message); err != nil {
		n.errors.Add(1)
		failed = true
		return
	}
	if ctx, err = s.Notify.PrepareOperation(ctx, channel); err != nil {
		n.errors.Add(1)
		failed = true
		return
	}
	var receipt *notify.ProgressReceipt
	var attempt uint64
	noop := false
	final, err = s.mutateNotificationProgressRecord(ctx, w.taskID, &w.record, func(r *notificationProgressRecord) error {
		d, e := progressDestination(r, w.target)
		if e != nil {
			return e
		}
		if d.Pending || d.Uncertain || d.Terminal {
			return errNotificationProgressState
		}
		if d.Attempted && d.LastHash == w.hash {
			noop = true
			return nil
		}
		if w.view.Percent < d.LastProgress {
			return errNotificationProgressState
		}
		if d.Attempt >= 1024 {
			return notify.ErrLimit
		}
		receipt = d.Receipt
		d.Attempted = true
		d.Pending = true
		d.Attempt++
		attempt = d.Attempt
		d.LastAction = "send"
		if receipt != nil {
			d.LastAction = "edit"
		}
		d.LastHash = w.hash
		d.LastProgress = max(d.LastProgress, w.view.Percent)
		return nil
	})
	if err != nil {
		n.errors.Add(1)
		failed = true
		return
	}
	if noop {
		return
	}
	// Linearize first-request admission against an observed cancellation only
	// after Pending is durable. If cancellation won, no network operation is
	// performed; an admitted in-flight request may finish and then be edited.
	if receipt == nil && w.view.Event != "task_progress" && (w.firstSend == nil || !w.firstSend.CompareAndSwap(0, 2)) {
		final, err = s.mutateNotificationProgressRecord(ctx, w.taskID, nil, func(r *notificationProgressRecord) error {
			d, e := progressDestination(r, w.target)
			if e != nil {
				return e
			}
			if d.Attempt != attempt || !d.Pending || d.LastHash != w.hash {
				return errNotificationProgressState
			}
			d.Pending = false
			d.Terminal = true
			return nil
		})
		if err != nil {
			n.errors.Add(1)
			failed = true
		}
		return
	}
	var got notify.ProgressReceipt
	if receipt == nil {
		got, err = s.Notify.SendProgress(ctx, channel, w.message)
	} else {
		err = s.Notify.EditProgress(ctx, channel, *receipt, w.message)
	}
	success := err == nil
	cleanup, stop := context.WithTimeout(s.ctx, 2*time.Second)
	defer stop()
	final, err = s.mutateNotificationProgressRecord(cleanup, w.taskID, nil, func(r *notificationProgressRecord) error {
		d, e := progressDestination(r, w.target)
		if e != nil {
			return e
		}
		if d.Attempt != attempt || !d.Pending || d.LastHash != w.hash {
			return errNotificationProgressState
		}
		d.Pending = false
		if !success {
			d.Uncertain = true
			return nil
		}
		if receipt == nil {
			d.Receipt = &got
		}
		if w.view.Terminal {
			d.Terminal = true
		}
		return nil
	})
	if !success {
		n.uncertain.Add(1)
	} else if receipt == nil {
		n.sent.Add(1)
	} else {
		n.edited.Add(1)
	}
	if err != nil {
		n.errors.Add(1)
		failed = true
	}
}
func (s *Server) notificationProgressStatus(w http.ResponseWriter, r *http.Request) {
	n := s.notificationProgress
	if n == nil {
		httpError(w, 503, "Task progress notifications unavailable")
		return
	}
	n.mu.Lock()
	tasks := len(n.states)
	pairs := 0
	for _, st := range n.states {
		pairs += len(st.record.Targets)
	}
	closed := n.closed
	n.mu.Unlock()
	counters := map[string]uint64{"observed": n.observed.Load(), "coalesced": n.suppressed.Load(), "rejected": n.rejected.Load(), "expired": n.expired.Load(), "errors": n.errors.Load(), "uncertain": n.uncertain.Load(), "sendAccepted": n.sent.Load(), "editAccepted": n.edited.Load(), "invalidated": n.invalidated.Load()}
	writeJSON(w, 200, map[string]any{"supportedChannels": []string{"telegram"}, "scope": "ordinary task_progress; Telegram text-mode automatic fallback uses existing completion subscriptions", "limits": map[string]any{"tasks": progressMaxTasks, "destinations": progressMaxDestinations, "perTaskDestinations": 64, "channelCooldowns": progressMaxCooldowns, "perMessageIntervalSeconds": 2, "perChannelIntervalSeconds": 1, "globalIntervalSeconds": 0.25, "requestTimeoutSeconds": 3, "memoryRetentionSeconds": 600}, "tasks": tasks, "destinations": pairs, "active": n.active.Load(), "closed": closed, "counters": counters, "degraded": n.errors.Load() > 0 || n.uncertain.Load() > 0 || n.rejected.Load() > 0 || n.expired.Load() > 0, "guarantee": "Accepted API responses are not proof of recipient delivery; uncertain attempts are not replayed"})
}
