// SPDX-License-Identifier: AGPL-3.0-only
package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/AniBakaBaka/AniDan/internal/notify"
	"github.com/AniBakaBaka/AniDan/internal/store"
)

const (
	notificationProgressRecordVersion = 1
	notificationProgressRecordLimit   = 64 << 10
	notificationProgressTargetLimit   = 64
	notificationProgressAttemptLimit  = 1024
	notificationProgressRecordTTL     = 7 * 24 * time.Hour
	notificationProgressStoreTimeout  = 3 * time.Second
)

var (
	errNotificationProgressState   = errors.New("invalid notification progress state; original audience retained")
	errNotificationProgressExpired = errors.New("notification progress state expired; original audience retained")
)

// This is protocol state, not a disposable response cache. It contains only the
// first audience snapshot, opaque message receipts, and bounded delivery state.
// A lost or malformed record must never become permission to send a new message.
type notificationProgressRecord struct {
	Version   int                               `json:"version"`
	TaskID    string                            `json:"taskId"`
	CreatedAt time.Time                         `json:"createdAt"`
	ExpiresAt time.Time                         `json:"expiresAt"`
	Targets   []notificationProgressDestination `json:"targets"`
}

type notificationProgressDestination struct {
	Target       notify.DeliveryTarget   `json:"target"`
	Receipt      *notify.ProgressReceipt `json:"receipt"`
	Attempted    bool                    `json:"attempted"`
	Pending      bool                    `json:"pending"`
	Uncertain    bool                    `json:"uncertain"`
	Terminal     bool                    `json:"terminal"`
	LastHash     string                  `json:"lastHash"`
	LastProgress int                     `json:"lastProgress"`
	Attempt      uint64                  `json:"attempt"`
	LastAction   string                  `json:"lastAction"`
}

func notificationProgressKey(taskID string) string {
	h := sha256.Sum256([]byte(taskID))
	return "notification-progress:" + hex.EncodeToString(h[:])
}

func notificationProgressTaskID(id string) bool {
	if len(id) == 0 || len(id) > 128 {
		return false
	}
	for _, c := range id {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("_-.:", c)) {
			return false
		}
	}
	return true
}

func notificationProgressHex(v string) bool {
	if len(v) != 64 {
		return false
	}
	for _, c := range v {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

func notificationProgressDecimal(v string, positive bool) bool {
	n, err := strconv.ParseInt(v, 10, 64)
	return err == nil && n != 0 && (!positive || n > 0) && strconv.FormatInt(n, 10) == v
}

func notificationProgressRecipient(v string) bool {
	if notificationProgressDecimal(v, false) {
		return true
	}
	if len(v) < 2 || len(v) > 33 || v[0] != '@' {
		return false
	}
	for _, c := range v[1:] {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '_') {
			return false
		}
	}
	return true
}

func (r *notificationProgressRecord) validate(taskID string, now time.Time) error {
	if r == nil || r.Version != notificationProgressRecordVersion || !notificationProgressTaskID(taskID) || r.TaskID != taskID || r.CreatedAt.IsZero() || r.CreatedAt.After(now.Add(5*time.Minute)) || !r.ExpiresAt.Equal(r.CreatedAt.Add(notificationProgressRecordTTL)) || r.Targets == nil || len(r.Targets) > notificationProgressTargetLimit {
		return errNotificationProgressState
	}
	if !r.ExpiresAt.After(now) {
		return errNotificationProgressExpired
	}
	ids := make(map[int64]bool, len(r.Targets))
	for _, d := range r.Targets {
		if d.Target.ID <= 0 || ids[d.Target.ID] || !notificationProgressHex(d.Target.Fingerprint) || d.Attempt > notificationProgressAttemptLimit || d.LastProgress < 0 || d.LastProgress > 100 || d.Attempted != (d.Attempt > 0) {
			return errNotificationProgressState
		}
		ids[d.Target.ID] = true
		if !d.Attempted {
			if d.Receipt != nil || d.Pending || d.Uncertain || d.LastHash != "" || d.LastAction != "" || d.LastProgress != 0 {
				return errNotificationProgressState
			}
		} else if !notificationProgressHex(d.LastHash) || (d.LastAction != "send" && d.LastAction != "edit") || (d.LastAction == "edit" && (d.Receipt == nil || d.Attempt < 2)) || (d.LastAction == "send" && d.Attempt != 1) {
			return errNotificationProgressState
		}
		if (d.Pending && d.Uncertain) || (d.Attempted && d.Receipt == nil && !d.Pending && !d.Uncertain && !d.Terminal) {
			return errNotificationProgressState
		}
		if d.Receipt != nil {
			v := d.Receipt
			if !notificationProgressDecimal(v.MessageID, true) || !notificationProgressDecimal(v.ChatID, false) || !notificationProgressRecipient(v.Recipient) || !notificationProgressHex(v.Binding) || (v.Recipient[0] != '@' && v.Recipient != v.ChatID) {
				return errNotificationProgressState
			}
		}
	}
	return nil
}

func cloneNotificationProgressRecord(r *notificationProgressRecord) *notificationProgressRecord {
	if r == nil {
		return nil
	}
	c := *r
	c.Targets = append([]notificationProgressDestination{}, r.Targets...)
	for i := range c.Targets {
		if r.Targets[i].Receipt != nil {
			receipt := *r.Targets[i].Receipt
			c.Targets[i].Receipt = &receipt
		}
	}
	return &c
}

// Check the declared wire schema before decoding. encoding/json alone accepts
// duplicate keys, case aliases, missing fields and null scalar values. These
// ambiguities cannot be permitted to erase an attempted or uncertain outcome.
func notificationProgressJSONValue(d *json.Decoder, typ reflect.Type, depth int) error {
	if depth > 5 {
		return errNotificationProgressState
	}
	token, err := d.Token()
	if err != nil {
		return err
	}
	if typ.Kind() == reflect.Pointer {
		if token == nil {
			return nil
		}
		typ = typ.Elem()
	}
	if typ == reflect.TypeFor[time.Time]() {
		if _, ok := token.(string); !ok {
			return errNotificationProgressState
		}
		return nil
	}
	switch typ.Kind() {
	case reflect.Struct:
		if token != json.Delim('{') {
			return errNotificationProgressState
		}
		fields := make(map[string]reflect.Type, typ.NumField())
		for i := 0; i < typ.NumField(); i++ {
			field := typ.Field(i)
			fields[strings.Split(field.Tag.Get("json"), ",")[0]] = field.Type
		}
		seen := make(map[string]bool, len(fields))
		for d.More() {
			key, e := d.Token()
			if e != nil {
				return e
			}
			name, ok := key.(string)
			child, exists := fields[name]
			if !ok || !exists || seen[name] {
				return errNotificationProgressState
			}
			seen[name] = true
			if err = notificationProgressJSONValue(d, child, depth+1); err != nil {
				return err
			}
		}
		if len(seen) != len(fields) {
			return errNotificationProgressState
		}
		token, err = d.Token()
		if err != nil || token != json.Delim('}') {
			return errNotificationProgressState
		}
	case reflect.Slice:
		if token != json.Delim('[') {
			return errNotificationProgressState
		}
		for count := 0; d.More(); count++ {
			if count >= notificationProgressTargetLimit {
				return errNotificationProgressState
			}
			if err = notificationProgressJSONValue(d, typ.Elem(), depth+1); err != nil {
				return err
			}
		}
		token, err = d.Token()
		if err != nil || token != json.Delim(']') {
			return errNotificationProgressState
		}
	case reflect.String:
		if _, ok := token.(string); !ok {
			return errNotificationProgressState
		}
	case reflect.Bool:
		if _, ok := token.(bool); !ok {
			return errNotificationProgressState
		}
	case reflect.Int, reflect.Int64, reflect.Uint64:
		if _, ok := token.(json.Number); !ok {
			return errNotificationProgressState
		}
	default:
		return errNotificationProgressState
	}
	return nil
}

func decodeNotificationProgressRecord(raw string) (*notificationProgressRecord, error) {
	if len(raw) == 0 || len(raw) > notificationProgressRecordLimit || !utf8.ValidString(raw) {
		return nil, errNotificationProgressState
	}
	d := json.NewDecoder(strings.NewReader(raw))
	d.UseNumber()
	if err := notificationProgressJSONValue(d, reflect.TypeFor[notificationProgressRecord](), 0); err != nil {
		return nil, errNotificationProgressState
	}
	if _, err := d.Token(); err != io.EOF {
		return nil, errNotificationProgressState
	}
	var r notificationProgressRecord
	d = json.NewDecoder(strings.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(&r); err != nil {
		return nil, errNotificationProgressState
	}
	return &r, nil
}

func (s *Server) readNotificationProgressRecord(ctx context.Context, taskID string) (*notificationProgressRecord, error) {
	ctx, cancel := context.WithTimeout(ctx, notificationProgressStoreTimeout)
	defer cancel()
	return s.readNotificationProgressRecordTx(ctx, s.Store.DB, taskID, false)
}

func (s *Server) readNotificationProgressRecordTx(ctx context.Context, ex store.Executor, taskID string, lock bool) (*notificationProgressRecord, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !notificationProgressTaskID(taskID) {
		return nil, errNotificationProgressState
	}
	length, expiryType := "OCTET_LENGTH(cache_value)", "TEXT"
	if s.Store.Dialect == "sqlite" {
		length = "LENGTH(CAST(cache_value AS BLOB))"
	} else if s.Store.Dialect == "mysql" {
		expiryType = "CHAR"
	}
	// CASE bounds transferred bytes; SUBSTR independently bounds characters. Do
	// not filter by region/expiry in WHERE: such rows exist and must fail closed.
	query := "SELECT CASE WHEN " + length + "<=? THEN SUBSTR(cache_value,1,65537) ELSE NULL END," + length + ",SUBSTR(cache_provider,1,64),SUBSTR(CAST(expires_at AS " + expiryType + "),1,65) FROM cache_data WHERE cache_key=?"
	if lock && s.Store.Dialect != "sqlite" {
		query += " FOR UPDATE"
	}
	var raw, region sql.NullString
	var expiry string
	var size int64
	err := ex.QueryRowContext(ctx, s.Store.Rebind(query), notificationProgressRecordLimit, notificationProgressKey(taskID)).Scan(&raw, &size, &region, &expiry)
	if err != nil {
		return nil, err
	}
	if size < 1 || size > notificationProgressRecordLimit || !raw.Valid || int64(len(raw.String)) != size || !region.Valid || region.String != "notification_events" || len(expiry) > 64 {
		return nil, errNotificationProgressState
	}
	r, err := decodeNotificationProgressRecord(raw.String)
	if err != nil {
		return nil, err
	}
	if err = r.validate(taskID, time.Now()); err != nil {
		return nil, err
	}
	// Read SQL's naive wall time as text, preserving application time even if a
	// MySQL driver's loc differs. Current schemas store six fractional digits;
	// legacy DATETIME(0) may retain only the exact whole-second truncation.
	expires, err := time.Parse("2006-01-02T15:04:05.999999999", strings.ReplaceAll(expiry, " ", "T"))
	expected := r.ExpiresAt.In(s.authLocation())
	wall := expires.Format("2006-01-02T15:04:05.999999")
	if err != nil || expires.Nanosecond()%1000 != 0 || (wall != expected.Truncate(time.Microsecond).Format("2006-01-02T15:04:05.999999") && wall != expected.Truncate(time.Second).Format("2006-01-02T15:04:05")) {
		return nil, errNotificationProgressState
	}
	return r, nil
}

func notificationProgressTransition(before, after *notificationProgressRecord) error {
	if before.Version != after.Version || before.TaskID != after.TaskID || !before.CreatedAt.Equal(after.CreatedAt) || !before.ExpiresAt.Equal(after.ExpiresAt) || len(before.Targets) != len(after.Targets) {
		return errNotificationProgressState
	}
	for i, old := range before.Targets {
		next := after.Targets[i]
		if old.Target != next.Target || old.Attempt > next.Attempt || old.LastProgress > next.LastProgress || old.Attempted && !next.Attempted || old.Uncertain && !next.Uncertain || old.Terminal && !next.Terminal || (old.Receipt != nil && (next.Receipt == nil || *old.Receipt != *next.Receipt)) {
			return errNotificationProgressState
		}
		if (old.Uncertain || old.Terminal) && (old.Attempt != next.Attempt || old.LastHash != next.LastHash || old.LastAction != next.LastAction || old.LastProgress != next.LastProgress) {
			return errNotificationProgressState
		}
		if next.Attempt > old.Attempt+1 || (next.Attempt == old.Attempt && (old.LastHash != next.LastHash || old.LastAction != next.LastAction || old.LastProgress != next.LastProgress)) {
			return errNotificationProgressState
		}
		if (next.Attempt > old.Attempt && (old.Pending || !next.Pending)) || (!old.Pending && next.Pending && old.Attempt == next.Attempt) {
			return errNotificationProgressState
		}
		// Only the first recorded send can acquire a receipt. Later edits always
		// retain it, including failed or ambiguous edits.
		if old.Receipt == nil && next.Receipt != nil && (!old.Attempted || !old.Pending || next.Pending || old.LastAction != "send" || old.Attempt != next.Attempt || old.Uncertain) {
			return errNotificationProgressState
		}
	}
	return nil
}

func (s *Server) mutateNotificationProgressRecord(ctx context.Context, taskID string, initial *notificationProgressRecord, fn func(*notificationProgressRecord) error) (*notificationProgressRecord, error) {
	ctx, cancel := context.WithTimeout(ctx, notificationProgressStoreTimeout)
	defer cancel()
	if fn == nil {
		return nil, errNotificationProgressState
	}
	// A canceled waiter must not wait indefinitely behind another caller. No
	// goroutine can outlive cancellation while still waiting to acquire the lock.
	for !s.notificationProgressStoreMu.TryLock() {
		timer := time.NewTimer(5 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
	defer s.notificationProgressStoreMu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	tx, err := s.Store.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	r, err := s.readNotificationProgressRecordTx(ctx, tx, taskID, true)
	fresh := errors.Is(err, sql.ErrNoRows)
	if fresh {
		if err = initial.validate(taskID, time.Now()); err != nil {
			return nil, err
		}
		r = cloneNotificationProgressRecord(initial)
	} else if err != nil {
		return nil, err
	}
	before := cloneNotificationProgressRecord(r)
	if err = fn(r); err != nil {
		return nil, err
	}
	if err = r.validate(taskID, time.Now()); err != nil {
		return nil, err
	}
	if err = notificationProgressTransition(before, r); err != nil {
		return nil, err
	}
	raw, err := json.Marshal(r)
	if err != nil || len(raw) > notificationProgressRecordLimit {
		return nil, errNotificationProgressState
	}
	if fresh {
		_, err = s.Store.InsertTx(ctx, tx, "cache_data", store.Row{"cache_key": notificationProgressKey(taskID), "cache_provider": "notification_events", "cache_value": string(raw), "expires_at": r.ExpiresAt.In(s.authLocation()).Format("2006-01-02T15:04:05.999999")})
	} else {
		previous, _ := json.Marshal(before)
		if !bytes.Equal(previous, raw) {
			var result sql.Result
			result, err = tx.ExecContext(ctx, s.Store.Rebind("UPDATE cache_data SET cache_value=? WHERE cache_key=?"), string(raw), notificationProgressKey(taskID))
			if err == nil {
				var count int64
				count, err = result.RowsAffected()
				if err == nil && count != 1 {
					err = errNotificationProgressState
				}
			}
		}
	}
	if err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return cloneNotificationProgressRecord(r), nil
}
