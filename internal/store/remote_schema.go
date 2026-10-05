// SPDX-License-Identifier: AGPL-3.0-only
package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// SQLQueryer permits bounded schema operations on a pinned connection or
// transaction without borrowing another physical connection from the pool.
type SQLQueryer interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

// FreshTableDDL returns allowlisted DDL without IF NOT EXISTS. Callers must
// establish exclusive ownership and inspect the namespace before executing it.
func (s *Store) FreshTableDDL(name string) (string, error) {
	t, err := s.table(name)
	if err != nil {
		return "", err
	}
	return strings.Replace(s.createTable(t), "CREATE TABLE IF NOT EXISTS ", "CREATE TABLE ", 1), nil
}

// FreshIndexDDL returns the non-unique secondary indexes for one schema table.
func (s *Store) FreshIndexDDL(name string) ([]string, error) {
	t, err := s.table(name)
	if err != nil {
		return nil, err
	}
	statements := make([]string, 0, len(t.Indexes))
	for _, idx := range t.Indexes {
		cols := make([]string, len(idx.Columns))
		for i, c := range idx.Columns {
			cols[i] = s.Quote(c)
			if s.Dialect == "mysql" && idx.MySQLLength > 0 {
				cols[i] += fmt.Sprintf("(%d)", idx.MySQLLength)
			}
		}
		statements = append(statements, "CREATE INDEX "+s.Quote(idx.Name)+" ON "+s.Quote(name)+" ("+strings.Join(cols, ",")+")")
	}
	return statements, nil
}
