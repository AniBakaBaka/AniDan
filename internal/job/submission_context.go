// SPDX-License-Identifier: AGPL-3.0-only
package job

import (
	"context"
	"errors"
	"time"
)

// SubmitRegisteredContext uses the caller's cancellation for admission only.
// After a successful commit the task has the ordinary manager-owned lifetime;
// canceling the requesting conversation does not cancel an admitted task.
// An uncertain persistence outcome remains an error, never an automatic retry.
func (m *Manager) SubmitRegisteredContext(ctx context.Context, kind string, params any) (string, error) {
	if ctx == nil {
		return "", errors.New("task admission context is required")
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	raw, err := marshalTaskParams(kind, params)
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(m.ctx, cancel)
	defer stop()
	defer cancel()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for !m.mu.TryLock() {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-ticker.C:
		}
	}
	defer m.mu.Unlock()
	return m.submitLinkedLocked(ctx, kind, raw, nil, SubmitOptions{}, nil, nil)
}
