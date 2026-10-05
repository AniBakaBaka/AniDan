// SPDX-License-Identifier: AGPL-3.0-only
package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/AniBakaBaka/AniDan/internal/config"
	"github.com/AniBakaBaka/AniDan/internal/job"
	"github.com/AniBakaBaka/AniDan/internal/migrate"
)

type webMigrationRoot struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	Path  string `json:"path"`
}
type webMigrationRootSelection struct {
	From   string `json:"from"`
	RootID string `json:"rootId"`
	Path   string `json:"path"`
}

func (s *Server) registerWebMigration(mux *http.ServeMux) {
	mux.HandleFunc("/api/ui/migration/", s.webMigrationHTTP)
}
func (s *Server) webMigrationConfig() config.Config {
	if s.MigrationActivation != nil {
		cfg := s.MigrationActivation.BootstrapConfig()
		// New may have read or generated the installation key after the
		// bootstrap policy was captured. Preserve that effective MFA key.
		cfg.JWTSecret, cfg.JWTAlgorithm = s.Config.JWTSecret, s.Config.JWTAlgorithm
		return cfg
	}
	return s.Config
}

// Reserve bounded multipart framing inside the application's strict body cap.
func (s *Server) webMigrationUploadLimit() int64 {
	return max(int64(0), min(s.Config.MaxBodyBytes-(64<<10), 128<<20))
}
func (s *Server) webMigrationRoots() []webMigrationRoot {
	out := []webMigrationRoot{}
	for i, p := range s.webMigrationConfig().ReadRoots {
		if !utf8.ValidString(p) {
			continue
		}
		p, e := webMigrationCanonicalDirectory(p)
		if e != nil || !utf8.ValidString(p) || p == string(filepath.Separator) || webMigrationWithin(p, s.DataDir) || webMigrationWithin(s.DataDir, p) || webMigrationWithin(p, s.webMigrationBootstrap()) || webMigrationWithin(s.webMigrationBootstrap(), p) {
			continue
		}
		sum := sha256.Sum256([]byte(p))
		out = append(out, webMigrationRoot{ID: hex.EncodeToString(sum[:8]), Label: fmt.Sprintf("Readable copy root %d", i+1), Path: p})
	}
	return out
}
func (s *Server) webMigrationHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	u, _, e := s.authClaims(r)
	if e != nil || u == nil {
		w.Header().Set("WWW-Authenticate", "Bearer")
		httpError(w, 401, "A signed-in administrator Bearer session is required for migration.")
		return
	}
	admin := s.Config.AdminUsername
	if admin == "" {
		admin = "admin"
	}
	if authString(u["username"]) != admin {
		httpError(w, 403, "Migration requires the configured administrator account.")
		return
	}
	actor, e := s.localReviewActor(r)
	if e != nil {
		httpError(w, 401, "A valid administrator identity is required.")
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/api/ui/migration/")
	if path == "capabilities" && r.Method == "GET" {
		s.webMigrationCapabilities(w)
		return
	}
	if reason := s.webMigrationRootReason(); reason != "" {
		httpError(w, 409, reason)
		return
	}
	if path == "detect" && r.Method == "GET" {
		s.webMigrationDetectHTTP(w, r, actor)
		return
	}
	rt := s.webMigrations()
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if rt.err != nil {
		httpError(w, 503, "Private migration storage is unavailable. Check its permissions, capacity and operation records.")
		return
	}
	if active := rt.ops[rt.active]; active != nil {
		s.webMigrationRefresh(r.Context(), rt, active)
	}
	if path == "detect/import" && r.Method == "POST" {
		s.webMigrationDetectedImport(w, r, rt, actor)
		return
	}
	if path == "upload" && r.Method == "POST" {
		s.webMigrationUpload(w, r, rt, actor)
		return
	}
	if path == "operations" && r.Method == "GET" {
		items := []map[string]any{}
		for _, op := range rt.ops {
			if op.Actor == actor {
				s.webMigrationRefresh(r.Context(), rt, op)
				items = append(items, s.webMigrationView(op))
			}
		}
		sort.Slice(items, func(i, j int) bool { return items[i]["createdAt"].(time.Time).After(items[j]["createdAt"].(time.Time)) })
		diagnostics := []string{}
		if rt.orphans > 0 {
			diagnostics = append(diagnostics, "Incomplete uploads without an ownership record were retained privately and count toward the operation limit. They cannot be resumed or adopted; start a new operation.")
		}
		writeJSON(w, 200, map[string]any{"items": items, "diagnostics": diagnostics, "retainedIncompleteUploads": rt.orphans})
		return
	}
	p := strings.Split(path, "/")
	if len(p) < 1 || len(p) > 2 || !webMigrationHexText(p[0], 32) {
		httpError(w, 404, "Migration operation not found.")
		return
	}
	op := rt.ops[p[0]]
	if op == nil || op.Actor != actor {
		httpError(w, 404, "Migration operation not found.")
		return
	}
	s.webMigrationRefresh(r.Context(), rt, op)
	if len(p) == 1 && r.Method == "GET" {
		writeJSON(w, 200, s.webMigrationView(op))
		return
	}
	if len(p) != 2 || r.Method != "POST" {
		httpError(w, 405, "Unsupported migration operation.")
		return
	}
	switch p[1] {
	case "config":
		s.webMigrationConfigUpload(w, r, rt, op)
	case "inspect":
		s.webMigrationInspect(w, r, rt, op)
	case "prepare":
		s.webMigrationPrepareHTTP(w, r, rt, op)
	case "cancel":
		s.webMigrationCancel(w, r, rt, op)
	case "apply":
		s.webMigrationApply(w, r, rt, op)
	default:
		httpError(w, 404, "Migration operation not found.")
	}
}
func (s *Server) webMigrationCapabilities(w http.ResponseWriter) {
	available, reason := s.webMigrationActivationStatus()
	limits := s.webMigrationConfig().MigrationReview.Options()
	uploadReason := ""
	if s.webMigrationUploadLimit() == 0 {
		uploadReason = "The configured request body limit is too small for multipart migration uploads; increase it above 64 KiB."
	}
	rootReason := s.webMigrationRootReason()
	if rootReason != "" {
		uploadReason = rootReason
	}
	writeJSON(w, 200, map[string]any{"uploadMaxBytes": s.webMigrationUploadLimit(), "uploadAvailable": s.webMigrationUploadLimit() > 0 && rootReason == "", "uploadReason": uploadReason, "inspectionAvailable": rootReason == "", "inspectionReason": rootReason, "decodedMaxBytes": int64(8 << 30), "rowMaxBytes": limits.MaxRowBytes, "fileMaxCount": max(0, limits.MaxFiles-1), "receiptEntryMaxCount": limits.MaxFiles, "operationLimit": webMigrationMaxOperations, "maxMappings": 128, "legacyConfigMaxBytes": min(s.webMigrationUploadLimit(), 1<<20), "detectionAvailable": rootReason == "" && len(s.webMigrationRoots()) > 0 && len(s.webMigrationRoots()) <= 32, "detectionReason": webMigrationDetectionReason(rootReason, len(s.webMigrationRoots())), "detectedExportMaxBytes": webMigrationDetectedMaxBytes, "detectedExportTimeoutSeconds": int(webMigrationDetectedTimeout.Seconds()), "sourceFormats": []string{"legacy-v2-json", "legacy-v2-json-gzip"}, "targetDrivers": []string{"sqlite"}, "roots": s.webMigrationRoots(), "activationAvailable": available, "activationReason": reason, "activation": s.MigrationActivation.Status(), "matrix": []map[string]any{{"source": "Legacy v2 JSON or gzip exported from SQLite, PostgreSQL or MySQL", "target": "Fresh server-owned SQLite directory", "supported": true}, {"source": "Native full backup bundle", "target": "Migration wizard", "supported": false, "reason": "Use the backup restore workflow for native full bundles."}, {"source": "Legacy snapshot", "target": "Remote PostgreSQL or MySQL DSN", "supported": false, "reason": "This wizard does not accept remote target credentials or DSNs."}}, "prerequisites": []string{"Stop the old application's database and file writers before the final snapshot/copy; the wizard cannot stop another application.", "Keep an independent backup of the old database, configuration and every referenced file.", "Copy referenced files under a configured readable copy root, separate from the active AniDan data directory.", "Inspection verifies all rows and referenced files before preparation. Any source change invalidates the reviewed plan.", "Preparation creates a new isolated target. Activation closes and reopens this application and requires a new login using migrated credentials.", "Imported pending jobs and schedules remain subject to the separate first-boot recovery review.", "Uploads and private operation records are retained for recovery; at most 32 operations are accepted.", "After an interrupted request, refresh operation status. POST requests are never automatically retried."}})
}
func (s *Server) webMigrationRootReason() string {
	if _, err := webMigrationCanonicalDirectory(s.webMigrationBootstrap()); err != nil {
		return "Migration requires an existing canonical data directory without symbolic links. Configure the installation's canonical data directory and restart before uploading or inspecting."
	}
	return ""
}

func webMigrationMultipart(r *http.Request) (*multipart.Reader, error) {
	if r.URL.RawQuery != "" {
		return nil, errors.New("migration upload does not accept query parameters")
	}
	return r.MultipartReader()
}
func webMigrationReceive(r *http.Request, dir, name string, max int64, configFile bool) (string, int64, string, error) {
	m, e := webMigrationMultipart(r)
	if e != nil {
		return "", 0, "", e
	}
	part, e := m.NextPart()
	if e != nil {
		return "", 0, "", e
	}
	defer part.Close()
	filename := part.FileName()
	lower := strings.ToLower(filename)
	if part.FormName() != "file" || filename == "" || len(filename) > 255 || strings.ContainsAny(filename, "\\\x00\r\n") {
		return "", 0, "", errors.New("one file upload required")
	}
	if configFile {
		if !strings.HasSuffix(lower, ".yml") && !strings.HasSuffix(lower, ".yaml") {
			return "", 0, "", errors.New("legacy configuration must be YAML")
		}
	} else {
		if strings.HasSuffix(lower, ".json.gz") {
			name = "source.json.gz"
		} else if strings.HasSuffix(lower, ".json") {
			name = "source.json"
		} else {
			return "", 0, "", errors.New("only legacy JSON or gzip snapshots are supported")
		}
	}
	f, e := os.OpenFile(filepath.Join(dir, name), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if e != nil {
		return "", 0, "", e
	}
	defer f.Close()
	success := false
	defer func() {
		if !success {
			_ = os.Remove(filepath.Join(dir, name))
		}
	}()
	h := sha256.New()
	n, e := io.Copy(io.MultiWriter(f, h), io.LimitReader(part, max+1))
	if e != nil || n < 1 || n > max {
		return "", 0, "", errors.New("upload is empty, interrupted or exceeds its byte limit")
	}
	if _, e = m.NextPart(); e != io.EOF {
		return "", 0, "", errors.New("exactly one file part is required")
	}
	if e = f.Sync(); e != nil {
		return "", 0, "", e
	}
	if e = f.Close(); e != nil {
		return "", 0, "", e
	}
	success = true
	return filename, n, hex.EncodeToString(h.Sum(nil)), nil
}
func (s *Server) webMigrationUpload(w http.ResponseWriter, r *http.Request, rt *webMigrationRuntime, actor string) {
	if len(rt.ops)+rt.orphans >= webMigrationMaxOperations {
		httpError(w, 409, "The 32-operation storage limit has been reached. Retain recovery files and ask the installation administrator to archive completed operations.")
		return
	}
	id, e := webMigrationRandom(16)
	if e != nil {
		httpError(w, 503, "Migration identity generation is unavailable.")
		return
	}
	aud, e := webMigrationRandom(32)
	if e != nil {
		httpError(w, 503, "Migration identity generation is unavailable.")
		return
	}
	dir := filepath.Join(rt.root, "operations", id)
	if e = os.Mkdir(dir, 0700); e != nil {
		httpError(w, 503, "Cannot create private migration operation storage.")
		return
	}
	op := &webMigrationOperation{ID: id, Actor: actor, SessionAudience: aud, State: "failed", Filename: "incomplete upload", SHA256: strings.Repeat("0", 64), CreatedAt: time.Now().UTC(), UploadInterrupted: true, ErrorCode: "uploadInterrupted", Error: "This upload did not complete. Start a new operation; retained private bytes are never replayed.", Progress: webMigrationProgress{0, "Upload incomplete; start a new operation"}}
	rt.ops[id] = op
	// Persist ownership before reading potentially long-running upload bytes.
	// A crash therefore leaves an actor-scoped failed upload, never guessed success.
	if e = rt.save(op); e != nil {
		httpError(w, 503, "Cannot persist upload ownership. Refresh operations before starting another upload.")
		return
	}
	filename, n, sum, e := webMigrationReceive(r, dir, "source.json", s.webMigrationUploadLimit(), false)
	if e != nil {
		httpError(w, 400, "Upload one legacy v2 .json or .json.gz file within the advertised limit. Native bundles and remote DSN targets are unsupported.")
		return
	}
	op.State, op.Filename, op.Size, op.SHA256 = "uploaded", filename, n, sum
	op.UploadInterrupted, op.ErrorCode, op.Error = false, "", ""
	op.Progress = webMigrationProgress{0, "Uploaded; map source file roots and inspect"}
	if e = rt.save(op); e != nil {
		op.Error = "Upload bytes were stored but operation persistence is uncertain. Refresh operations before another action."
		httpError(w, 503, op.Error)
		return
	}
	writeJSON(w, 201, s.webMigrationView(op))
}
func (s *Server) webMigrationConfigUpload(w http.ResponseWriter, r *http.Request, rt *webMigrationRuntime, op *webMigrationOperation) {
	if op.SourceMode == "detected" {
		httpError(w, 409, "Detected installation settings were captured privately. Detect a new operation to change them.")
		return
	}
	if (op.State != "uploaded" && op.State != "review") || op.HasConfig {
		httpError(w, 409, "Configuration may be uploaded once before preparation. Use a new operation to replace it.")
		return
	}
	_, _, sum, e := webMigrationReceive(r, filepath.Join(rt.root, "operations", op.ID), "legacy-config.yml", min(s.webMigrationUploadLimit(), 1<<20), true)
	if e != nil {
		httpError(w, 400, "Upload one legacy .yml or .yaml configuration file no larger than 1 MiB.")
		return
	}
	op.HasConfig = true
	op.LegacyConfigSHA256 = sum
	op.Review = nil
	op.State = "uploaded"
	op.Error = ""
	op.ErrorCode = ""
	op.Progress = webMigrationProgress{0, "Private configuration uploaded; inspect again"}
	if rt.save(op) != nil {
		httpError(w, 503, "Configuration was stored but operation persistence is uncertain; refresh status.")
		return
	}
	writeJSON(w, 200, s.webMigrationView(op))
}
func (s *Server) webMigrationMappings(in []webMigrationRootSelection) ([]migrate.RootMapping, error) {
	if len(in) > 128 {
		return nil, errors.New("too many mappings")
	}
	available := map[string]string{}
	for _, r := range s.webMigrationRoots() {
		available[r.ID] = r.Path
	}
	out := []migrate.RootMapping{}
	for _, r := range in {
		root, ok := available[r.RootID]
		if !ok || len(r.Path) > 4096 || len(r.From) > 4096 || r.From == "" || strings.ContainsAny(r.Path, "\\\x00") || filepath.IsAbs(r.Path) {
			return nil, errors.New("invalid mapping")
		}
		p := r.Path
		if p == "" {
			p = "."
		}
		for _, part := range strings.Split(p, "/") {
			if part == ".." {
				return nil, errors.New("invalid mapping")
			}
		}
		if !filepath.IsLocal(p) {
			return nil, errors.New("invalid mapping")
		}
		actual := filepath.Join(root, p)
		if !webMigrationWithin(root, actual) {
			return nil, errors.New("invalid mapping")
		}
		out = append(out, migrate.RootMapping{From: r.From, To: actual})
	}
	return webMigrationValidateRoots(out, s.webMigrationConfig().ReadRoots, s.webMigrationBootstrap())
}
func (s *Server) webMigrationInspect(w http.ResponseWriter, r *http.Request, rt *webMigrationRuntime, op *webMigrationOperation) {
	if op.State != "uploaded" && op.State != "review" {
		httpError(w, 409, "Only an uploaded or expired review operation can be inspected. Refresh its status.")
		return
	}
	var in struct {
		Roots []webMigrationRootSelection `json:"roots"`
	}
	if webMigrationRequestDecode(r, &in) != nil {
		httpError(w, 400, "Provide only explicit source root mappings.")
		return
	}
	roots, e := s.webMigrationMappings(in.Roots)
	if op.SourceMode == "detected" {
		if len(in.Roots) != 0 {
			httpError(w, 400, "Detected installations retain their server-derived file roots.")
			return
		}
		roots, e = webMigrationValidateRoots(op.Roots, s.webMigrationConfig().ReadRoots, s.webMigrationBootstrap())
	}
	if e != nil {
		httpError(w, 400, "Select configured readable copy roots with confined relative paths, separate from the active data directory.")
		return
	}
	if rt.active != "" {
		httpError(w, 409, "Another migration phase is running. Refresh its status before continuing.")
		return
	}
	op.Roots = roots
	if op.SourceMode != "detected" {
		op.RootSelections = append([]webMigrationRootSelection{}, in.Roots...)
	}
	op.Review = nil
	op.Prepared = nil
	s.webMigrationSubmit(w, rt, op, true)
}
func (s *Server) webMigrationPrepareHTTP(w http.ResponseWriter, r *http.Request, rt *webMigrationRuntime, op *webMigrationOperation) {
	var in struct {
		ReviewToken     string `json:"reviewToken"`
		Confirmation    string `json:"confirmation"`
		SourceStopped   bool   `json:"sourceStopped"`
		BackupConfirmed bool   `json:"backupConfirmed"`
	}
	if webMigrationRequestDecode(r, &in) != nil || in.Confirmation != "PREPARE_MIGRATION" || !in.SourceStopped || !in.BackupConfirmed {
		httpError(w, 400, "Confirm the reviewed migration, stopped source writers and independent backups.")
		return
	}
	if op.State != "review" || op.Review == nil || !op.Review.ExpiresAt.After(time.Now()) || !s.webMigrationVerify(in.ReviewToken, op, "prepare") {
		httpError(w, 409, "The reviewed plan is expired, already consumed or does not match this account and operation. Inspect again.")
		return
	}
	if rt.active != "" {
		httpError(w, 409, "Another migration phase is running. Refresh its status before continuing.")
		return
	}
	s.webMigrationSubmit(w, rt, op, false)
}
func (s *Server) webMigrationInput(rt *webMigrationRuntime, op *webMigrationOperation, dry bool) webMigrationPrepareInput {
	dir := filepath.Join(rt.root, "operations", op.ID)
	name := "source.json"
	if strings.HasSuffix(strings.ToLower(op.Filename), ".json.gz") {
		name += ".gz"
	}
	in := webMigrationPrepareInput{BootstrapConfig: s.webMigrationConfig(), BootstrapDataDir: s.webMigrationBootstrap(), SourcePath: filepath.Join(dir, name), SourceKind: "legacy", ExpectedSnapshotSHA256: op.SHA256, TargetID: op.ID, SessionAudience: op.SessionAudience, Roots: append([]migrate.RootMapping(nil), op.Roots...), DryRun: dry, SourceQuiesced: !dry}
	if op.HasConfig {
		in.LegacyConfigPath = filepath.Join(dir, "legacy-config.yml")
	}
	if !dry && op.Review != nil {
		in.ExpectedProofSHA256 = op.Review.ProofSHA256
	}
	return in
}
func (s *Server) webMigrationSubmit(w http.ResponseWriter, rt *webMigrationRuntime, op *webMigrationOperation, dry bool) {
	phase := "preparing"
	message := "Preparing a fresh isolated SQLite target"
	if dry {
		phase = "inspecting"
		message = "Inspecting snapshot and mapped files"
	}
	op.State = phase
	op.TicketRevision++
	op.Error = ""
	op.ErrorCode = ""
	op.JobID = ""
	op.CancelRequested = false
	op.Progress = webMigrationProgress{1, message}
	rt.active = op.ID
	if rt.save(op) != nil {
		rt.active = ""
		op.State = "uncertain"
		httpError(w, 503, "Cannot persist migration admission; refresh status before another action.")
		return
	}
	in := s.webMigrationInput(rt, op, dry)
	configDigest := op.LegacyConfigSHA256
	uploadLimit := s.webMigrationUploadLimit()
	if op.SourceMode == "detected" {
		uploadLimit = webMigrationDetectedMaxBytes
	}
	id, e := s.Jobs.SubmitWithOptions("web_migration_"+phase, map[string]string{"operationId": op.ID}, func(ctx context.Context, progress func(int, string)) (any, error) {
		progress(5, message)
		var result webMigrationPrepareResult
		var err error
		if in.LegacyConfigPath != "" {
			raw, readErr := webMigrationRead(in.LegacyConfigPath, 1<<20)
			hash := sha256.Sum256(raw)
			if readErr != nil || hex.EncodeToString(hash[:]) != configDigest {
				err = errors.New("private configuration upload changed")
			}
		}
		if err == nil {
			result, err = webMigrationPrepare(ctx, in)
		}
		// A failed read-only inspection may be corrected explicitly on the same
		// immutable upload. Recheck both byte identities and absence of retained
		// publication/staging before returning the operation to its editable state.
		editableInspection := false
		if dry && err != nil && ctx.Err() == nil {
			target := filepath.Join(rt.root, "targets", in.TargetID)
			_, targetErr := os.Lstat(target)
			_, stageErr := os.Lstat(target + ".staging")
			editableInspection = os.IsNotExist(targetErr) && os.IsNotExist(stageErr) && webMigrationUploadUnchanged(ctx, in.SourcePath, in.ExpectedSnapshotSHA256, uploadLimit)
			if editableInspection && in.LegacyConfigPath != "" {
				editableInspection = webMigrationUploadUnchanged(ctx, in.LegacyConfigPath, configDigest, min(uploadLimit, 1<<20))
			}
		}
		rt.mu.Lock()
		defer rt.mu.Unlock()
		defer func() {
			if rt.active == op.ID {
				rt.active = ""
			}
		}()
		if err == nil && (ctx.Err() != nil || op.CancelRequested) {
			err = context.Canceled
		}
		if err != nil {
			op.State = "failed"
			op.Error = "Migration " + phase + " failed verification. Check snapshot format, complete root mappings, stopped source writers and available disk space; upload a new operation after correcting the source."
			if err.Error() == webMigrationAuthenticationError {
				op.ErrorCode = "authCompatibility"
				op.Error = webMigrationAuthenticationError + ". Upload a new operation with the matching original legacy configuration."
			}
			if ctx.Err() != nil || op.CancelRequested {
				op.State = "canceled"
				op.Error = "Migration canceled or interrupted; refresh status to reconcile any completed target."
			} else if editableInspection {
				op.State = "uploaded"
				op.Review, op.Prepared = nil, nil
				op.ErrorCode = "inspectionNeedsCorrection"
				op.Error = "Inspection did not complete. Check the root mappings, referenced copy files and optional legacy configuration, then explicitly inspect this same upload again. A different snapshot requires a new operation."
				if err.Error() == webMigrationAuthenticationError {
					op.ErrorCode = "authCompatibility"
					op.Error = webMigrationAuthenticationError + ". Add the matching original legacy configuration if it has not been uploaded, then explicitly inspect again. A different snapshot or replacement YAML requires a new operation."
				}
			}
			op.Progress = webMigrationProgress{0, op.Error}
			s.webMigrationReconcile(context.Background(), rt, op)
			if rt.save(op) != nil {
				op.State = "uncertain"
			}
			return nil, errors.New("Migration " + phase + " did not complete; inspect the operation status before any further action")
		}
		if dry {
			op.Review = webMigrationReviewOf(result.Receipt, result.ProofSHA256)
			op.Review.TargetAdminUsername = result.RuntimeConfig.AdminUsername
			if op.Review.TargetAdminUsername == "" {
				op.Review.TargetAdminUsername = "admin"
			}
			op.Review.TargetTimezone = result.RuntimeConfig.Timezone
			op.Review.Warnings = result.Summary.Warnings
			op.Review.SourceDBType = result.Summary.SourceDBType
			op.State = "review"
			op.Progress = webMigrationProgress{100, "Inspection complete; review the verified plan"}
		} else {
			op.State = "uncertain"
			s.webMigrationReconcile(context.Background(), rt, op)
			if op.State != "prepared" {
				op.Error = "Preparation returned but published evidence could not be reconciled; refresh status."
			}
		}
		if rt.save(op) != nil {
			op.State = "uncertain"
			op.Error = "Operation result persistence is uncertain; refresh status to reconcile."
			return nil, errors.New("Migration status persistence is uncertain")
		}
		progress(100, op.Progress.Message)
		return nil, nil // Generic task history must not expose the private operation identity.
	}, job.SubmitOptions{Title: "Migration " + phase, QueueType: "management", UniqueKey: "web-migration-active"})
	if e != nil {
		rt.active = ""
		op.State = "failed"
		op.Error = "Migration job admission failed. Upload a new operation after resolving job queue availability; this request is not replayed."
		_ = rt.save(op)
		httpError(w, 503, op.Error)
		return
	}
	op.JobID = id
	if rt.save(op) != nil {
		op.Error = "Job accepted but record persistence is uncertain; refresh status. Do not replay this request."
	}
	writeJSON(w, 202, s.webMigrationView(op))
}

func webMigrationUploadUnchanged(ctx context.Context, path, expected string, limit int64) bool {
	if limit < 1 || !webMigrationHexText(expected, 64) {
		return false
	}
	root, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		return false
	}
	defer root.Close()
	name := filepath.Base(path)
	before, err := root.Lstat(name)
	if err != nil || !before.Mode().IsRegular() || before.Size() < 1 || before.Size() > limit {
		return false
	}
	f, err := root.OpenFile(name, regularReadFlags, 0)
	if err != nil {
		return false
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(before, opened) {
		return false
	}
	hash := sha256.New()
	n, err := backupCopy(ctx, hash, f, limit)
	if err != nil || n != before.Size() {
		return false
	}
	after, err := f.Stat()
	if err != nil || after.Size() != n || !after.ModTime().Equal(before.ModTime()) {
		return false
	}
	named, err := root.Lstat(name)
	return err == nil && named.Mode().IsRegular() && os.SameFile(before, named) && hex.EncodeToString(hash.Sum(nil)) == expected
}
func (s *Server) webMigrationRefresh(ctx context.Context, rt *webMigrationRuntime, op *webMigrationOperation) {
	if webMigrationRunning(op.State) && op.State != "activating" && op.JobID != "" {
		if task, e := s.Jobs.GetContext(ctx, op.JobID); e == nil && job.Terminal(task.Status) {
			op.State = "uncertain"
			if op.CancelRequested {
				op.State = "canceled"
			}
			op.Error = "The migration job stopped. Refresh status to reconcile the generated target; do not replay the request."
			if rt.active == op.ID {
				rt.active = ""
			}
		}
	}
	s.webMigrationReconcile(ctx, rt, op)
}
func (s *Server) webMigrationCancel(w http.ResponseWriter, r *http.Request, rt *webMigrationRuntime, op *webMigrationOperation) {
	var in struct{}
	if webMigrationRequestDecode(r, &in) != nil {
		httpError(w, 400, "Cancellation requires an empty JSON object.")
		return
	}
	if op.State == "activating" || op.State == "active" || op.State == "prepared" || op.State == "uncertain" {
		httpError(w, 409, "This operation may already have a prepared or active target; cancellation is unavailable. Refresh status.")
		return
	}
	op.CancelRequested = true
	if webMigrationRunning(op.State) && op.JobID != "" {
		if e := s.Jobs.Cancel(op.JobID); e != nil {
			httpError(w, 409, "Cancellation could not be confirmed. Refresh status before another action.")
			return
		}
		op.Progress.Message = "Cancellation requested; awaiting outcome reconciliation"
	} else {
		op.State = "canceled"
		op.Progress = webMigrationProgress{0, "Canceled"}
	}
	if op.State != "preparing" {
		op.Review = nil
	}
	if rt.save(op) != nil {
		httpError(w, 503, "Cancellation persistence is uncertain; refresh status.")
		return
	}
	writeJSON(w, 202, s.webMigrationView(op))
}
func (s *Server) webMigrationApply(w http.ResponseWriter, r *http.Request, rt *webMigrationRuntime, op *webMigrationOperation) {
	var in struct {
		ApplyToken       string `json:"applyToken"`
		Confirmation     string `json:"confirmation"`
		BackupConfirmed  bool   `json:"backupConfirmed"`
		ReloginConfirmed bool   `json:"reloginConfirmed"`
	}
	if webMigrationRequestDecode(r, &in) != nil || in.Confirmation != "APPLY_MIGRATED_LIBRARY" || !in.BackupConfirmed || !in.ReloginConfirmed {
		httpError(w, 400, "Confirm activation, retained backups and signing in again with migrated credentials.")
		return
	}
	if op.State != "prepared" || op.Prepared == nil || !s.webMigrationVerify(in.ApplyToken, op, "apply") {
		httpError(w, 409, "Activation review is expired, consumed or does not match this account and operation. Refresh status.")
		return
	}
	available, reason := s.webMigrationActivationStatus()
	if !available {
		httpError(w, 409, reason)
		return
	}
	if rt.active != "" {
		httpError(w, 409, "Another migration phase is running.")
		return
	}
	op.State = "activating"
	op.ActivationAttempted = true
	op.TicketRevision++
	op.Progress = webMigrationProgress{95, "Activation accepted; reconnect and sign in again"}
	rt.active = op.ID
	if rt.save(op) != nil {
		rt.active = ""
		op.State = "uncertain"
		httpError(w, 503, "Cannot persist activation admission; refresh status.")
		return
	}
	_, e := s.MigrationActivation.ActivateVerified(r.Context(), op.Prepared.TargetDir, op.Prepared.ConfigSHA256, op.Prepared.ReceiptSHA256)
	if e != nil {
		rt.active = ""
		op.State = "uncertain"
		op.Error = "Activation could not be confirmed. Refresh status; do not replay the request."
		_ = rt.save(op)
		httpError(w, 409, op.Error)
		return
	}
	writeJSON(w, 202, s.webMigrationView(op))
}

func webMigrationRequestDecode(r *http.Request, v any) error {
	raw, e := io.ReadAll(io.LimitReader(r.Body, 64<<10+1))
	if e != nil || len(raw) > 64<<10 {
		return errors.New("migration request exceeds limit")
	}
	if trimmed := bytes.TrimSpace(raw); len(trimmed) == 0 || trimmed[0] != '{' {
		return errors.New("migration request must be a JSON object")
	}
	return webMigrationDecode(raw, v)
}
