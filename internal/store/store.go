// SPDX-License-Identifier: AGPL-3.0-only
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

type Store struct {
	DB       *sql.DB
	Dialect  string
	timeMu   sync.RWMutex
	location *time.Location
	endpoint endpointProof
}

func Open(ctx context.Context, driver, dsn string) (*Store, error) {
	driver = strings.ToLower(driver)
	if driver == "" {
		driver = "sqlite"
	}
	dialect := driver
	switch driver {
	case "sqlite", "sqlite3":
		driver = "sqlite"
		dialect = "sqlite"
		if dsn == "" {
			dsn = "anidan.db"
		}
		sep := "?"
		if strings.Contains(dsn, "?") {
			sep = "&"
		}
		dsn += sep + "_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(FULL)"
	case "mysql":
	case "postgres", "postgresql", "pgx":
		driver = "pgx"
		dialect = "postgres"
	default:
		return nil, fmt.Errorf("unsupported database driver %q", driver)
	}
	var db *sql.DB
	var err error
	var endpoint string
	if dialect == "sqlite" {
		db, err = sql.Open(driver, dsn)
	} else {
		db, endpoint, err = openRemoteConnector(dialect, dsn)
	}
	if err != nil {
		return nil, err
	}
	if dialect == "sqlite" {
		db.SetMaxOpenConns(1)
		db.SetMaxIdleConns(1)
	} else {
		db.SetMaxOpenConns(4)
		db.SetMaxIdleConns(2)
		db.SetConnMaxLifetime(5 * time.Minute)
	}
	if err = db.PingContext(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{DB: db, Dialect: dialect, endpoint: endpointProof{db: db, dialect: dialect, digest: endpoint}}, nil
}

// OpenReadOnly opens only a source snapshot connection. It never initializes schemas.
// The caller must also begin a read-only transaction for remote database snapshots.
func OpenReadOnly(ctx context.Context, driver, dsn string) (*Store, error) {
	driver = strings.ToLower(driver)
	if driver == "sqlite" || driver == "sqlite3" || driver == "" {
		if dsn == "" || dsn == ":memory:" {
			return nil, errors.New("readonly source requires an existing SQLite file")
		}
		if !strings.HasPrefix(dsn, "file:") {
			dsn = "file:" + url.PathEscape(dsn)
		}
		if strings.Contains(dsn, "?") {
			return nil, errors.New("readonly SQLite source DSN must not contain query parameters")
		}
		db, err := sql.Open("sqlite", dsn+"?mode=ro&_pragma=query_only(1)&_pragma=busy_timeout(5000)")
		if err != nil {
			return nil, err
		}
		db.SetMaxOpenConns(1)
		if err = db.PingContext(ctx); err != nil {
			db.Close()
			return nil, err
		}
		return &Store{DB: db, Dialect: "sqlite"}, nil
	}
	return Open(ctx, driver, dsn)
}
func (s *Store) Close() error { return s.DB.Close() }
func (s *Store) Quote(v string) string {
	if s.Dialect == "mysql" {
		return "`" + strings.ReplaceAll(v, "`", "``") + "`"
	}
	return `"` + strings.ReplaceAll(v, `"`, `""`) + `"`
}
func (s *Store) Placeholder(n int) string {
	if s.Dialect == "postgres" {
		return "$" + strconv.Itoa(n)
	}
	return "?"
}
func (s *Store) table(name string) (Table, error) {
	t, ok := Schema[name]
	if !ok {
		return Table{}, fmt.Errorf("unknown table %q", name)
	}
	return t, nil
}
func (s *Store) Init(ctx context.Context) error {
	existing, err := s.DatabaseTables(ctx)
	if err != nil {
		return err
	}
	if len(existing) > 0 {
		for _, name := range existing {
			if _, ok := Schema[name]; !ok {
				return fmt.Errorf("schema drift: unknown table %s; initialization refused", name)
			}
		}
		if err = s.VerifySchema(ctx); err != nil {
			return err
		}
	}

	for _, name := range Tables() {
		t := Schema[name]
		if _, err := s.DB.ExecContext(ctx, s.createTable(t)); err != nil {
			return fmt.Errorf("create %s: %w", name, err)
		}
	}
	// CREATE IF NOT EXISTS must never conceal incompatible existing columns.
	if err := s.VerifySchema(ctx); err != nil {
		return err
	}
	for _, name := range Tables() {
		t := Schema[name]
		for _, idx := range t.Indexes {
			if s.Dialect == "mysql" {
				var n int
				if err := s.DB.QueryRowContext(ctx, "SELECT COUNT(*) FROM information_schema.statistics WHERE table_schema=DATABASE() AND table_name=? AND index_name=?", name, idx.Name).Scan(&n); err != nil {
					return err
				}
				if n > 0 {
					continue
				}
			}
			cols := make([]string, len(idx.Columns))
			for i, c := range idx.Columns {
				cols[i] = s.Quote(c)
				if s.Dialect == "mysql" && idx.MySQLLength > 0 {
					cols[i] += fmt.Sprintf("(%d)", idx.MySQLLength)
				}
			}
			prefix := "CREATE INDEX IF NOT EXISTS "
			if s.Dialect == "mysql" {
				prefix = "CREATE INDEX "
			}
			if _, err := s.DB.ExecContext(ctx, prefix+s.Quote(idx.Name)+" ON "+s.Quote(name)+" ("+strings.Join(cols, ",")+")"); err != nil {
				return fmt.Errorf("index %s: %w", idx.Name, err)
			}
		}
	}
	return nil
}
func (s *Store) createTable(t Table) string {
	parts := []string{}
	inlinePK := false
	for _, c := range t.Columns {
		typ := "TEXT"
		switch c.Kind {
		case "integer":
			typ = "INTEGER"
		case "bigint":
			typ = "BIGINT"
		case "bool":
			typ = "BOOLEAN"
		case "string":
			typ = fmt.Sprintf("VARCHAR(%d)", c.Length)
		case "datetime":
			typ = "TIMESTAMP"
			if s.Dialect == "sqlite" {
				typ = "TEXT"
			} else if s.Dialect == "mysql" {
				typ = "DATETIME(6)"
			}
		case "decimal":
			typ = fmt.Sprintf("DECIMAL(%d,%d)", c.Precision, c.Scale)
			if s.Dialect == "sqlite" {
				typ = "TEXT"
			}
		case "text":
			if s.Dialect == "mysql" && c.Medium {
				typ = "MEDIUMTEXT"
			}
		}
		if s.Dialect == "sqlite" && (c.Kind == "integer" || c.Kind == "bigint") {
			typ = "INTEGER"
		}
		p := s.Quote(c.Name) + " " + typ
		if len(t.PrimaryKey) == 1 && t.PrimaryKey[0] == c.Name && s.Dialect == "sqlite" && (c.Kind == "integer" || c.Kind == "bigint") {
			p += " PRIMARY KEY"
			inlinePK = true
			if c.Auto {
				p += " AUTOINCREMENT"
			}
		}
		if c.Auto && s.Dialect == "postgres" {
			p += " GENERATED BY DEFAULT AS IDENTITY"
		}
		if !c.Nullable {
			p += " NOT NULL"
		}
		if c.Auto && s.Dialect == "mysql" {
			p += " AUTO_INCREMENT"
		}
		if c.ServerDefault != nil {
			d := *c.ServerDefault
			if c.Kind == "bool" {
				if d == "0" {
					d = "FALSE"
				} else {
					d = "TRUE"
				}
			} else if c.Kind != "integer" && c.Kind != "bigint" {
				d = "'" + strings.ReplaceAll(d, "'", "''") + "'"
			}
			p += " DEFAULT " + d
		}
		if len(c.Enum) > 0 {
			vs := []string{}
			for _, e := range c.Enum {
				vs = append(vs, "'"+strings.ReplaceAll(e, "'", "''")+"'")
			}
			p += " CHECK (" + s.Quote(c.Name) + " IN (" + strings.Join(vs, ",") + "))"
		}
		parts = append(parts, p)
	}
	join := func(cols []string) string {
		v := []string{}
		for _, c := range cols {
			v = append(v, s.Quote(c))
		}
		return strings.Join(v, ",")
	}
	if !inlinePK {
		parts = append(parts, "PRIMARY KEY ("+join(t.PrimaryKey)+")")
	}
	for _, cols := range t.Unique {
		parts = append(parts, "UNIQUE ("+join(cols)+")")
	}
	for _, c := range t.Columns {
		if c.Reference != "" {
			r := strings.Split(c.Reference, ".")
			parts = append(parts, "FOREIGN KEY ("+s.Quote(c.Name)+") REFERENCES "+s.Quote(r[0])+" ("+s.Quote(r[1])+") ON DELETE "+c.OnDelete)
		}
	}
	stmt := "CREATE TABLE IF NOT EXISTS " + s.Quote(t.Name) + " (" + strings.Join(parts, ",") + ")"
	if s.Dialect == "mysql" {
		stmt += " ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci"
	}
	return stmt
}

// VerifySchema rejects unknown/missing legacy columns; it does not modify an existing schema.
func (s *Store) VerifySchema(ctx context.Context) error {
	return s.VerifySchemaWith(ctx, s.DB)
}

// VerifySchemaWith performs read-only verification on a caller-owned session.
func (s *Store) VerifySchemaWith(ctx context.Context, q SQLQueryer) error {
	for _, name := range Tables() {
		rows, err := q.QueryContext(ctx, "SELECT * FROM "+s.Quote(name)+" WHERE 1=0")
		if err != nil {
			return fmt.Errorf("schema %s: %w", name, err)
		}
		cols, err := rows.Columns()
		types, typeErr := rows.ColumnTypes()
		rows.Close()
		if typeErr != nil {
			return typeErr
		}
		for _, ct := range types {
			if c, ok := Schema[name].Column(ct.Name()); ok {
				compatible := compatibleType(s.Dialect, c, ct.DatabaseTypeName())
				if !compatible && s.Dialect == "postgres" && len(c.Enum) > 0 {
					if oid, err := strconv.ParseInt(ct.DatabaseTypeName(), 10, 64); err == nil {
						compatible, err = s.verifyPostgresEnum(ctx, q, oid, c.Enum)
						if err != nil {
							return err
						}
					}
				}
				if !compatible {
					return fmt.Errorf("schema drift: %s.%s type %s is incompatible with %s", name, c.Name, ct.DatabaseTypeName(), c.Kind)
				}
			}
		}
		if err != nil {
			return err
		}
		t := Schema[name]
		if len(cols) != len(t.Columns) {
			return fmt.Errorf("schema drift in %s: expected %d columns, got %d", name, len(t.Columns), len(cols))
		}
		for _, c := range cols {
			if _, ok := t.Column(c); !ok {
				return fmt.Errorf("schema drift: unknown column %s.%s", name, c)
			}
		}
	}
	return nil
}
func NormalizeValue(c Column, v any) (any, error) {
	if v == nil {
		if !c.Nullable {
			return nil, fmt.Errorf("%s cannot be null", c.Name)
		}
		return nil, nil
	}
	switch c.Kind {
	case "integer", "bigint":
		switch n := v.(type) {
		case json.Number:
			i, e := n.Int64()
			return i, e
		case int:
			return int64(n), nil
		case int64:
			return n, nil
		case int32:
			return int64(n), nil
		case uint64:
			if n > math.MaxInt64 {
				return nil, errors.New("integer overflows int64")
			}
			return int64(n), nil
		case float64:
			if math.IsNaN(n) || math.IsInf(n, 0) || math.Trunc(n) != n || n >= float64(math.MaxInt64) || n < float64(math.MinInt64) {
				return nil, errors.New("invalid or inexact int64")
			}
			if n > 1<<53 || n < -(1<<53) {
				return nil, errors.New("unsafe float64 integer; decode JSON with UseNumber")
			}
			return int64(n), nil
		case string:
			i, e := strconv.ParseInt(n, 10, 64)
			return i, e
		}
		return nil, fmt.Errorf("%s requires integer", c.Name)
	case "bool":
		switch n := v.(type) {
		case bool:
			return n, nil
		case int64:
			if n == 0 || n == 1 {
				return n == 1, nil
			}
		case int:
			if n == 0 || n == 1 {
				return n == 1, nil
			}
		case json.Number:
			if n == "0" || n == "1" {
				return n == "1", nil
			}
		case []byte:
			if string(n) == "0" || string(n) == "1" {
				return string(n) == "1", nil
			}
		}
		return nil, fmt.Errorf("%s requires boolean", c.Name)
	case "decimal":
		switch n := v.(type) {
		case json.Number:
			return decimalValue(c, string(n))
		case string:
			return decimalValue(c, n)
		case []byte:
			return decimalValue(c, string(n))
		case int64:
			return decimalValue(c, strconv.FormatInt(n, 10))
		case float64:
			return decimalValue(c, strconv.FormatFloat(n, 'f', -1, 64))
		}
		return nil, fmt.Errorf("%s requires decimal", c.Name)
	default:
		var str string
		switch n := v.(type) {
		case string:
			str = n
		case []byte:
			str = string(n)
		case time.Time:
			if c.Kind != "datetime" {
				return nil, fmt.Errorf("%s requires string", c.Name)
			}
			str = n.Format("2006-01-02T15:04:05.999999")
		default:
			return nil, fmt.Errorf("%s requires string", c.Name)
		}
		if len(c.Enum) > 0 {
			ok := false
			for _, x := range c.Enum {
				if str == x {
					ok = true
				}
			}
			if !ok {
				return nil, fmt.Errorf("invalid enum for %s", c.Name)
			}
		}
		if c.Kind == "string" && c.Length > 0 && len([]rune(str)) > c.Length {
			return nil, fmt.Errorf("%s exceeds %d characters", c.Name, c.Length)
		}
		if c.Kind == "datetime" {
			if _, err := parseNaive(str); err != nil {
				return nil, fmt.Errorf("%s: %w", c.Name, err)
			}
		}
		return str, nil
	}
}
func decimalValue(c Column, s string) (string, error) {
	neg := ""
	if strings.HasPrefix(s, "-") {
		neg = "-"
		s = s[1:]
	} else if strings.HasPrefix(s, "+") {
		s = s[1:]
	}
	p := strings.Split(s, ".")
	if len(p) > 2 || p[0] == "" {
		return "", fmt.Errorf("invalid decimal %s", c.Name)
	}
	for _, x := range p {
		for _, r := range x {
			if r < '0' || r > '9' {
				return "", fmt.Errorf("invalid decimal %s", c.Name)
			}
		}
	}
	whole := strings.TrimLeft(p[0], "0")
	if whole == "" {
		whole = "0"
	}
	frac := ""
	if len(p) == 2 {
		frac = strings.TrimRight(p[1], "0")
	}
	if len(frac) > c.Scale || len(whole) > c.Precision-c.Scale {
		return "", fmt.Errorf("decimal exceeds %s precision/scale", c.Name)
	}
	if whole == "0" && frac == "" {
		neg = ""
	}
	if frac != "" {
		return neg + whole + "." + frac, nil
	}
	return neg + whole, nil
}
func parseNaive(v string) (time.Time, error) {
	for _, layout := range []string{"2006-01-02T15:04:05.999999999", "2006-01-02 15:04:05.999999999", "2006-01-02"} {
		if t, e := time.Parse(layout, v); e == nil {
			return t, nil
		}
	}
	return time.Time{}, errors.New("expected naive wall-clock ISO datetime, without timezone conversion")
}
func ValidateRow(table string, row Row, complete bool) (Row, error) {
	t, ok := Schema[table]
	if !ok {
		return nil, fmt.Errorf("unknown table %q", table)
	}
	out := Row{}
	for k, v := range row {
		c, ok := t.Column(k)
		if !ok {
			return nil, fmt.Errorf("unknown field %s.%s (schema drift)", table, k)
		}
		n, e := NormalizeValue(c, v)
		if e != nil {
			return nil, fmt.Errorf("%s.%s: %w", table, k, e)
		}
		out[k] = n
	}
	if complete {
		for _, c := range t.Columns {
			if _, ok := out[c.Name]; !ok {
				return nil, fmt.Errorf("missing field %s.%s (schema drift)", table, c.Name)
			}
		}
	}
	return out, nil
}
func (s *Store) bind(c Column, v any) any {
	if c.Kind == "datetime" && v != nil && s.Dialect != "sqlite" {
		if t, e := parseNaive(v.(string)); e == nil {
			if s.Dialect == "mysql" {
				// DATETIME represents legacy wall-clock fields, not an instant.
				// The MySQL driver converts time.Time through its DSN `loc`
				// before encoding. Text avoids silently shifting those fields
				// when loc differs from UTC or the application's timezone.
				return t.Format("2006-01-02 15:04:05.999999")
			}
			return t
		}
	}
	return v
}
func (s *Store) where(t Table, filters Row, start int) (string, []any, error) {
	keys := []string{}
	for k := range filters {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := []string{}
	args := []any{}
	for _, k := range keys {
		c, ok := t.Column(k)
		if !ok {
			return "", nil, fmt.Errorf("unknown filter %s.%s", t.Name, k)
		}
		v := filters[k]
		if v == nil {
			parts = append(parts, s.Quote(k)+" IS NULL")
			continue
		}
		n, e := NormalizeValue(c, v)
		if e != nil {
			return "", nil, e
		}
		parts = append(parts, s.Quote(k)+"="+s.Placeholder(start+len(args)))
		args = append(args, s.bind(c, n))
	}
	if len(parts) == 0 {
		return "", args, nil
	}
	return " WHERE " + strings.Join(parts, " AND "), args, nil
}
func (s *Store) List(ctx context.Context, name string, filters Row, limit, offset int) ([]Row, error) {
	t, e := s.table(name)
	if e != nil {
		return nil, e
	}
	if limit <= 0 {
		limit = 100
	}
	if limit > 100000 {
		limit = 100000
	}
	if offset < 0 {
		return nil, errors.New("negative offset")
	}
	w, args, e := s.where(t, filters, 1)
	if e != nil {
		return nil, e
	}
	order := []string{}
	for _, k := range t.PrimaryKey {
		order = append(order, s.Quote(k))
	}
	q := "SELECT * FROM " + s.Quote(name) + w + " ORDER BY " + strings.Join(order, ",") + " LIMIT " + s.Placeholder(len(args)+1) + " OFFSET " + s.Placeholder(len(args)+2)
	args = append(args, limit, offset)
	rows, e := s.DB.QueryContext(ctx, q, args...)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []Row{}
	for rows.Next() {
		r, e := ScanRow(rows, t)
		if e != nil {
			return nil, e
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
func ScanRow(rows *sql.Rows, t Table) (Row, error) {
	cols, e := rows.Columns()
	if e != nil {
		return nil, e
	}
	vals := make([]any, len(cols))
	ptrs := make([]any, len(cols))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	if e = rows.Scan(ptrs...); e != nil {
		return nil, e
	}
	r := Row{}
	for i, k := range cols {
		c, ok := t.Column(k)
		if !ok {
			return nil, fmt.Errorf("unknown column %s.%s", t.Name, k)
		}
		v := vals[i]
		if b, ok := v.([]byte); ok && c.Kind != "bool" {
			v = string(b)
		}
		n, e := NormalizeValue(c, v)
		if e != nil {
			return nil, fmt.Errorf("read %s.%s: %w", t.Name, k, e)
		}
		r[k] = n
	}
	return r, nil
}
func keyFilters(t Table, id any) (Row, error) {
	if r, ok := id.(Row); ok {
		if len(r) != len(t.PrimaryKey) {
			return nil, errors.New("primary key field count mismatch")
		}
		for _, k := range t.PrimaryKey {
			if _, ok := r[k]; !ok {
				return nil, errors.New("missing primary key " + k)
			}
		}
		return r, nil
	}
	if len(t.PrimaryKey) != 1 {
		return nil, errors.New("composite primary key requires store.Row")
	}
	return Row{t.PrimaryKey[0]: id}, nil
}
func (s *Store) Get(ctx context.Context, name string, id any) (Row, error) {
	t, e := s.table(name)
	if e != nil {
		return nil, e
	}
	f, e := keyFilters(t, id)
	if e != nil {
		return nil, e
	}
	r, e := s.List(ctx, name, f, 1, 0)
	if e != nil {
		return nil, e
	}
	if len(r) == 0 {
		return nil, sql.ErrNoRows
	}
	return r[0], nil
}
func (s *Store) Count(ctx context.Context, name string, filters Row) (int64, error) {
	t, e := s.table(name)
	if e != nil {
		return 0, e
	}
	w, args, e := s.where(t, filters, 1)
	if e != nil {
		return 0, e
	}
	var n int64
	e = s.DB.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+s.Quote(name)+w, args...).Scan(&n)
	return n, e
}
func (s *Store) insertParts(name string, row Row, defaults bool) (string, []any, Table, error) {
	t, e := s.table(name)
	if e != nil {
		return "", nil, t, e
	}
	r, e := ValidateRow(name, row, false)
	if e != nil {
		return "", nil, t, e
	}
	if defaults {
		for _, c := range t.Columns {
			if _, ok := r[c.Name]; ok {
				continue
			}
			if c.NowDefault {
				r[c.Name] = s.defaultTime()
			} else if c.HasDefault {
				n, e := NormalizeValue(c, c.Default)
				if e != nil {
					return "", nil, t, e
				}
				r[c.Name] = n
			}
		}
	}
	keys := []string{}
	args := []any{}
	ps := []string{}
	for _, c := range t.Columns {
		if v, ok := r[c.Name]; ok {
			keys = append(keys, s.Quote(c.Name))
			args = append(args, s.bind(c, v))
			ps = append(ps, s.Placeholder(len(args)))
		}
	}
	if len(keys) == 0 {
		if s.Dialect == "mysql" {
			return "INSERT INTO " + s.Quote(name) + " () VALUES ()", args, t, nil
		}
		return "INSERT INTO " + s.Quote(name) + " DEFAULT VALUES", args, t, nil
	}
	return "INSERT INTO " + s.Quote(name) + " (" + strings.Join(keys, ",") + ") VALUES (" + strings.Join(ps, ",") + ")", args, t, nil
}

type Executor interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func (s *Store) Insert(ctx context.Context, name string, row Row) (int64, error) {
	return s.insert(ctx, s.DB, name, row, true)
}

// InsertTx uses the same declared application defaults as Insert.
func (s *Store) InsertTx(ctx context.Context, tx *sql.Tx, name string, row Row) (int64, error) {
	return s.insert(ctx, tx, name, row, true)
}

// InsertExactTx requires every physical field and never invents defaults. It is
// the migration path, distinct from normal transactional runtime insertion.
func (s *Store) InsertExactTx(ctx context.Context, tx *sql.Tx, name string, row Row) (int64, error) {
	r, e := ValidateRow(name, row, true)
	if e != nil {
		return 0, e
	}
	return s.insert(ctx, tx, name, r, false)
}
func (s *Store) insert(ctx context.Context, ex Executor, name string, row Row, defaults bool) (int64, error) {
	q, args, t, e := s.insertParts(name, row, defaults)
	if e != nil {
		return 0, e
	}
	numeric := false
	if len(t.PrimaryKey) == 1 {
		c, _ := t.Column(t.PrimaryKey[0])
		numeric = c.Kind == "integer" || c.Kind == "bigint"
	}
	if s.Dialect == "postgres" && numeric {
		var id int64
		e = ex.QueryRowContext(ctx, q+" RETURNING "+s.Quote(t.PrimaryKey[0]), args...).Scan(&id)
		return id, e
	}
	res, e := ex.ExecContext(ctx, q, args...)
	if e != nil {
		return 0, e
	}
	if numeric {
		if id, ok := row[t.PrimaryKey[0]]; ok {
			c, _ := t.Column(t.PrimaryKey[0])
			n, _ := NormalizeValue(c, id)
			return n.(int64), nil
		}
		return res.LastInsertId()
	}
	return 0, nil
}
func (s *Store) Update(ctx context.Context, name string, id any, row Row) error {
	t, e := s.table(name)
	if e != nil {
		return e
	}
	f, e := keyFilters(t, id)
	if e != nil {
		return e
	}
	r, e := ValidateRow(name, row, false)
	if e != nil {
		return e
	}
	if len(r) == 0 {
		return nil
	}
	for _, c := range t.Columns {
		if c.NowUpdate {
			if _, ok := r[c.Name]; !ok {
				r[c.Name] = s.defaultTime()
			}
		}
	}
	parts := []string{}
	args := []any{}
	for _, c := range t.Columns {
		v, ok := r[c.Name]
		if !ok {
			continue
		}
		for _, pk := range t.PrimaryKey {
			if c.Name == pk {
				return errors.New("primary key updates are forbidden")
			}
		}
		args = append(args, s.bind(c, v))
		parts = append(parts, s.Quote(c.Name)+"="+s.Placeholder(len(args)))
	}
	if len(parts) == 0 {
		return nil
	}
	w, a, e := s.where(t, f, len(args)+1)
	if e != nil {
		return e
	}
	args = append(args, a...)
	_, e = s.DB.ExecContext(ctx, "UPDATE "+s.Quote(name)+" SET "+strings.Join(parts, ",")+w, args...)
	return e
}
func (s *Store) Delete(ctx context.Context, name string, id any) error {
	t, e := s.table(name)
	if e != nil {
		return e
	}
	f, e := keyFilters(t, id)
	if e != nil {
		return e
	}
	w, args, e := s.where(t, f, 1)
	if e != nil {
		return e
	}
	_, e = s.DB.ExecContext(ctx, "DELETE FROM "+s.Quote(name)+w, args...)
	return e
}

// Rebind changes unquoted question-mark placeholders into PostgreSQL placeholders.
// Identifiers and SQL literals are left unchanged. Use with parameterized statements.
func (s *Store) Rebind(q string) string {
	if s.Dialect != "postgres" {
		return q
	}
	var out strings.Builder
	var quote byte
	n := 0
	for i := 0; i < len(q); i++ {
		b := q[i]
		if quote != 0 {
			out.WriteByte(b)
			if b == quote {
				if i+1 < len(q) && q[i+1] == quote {
					i++
					out.WriteByte(q[i])
				} else {
					quote = 0
				}
			}
			continue
		}
		if b == '\'' || b == '"' || b == '`' {
			quote = b
			out.WriteByte(b)
		} else if b == '?' {
			n++
			out.WriteString("$" + strconv.Itoa(n))
		} else {
			out.WriteByte(b)
		}
	}
	return out.String()
}

// SetTimezone controls generated application timestamps only. Existing values and
// explicit caller timestamps are never converted or reinterpreted.
func (s *Store) SetTimezone(name string) error {
	loc, e := time.LoadLocation(name)
	if e != nil {
		return e
	}
	s.timeMu.Lock()
	s.location = loc
	s.timeMu.Unlock()
	return nil
}
func (s *Store) defaultTime() string {
	s.timeMu.RLock()
	loc := s.location
	s.timeMu.RUnlock()
	if loc == nil {
		loc = time.Local
	}
	return time.Now().In(loc).Format("2006-01-02T15:04:05.999999")
}
