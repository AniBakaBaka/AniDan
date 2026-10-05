// SPDX-License-Identifier: AGPL-3.0-only
package migrate

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/AniBakaBaka/AniDan/internal/migrationlimits"
	"github.com/AniBakaBaka/AniDan/internal/store"
)

const startupJournalLimit = 64 << 10

type StartupVerificationLimits = migrationlimits.Limits

func DefaultStartupVerificationLimits() StartupVerificationLimits { return migrationlimits.Defaults() }

// StartupEvidence binds the exact published metadata bytes to their import.
// Reading it does not approve jobs, initialize SQL, or modify any files.
type StartupEvidence struct {
	Receipt       Receipt
	ReceiptSHA256 string
	JournalSHA256 string
	limits        StartupVerificationLimits
}

// ReadStartupEvidence returns nil only when neither migration metadata file
// exists. Any partial, malformed, unsupported or mismatched publication fails.
func ReadStartupEvidence(ctx context.Context, target string, configured ...StartupVerificationLimits) (*StartupEvidence, error) {
	limits := DefaultStartupVerificationLimits()
	if len(configured) > 1 {
		return nil, errors.New("at most one startup verification limit set is allowed")
	}
	if len(configured) == 1 {
		limits = configured[0]
	}
	if err := limits.Validate(); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(limits.TimeoutSeconds)*time.Second)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	root, err := openDirectoryRoot(target)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	_, receiptErr := root.Lstat("migration-receipt.json")
	_, journalErr := root.Lstat(".anidan-migration.json")
	if os.IsNotExist(receiptErr) && os.IsNotExist(journalErr) {
		return nil, nil
	}
	if receiptErr != nil {
		return nil, fmt.Errorf("migration receipt: %w", receiptErr)
	}
	if journalErr != nil {
		return nil, fmt.Errorf("migration journal: %w", journalErr)
	}
	absolute, err := filepath.Abs(target)
	if err != nil {
		return nil, err
	}
	resolved, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return nil, err
	}
	if resolved != absolute {
		return nil, errors.New("migrated data directory must use its canonical path without symbolic links")
	}
	e := &StartupEvidence{limits: limits}
	var j journal
	if e.ReceiptSHA256, err = readStartupJSON(ctx, root, "migration-receipt.json", limits.MaxReceiptBytes, &e.Receipt); err != nil {
		return nil, fmt.Errorf("migration receipt: %w", err)
	}
	if e.JournalSHA256, err = readStartupJSON(ctx, root, ".anidan-migration.json", startupJournalLimit, &j); err != nil {
		return nil, fmt.Errorf("migration journal: %w", err)
	}
	r := &e.Receipt
	if (r.Version != "1.0" && r.Version != "2.0") || r.LegacyCommit != LegacyCommit || !r.Verified || r.DryRun {
		return nil, errors.New("migration receipt does not identify a supported completed import")
	}
	if r.SchemaSHA256 != store.SchemaFingerprint() || j.SchemaSHA256 != r.SchemaSHA256 {
		return nil, errors.New("migration schema identity mismatch")
	}
	if r.TargetDir != absolute || j.TargetDir != absolute {
		return nil, errors.New("migration target identity mismatch")
	}
	if !startupDigest(r.SnapshotSHA256) || j.SnapshotSHA256 != r.SnapshotSHA256 || !startupDigest(r.DatabaseSHA256) {
		return nil, errors.New("migration snapshot or database identity mismatch")
	}
	if r.Version == "1.0" {
		if r.RemoteTarget != nil || r.RemoteOwnership != nil || j.TargetDriver != "" || j.TargetBindingSHA256 != "" || j.MigrationID != "" {
			return nil, errors.New("SQLite migration contains unexpected remote identity")
		}
	} else {
		if r.RemoteTarget == nil || r.RemoteOwnership == nil {
			return nil, errors.New("remote migration identity is missing")
		}
		b, o := r.RemoteTarget, r.RemoteOwnership
		if (b.Driver != "postgres" && b.Driver != "mysql") || !startupDigest(b.EndpointSHA256) || !startupDigest(b.NamespaceSHA256) || o.validate() != nil || *o != remoteOwner(j) || j.TargetDriver != b.Driver || j.TargetBindingSHA256 != b.SHA256() || logicalDatabaseHash(r.Tables) != r.DatabaseSHA256 {
			return nil, errors.New("remote migration identity or logical digest mismatch")
		}
	}
	if _, err = time.Parse(time.RFC3339, r.CreatedAt); err != nil {
		return nil, errors.New("invalid migration creation time")
	}
	switch r.SourceDBType {
	case "sqlite", "mysql", "postgres", "postgresql":
	default:
		return nil, errors.New("unsupported migration source database type")
	}
	rootBytes, err := json.Marshal(r.Roots)
	if err != nil {
		return nil, err
	}
	if startupHash(rootBytes) != j.RootsSHA256 {
		return nil, errors.New("migration root mapping identity mismatch")
	}
	if len(r.Tables) != len(store.Tables()) {
		return nil, errors.New("migration table manifest is incomplete")
	}
	for _, name := range store.Tables() {
		table, ok := r.Tables[name]
		if !ok || table.Rows < 0 || !startupDigest(table.SHA256) {
			return nil, fmt.Errorf("invalid migration table manifest: %s", name)
		}
	}
	if len(r.Files) > limits.MaxFiles {
		return nil, fmt.Errorf("migration file manifest exceeds startup limit of %d files", limits.MaxFiles)
	}
	seen := make(map[string]bool, len(r.Files))
	var total int64
	var configHash string
	for _, f := range r.Files {
		rel := filepath.FromSlash(f.Destination)
		if !filepath.IsLocal(rel) || filepath.ToSlash(filepath.Clean(rel)) != f.Destination || rel == "." || seen[rel] {
			return nil, errors.New("invalid or duplicate migration file destination")
		}
		switch rel {
		case "anidan.db", "anidan.db-wal", "anidan.db-shm", "migration-receipt.json", ".anidan-migration.json":
			return nil, errors.New("reserved migration file destination")
		}
		if !startupDigest(f.SHA256) || f.Size < 0 || f.Size > limits.MaxFileBytes || total > limits.MaxTotalFileBytes-f.Size {
			return nil, fmt.Errorf("invalid migration file hash or size; startup verification allows at most %d bytes per referenced file and %d bytes total", limits.MaxFileBytes, limits.MaxTotalFileBytes)
		}
		total += f.Size
		seen[rel] = true
		if rel == "config.json" {
			configHash = f.SHA256
		}
	}
	// Native historical restores append a receipted runtime configuration after
	// the migration journal was published. A nonempty original journal binding
	// must still match; an appended config is covered by its own receipt hash.
	if j.RuntimeConfigSHA256 != "" && j.RuntimeConfigSHA256 != configHash {
		return nil, errors.New("migration runtime configuration identity mismatch")
	}
	return e, nil
}

// VerifyStartupContents verifies only a pristine published import. Call it
// before the runtime writes SQL. Approved restarts use the bound metadata
// identity instead: their database and runtime-managed files have changed.
func VerifyStartupContents(ctx context.Context, target string, evidence *StartupEvidence) error {
	if evidence == nil {
		return errors.New("migration evidence is required")
	}
	if err := evidence.limits.Validate(); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(evidence.limits.TimeoutSeconds)*time.Second)
	defer cancel()
	root, err := openDirectoryRoot(target)
	if err != nil {
		return err
	}
	defer root.Close()
	if evidence.Receipt.Version == "1.0" {
		if _, err = root.Lstat("anidan.db-wal"); err == nil {
			wal, e := openRegularDestination(ctx, root, "anidan.db-wal")
			if e != nil {
				return e
			}
			info, e := wal.Stat()
			wal.Close()
			if e != nil {
				return e
			}
			if info.Size() != 0 {
				return errors.New("migration target has a nonempty WAL before first-boot verification")
			}
		} else if !os.IsNotExist(err) {
			return err
		}
		sum, _, err := hashStartupFile(ctx, root, "anidan.db", evidence.limits.MaxDatabaseBytes)
		if err != nil {
			return fmt.Errorf("migration database: %w", err)
		}
		if sum != evidence.Receipt.DatabaseSHA256 {
			return errors.New("migration database differs from its pristine receipt; review initialization refused")
		}
	}
	for _, file := range evidence.Receipt.Files {
		sum, size, err := hashStartupFile(ctx, root, filepath.FromSlash(file.Destination), file.Size)
		if err != nil {
			return fmt.Errorf("migration referenced file %s: %w", file.Destination, err)
		}
		if sum != file.SHA256 || size != file.Size {
			return fmt.Errorf("migration referenced file changed: %s", file.Destination)
		}
	}
	current, err := ReadStartupEvidence(ctx, target, evidence.limits)
	if err != nil {
		return err
	}
	if current == nil || current.ReceiptSHA256 != evidence.ReceiptSHA256 || current.JournalSHA256 != evidence.JournalSHA256 {
		return errors.New("migration metadata changed during first-boot verification")
	}
	return nil
}

func readStartupJSON(ctx context.Context, root *os.Root, name string, limit int64, out any) (string, error) {
	f, err := openRegularDestination(ctx, root, name)
	if err != nil {
		return "", err
	}
	defer f.Close()
	before, err := f.Stat()
	if err != nil {
		return "", err
	}
	if before.Size() > limit {
		return "", fmt.Errorf("migration metadata exceeds startup byte limit of %d bytes", limit)
	}
	data, err := io.ReadAll(contextReader{ctx, io.LimitReader(f, before.Size()+1)})
	if err != nil {
		return "", err
	}
	after, err := f.Stat()
	if err != nil {
		return "", err
	}
	if int64(len(data)) != before.Size() || after.Size() != before.Size() || !after.ModTime().Equal(before.ModTime()) {
		return "", errors.New("migration metadata changed while reading")
	}
	keys := json.NewDecoder(contextReader{ctx, bytes.NewReader(data)})
	keys.UseNumber()
	if err = checkStartupJSON(keys, 0); err != nil {
		return "", err
	}
	if _, err = keys.Token(); err != io.EOF {
		return "", errors.New("trailing migration metadata")
	}
	decoder := json.NewDecoder(&strictUTF8Reader{reader: contextReader{ctx, bytes.NewReader(data)}})
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(out); err != nil {
		return "", err
	}
	return startupHash(data), nil
}

func checkStartupJSON(d *json.Decoder, depth int) error {
	if depth > 32 {
		return errors.New("migration metadata nesting exceeds limit")
	}
	token, err := d.Token()
	if err != nil {
		return err
	}
	switch token {
	case json.Delim('{'):
		seen := map[string]bool{}
		for d.More() {
			token, err := d.Token()
			if err != nil {
				return err
			}
			key, ok := token.(string)
			if !ok || seen[key] {
				return errors.New("duplicate migration metadata field")
			}
			seen[key] = true
			if err = checkStartupJSON(d, depth+1); err != nil {
				return err
			}
		}
		return expectToken(d, json.Delim('}'))
	case json.Delim('['):
		for d.More() {
			if err = checkStartupJSON(d, depth+1); err != nil {
				return err
			}
		}
		return expectToken(d, json.Delim(']'))
	case json.Delim('}'), json.Delim(']'):
		return errors.New("unexpected migration metadata delimiter")
	}
	return nil
}

func hashStartupFile(ctx context.Context, root *os.Root, name string, limit int64) (string, int64, error) {
	f, err := openRegularDestination(ctx, root, name)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	before, err := f.Stat()
	if err != nil {
		return "", 0, err
	}
	if before.Size() > limit {
		return "", 0, fmt.Errorf("migration file exceeds startup verification size limit of %d bytes", limit)
	}
	h := sha256.New()
	n, err := copyContext(ctx, h, io.LimitReader(f, before.Size()+1))
	if err != nil {
		return "", n, err
	}
	after, err := f.Stat()
	if err != nil {
		return "", n, err
	}
	if n != before.Size() || after.Size() != before.Size() || !after.ModTime().Equal(before.ModTime()) {
		return "", n, errors.New("migration file changed while verifying")
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

func startupDigest(value string) bool {
	if len(value) != sha256.Size*2 || strings.ToLower(value) != value {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func startupHash(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
