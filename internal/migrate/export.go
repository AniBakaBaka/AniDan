// SPDX-License-Identifier: AGPL-3.0-only
package migrate

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/AniBakaBaka/AniDan/internal/store"
)

// Export writes an exclusive new v2 JSON snapshot using a repeatable-read,
// read-only transaction. It does not call Init or run upstream application code.
// Supply a dedicated read-only database account. The SQL transaction also enforces
// read-only mode; file roots must still be quiesced separately by the operator.
func Export(ctx context.Context, driver, dsn, destination string) error {
	return exportWithLimits(ctx, driver, dsn, destination, 0, 0)
}

// The remote-target bridge applies explicit input/disk bounds. Existing Export
// callers retain their original unrestricted snapshot interface.
func exportWithLimits(ctx context.Context, driver, dsn, destination string, maxBytes, maxRowBytes int64) error {
	s, e := store.OpenReadOnly(ctx, driver, dsn)
	if e != nil {
		return e
	}
	defer s.Close()
	isolation := sql.LevelRepeatableRead
	if s.Dialect == "sqlite" {
		isolation = sql.LevelSerializable
	}
	tx, e := s.DB.BeginTx(ctx, &sql.TxOptions{ReadOnly: true, Isolation: isolation})
	if e != nil {
		return fmt.Errorf("start read-only snapshot: %w", e)
	}
	defer tx.Rollback()
	return exportSnapshotTransaction(ctx, s, tx, destination, maxBytes, maxRowBytes, time.Now(), nil)
}

// exportSnapshotTransaction consumes the caller's read-only transaction. Schema
// verification precedes every size expression and data row read.
func exportSnapshotTransaction(ctx context.Context, s *store.Store, tx *sql.Tx, destination string, maxBytes, maxRowBytes int64, capturedAt time.Time, afterSchema func() error) error {
	var e error
	if e = verifySourceTables(ctx, s, tx); e != nil {
		return e
	}
	if e = s.VerifySchemaWith(ctx, tx); e != nil {
		return e
	}
	if afterSchema != nil {
		if e = afterSchema(); e != nil {
			return e
		}
	}
	f, e := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if e != nil {
		return e
	}
	out := &boundedSnapshotWriter{file: f, max: maxBytes}
	complete := false
	defer func() {
		f.Close()
		if !complete {
			os.Remove(destination)
		}
	}()
	if _, e = out.WriteString(`{"data":{`); e != nil {
		return e
	}
	counts := map[string]int64{}
	var total int64
	for ti, name := range store.Tables() {
		if ti > 0 {
			if _, e = out.WriteString(","); e != nil {
				return e
			}
		}
		key, _ := json.Marshal(name)
		if _, e = out.Write(append(key, []byte(":[")...)); e != nil {
			return e
		}
		query := "SELECT * FROM " + s.Quote(name)
		var queryArgs []any
		if maxRowBytes > 0 {
			size := sourceRowBytesExpression(s, store.Schema[name])
			var oversized int
			if e = tx.QueryRowContext(ctx, s.Rebind("SELECT COUNT(*) FROM "+s.Quote(name)+" WHERE "+size+">?"), maxRowBytes).Scan(&oversized); e != nil {
				return errors.New("cannot bound source snapshot rows")
			}
			if oversized != 0 {
				return fmt.Errorf("source table %s exceeds configured row byte limit", name)
			}
			query += " WHERE " + size + "<=?"
			query = s.Rebind(query)
			queryArgs = []any{maxRowBytes}
		}
		rows, e := tx.QueryContext(ctx, query, queryArgs...)
		if e != nil {
			return e
		}
		cols, e := rows.Columns()
		if e != nil {
			rows.Close()
			return e
		}
		t := store.Schema[name]
		if len(cols) != len(t.Columns) {
			rows.Close()
			return fmt.Errorf("source schema drift: %s column count differs", name)
		}
		for _, c := range cols {
			if _, ok := t.Column(c); !ok {
				rows.Close()
				return fmt.Errorf("source schema drift: unknown %s.%s", name, c)
			}
		}
		var count int64
		for rows.Next() {
			if e = ctx.Err(); e != nil {
				rows.Close()
				return e
			}
			r, e := store.ScanRow(rows, t)
			if e != nil {
				rows.Close()
				return e
			}
			for _, column := range t.Columns {
				if value, ok := r[column.Name].(string); ok && !utf8.ValidString(value) {
					rows.Close()
					return fmt.Errorf("source table %s contains invalid UTF-8", name)
				}
				if column.Kind == "decimal" && r[column.Name] != nil {
					r[column.Name] = json.Number(r[column.Name].(string))
				}
			}
			if count > 0 {
				if _, e = out.WriteString(","); e != nil {
					rows.Close()
					return e
				}
			}
			encoded, encodeErr := json.Marshal(r)
			if encodeErr != nil {
				rows.Close()
				return encodeErr
			}
			if maxRowBytes > 0 && int64(len(encoded)) > maxRowBytes {
				rows.Close()
				return fmt.Errorf("source table %s JSON exceeds configured row byte limit", name)
			}
			if _, e = out.Write(append(encoded, '\n')); e != nil {
				rows.Close()
				return e
			}
			count++
		}
		e = rows.Err()
		rows.Close()
		if e != nil {
			return e
		}
		if _, e = out.WriteString("]"); e != nil {
			return e
		}
		counts[name] = count
		total += count
	}
	meta := Metadata{"2.0", s.Dialect, capturedAt.Format("2006-01-02T15:04:05.999999"), store.Tables(), counts, total}
	if _, e = out.WriteString(`},"metadata":`); e != nil {
		return e
	}
	if e = json.NewEncoder(out).Encode(meta); e != nil {
		return e
	}
	if _, e = out.WriteString("}\n"); e != nil {
		return e
	}
	if e = tx.Commit(); e != nil {
		return e
	}
	if e = f.Sync(); e != nil {
		return e
	}
	if e = f.Close(); e != nil {
		return e
	}
	complete = true
	return nil
}

// Include every value, including unbounded PostgreSQL NUMERIC and native enum
// representations. The schema has already been verified before this expression
// is used. The JSON bound below additionally accounts for escaping and keys.
func sourceRowBytesExpression(s *store.Store, t store.Table) string {
	parts := []string{"0"}
	for _, column := range t.Columns {
		value := s.Quote(column.Name)
		switch s.Dialect {
		case "sqlite":
			value = "length(CAST(" + value + " AS BLOB))"
		case "postgres":
			value = "octet_length(CAST(" + value + " AS TEXT))"
		default:
			value = "octet_length(" + value + ")"
		}
		parts = append(parts, "COALESCE("+value+",0)")
	}
	return "(" + strings.Join(parts, "+") + ")"
}
func verifySourceTables(ctx context.Context, s *store.Store, tx *sql.Tx) error {
	var q string
	switch s.Dialect {
	case "sqlite":
		q = "SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%'"
	case "mysql":
		q = "SELECT table_name FROM information_schema.tables WHERE table_schema=DATABASE() AND table_type='BASE TABLE'"
	case "postgres":
		q = "SELECT table_name FROM information_schema.tables WHERE table_schema=current_schema() AND table_type='BASE TABLE'"
	default:
		return errors.New("unknown source dialect")
	}
	rows, e := tx.QueryContext(ctx, q)
	if e != nil {
		return e
	}
	defer rows.Close()
	seen := map[string]bool{}
	for rows.Next() {
		var n string
		if e = rows.Scan(&n); e != nil {
			return e
		}
		if _, ok := store.Schema[n]; !ok {
			return fmt.Errorf("source schema drift: unknown table %s; export refused without dropping data", n)
		}
		seen[n] = true
	}
	if e = rows.Err(); e != nil {
		return e
	}
	missing := []string{}
	for _, n := range store.Tables() {
		if !seen[n] {
			missing = append(missing, n)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("source schema drift: missing tables %s", strings.Join(missing, ", "))
	}
	return nil
}

// Writes never exceed max. An incomplete exclusive snapshot is removed by the
// existing Export cleanup; no source file is touched.
type boundedSnapshotWriter struct {
	file         *os.File
	max, written int64
}

func (w *boundedSnapshotWriter) Write(p []byte) (int, error) {
	if w.max > 0 && int64(len(p)) > w.max-w.written {
		return 0, errors.New("snapshot exceeds configured byte limit")
	}
	n, err := w.file.Write(p)
	w.written += int64(n)
	return n, err
}
func (w *boundedSnapshotWriter) WriteString(s string) (int, error) { return w.Write([]byte(s)) }
