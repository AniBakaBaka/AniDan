// SPDX-License-Identifier: AGPL-3.0-only
package job

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"
)

// LifecyclePhase identifies a committed execution transition. Admission, pause,
// resume and crash recovery are not new executions and do not emit these phases.
type LifecyclePhase string

const (
	LifecycleStarted         LifecyclePhase = "started"
	LifecycleProgress        LifecyclePhase = "progress"
	LifecycleFinished        LifecyclePhase = "finished"
	lifecycleMaxEvents                      = 256
	lifecycleMaxBytes                       = 8 << 20
	lifecycleCallbackTimeout                = 3 * time.Second
)

// LifecycleEvent is internal state, not a transport payload. Params, Result and
// Description may contain private data; consumers must allowlist notification
// fields. Task and all its mutable fields belong exclusively to the receiver.
type LifecycleEvent struct {
	Phase      LifecyclePhase
	Task       Task
	Cancelled  bool
	OccurredAt time.Time
}

// LifecycleObserver is called serially, outside manager locks and transactions.
// It must honor ctx and return promptly. A callback can reenter Get/Submit. The
// queue is best-effort, bounded by both count and bytes, and is not an outbox.
// Panics are isolated. No goroutine is started per event: a callback that ignores
// cancellation can retain at most the single dispatcher, and CloseContext then
// times out rather than claiming shutdown completed.
type LifecycleObserver func(context.Context, LifecycleEvent)

type LifecycleStats struct {
	Queued      int
	QueuedBytes int
	// Delivered counts returned callback invocations (including isolated panics),
	// never external notification acceptance.
	Delivered uint64
	Dropped   uint64
	Panics    uint64
	Timeouts  uint64
	Active    bool
}

type lifecycleDelivery struct {
	event      LifecycleEvent
	bytes      int
	generation uint64
}

type lifecycleRuntime struct {
	mu         sync.Mutex
	observer   LifecycleObserver
	excluded   map[string]bool
	generation uint64
	queue      []lifecycleDelivery
	stats      LifecycleStats
	wake       chan struct{}
}

func newLifecycleRuntime() *lifecycleRuntime {
	return &lifecycleRuntime{wake: make(chan struct{}, 1)}
}

// SetLifecycleObserver replaces the observer and discards its queued snapshots.
// An already executing callback finishes with its original observer. nil detaches
// the observer. No historical events are replayed when it is installed. Exact
// excluded kinds (at most 64, 500 bytes each) are filtered before snapshot copies.
func (m *Manager) SetLifecycleObserver(observer LifecycleObserver, excludedKinds ...string) error {
	if len(excludedKinds) > 64 {
		return errors.New("lifecycle observer supports at most 64 excluded kinds")
	}
	excluded := make(map[string]bool, len(excludedKinds))
	for _, kind := range excludedKinds {
		if strings.TrimSpace(kind) == "" || len(kind) > 500 {
			return errors.New("invalid excluded lifecycle kind")
		}
		excluded[kind] = true
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed || m.ctx.Err() != nil {
		return ErrClosed
	}
	n := m.lifecycle
	n.mu.Lock()
	defer n.mu.Unlock()
	n.observer = observer
	n.excluded = excluded
	n.generation++
	n.stats.Dropped += uint64(len(n.queue))
	n.queue = nil
	n.stats.Queued = 0
	n.stats.QueuedBytes = 0
	return nil
}

func (m *Manager) LifecycleStats() LifecycleStats {
	n := m.lifecycle
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.stats
}

func cloneTask(t Task) Task {
	t.Params = append(t.Params[:0:0], t.Params...)
	t.Result = append(t.Result[:0:0], t.Result...)
	if t.FinishedAt != nil {
		at := *t.FinishedAt
		t.FinishedAt = &at
	}
	t.Parent = cloneParentReference(t.Parent)
	return t
}

func lifecycleBytes(e LifecycleEvent) int {
	t := e.Task
	return 512 + len(t.ID) + len(t.Title) + len(t.Description) + len(t.Kind) + len(t.QueueType) + len(t.ScheduledTaskID) + len(t.UniqueKey) + len(t.Params) + len(t.Result) + parentReferenceBytes(t.Parent)
}

// queueLifecycle is only called after committed persistence and from transition
// serialization (m.mu). The actual observer is never called on this path.
func (m *Manager) queueLifecycle(event LifecycleEvent) {
	n := m.lifecycle
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.observer == nil || n.excluded[event.Task.Kind] || m.ctx.Err() != nil {
		return
	}
	size := lifecycleBytes(event)
	if size > lifecycleMaxBytes {
		n.stats.Dropped++
		return
	}
	for len(n.queue) >= lifecycleMaxEvents || n.stats.QueuedBytes+size > lifecycleMaxBytes {
		// Terminal snapshots have priority over unobserved starts/progress. Never
		// reorder retained events and never evict another terminal snapshot.
		i := -1
		if event.Phase == LifecycleFinished {
			for j, item := range n.queue {
				if item.event.Phase != LifecycleFinished {
					i = j
					break
				}
			}
		}
		if i < 0 {
			n.stats.Dropped++
			return
		}
		n.stats.QueuedBytes -= n.queue[i].bytes
		copy(n.queue[i:], n.queue[i+1:])
		n.queue[len(n.queue)-1] = lifecycleDelivery{}
		n.queue = n.queue[:len(n.queue)-1]
		n.stats.Queued = len(n.queue)
		n.stats.Dropped++
	}
	event.Task = cloneTask(event.Task)
	n.queue = append(n.queue, lifecycleDelivery{event: event, bytes: size, generation: n.generation})
	n.stats.Queued = len(n.queue)
	n.stats.QueuedBytes += size
	select {
	case n.wake <- struct{}{}:
	default:
	}
}

func (m *Manager) lifecycleLoop() {
	defer m.wg.Done()
	n := m.lifecycle
	defer func() {
		n.mu.Lock()
		n.stats.Dropped += uint64(len(n.queue))
		n.queue = nil
		n.stats.Queued = 0
		n.stats.QueuedBytes = 0
		n.mu.Unlock()
	}()
	for {
		select {
		case <-m.ctx.Done():
			return
		case <-n.wake:
		}
		for {
			if m.ctx.Err() != nil {
				return
			}
			n.mu.Lock()
			if len(n.queue) == 0 {
				n.mu.Unlock()
				break
			}
			item := n.queue[0]
			n.queue[0] = lifecycleDelivery{}
			n.queue = n.queue[1:]
			n.stats.Queued = len(n.queue)
			n.stats.QueuedBytes -= item.bytes
			observer := n.observer
			if observer == nil || item.generation != n.generation {
				n.stats.Dropped++
				n.mu.Unlock()
				continue
			}
			n.stats.Active = true
			n.mu.Unlock()
			ctx, cancel := context.WithTimeout(m.ctx, lifecycleCallbackTimeout)
			panicked := callLifecycleObserver(ctx, observer, item.event)
			timedOut := ctx.Err() == context.DeadlineExceeded
			cancel()
			n.mu.Lock()
			n.stats.Active = false
			n.stats.Delivered++
			if panicked {
				n.stats.Panics++
			}
			if timedOut {
				n.stats.Timeouts++
			}
			n.mu.Unlock()
		}
	}
}

func callLifecycleObserver(ctx context.Context, observer LifecycleObserver, event LifecycleEvent) (panicked bool) {
	defer func() {
		if recover() != nil {
			panicked = true
		}
	}()
	observer(ctx, event)
	return false
}
