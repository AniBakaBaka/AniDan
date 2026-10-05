// SPDX-License-Identifier: AGPL-3.0-or-later
package server

import (
	"archive/tar"
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
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/AniBakaBaka/AniDan/internal/config"
	"github.com/AniBakaBaka/AniDan/internal/job"
	"github.com/AniBakaBaka/AniDan/internal/migrate"
	"github.com/AniBakaBaka/AniDan/internal/store"
)

const maxBackupBytes int64 = 8 << 30
const maxBackupFiles = 100000
const maxManifestBytes int64 = 32 << 20

const restoreCacheIsolationWarning = "Restore cache isolation: source Redis/Valkey endpoints and credentials are omitted; remote cache modes use target hybrid caching (memory plus target SQLite), and the target has an independent cache namespace. The source cache was not contacted or cleared."

type bundleRoot struct {
	From string `json:"from"`
	Path string `json:"path"`
}
type bundleFile struct {
	Path       string `json:"path"`
	Size       int64  `json:"size"`
	SHA256     string `json:"sha256"`
	source     string
	sourceRoot string
	sourceRel  string
}
type backupManifest struct {
	Format         string           `json:"format"`
	CreatedAt      string           `json:"created_at"`
	Metadata       migrate.Metadata `json:"metadata"`
	SchemaSHA256   string           `json:"schema_sha256"`
	SnapshotSHA256 string           `json:"snapshot_sha256"`
	ConfigSHA256   string           `json:"config_sha256"`
	Roots          []bundleRoot     `json:"roots"`
	Files          []bundleFile     `json:"files"`
	Warnings       []string         `json:"warnings"`
}
type backupInfo struct {
	Filename        string           `json:"filename"`
	Size            int64            `json:"size"`
	CreatedAt       string           `json:"created_at"`
	DBType          string           `json:"db_type"`
	SHA256          string           `json:"sha256"`
	TotalRecords    int64            `json:"total_records"`
	Version         string           `json:"version"`
	ContainsFiles   bool             `json:"contains_files"`
	ContainsSecrets bool             `json:"contains_secrets"`
	FileCount       int              `json:"file_count"`
	TableRecords    map[string]int64 `json:"table_records,omitempty"`
}
type preparedBackup struct {
	snapshot string
	roots    []migrate.RootMapping
	manifest *backupManifest
	receipt  migrate.Receipt
	config   *config.Config
	cleanup  func()
}

func backupCopy(ctx context.Context, w io.Writer, r io.Reader, max int64) (int64, error) {
	buf := make([]byte, 128<<10)
	var n int64
	for {
		if err := ctx.Err(); err != nil {
			return n, err
		}
		count, re := r.Read(buf)
		if count > 0 {
			if n+int64(count) > max {
				return n, errors.New("backup byte limit exceeded")
			}
			nw, we := w.Write(buf[:count])
			n += int64(nw)
			if we != nil {
				return n, we
			}
			if nw != count {
				return n, io.ErrShortWrite
			}
		}
		if re == io.EOF {
			return n, nil
		}
		if re != nil {
			return n, re
		}
	}
}
func backupHash(ctx context.Context, p string) (string, int64, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() {
		return "", 0, errors.New("backup source is not a regular file")
	}
	h := sha256.New()
	n, err := backupCopy(ctx, h, f, maxBackupBytes)
	if err != nil {
		return "", 0, err
	}
	after, err := f.Stat()
	if err != nil {
		return "", 0, err
	}
	if st.Size() != n || after.Size() != n || !st.ModTime().Equal(after.ModTime()) {
		return "", 0, errors.New("file changed during backup")
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}
func backupJSONFile(p string, v any) error {
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	if err = json.NewEncoder(f).Encode(v); err != nil {
		return err
	}
	return f.Sync()
}
func backupWithin(root, p string) bool {
	rel, err := filepath.Rel(root, p)
	return err == nil && filepath.IsLocal(rel)
}
func normalizedBackupPath(p string) string {
	return strings.TrimSuffix(path.Clean(strings.ReplaceAll(p, "\\", "/")), "/")
}
func (s *Server) backupRoots() ([]migrate.RootMapping, error) {
	data, err := filepath.Abs(s.DataDir)
	if err != nil {
		return nil, err
	}
	roots := []migrate.RootMapping{}
	seen := map[string]string{}
	add := func(from, to string) error {
		from = normalizedBackupPath(from)
		if from == "" || from == "." || from == "/" {
			return errors.New("backup requires an explicit data root")
		}
		to, err = filepath.EvalSymlinks(to)
		if err != nil {
			return err
		}
		if old, ok := seen[from]; ok {
			if old != to {
				return errors.New("conflicting backup roots")
			}
			return nil
		}
		seen[from] = to
		roots = append(roots, migrate.RootMapping{From: from, To: to})
		return nil
	}
	if err = add("/app/config", data); err != nil {
		return nil, err
	}
	if err = add(data, data); err != nil {
		return nil, err
	}
	if filepath.Clean(s.DataDir) != "." {
		if err = add(s.DataDir, data); err != nil {
			return nil, err
		}
	}
	if len(s.Config.ReadRoots) > 60 {
		return nil, errors.New("too many configured file roots")
	}
	for _, r := range s.Config.ReadRoots {
		abs, e := filepath.Abs(r)
		if e != nil {
			return nil, e
		}
		if e = add(abs, abs); e != nil {
			return nil, e
		}
		if filepath.Clean(r) != "." {
			if e = add(r, abs); e != nil {
				return nil, e
			}
		}
	}
	return roots, nil
}
func makeBundleManifest(receipt migrate.Receipt, roots []migrate.RootMapping) (backupManifest, error) {
	m := backupManifest{Format: "anidan-library-v1", CreatedAt: time.Now().UTC().Format(time.RFC3339), SchemaSHA256: store.SchemaFingerprint(), SnapshotSHA256: receipt.SnapshotSHA256, Roots: []bundleRoot{}, Files: []bundleFile{}, Warnings: []string{"Contains database credentials/tokens and runtime signing keys; keep this archive private.", "Includes every referenced local library file; unreferenced files, remote URLs, external Redis and arbitrary host files are not included.", "External writers must remain quiescent; file hashes are verified before publication."}}
	counts := map[string]int64{}
	var total int64
	for name, t := range receipt.Tables {
		counts[name] = t.Rows
		total += t.Rows
	}
	m.Metadata = migrate.Metadata{Version: "2.0", SourceDBType: receipt.SourceDBType, CreatedAt: receipt.SourceCreatedAt, Tables: store.Tables(), TableRecords: counts, TotalRecords: total}
	actualRoots := map[string]string{}
	for _, r := range roots {
		archiveRoot, ok := actualRoots[r.To]
		if !ok {
			archiveRoot = fmt.Sprintf("roots/%d", len(actualRoots))
			actualRoots[r.To] = archiveRoot
		}
		m.Roots = append(m.Roots, bundleRoot{From: normalizedBackupPath(r.From), Path: archiveRoot})
	}
	files := map[string]bundleFile{}
	for _, f := range receipt.Files {
		for _, original := range f.OriginalPaths {
			legacy := normalizedBackupPath(original)
			if strings.HasPrefix(legacy, "/data/images/") {
				legacy = "/app/config/image/" + strings.TrimPrefix(legacy, "/data/images/")
			}
			var chosen *bundleRoot
			for i := range m.Roots {
				root := &m.Roots[i]
				if (legacy == root.From || strings.HasPrefix(legacy, root.From+"/")) && (chosen == nil || len(root.From) > len(chosen.From)) {
					chosen = root
				}
			}
			if chosen == nil {
				return m, fmt.Errorf("no archive root for %s", original)
			}
			rel := strings.TrimPrefix(legacy[len(chosen.From):], "/")
			archivePath := path.Join(chosen.Path, rel)
			if rel == "" || !filepath.IsLocal(filepath.FromSlash(archivePath)) {
				return m, errors.New("invalid archive file path")
			}
			item := bundleFile{Path: archivePath, Size: f.Size, SHA256: f.SHA256, source: f.Source}
			for _, root := range roots {
				if backupWithin(root.To, f.Source) && len(root.To) > len(item.sourceRoot) {
					item.sourceRoot = root.To
					item.sourceRel, _ = filepath.Rel(root.To, f.Source)
				}
			}
			if item.sourceRoot == "" {
				return m, errors.New("source escaped all allowed file roots")
			}
			if old, ok := files[archivePath]; ok && (old.SHA256 != item.SHA256 || old.Size != item.Size) {
				return m, errors.New("archive file mapping conflict")
			}
			files[archivePath] = item
		}
	}
	if len(files) > maxBackupFiles {
		return m, errors.New("backup file count exceeds safety limit")
	}
	for _, f := range files {
		m.Files = append(m.Files, f)
	}
	sort.Slice(m.Files, func(i, j int) bool { return m.Files[i].Path < m.Files[j].Path })
	return m, nil
}
func writeBundle(ctx context.Context, destination, snapshot, runtimeConfig string, manifest backupManifest) error {
	f, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	complete := false
	defer func() {
		f.Close()
		if !complete {
			os.Remove(destination)
		}
	}()
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	data, err := json.Marshal(manifest)
	if err != nil {
		return err
	}
	if int64(len(data)) > maxManifestBytes {
		return errors.New("backup manifest too large")
	}
	if err = tw.WriteHeader(&tar.Header{Name: "manifest.json", Mode: 0600, Size: int64(len(data)), ModTime: time.Now().UTC(), Typeflag: tar.TypeReg}); err != nil {
		return err
	}
	if _, err = tw.Write(data); err != nil {
		return err
	}
	sources := []bundleFile{{Path: "database.json", SHA256: manifest.SnapshotSHA256, source: snapshot, sourceRoot: filepath.Dir(snapshot), sourceRel: filepath.Base(snapshot)}, {Path: "runtime-config.json", SHA256: manifest.ConfigSHA256, source: runtimeConfig, sourceRoot: filepath.Dir(runtimeConfig), sourceRel: filepath.Base(runtimeConfig)}}
	sources = append(sources, manifest.Files...)
	var total int64
	for _, item := range sources {
		source, err := openWithin(item.sourceRoot, item.sourceRel)
		if err != nil {
			return err
		}
		st, err := source.Stat()
		if err != nil || !st.Mode().IsRegular() {
			source.Close()
			return errors.New("backup source changed or is not regular")
		}
		if st.Size() > maxBackupBytes-total {
			source.Close()
			return errors.New("backup exceeds 8 GiB decompressed limit")
		}
		if item.Size > 0 && st.Size() != item.Size {
			source.Close()
			return errors.New("backup source size changed")
		}
		if err = tw.WriteHeader(&tar.Header{Name: item.Path, Mode: 0600, Size: st.Size(), ModTime: st.ModTime(), Typeflag: tar.TypeReg}); err != nil {
			source.Close()
			return err
		}
		h := sha256.New()
		n, copyErr := backupCopy(ctx, io.MultiWriter(tw, h), source, st.Size())
		after, statErr := source.Stat()
		source.Close()
		if copyErr != nil {
			return copyErr
		}
		if statErr != nil {
			return statErr
		}
		if n != st.Size() || after.Size() != st.Size() || !after.ModTime().Equal(st.ModTime()) || hex.EncodeToString(h.Sum(nil)) != item.SHA256 {
			return errors.New("source file changed during archive capture")
		}
		total += n
	}
	if err = tw.Close(); err != nil {
		return err
	}
	if err = gz.Close(); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	complete = true
	return nil
}
func extractBundle(ctx context.Context, filename, destination string) (backupManifest, error) {
	var m backupManifest
	f, err := os.Open(filename)
	if err != nil {
		return m, err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return m, err
	}
	defer gz.Close()
	lr := &io.LimitedReader{R: gz, N: maxBackupBytes + maxManifestBytes + (128 << 20)}
	tr := tar.NewReader(lr)
	header, err := tr.Next()
	if err != nil {
		return m, err
	}
	if header.Name != "manifest.json" || header.Typeflag != tar.TypeReg || header.Size < 1 || header.Size > maxManifestBytes {
		return m, errors.New("full backup must start with a bounded manifest.json")
	}
	decoder := json.NewDecoder(io.LimitReader(tr, maxManifestBytes+1))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&m); err != nil {
		return m, err
	}
	if m.Format != "anidan-library-v1" || m.SchemaSHA256 != store.SchemaFingerprint() {
		return m, errors.New("unsupported backup format or schema fingerprint")
	}
	if len(m.Files) > maxBackupFiles || len(m.Roots) > 128 {
		return m, errors.New("backup manifest exceeds safety limits")
	}
	expected := map[string]bundleFile{"database.json": {SHA256: m.SnapshotSHA256, Size: -1}, "runtime-config.json": {SHA256: m.ConfigSHA256, Size: -1}}
	var total int64
	for _, item := range m.Files {
		if !(strings.HasPrefix(item.Path, "roots/") || (strings.HasPrefix(item.Path, "payloads/") && importPayloadNameRE.MatchString(strings.TrimPrefix(item.Path, "payloads/")))) || !filepath.IsLocal(filepath.FromSlash(item.Path)) || strings.Contains(item.Path, "\\") || item.Size < 0 || item.Size > maxBackupBytes-total {
			return m, errors.New("invalid archive manifest file")
		}
		if _, ok := expected[item.Path]; ok {
			return m, errors.New("duplicate archive file")
		}
		total += item.Size
		expected[item.Path] = item
	}
	root, err := os.OpenRoot(destination)
	if err != nil {
		return m, err
	}
	defer root.Close()
	seen := map[string]bool{}
	for {
		header, err = tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return m, err
		}
		item, ok := expected[header.Name]
		if !ok || seen[header.Name] || header.Typeflag != tar.TypeReg || header.Linkname != "" || header.Size < 0 || header.Size > maxBackupBytes || !filepath.IsLocal(filepath.FromSlash(header.Name)) {
			return m, errors.New("unexpected, duplicate or unsafe archive entry")
		}
		if item.Size >= 0 && header.Size != item.Size {
			return m, errors.New("archive size differs from manifest")
		}
		if header.Name == "runtime-config.json" && header.Size > 1<<20 {
			return m, errors.New("runtime configuration is too large")
		}
		name := filepath.FromSlash(header.Name)
		if err = root.MkdirAll(filepath.Dir(name), 0700); err != nil {
			return m, err
		}
		out, e := root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if e != nil {
			return m, e
		}
		h := sha256.New()
		n, e := backupCopy(ctx, io.MultiWriter(out, h), tr, header.Size)
		if e == nil {
			e = out.Sync()
		}
		ce := out.Close()
		if e != nil {
			return m, e
		}
		if ce != nil {
			return m, ce
		}
		if n != header.Size || hex.EncodeToString(h.Sum(nil)) != item.SHA256 {
			return m, errors.New("archive file checksum mismatch")
		}
		seen[header.Name] = true
	}
	if lr.N <= 0 {
		return m, errors.New("archive decompression limit exceeded")
	}
	trailing, err := backupCopy(ctx, io.Discard, lr, 1<<20)
	if err != nil {
		return m, err
	}
	if trailing != 0 {
		return m, errors.New("unexpected data after tar archive")
	}
	if len(seen) != len(expected) {
		return m, errors.New("archive is missing required files")
	}
	for _, r := range m.Roots {
		if r.From == "" || r.From == "/" || !strings.HasPrefix(r.Path, "roots/") || !filepath.IsLocal(filepath.FromSlash(r.Path)) || strings.Contains(r.Path, "\\") {
			return m, errors.New("unsafe archive root mapping")
		}
		if err = root.MkdirAll(filepath.FromSlash(r.Path), 0700); err != nil {
			return m, err
		}
	}
	return m, nil
}
func (s *Server) prepareBackup(ctx context.Context, archive string, dryRun bool, target string) (preparedBackup, error) {
	var out preparedBackup
	temp, err := os.MkdirTemp("", "anidan-restore-")
	if err != nil {
		return out, err
	}
	out.cleanup = func() { os.RemoveAll(temp) }
	cfg := s.Config
	ok := false
	defer func() {
		if !ok {
			out.cleanup()
		}
	}()
	if strings.HasSuffix(archive, ".tar.gz") {
		manifest, err := extractBundle(ctx, archive, temp)
		if err != nil {
			return out, err
		}
		out.manifest = &manifest
		out.snapshot = filepath.Join(temp, "database.json")
		for _, r := range manifest.Roots {
			out.roots = append(out.roots, migrate.RootMapping{From: r.From, To: filepath.Join(temp, filepath.FromSlash(r.Path))})
		}
		raw, err := os.ReadFile(filepath.Join(temp, "runtime-config.json"))
		if err != nil {
			return out, err
		}
		// Native bundles predating optional caches have no cache object. Overlay
		// defaults rather than decoding an unusable zero-valued cache.
		cfg = config.Defaults()
		if err = json.Unmarshal(raw, &cfg); err != nil {
			return out, err
		}
	} else {
		out.snapshot = archive
		out.roots, err = s.backupRoots()
		if err != nil {
			return out, err
		}
	}
	if target == "" {
		target = filepath.Join(temp, "preflight")
	}
	target, err = filepath.Abs(target)
	if err != nil {
		return out, err
	}
	cfg.Driver = "sqlite"
	cfg.DataDir = target
	cfg.DSN = filepath.Join(target, "anidan.db")
	cfg.ReadRoots = nil
	cfg.AdminPassword = ""
	cfg.WebDir = s.Config.WebDir
	cfg.StaticDir = s.Config.StaticDir
	if cfg.Cache, err = cfg.Cache.ForIsolatedTarget(target); err != nil {
		return out, err
	}
	// Verification budgets are an operator-local policy. Archived configuration
	// may preserve application settings, but cannot silently raise these limits.
	cfg.MigrationReview = s.Config.MigrationReview
	if err = cfg.Validate(); err != nil {
		return out, fmt.Errorf("restored configuration requires review: %w", err)
	}
	out.config = &cfg
	// SourceQuiesced applies to this extracted immutable snapshot; it does not assert
	// that the live destination can be overwritten. Run never overwrites live data.
	receipt, err := migrate.Run(ctx, migrate.Config{Snapshot: out.snapshot, TargetDir: target, Roots: out.roots, DryRun: dryRun, SourceQuiesced: true, MaxBytes: maxBackupBytes, MaxRowBytes: 32 << 20})
	if err != nil {
		return out, err
	}
	receipt.Warnings = append(receipt.Warnings, restoreCacheIsolationWarning)
	if err = restoreImportPayloads(ctx, temp, target, dryRun, out.manifest, &receipt); err != nil {
		return out, err
	}
	if !dryRun {
		if err = applyRestoreReviewGate(ctx, target, &receipt); err != nil {
			return out, err
		}
	}
	if err = publishRestoreConfig(ctx, target, cfg, dryRun, &receipt); err != nil {
		return out, err
	}
	out.receipt = receipt
	ok = true
	return out, nil
}

// Publish only after library/payload verification and the recovery review gate.
// migrate.Run's RuntimeConfig path publishes too early for native supplemental
// payloads. Including the final bytes in Files still makes migration resume
// verify this configuration alongside every restored library file.
func publishRestoreConfig(ctx context.Context, target string, cfg config.Config, dryRun bool, receipt *migrate.Receipt) error {
	raw, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	sum := sha256.Sum256(raw)
	file := migrate.FileReceipt{Source: "isolated restore runtime configuration", Destination: "config.json", SHA256: hex.EncodeToString(sum[:]), Size: int64(len(raw))}
	if dryRun {
		receipt.Files = append(receipt.Files, file)
		return nil
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	root, err := os.OpenRoot(target)
	if err != nil {
		return err
	}
	defer root.Close()
	pending := ".restore-config-" + randomID() + ".json"
	f, err := root.OpenFile(pending, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer root.Remove(pending)
	_, err = f.Write(raw)
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	actual, size, err := backupHash(ctx, filepath.Join(target, pending))
	if err != nil {
		return err
	}
	if actual != file.SHA256 || size != file.Size {
		return errors.New("restored runtime configuration checksum mismatch")
	}
	if err = root.Link(pending, "config.json"); err != nil {
		return err
	}
	complete := false
	defer func() {
		if !complete {
			root.Remove("config.json")
		}
	}()
	receipt.Files = append(receipt.Files, file)
	if err = writeRestoreReceipt(target, receipt); err != nil {
		return err
	}
	complete = true
	return nil
}

var importPayloadNameRE = regexp.MustCompile(`^[0-9a-f]{64}\.txt$`)

// Include immutable spooled import objects, not request buffers. Keeping all
// objects also preserves explicit retry of historical jobs.
func (s *Server) addImportPayloads(ctx context.Context, m *backupManifest) error {
	dir := filepath.Join(s.DataDir, "import_payloads")
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".pending-") {
			continue
		}
		if !importPayloadNameRE.MatchString(entry.Name()) || !entry.Type().IsRegular() {
			return errors.New("invalid import payload object in data directory")
		}
		if len(m.Files) >= maxBackupFiles {
			return errors.New("backup file count exceeds safety limit")
		}
		f, err := openWithin(dir, entry.Name())
		if err != nil {
			return err
		}
		h := sha256.New()
		n, err := backupCopy(ctx, h, f, maxBackupBytes)
		f.Close()
		if err != nil {
			return err
		}
		sum := hex.EncodeToString(h.Sum(nil))
		if sum != strings.TrimSuffix(entry.Name(), ".txt") {
			return errors.New("import payload filename/checksum mismatch")
		}
		m.Files = append(m.Files, bundleFile{Path: "payloads/" + entry.Name(), Size: n, SHA256: sum, source: filepath.Join(dir, entry.Name()), sourceRoot: dir, sourceRel: entry.Name()})
	}
	sort.Slice(m.Files, func(i, j int) bool { return m.Files[i].Path < m.Files[j].Path })
	return nil
}
func restoreImportPayloads(ctx context.Context, temp, target string, dry bool, manifest *backupManifest, receipt *migrate.Receipt) error {
	if manifest == nil {
		return nil
	}
	var root *os.Root
	var err error
	if !dry {
		root, err = os.OpenRoot(target)
		if err != nil {
			return err
		}
		defer root.Close()
	}
	added := false
	for _, item := range manifest.Files {
		if !strings.HasPrefix(item.Path, "payloads/") {
			continue
		}
		name := strings.TrimPrefix(item.Path, "payloads/")
		if !importPayloadNameRE.MatchString(name) {
			return errors.New("invalid spooled import object path")
		}
		destination := filepath.Join("import_payloads", name)
		if !dry {
			if err = root.MkdirAll("import_payloads", 0700); err != nil {
				return err
			}
			source, err := openWithin(temp, filepath.FromSlash(item.Path))
			if err != nil {
				return err
			}
			out, err := root.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
			if err != nil {
				source.Close()
				return err
			}
			h := sha256.New()
			n, copyErr := backupCopy(ctx, io.MultiWriter(out, h), source, item.Size)
			source.Close()
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
			if n != item.Size || hex.EncodeToString(h.Sum(nil)) != item.SHA256 {
				return errors.New("restored import payload hash mismatch")
			}
		}
		receipt.Files = append(receipt.Files, migrate.FileReceipt{Source: filepath.Join(temp, filepath.FromSlash(item.Path)), OriginalPaths: []string{destination}, Destination: filepath.ToSlash(destination), SHA256: item.SHA256, Size: item.Size})
		added = true
	}
	if added && !dry {
		// Config.json is written only after this update, so no activation-ready config
		// is emitted until supplemental payloads have also passed verification.
		return writeRestoreReceipt(target, receipt)
	}
	return nil
}

// A historical snapshot's pending jobs may have executed after capture. Preserve
// their original records, but gate automatic replay on an explicit operator review.
func applyRestoreReviewGate(ctx context.Context, target string, receipt *migrate.Receipt) error {
	db, err := store.Open(ctx, "sqlite", filepath.Join(target, "anidan.db"))
	if err != nil {
		return err
	}
	closed := false
	defer func() {
		if !closed {
			db.Close()
		}
	}()
	row, err := db.Get(ctx, "config", job.RecoveryGateKey)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && row == nil) {
		_, err = db.Insert(ctx, "config", store.Row{"config_key": job.RecoveryGateKey, "config_value": "true", "description": "Historical restore: review pending tasks and schedules before recovery"})
	} else if err == nil {
		err = db.Update(ctx, "config", job.RecoveryGateKey, store.Row{"config_value": "true"})
	}
	if err != nil {
		return err
	}
	rows, err := db.DB.QueryContext(ctx, "SELECT * FROM config ORDER BY config_key")
	if err != nil {
		return err
	}
	h := sha256.New()
	var count int64
	for rows.Next() {
		record, e := store.ScanRow(rows, store.Schema["config"])
		if e != nil {
			rows.Close()
			return e
		}
		raw, e := json.Marshal(record)
		if e != nil {
			rows.Close()
			return e
		}
		h.Write(raw)
		h.Write([]byte{'\n'})
		count++
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	receipt.Tables["config"] = migrate.TableReceipt{Rows: count, SHA256: hex.EncodeToString(h.Sum(nil))}
	if _, err = db.DB.ExecContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		return err
	}
	if err = db.Close(); err != nil {
		return err
	}
	closed = true
	receipt.DatabaseSHA256, _, err = backupHash(ctx, filepath.Join(target, "anidan.db"))
	if err != nil {
		return err
	}
	receipt.Warnings = append(receipt.Warnings, "Intentional restore safety change: config."+job.RecoveryGateKey+" is set to true. Original pending job rows are preserved; automatic pending recovery and cron remain blocked until explicit operator review. Config table and database checksums above include this control row.")
	return writeRestoreReceipt(target, receipt)
}

func writeRestoreReceipt(target string, receipt *migrate.Receipt) error {
	root, err := os.OpenRoot(target)
	if err != nil {
		return err
	}
	defer root.Close()
	name := ".receipt-" + randomID() + ".json"
	f, err := root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer root.Remove(name)
	err = json.NewEncoder(f).Encode(receipt)
	if err == nil {
		err = f.Sync()
	}
	ce := f.Close()
	if err != nil {
		return err
	}
	if ce != nil {
		return ce
	}
	if err = root.Rename(name, "migration-receipt.json"); err != nil {
		return err
	}
	dir, err := root.Open(".")
	if err != nil {
		return err
	}
	err = dir.Sync()
	dir.Close()
	return err
}
