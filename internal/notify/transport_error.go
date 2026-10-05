// SPDX-License-Identifier: AGPL-3.0-only
package notify

import (
	"context"
	"errors"
	"time"
)

// cancellationError retains stable cancellation classification without exposing
// a URL/proxy credential from an underlying transport error. A socket's deadline
// may fire just before the context timer publishes Err, so check the deadline
// itself before converting that race into a generic network failure.
func cancellationError(ctx context.Context, cause error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if errors.Is(cause, context.Canceled) {
		return context.Canceled
	}
	if errors.Is(cause, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	if deadline, ok := ctx.Deadline(); ok && !time.Now().Before(deadline) {
		return context.DeadlineExceeded
	}
	return nil
}
func transportFailure(ctx context.Context, cause error, message string) error {
	if err := cancellationError(ctx, cause); err != nil {
		return err
	}
	return errors.New(message)
}
