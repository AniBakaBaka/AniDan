package job

import (
	"context"
	"encoding/json"
)

// SubmitChild submits the single execution job of a live scheduler task. The
// child and the parent's executionTaskId result are committed atomically, so the
// link is queryable even if the scheduler is interrupted before returning. The
// child has its own lifetime: completing or cancelling the parent after this
// commit does not cancel the child's execution.
//
// ctx must be the parent's handler context (or a context derived from it).
// Cancellation and pause are checked again under the manager lock immediately
// before submission. The transaction uses ctx, not the manager's global context.
func (m *Manager) SubmitChild(ctx context.Context, kind string, params any, fn Func, opts SubmitOptions) (string, error) {
	if ctx == nil {
		return "", ErrInvalidState
	}
	parent, ok := ctx.Value(checkpointKey{}).(*execution)
	if !ok || parent.manager != m {
		return "", ErrInvalidState
	}
	raw, err := marshalTaskParams(kind, params)
	if err != nil {
		return "", err
	}
	for {
		if err = Checkpoint(ctx); err != nil {
			return "", err
		}
		m.mu.Lock()
		if err = ctx.Err(); err == nil {
			err = parent.ctx.Err()
		}
		if err != nil {
			m.mu.Unlock()
			return "", err
		}
		if parent.cancelRequested || m.executions[parent.task.ID] != parent || !parent.started || (parent.task.Status != Running && parent.task.Status != Paused) {
			m.mu.Unlock()
			return "", ErrInvalidState
		}
		// Pause may have raced the checkpoint above. Never wait while holding mu.
		if parent.pause.Load() != nil {
			m.mu.Unlock()
			continue
		}
		if executionChildID(parent.task.Result) != "" {
			m.mu.Unlock()
			return "", ErrAlreadyRunning
		}
		id, err := m.submitLinkedLocked(ctx, kind, raw, fn, opts, nil, parent)
		m.mu.Unlock()
		return id, err
	}
}

func executionChildID(result json.RawMessage) string {
	var link struct {
		ID string `json:"executionTaskId"`
	}
	_ = json.Unmarshal(result, &link)
	return link.ID
}

// Preserve a committed child link when a scheduler returns nil, fails, or
// returns other result fields. Handler output cannot replace the linked ID.
func preserveChildResult(previous, result json.RawMessage) (json.RawMessage, error) {
	id := executionChildID(previous)
	if id == "" {
		return result, nil
	}
	merged := map[string]json.RawMessage{}
	if len(result) > 0 {
		if err := json.Unmarshal(result, &merged); err != nil {
			merged = map[string]json.RawMessage{"result": result}
		}
	}
	if merged == nil {
		merged = map[string]json.RawMessage{}
	}
	merged["executionTaskId"], _ = json.Marshal(id)
	return json.Marshal(merged)
}
