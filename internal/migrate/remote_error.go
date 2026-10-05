// SPDX-License-Identifier: AGPL-3.0-only
package migrate

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"

	"github.com/go-sql-driver/mysql"
	"github.com/jackc/pgx/v5/pgconn"
)

// Inspection diagnostics retain only schema-owned phase/table and numeric or
// SQLSTATE codes. Never wrap the raw driver error, which can contain identities
// or row values. Error type names aid failures without a protocol error code.
func remoteInspectionError(ctx context.Context, table, phase string, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err == nil {
		return fmt.Errorf("remote schema inspection failed for %s (%s)", table, phase)
	}
	var my *mysql.MySQLError
	if errors.As(err, &my) {
		return fmt.Errorf("remote schema inspection failed for %s (%s; mysql code %d)", table, phase, my.Number)
	}
	var pg *pgconn.PgError
	if errors.As(err, &pg) {
		code := pg.Code
		if len(code) != 5 || strings.IndexFunc(code, func(r rune) bool { return r < '0' || r > '9' && r < 'A' || r > 'Z' }) >= 0 {
			code = "unknown"
		}
		return fmt.Errorf("remote schema inspection failed for %s (%s; PostgreSQL state %s)", table, phase, code)
	}
	return fmt.Errorf("remote schema inspection failed for %s (%s; error type %s)", table, phase, reflect.TypeOf(err))
}
