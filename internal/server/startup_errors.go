// SPDX-License-Identifier: AGPL-3.0-only
package server

import (
	"context"
	"errors"
	"net"
	"strings"

	"github.com/go-sql-driver/mysql"
	"github.com/jackc/pgx/v5/pgconn"
)

// Driver diagnostics can contain connection identity and row values, including
// when a pooled connection reconnects after the initial preflight. Keep
// application-authored migration refusals actionable while redacting these
// recognized typed driver/network diagnostics. Untyped driver TLS/authentication
// errors are not universally classified here; the initial CLI open has a
// separate fixed-phase error boundary.
func remoteStartupError(driver string, err error) error {
	if err == nil {
		return nil
	}
	switch strings.ToLower(driver) {
	case "postgres", "postgresql", "pgx", "mysql":
	default:
		return err
	}
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	var connect *pgconn.ConnectError
	var parse *pgconn.ParseConfigError
	var pg *pgconn.PgError
	var my *mysql.MySQLError
	var network *net.OpError
	if errors.As(err, &connect) || errors.As(err, &parse) || errors.As(err, &pg) || errors.As(err, &my) || errors.As(err, &network) {
		return errors.New("remote database operation failed during startup; driver diagnostic redacted")
	}
	return err
}
