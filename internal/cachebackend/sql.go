// SPDX-License-Identifier: AGPL-3.0-only
package cachebackend

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"sync/atomic"
	"time"

	"github.com/AniBakaBaka/AniDan/internal/store"
)

const sqlProvider = "anidan_ephemeral_v1"
const sqlMetaProvider = "anidan_ephemeral_meta_v1"

type sqlBackend struct {
	store         *store.Store
	options       Options
	closed        atomic.Bool
	expiryQuantum time.Duration
}

func newSQL(ctx context.Context, o Options, db *store.Store) (backend, error) {
	var e error
	s := &sqlBackend{store: db, options: o, expiryQuantum: time.Microsecond}
	if db.Dialect == "mysql" {
		probeCtx, cancel := context.WithTimeout(ctx, o.SocketTimeout)
		defer cancel()
		var precision sql.NullInt64
		e = db.DB.QueryRowContext(probeCtx, "SELECT DATETIME_PRECISION FROM information_schema.COLUMNS WHERE TABLE_SCHEMA=DATABASE() AND TABLE_NAME='cache_data' AND COLUMN_NAME='expires_at'").Scan(&precision)
		if e != nil {
			return nil, cacheError("database timestamp precision", preferContext(probeCtx, e))
		}
		if !precision.Valid || precision.Int64 < 0 || precision.Int64 > 6 {
			return nil, ErrCorrupt
		}
		s.expiryQuantum = time.Second
		for i := int64(0); i < precision.Int64; i++ {
			s.expiryQuantum /= 10
		}
	}
	_, e = s.generation(ctx)
	if e != nil {
		return nil, e
	}
	return s, nil
}

// Newly owned cache indexes always use UTC wall values in the legacy naive
// column. Their markers are excluded from legacy local-time cleanup. This avoids
// DST folds and application timezone changes corrupting expiry ordering.
func (s *sqlBackend) wall(t time.Time) any {
	v := t.UTC().Format("2006-01-02T15:04:05.999999")
	if s.store.Dialect != "postgres" {
		// MySQL's time.Time binding applies DSN loc. A wall-clock string avoids
		// converting our UTC-owned DATETIME indexes when driver locations differ.
		return v
	}
	parsed, _ := time.Parse("2006-01-02T15:04:05.999999", v)
	return parsed
}

// Legacy MySQL DATETIME(0) truncates fractional seconds. Rounding cleanup
// timestamps upward prevents purge from deleting an envelope that is still live.
// Exact serving and diagnostic expiry come from the bounded UTC envelope header.
func (s *sqlBackend) indexWall(t time.Time) any {
	rounded := t.Truncate(s.expiryQuantum)
	if rounded.Before(t) {
		rounded = rounded.Add(s.expiryQuantum)
	}
	return s.wall(rounded)
}
func (s *sqlBackend) cutoff(t time.Time) any { return s.wall(t) }
func (s *sqlBackend) prefix(region string) string {
	p := namespacePrefix(s.options.Namespace)
	if region != "" {
		p += region + ":"
	}
	return p
}

// The provider equality AND exact prefix equality are mandatory positive
// ownership predicates. No migrated or durable cache_data rows are adopted.
func (s *sqlBackend) owned(region string) (string, []any) {
	p := s.prefix(region)
	return "cache_provider=? AND SUBSTR(cache_key,1,?)=?", []any{sqlProvider, len(p), p}
}
func (s *sqlBackend) bytes(column string) string {
	if s.store.Dialect == "sqlite" {
		return "LENGTH(CAST(" + column + " AS BLOB))"
	}
	return "OCTET_LENGTH(" + column + ")"
}
func (s *sqlBackend) size() string { return s.bytes("cache_key") + "+" + s.bytes("cache_value") }
func (s *sqlBackend) check(ctx context.Context) error {
	if e := ctx.Err(); e != nil {
		return e
	}
	if s.closed.Load() {
		return ErrClosed
	}
	return nil
}
func (s *sqlBackend) get(ctx context.Context, key string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, s.options.SocketTimeout)
	defer cancel()
	if e := s.check(ctx); e != nil {
		return nil, e
	}
	var b sql.NullString
	query := "SELECT CASE WHEN " + s.bytes("cache_value") + "<=? THEN cache_value ELSE NULL END FROM cache_data WHERE cache_key=? AND cache_provider=?"
	e := s.store.DB.QueryRowContext(ctx, s.store.Rebind(query), maxEnvelope(s.options), key, sqlProvider).Scan(&b)
	if errors.Is(e, sql.ErrNoRows) {
		return nil, ErrMiss
	}
	if e != nil {
		return nil, cacheError("database read", preferContext(ctx, e))
	}
	if !b.Valid {
		return nil, ErrTooLarge
	}
	return []byte(b.String), nil
}
func preferContext(ctx context.Context, err error) error {
	if e := ctx.Err(); e != nil {
		return e
	}
	// Socket deadlines can wake a read just before the context timer callback.
	// Preserve the deadline classification instead of racing into a generic
	// transport error when the same caller deadline has already elapsed.
	if err != nil {
		if deadline, ok := ctx.Deadline(); ok && !time.Now().Before(deadline) {
			return context.DeadlineExceeded
		}
	}
	return err
}

// lock creates/locks one small, positively owned metadata row per namespace.
// Unlike a process-only mutex, the database transaction lock also makes quota
// admission and clear-generation checks atomic across service instances.
func (s *sqlBackend) lock(ctx context.Context) (*sql.Tx, string, error) {
	if e := s.check(ctx); e != nil {
		return nil, "", e
	}
	tx, e := s.store.DB.BeginTx(ctx, nil)
	if e != nil {
		return nil, "", cacheError("database transaction", preferContext(ctx, e))
	}
	fail := func(e error) (*sql.Tx, string, error) { _ = tx.Rollback(); return nil, "", e }
	key := s.prefix("") + "!generation"
	seed := nonce()
	q := "INSERT INTO cache_data (cache_key,cache_value,cache_provider,expires_at) VALUES (?,?,?,?) ON CONFLICT(cache_key) DO NOTHING"
	if s.store.Dialect == "mysql" {
		q = "INSERT INTO cache_data (cache_key,cache_value,cache_provider,expires_at) VALUES (?,?,?,?) ON DUPLICATE KEY UPDATE cache_key=cache_key"
	}
	if _, e = tx.ExecContext(ctx, s.store.Rebind(q), key, seed, sqlMetaProvider, s.indexWall(time.Now().Add(MaxTTL))); e != nil {
		return fail(cacheError("database lock", preferContext(ctx, e)))
	}
	// UPDATE obtains the row lock on every supported dialect, including SQLite.
	if _, e = tx.ExecContext(ctx, s.store.Rebind("UPDATE cache_data SET cache_value=CASE WHEN expires_at<=? THEN ? ELSE cache_value END,expires_at=? WHERE cache_key=? AND cache_provider=?"), s.wall(time.Now()), seed, s.indexWall(time.Now().Add(MaxTTL)), key, sqlMetaProvider); e != nil {
		return fail(cacheError("database lock", preferContext(ctx, e)))
	}
	var epoch sql.NullString
	q = "SELECT CASE WHEN " + s.bytes("cache_value") + "=32 THEN cache_value ELSE NULL END FROM cache_data WHERE cache_key=? AND cache_provider=?"
	if e = tx.QueryRowContext(ctx, s.store.Rebind(q), key, sqlMetaProvider).Scan(&epoch); e != nil {
		if errors.Is(e, sql.ErrNoRows) {
			return fail(ErrOwnership)
		}
		return fail(cacheError("database generation", preferContext(ctx, e)))
	}
	if !epoch.Valid || !isNonce(epoch.String) {
		return fail(ErrCorrupt)
	}
	return tx, epoch.String, nil
}
func isNonce(v string) bool {
	if len(v) != 32 {
		return false
	}
	for _, c := range v {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}
func (s *sqlBackend) bump(ctx context.Context, tx *sql.Tx) error {
	_, e := tx.ExecContext(ctx, s.store.Rebind("UPDATE cache_data SET cache_value=?,expires_at=? WHERE cache_key=? AND cache_provider=?"), nonce(), s.indexWall(time.Now().Add(MaxTTL)), s.prefix("")+"!generation", sqlMetaProvider)
	return e
}
func (s *sqlBackend) purge(ctx context.Context, tx *sql.Tx) error {
	w, a := s.owned("")
	a = append(a, s.cutoff(time.Now()))
	_, e := tx.ExecContext(ctx, s.store.Rebind("DELETE FROM cache_data WHERE "+w+" AND expires_at<=?"), a...)
	return e
}
func (s *sqlBackend) set(ctx context.Context, key string, b []byte, expires time.Time, expected string) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, s.options.SocketTimeout)
	defer cancel()
	if len(b) > maxEnvelope(s.options) || (s.store.Dialect == "mysql" && len(b) > 16777215) {
		return false, ErrTooLarge
	}
	if !expires.After(time.Now()) {
		return false, ErrMiss
	}
	tx, epoch, e := s.lock(ctx)
	if e != nil {
		return false, e
	}
	defer tx.Rollback()
	if expected != "" && expected != epoch {
		return false, nil
	}
	if e = s.purge(ctx, tx); e != nil {
		return false, cacheError("database cleanup", preferContext(ctx, e))
	}
	var ownsExisting int
	var oldBytes int64
	e = tx.QueryRowContext(ctx, s.store.Rebind("SELECT CASE WHEN cache_provider=? THEN 1 ELSE 0 END,"+s.size()+" FROM cache_data WHERE cache_key=?"), sqlProvider, key).Scan(&ownsExisting, &oldBytes)
	if e != nil && !errors.Is(e, sql.ErrNoRows) {
		return false, cacheError("database quota", preferContext(ctx, e))
	}
	exists := e == nil
	if exists && ownsExisting != 1 {
		return false, ErrOwnership
	}
	w, a := s.owned("")
	var count, used int64
	e = tx.QueryRowContext(ctx, s.store.Rebind("SELECT COUNT(*),COALESCE(SUM("+s.size()+"),0) FROM cache_data WHERE "+w), a...).Scan(&count, &used)
	if e != nil {
		return false, cacheError("database quota", preferContext(ctx, e))
	}
	if exists {
		count--
		used -= oldBytes
	}
	if count >= int64(s.options.MaxEntries) || used+int64(len(key)+len(b)) > s.options.MaxBytes {
		return false, ErrQuota
	}
	if exists {
		_, e = tx.ExecContext(ctx, s.store.Rebind("UPDATE cache_data SET cache_value=?,expires_at=? WHERE cache_key=? AND cache_provider=?"), string(b), s.indexWall(expires), key, sqlProvider)
	} else {
		_, e = tx.ExecContext(ctx, s.store.Rebind("INSERT INTO cache_data (cache_key,cache_value,cache_provider,expires_at) VALUES (?,?,?,?)"), key, string(b), sqlProvider, s.indexWall(expires))
	}
	if e != nil {
		return false, cacheError("database write", preferContext(ctx, e))
	}
	if e = tx.Commit(); e != nil {
		return false, cacheError("database commit", preferContext(ctx, e))
	}
	return true, nil
}
func (s *sqlBackend) generation(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, s.options.SocketTimeout)
	defer cancel()
	tx, epoch, e := s.lock(ctx)
	if e != nil {
		return "", e
	}
	defer tx.Rollback()
	if e = tx.Commit(); e != nil {
		return "", cacheError("database generation", preferContext(ctx, e))
	}
	return epoch, nil
}
func (s *sqlBackend) delete(ctx context.Context, key string) error {
	ctx, cancel := context.WithTimeout(ctx, s.options.SocketTimeout)
	defer cancel()
	tx, _, e := s.lock(ctx)
	if e != nil {
		return e
	}
	defer tx.Rollback()
	if e = s.bump(ctx, tx); e == nil {
		_, e = tx.ExecContext(ctx, s.store.Rebind("DELETE FROM cache_data WHERE cache_key=? AND cache_provider=?"), key, sqlProvider)
	}
	if e == nil {
		e = tx.Commit()
	}
	return cacheError("database delete", preferContext(ctx, e))
}
func (s *sqlBackend) clear(ctx context.Context, region string) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, s.options.SocketTimeout)
	defer cancel()
	tx, _, e := s.lock(ctx)
	if e != nil {
		return 0, e
	}
	defer tx.Rollback()
	if e = s.bump(ctx, tx); e != nil {
		return 0, cacheError("database clear", preferContext(ctx, e))
	}
	if e = s.purge(ctx, tx); e != nil {
		return 0, cacheError("database cleanup", preferContext(ctx, e))
	}
	w, a := s.owned(region)
	r, e := tx.ExecContext(ctx, s.store.Rebind("DELETE FROM cache_data WHERE "+w), a...)
	if e != nil {
		return 0, cacheError("database clear", preferContext(ctx, e))
	}
	n, e := r.RowsAffected()
	if e == nil {
		e = tx.Commit()
	}
	if e != nil {
		return 0, cacheError("database clear", preferContext(ctx, e))
	}
	return int(n), nil
}

// envelopeExpiry reads only the fixed v1 control header emitted by encodeEntry.
// SQL SUBSTR limits the selected text to 96 characters before driver allocation.
func envelopeExpiry(header string) (time.Time, error) {
	d := json.NewDecoder(bytes.NewBufferString(header))
	opening, e := d.Token()
	if e != nil || opening != json.Delim('{') {
		return time.Time{}, ErrCorrupt
	}
	field, e := d.Token()
	if e != nil || field != "v" {
		return time.Time{}, ErrCorrupt
	}
	var version int
	if e = d.Decode(&version); e != nil || version != 1 {
		return time.Time{}, ErrCorrupt
	}
	field, e = d.Token()
	if e != nil || field != "expiresAt" {
		return time.Time{}, ErrCorrupt
	}
	var expires time.Time
	if e = d.Decode(&expires); e != nil || expires.IsZero() {
		return time.Time{}, ErrCorrupt
	}
	return expires, nil
}
func (s *sqlBackend) stats(ctx context.Context, region string) (Stats, error) {
	ctx, cancel := context.WithTimeout(ctx, s.options.SocketTimeout)
	defer cancel()
	if e := s.check(ctx); e != nil {
		return Stats{}, e
	}
	w, a := s.owned(region)
	a = append(a, s.cutoff(time.Now()))
	q := "SELECT " + s.size() + ",SUBSTR(cache_value,1,96) FROM cache_data WHERE " + w + " AND expires_at>? LIMIT 65537"
	rows, e := s.store.DB.QueryContext(ctx, s.store.Rebind(q), a...)
	if e != nil {
		return Stats{}, cacheError("database stats", preferContext(ctx, e))
	}
	defer rows.Close()
	var out Stats
	scanned := 0
	now := time.Now()
	for rows.Next() {
		scanned++
		if scanned > 65536 {
			return Stats{}, ErrQuota
		}
		var size int64
		var header string
		if e = rows.Scan(&size, &header); e != nil {
			return Stats{}, cacheError("database stats", preferContext(ctx, e))
		}
		expires, e := envelopeExpiry(header)
		if e != nil {
			return Stats{}, e
		}
		if expires.After(now) {
			out.Entries++
			out.Bytes += size
		}
	}
	if e = rows.Err(); e != nil {
		return Stats{}, cacheError("database stats", preferContext(ctx, e))
	}
	return out, nil
}
func (s *sqlBackend) list(ctx context.Context, region, after string, limit int) (Page, error) {
	ctx, cancel := context.WithTimeout(ctx, s.options.SocketTimeout)
	defer cancel()
	if e := s.check(ctx); e != nil {
		return Page{}, e
	}
	w, a := s.owned(region)
	a = append(a, s.cutoff(time.Now()), after)
	q := "SELECT CASE WHEN " + s.bytes("cache_key") + "<=256 THEN cache_key ELSE NULL END," + s.size() + ",SUBSTR(cache_value,1,96) FROM cache_data WHERE " + w + " AND expires_at>? AND cache_key>? ORDER BY cache_key LIMIT 65537"
	rows, e := s.store.DB.QueryContext(ctx, s.store.Rebind(q), a...)
	if e != nil {
		return Page{}, cacheError("database list", preferContext(ctx, e))
	}
	defer rows.Close()
	out := Page{Items: []Item{}}
	scanned := 0
	now := time.Now()
	for rows.Next() {
		scanned++
		if scanned > 65536 {
			return Page{}, ErrQuota
		}
		var physical sql.NullString
		var size int64
		var header string
		if e = rows.Scan(&physical, &size, &header); e != nil {
			return Page{}, cacheError("database list", preferContext(ctx, e))
		}
		if !physical.Valid {
			return Page{}, ErrCorrupt
		}
		expires, e := envelopeExpiry(header)
		if e != nil {
			return Page{}, e
		}
		if !expires.After(now) {
			continue
		}
		if len(out.Items) == limit {
			out.Next = out.Items[len(out.Items)-1].Key
			break
		}
		item, e := itemFromKey(physical.String, size, expires)
		if e != nil {
			return Page{}, e
		}
		out.Items = append(out.Items, item)
	}
	if e = rows.Err(); e != nil {
		return Page{}, cacheError("database list", preferContext(ctx, e))
	}
	return out, nil
}
func (s *sqlBackend) close() error { s.closed.Store(true); return nil }
