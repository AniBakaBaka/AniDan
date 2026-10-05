// Package job provides bounded, durable jobs using Misaka's physical task schema.
package job

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

const RecoveryGateKey = "anidan.restore.review_required"

const (
	Pending        = "排队中"
	Running        = "运行中"
	Paused         = "已暂停"
	Completed      = "已完成"
	Failed         = "失败"
	MaxParamsBytes = 1 << 20
	MaxResultBytes = 1 << 20
	MaxListItems   = 1000
)

var (
	ErrClosed         = errors.New("job manager is closed")
	ErrQueueFull      = errors.New("job queue is full")
	ErrNotFound       = errors.New("task not found")
	ErrNotRetryable   = errors.New("task is not terminal or its handler is unavailable")
	ErrUnknownHandler = errors.New("job handler is not registered")
	ErrInvalidState   = errors.New("task does not support this transition")
	ErrManagerActive  = errors.New("another job manager holds the database lease")
	ErrRecoveryReview = errors.New("restored jobs require explicit operator review")
	ErrAlreadyRunning = errors.New("a run of this scheduled task is already active")
)

// Task uses upstream TaskInfo field names. Params are intentionally not exposed by
// JSON: job parameters may contain URLs, private media data or credentials.
type Task struct {
	Parent                 *ParentReference `json:"-"`
	RecoveryReviewRequired bool             `json:"recoveryReviewRequired,omitempty"`
	ID                     string           `json:"taskId"`
	Title                  string           `json:"title"`
	Status                 string           `json:"status"`
	Progress               int              `json:"progress"`
	Description            string           `json:"description"`
	CreatedAt              time.Time        `json:"createdAt"`
	UpdatedAt              time.Time        `json:"updatedAt"`
	FinishedAt             *time.Time       `json:"finishedAt,omitempty"`
	QueueType              string           `json:"queueType"`
	Kind                   string           `json:"taskType,omitempty"`
	IsSystemTask           bool             `json:"isSystemTask"`
	ScheduledTaskID        string           `json:"scheduledTaskId,omitempty"`
	UniqueKey              string           `json:"-"`
	Params                 json.RawMessage  `json:"-"`
	Result                 json.RawMessage  `json:"result,omitempty"`
}

// Handler must honor ctx, use Checkpoint before external effects, and be
// idempotent if the user explicitly retries. An interrupted running job is never
// automatically replayed. Registering a handler explicitly permits recovery of
// persisted pending jobs with that kind, never running/failed/paused ones.
type Handler func(ctx context.Context, params json.RawMessage, progress func(int, string)) (any, error)
type Func func(context.Context, func(int, string)) (any, error)

// DiagnosticResult explicitly marks sanitized, operator-visible diagnostics that
// may be persisted when a handler returns an error. Ordinary failed-handler
// results remain private. Never include credentials, raw requests or other
// unreviewed private state here. Cancellation, shutdown and panic discard these
// diagnostics; all persisted results remain bounded by MaxResultBytes.
type DiagnosticResult map[string]any

type SubmitOptions struct {
	Title           string
	QueueType       string
	UniqueKey       string
	ScheduledTaskID string
}

type ScheduledTask struct {
	RecoveryReviewRequired bool            `json:"recoveryReviewRequired,omitempty"`
	ID                     string          `json:"taskId"`
	Name                   string          `json:"name"`
	Kind                   string          `json:"jobType"`
	CronExpression         string          `json:"cronExpression"`
	IsEnabled              bool            `json:"isEnabled"`
	TaskConfig             json.RawMessage `json:"taskConfig"`
	LastRunAt              *time.Time      `json:"lastRunAt"`
	NextRunAt              *time.Time      `json:"nextRunAt"`
	IsSystemTask           bool            `json:"isSystemTask"`
}

type Stats struct {
	RecoveryReviewRequired bool `json:"recoveryReviewRequired"`
	Workers                int  `json:"workers"`
	QueueCapacity          int  `json:"queueCapacity"`
	Queued                 int  `json:"queued"`
	Running                int  `json:"running"`
	Paused                 bool `json:"paused"`
	Closed                 bool `json:"closed"`
}

type checkpointKey struct{}

// Checkpoint waits for a paused task to resume, or returns ctx.Err on cancel.
// Pausing a Go function is cooperative; no goroutine is forcibly terminated.
func Checkpoint(ctx context.Context) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		e, ok := ctx.Value(checkpointKey{}).(*execution)
		if !ok {
			return nil
		}
		gate := e.pause.Load()
		if gate == nil {
			return ctx.Err()
		}
		select {
		case <-*gate:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func Terminal(status string) bool {
	return status == Completed || status == Failed || status == "成功" || status == "已取消"
}
