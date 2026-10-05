// SPDX-License-Identifier: AGPL-3.0-only
package server

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/AniBakaBaka/AniDan/internal/config"
	"github.com/AniBakaBaka/AniDan/internal/migrate"
	"github.com/AniBakaBaka/AniDan/internal/store"
)

// PreflightMigrationTarget runs before CLI directory/log setup or any writable
// database open. It validates configuration against published migration metadata;
// the later first-boot gate verifies contents and durable review state. Callers
// of New that open their own Store should invoke this before opening it too.
func PreflightMigrationTarget(ctx context.Context, cfg config.Config) error {
	if err := cfg.MigrationReview.Validate(); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(cfg.MigrationReview.TimeoutSeconds)*time.Second)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return err
	}
	var evidence *migrate.StartupEvidence
	if _, err := os.Lstat(cfg.DataDir); err == nil {
		evidence, err = migrate.ReadStartupEvidence(ctx, cfg.DataDir, cfg.MigrationReview.Options())
		if err != nil {
			return &migrationPreflightError{stage: "files", cause: err}
		}
	} else if !os.IsNotExist(err) {
		return &migrationPreflightError{stage: "files", cause: err}
	}
	if cfg.RequireMigrationEvidence && evidence == nil {
		return errors.New("selected migration evidence is missing; refusing ordinary startup")
	}
	switch strings.ToLower(cfg.Driver) {
	case "postgres", "postgresql", "pgx", "mysql":
		if evidence != nil && evidence.Receipt.Version != "2.0" {
			return errors.New("configured remote driver does not match the published SQLite migration")
		}
		// Opening and querying this remote connection does not initialize schema,
		// create the data directory, or start runtime/background services.
		db, err := store.OpenReadOnly(ctx, cfg.Driver, cfg.DSN)
		if err != nil {
			return &migrationPreflightError{stage: "connect", cause: err}
		}
		defer db.Close()
		tx, err := db.DB.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
		if err != nil {
			return &migrationPreflightError{stage: "transaction", cause: err}
		}
		defer tx.Rollback()
		if err = migrate.VerifyRemoteTargetIdentity(ctx, db, cfg.DSN, tx, evidence); err != nil {
			return err
		}
		if evidence != nil {
			if err = migrate.VerifyRemoteRuntimeSchema(ctx, db, tx); err != nil {
				return errors.New("remote migration schema differs before runtime initialization")
			}
		}
		return ctx.Err()
	case "", "sqlite", "sqlite3":
		if evidence == nil {
			return nil
		}
		if evidence.Receipt.Version != "1.0" {
			return errors.New("configured SQLite driver does not match the published remote migration")
		}
	default:
		if evidence == nil {
			return nil
		}
		return errors.New("configured driver does not match the published migration target")
	}
	want := filepath.Join(evidence.Receipt.TargetDir, "anidan.db")
	configured := cfg.DSN
	if configured == "" {
		// Match Store.Open's fallback, not an assumed data-directory rewrite.
		// Config.Load normally fills this field before the CLI calls us.
		configured = "anidan.db"
	}
	if strings.HasPrefix(configured, "file:") {
		u, err := url.Parse(configured)
		if err != nil || u.User != nil || u.Fragment != "" || u.Host != "" && u.Host != "localhost" {
			return errors.New("migration target preflight requires a local SQLite path")
		}
		if u.Opaque != "" {
			configured, err = url.PathUnescape(u.Opaque)
			if err != nil {
				return errors.New("invalid migration SQLite path")
			}
		} else {
			configured = u.Path
		}
		query, err := url.ParseQuery(u.RawQuery)
		if err != nil {
			return errors.New("invalid migration SQLite options")
		}
		if err := validateMigrationSQLiteOptions(query); err != nil {
			return err
		}
	} else {
		var rawQuery string
		configured, rawQuery, _ = strings.Cut(configured, "?")
		query, err := url.ParseQuery(rawQuery)
		if err != nil {
			return errors.New("invalid migration SQLite options")
		}
		if err := validateMigrationSQLiteOptions(query); err != nil {
			return err
		}
	}
	actual, err := filepath.Abs(configured)
	if err != nil || configured == "" || actual != want {
		return errors.New("configured database does not match the published migration target; no database was opened")
	}
	info, err := os.Lstat(want)
	if err != nil {
		return fmt.Errorf("migration target database: %w", err)
	}
	if !info.Mode().IsRegular() {
		return errors.New("migration target database must be a regular nonsymlink file")
	}
	return ctx.Err()
}

func validateMigrationSQLiteOptions(query url.Values) error {
	for _, mode := range query["mode"] {
		if mode == "memory" {
			return errors.New("migration target cannot use an in-memory database")
		}
	}
	for _, key := range []string{"vfs", "immutable", "nolock"} {
		if _, present := query[key]; present {
			return errors.New("migration target requires the standard file-backed SQLite locking and WAL reader")
		}
	}
	return nil
}
