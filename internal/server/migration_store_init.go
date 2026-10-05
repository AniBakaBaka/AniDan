// SPDX-License-Identifier: AGPL-3.0-only
package server

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/AniBakaBaka/AniDan/internal/config"
	"github.com/AniBakaBaka/AniDan/internal/migrate"
	"github.com/AniBakaBaka/AniDan/internal/store"
)

// InitializeStore keeps ordinary installation behavior while refusing schema
// repair on an already-completed remote migration. Call CLI preflight first,
// before creating logs/directories or opening a writable SQLite connection.
func InitializeStore(ctx context.Context, cfg config.Config, s *store.Store) (resultErr error) {
	defer func() {
		driver := cfg.Driver
		if s != nil {
			driver = s.Dialect
		}
		resultErr = remoteStartupError(driver, resultErr)
	}()
	evidence, err := verifyOpenedMigrationTarget(ctx, cfg, s)
	if err != nil {
		return err
	}
	if evidence != nil && evidence.Receipt.Version == "2.0" {
		return nil
	}
	if cfg.LegacyDatabase {
		return s.VerifySchema(ctx)
	}
	return s.Init(ctx)
}

// This second read-only check verifies the Store actually supplied to New;
// configuration preflight's separate connection is not a substitute for it.
func verifyOpenedMigrationTarget(ctx context.Context, cfg config.Config, s *store.Store) (*migrate.StartupEvidence, error) {
	absolute, err := filepath.Abs(cfg.DataDir)
	if err != nil {
		return nil, err
	}
	cfg.DataDir = absolute
	if s == nil || s.DB == nil {
		return nil, errors.New("an open store is required")
	}
	if err := cfg.MigrationReview.Validate(); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(cfg.MigrationReview.TimeoutSeconds)*time.Second)
	defer cancel()
	var evidence *migrate.StartupEvidence
	if _, err := os.Lstat(cfg.DataDir); err == nil {
		evidence, err = migrate.ReadStartupEvidence(ctx, cfg.DataDir, cfg.MigrationReview.Options())
		if err != nil {
			return nil, err
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	if cfg.RequireMigrationEvidence && evidence == nil {
		return nil, errors.New("selected migration evidence is missing; refusing database initialization")
	}
	if s.Dialect == "sqlite" {
		if evidence == nil {
			return nil, nil
		}
		if err := PreflightMigrationTarget(ctx, cfg); err != nil {
			return nil, err
		}
		temporary := &Server{Store: s, DataDir: cfg.DataDir, Config: cfg}
		if err := temporary.verifyMigrationDatabaseIdentity(ctx, evidence); err != nil {
			return nil, err
		}
		return evidence, nil
	}
	if evidence != nil {
		configured := strings.ToLower(cfg.Driver)
		if configured == "postgresql" || configured == "pgx" {
			configured = "postgres"
		}
		if configured != s.Dialect {
			return nil, errors.New("configured driver differs from the supplied migration store")
		}
	}
	tx, err := s.DB.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return nil, errors.New("cannot inspect supplied remote migration store")
	}
	defer tx.Rollback()
	if err = migrate.VerifyRemoteTargetIdentity(ctx, s, cfg.DSN, tx, evidence); err != nil {
		return nil, err
	}
	if evidence != nil {
		if err = migrate.VerifyRemoteRuntimeSchema(ctx, s, tx); err != nil {
			return nil, err
		}
	}
	return evidence, nil
}
