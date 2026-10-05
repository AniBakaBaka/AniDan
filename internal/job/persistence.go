package job

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

const taskColumns = "id, title, status, progress, description, created_at, updated_at, finished_at, queue_type, task_type, task_parameters, scheduled_task_id, unique_key"
const leaseKey = "anidan.job.manager.lease"
const resultPrefix = "anidan.job.result."
const leaseDuration = 30 * time.Second

func newID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	s := hex.EncodeToString(b[:])
	return s[:8] + "-" + s[8:12] + "-" + s[12:16] + "-" + s[16:20] + "-" + s[20:], nil
}
func (m *Manager) bind(query string) string {
	if m.store.Dialect != "postgresql" && m.store.Dialect != "postgres" {
		return query
	}
	var b strings.Builder
	n := 0
	for _, r := range query {
		if r == '?' {
			n++
			fmt.Fprintf(&b, "$%d", n)
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}
func (m *Manager) dbTime(t time.Time) string {
	return t.In(m.location).Format("2006-01-02 15:04:05.000000")
}
func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}
func (m *Manager) parseTime(v any) (*time.Time, error) {
	if v == nil {
		return nil, nil
	}
	if t, ok := v.(time.Time); ok {
		return &t, nil
	}
	s := asString(v)
	if s == "" {
		return nil, nil
	}
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02 15:04:05.999999999-07:00", "2006-01-02 15:04:05.999999999", "2006-01-02T15:04:05.999999999", "2006-01-02 15:04:05"} {
		if t, err := time.ParseInLocation(layout, s, m.location); err == nil {
			return &t, nil
		}
	}
	return nil, fmt.Errorf("invalid persisted task timestamp %q", s)
}
func asString(v any) string {
	if v == nil {
		return ""
	}
	if b, ok := v.([]byte); ok {
		return string(b)
	}
	return fmt.Sprint(v)
}

type scanner interface{ Scan(...any) error }

func (m *Manager) scanTask(row scanner) (Task, error) {
	var t Task
	var title, desc, queue, kind, params, schedule, unique sql.NullString
	var created, updated, finished any
	err := row.Scan(&t.ID, &title, &t.Status, &t.Progress, &desc, &created, &updated, &finished, &queue, &kind, &params, &schedule, &unique)
	if err != nil {
		return t, err
	}
	t.RecoveryReviewRequired = m.recoveryReview && (t.Status == Pending || t.Status == Paused)
	t.Title = title.String
	t.Description = desc.String
	t.QueueType = queue.String
	if t.QueueType == "" {
		t.QueueType = "download"
	}
	t.Kind = kind.String
	t.Params = json.RawMessage(params.String)
	t.ScheduledTaskID = schedule.String
	t.UniqueKey = unique.String
	t.IsSystemTask = strings.HasPrefix(t.ScheduledTaskID, "system_")
	c, err := m.parseTime(created)
	if err != nil {
		return t, err
	}
	u, err := m.parseTime(updated)
	if err != nil {
		return t, err
	}
	t.FinishedAt, err = m.parseTime(finished)
	if err != nil {
		return t, err
	}
	if c != nil {
		t.CreatedAt = *c
	}
	if u != nil {
		t.UpdatedAt = *u
	}
	return t, nil
}
func (m *Manager) insertTask(ctx context.Context, tx *sql.Tx, t Task) error {
	_, err := tx.ExecContext(ctx, m.bind("INSERT INTO task_history ("+taskColumns+") VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)"), t.ID, t.Title, t.Status, t.Progress, t.Description, m.dbTime(t.CreatedAt), m.dbTime(t.UpdatedAt), nil, t.QueueType, nullString(t.Kind), string(t.Params), nullString(t.ScheduledTaskID), nullString(t.UniqueKey))
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, m.bind("INSERT INTO task_state_cache (task_id, task_type, task_parameters, created_at, updated_at) VALUES (?, ?, ?, ?, ?)"), t.ID, t.Kind, string(t.Params), m.dbTime(t.CreatedAt), m.dbTime(t.UpdatedAt))
	return err
}
func (m *Manager) setStatus(ctx context.Context, id, status, desc string, progress int, finished bool) error {
	var end any
	if finished {
		end = m.dbTime(time.Now())
	}
	_, err := m.store.DB.ExecContext(ctx, m.bind("UPDATE task_history SET status = ?, description = ?, progress = ?, updated_at = ?, finished_at = ? WHERE id = ?"), status, desc, progress, m.dbTime(time.Now()), end, id)
	return err
}
func (m *Manager) putConfig(ctx context.Context, tx *sql.Tx, key, value string) error {
	query := "INSERT INTO config (config_key, config_value, description) VALUES (?, ?, ?) ON CONFLICT (config_key) DO UPDATE SET config_value = excluded.config_value"
	if m.store.Dialect == "mysql" {
		query = "INSERT INTO config (config_key, config_value, description) VALUES (?, ?, ?) ON DUPLICATE KEY UPDATE config_value = VALUES(config_value)"
	}
	_, err := tx.ExecContext(ctx, m.bind(query), key, value, "AniDan task execution metadata")
	return err
}
func (m *Manager) finish(ctx context.Context, t Task, status, desc string, result json.RawMessage, at time.Time) (Task, bool, error) {
	tx, err := m.store.DB.BeginTx(ctx, nil)
	if err != nil {
		return Task{}, false, err
	}
	defer tx.Rollback()
	// Progress SQL runs outside m.mu. Preserve its persisted value atomically,
	// without a read-then-write transaction upgrade on single-connection SQLite.
	res, err := tx.ExecContext(ctx, m.bind("UPDATE task_history SET status = ?, description = ?, progress = CASE WHEN ? = ? THEN 100 WHEN progress > ? THEN progress ELSE ? END, updated_at = ?, finished_at = ? WHERE id = ? AND status IN (?, ?)"), status, desc, status, Completed, t.Progress, t.Progress, m.dbTime(at), m.dbTime(at), t.ID, Running, Paused)
	if err != nil {
		return Task{}, false, err
	}
	n, err := res.RowsAffected()
	if err != nil || n != 1 {
		return Task{}, false, err
	}
	var progress int
	if err = tx.QueryRowContext(ctx, m.bind("SELECT progress FROM task_history WHERE id = ?"), t.ID).Scan(&progress); err != nil {
		return Task{}, false, err
	}
	if len(result) > 0 {
		if err = m.putConfig(ctx, tx, resultPrefix+t.ID, string(result)); err != nil {
			return Task{}, false, err
		}
	}
	if _, err = tx.ExecContext(ctx, m.bind("DELETE FROM task_state_cache WHERE task_id = ?"), t.ID); err != nil {
		return Task{}, false, err
	}
	if err = tx.Commit(); err != nil {
		return Task{}, false, err
	}
	t.Status, t.Description, t.Progress = status, desc, progress
	t.Result, t.UpdatedAt, t.FinishedAt = result, at, &at
	return t, true, nil
}

func (m *Manager) acquireLease(ctx context.Context) error {
	token, err := newID()
	if err != nil {
		return err
	}
	m.owner = token
	tx, err := m.store.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	emptyInsert := "INSERT INTO config (config_key, config_value, description) VALUES (?, ?, ?) ON CONFLICT (config_key) DO NOTHING"
	if m.store.Dialect == "mysql" {
		emptyInsert = "INSERT IGNORE INTO config (config_key, config_value, description) VALUES (?, ?, ?)"
	}
	if _, err = tx.ExecContext(ctx, m.bind(emptyInsert), leaseKey, "", "AniDan single manager lease"); err != nil {
		return err
	}
	var old string
	if err = tx.QueryRowContext(ctx, m.bind("SELECT config_value FROM config WHERE config_key = ?"), leaseKey).Scan(&old); err != nil {
		return err
	}
	parts := strings.Split(old, "|")
	if len(parts) == 2 {
		n, e := strconv.ParseInt(parts[1], 10, 64)
		if e != nil {
			return fmt.Errorf("invalid manager lease: %w", e)
		}
		if time.Now().Before(time.Unix(0, n)) {
			return ErrManagerActive
		}
	} else if old != "" {
		return errors.New("invalid manager lease metadata")
	}
	m.leaseValue = token + "|" + strconv.FormatInt(time.Now().Add(leaseDuration).UnixNano(), 10)
	res, err := tx.ExecContext(ctx, m.bind("UPDATE config SET config_value = ? WHERE config_key = ? AND config_value = ?"), m.leaseValue, leaseKey, old)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrManagerActive
	}
	return tx.Commit()
}
func (m *Manager) renewLease(ctx context.Context) error {
	next := m.owner + "|" + strconv.FormatInt(time.Now().Add(leaseDuration).UnixNano(), 10)
	res, err := m.store.DB.ExecContext(ctx, m.bind("UPDATE config SET config_value = ? WHERE config_key = ? AND config_value = ?"), next, leaseKey, m.leaseValue)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrManagerActive
	}
	m.leaseValue = next
	return nil
}
func (m *Manager) releaseLease(ctx context.Context) error {
	_, err := m.store.DB.ExecContext(ctx, m.bind("DELETE FROM config WHERE config_key = ? AND config_value = ?"), leaseKey, m.leaseValue)
	return err
}

func (m *Manager) recoverInterrupted(ctx context.Context) error {
	tx, err := m.store.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// Running jobs may have committed an external effect. Never re-execute them.
	_, err = tx.ExecContext(ctx, m.bind("UPDATE task_history SET status = ?, description = ?, updated_at = ?, finished_at = ? WHERE status = ?"), Failed, "Interrupted by service restart; inspect effects before retrying", m.dbTime(time.Now()), m.dbTime(time.Now()), Running)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, m.bind("DELETE FROM task_state_cache WHERE task_id IN (SELECT id FROM task_history WHERE status IN (?, ?, ?, ?))"), Failed, Completed, "成功", "已取消")
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (m *Manager) loadRecoveryReview(ctx context.Context) error {
	var value string
	err := m.store.DB.QueryRowContext(ctx, m.bind("SELECT config_value FROM config WHERE config_key = ?"), RecoveryGateKey).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	switch strings.ToLower(value) {
	case "true", "1":
		m.recoveryReview = true
	case "false", "0", "":
		m.recoveryReview = false
	default:
		return errors.New("invalid restored-job review gate value")
	}
	return nil
}

func (m *Manager) recordExecutionMetric(ctx context.Context, id, kind string, duration time.Duration, status, description string) error {
	m.mu.Lock()
	now := m.dbTime(time.Now())
	m.mu.Unlock()
	ms := float64(duration) / float64(time.Millisecond)
	if ms < 0 {
		ms = 0
	}
	if ms > 9999999999.99 {
		ms = 9999999999.99
	}
	value := fmt.Sprintf("%.2f", ms)
	_, err := m.store.DB.ExecContext(ctx, m.bind("INSERT INTO task_perf_events (flow_type,correlation_id,step_name,duration_ms,success,details,total_duration_ms,created_at) VALUES (?,?,?,?,?,?,?,?)"), truncate(kind, 100), id, "execute", value, status == Completed, truncate(description, 500), value, now)
	return err
}
