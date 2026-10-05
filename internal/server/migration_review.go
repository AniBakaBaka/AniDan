// SPDX-License-Identifier: AGPL-3.0-only
package server

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"time"

	"github.com/AniBakaBaka/AniDan/internal/job"
	"github.com/AniBakaBaka/AniDan/internal/migrate"
	"github.com/AniBakaBaka/AniDan/internal/store"
)

const migrationReviewStateKey = "anidan.migration.review_state"
const migrationReviewStateLimit = 8192

// migrationReviewContext contains provenance only, never imported task payloads
// or credentials. RecoveryReviewRequired remains the existing top-level field.
type migrationReviewContext struct {
	ReceiptSHA256   string `json:"receiptSHA256"`
	SnapshotSHA256  string `json:"snapshotSHA256"`
	SourceDBType    string `json:"sourceDBType"`
	SourceCreatedAt string `json:"sourceCreatedAt"`
	MigratedAt      string `json:"migratedAt"`
	InitializedAt   string `json:"initializedAt"`
}

type migrationReviewState struct {
	Version        int    `json:"version"`
	ReceiptSHA256  string `json:"receiptSHA256"`
	JournalSHA256  string `json:"journalSHA256"`
	SnapshotSHA256 string `json:"snapshotSHA256"`
	SchemaSHA256   string `json:"schemaSHA256"`
	DatabaseSHA256 string `json:"databaseSHA256"`
	TargetDir      string `json:"targetDir"`
	InitializedAt  string `json:"initializedAt"`
}

// initializeMigrationReview precedes journal recovery, auth changes and job
// manager creation. CLI target preflight runs before writable Store.Open/Init
// or log setup; Store.Open can still touch its own SQLite coordination files.
// Migration output itself stays byte/row preserving.
func (s *Server) initializeMigrationReview(ctx context.Context) error {
	limits := s.Config.MigrationReview.Options()
	if err := limits.Validate(); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(limits.TimeoutSeconds)*time.Second)
	defer cancel()
	raw, exists, err := s.readMigrationReviewState(ctx)
	if err != nil {
		return err
	}
	evidence, err := migrate.ReadStartupEvidence(ctx, s.DataDir, limits)
	if err != nil {
		return fmt.Errorf("migration review: %w", err)
	}
	if evidence == nil {
		if s.Config.RequireMigrationEvidence {
			return errors.New("selected migration evidence is missing; refusing runtime initialization")
		}
		if s.Store.Dialect != "sqlite" {
			if err = migrate.VerifyRemoteTargetIdentity(ctx, s.Store, s.Config.DSN, s.Store.DB, nil); err != nil {
				return err
			}
		}
		if exists {
			return errors.New("migration review state exists but its receipt and journal are missing")
		}
		return nil
	}
	if err = s.verifyMigrationDatabaseIdentity(ctx, evidence); err != nil {
		return err
	}
	state, stateErr := decodeMigrationReviewState(raw)
	if exists && stateErr == nil && state.matches(evidence) {
		return s.validateMigrationGate(ctx)
	}
	// A source can itself contain an older native review marker. Never trust it
	// as approval for this import. Only a complete pristine NEW import permits
	// replacing it; changed/lost metadata after runtime writes fails closed.
	if evidence.Receipt.Version == "2.0" {
		if err = migrate.VerifyRemoteLogicalContents(ctx, s.Store, evidence); err != nil {
			return err
		}
	}
	if err = migrate.VerifyStartupContents(ctx, s.DataDir, evidence); err != nil {
		return fmt.Errorf("migration review initialization refused: %w", err)
	}
	state = migrationReviewState{
		Version: 1, ReceiptSHA256: evidence.ReceiptSHA256,
		JournalSHA256:  evidence.JournalSHA256,
		SnapshotSHA256: evidence.Receipt.SnapshotSHA256,
		SchemaSHA256:   evidence.Receipt.SchemaSHA256,
		DatabaseSHA256: evidence.Receipt.DatabaseSHA256,
		TargetDir:      evidence.Receipt.TargetDir,
		InitializedAt:  time.Now().UTC().Format(time.RFC3339Nano),
	}
	encoded, err := json.Marshal(state)
	if err != nil {
		return err
	}
	tx, err := s.Store.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// Do not overwrite a concurrent initializer's decision with our stale read.
	var current string
	query := "SELECT substr(config_value,1,?) FROM config WHERE config_key=?"
	if s.Store.Dialect != "sqlite" {
		query += " FOR UPDATE"
	}
	err = tx.QueryRowContext(ctx, s.Store.Rebind(query), migrationReviewStateLimit+1, migrationReviewStateKey).Scan(&current)
	if errors.Is(err, sql.ErrNoRows) {
		if exists {
			return errors.New("migration review state changed during initialization")
		}
	} else if err != nil {
		return err
	} else if !exists || current != raw {
		return errors.New("migration review state changed during initialization")
	}
	if err = s.writeMigrationReviewDecision(ctx, tx, string(encoded), exists); err != nil {
		return err
	}

	return tx.Commit()
}

// The existence decision belongs to the earlier locked recheck. A later COUNT
// must never turn an absent-state INSERT into UPDATE: another initializer may
// have committed and even received approval in between those statements.
func (s *Server) writeMigrationReviewDecision(ctx context.Context, tx *sql.Tx, encoded string, existed bool) error {
	if existed {
		result, err := tx.ExecContext(ctx, s.Store.Rebind("UPDATE config SET config_value=? WHERE config_key=?"), encoded, migrationReviewStateKey)
		if err != nil {
			return err
		}
		count, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if count != 1 {
			return errors.New("migration review state changed before update")
		}
	} else {
		if _, err := s.Store.InsertTx(ctx, tx, "config", store.Row{"config_key": migrationReviewStateKey, "config_value": encoded, "description": "Native first-boot migration review control"}); err != nil {
			return err
		}
	}
	var n int
	if err := tx.QueryRowContext(ctx, s.Store.Rebind("SELECT COUNT(*) FROM config WHERE config_key=?"), job.RecoveryGateKey).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		_, err := tx.ExecContext(ctx, s.Store.Rebind("UPDATE config SET config_value=? WHERE config_key=?"), "true", job.RecoveryGateKey)
		return err
	}
	_, err := s.Store.InsertTx(ctx, tx, "config", store.Row{"config_key": job.RecoveryGateKey, "config_value": "true", "description": "Native first-boot migration review control"})
	return err
}

func (s *Server) verifyMigrationDatabaseIdentity(ctx context.Context, evidence *migrate.StartupEvidence) error {
	if s.Store.Dialect != "sqlite" {
		return migrate.VerifyRemoteTargetIdentity(ctx, s.Store, s.Config.DSN, s.Store.DB, evidence)
	}
	if evidence == nil || evidence.Receipt.Version != "1.0" {
		return errors.New("open SQLite database does not match the migration receipt type")
	}
	rows, err := s.Store.DB.QueryContext(ctx, "PRAGMA database_list")
	if err != nil {
		return err
	}
	defer rows.Close()
	want := filepath.Join(s.DataDir, "anidan.db")
	found := false
	for rows.Next() {
		var seq int
		var name, filename string
		if err = rows.Scan(&seq, &name, &filename); err != nil {
			return err
		}
		if name == "main" {
			absolute, err := filepath.Abs(filename)
			if err != nil || filename == "" || absolute != want {
				return errors.New("open database does not match the migration receipt target")
			}
			found = true
		}
	}
	if err = rows.Err(); err != nil {
		return err
	}
	if !found {
		return errors.New("migration main database identity is missing")
	}
	return nil
}

func (s *Server) readMigrationReviewState(ctx context.Context) (string, bool, error) {
	var raw string
	err := s.Store.DB.QueryRowContext(ctx, s.Store.Rebind("SELECT substr(config_value,1,?) FROM config WHERE config_key=?"), migrationReviewStateLimit+1, migrationReviewStateKey).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return raw, err == nil, err
}

func decodeMigrationReviewState(raw string) (migrationReviewState, error) {
	var state migrationReviewState
	if len(raw) > migrationReviewStateLimit {
		return state, errors.New("migration review state exceeds limit")
	}
	d := json.NewDecoder(bytes.NewBufferString(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(&state); err != nil {
		return state, err
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return state, errors.New("trailing migration review state")
	}
	if state.Version != 1 {
		return state, errors.New("unsupported migration review state")
	}
	if _, err := time.Parse(time.RFC3339Nano, state.InitializedAt); err != nil {
		return state, errors.New("invalid migration initialization time")
	}
	canonical, err := json.Marshal(state)
	if err != nil || string(canonical) != raw {
		return state, errors.New("migration review state is not its canonical initialized representation")
	}
	return state, nil
}

func (state migrationReviewState) matches(e *migrate.StartupEvidence) bool {
	return e != nil && state.ReceiptSHA256 == e.ReceiptSHA256 && state.JournalSHA256 == e.JournalSHA256 && state.SnapshotSHA256 == e.Receipt.SnapshotSHA256 && state.SchemaSHA256 == e.Receipt.SchemaSHA256 && state.DatabaseSHA256 == e.Receipt.DatabaseSHA256 && state.TargetDir == e.Receipt.TargetDir
}

func (s *Server) validateMigrationGate(ctx context.Context) error {
	var gate string
	err := s.Store.DB.QueryRowContext(ctx, s.Store.Rebind("SELECT substr(config_value,1,6) FROM config WHERE config_key=?"), job.RecoveryGateKey).Scan(&gate)
	if err != nil {
		return fmt.Errorf("migration review gate is missing or unreadable: %w", err)
	}
	if gate != "true" && gate != "false" {
		return errors.New("invalid migration review gate")
	}
	return nil
}

// migrationReviewStatus also revalidates the receipt binding before an explicit
// recovery approval. Runtime database/media changes are expected after boot.
func (s *Server) migrationReviewStatus(ctx context.Context) (*migrationReviewContext, error) {
	raw, exists, err := s.readMigrationReviewState(ctx)
	if err != nil {
		return nil, err
	}
	evidence, err := migrate.ReadStartupEvidence(ctx, s.DataDir, s.Config.MigrationReview.Options())
	if err != nil {
		return nil, err
	}
	if !exists {
		if evidence == nil && s.Store.Dialect != "sqlite" {
			if err = migrate.VerifyRemoteTargetIdentity(ctx, s.Store, s.Config.DSN, s.Store.DB, nil); err != nil {
				return nil, err
			}
		}
		if evidence != nil {
			return nil, errors.New("migration receipt exists but initialized review state is missing")
		}
		return nil, nil
	}
	state, err := decodeMigrationReviewState(raw)
	if err != nil {
		return nil, err
	}
	if !state.matches(evidence) {
		return nil, errors.New("migration receipt no longer matches initialized review state")
	}
	if err = s.verifyMigrationDatabaseIdentity(ctx, evidence); err != nil {
		return nil, err
	}
	if err = s.validateMigrationGate(ctx); err != nil {
		return nil, err
	}
	return &migrationReviewContext{ReceiptSHA256: state.ReceiptSHA256, SnapshotSHA256: state.SnapshotSHA256, SourceDBType: evidence.Receipt.SourceDBType, SourceCreatedAt: evidence.Receipt.SourceCreatedAt, MigratedAt: evidence.Receipt.CreatedAt, InitializedAt: state.InitializedAt}, nil
}
