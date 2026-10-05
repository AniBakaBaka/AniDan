// SPDX-License-Identifier: AGPL-3.0-only
package server

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/AniBakaBaka/AniDan/internal/migrate"
)

const webMigrationMaxOperations = 32
const webMigrationTicketLifetime = 5 * time.Minute
const webMigrationRecordLimit = 2 << 20

type webMigrationRuntime struct {
	mu      sync.Mutex
	root    string
	ops     map[string]*webMigrationOperation
	active  string
	orphans int
	err     error
}
type webMigrationProgress struct {
	Percent int    `json:"percent"`
	Message string `json:"message"`
}
type webMigrationTable struct {
	Name string `json:"name"`
	Rows int64  `json:"rows"`
}
type webMigrationReview struct {
	SourceDBType        string              `json:"sourceDBType"`
	SourceCreatedAt     string              `json:"sourceCreatedAt"`
	TargetAdminUsername string              `json:"targetAdminUsername,omitempty"`
	TargetTimezone      string              `json:"targetTimezone,omitempty"`
	SnapshotSHA256      string              `json:"snapshotSHA256"`
	ProofSHA256         string              `json:"proofSHA256"`
	Tables              []webMigrationTable `json:"tables"`
	FileCount           int                 `json:"fileCount"`
	TotalFileBytes      int64               `json:"totalFileBytes"`
	Warnings            []string            `json:"warnings"`
	TargetDir           string              `json:"targetDir"`
	ExpiresAt           time.Time           `json:"expiresAt"`
	ReviewToken         string              `json:"reviewToken,omitempty"`
}
type webMigrationPrepared struct {
	ReceiptSHA256          string    `json:"receiptSHA256"`
	ConfigSHA256           string    `json:"configSHA256"`
	TargetDir              string    `json:"targetDir"`
	ConfigFile             string    `json:"configFile"`
	RecoveryReviewRequired bool      `json:"recoveryReviewRequired"`
	ActivationAvailable    bool      `json:"activationAvailable"`
	ActivationReason       string    `json:"activationReason,omitempty"`
	ApplyToken             string    `json:"applyToken,omitempty"`
	ExpiresAt              time.Time `json:"expiresAt"`
}
type webMigrationOperation struct {
	ID                  string                      `json:"id"`
	Actor               string                      `json:"actor"` // persisted privately; never serialized as an HTTP response
	SessionAudience     string                      `json:"sessionAudience"`
	State               string                      `json:"state"`
	Filename            string                      `json:"filename"`
	Size                int64                       `json:"size"`
	SHA256              string                      `json:"sha256"`
	Progress            webMigrationProgress        `json:"progress"`
	JobID               string                      `json:"jobId,omitempty"`
	CreatedAt           time.Time                   `json:"createdAt"`
	UpdatedAt           time.Time                   `json:"updatedAt"`
	Review              *webMigrationReview         `json:"review,omitempty"`
	Prepared            *webMigrationPrepared       `json:"prepared,omitempty"`
	Error               string                      `json:"error,omitempty"`
	ErrorCode           string                      `json:"errorCode,omitempty"`
	UploadInterrupted   bool                        `json:"uploadInterrupted,omitempty"`
	Roots               []migrate.RootMapping       `json:"roots,omitempty"`
	HasConfig           bool                        `json:"hasConfig,omitempty"`
	LegacyConfigSHA256  string                      `json:"legacyConfigSHA256,omitempty"`
	CancelRequested     bool                        `json:"cancelRequested,omitempty"`
	ActivationAttempted bool                        `json:"activationAttempted,omitempty"`
	TicketRevision      uint64                      `json:"ticketRevision"`
	RootSelections      []webMigrationRootSelection `json:"rootSelections,omitempty"`
	SourceMode          string                      `json:"sourceMode,omitempty"`
	SourceDisplay       *webMigrationSourceDisplay  `json:"sourceDisplay,omitempty"`
	DetectedSelection   string                      `json:"detectedSelection,omitempty"` // private replay receipt, never exposed
	CaptureComplete     bool                        `json:"captureComplete,omitempty"`
}
type webMigrationClaim struct {
	Version  int    `json:"version"`
	Revision uint64 `json:"revision"`
	Purpose  string `json:"purpose"`
	Actor    string `json:"actor"`
	ID       string `json:"id"`
	Snapshot string `json:"snapshot"`
	Proof    string `json:"proof"`
	Receipt  string `json:"receipt,omitempty"`
	Config   string `json:"config,omitempty"`
	Expires  int64  `json:"expires"`
}

func webMigrationHexText(s string, n int) bool {
	if len(s) != n {
		return false
	}
	b, e := hex.DecodeString(s)
	return e == nil && hex.EncodeToString(b) == s
}
func webMigrationRandom(n int) (string, error) {
	b := make([]byte, n)
	_, e := rand.Read(b)
	return hex.EncodeToString(b), e
}
func webMigrationRunning(state string) bool {
	return state == "inspecting" || state == "preparing" || state == "activating"
}
func (s *Server) webMigrationBootstrap() string {
	if s.MigrationActivation != nil {
		return s.MigrationActivation.BootstrapDataDir()
	}
	return s.DataDir
}
func (s *Server) webMigrations() *webMigrationRuntime {
	s.webMigrationOnce.Do(func() {
		rt := &webMigrationRuntime{ops: map[string]*webMigrationOperation{}}
		s.webMigration = rt
		bootstrap, e := filepath.Abs(s.webMigrationBootstrap())
		if e != nil {
			rt.err = e
			return
		}
		if e = webMigrationDirectory(bootstrap, false); e != nil {
			rt.err = e
			return
		}
		rt.root = filepath.Join(bootstrap, ".web-migrations")
		for _, p := range []string{rt.root, filepath.Join(rt.root, "operations"), filepath.Join(rt.root, "targets")} {
			if e = webMigrationDirectory(p, true); e != nil {
				rt.err = e
				return
			}
		}
		entries, e := webMigrationDirectoryEntries(filepath.Join(rt.root, "operations"), webMigrationMaxOperations)
		if e != nil || len(entries) > webMigrationMaxOperations {
			rt.err = errors.New("migration operation storage unavailable or full")
			return
		}
		for _, entry := range entries {
			if !webMigrationHexText(entry.Name(), 32) || !entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
				rt.err = errors.New("invalid migration operation storage")
				return
			}
			dir := filepath.Join(rt.root, "operations", entry.Name())
			if e = webMigrationDirectory(dir, false); e != nil {
				rt.err = e
				return
			}
			raw, e := webMigrationRead(filepath.Join(dir, "operation.json"), webMigrationRecordLimit)
			if os.IsNotExist(e) {
				// A crash between mkdir and the first record cannot identify an
				// owner. Preserve and count its bytes without adopting the upload.
				rt.orphans++
				continue
			}
			if e != nil {
				rt.err = e
				return
			}
			var op webMigrationOperation
			if e = webMigrationDecode(raw, &op); e != nil || op.ID != entry.Name() || !webMigrationHexText(op.Actor, 64) || !webMigrationHexText(op.SHA256, 64) || !webMigrationHexText(op.SessionAudience, 64) {
				rt.err = errors.New("invalid migration operation record")
				return
			}
			if op.Review != nil {
				op.Review.ReviewToken = ""
			}
			if op.Prepared != nil {
				op.Prepared.ApplyToken = ""
			}
			if op.SourceMode == "detected" && op.State == "inspecting" && op.CancelRequested {
				op.State, op.Review, op.Prepared = "canceled", nil, nil
				op.ErrorCode = "captureCanceled"
				op.Error = "The recorded cancellation was preserved after restart. No source capture or inspection is resumed."
				op.Progress = webMigrationProgress{0, op.Error}
			} else if op.State == "inspecting" && webMigrationCapturedInspectionRecoverable(rt.root, &op) {
				op.State = "uploaded"
				op.Review, op.Prepared = nil, nil
				op.ErrorCode = "inspectionNeedsCorrection"
				op.Error = "The original SQL capture completed before inspection was interrupted. Explicitly inspect this retained snapshot again; no database capture or preparation is resumed automatically."
				op.Progress = webMigrationProgress{0, op.Error}
			} else if webMigrationRunning(op.State) || (op.ActivationAttempted && op.State == "prepared") {
				op.State = "uncertain"
				op.Error = "The application stopped during this phase. Refresh status to reconcile the generated target; do not replay the request."
				op.Progress.Message = "Interrupted; reconciliation required"
			}
			rt.ops[op.ID] = &op
		}
		// A published or staged target without an authority record is never an
		// incomplete upload. Refuse all mutations until local recovery resolves it.
		targets, e := webMigrationDirectoryEntries(filepath.Join(rt.root, "targets"), 2*webMigrationMaxOperations)
		if e != nil {
			rt.err = e
			return
		}
		for _, target := range targets {
			id := strings.TrimSuffix(target.Name(), ".staging")
			if !webMigrationHexText(id, 32) || rt.ops[id] == nil || !target.IsDir() || target.Type()&os.ModeSymlink != 0 {
				rt.err = errors.New("migration target has no valid authority record")
				return
			}
		}
	})
	return s.webMigration
}
func webMigrationDirectoryEntries(path string, limit int) ([]os.DirEntry, error) {
	d, e := os.Open(path)
	if e != nil {
		return nil, e
	}
	defer d.Close()
	entries, e := d.ReadDir(limit + 1)
	if e != nil && e != io.EOF {
		return nil, e
	}
	if len(entries) > limit {
		return nil, errors.New("migration storage entry limit exceeded")
	}
	return entries, nil
}
func webMigrationDirectory(path string, create bool) error {
	st, e := os.Lstat(path)
	if os.IsNotExist(e) && create {
		if e = os.Mkdir(path, 0700); e != nil {
			return e
		}
		st, e = os.Lstat(path)
	}
	if e != nil {
		return e
	}
	if !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
		return errors.New("migration directories must be real directories")
	}
	actual, e := filepath.EvalSymlinks(path)
	if e != nil || actual != path {
		return errors.New("migration directory must use a canonical path")
	}
	return nil
}
func webMigrationRead(path string, limit int64) ([]byte, error) {
	st, e := os.Lstat(path)
	if e != nil {
		return nil, e
	}
	if !st.Mode().IsRegular() || st.Size() > limit {
		return nil, errors.New("invalid migration file")
	}
	f, e := os.Open(path)
	if e != nil {
		return nil, e
	}
	defer f.Close()
	fst, e := f.Stat()
	if e != nil || !os.SameFile(st, fst) {
		return nil, errors.New("migration file changed")
	}
	b, e := io.ReadAll(io.LimitReader(f, limit+1))
	if int64(len(b)) > limit {
		return nil, errors.New("migration file exceeds limit")
	}
	return b, e
}
func webMigrationDecode(raw []byte, v any) error {
	// Reuse the bounded key walk so duplicate confirmation/identity fields
	// cannot be interpreted differently by review, persistence and execution.
	keys := json.NewDecoder(bytes.NewReader(raw))
	if e := activationJSONValue(keys, 0); e != nil {
		return e
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if e := d.Decode(v); e != nil {
		return e
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return errors.New("one JSON value required")
	}
	return nil
}
func (rt *webMigrationRuntime) save(op *webMigrationOperation) error {
	op.UpdatedAt = time.Now().UTC()
	raw, e := json.Marshal(op)
	if e != nil || len(raw) > webMigrationRecordLimit {
		return errors.New("migration record exceeds limit")
	}
	dir := filepath.Join(rt.root, "operations", op.ID)
	if e = webMigrationDirectory(dir, false); e != nil {
		return e
	}
	f, e := os.CreateTemp(dir, ".record-")
	if e != nil {
		return e
	}
	name := f.Name()
	defer os.Remove(name)
	if _, e = f.Write(raw); e == nil {
		e = f.Sync()
	}
	e = errors.Join(e, f.Close())
	if e != nil {
		return e
	}
	if e = os.Rename(name, filepath.Join(dir, "operation.json")); e != nil {
		return e
	}
	d, e := os.Open(dir)
	if e != nil {
		return e
	}
	defer d.Close()
	return d.Sync()
}
func (s *Server) webMigrationClaim(op *webMigrationOperation, purpose string, expiry time.Time) webMigrationClaim {
	c := webMigrationClaim{Version: 1, Revision: op.TicketRevision, Purpose: purpose, Actor: op.Actor, ID: op.ID, Snapshot: op.SHA256, Expires: expiry.Unix()}
	if op.Review != nil {
		c.Proof = op.Review.ProofSHA256
	}
	if purpose == "apply" && op.Prepared != nil {
		c.Receipt = op.Prepared.ReceiptSHA256
		c.Config = op.Prepared.ConfigSHA256
	}
	return c
}
func (s *Server) webMigrationSign(c webMigrationClaim) (string, error) {
	key, e := s.localReviewKey()
	if e != nil {
		return "", e
	}
	raw, e := json.Marshal(c)
	if e != nil {
		return "", e
	}
	h := hmac.New(sha256.New, key)
	h.Write(raw)
	return base64.RawURLEncoding.EncodeToString(raw) + "." + base64.RawURLEncoding.EncodeToString(h.Sum(nil)), nil
}
func (s *Server) webMigrationVerify(token string, op *webMigrationOperation, purpose string) bool {
	if len(token) > 2048 {
		return false
	}
	p := strings.Split(token, ".")
	if len(p) != 2 {
		return false
	}
	raw, e := base64.RawURLEncoding.Strict().DecodeString(p[0])
	if e != nil {
		return false
	}
	sig, e := base64.RawURLEncoding.Strict().DecodeString(p[1])
	if e != nil {
		return false
	}
	key, e := s.localReviewKey()
	if e != nil {
		return false
	}
	h := hmac.New(sha256.New, key)
	h.Write(raw)
	if !hmac.Equal(sig, h.Sum(nil)) {
		return false
	}
	var c webMigrationClaim
	if webMigrationDecode(raw, &c) != nil || c.Expires <= time.Now().Unix() || c.Expires > time.Now().Add(webMigrationTicketLifetime).Unix()+1 {
		return false
	}
	want := s.webMigrationClaim(op, purpose, time.Unix(c.Expires, 0))
	return c == want
}
func (s *Server) webMigrationActivationStatus() (bool, string) {
	if s.MigrationActivation == nil {
		return false, "This server was embedded without the application activation lifecycle; preparation is available."
	}
	v := s.MigrationActivation.Status()
	if !v.Available {
		if v.Message != "" {
			return false, v.Message
		}
		return false, "Application activation is unavailable."
	}
	if v.State == "uncertain" {
		return false, "Activation needs a restart to reconcile the selected library."
	}
	return true, ""
}
func (s *Server) webMigrationView(op *webMigrationOperation) map[string]any {
	out := map[string]any{"id": op.ID, "state": op.State, "filename": op.Filename, "size": op.Size, "sha256": op.SHA256, "progress": op.Progress, "jobId": op.JobID, "createdAt": op.CreatedAt, "updatedAt": op.UpdatedAt, "configUploaded": op.HasConfig, "roots": op.RootSelections}
	if op.SourceMode == "detected" {
		out["sourceMode"], out["sourceDisplay"] = op.SourceMode, op.SourceDisplay
	}
	if op.Error != "" {
		out["error"] = op.Error
	}
	if op.ErrorCode != "" {
		out["errorCode"] = op.ErrorCode
	}
	if op.UploadInterrupted {
		out["uploadInterrupted"] = true
	}
	if op.Review != nil {
		r := *op.Review
		r.ReviewToken = ""
		if op.State == "review" && r.ExpiresAt.After(time.Now()) {
			r.ReviewToken, _ = s.webMigrationSign(s.webMigrationClaim(op, "prepare", r.ExpiresAt))
		}
		out["review"] = r
	}
	if op.Prepared != nil {
		p := *op.Prepared
		p.ActivationAvailable, p.ActivationReason = s.webMigrationActivationStatus()
		p.ApplyToken = ""
		if op.State == "prepared" && p.ActivationAvailable {
			p.ExpiresAt = time.Now().UTC().Add(webMigrationTicketLifetime)
			p.ApplyToken, _ = s.webMigrationSign(s.webMigrationClaim(op, "apply", p.ExpiresAt))
		}
		out["prepared"] = p
	}
	return out
}
func webMigrationReviewOf(r migrate.Receipt, proof string) *webMigrationReview {
	v := &webMigrationReview{SourceDBType: r.SourceDBType, SourceCreatedAt: r.SourceCreatedAt, SnapshotSHA256: r.SnapshotSHA256, ProofSHA256: proof, Tables: []webMigrationTable{}, Warnings: []string{}, TargetDir: r.TargetDir, ExpiresAt: time.Now().UTC().Add(webMigrationTicketLifetime)}
	v.SourceCreatedAt = "unknown"
	if len(r.SourceCreatedAt) <= 64 {
		if created, e := time.Parse(time.RFC3339, r.SourceCreatedAt); e == nil {
			v.SourceCreatedAt = created.UTC().Format(time.RFC3339Nano)
		}
	}
	for name, t := range r.Tables {
		v.Tables = append(v.Tables, webMigrationTable{name, t.Rows})
	}
	sort.Slice(v.Tables, func(i, j int) bool { return v.Tables[i].Name < v.Tables[j].Name })
	for _, f := range r.Files {
		if f.Destination != "config.json" {
			v.FileCount++
			v.TotalFileBytes += f.Size
		}
	}
	// Import diagnostics can contain source row values and credentials. Expose
	// only schema-owned summaries, never the raw warning strings.
	if len(r.Warnings) > 0 {
		v.Warnings = append(v.Warnings, "The source needs attention: review legacy configuration compatibility and unresolved source file mappings before continuing.")
	}
	return v
}
func (s *Server) webMigrationReconcile(ctx context.Context, rt *webMigrationRuntime, op *webMigrationOperation) {
	if s.MigrationActivation != nil {
		a := s.MigrationActivation.Status()
		if a.OperationID == op.ID && a.State == "failed" && op.ActivationAttempted && op.State != "active" && op.Prepared != nil {
			op.State = "uncertain"
			if rt.active == op.ID {
				rt.active = ""
			}
			if s.MigrationActivation.VerifyFailedRetry(ctx, op.Prepared.TargetDir, op.Prepared.ConfigSHA256, op.Prepared.ReceiptSHA256, op.SessionAudience) == nil {
				op.State = "prepared"
				op.Error = ""
				op.Progress = webMigrationProgress{100, "Activation was rolled back safely; review and explicitly confirm a new attempt"}
			} else {
				op.Error = "The failed activation could not be verified for another attempt. Keep the target and refresh status; do not replay activation."
			}
			_ = rt.save(op)
			return
		}
		if a.OperationID == op.ID && a.State == "active" {
			op.State = "active"
			op.Progress = webMigrationProgress{100, "Migrated library is active; sign in again"}
			op.Error = ""
			_ = rt.save(op)
			return
		}
	}
	if op.ActivationAttempted || op.State == "active" {
		return
	}
	if op.State != "uncertain" && op.State != "failed" && op.State != "canceled" {
		return
	}
	target := filepath.Join(rt.root, "targets", op.ID)
	if _, e := os.Lstat(target); os.IsNotExist(e) {
		if op.State == "uncertain" {
			op.State = "failed"
			op.Error = "Interrupted before a verified target was found. Upload a new operation to restart; requests are never automatically replayed."
			_ = rt.save(op)
		}
		return
	}
	evidence, e := migrate.ReadStartupEvidence(ctx, target, s.Config.MigrationReview.Options())
	if e != nil || evidence == nil || op.Review == nil || migrate.ReceiptProofSHA256(evidence.Receipt) != op.Review.ProofSHA256 {
		op.State = "uncertain"
		op.Error = "A generated target exists but its verification did not match. Keep it for recovery; do not replay preparation."
		return
	}
	if migrate.VerifyStartupContents(ctx, target, evidence) != nil {
		op.State = "uncertain"
		op.Error = "The generated target is no longer pristine or could not be fully verified. Keep it for recovery; it cannot be activated from this review."
		return
	}
	raw, e := webMigrationRead(filepath.Join(target, "config.json"), 2<<20)
	if e != nil {
		return
	}
	sum := sha256.Sum256(raw)
	op.Prepared = &webMigrationPrepared{ReceiptSHA256: evidence.ReceiptSHA256, ConfigSHA256: hex.EncodeToString(sum[:]), TargetDir: target, ConfigFile: filepath.Join(target, "config.json"), RecoveryReviewRequired: true}
	op.State = "prepared"
	op.Progress = webMigrationProgress{100, "Verified target prepared; explicit activation is required"}
	op.Error = ""
	_ = rt.save(op)
}
