// SPDX-License-Identifier: AGPL-3.0-only
package store

import (
	"context"
	"fmt"
	"strings"
)

// DatabaseTables returns physical user tables in the current database/schema.
func (s *Store) DatabaseTables(ctx context.Context) ([]string, error) {
	var q string
	switch s.Dialect {
	case "sqlite":
		q = "SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%' ORDER BY name"
	case "mysql":
		q = "SELECT table_name FROM information_schema.tables WHERE table_schema=DATABASE() AND table_type='BASE TABLE' ORDER BY table_name"
	case "postgres":
		q = "SELECT table_name FROM information_schema.tables WHERE table_schema=current_schema() AND table_type='BASE TABLE' ORDER BY table_name"
	default:
		return nil, fmt.Errorf("unsupported dialect %s", s.Dialect)
	}
	rows, e := s.DB.QueryContext(ctx, q)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var n string
		if e = rows.Scan(&n); e != nil {
			return nil, e
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// ResetSequences is for an explicitly offline, fully imported target. It is not
// called by Init or by read-only export. SQLite/MySQL advance on explicit inserts.
func (s *Store) ResetSequences(ctx context.Context) error {
	return s.ResetSequencesWith(ctx, s.DB)
}

// ResetSequencesWith keeps offline migration operations on their pinned session.
func (s *Store) ResetSequencesWith(ctx context.Context, q SQLQueryer) error {
	if s.Dialect != "postgres" {
		return nil
	}
	for _, name := range Tables() {
		t := Schema[name]
		for _, c := range t.Columns {
			if !c.Auto {
				continue
			}
			var sequence *string
			if e := q.QueryRowContext(ctx, "SELECT pg_get_serial_sequence($1,$2)", s.Quote(name), c.Name).Scan(&sequence); e != nil {
				return e
			}
			if sequence == nil {
				return fmt.Errorf("missing identity/sequence for %s.%s", name, c.Name)
			}
			var max *int64
			if e := q.QueryRowContext(ctx, "SELECT MAX("+s.Quote(c.Name)+") FROM "+s.Quote(name)).Scan(&max); e != nil {
				return e
			}
			next := int64(1)
			called := false
			if max != nil && *max > 0 {
				next = *max
				called = true
			}
			if _, e := q.ExecContext(ctx, "SELECT setval($1::regclass,$2,$3)", *sequence, next, called); e != nil {
				return e
			}
		}
	}
	return nil
}
func compatibleType(dialect string, c Column, typ string) bool {
	typ = strings.ToUpper(typ)
	if i := strings.IndexByte(typ, '('); i >= 0 {
		typ = typ[:i]
	}
	switch c.Kind {
	case "bigint":
		return typ == "BIGINT" || typ == "INT8" || (dialect == "sqlite" && typ == "INTEGER")
	case "integer":
		return typ == "INT" || typ == "INTEGER" || typ == "INT4"
	case "bool":
		return typ == "BOOL" || typ == "BOOLEAN" || (dialect == "mysql" && typ == "TINYINT")
	case "decimal":
		return typ == "NUMERIC" || typ == "DECIMAL" || (dialect == "sqlite" && typ == "TEXT")
	case "datetime":
		return typ == "DATETIME" || typ == "TIMESTAMP" || typ == "TIMESTAMP WITHOUT TIME ZONE" || (dialect == "sqlite" && typ == "TEXT")
	case "text":
		return typ == "TEXT" || typ == "MEDIUMTEXT" || typ == "LONGTEXT"
	case "string":
		return typ == "VARCHAR" || typ == "CHARACTER VARYING" || typ == "TEXT" || (len(c.Enum) > 0 && (typ == "ENUM" || strings.HasSuffix(typ, "_TYPE")))
	}
	return false
}

// pgx reports unregistered user-defined enum OIDs as their decimal OID rather
// than a type name. Compare catalog enum labels instead of rejecting valid legacy
// PostgreSQL SQLAlchemy enums or accepting arbitrary unknown custom types.
func (s *Store) verifyPostgresEnum(ctx context.Context, q SQLQueryer, oid int64, expected []string) (bool, error) {
	rows, e := q.QueryContext(ctx, "SELECT enumlabel FROM pg_enum WHERE enumtypid=$1::oid ORDER BY enumsortorder", oid)
	if e != nil {
		return false, e
	}
	defer rows.Close()
	got := []string{}
	for rows.Next() {
		var v string
		if e = rows.Scan(&v); e != nil {
			return false, e
		}
		got = append(got, v)
	}
	if e = rows.Err(); e != nil {
		return false, e
	}
	if len(got) != len(expected) {
		return false, nil
	}
	for i, v := range got {
		if v != expected[i] {
			return false, nil
		}
	}
	return true, nil
}
