// SPDX-License-Identifier: AGPL-3.0-only
package server

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/AniBakaBaka/AniDan/internal/config"
	"github.com/AniBakaBaka/AniDan/internal/migrate"
	"github.com/AniBakaBaka/AniDan/internal/store"
	"golang.org/x/crypto/bcrypt"
)

// Inputs come only from the authenticated coordinator's private operation state.
// TargetID and SessionAudience are generated once and retained across preview,
// preparation and restart; neither may be regenerated for a confirmed preview.
type webMigrationPrepareInput struct {
	BootstrapConfig        config.Config
	BootstrapDataDir       string
	SourcePath             string
	SourceKind             string
	LegacyConfigPath       string
	TargetID               string
	SessionAudience        string
	ExpectedProofSHA256    string
	ExpectedSnapshotSHA256 string
	Roots                  []migrate.RootMapping
	DryRun                 bool
	SourceQuiesced         bool
}

// Only Summary and ProofSHA256 are suitable for an API response. Receipt and
// RuntimeConfig intentionally stay private: they contain host paths and keys.
type webMigrationPrepareResult struct {
	Summary       webMigrationSummary `json:"summary"`
	ProofSHA256   string              `json:"proofSHA256"`
	Receipt       migrate.Receipt     `json:"-"`
	RuntimeConfig config.Config       `json:"-"`
}

type webMigrationSummary struct {
	SourceDBType   string           `json:"sourceDBType"`
	SnapshotSHA256 string           `json:"snapshotSHA256"`
	TableCount     int              `json:"tableCount"`
	TableRecords   map[string]int64 `json:"tableRecords"`
	TotalRecords   int64            `json:"totalRecords"`
	FileCount      int              `json:"fileCount"`
	FileBytes      int64            `json:"fileBytes"`
	RootCount      int              `json:"rootCount"`
	ReviewRequired bool             `json:"reviewRequired"`
	Warnings       []string         `json:"warnings"`
}

// webMigrationPrepare reuses the strict importer; it never opens the active
// database, writes a source root, or starts a target runtime. A successful real
// run publishes a pristine target whose existing first-boot migration gate must
// complete before authentication changes, pending recovery or schedulers start.
// Even on error, Receipt can describe a completed import: cancellation or an
// fsync failure after rename requires read-only reconciliation by the coordinator.
func webMigrationPrepare(ctx context.Context, in webMigrationPrepareInput) (webMigrationPrepareResult, error) {
	var out webMigrationPrepareResult
	if err := ctx.Err(); err != nil {
		return out, err
	}
	if in.SourceKind != "" && in.SourceKind != "legacy" {
		return out, errors.New("this migration wizard supports legacy JSON/gzip snapshots; use backup restore for native full bundles")
	}
	if !webMigrationPrepareHex(in.TargetID, 16) || !webMigrationPrepareHex(in.SessionAudience, 32) {
		return out, errors.New("invalid migration operation identity")
	}
	if !in.DryRun && (!in.SourceQuiesced || !webMigrationPrepareHex(in.ExpectedProofSHA256, 32)) {
		return out, errors.New("preparation requires a reviewed preview and confirmation that legacy writers are stopped")
	}
	bootstrap, err := webMigrationCanonicalDirectory(in.BootstrapDataDir)
	if err != nil {
		return out, errors.New("migration bootstrap directory must be an existing canonical directory")
	}
	base := filepath.Join(bootstrap, ".web-migrations")
	op := filepath.Join(base, "operations", in.TargetID)
	if _, err = webMigrationCanonicalDirectory(op); err != nil {
		return out, errors.New("migration operation directory is missing or unsafe")
	}
	if filepath.Dir(in.SourcePath) != op || (filepath.Base(in.SourcePath) != "source.json" && filepath.Base(in.SourcePath) != "source.json.gz") {
		return out, errors.New("snapshot must be a server-owned upload for this operation")
	}
	if err = webMigrationRegularFile(in.SourcePath, maxBackupBytes); err != nil {
		return out, errors.New("snapshot upload is missing, unsafe or exceeds the migration byte limit")
	}
	if in.LegacyConfigPath != "" {
		if in.LegacyConfigPath != filepath.Join(op, "legacy-config.yml") {
			return out, errors.New("legacy configuration must be a server-owned upload for this operation")
		}
		if err = webMigrationRegularFile(in.LegacyConfigPath, 1<<20); err != nil {
			return out, errors.New("legacy configuration upload is missing, unsafe or exceeds 1 MiB")
		}
	}
	roots, err := webMigrationValidateRoots(in.Roots, in.BootstrapConfig.ReadRoots, bootstrap)
	if err != nil {
		return out, err
	}
	// The private workspace is already server-owned. An anchored Mkdir prevents
	// creating parents through an attacker-selected external path.
	workspace, err := os.OpenRoot(base)
	if err != nil {
		return out, errors.New("migration workspace is unavailable")
	}
	defer workspace.Close()
	if err = workspace.Mkdir("targets", 0700); err != nil && !os.IsExist(err) {
		return out, errors.New("cannot create migration targets directory")
	}
	targetParent := filepath.Join(base, "targets")
	if _, err = webMigrationCanonicalDirectory(targetParent); err != nil {
		return out, errors.New("migration targets directory is unsafe")
	}
	target := filepath.Join(targetParent, in.TargetID)
	cfg, err := webMigrationTargetConfig(in, target)
	if err != nil {
		return out, err
	}
	out.RuntimeConfig = cfg
	runtime, err := json.Marshal(cfg)
	if err != nil {
		return out, errors.New("cannot encode target configuration")
	}
	runtime = append(runtime, '\n')
	limits := cfg.MigrationReview.Options()
	receipt, err := migrate.Run(ctx, migrate.Config{
		Snapshot: in.SourcePath, TargetDir: target, TargetDriver: "sqlite", Roots: roots,
		DryRun: in.DryRun, SourceQuiesced: in.SourceQuiesced,
		ExpectedProofSHA256: in.ExpectedProofSHA256,
		ExpectedSHA256:      in.ExpectedSnapshotSHA256,
		MaxBytes:            maxBackupBytes, MaxRowBytes: limits.MaxRowBytes, MaxFiles: limits.MaxFiles,
		RuntimeConfig: runtime,
		ValidateTarget: func(ctx context.Context, db *store.Store, metadata migrate.Metadata) error {
			switch metadata.SourceDBType {
			case "sqlite", "mysql", "postgres", "postgresql":
			default:
				return errors.New("snapshot must identify a supported SQLite, MySQL or PostgreSQL export")
			}
			// This scratch database is newly created by the importer. Its allocated
			// pages become the final main file after checkpoint; inspect the actual
			// page count rather than estimating database size from snapshot bytes.
			var pages, pageSize int64
			if err := db.DB.QueryRowContext(ctx, "PRAGMA page_count").Scan(&pages); err != nil {
				return errors.New("cannot verify target database size")
			}
			if err := db.DB.QueryRowContext(ctx, "PRAGMA page_size").Scan(&pageSize); err != nil {
				return errors.New("cannot verify target database size")
			}
			if pages < 1 || pageSize < 1 || pages > limits.MaxDatabaseBytes/pageSize {
				return errors.New("migration database exceeds the configured startup database byte limit")
			}
			return webMigrationValidateAuthentication(ctx, db, cfg)
		},
	})
	out.Receipt = receipt
	if err != nil {
		if receipt.Verified {
			// A failure after rename can still leave a complete target. Retain
			// its proof for the coordinator's read-only reconciliation path.
			out.ProofSHA256 = migrate.ReceiptProofSHA256(receipt)
		}
		return out, err
	}
	if err = webMigrationValidateReceiptLimits(receipt, cfg.MigrationReview); err != nil {
		return out, err
	}
	if receipt.Verified {
		out.ProofSHA256 = migrate.ReceiptProofSHA256(receipt)
		out.Summary = webMigrationSummarize(receipt)
	}
	return out, nil
}

// A review can only authorize a target that fits the same local budgets used
// at first boot. Include config.json in every manifest limit. The real import
// binds this complete manifest and runtime configuration before copying bytes.
func webMigrationValidateReceiptLimits(receipt migrate.Receipt, limits config.MigrationReview) error {
	if len(receipt.Files) > limits.MaxFiles {
		return errors.New("migration files exceed the configured startup file-count limit")
	}
	var total int64
	for _, file := range receipt.Files {
		if file.Size < 0 || file.Size > limits.MaxFileBytes || total > limits.MaxTotalFileBytes-file.Size {
			return errors.New("migration files exceed the configured startup file or total byte limit")
		}
		total += file.Size
	}
	// writeJSON uses this exact indentation. Publication changes true to false,
	// adding one byte; the timestamp and physical database digest have fixed
	// encoded lengths and do not alter the preview's size calculation.
	receipt.DryRun = false
	encoded, err := json.MarshalIndent(receipt, "", "  ")
	if err != nil || int64(len(encoded)) > limits.MaxReceiptBytes {
		return errors.New("migration receipt exceeds the configured startup receipt byte limit")
	}
	return nil
}

func webMigrationTargetConfig(in webMigrationPrepareInput, target string) (config.Config, error) {
	cfg := in.BootstrapConfig
	if in.LegacyConfigPath != "" {
		legacy, err := config.TranslateLegacyForMigration(in.LegacyConfigPath, target)
		if err != nil {
			return config.Config{}, errors.New("legacy configuration could not be translated safely")
		}
		// Legacy signing keys also protect imported encrypted authentication
		// records. Preserve the local key unless YAML explicitly supplies one.
		if legacy.JWTSecret != "" {
			cfg.JWTSecret, cfg.JWTAlgorithm = legacy.JWTSecret, legacy.JWTAlgorithm
		}
		// Defaults do not establish that the uploaded YAML explicitly chose an
		// admin name; read it through the same typed parser before overriding.
		original, err := config.ReadLegacy(in.LegacyConfigPath)
		if err != nil {
			return config.Config{}, errors.New("legacy configuration could not be translated safely")
		}
		if original.Admin.User != "" {
			cfg.AdminUsername = original.Admin.User
		}
		cfg.Timezone, cfg.Cache = legacy.Timezone, legacy.Cache
	}
	cfg.Driver = "sqlite"
	cfg.DataDir = target
	cfg.DSN = filepath.Join(target, "anidan.db")
	cfg.SessionAudience = in.SessionAudience
	cfg.AdminPassword = ""
	cfg.ReadRoots = nil
	cfg.WriteRoots = nil
	// Listener, public URL, static assets and local verification budgets remain
	// the running installation's operational settings, not uploaded YAML values.
	var err error
	cfg.Cache, err = cfg.Cache.ForIsolatedTarget(target)
	if err != nil {
		return config.Config{}, errors.New("target cache configuration is invalid")
	}
	if err = cfg.Validate(); err != nil {
		return config.Config{}, errors.New("target runtime configuration is invalid")
	}
	return cfg, nil
}

func webMigrationSummarize(receipt migrate.Receipt) webMigrationSummary {
	out := webMigrationSummary{
		SnapshotSHA256: receipt.SnapshotSHA256, TableCount: len(receipt.Tables),
		TableRecords: make(map[string]int64, len(receipt.Tables)), RootCount: len(receipt.Roots),
		ReviewRequired: true,
		Warnings: []string{
			"Imported pending jobs and schedules require a separate first-boot review before execution.",
			"Retain the original database, configuration and files for rollback; never run both writers together.",
			"A snapshot cannot reveal data already omitted by the original exporter; unreferenced files and remote caches are not included.",
			"The target uses an isolated local cache; source Redis/Valkey endpoints are not contacted.",
			"Keep the original administrator password and MFA credentials; passkeys require their registered site origin.",
		},
	}
	// An arbitrary metadata string is not reflected into the browser.
	switch receipt.SourceDBType {
	case "sqlite", "mysql", "postgres", "postgresql":
		out.SourceDBType = receipt.SourceDBType
	default:
		out.SourceDBType = "unknown"
	}
	for name, table := range receipt.Tables {
		out.TableRecords[name] = table.Rows
		out.TotalRecords += table.Rows
	}
	for _, file := range receipt.Files {
		if file.Destination != "config.json" {
			out.FileCount++
			out.FileBytes += file.Size
		}
	}
	return out
}

const webMigrationAuthenticationError = "Migration requires a preserved administrator with a supported password hash and usable MFA credentials; supply the matching legacy configuration and preview again"

// Inspect only the imported administrator required by the privileged migration
// UI. No passwords are guessed, accounts generated, OTPs upgraded or rows edited.
func webMigrationValidateAuthentication(ctx context.Context, db *store.Store, cfg config.Config) error {
	if len(cfg.JWTSecret) < 16 {
		return errors.New(webMigrationAuthenticationError)
	}
	name := cfg.AdminUsername
	if name == "" {
		name = "admin"
	}
	users, err := db.List(ctx, "users", store.Row{"username": name}, 2, 0)
	if err != nil || len(users) != 1 {
		return errors.New(webMigrationAuthenticationError)
	}
	u := users[0]
	if !webMigrationSupportedPasswordHash(authString(u["hashed_password"])) {
		return errors.New(webMigrationAuthenticationError)
	}
	if authBool(u["is_otp"]) {
		stored := authString(u["otp_secret"])
		plain, err := authDecryptOTP(stored, cfg.JWTSecret)
		if err != nil && authPlainOTP(stored) {
			plain, err = stored, nil
		}
		if err != nil || authTOTP(plain, time.Unix(0, 0)) == "" {
			return errors.New(webMigrationAuthenticationError)
		}
	}
	return ctx.Err()
}

func webMigrationSupportedPasswordHash(hash string) bool {
	start := 7
	switch {
	case len(hash) == 60 && (strings.HasPrefix(hash, "$2a$") || strings.HasPrefix(hash, "$2b$") || strings.HasPrefix(hash, "$2y$")) && hash[6] == '$':
	case len(hash) == 59 && strings.HasPrefix(hash, "$2$") && hash[5] == '$':
		start = 6
	default:
		return false
	}
	if _, err := bcrypt.Cost([]byte(hash)); err != nil {
		return false
	}
	// Cost alone accepts malformed salt/hash encodings. Strict decoding checks
	// their alphabets, lengths and unused trailing bits without expensive hashing.
	encoding := base64.NewEncoding("./ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789").WithPadding(base64.NoPadding).Strict()
	salt, err := encoding.DecodeString(hash[start : start+22])
	if err != nil || len(salt) != 16 {
		return false
	}
	digest, err := encoding.DecodeString(hash[start+22:])
	return err == nil && len(digest) == 23
}

func webMigrationPrepareHex(value string, size int) bool {
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == size && value == strings.ToLower(value)
}

func webMigrationCanonicalDirectory(name string) (string, error) {
	abs, err := filepath.Abs(name)
	if err != nil {
		return "", err
	}
	actual, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", err
	}
	if actual != abs {
		return "", errors.New("symbolic link in migration directory")
	}
	info, err := os.Lstat(actual)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", errors.New("migration path is not a directory")
	}
	return actual, nil
}

func webMigrationRegularFile(name string, max int64) error {
	if _, err := webMigrationCanonicalDirectory(filepath.Dir(name)); err != nil {
		return err
	}
	info, err := os.Lstat(name)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() < 1 || info.Size() > max {
		return errors.New("invalid migration file")
	}
	return nil
}

func webMigrationValidateRoots(roots []migrate.RootMapping, allowed []string, bootstrap string) ([]migrate.RootMapping, error) {
	if len(roots) > 128 {
		return nil, errors.New("at most 128 legacy file roots may be mapped")
	}
	out := append([]migrate.RootMapping(nil), roots...)
	for i, root := range out {
		if root.From == "" || len(root.From) > 4096 || root.To == "" {
			return nil, errors.New("each root mapping requires an explicit legacy prefix and readable copy")
		}
		actual, err := webMigrationCanonicalDirectory(root.To)
		if err != nil {
			return nil, errors.New("mapped source root must be an existing canonical directory")
		}
		if webMigrationWithin(bootstrap, actual) || webMigrationWithin(actual, bootstrap) {
			return nil, errors.New("mapped source roots must be separate from the active data directory")
		}
		permitted := false
		for _, configured := range allowed {
			base, err := webMigrationCanonicalDirectory(configured)
			if err == nil && base != string(filepath.Separator) && webMigrationWithin(base, actual) {
				permitted = true
				break
			}
		}
		if !permitted {
			return nil, errors.New("mapped source root is outside configured readable copy roots")
		}
		out[i].To = actual
	}
	return out, nil
}

func webMigrationWithin(root, name string) bool {
	rel, err := filepath.Rel(root, name)
	return err == nil && filepath.IsLocal(rel)
}
