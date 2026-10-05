// SPDX-License-Identifier: AGPL-3.0-only
// Legacy backup format derived from misaka_danmu_server database_backup.py,
// pinned at 01751526f6e4154bcc8f517481d02b68cb2684a9. See LICENSE/NOTICE.
package migrate

import (
	"bufio"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"database/sql"
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

const LegacyCommit = "01751526f6e4154bcc8f517481d02b68cb2684a9"
const defaultMaxBytes int64 = 8 << 30

type Config struct {
	Snapshot     string
	SourceDriver string
	SourceDSN    string
	TargetDir    string
	TargetDriver string
	TargetDSN    string `json:"-"`
	Roots        []RootMapping
	DryRun       bool
	Resume       bool
	// SourceQuiesced acknowledges that old database/files writers are stopped.
	// The importer cannot coordinate external services or make a cross-resource snapshot.
	SourceQuiesced bool
	ExpectedSHA256 string
	// ExpectedProofSHA256 binds a reviewed SQLite preview to the exact logical
	// tables, file contents, paths, roots and runtime configuration published.
	// Empty retains the existing CLI behavior.
	ExpectedProofSHA256 string
	MaxBytes            int64
	MaxRowBytes         int64
	MaxFiles            int
	RuntimeConfig       []byte
	// ValidateTarget may inspect the imported, unpublished SQLite store. It must
	// not mutate imported rows or contact external services. Returning an error
	// prevents both preview approval and target publication.
	ValidateTarget func(context.Context, *store.Store, Metadata) error `json:"-"`
}
type Metadata struct {
	Version      string           `json:"version"`
	SourceDBType string           `json:"source_db_type"`
	CreatedAt    string           `json:"created_at"`
	Tables       []string         `json:"tables"`
	TableRecords map[string]int64 `json:"table_records"`
	TotalRecords int64            `json:"total_records"`
}
type TableReceipt struct {
	Rows   int64  `json:"rows"`
	SHA256 string `json:"sha256"`
}
type Receipt struct {
	Version         string                  `json:"version"`
	LegacyCommit    string                  `json:"legacy_commit"`
	SchemaSHA256    string                  `json:"schema_sha256"`
	SnapshotSHA256  string                  `json:"snapshot_sha256"`
	DatabaseSHA256  string                  `json:"database_sha256,omitempty"`
	SourceDBType    string                  `json:"source_db_type"`
	SourceCreatedAt string                  `json:"source_created_at"`
	TargetDir       string                  `json:"target_dir"`
	CreatedAt       string                  `json:"created_at"`
	DryRun          bool                    `json:"dry_run"`
	Verified        bool                    `json:"verified"`
	Tables          map[string]TableReceipt `json:"tables"`
	Files           []FileReceipt           `json:"files"`
	Roots           []RootMapping           `json:"roots"`
	Warnings        []string                `json:"warnings"`
	Rollback        string                  `json:"rollback"`
	RemoteTarget    *RemoteTargetBinding    `json:"remote_target,omitempty"`
	RemoteOwnership *RemoteOwnership        `json:"remote_ownership,omitempty"`
}
type journal struct {
	SnapshotSHA256, SchemaSHA256, TargetDir, RootsSHA256, RuntimeConfigSHA256 string
	TargetDriver, TargetBindingSHA256, MigrationID                            string `json:",omitempty"`
}

// Run performs a strict preflight and byte-preserving import into an unpublished,
// new directory. Only a fully verified directory is renamed to TargetDir.
// It never initializes or mutates a source database or overwrites TargetDir.
func Run(ctx context.Context, cfg Config) (Receipt, error) {
	var receipt Receipt
	switch strings.ToLower(cfg.TargetDriver) {
	case "", "sqlite", "sqlite3":
		if cfg.TargetDSN != "" {
			return receipt, errors.New("SQLite migration target is selected by TargetDir, not a target DSN")
		}
	case "mysql", "postgres", "postgresql":
		if cfg.ExpectedProofSHA256 != "" || cfg.ValidateTarget != nil {
			return receipt, errors.New("reviewed preview proof is supported only for SQLite migration targets")
		}
		return runRemoteMigration(ctx, cfg)
	default:
		return receipt, errors.New("unsupported migration target driver")
	}
	if !cfg.DryRun && !cfg.SourceQuiesced {
		return receipt, errors.New("stop legacy imports, refresh, webhooks and scheduled writers; rerun with SourceQuiesced acknowledged")
	}
	if cfg.SourceDSN != "" && cfg.Resume {
		return receipt, errors.New("live re-export has a new snapshot identity; resume requires a saved export supplied with -snapshot")
	}
	if cfg.Snapshot != "" && cfg.SourceDSN != "" {
		return receipt, errors.New("choose snapshot or live read-only source, not both")
	}
	if cfg.Snapshot == "" && cfg.SourceDSN == "" {
		return receipt, errors.New("a legacy v2 snapshot or read-only source DSN is required")
	}
	if cfg.MaxBytes <= 0 {
		cfg.MaxBytes = defaultMaxBytes
	}
	if cfg.MaxRowBytes <= 0 {
		cfg.MaxRowBytes = 32 << 20
	}
	if cfg.SourceDSN != "" {
		tmp, e := os.MkdirTemp("", "anidan-export-")
		if e != nil {
			return receipt, e
		}
		defer os.RemoveAll(tmp)
		cfg.Snapshot = filepath.Join(tmp, "snapshot.json")
		if e = Export(ctx, cfg.SourceDriver, cfg.SourceDSN, cfg.Snapshot); e != nil {
			return receipt, e
		}
	}
	snapshot, e := filepath.Abs(cfg.Snapshot)
	if e != nil {
		return receipt, e
	}
	sum, _, e := hashFile(ctx, snapshot)
	if e != nil {
		return receipt, e
	}
	if cfg.ExpectedSHA256 != "" && !strings.EqualFold(sum, cfg.ExpectedSHA256) {
		return receipt, errors.New("snapshot SHA-256 mismatch")
	}
	if cfg.TargetDir == "" {
		cfg.TargetDir = "anidan-migrated-" + time.Now().UTC().Format("20060102-150405")
	}
	target, e := filepath.Abs(cfg.TargetDir)
	if e != nil {
		return receipt, e
	}
	if e = ensureTargetSeparate(target, snapshot, cfg.Roots); e != nil {
		return receipt, e
	}
	rootBytes, _ := json.Marshal(cfg.Roots)
	rh := sha256.Sum256(rootBytes)
	runtimeHash, e := validateRuntimeConfig(cfg.RuntimeConfig)
	if e != nil {
		return receipt, e
	}
	j := journal{SnapshotSHA256: sum, SchemaSHA256: store.SchemaFingerprint(), TargetDir: target, RootsSHA256: hex.EncodeToString(rh[:]), RuntimeConfigSHA256: runtimeHash}
	if _, err := os.Lstat(target); err == nil {
		if cfg.Resume && !cfg.DryRun {
			receipt, e = verifyExisting(ctx, target, j)
			if e == nil {
				e = verifyExpectedProof(receipt, cfg.ExpectedProofSHA256)
			}
			return receipt, e
		}
		return receipt, fmt.Errorf("target already exists: %s; refusing to overwrite", target)
	} else if !os.IsNotExist(err) {
		return receipt, err
	}
	stage := target + ".staging"
	if cfg.DryRun {
		stage, e = os.MkdirTemp("", "anidan-preflight-")
		if e != nil {
			return receipt, e
		}
		defer os.RemoveAll(stage)
	} else {
		if e = os.Mkdir(stage, 0700); e != nil {
			if !os.IsExist(e) || !cfg.Resume {
				return receipt, fmt.Errorf("staging directory exists or cannot be created; use Resume only for the same snapshot: %w", e)
			}
			if e = rejectSymlink(stage); e != nil {
				return receipt, e
			}
			var old journal
			if e = readJSONContext(ctx, filepath.Join(stage, ".anidan-migration.json"), &old); e != nil {
				return receipt, fmt.Errorf("unrecognized staging directory: %w", e)
			}
			if old != j {
				return receipt, errors.New("staging journal differs; choose a new target")
			}
		} else if e = writeJSON(filepath.Join(stage, ".anidan-migration.json"), j); e != nil {
			return receipt, e
		}
	}
	if !cfg.DryRun {
		unlock, err := acquireStageLock(stage)
		if err != nil {
			return receipt, err
		}
		defer unlock()
	}
	// Receipt is regenerated if a prior run crashed after verification but before publication.
	if e = rejectNonregularIfExists(filepath.Join(stage, "migration-receipt.json")); e != nil {
		return receipt, e
	}
	if e = os.Remove(filepath.Join(stage, "migration-receipt.json")); e != nil && !os.IsNotExist(e) {
		return receipt, e
	}
	dbPath := filepath.Join(stage, "anidan.db")
	// Only importer-owned, unpublished scratch databases may be restarted.
	for _, suffix := range []string{"", "-wal", "-shm"} {
		p := dbPath + suffix
		if e = rejectNonregularIfExists(p); e != nil {
			return receipt, e
		}
		if e = os.Remove(p); e != nil && !os.IsNotExist(e) {
			return receipt, e
		}
	}
	s, e := store.Open(ctx, "sqlite", dbPath)
	if e != nil {
		return receipt, e
	}
	closed := false
	defer func() {
		if !closed {
			s.Close()
		}
	}()
	if e = s.Init(ctx); e != nil {
		return receipt, e
	}
	if _, e = s.DB.ExecContext(ctx, "PRAGMA cache_size=-2048"); e != nil {
		return receipt, e
	}
	mapper, e := newFileMapper(cfg.Roots, target)
	if e != nil {
		return receipt, e
	}
	defer mapper.Close()
	if cfg.MaxFiles > 0 {
		mapper.limit = cfg.MaxFiles
	}
	meta, counts, e := importSnapshot(ctx, s, snapshot, cfg.MaxBytes, cfg.MaxRowBytes, mapper)
	if e != nil {
		return receipt, e
	}
	again, _, e := hashFile(ctx, snapshot)
	if e != nil {
		return receipt, e
	}
	if again != sum {
		return receipt, errors.New("snapshot changed during import")
	}
	if e = validateRelations(ctx, s); e != nil {
		return receipt, e
	}
	if cfg.ValidateTarget != nil {
		if e = cfg.ValidateTarget(ctx, s, meta); e != nil {
			return receipt, e
		}
	}
	tables, e := checksumTables(ctx, s)
	if e != nil {
		return receipt, e
	}
	for name, n := range counts {
		if tables[name].Rows != n {
			return receipt, fmt.Errorf("verification row count differs for %s", name)
		}
	}
	files := mapper.receipts()
	libraryFileCount := len(files)
	if len(cfg.RuntimeConfig) > 0 {
		files = append(files, FileReceipt{Source: "translated runtime configuration", Destination: "config.json", SHA256: runtimeHash, Size: int64(len(cfg.RuntimeConfig))})
	}
	// A reviewed web preview has already checked its startup budgets. Reject
	// changed source contents or configuration before copying any source file;
	// the final proof check below still follows verification of the copied bytes.
	if e = verifyExpectedProof(Receipt{
		Version: "1.0", LegacyCommit: LegacyCommit, SchemaSHA256: store.SchemaFingerprint(),
		SnapshotSHA256: sum, SourceDBType: meta.SourceDBType, SourceCreatedAt: meta.CreatedAt,
		TargetDir: target, Tables: tables, Files: files, Roots: cfg.Roots,
	}, cfg.ExpectedProofSHA256); e != nil {
		return receipt, e
	}
	if !cfg.DryRun {
		if e = copyFiles(ctx, files[:libraryFileCount], stage); e != nil {
			return receipt, e
		}
	}
	if len(cfg.RuntimeConfig) > 0 && !cfg.DryRun {
		if e = writeRuntimeConfig(ctx, stage, cfg.RuntimeConfig, runtimeHash); e != nil {
			return receipt, e
		}
	}
	if _, e = s.DB.ExecContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)"); e != nil {
		return receipt, e
	}
	if e = s.Close(); e != nil {
		return receipt, e
	}
	closed = true
	dbHash, _, e := hashDestinationPath(ctx, dbPath)
	if e != nil {
		return receipt, e
	}
	receipt = Receipt{Version: "1.0", LegacyCommit: LegacyCommit, SchemaSHA256: store.SchemaFingerprint(), SnapshotSHA256: sum, DatabaseSHA256: dbHash, SourceDBType: meta.SourceDBType, SourceCreatedAt: meta.CreatedAt, TargetDir: target, CreatedAt: time.Now().UTC().Format(time.RFC3339), DryRun: cfg.DryRun, Verified: true, Tables: tables, Files: files, Roots: cfg.Roots, Warnings: []string{"Legacy backup export may silently contain empty tables after source export errors. A successful import cannot detect data already absent from the snapshot.", "YAML, environment variables, Redis, unreferenced files, and remote image URLs are not included in legacy database backups. Retain these separately.", "Legacy job and authentication records are preserved as data. Service-level recovery, signing keys, domain/origin and external credentials must be reviewed before cutover."}, Rollback: "Stop AniDan before any rollback. Restart the unchanged legacy service against its original database, configuration and files. Never run both writers concurrently. No source data was changed."}
	// copyFiles has now verified the actual copied bytes. This comparison must
	// precede receipt publication and the final directory rename.
	if e = verifyExpectedProof(receipt, cfg.ExpectedProofSHA256); e != nil {
		return receipt, e
	}
	if cfg.DryRun {
		return receipt, nil
	}
	if e = writeJSON(filepath.Join(stage, "migration-receipt.json"), receipt); e != nil {
		return receipt, e
	}
	// Recheck the target immediately before publication; rename fails for an existing
	// nonempty target. Exclusive target lock also guards cooperating migrations.
	lock, e := os.OpenFile(target+".publish-lock", os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if e != nil {
		return receipt, fmt.Errorf("publication lock: %w", e)
	}
	lock.Close()
	defer os.Remove(target + ".publish-lock")
	if _, err := os.Lstat(target); err == nil {
		return receipt, errors.New("target appeared before publication; refusing overwrite")
	} else if !os.IsNotExist(err) {
		return receipt, err
	}
	if e = syncDir(stage); e != nil {
		return receipt, e
	}
	if e = ctx.Err(); e != nil {
		return receipt, e
	}
	if e = publishDirectory(stage, target); e != nil {
		return receipt, e
	}
	if e = syncDir(filepath.Dir(target)); e != nil {
		return receipt, e
	}
	return receipt, nil
}

// ReceiptProofSHA256 excludes volatile publication time, dry-run state and the
// physical SQLite database hash, which can differ across equivalent imports.
// RuntimeConfig is included through its config.json entry in Files. Callers
// must retain the proof in private operation state, not trust a client receipt.
func ReceiptProofSHA256(receipt Receipt) string {
	proof := struct {
		Version, LegacyCommit, SchemaSHA256, SnapshotSHA256 string
		SourceDBType, SourceCreatedAt, TargetDir            string
		Tables                                              map[string]TableReceipt
		Files                                               []FileReceipt
		Roots                                               []RootMapping
	}{receipt.Version, receipt.LegacyCommit, receipt.SchemaSHA256, receipt.SnapshotSHA256,
		receipt.SourceDBType, receipt.SourceCreatedAt, receipt.TargetDir, receipt.Tables,
		append([]FileReceipt(nil), receipt.Files...), receipt.Roots}
	for i := range proof.Files {
		proof.Files[i].OriginalPaths = append([]string(nil), proof.Files[i].OriginalPaths...)
		sort.Strings(proof.Files[i].OriginalPaths)
	}
	sort.Slice(proof.Files, func(i, j int) bool { return proof.Files[i].Destination < proof.Files[j].Destination })
	encoded, _ := json.Marshal(proof) // These concrete receipt fields cannot fail JSON encoding.
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}

func verifyExpectedProof(receipt Receipt, expected string) error {
	if expected != "" && expected != ReceiptProofSHA256(receipt) {
		return errors.New("migration contents changed since the reviewed preview; preview again before preparing")
	}
	return nil
}
func importSnapshot(ctx context.Context, s *store.Store, p string, max, maxRow int64, mapper *fileMapper) (Metadata, map[string]int64, error) {
	var meta Metadata
	counts := map[string]int64{}
	f, e := openRegularPath(ctx, p)
	if e != nil {
		return meta, counts, e
	}
	defer f.Close()
	b := bufio.NewReader(contextReader{ctx, f})
	peek, _ := b.Peek(2)
	var r io.Reader = b
	if len(peek) == 2 && peek[0] == 0x1f && peek[1] == 0x8b {
		z, err := gzip.NewReader(b)
		if err != nil {
			return meta, counts, err
		}
		defer z.Close()
		r = z
	}
	lr := &io.LimitedReader{R: r, N: max + 1}
	dec := json.NewDecoder(&rowBoundReader{reader: &strictUTF8Reader{reader: lr}, max: maxRow})
	dec.UseNumber()
	tx, e := s.DB.BeginTx(ctx, nil)
	if e != nil {
		return meta, counts, e
	}
	defer tx.Rollback()
	if _, e = tx.ExecContext(ctx, "PRAGMA defer_foreign_keys=ON"); e != nil {
		return meta, counts, e
	}
	if e = expectToken(dec, json.Delim('{')); e != nil {
		return meta, counts, e
	}
	seen := map[string]bool{}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return meta, counts, err
		}
		key, ok := tok.(string)
		if !ok || seen[key] {
			return meta, counts, errors.New("duplicate or invalid snapshot envelope key")
		}
		seen[key] = true
		switch key {
		case "metadata":
			raw, err := readObject(dec, maxRow)
			if err != nil {
				return meta, counts, err
			}
			buf, err := json.Marshal(raw)
			if err != nil {
				return meta, counts, err
			}
			md := json.NewDecoder(strings.NewReader(string(buf)))
			md.DisallowUnknownFields()
			if err = md.Decode(&meta); err != nil {
				return meta, counts, fmt.Errorf("metadata: %w", err)
			}
		case "data":
			if err = expectToken(dec, json.Delim('{')); err != nil {
				return meta, counts, err
			}
			for dec.More() {
				token, err := dec.Token()
				if err != nil {
					return meta, counts, err
				}
				table, ok := token.(string)
				if !ok {
					return meta, counts, errors.New("invalid table key")
				}
				if _, ok = store.Schema[table]; !ok {
					return meta, counts, fmt.Errorf("unknown table %s (schema drift)", table)
				}
				if _, ok = counts[table]; ok {
					return meta, counts, fmt.Errorf("duplicate table %s", table)
				}
				counts[table] = 0
				if err = expectToken(dec, json.Delim('[')); err != nil {
					return meta, counts, err
				}
				for dec.More() {
					if err = ctx.Err(); err != nil {
						return meta, counts, err
					}
					row, err := readObject(dec, maxRow)
					if err != nil {
						return meta, counts, fmt.Errorf("%s row %d: %w", table, counts[table]+1, err)
					}
					norm, err := store.ValidateRow(table, store.Row(row), true)
					if err != nil {
						return meta, counts, fmt.Errorf("%s row %d: %w", table, counts[table]+1, err)
					}
					if err = mapper.mapRow(ctx, table, norm); err != nil {
						return meta, counts, fmt.Errorf("%s row %d files: %w", table, counts[table]+1, err)
					}
					if _, err = s.InsertExactTx(ctx, tx, table, norm); err != nil {
						return meta, counts, fmt.Errorf("%s row %d insert failed: %w", table, counts[table]+1, err)
					}
					if err = verifyInserted(ctx, s, tx, table, norm); err != nil {
						return meta, counts, err
					}
					counts[table]++
				}
				if err = expectToken(dec, json.Delim(']')); err != nil {
					return meta, counts, err
				}
			}
			if err = expectToken(dec, json.Delim('}')); err != nil {
				return meta, counts, err
			}
		default:
			return meta, counts, fmt.Errorf("unknown snapshot envelope field %q", key)
		}
	}
	if e = expectToken(dec, json.Delim('}')); e != nil {
		return meta, counts, e
	}
	var extra any
	if e = dec.Decode(&extra); e != io.EOF {
		return meta, counts, errors.New("trailing data or malformed compressed snapshot")
	}
	if lr.N <= 0 {
		return meta, counts, fmt.Errorf("snapshot exceeds decompressed limit %d", max)
	}
	if !seen["metadata"] || !seen["data"] {
		return meta, counts, errors.New("snapshot requires metadata and data")
	}
	if meta.Version != "2.0" {
		return meta, counts, fmt.Errorf("unsupported legacy snapshot version %q; expected 2.0", meta.Version)
	}
	if len(counts) != len(store.Schema) || len(meta.Tables) != len(store.Schema) || len(meta.TableRecords) != len(store.Schema) {
		return meta, counts, fmt.Errorf("incomplete/drifted snapshot: all %d legacy tables and count metadata are required", len(store.Schema))
	}
	mt := map[string]bool{}
	for _, n := range meta.Tables {
		if mt[n] {
			return meta, counts, fmt.Errorf("duplicate metadata table %s", n)
		}
		mt[n] = true
	}
	var total int64
	for _, n := range store.Tables() {
		cnt, ok := counts[n]
		if !ok || !mt[n] {
			return meta, counts, fmt.Errorf("missing table %s", n)
		}
		decl, ok := meta.TableRecords[n]
		if !ok || decl != cnt {
			return meta, counts, fmt.Errorf("table count mismatch %s: declared %d actual %d", n, decl, cnt)
		}
		total += cnt
	}
	if total != meta.TotalRecords {
		return meta, counts, fmt.Errorf("total record count mismatch: declared %d actual %d", meta.TotalRecords, total)
	}
	if e = tx.Commit(); e != nil {
		return meta, counts, fmt.Errorf("relation/transaction verification: %w", e)
	}
	return meta, counts, nil
}
func readObject(d *json.Decoder, max int64) (map[string]any, error) {
	v, e := readJSONValue(d, 0, d.InputOffset()+max)
	if e != nil {
		return nil, e
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, errors.New("expected JSON object")
	}
	return m, nil
}
func readJSONValue(d *json.Decoder, depth int, end int64) (any, error) {
	if d.InputOffset() > end {
		return nil, errors.New("JSON object exceeds configured row byte limit")
	}
	if depth > 64 {
		return nil, errors.New("JSON nesting exceeds limit")
	}
	tok, e := d.Token()
	if e != nil {
		return nil, e
	}
	if d.InputOffset() > end {
		return nil, errors.New("JSON object exceeds configured row byte limit")
	}
	delim, ok := tok.(json.Delim)
	if !ok {
		return tok, nil
	}
	switch delim {
	case '{':
		m := map[string]any{}
		for d.More() {
			k, e := d.Token()
			if e != nil {
				return nil, e
			}
			key, ok := k.(string)
			if !ok {
				return nil, errors.New("expected JSON field name")
			}
			if _, ok = m[key]; ok {
				return nil, fmt.Errorf("duplicate JSON field %s", key)
			}
			v, e := readJSONValue(d, depth+1, end)
			if e != nil {
				return nil, e
			}
			m[key] = v
		}
		if e = expectToken(d, json.Delim('}')); e != nil {
			return nil, e
		}
		return m, nil
	case '[':
		a := []any{}
		for d.More() {
			v, e := readJSONValue(d, depth+1, end)
			if e != nil {
				return nil, e
			}
			a = append(a, v)
		}
		if e = expectToken(d, json.Delim(']')); e != nil {
			return nil, e
		}
		return a, nil
	default:
		return nil, errors.New("unexpected closing JSON delimiter")
	}
}
func expectToken(d *json.Decoder, want json.Token) error {
	v, e := d.Token()
	if e != nil {
		return e
	}
	if v != want {
		return fmt.Errorf("expected %v, got %v", want, v)
	}
	return nil
}
func verifyInserted(ctx context.Context, s *store.Store, tx *sql.Tx, name string, want store.Row) error {
	t := store.Schema[name]
	parts := []string{}
	args := []any{}
	for _, k := range t.PrimaryKey {
		parts = append(parts, s.Quote(k)+"=?")
		args = append(args, want[k])
	}
	rows, e := tx.QueryContext(ctx, "SELECT * FROM "+s.Quote(name)+" WHERE "+strings.Join(parts, " AND "), args...)
	if e != nil {
		return e
	}
	defer rows.Close()
	if !rows.Next() {
		return fmt.Errorf("inserted %s row cannot be reread", name)
	}
	got, e := store.ScanRow(rows, t)
	if e != nil {
		return e
	}
	if !reflect.DeepEqual(got, want) {
		return fmt.Errorf("round-trip verification failed for %s; no source data changed", name)
	}
	return rows.Err()
}
func validateRelations(ctx context.Context, s *store.Store) error {
	var result string
	if e := s.DB.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&result); e != nil {
		return e
	}
	if result != "ok" {
		return fmt.Errorf("SQLite integrity check: %s", result)
	}
	checks := [][3]string{{"external_calendar_item", "local_anime_id", "anime"}, {"external_calendar_item", "local_source_id", "anime_sources"}, {"bangumi_auth", "user_id", "users"}, {"oauth_states", "user_id", "users"}, {"oauth_credentials", "user_id", "users"}}
	for _, c := range checks {
		var n int64
		q := "SELECT COUNT(*) FROM " + s.Quote(c[0]) + " x LEFT JOIN " + s.Quote(c[2]) + " p ON x." + s.Quote(c[1]) + "=p.id WHERE x." + s.Quote(c[1]) + " IS NOT NULL AND p.id IS NULL"
		if e := s.DB.QueryRowContext(ctx, q).Scan(&n); e != nil {
			return e
		}
		if n > 0 {
			return fmt.Errorf("dangling application references: %s.%s has %d missing %s records", c[0], c[1], n, c[2])
		}
	}
	var n int64
	e := s.DB.QueryRowContext(ctx, "SELECT COUNT(*) FROM external_calendar_item c JOIN anime_sources s ON c.local_source_id=s.id WHERE c.local_anime_id IS NOT NULL AND s.anime_id<>c.local_anime_id").Scan(&n)
	if e != nil {
		return e
	}
	if n > 0 {
		return errors.New("calendar local anime/source references disagree")
	}
	return nil
}
func checksumTables(ctx context.Context, s *store.Store) (map[string]TableReceipt, error) {
	out := map[string]TableReceipt{}
	for _, name := range store.Tables() {
		t := store.Schema[name]
		keys := []string{}
		for _, k := range t.PrimaryKey {
			keys = append(keys, s.Quote(k))
		}
		rows, e := s.DB.QueryContext(ctx, "SELECT * FROM "+s.Quote(name)+" ORDER BY "+strings.Join(keys, ","))
		if e != nil {
			return nil, e
		}
		h := sha256.New()
		var count int64
		for rows.Next() {
			r, e := store.ScanRow(rows, t)
			if e != nil {
				rows.Close()
				return nil, e
			}
			buf, e := json.Marshal(r)
			if e != nil {
				rows.Close()
				return nil, e
			}
			h.Write(buf)
			h.Write([]byte{'\n'})
			count++
		}
		e = rows.Err()
		rows.Close()
		if e != nil {
			return nil, e
		}
		out[name] = TableReceipt{count, hex.EncodeToString(h.Sum(nil))}
	}
	return out, nil
}
func verifyExisting(ctx context.Context, target string, j journal) (Receipt, error) {
	var r Receipt
	if e := rejectSymlink(target); e != nil {
		return r, e
	}
	root, e := openDirectoryRoot(target)
	if e != nil {
		return r, e
	}
	defer root.Close()
	var old journal
	if e := readJSONAt(ctx, root, ".anidan-migration.json", &old); e != nil {
		return r, fmt.Errorf("existing target is not a recognized migration: %w", e)
	}
	if old != j {
		return r, errors.New("existing target belongs to a different migration")
	}
	if e := readJSONAt(ctx, root, "migration-receipt.json", &r); e != nil {
		return r, e
	}
	if !r.Verified || r.DryRun {
		return r, errors.New("target receipt does not record a completed import")
	}
	if st, err := root.Lstat("anidan.db-wal"); err == nil {
		if !st.Mode().IsRegular() {
			return r, errors.New("target SQLite WAL is not a regular file")
		}
		if st.Size() > 0 {
			return r, errors.New("target has a nonempty SQLite WAL; stop runtime and checkpoint before verification")
		}
	} else if !os.IsNotExist(err) {
		return r, err
	}
	dbFile, e := openRegularDestination(ctx, root, "anidan.db")
	if e != nil {
		return r, e
	}
	sum, _, e := hashOpenFile(ctx, dbFile)
	dbFile.Close()
	if e != nil {
		return r, e
	}
	if sum != r.DatabaseSHA256 {
		return r, errors.New("target database has changed since migration; refusing to resume or overwrite")
	}
	for _, f := range r.Files {
		relative := filepath.FromSlash(f.Destination)
		if !filepath.IsLocal(relative) {
			return r, errors.New("invalid receipt destination")
		}
		file, e := openRegularDestination(ctx, root, relative)
		if e != nil {
			return r, e
		}
		sum, size, e := hashOpenFile(ctx, file)
		file.Close()
		if e != nil {
			return r, e
		}
		if sum != f.SHA256 || size != f.Size {
			return r, fmt.Errorf("target file changed: %s", f.Destination)
		}
	}
	return r, nil
}
func ensureTargetSeparate(target, snapshot string, roots []RootMapping) error {
	parent := filepath.Dir(target)
	resolvedParent, e := filepath.EvalSymlinks(parent)
	if e != nil {
		return e
	}
	if filepath.Clean(resolvedParent) != filepath.Clean(parent) {
		return errors.New("target parent contains a symbolic link; use its canonical path")
	}
	if filepath.Clean(target) == filepath.Clean(snapshot) {
		return errors.New("target equals snapshot")
	}
	for _, r := range roots {
		root, e := filepath.Abs(r.To)
		if e != nil {
			return e
		}
		resolved, e := filepath.EvalSymlinks(root)
		if e != nil {
			return e
		}
		if within(resolved, target) || within(target, resolved) {
			return errors.New("target must be separate from every source file root")
		}
	}
	return rejectSymlinkIfExists(filepath.Dir(target))
}
func readJSON(p string, v any) error {
	return readJSONContext(context.Background(), p, v)
}
func readJSONContext(ctx context.Context, p string, v any) error {
	if e := ctx.Err(); e != nil {
		return e
	}
	root, e := openDirectoryRoot(filepath.Dir(p))
	if e != nil {
		return e
	}
	defer root.Close()
	return readJSONAt(ctx, root, filepath.Base(p), v)
}
func readJSONAt(ctx context.Context, root *os.Root, p string, v any) error {
	f, e := openRegularDestination(ctx, root, p)
	if e != nil {
		return e
	}
	defer f.Close()
	before, e := f.Stat()
	if e != nil {
		return e
	}
	// Bound this read to the opened file's size, without imposing a new receipt
	// size cap on installations with explicitly expanded file manifests.
	d := json.NewDecoder(contextReader{ctx, io.LimitReader(f, before.Size())})
	d.DisallowUnknownFields()
	if e = d.Decode(v); e != nil {
		return e
	}
	var extra json.RawMessage
	if e = d.Decode(&extra); e != io.EOF {
		if e != nil {
			return e
		}
		return errors.New("trailing JSON in migration metadata")
	}
	after, e := f.Stat()
	if e != nil {
		return e
	}
	if before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
		return fmt.Errorf("migration metadata changed while reading: %s", p)
	}
	return ctx.Err()
}
func writeJSON(p string, v any) error {
	b, e := json.MarshalIndent(v, "", "  ")
	if e != nil {
		return e
	}
	f, e := os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if e != nil {
		return e
	}
	_, we := f.Write(b)
	se := f.Sync()
	ce := f.Close()
	if we != nil {
		return we
	}
	if se != nil {
		return se
	}
	return ce
}
func syncDir(p string) error {
	root, e := openDirectoryRoot(p)
	if e != nil {
		return e
	}
	defer root.Close()
	f, e := root.Open(".")
	if e != nil {
		return e
	}
	defer f.Close()
	return f.Sync()
}
func sortedKeys[V any](m map[string]V) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}
