// SPDX-License-Identifier: AGPL-3.0-only
package migrate

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"

	"github.com/AniBakaBaka/AniDan/internal/store"
)

// logicalTableOrder sorts textual keys by UTF-8 bytes, independently of the
// destination database's locale/collation. Receipts never use driver row order.
func logicalTableOrder(s *store.Store, t store.Table) string {
	keys := make([]string, 0, len(t.PrimaryKey))
	for _, name := range t.PrimaryKey {
		col, _ := t.Column(name)
		expr := s.Quote(name)
		switch col.Kind {
		case "string", "text", "enum":
			switch s.Dialect {
			case "postgres":
				expr = "convert_to(" + expr + ",'UTF8')"
			case "mysql":
				expr = "BINARY " + expr
			default:
				expr = "CAST(" + expr + " AS BLOB)"
			}
		}
		keys = append(keys, expr)
	}
	return strings.Join(keys, ",")
}

// checksumLogicalTables is bounded by the configured logical JSON byte budget.
// SQL errors intentionally omit driver details and row values.
func checksumLogicalTables(ctx context.Context, s *store.Store, q store.SQLQueryer, maxBytes int64, rowLimits ...int64) (map[string]TableReceipt, error) {
	if maxBytes <= 0 {
		return nil, errors.New("logical verification requires a positive byte limit")
	}
	maxRowBytes := int64(32 << 20)
	if len(rowLimits) > 1 {
		return nil, errors.New("at most one logical row byte limit is allowed")
	}
	if len(rowLimits) == 1 {
		maxRowBytes = rowLimits[0]
	}
	if maxRowBytes < 1 || maxRowBytes > 256<<20 {
		return nil, errors.New("logical row byte limit must be 1..268435456")
	}
	result := make(map[string]TableReceipt, len(store.Schema))
	var used int64
	for _, name := range store.Tables() {
		t := store.Schema[name]
		sizeExpr := logicalRowTextBytes(s, t)
		var oversized int
		err := q.QueryRowContext(ctx, s.Rebind("SELECT COUNT(*) FROM "+s.Quote(name)+" WHERE "+sizeExpr+">?"), maxRowBytes).Scan(&oversized)
		if err != nil {
			return nil, fmt.Errorf("cannot bound migration table %s", name)
		}
		if oversized != 0 {
			return nil, fmt.Errorf("migration table %s exceeds configured row byte limit", name)
		}
		rows, err := q.QueryContext(ctx, s.Rebind("SELECT * FROM "+s.Quote(name)+" WHERE "+sizeExpr+"<=? ORDER BY "+logicalTableOrder(s, t)), maxRowBytes)
		if err != nil {
			return nil, fmt.Errorf("cannot read migration table %s", name)
		}
		h := sha256.New()
		var count int64
		for rows.Next() {
			if err = ctx.Err(); err != nil {
				rows.Close()
				return nil, err
			}
			row, e := store.ScanRow(rows, t)
			if e != nil {
				rows.Close()
				return nil, fmt.Errorf("cannot decode migration table %s", name)
			}
			b, e := json.Marshal(row)
			if e != nil {
				rows.Close()
				return nil, fmt.Errorf("cannot encode migration table %s", name)
			}
			if int64(len(b)) > maxRowBytes {
				rows.Close()
				return nil, fmt.Errorf("migration table %s JSON exceeds configured row byte limit", name)
			}
			n := int64(len(b)) + 1
			if used > maxBytes-n {
				rows.Close()
				return nil, errors.New("logical migration verification exceeds configured byte limit")
			}
			used += n
			h.Write(b)
			h.Write([]byte{'\n'})
			count++
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, fmt.Errorf("cannot finish reading migration table %s", name)
		}
		result[name] = TableReceipt{Rows: count, SHA256: hex.EncodeToString(h.Sum(nil))}
	}
	return result, nil
}

// Restrict materialized SQL text before the driver can allocate an unbounded
// row. A second JSON bound includes escaping and field names in the proof.
func logicalRowTextBytes(s *store.Store, t store.Table) string {
	parts := []string{"0"}
	for _, c := range t.Columns {
		if c.Kind != "string" && c.Kind != "text" {
			continue
		}
		col := s.Quote(c.Name)
		if s.Dialect == "sqlite" {
			parts = append(parts, "COALESCE(length(CAST("+col+" AS BLOB)),0)")
		} else {
			parts = append(parts, "COALESCE(octet_length("+col+"),0)")
		}
	}
	return "(" + strings.Join(parts, "+") + ")"
}

func logicalDatabaseHash(tables map[string]TableReceipt) string { return remoteHash(tables) }

func transferRemoteRows(ctx context.Context, bridge *store.Store, target *remoteTarget, want map[string]TableReceipt, maxBytes, maxRowBytes int64) error {
	tx, err := target.conn.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return errors.New("cannot begin remote migration data transaction")
	}
	defer tx.Rollback()
	have, err := checksumLogicalTables(ctx, target.store, tx, maxBytes, maxRowBytes)
	if err != nil {
		return err
	}
	if reflect.DeepEqual(have, want) {
		// An earlier COMMIT may have succeeded despite a lost response. Accept only
		// exact complete proof, never a prefix or row-count-only match.
		return tx.Rollback()
	}
	for _, table := range have {
		if table.Rows != 0 {
			return errors.New("remote migration target contains nonmatching data; preserved for manual review")
		}
	}
	for _, name := range store.Tables() {
		rows, e := bridge.DB.QueryContext(ctx, "SELECT * FROM "+bridge.Quote(name)+" ORDER BY "+logicalTableOrder(bridge, store.Schema[name]))
		if e != nil {
			return fmt.Errorf("cannot read prepared table %s", name)
		}
		var count int64
		for rows.Next() {
			if e = ctx.Err(); e != nil {
				rows.Close()
				return e
			}
			row, e := store.ScanRow(rows, store.Schema[name])
			if e != nil {
				rows.Close()
				return fmt.Errorf("cannot decode prepared table %s", name)
			}
			if _, e = target.store.InsertExactTx(ctx, tx, name, row); e != nil {
				rows.Close()
				return fmt.Errorf("remote insert failed for table %s at row %d; transaction rollback requested", name, count+1)
			}
			count++
		}
		e = rows.Err()
		rows.Close()
		if e != nil || count != want[name].Rows {
			return fmt.Errorf("prepared row count changed for table %s", name)
		}
	}
	actual, err := checksumLogicalTables(ctx, target.store, tx, maxBytes, maxRowBytes)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(actual, want) {
		return errors.New("remote migration exact logical verification failed; transaction rollback requested")
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return errors.New("remote migration commit outcome is uncertain; retain the staging directory and resume only this same migration to verify it")
	}
	return nil
}

// VerifyRemoteLogicalContents checks a pristine imported SQL database using one
// read-only repeatable snapshot. It is not used after runtime initialization.
func VerifyRemoteLogicalContents(ctx context.Context, s *store.Store, evidence *StartupEvidence) error {
	if evidence == nil || evidence.Receipt.Version != "2.0" || evidence.Receipt.RemoteTarget == nil {
		return errors.New("remote migration evidence is required")
	}
	tx, err := s.DB.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return errors.New("cannot begin remote migration verification")
	}
	defer tx.Rollback()
	actual, err := checksumLogicalTables(ctx, s, tx, evidence.limits.MaxDatabaseBytes, evidence.limits.MaxRowBytes)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(actual, evidence.Receipt.Tables) || logicalDatabaseHash(actual) != evidence.Receipt.DatabaseSHA256 {
		return errors.New("remote database differs from its pristine migration receipt; review initialization refused")
	}
	return nil
}

func remoteConsistentTables(ctx context.Context, target *remoteTarget, maxBytes, maxRowBytes int64) (map[string]TableReceipt, error) {
	tx, err := target.conn.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return nil, errors.New("cannot begin consistent remote migration proof")
	}
	defer tx.Rollback()
	return checksumLogicalTables(ctx, target.store, tx, maxBytes, maxRowBytes)
}
