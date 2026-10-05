// SPDX-License-Identifier: AGPL-3.0-only
package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/AniBakaBaka/AniDan/internal/config"
	"github.com/AniBakaBaka/AniDan/internal/migrate"
	"github.com/AniBakaBaka/AniDan/internal/store"
)

const migrationActivationFile = ".anidan-active-migration.json"
const migrationActivationWitnessFile = ".anidan-migration-activation-witness.json"
const migrationActivationLimit = 2 << 20

// The selector is local installation state, never imported from an archive or
// accepted as browser configuration. Its target is always derived from an ID.
type migrationActivationSelection struct {
	Version         int    `json:"version"`
	OperationID     string `json:"operationId"`
	ConfigSHA256    string `json:"configSHA256"`
	ReceiptSHA256   string `json:"receiptSHA256"`
	JournalSHA256   string `json:"journalSHA256"`
	SessionAudience string `json:"sessionAudience"`
}

type MigrationActivationStatus struct {
	Available   bool   `json:"available"`
	State       string `json:"state"`
	OperationID string `json:"operationId,omitempty"`
	Message     string `json:"message,omitempty"`
}

type MigrationActivationRequest struct{ selection migrationActivationSelection }

// MigrationActivation connects only server-generated staged SQLite imports to
// the executable's drain/close/reopen lifecycle. Embedded servers are disabled
// unless their owner explicitly installs and services this coordinator.
type MigrationActivation struct {
	mu        sync.Mutex
	bootstrap config.Config
	current   *migrationActivationSelection
	witness   *migrationActivationSelection
	pending   *MigrationActivationRequest
	failed    *migrationActivationSelection
	requests  chan *MigrationActivationRequest
	status    MigrationActivationStatus
	fault     func(string) error // deterministic tests; never configured by the API
}

// NewMigrationActivation must run before creating directories, logs, writable
// database connections, or background services. A bad existing selector is a
// startup error, never permission to silently reopen the bootstrap database.
func NewMigrationActivation(ctx context.Context, bootstrap config.Config) (*MigrationActivation, config.Config, error) {
	configured := bootstrap
	absolute, err := filepath.Abs(bootstrap.DataDir)
	if err != nil {
		return nil, bootstrap, err
	}
	bootstrap.DataDir = absolute
	m := &MigrationActivation{bootstrap: bootstrap, requests: make(chan *MigrationActivationRequest, 1), status: MigrationActivationStatus{Available: true, State: "idle"}}
	if _, err = os.Lstat(absolute); os.IsNotExist(err) {
		return m, configured, nil
	} else if err != nil {
		return nil, bootstrap, err
	}
	root, err := os.OpenRoot(absolute)
	if err != nil {
		return nil, bootstrap, err
	}
	defer root.Close()
	witness, err := activationReadWitness(ctx, root)
	if err != nil {
		return nil, bootstrap, errors.New("migration activation witness is unreadable or invalid; retained data requires selection review")
	}
	raw, err := activationReadRegular(ctx, root, migrationActivationFile, 8192)
	if os.IsNotExist(err) {
		if witness != nil {
			return nil, bootstrap, errors.New("migration activation selector is missing after activation began; retained data requires selection review")
		}
		// A configured symlink is valid for an ordinary existing installation,
		// but the narrow web activation contract requires canonical private
		// paths. Preserve the configured spelling and simply withhold activation.
		if err = activationCanonicalDirectory(absolute); err != nil {
			m.status = MigrationActivationStatus{State: "unavailable", Message: "Web migration activation requires a canonical data directory; ordinary application startup is unchanged"}
		}
		return m, configured, nil
	}
	if err != nil {
		return nil, bootstrap, errors.New("active migration selector is unreadable; refusing startup")
	}
	if witness == nil {
		return nil, bootstrap, errors.New("active migration selector has no installation witness; retained data requires selection review")
	}
	// Preserve ordinary CLI installations (including their existing symlink
	// paths) when no web selector exists. A selected import is stricter.
	if err = activationCanonicalDirectory(absolute); err != nil {
		return nil, bootstrap, err
	}
	var selected migrationActivationSelection
	if err = activationDecode(raw, &selected); err != nil || selected.Version != 1 {
		return nil, bootstrap, errors.New("active migration selector is invalid; refusing startup")
	}
	cfg, err := m.verifySelection(ctx, selected)
	if err != nil {
		return nil, bootstrap, err
	}
	m.current = &selected
	m.witness = witness
	m.status = MigrationActivationStatus{Available: true, State: "active", OperationID: selected.OperationID}
	return m, cfg, nil
}

func (m *MigrationActivation) Status() MigrationActivationStatus {
	if m == nil {
		return MigrationActivationStatus{State: "unavailable"}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.status
}

func (m *MigrationActivation) Requests() <-chan *MigrationActivationRequest { return m.requests }

func (m *MigrationActivation) BootstrapDataDir() string { return m.bootstrap.DataDir }

func (m *MigrationActivation) BootstrapConfig() config.Config {
	cfg := m.bootstrap
	cfg.ReadRoots = append([]string(nil), cfg.ReadRoots...)
	cfg.WriteRoots = append([]string(nil), cfg.WriteRoots...)
	cfg.DockerAllowedImages = append([]string(nil), cfg.DockerAllowedImages...)
	cfg.Warnings = append([]string(nil), cfg.Warnings...)
	return cfg
}

// CurrentConfiguration rechecks the persisted old selection before a rollback.
// An externally changed selector never authorizes reopening an older database.
func (m *MigrationActivation) CurrentConfiguration(ctx context.Context) (config.Config, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.currentConfigurationLocked(ctx)
}

func (m *MigrationActivation) currentConfigurationLocked(ctx context.Context) (config.Config, error) {
	if m.status.State == "uncertain" || m.current == nil && m.witness != nil {
		return config.Config{}, errors.New("activation selection is uncertain; retained data requires selection review")
	}
	if err := activationCanonicalDirectory(m.bootstrap.DataDir); err != nil {
		return config.Config{}, err
	}
	root, err := os.OpenRoot(m.bootstrap.DataDir)
	if err != nil {
		return config.Config{}, err
	}
	defer root.Close()
	if err = m.verifyWitness(ctx, root); err != nil {
		return config.Config{}, err
	}
	raw, err := activationReadRegular(ctx, root, migrationActivationFile, 8192)
	if m.current == nil {
		if !os.IsNotExist(err) {
			return config.Config{}, errors.New("bootstrap selection changed; cannot reopen previous application")
		}
		if err = PreflightMigrationTarget(ctx, m.bootstrap); err != nil {
			return config.Config{}, err
		}
		return m.bootstrap, nil
	}
	var value migrationActivationSelection
	if err != nil || activationDecode(raw, &value) != nil || value != *m.current {
		return config.Config{}, errors.New("previous migration selector changed; refusing rollback")
	}
	return m.verifySelection(ctx, value)
}

// VerifyFailedRetry is read-only and authorizes no activation. The API may use
// it to offer a fresh explicit confirmation only for this process's known,
// cleanly rolled-back attempt. Restarted or uncertain attempts never qualify.
func (m *MigrationActivation) VerifyFailedRetry(ctx context.Context, target, configSHA, receiptSHA, sessionAudience string) error {
	if m == nil {
		return errors.New("in-process activation is unavailable")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.status.State != "failed" || m.failed == nil || m.pending != nil {
		return errors.New("activation retry requires a known clean failed attempt")
	}
	failed := *m.failed
	if target != m.target(failed.OperationID) || m.status.OperationID != failed.OperationID || configSHA != failed.ConfigSHA256 || receiptSHA != failed.ReceiptSHA256 || sessionAudience != failed.SessionAudience {
		return errors.New("activation retry does not match the failed attempt")
	}
	if _, err := m.currentConfigurationLocked(ctx); err != nil {
		return err
	}
	_, err := m.verifySelection(ctx, failed)
	return err
}

// ActivateVerified is called only after the web API has authenticated the actor
// and verified its separate explicit activation confirmation. These arguments
// come from the private operation record, never from browser-supplied paths.
func (m *MigrationActivation) ActivateVerified(ctx context.Context, target, configSHA, receiptSHA string) (MigrationActivationStatus, error) {
	if m == nil {
		return m.Status(), errors.New("in-process activation is unavailable")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.status.Available {
		return m.status, errors.New("web migration activation requires a canonical data directory")
	}
	id := filepath.Base(target)
	if !activationHex(id, 32) || target != m.target(id) || !activationHex(configSHA, 64) || !activationHex(receiptSHA, 64) {
		return m.status, errors.New("activation requires a server-generated verified target")
	}
	if m.status.State == "uncertain" {
		return m.status, errors.New("activation outcome is uncertain; restart and verify the persisted selection")
	}
	if m.pending != nil {
		p := m.pending.selection
		if p.OperationID == id && p.ConfigSHA256 == configSHA && p.ReceiptSHA256 == receiptSHA {
			return m.status, nil
		}
		return m.status, errors.New("another activation is already pending")
	}
	if m.current != nil && m.current.OperationID == id {
		if m.current.ConfigSHA256 != configSHA || m.current.ReceiptSHA256 != receiptSHA {
			return m.status, errors.New("active migration evidence differs")
		}
		m.status = MigrationActivationStatus{Available: true, State: "active", OperationID: id}
		return m.status, nil
	}
	evidence, err := migrate.ReadStartupEvidence(ctx, target, m.bootstrap.MigrationReview.Options())
	if err != nil || evidence == nil || evidence.ReceiptSHA256 != receiptSHA {
		return m.status, errors.New("staged migration evidence differs")
	}
	raw, err := activationReadConfig(ctx, target)
	if err != nil {
		return m.status, err
	}
	cfg := config.Defaults()
	if err = activationDecode(raw, &cfg); err != nil {
		return m.status, errors.New("staged runtime configuration is invalid")
	}
	selected := migrationActivationSelection{Version: 1, OperationID: id, ConfigSHA256: configSHA, ReceiptSHA256: receiptSHA, JournalSHA256: evidence.JournalSHA256, SessionAudience: cfg.SessionAudience}
	if _, err = m.verifySelection(ctx, selected); err != nil {
		return m.status, err
	}
	request := &MigrationActivationRequest{selection: selected}
	m.pending = request
	m.failed = nil
	m.status = MigrationActivationStatus{Available: true, State: "pending", OperationID: id, Message: "Activation accepted; reconnect after the application restarts"}
	m.requests <- request
	return m.status, nil
}

// Request additionally checks the operation ID held by the private API record.
func (m *MigrationActivation) Request(ctx context.Context, operationID, target, receiptSHA, configSHA string) (MigrationActivationStatus, error) {
	if filepath.Base(target) != operationID {
		return m.Status(), errors.New("activation operation does not match its generated target")
	}
	return m.ActivateVerified(ctx, target, configSHA, receiptSHA)
}

func (m *MigrationActivation) Verify(ctx context.Context, request *MigrationActivationRequest) (config.Config, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if request == nil || request != m.pending {
		return config.Config{}, errors.New("activation request is not current")
	}
	if m.status.State == "uncertain" {
		return config.Config{}, errors.New("activation selection is uncertain; refusing to retry")
	}
	cfg, err := m.verifySelection(ctx, request.selection)
	if err == nil {
		m.status.State = "activating"
	}
	return cfg, err
}

// Commit returns committed=true once first-witness publication or selector
// rename may have changed durable state, including on an I/O error. That
// uncertainty MUST NOT cause the old runtime to be served again. A fresh startup
// inspects both records and fails closed on a missing/mismatched selection.
func (m *MigrationActivation) Commit(ctx context.Context, request *MigrationActivationRequest) (committed bool, resultErr error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if request == nil || request != m.pending {
		return false, errors.New("activation request is not current")
	}
	if m.status.State == "uncertain" {
		return true, errors.New("activation selection is uncertain; refusing to retry")
	}
	if _, err := m.verifySelection(ctx, request.selection); err != nil {
		return false, err
	}
	if err := activationCanonicalDirectory(m.bootstrap.DataDir); err != nil {
		return false, err
	}
	root, err := os.OpenRoot(m.bootstrap.DataDir)
	if err != nil {
		return false, err
	}
	defer root.Close()
	if err = m.verifyWitness(ctx, root); err != nil {
		return false, err
	}
	// Compare with the known selector before replacement. Never overwrite a
	// concurrent activation, corrupt file, or symlink with an apparently valid one.
	old, err := activationReadRegular(ctx, root, migrationActivationFile, 8192)
	if m.current == nil {
		if !os.IsNotExist(err) {
			return false, errors.New("active migration selector changed before activation")
		}
	} else {
		var value migrationActivationSelection
		if err != nil || activationDecode(old, &value) != nil || value != *m.current {
			return false, errors.New("active migration selector changed before activation")
		}
	}
	raw, err := json.Marshal(request.selection)
	if err != nil {
		return false, err
	}
	temporary := migrationActivationFile + ".tmp-" + randomID()
	f, err := root.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return false, err
	}
	defer root.Remove(temporary)
	_, err = f.Write(raw)
	if err == nil {
		err = f.Sync()
	}
	err = errors.Join(err, f.Close())
	if err != nil {
		return false, err
	}
	if err = m.inject("before-rename"); err != nil {
		return false, err
	}
	if err = ctx.Err(); err != nil {
		return false, err
	}
	defer func() {
		if committed && resultErr != nil {
			m.status.State = "uncertain"
			m.status.Message = "Selection persistence is uncertain; retained data requires startup selection review"
		}
	}()
	if m.witness == nil {
		// A separate immutable witness prevents deleting the selector from
		// silently turning an activated installation back into its old root.
		// Never erase this fence on failure: a partial first commit is explicit.
		committed = true
		if err = publishImmutableBytes(ctx, root, migrationActivationWitnessFile, raw, 8192); err != nil {
			return true, err
		}
		witness := request.selection
		m.witness = &witness
		if err = m.inject("after-witness"); err != nil {
			return true, err
		}
	}
	// After rename has been attempted, an error is conservatively uncertain;
	// returning false must guarantee that no durable selection may have changed.
	committed = true
	if err = root.Rename(temporary, migrationActivationFile); err != nil {
		return true, err
	}
	if err = m.inject("after-rename"); err != nil {
		return true, err
	}
	dir, err := root.Open(".")
	if err != nil {
		return true, err
	}
	err = errors.Join(dir.Sync(), dir.Close())
	if err != nil {
		return true, err
	}
	selected := request.selection
	m.current = &selected
	return true, nil
}

func (m *MigrationActivation) Complete(request *MigrationActivationRequest, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if request == nil || request != m.pending {
		return
	}
	if m.status.State == "uncertain" {
		return
	}
	m.pending = nil
	if err != nil {
		failed := request.selection
		m.failed = &failed
		m.status.State = "failed"
		m.status.Message = "Activation failed; the previous application remains active. The verified target is retained for retry"
		return
	}
	m.failed = nil
	m.status = MigrationActivationStatus{Available: true, State: "active", OperationID: request.selection.OperationID}
}

func (m *MigrationActivation) target(id string) string {
	return filepath.Join(m.bootstrap.DataDir, ".web-migrations", "targets", id)
}

func (m *MigrationActivation) verifySelection(ctx context.Context, selected migrationActivationSelection) (config.Config, error) {
	var empty config.Config
	if !activationSelectionValid(selected) {
		return empty, errors.New("active migration selector has invalid bindings")
	}
	target := m.target(selected.OperationID)
	if err := activationCanonicalDirectory(target); err != nil {
		return empty, err
	}
	evidence, err := migrate.ReadStartupEvidence(ctx, target, m.bootstrap.MigrationReview.Options())
	if err != nil || evidence == nil || evidence.Receipt.Version != "1.0" || evidence.ReceiptSHA256 != selected.ReceiptSHA256 || evidence.JournalSHA256 != selected.JournalSHA256 {
		return empty, errors.New("active migration evidence differs; refusing unverified target")
	}
	raw, err := activationReadConfig(ctx, target)
	if err != nil {
		return empty, err
	}
	hash := sha256.Sum256(raw)
	if hex.EncodeToString(hash[:]) != selected.ConfigSHA256 {
		return empty, errors.New("staged runtime configuration changed")
	}
	bound := false
	for _, f := range evidence.Receipt.Files {
		if f.Destination == "config.json" && f.SHA256 == selected.ConfigSHA256 && f.Size == int64(len(raw)) {
			bound = true
		}
	}
	if !bound {
		return empty, errors.New("staged runtime configuration is not receipt-bound")
	}
	cfg := config.Defaults()
	if err = activationDecode(raw, &cfg); err != nil {
		return empty, errors.New("staged runtime configuration is invalid")
	}
	if cfg.Driver != "sqlite" || cfg.DataDir != target || cfg.DSN != filepath.Join(target, "anidan.db") || cfg.SessionAudience != selected.SessionAudience || cfg.SessionAudience == m.bootstrap.SessionAudience {
		return empty, errors.New("staged runtime identity or session audience is invalid")
	}
	// Operational installation paths remain those of the running executable.
	// Secrets, database credentials and imported application settings never pass
	// through Config.Load's ambient environment overrides at activation/restart.
	cfg.Listen, cfg.WebDir, cfg.StaticDir, cfg.PublicURL = m.bootstrap.Listen, m.bootstrap.WebDir, m.bootstrap.StaticDir, m.bootstrap.PublicURL
	cfg.MigrationReview = m.bootstrap.MigrationReview
	cfg.RequireMigrationEvidence = true
	if err = cfg.Validate(); err != nil {
		return empty, errors.New("staged runtime configuration failed validation")
	}
	if err = PreflightMigrationTarget(ctx, cfg); err != nil {
		return empty, err
	}
	ro, err := store.OpenReadOnly(ctx, "sqlite", cfg.DSN)
	if err != nil {
		return empty, errors.New("cannot inspect staged migration database")
	}
	defer ro.Close()
	temporary := &Server{Store: ro, DataDir: target, Config: cfg}
	rawState, exists, err := temporary.readMigrationReviewState(ctx)
	if err != nil {
		return empty, errors.New("cannot inspect staged migration review state")
	}
	state, stateErr := decodeMigrationReviewState(rawState)
	if exists && stateErr == nil && state.matches(evidence) {
		// Existing used targets retain their original receipt identity, while
		// ordinary library/database writes no longer have pristine byte hashes.
		if err = temporary.validateMigrationGate(ctx); err != nil {
			return empty, err
		}
	} else if err = migrate.VerifyStartupContents(ctx, target, evidence); err != nil {
		return empty, err
	}
	return cfg, ctx.Err()
}

func activationCanonicalDirectory(path string) error {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return err
	}
	if resolved != path {
		return errors.New("activation data directories must be canonical and nonsymlink")
	}
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() {
		return errors.New("activation data root is not a directory")
	}
	return nil
}

func activationReadConfig(ctx context.Context, target string) ([]byte, error) {
	root, err := os.OpenRoot(target)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	return activationReadRegular(ctx, root, "config.json", migrationActivationLimit)
}

func activationReadWitness(ctx context.Context, root *os.Root) (*migrationActivationSelection, error) {
	raw, err := activationReadRegular(ctx, root, migrationActivationWitnessFile, 8192)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var witness migrationActivationSelection
	if err = activationDecode(raw, &witness); err != nil || !activationSelectionValid(witness) {
		return nil, errors.New("invalid migration activation witness")
	}
	return &witness, nil
}

func (m *MigrationActivation) verifyWitness(ctx context.Context, root *os.Root) error {
	witness, err := activationReadWitness(ctx, root)
	if err != nil || (witness == nil) != (m.witness == nil) || witness != nil && *witness != *m.witness {
		return errors.New("migration activation witness changed; retained data requires selection review")
	}
	return nil
}

func activationSelectionValid(selected migrationActivationSelection) bool {
	return selected.Version == 1 && activationHex(selected.OperationID, 32) && activationHex(selected.ConfigSHA256, 64) && activationHex(selected.ReceiptSHA256, 64) && activationHex(selected.JournalSHA256, 64) && activationHex(selected.SessionAudience, 64)
}

func activationReadRegular(ctx context.Context, root *os.Root, name string, limit int64) ([]byte, error) {
	before, err := root.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !before.Mode().IsRegular() || before.Size() > limit {
		return nil, errors.New("activation evidence must be a bounded regular file")
	}
	f, err := root.OpenFile(name, regularReadFlags, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(before, opened) || !opened.Mode().IsRegular() {
		return nil, errors.New("activation evidence changed while opening")
	}
	raw, err := io.ReadAll(io.LimitReader(contextReader{ctx, f}, limit+1))
	if err != nil {
		return nil, err
	}
	after, err := f.Stat()
	if err != nil {
		return nil, err
	}
	named, err := root.Lstat(name)
	if err != nil || !named.Mode().IsRegular() || !os.SameFile(opened, named) || after.Size() != before.Size() || int64(len(raw)) != before.Size() || !before.ModTime().Equal(after.ModTime()) {
		return nil, errors.New("activation evidence changed while reading")
	}
	return raw, ctx.Err()
}

func activationDecode(raw []byte, out any) error {
	// A key walk rejects duplicate keys, including inside nested configuration.
	d := json.NewDecoder(bytes.NewReader(raw))
	if err := activationJSONValue(d, 0); err != nil {
		return err
	}
	if _, err := d.Token(); err != io.EOF {
		return errors.New("trailing activation JSON")
	}
	d = json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	return d.Decode(out)
}

func activationJSONValue(d *json.Decoder, depth int) error {
	if depth > 32 {
		return errors.New("activation JSON exceeds nesting limit")
	}
	token, err := d.Token()
	if err != nil {
		return err
	}
	switch token {
	case json.Delim('{'):
		seen := map[string]bool{}
		for d.More() {
			key, err := d.Token()
			if err != nil {
				return err
			}
			name, ok := key.(string)
			if !ok || seen[name] {
				return errors.New("duplicate activation JSON field")
			}
			seen[name] = true
			if err = activationJSONValue(d, depth+1); err != nil {
				return err
			}
		}
		_, err = d.Token()
	case json.Delim('['):
		for d.More() {
			if err = activationJSONValue(d, depth+1); err != nil {
				return err
			}
		}
		_, err = d.Token()
	}
	return err
}

func activationHex(value string, length int) bool {
	if len(value) != length || strings.ToLower(value) != value {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func (m *MigrationActivation) inject(phase string) error {
	if m.fault != nil {
		return m.fault(phase)
	}
	return nil
}
