// SPDX-License-Identifier: AGPL-3.0-only
package job

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
)

// ParentMetadataPrefix stores a private, bounded immediate-parent reference for
// a child's lifetime. Maintenance should delete this key with its task history.
// No original parameters, credentials or ancestor chain are copied here.
const ParentMetadataPrefix = "anidan.job.parent."
const maxParentMetadataBytes = 2048

// ParentReference describes the dispatcher that admitted one execution child.
// ScheduledTaskID is a reference, not inheritance of scheduled-task ownership.
// A child remains independent of parent cancellation after admission commits.
type ParentReference struct {
	TaskID          string `json:"taskId"`
	Kind            string `json:"kind"`
	ScheduledTaskID string `json:"scheduledTaskId,omitempty"`
}

// ExecutionMetadata is an immutable admission-time snapshot. No lock or SQL is
// needed to read it, including while a handler holds a one-connection transaction.
type ExecutionMetadata struct {
	TaskID          string
	Kind            string
	ScheduledTaskID string
	Parent          *ParentReference
}

func ExecutionInfo(ctx context.Context) (ExecutionMetadata, bool) {
	if ctx == nil {
		return ExecutionMetadata{}, false
	}
	e, ok := ctx.Value(checkpointKey{}).(*execution)
	if !ok {
		return ExecutionMetadata{}, false
	}
	info := e.info
	info.Parent = cloneParentReference(info.Parent)
	return info, true
}

func executionMetadata(t Task) ExecutionMetadata {
	return ExecutionMetadata{TaskID: t.ID, Kind: t.Kind, ScheduledTaskID: t.ScheduledTaskID, Parent: cloneParentReference(t.Parent)}
}

func cloneParentReference(parent *ParentReference) *ParentReference {
	if parent == nil {
		return nil
	}
	copy := *parent
	return &copy
}

func parentReferenceBytes(p *ParentReference) int {
	if p == nil {
		return 0
	}
	return 32 + len(p.TaskID) + len(p.Kind) + len(p.ScheduledTaskID)
}

func validParentReference(p *ParentReference) bool {
	return p != nil && strings.TrimSpace(p.TaskID) != "" && len(p.TaskID) <= 500 && strings.TrimSpace(p.Kind) != "" && len(p.Kind) <= 500 && len(p.ScheduledTaskID) <= 500
}

func (m *Manager) writeParentReference(ctx context.Context, tx *sql.Tx, id string, parent *ParentReference) error {
	if !validParentReference(parent) {
		return errors.New("invalid child parent reference")
	}
	raw, err := json.Marshal(parent)
	if err != nil || len(raw) > maxParentMetadataBytes {
		return errors.New("child parent reference exceeds bound")
	}
	return m.putConfig(ctx, tx, ParentMetadataPrefix+id, string(raw))
}

func (m *Manager) readParentReference(ctx context.Context, id string) (*ParentReference, error) {
	var raw string
	err := m.store.DB.QueryRowContext(ctx, m.bind("SELECT SUBSTR(config_value, 1, ?) FROM config WHERE config_key = ?"), maxParentMetadataBytes+1, ParentMetadataPrefix+id).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var parent ParentReference
	if len(raw) > maxParentMetadataBytes || json.Unmarshal([]byte(raw), &parent) != nil || !validParentReference(&parent) {
		return nil, errors.New("invalid persisted child parent reference")
	}
	return &parent, nil
}
