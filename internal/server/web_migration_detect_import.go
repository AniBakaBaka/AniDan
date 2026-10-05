// SPDX-License-Identifier: AGPL-3.0-only
package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/AniBakaBaka/AniDan/internal/job"
	"github.com/AniBakaBaka/AniDan/internal/migrate"
	"gopkg.in/yaml.v3"
)

func (s *Server) webMigrationDetectedImport(w http.ResponseWriter, r *http.Request, rt *webMigrationRuntime, actor string) {
	var in struct {
		CandidateID                string `json:"candidateId"`
		SelectionToken             string `json:"selectionToken"`
		Confirmation               string `json:"confirmation"`
		SourceStopped              bool   `json:"sourceStopped"`
		BackupConfirmed            bool   `json:"backupConfirmed"`
		EffectiveSettingsConfirmed bool   `json:"effectiveSettingsConfirmed"`
	}
	if webMigrationRequestDecode(r, &in) != nil || !webMigrationHexText(in.CandidateID, 64) || in.Confirmation != "READ_DETECTED_LEGACY" || !in.SourceStopped || !in.BackupConfirmed || !in.EffectiveSettingsConfirmed {
		httpError(w, 400, "Select the detected original and confirm its effective settings, stopped database/file writers, and independent backup.")
		return
	}
	if rt.active != "" || len(rt.ops)+rt.orphans >= webMigrationMaxOperations {
		httpError(w, 409, "Another migration is running or private operation storage is full. Read migration history before continuing.")
		return
	}
	selection := sha256.Sum256([]byte(in.SelectionToken))
	selectionDigest := hex.EncodeToString(selection[:])
	for _, op := range rt.ops {
		if op.Actor == actor && op.DetectedSelection == selectionDigest {
			httpError(w, 409, "This detection approval was already used. Read migration history; do not replay the source capture.")
			return
		}
	}
	candidates, _ := s.webMigrationDiscover(r.Context())
	var selected *webMigrationDetectedCandidate
	for i := range candidates {
		if candidates[i].view.ID == in.CandidateID {
			selected = &candidates[i]
			break
		}
	}
	if selected == nil || !selected.view.Available || !s.webMigrationCandidateVerify(actor, *selected, in.SelectionToken) {
		httpError(w, 409, "The detected installation changed, expired or belongs to another account. Detect it again before reading the source.")
		return
	}
	roots := []migrate.RootMapping{{From: "/app/config", To: selected.configDir}, {From: "config", To: selected.configDir}}
	if selected.configDir != "/app/config" {
		roots = append(roots, migrate.RootMapping{From: selected.configDir, To: selected.configDir})
	}
	var err error
	roots, err = webMigrationValidateRoots(roots, s.webMigrationConfig().ReadRoots, s.webMigrationBootstrap())
	if err != nil {
		httpError(w, 409, "The detected asset directory is no longer an allowed, separate original source.")
		return
	}
	privateConfig, err := webMigrationDetectedRuntimeYAML(selected.settings.EffectiveYAML)
	if err != nil {
		httpError(w, 409, "Original authentication settings could not be captured safely.")
		return
	}
	id, err := webMigrationRandom(16)
	if err != nil {
		httpError(w, 503, "Migration identity generation is unavailable.")
		return
	}
	audience, err := webMigrationRandom(32)
	if err != nil {
		httpError(w, 503, "Migration identity generation is unavailable.")
		return
	}
	dir := filepath.Join(rt.root, "operations", id)
	if err = os.Mkdir(dir, 0700); err != nil {
		httpError(w, 503, "Private migration capture storage is unavailable.")
		return
	}
	initial := strings.Repeat("0", 64)
	op := &webMigrationOperation{ID: id, Actor: actor, SessionAudience: audience, State: "failed", Filename: "Detected original installation", SHA256: initial, CreatedAt: time.Now().UTC(), SourceMode: "detected", SourceDisplay: &selected.view.webMigrationSourceDisplay, DetectedSelection: selectionDigest, Roots: roots, Error: "Original source capture has not completed. Do not replay an interrupted request.", ErrorCode: "captureIncomplete"}
	rel, _ := filepath.Rel(selected.root.Path, selected.configDir)
	for _, root := range roots {
		op.RootSelections = append(op.RootSelections, webMigrationRootSelection{From: root.From, RootID: selected.root.ID, Path: filepath.ToSlash(rel)})
	}
	rt.ops[id] = op
	if err = rt.save(op); err != nil {
		httpError(w, 503, "Cannot persist capture ownership. Read migration history before starting another capture.")
		return
	}
	if err = webMigrationWriteDetectedConfig(dir, privateConfig); err != nil {
		httpError(w, 503, "Original authentication capture did not complete. Retained private files require a new operation.")
		return
	}
	digest := sha256.Sum256(privateConfig)
	op.HasConfig, op.LegacyConfigSHA256 = true, hex.EncodeToString(digest[:])
	op.State, op.Error, op.ErrorCode = "inspecting", "", ""
	op.Progress = webMigrationProgress{1, "Reading the explicitly selected original SQL database without changing its data"}
	op.TicketRevision++
	rt.active = id
	if err = rt.save(op); err != nil {
		rt.active = ""
		op.State = "uncertain"
		httpError(w, 503, "Capture admission persistence is uncertain. Read migration history.")
		return
	}
	chosen := *selected
	config := chosen.settings
	source := migrate.DetectedSourceConfig{Driver: config.Driver, Host: config.Host, Port: config.Port, User: config.User, Password: config.Password, Database: config.Database}
	if config.Driver == "postgres" {
		source.Schema = "public"
	}
	prepareInput := s.webMigrationInput(rt, op, true)
	options := migrate.DetectedExportOptions{MaxBytes: webMigrationDetectedMaxBytes, MaxRowBytes: s.webMigrationConfig().MigrationReview.Options().MaxRowBytes, Timeout: webMigrationDetectedTimeout}
	jobID, err := s.Jobs.SubmitWithOptions("web_migration_detected", map[string]string{"operationId": id}, func(ctx context.Context, progress func(int, string)) (any, error) {
		// Reopen the same selected configuration immediately before the single
		// source capture. A queued request cannot silently adopt edited settings.
		current := s.webMigrationLoadCandidate(ctx, chosen.root, chosen.configRel, chosen.composeRel)
		var result webMigrationPrepareResult
		var provenance migrate.DetectedSourceProvenance
		var captureErr error
		if !current.view.Available || current.view.ID != chosen.view.ID || !bytes.Equal(current.config, chosen.config) || !bytes.Equal(current.compose, chosen.compose) {
			captureErr = errors.New("detected settings changed before capture")
		}
		if captureErr == nil {
			captureErr = s.webMigrationRejectCurrentSQL(ctx, source)
		}
		if captureErr == nil {
			progress(5, "Capturing one read-only original database snapshot")
			provenance, captureErr = migrate.ExportDetectedSource(ctx, source, prepareInput.SourcePath, options)
		}
		var sum string
		var size int64
		if captureErr == nil {
			sum, size, captureErr = webMigrationDetectedSnapshotDigest(ctx, prepareInput.SourcePath)
		}
		if captureErr == nil {
			prepareInput.ExpectedSnapshotSHA256 = sum
			// Commit capture identity before entering a potentially long asset
			// inspection. A restart can offer explicit reinspection of this exact
			// snapshot without reconnecting to the original database.
			rt.mu.Lock()
			op.SHA256, op.Size, op.CaptureComplete = sum, size, true
			op.SourceDisplay.CapturedAt, op.SourceDisplay.Schema = provenance.CapturedAt, provenance.Schema
			if ctx.Err() != nil || op.CancelRequested {
				captureErr = context.Canceled
			} else if rt.save(op) != nil {
				captureErr = errors.New("capture completion persistence is uncertain")
			}
			rt.mu.Unlock()
		}
		if captureErr == nil {
			progress(35, "Checking original rows, automatically mapped assets and target authentication")
			result, captureErr = webMigrationPrepare(ctx, prepareInput)
		}
		editableInspection := false
		if captureErr != nil && sum != "" && ctx.Err() == nil {
			target := filepath.Join(rt.root, "targets", id)
			_, targetErr := os.Lstat(target)
			_, stageErr := os.Lstat(target + ".staging")
			editableInspection = os.IsNotExist(targetErr) && os.IsNotExist(stageErr) && webMigrationUploadUnchanged(ctx, prepareInput.SourcePath, sum, webMigrationDetectedMaxBytes) && webMigrationUploadUnchanged(ctx, prepareInput.LegacyConfigPath, hex.EncodeToString(digest[:]), 1<<20)
		}
		rt.mu.Lock()
		defer rt.mu.Unlock()
		defer func() {
			if rt.active == id {
				rt.active = ""
			}
		}()
		if sum != "" {
			op.SHA256, op.Size = sum, size
			op.SourceDisplay.CapturedAt, op.SourceDisplay.Schema = provenance.CapturedAt, provenance.Schema
		}
		if captureErr == nil && (ctx.Err() != nil || op.CancelRequested) {
			captureErr = context.Canceled
		}
		if captureErr != nil {
			op.State, op.ErrorCode = "failed", "detectedCaptureFailed"
			op.Error = "Detected-source reading or verification did not complete. Check the selected endpoint's reachability and read permissions, supported original schema, complete mapped files and effective authentication settings. Source data was not modified. Retained captures are not automatically retried."
			if editableInspection {
				// A complete private capture may be inspected again explicitly;
				// this never recontacts SQL or replaces the captured source.json.
				op.State, op.ErrorCode = "uploaded", "inspectionNeedsCorrection"
				op.Error = "The original SQL snapshot was captured, but file/authentication verification did not complete. Restore missing files within the detected directory and explicitly inspect this captured snapshot again; changed settings require a new detection."
			}
			if ctx.Err() != nil || op.CancelRequested {
				op.State, op.ErrorCode, op.Error = "canceled", "captureCanceled", "Original source capture was canceled or interrupted. Read status; do not replay the request."
			}
			op.Progress = webMigrationProgress{0, op.Error}
			if rt.save(op) != nil {
				op.State = "uncertain"
			}
			return nil, errors.New("Detected original migration did not complete; inspect the private operation status")
		}
		op.Review = webMigrationReviewOf(result.Receipt, result.ProofSHA256)
		op.Review.TargetAdminUsername, op.Review.TargetTimezone = result.RuntimeConfig.AdminUsername, result.RuntimeConfig.Timezone
		if op.Review.TargetAdminUsername == "" {
			op.Review.TargetAdminUsername = "admin"
		}
		op.Review.Warnings = append(result.Summary.Warnings, "This is the original database snapshot captured at the displayed time; later original database writes are not imported. Keep original database and file writers stopped until cutover.")
		op.Review.SourceDBType = result.Summary.SourceDBType
		op.State, op.ErrorCode, op.Error = "review", "", ""
		op.Progress = webMigrationProgress{100, "Detected original captured and verified; review before preparing the new library"}
		if rt.save(op) != nil {
			op.State = "uncertain"
			return nil, errors.New("Detected migration result persistence is uncertain")
		}
		progress(100, op.Progress.Message)
		return nil, nil
	}, job.SubmitOptions{Title: "Inspect detected original installation", QueueType: "management", UniqueKey: "web-migration-active"})
	if err != nil {
		rt.active, op.State, op.ErrorCode = "", "failed", "captureAdmissionFailed"
		op.Error = "Original-source capture was not admitted. Read history and resolve job availability before explicitly creating a new detection."
		_ = rt.save(op)
		httpError(w, 503, op.Error)
		return
	}
	op.JobID = jobID
	if rt.save(op) != nil {
		op.Error = "Capture accepted but status persistence is uncertain. Read history; do not replay the request."
	}
	writeJSON(w, 202, s.webMigrationView(op))
}

func webMigrationCapturedInspectionRecoverable(workspace string, op *webMigrationOperation) bool {
	if op.SourceMode != "detected" || !op.CaptureComplete || op.CancelRequested || op.ActivationAttempted || !webMigrationHexText(op.SHA256, 64) || op.SHA256 == strings.Repeat("0", 64) || op.Size < 1 || op.Size > webMigrationDetectedMaxBytes || !op.HasConfig || !webMigrationHexText(op.LegacyConfigSHA256, 64) {
		return false
	}
	target := filepath.Join(workspace, "targets", op.ID)
	for _, path := range []string{target, target + ".staging"} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			return false
		}
	}
	dir := filepath.Join(workspace, "operations", op.ID)
	return webMigrationRegularFile(filepath.Join(dir, "source.json"), webMigrationDetectedMaxBytes) == nil && webMigrationRegularFile(filepath.Join(dir, "legacy-config.yml"), 1<<20) == nil
}

// Only settings needed by target authentication/timezone/cache translation are
// persisted. Source DB credentials, raw Compose and plaintext bootstrap admin
// passwords are used only in memory and never copied into operation YAML.
func webMigrationDetectedRuntimeYAML(raw []byte) ([]byte, error) {
	var in map[string]any
	if err := yaml.Unmarshal(raw, &in); err != nil {
		return nil, err
	}
	out := map[string]any{}
	for _, key := range []string{"jwt", "tz"} {
		if value, ok := in[key]; ok {
			out[key] = value
		}
	}
	if admin, ok := in["admin"].(map[string]any); ok {
		if user, exists := admin["initial_user"]; exists {
			out["admin"] = map[string]any{"initial_user": user}
		}
	}
	if cache, ok := in["cache"].(map[string]any); ok {
		copy := map[string]any{}
		for key, value := range cache {
			if key != "redis_url" {
				copy[key] = value
			}
		}
		out["cache"] = copy
	}
	return yaml.Marshal(out)
}
func webMigrationWriteDetectedConfig(dir string, raw []byte) error {
	if len(raw) < 1 || len(raw) > 1<<20 {
		return errors.New("invalid private migration configuration")
	}
	f, err := os.OpenFile(filepath.Join(dir, "legacy-config.yml"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	_, writeErr := f.Write(raw)
	return errors.Join(writeErr, f.Sync(), f.Close())
}
func webMigrationDetectedSnapshotDigest(ctx context.Context, name string) (string, int64, error) {
	if err := webMigrationRegularFile(name, webMigrationDetectedMaxBytes); err != nil {
		return "", 0, err
	}
	root, err := os.OpenRoot(filepath.Dir(name))
	if err != nil {
		return "", 0, err
	}
	defer root.Close()
	f, err := root.OpenFile(filepath.Base(name), regularReadFlags, 0)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := backupCopy(ctx, h, f, webMigrationDetectedMaxBytes)
	if err != nil {
		return "", 0, err
	}
	sum := hex.EncodeToString(h.Sum(nil))
	if !webMigrationUploadUnchanged(ctx, name, sum, webMigrationDetectedMaxBytes) {
		return "", 0, errors.New("captured snapshot changed")
	}
	return sum, n, nil
}

// Reject an exactly known current SQL endpoint/database before opening the
// detected connection. This does not claim to identify every DNS alias or a
// physical server UUID; source schema/native markers are checked separately.
func (s *Server) webMigrationRejectCurrentSQL(ctx context.Context, source migrate.DetectedSourceConfig) error {
	if s.Store == nil || s.Store.Dialect != source.Driver {
		return nil
	}
	current, ok := s.Store.RemoteEndpointSHA256()
	if !ok {
		return errors.New("current SQL endpoint identity cannot be verified")
	}
	parts := []string{"postgres", strings.ToLower(source.Host), strconv.Itoa(source.Port)}
	query := "SELECT current_database()"
	if source.Driver == "mysql" {
		parts = []string{"mysql", "tcp", net.JoinHostPort(strings.ToLower(source.Host), strconv.Itoa(source.Port))}
		query = "SELECT DATABASE()"
	}
	raw, _ := json.Marshal(parts)
	h := sha256.Sum256(raw)
	if hex.EncodeToString(h[:]) != current {
		return nil
	}
	var database string
	if err := s.Store.DB.QueryRowContext(ctx, query).Scan(&database); err != nil {
		return errors.New("current SQL database identity cannot be verified")
	}
	if database == source.Database {
		return errors.New("detected original is the current AniDan SQL database")
	}
	return nil
}
