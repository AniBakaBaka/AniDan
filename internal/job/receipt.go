// SPDX-License-Identifier: AGPL-3.0-only
package job

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

type admissionReceipt struct {
	key       string
	expiresAt time.Time
	duplicate bool
}

// SubmitWithReceipt atomically admits a registered task and its replay receipt.
// An unexpired receipt returns its task ID without queueing another execution,
// even if the queue is full or its handler is no longer registered. Legacy {}
// receipts remain duplicates with an empty task ID. Expired receipts are replaced
// only if the new task commits successfully. Receipt payloads contain only taskId.
//
// Receipts retain the notification_events cache region so existing administrative
// cache controls continue protecting active notification replay receipts. key must
// be a namespaced, nonempty cache key; expiresAt must be in the future. Submission
// transactions use the caller's context and the manager's admission lock/lease.
func (m *Manager) SubmitWithReceipt(ctx context.Context, kind string, params any, opts SubmitOptions, key string, expiresAt time.Time) (id string, duplicate bool, err error) {
	if ctx == nil {
		return "", false, ErrInvalidState
	}
	if strings.TrimSpace(key) == "" || len(key) > 500 || expiresAt.IsZero() || !expiresAt.After(time.Now()) || expiresAt.Year() > 9999 {
		return "", false, errors.New("nonempty receipt key of at most 500 bytes and future expiry required")
	}
	raw, err := marshalTaskParams(kind, params)
	if err != nil {
		return "", false, err
	}
	receipt := &admissionReceipt{key: key, expiresAt: expiresAt}
	m.mu.Lock()
	defer m.mu.Unlock()
	id, err = m.submitAdmissionLocked(ctx, kind, raw, nil, opts, nil, nil, receipt, nil)
	return id, receipt.duplicate, err
}

func (m *Manager) readAdmissionReceipt(ctx context.Context, tx *sql.Tx, key string) (string, bool, error) {
	var raw string
	var expiry any
	err := tx.QueryRowContext(ctx, m.bind("SELECT cache_value, expires_at FROM cache_data WHERE cache_key = ?"), key).Scan(&raw, &expiry)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	expires, err := m.receiptExpiry(expiry)
	if err != nil || expires == nil {
		return "", false, errors.New("invalid persisted replay receipt expiry")
	}
	if !expires.After(time.Now()) {
		return "", false, nil
	}
	var receipt map[string]json.RawMessage
	if len(raw) > MaxResultBytes || json.Unmarshal([]byte(raw), &receipt) != nil || receipt == nil {
		return "", false, errors.New("invalid persisted replay receipt")
	}
	id := ""
	if value, ok := receipt["taskId"]; ok {
		if err := json.Unmarshal(value, &id); err != nil || len(id) > 500 {
			return "", false, errors.New("invalid persisted replay receipt task ID")
		}
	}
	return id, true, nil
}

func (m *Manager) receiptExpiry(value any) (*time.Time, error) {
	// cache_data.expires_at is a naive wall-clock column. PostgreSQL/MySQL may
	// return time.Time tagged with the driver's zone; preserve the stored clock
	// fields and interpret them in the application zone, as with SQLite strings.
	if wall, ok := value.(time.Time); ok {
		value = wall.Format("2006-01-02 15:04:05.999999999")
	}
	return m.parseTime(value)
}

func (m *Manager) writeAdmissionReceipt(ctx context.Context, tx *sql.Tx, receipt *admissionReceipt, id string) error {
	// The only existing row reaching this point has already expired. Keeping its
	// replacement in the task transaction also preserves it on admission failure.
	if _, err := tx.ExecContext(ctx, m.bind("DELETE FROM cache_data WHERE cache_key = ?"), receipt.key); err != nil {
		return err
	}
	raw, err := json.Marshal(map[string]string{"taskId": id})
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, m.bind("INSERT INTO cache_data (cache_key, cache_value, cache_provider, expires_at) VALUES (?, ?, ?, ?)"), receipt.key, string(raw), "notification_events", m.dbTime(receipt.expiresAt))
	return err
}
