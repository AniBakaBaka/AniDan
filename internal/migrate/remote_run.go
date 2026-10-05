// SPDX-License-Identifier: AGPL-3.0-only
package migrate

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/AniBakaBaka/AniDan/internal/store"
)

const remoteWorkDirectory = ".anidan-remote-work"

func runRemoteMigration(ctx context.Context, cfg Config) (Receipt, error) {
	var receipt Receipt
	if !cfg.DryRun && !cfg.SourceQuiesced {
		return receipt, errors.New("stop legacy database and file writers before remote migration")
	}
	if cfg.TargetDSN == "" {
		return receipt, errors.New("remote migration requires a target DSN supplied through the selected environment variable")
	}
	if (cfg.Snapshot == "") == (cfg.SourceDSN == "") && !(cfg.Resume && cfg.Snapshot == "" && cfg.SourceDSN == "" && !cfg.DryRun) {
		return receipt, errors.New("choose exactly one snapshot or read-only source DSN")
	}
	if cfg.Resume && cfg.SourceDSN != "" {
		return receipt, errors.New("resume requires the saved snapshot, not another live source export")
	}
	if cfg.MaxBytes < 0 || cfg.MaxRowBytes < 0 || cfg.MaxFiles < 0 {
		return receipt, errors.New("remote migration limits must be nonnegative")
	}
	if cfg.MaxBytes <= 0 {
		cfg.MaxBytes = defaultMaxBytes
	}
	if cfg.MaxRowBytes <= 0 {
		cfg.MaxRowBytes = 32 << 20
	}
	if cfg.MaxRowBytes > 256<<20 {
		return receipt, errors.New("remote migration row limit must not exceed256MiB")
	}
	if cfg.MaxBytes == int64(^uint64(0)>>1) || cfg.MaxRowBytes > cfg.MaxBytes {
		return receipt, errors.New("invalid migration byte limits")
	}
	if cfg.TargetDir == "" {
		return receipt, errors.New("remote migration requires an explicit new target data directory")
	}
	target, err := filepath.Abs(cfg.TargetDir)
	if err != nil {
		return receipt, err
	}
	runtimeHash, err := validateRuntimeConfig(cfg.RuntimeConfig)
	if err != nil {
		return receipt, err
	}
	if err = validateRemoteRuntimeConfig(cfg.RuntimeConfig, cfg.TargetDriver, target); err != nil {
		return receipt, err
	}
	limits, err := remoteStartupOptions(cfg)
	if err != nil {
		return receipt, err
	}
	remote, err := openRemoteTarget(ctx, cfg.TargetDriver, cfg.TargetDSN)
	if err != nil {
		return receipt, err
	}
	defer remote.Close()
	if err = remote.Acquire(ctx); err != nil {
		return receipt, err
	}
	if cfg.Resume && cfg.Snapshot == "" && cfg.SourceDSN == "" {
		var j journal
		if err = readJSONContext(ctx, filepath.Join(target, ".anidan-migration.json"), &j); err != nil {
			return receipt, errors.New("verification-only resume requires a completed published target; supply the saved snapshot for an interrupted staging directory")
		}
		if j.TargetDir != target || j.TargetDriver != remote.Binding().Driver || j.TargetBindingSHA256 != remote.Binding().SHA256() {
			return receipt, errors.New("published remote target identity differs")
		}
		if len(cfg.Roots) > 0 {
			roots, _ := json.Marshal(cfg.Roots)
			if startupHash(roots) != j.RootsSHA256 {
				return receipt, errors.New("published root mapping differs")
			}
		}
		return verifyRemoteExisting(ctx, target, j, remote, cfg.MaxBytes, cfg.MaxRowBytes, limits)
	}
	if cfg.SourceDSN != "" {
		temporary, e := os.MkdirTemp("", "anidan-remote-export-")
		if e != nil {
			return receipt, e
		}
		defer os.RemoveAll(temporary)
		cfg.Snapshot = filepath.Join(temporary, "snapshot.json")
		if e = exportWithLimits(ctx, cfg.SourceDriver, cfg.SourceDSN, cfg.Snapshot, cfg.MaxBytes, cfg.MaxRowBytes); e != nil {
			return receipt, errors.New("read-only source export failed; no remote schema was created")
		}
	}
	snapshot, err := filepath.Abs(cfg.Snapshot)
	if err != nil {
		return receipt, err
	}
	if err = ensureTargetSeparate(target, snapshot, cfg.Roots); err != nil {
		return receipt, err
	}
	snapshotFile, e := openRegularPath(ctx, snapshot)
	if e != nil {
		return receipt, e
	}
	info, e := snapshotFile.Stat()
	snapshotFile.Close()
	if e != nil {
		return receipt, e
	}
	if info.Size() > cfg.MaxBytes {
		return receipt, errors.New("encoded snapshot exceeds remote migration byte limit")
	}
	sum, _, err := hashFile(ctx, snapshot)
	if err != nil {
		return receipt, err
	}
	if cfg.ExpectedSHA256 != "" && !strings.EqualFold(sum, cfg.ExpectedSHA256) {
		return receipt, errors.New("snapshot SHA-256 mismatch")
	}
	roots, _ := json.Marshal(cfg.Roots)
	j := journal{SnapshotSHA256: sum, SchemaSHA256: store.SchemaFingerprint(), TargetDir: target, RootsSHA256: startupHash(roots), RuntimeConfigSHA256: runtimeHash, TargetDriver: remote.Binding().Driver, TargetBindingSHA256: remote.Binding().SHA256()}
	if _, e := os.Lstat(target); e == nil {
		if !cfg.Resume || cfg.DryRun {
			return receipt, errors.New("remote migration target directory already exists; refusing overwrite")
		}
		return verifyRemoteExisting(ctx, target, j, remote, cfg.MaxBytes, cfg.MaxRowBytes, limits)
	} else if !os.IsNotExist(e) {
		return receipt, e
	}
	stage := target + ".staging"
	if cfg.DryRun {
		stage, err = os.MkdirTemp("", "anidan-remote-preflight-")
		if err != nil {
			return receipt, err
		}
		defer os.RemoveAll(stage)
	}
	if !cfg.DryRun {
		if err = os.Mkdir(stage, 0700); err != nil {
			if !os.IsExist(err) || !cfg.Resume {
				return receipt, errors.New("remote migration staging directory exists; resume only the same owned migration")
			}
			if err = rejectSymlink(stage); err != nil {
				return receipt, err
			}
			var previous journal
			if err = readJSONContext(ctx, filepath.Join(stage, ".anidan-migration.json"), &previous); err != nil {
				return receipt, errors.New("unrecognized remote migration staging directory")
			}
			j.MigrationID = previous.MigrationID
			if previous != j || !validRemoteHex(j.MigrationID, 16) {
				return receipt, errors.New("remote staging identity differs; existing data preserved")
			}
		} else {
			var id [16]byte
			if _, err = rand.Read(id[:]); err != nil {
				return receipt, err
			}
			j.MigrationID = hex.EncodeToString(id[:])
			if err = writeJSON(filepath.Join(stage, ".anidan-migration.json"), j); err != nil {
				return receipt, err
			}
		}
	} else {
		j.MigrationID = strings.Repeat("0", 32)
	}
	if !cfg.DryRun {
		unlock, e := acquireStageLock(stage)
		if e != nil {
			return receipt, e
		}
		defer unlock()
	}
	owner := remoteOwner(j)
	if err = remote.Preflight(ctx, owner, cfg.Resume); err != nil {
		return receipt, err
	}
	// Nothing remote has been initialized yet. Preserve the exact source export
	// and validated preparation database in this owned staging area on failures.
	work := filepath.Join(stage, remoteWorkDirectory)
	if err = os.Mkdir(work, 0700); err != nil && !os.IsExist(err) {
		return receipt, err
	}
	if err = rejectSymlink(work); err != nil {
		return receipt, err
	}
	if err = checkRemoteWorkEntries(work); err != nil {
		return receipt, err
	}
	savedSnapshot := filepath.Join(work, "source-snapshot.json")
	if err = retainRemoteSnapshot(ctx, snapshot, savedSnapshot, sum); err != nil {
		return receipt, err
	}
	bridgePath := filepath.Join(work, "prepared.db")
	for _, suffix := range []string{"", "-wal", "-shm"} {
		p := bridgePath + suffix
		if err = rejectNonregularIfExists(p); err != nil {
			return receipt, err
		}
		if err = os.Remove(p); err != nil && !os.IsNotExist(err) {
			return receipt, err
		}
	}
	bridge, err := store.Open(ctx, "sqlite", bridgePath)
	if err != nil {
		return receipt, err
	}
	defer bridge.Close()
	if err = bridge.Init(ctx); err != nil {
		return receipt, err
	}
	if _, err = bridge.DB.ExecContext(ctx, "PRAGMA cache_size=-2048"); err != nil {
		return receipt, err
	}
	mapper, err := newFileMapper(cfg.Roots, target)
	if err != nil {
		return receipt, err
	}
	defer mapper.Close()
	if cfg.MaxFiles > 0 {
		mapper.limit = cfg.MaxFiles
	}
	meta, counts, err := importSnapshot(ctx, bridge, savedSnapshot, cfg.MaxBytes, cfg.MaxRowBytes, mapper)
	if err != nil {
		if ctx.Err() != nil {
			return receipt, ctx.Err()
		}
		return receipt, errors.New("remote migration snapshot validation or file mapping failed; no remote schema was created")
	}
	again, _, err := hashFile(ctx, savedSnapshot)
	if err != nil || again != sum {
		return receipt, errors.New("saved snapshot changed during preparation")
	}
	if err = validateRelations(ctx, bridge); err != nil {
		return receipt, err
	}
	tables, err := checksumLogicalTables(ctx, bridge, bridge.DB, min(cfg.MaxBytes, limits.MaxDatabaseBytes), min(cfg.MaxRowBytes, limits.MaxRowBytes))
	if err != nil {
		return receipt, err
	}
	for name, n := range counts {
		if tables[name].Rows != n {
			return receipt, fmt.Errorf("prepared row count mismatch: %s", name)
		}
	}
	mappedFiles := mapper.receipts()
	files := mappedFiles
	if len(cfg.RuntimeConfig) > 0 {
		files = append(files, FileReceipt{Source: "translated runtime configuration", Destination: "config.json", SHA256: runtimeHash, Size: int64(len(cfg.RuntimeConfig))})
	}
	binding := remote.Binding()
	receipt = Receipt{Version: "2.0", LegacyCommit: LegacyCommit, SchemaSHA256: j.SchemaSHA256, SnapshotSHA256: sum, DatabaseSHA256: logicalDatabaseHash(tables), SourceDBType: meta.SourceDBType, SourceCreatedAt: meta.CreatedAt, TargetDir: target, CreatedAt: time.Now().UTC().Format(time.RFC3339), DryRun: cfg.DryRun, Verified: true, Tables: tables, Files: files, Roots: cfg.Roots, RemoteTarget: &binding, RemoteOwnership: &owner, Warnings: []string{
		"The remote target and local files do not share a distributed transaction. Owned partial schema or committed data may remain after interruption; resume only the same migration and never adopt or overwrite other data.",
		"MySQL/MariaDB schema DDL implicitly commits. Failed schema preparation is preserved; no DROP, TRUNCATE or destructive rollback is performed.",
		"Preparation uses additional local disk for an exact source snapshot and SQLite bridge. Source database and XML writers must remain stopped until cutover. Private .anidan-remote-work files may remain after an interrupted final cleanup; never serve or expose them.",
		"Historical pending tasks and schedules require explicit review at first boot. Runtime target credentials must be supplied separately using ANIDAN_DSN.",
	}, Rollback: "Stop AniDan before rollback. Retain the owned remote database and migration directory for review, and restart the unchanged legacy service against its original database and files. No automatic remote cleanup is performed."}
	if err = validateRemotePreparedReceipt(receipt, limits); err != nil {
		return receipt, err
	}
	if cfg.DryRun {
		return receipt, nil
	}
	if err = copyFiles(ctx, mappedFiles, stage); err != nil {
		return receipt, err
	}
	if len(cfg.RuntimeConfig) > 0 {
		if err = writeRuntimeConfig(ctx, stage, cfg.RuntimeConfig, runtimeHash); err != nil {
			return receipt, err
		}
	}
	if _, err = bridge.DB.ExecContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		return receipt, err
	}
	if err = syncRemotePreparation(stage, files); err != nil {
		return receipt, err
	}
	if err = remote.PrepareSchema(ctx, owner, cfg.Resume); err != nil {
		return receipt, err
	}
	if err = transferRemoteRows(ctx, bridge, remote, tables, cfg.MaxBytes, cfg.MaxRowBytes); err != nil {
		return receipt, err
	}
	if err = remote.ResetSequences(ctx); err != nil {
		return receipt, err
	}
	if err = remote.VerifySchema(ctx); err != nil {
		return receipt, err
	}
	actual, err := remoteConsistentTables(ctx, remote, cfg.MaxBytes, cfg.MaxRowBytes)
	if err != nil {
		return receipt, err
	}
	if !reflect.DeepEqual(actual, tables) {
		return receipt, errors.New("remote data changed before publication; target preserved for review")
	}
	if err = verifyRemoteStagedFiles(ctx, stage, files); err != nil {
		return receipt, err
	}
	if err = bridge.Close(); err != nil {
		return receipt, err
	}
	// These are exact owned scratch names, never arbitrary stage contents.
	if err = replaceRemoteReceipt(stage, receipt); err != nil {
		return receipt, err
	}
	if err = publishRemoteDirectory(stage, target); err != nil {
		return receipt, err
	}
	if err = removeRemoteWork(filepath.Join(target, remoteWorkDirectory)); err != nil {
		return receipt, errors.New("remote migration is published, but private preparation cleanup is incomplete; preserve it and resume this migration for verification")
	}
	return receipt, nil
}

func remoteOwner(j journal) RemoteOwnership {
	return RemoteOwnership{Version: 1, MigrationID: j.MigrationID, TargetBindingSHA256: j.TargetBindingSHA256, SnapshotSHA256: j.SnapshotSHA256, SchemaSHA256: j.SchemaSHA256, TargetDirSHA256: remoteHash(j.TargetDir)}
}

func validateRemoteRuntimeConfig(data []byte, driver, target string) error {
	if len(data) == 0 {
		return nil
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := checkStartupJSON(decoder, 0); err != nil {
		return errors.New("remote runtime configuration contains duplicate or malformed fields")
	}
	if _, err := decoder.Token(); err != io.EOF {
		return errors.New("remote runtime configuration contains trailing data")
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil || fields == nil {
		return errors.New("remote runtime configuration must be an object")
	}
	for key := range fields {
		for _, canonical := range []string{"driver", "dsn", "dataDir", "migrationReview"} {
			if strings.EqualFold(key, canonical) && key != canonical {
				return errors.New("remote runtime configuration identity fields must use canonical names")
			}
		}
	}
	var config struct {
		Driver  string `json:"driver"`
		DSN     string `json:"dsn"`
		DataDir string `json:"dataDir"`
	}
	if err := json.Unmarshal(data, &config); err != nil {
		return errors.New("invalid remote runtime configuration")
	}
	if driver == "postgresql" {
		driver = "postgres"
	}
	if config.Driver != driver || config.DSN != "" || config.DataDir != target {
		return errors.New("remote runtime configuration must bind the driver and data directory without a DSN")
	}
	return nil
}

func checkRemoteWorkEntries(work string) error {
	entries, err := os.ReadDir(work)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		switch entry.Name() {
		case "source-snapshot.json", "source-snapshot.tmp", "prepared.db", "prepared.db-wal", "prepared.db-shm":
		default:
			return errors.New("unexpected remote migration scratch entry; preserved for review")
		}
		if err = rejectNonregularIfExists(filepath.Join(work, entry.Name())); err != nil {
			return err
		}
	}
	return nil
}
func retainRemoteSnapshot(ctx context.Context, source, destination, sum string) error {
	if _, err := os.Lstat(destination); err == nil {
		actual, _, e := hashFile(ctx, destination)
		if e != nil || actual != sum {
			return errors.New("saved remote snapshot differs; preserved for review")
		}
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	in, err := openRegularPath(ctx, source)
	if err != nil {
		return err
	}
	defer in.Close()
	temporary := filepath.Join(filepath.Dir(destination), "source-snapshot.tmp")
	if err = rejectNonregularIfExists(temporary); err != nil {
		return err
	}
	if err = os.Remove(temporary); err != nil && !os.IsNotExist(err) {
		return err
	}
	out, err := os.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(out, contextReader{ctx, in})
	syncErr := out.Sync()
	closeErr := out.Close()
	if copyErr != nil {
		return copyErr
	}
	if syncErr != nil {
		return syncErr
	}
	if closeErr != nil {
		return closeErr
	}
	actual, _, err := hashFile(ctx, temporary)
	if err != nil || actual != sum {
		return errors.New("source snapshot changed while retaining it")
	}
	root, err := openDirectoryRoot(filepath.Dir(destination))
	if err != nil {
		return err
	}
	defer root.Close()
	if err = root.Link("source-snapshot.tmp", filepath.Base(destination)); err != nil {
		return err
	}
	if err = root.Remove("source-snapshot.tmp"); err != nil {
		return err
	}

	return syncDir(filepath.Dir(destination))
}
func removeRemoteWork(work string) error {
	if err := checkRemoteWorkEntries(work); err != nil {
		return err
	}
	for _, name := range []string{"prepared.db-wal", "prepared.db-shm", "prepared.db", "source-snapshot.json", "source-snapshot.tmp"} {
		if err := os.Remove(filepath.Join(work, name)); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	if err := os.Remove(work); err != nil {
		return err
	}
	return syncDir(filepath.Dir(work))
}
func verifyRemoteStagedFiles(ctx context.Context, stage string, files []FileReceipt) error {
	root, err := openDirectoryRoot(stage)
	if err != nil {
		return err
	}
	defer root.Close()
	for _, file := range files {
		f, e := openRegularDestination(ctx, root, filepath.FromSlash(file.Destination))
		if e != nil {
			return e
		}
		sum, size, e := hashOpenFile(ctx, f)
		f.Close()
		if e != nil || sum != file.SHA256 || size != file.Size {
			return errors.New("staged migration files changed before publication")
		}
	}
	return nil
}
func replaceRemoteReceipt(stage string, receipt Receipt) error {
	path := filepath.Join(stage, "migration-receipt.json")
	if err := rejectNonregularIfExists(path); err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return writeJSON(path, receipt)
}
func publishRemoteDirectory(stage, target string) error {
	// acquireStageLock remains held by the caller across rename, and Linux
	// RENAME_NOREPLACE is the final exclusion. Do not create a crash-persistent
	// O_EXCL sentinel in the parent. Existing unrelated/legacy sentinels are
	// preserved and still cause an explicit refusal.
	if _, err := os.Lstat(target + ".publish-lock"); err == nil {
		return errors.New("remote migration publication lock is unavailable")
	} else if !os.IsNotExist(err) {
		return err
	}
	var err error
	if _, err = os.Lstat(target); err == nil {
		return errors.New("remote migration target directory appeared; refusing overwrite")
	} else if !os.IsNotExist(err) {
		return err
	}
	if err = syncDir(stage); err != nil {
		return err
	}
	if err = publishDirectory(stage, target); err != nil {
		return err
	}
	return syncDir(filepath.Dir(target))
}
func verifyRemoteExisting(ctx context.Context, target string, j journal, remote *remoteTarget, maxBytes, maxRowBytes int64, limits StartupVerificationLimits) (Receipt, error) {
	var receipt Receipt
	evidence, err := ReadStartupEvidence(ctx, target, limits)
	if err != nil || evidence == nil {
		return receipt, errors.New("existing target is not a verified remote migration")
	}
	receipt = evidence.Receipt
	var old journal
	if err = readJSONContext(ctx, filepath.Join(target, ".anidan-migration.json"), &old); err != nil {
		return receipt, err
	}
	j.MigrationID = old.MigrationID
	if j != old || receipt.RemoteTarget == nil || *receipt.RemoteTarget != remote.Binding() || receipt.RemoteOwnership == nil {
		return receipt, errors.New("existing remote target belongs to a different migration")
	}
	if err = remote.Preflight(ctx, *receipt.RemoteOwnership, true); err != nil {
		return receipt, err
	}
	if err = remote.VerifySchema(ctx); err != nil {
		return receipt, err
	}
	tables, err := remoteConsistentTables(ctx, remote, maxBytes, maxRowBytes)
	if err != nil {
		return receipt, err
	}
	if !reflect.DeepEqual(tables, receipt.Tables) || logicalDatabaseHash(tables) != receipt.DatabaseSHA256 {
		return receipt, errors.New("published remote target has changed; refusing resume or overwrite")
	}
	if err = VerifyStartupContents(ctx, target, evidence); err != nil {
		return receipt, err
	}
	work := filepath.Join(target, remoteWorkDirectory)
	if _, e := os.Lstat(work); e == nil {
		if e = rejectSymlink(work); e != nil {
			return receipt, e
		}
		if e = removeRemoteWork(work); e != nil {
			return receipt, e
		}
	} else if !os.IsNotExist(e) {
		return receipt, e
	}
	return receipt, nil
}

func remoteStartupOptions(cfg Config) (StartupVerificationLimits, error) {
	limits := DefaultStartupVerificationLimits()
	if len(cfg.RuntimeConfig) > 0 {
		var wrapper struct {
			MigrationReview json.RawMessage `json:"migrationReview"`
		}
		if err := json.Unmarshal(cfg.RuntimeConfig, &wrapper); err != nil {
			return limits, errors.New("invalid runtime verification settings")
		}
		if len(wrapper.MigrationReview) > 0 {
			if string(wrapper.MigrationReview) == "null" {
				return limits, errors.New("runtime verification settings must not be null")
			}
			if err := json.Unmarshal(wrapper.MigrationReview, &limits); err != nil {
				return limits, errors.New("invalid runtime verification settings")
			}
		}
	} else {
		if cfg.MaxFiles > 0 {
			limits.MaxFiles = cfg.MaxFiles
		}
		if cfg.MaxRowBytes > 0 {
			limits.MaxRowBytes = cfg.MaxRowBytes
		}
		if cfg.MaxBytes > limits.MaxDatabaseBytes {
			limits.MaxDatabaseBytes = cfg.MaxBytes
		}
	}
	return limits, limits.Validate()
}
func validateRemotePreparedReceipt(r Receipt, limits StartupVerificationLimits) error {
	if err := limits.Validate(); err != nil {
		return err
	}
	switch r.SourceDBType {
	case "sqlite", "mysql", "postgres", "postgresql":
	default:
		return errors.New("unsupported source database type in snapshot")
	}
	if len(r.Files) > limits.MaxFiles {
		return errors.New("prepared file manifest exceeds configured first-boot file limit")
	}
	var total int64
	for _, f := range r.Files {
		if f.Size < 0 || f.Size > limits.MaxFileBytes || total > limits.MaxTotalFileBytes-f.Size {
			return errors.New("prepared referenced files exceed configured first-boot byte limits")
		}
		total += f.Size
	}
	encoded, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	if int64(len(encoded)) > limits.MaxReceiptBytes {
		return errors.New("prepared receipt exceeds configured first-boot metadata limit")
	}
	return nil
}

// Persist both file leaves and every newly created parent link before remote
// ownership/DDL can commit. This closes the avoidable ordering gap; hardware or
// filesystem power-loss guarantees are not established by process fixtures.
func syncRemotePreparation(stage string, files []FileReceipt) error {
	directories := map[string]bool{stage: true, filepath.Join(stage, remoteWorkDirectory): true}
	for _, file := range files {
		directory := filepath.Dir(filepath.Join(stage, filepath.FromSlash(file.Destination)))
		for directory != stage {
			if !within(stage, directory) {
				return errors.New("prepared directory escaped staging root")
			}
			directories[directory] = true
			directory = filepath.Dir(directory)
		}
	}
	names := make([]string, 0, len(directories))
	for name := range directories {
		names = append(names, name)
	}
	sort.Slice(names, func(i, j int) bool { return len(names[i]) > len(names[j]) })
	for _, name := range names {
		if err := syncDir(name); err != nil {
			return err
		}
	}
	return syncDir(filepath.Dir(stage))
}
