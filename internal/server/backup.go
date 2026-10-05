// SPDX-License-Identifier: AGPL-3.0-or-later
package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/AniBakaBaka/AniDan/internal/migrate"
	"github.com/AniBakaBaka/AniDan/internal/store"
)

var backupFilenameRE = regexp.MustCompile(`^(?:danmuapi|anidan)_backup_[0-9]{8}_[0-9]{6}(?:_[a-zA-Z0-9]{3,32})?\.(?:json|tar)\.gz$`)

func (s *Server) registerBackups(m *http.ServeMux) {
	m.HandleFunc("GET /api/ui/backup/list", s.operator(s.backupsList))
	m.HandleFunc("POST /api/ui/backup/create", s.operator(s.backupCreate))
	m.HandleFunc("GET /api/ui/backup/download/{filename}", s.backupDownloadAuth)
	m.HandleFunc("DELETE /api/ui/backup/delete/{filename}", s.operator(s.backupDelete))
	m.HandleFunc("DELETE /api/ui/backup/delete-batch", s.operator(s.backupDeleteBatch))
	m.HandleFunc("POST /api/ui/backup/upload", s.operator(s.backupUpload))
	m.HandleFunc("GET /api/ui/backup/detail/{filename}", s.operator(s.backupDetail))
	m.HandleFunc("POST /api/ui/backup/dry-run", s.operator(s.backupDryRun))
	m.HandleFunc("POST /api/ui/backup/restore", s.operator(s.backupRestore))
	m.HandleFunc("GET /api/ui/backup/config", s.operator(s.backupConfig))
	m.HandleFunc("GET /api/ui/backup/job-status", s.operator(s.backupJobStatus))
}
func (s *Server) backupDirectory() (string, error) {
	root, err := filepath.Abs(s.DataDir)
	if err != nil {
		return "", err
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	row, readErr := s.Store.Get(ctx, "config", "backupPath")
	if readErr != nil && !errors.Is(readErr, sql.ErrNoRows) {
		return "", readErr
	}
	dir := ""
	if row != nil {
		dir = str(row["config_value"])
	}
	if dir == "" {
		dir = filepath.Join(root, "sql_backup")
	} else {
		dir = s.normalizeStoredPath(dir)
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	if !backupWithin(root, abs) {
		return "", errors.New("backupPath must be inside the configured AniDan data directory")
	}
	if err = s.guardWebMigrationPath(abs); err != nil {
		return "", err
	}
	rel, err := filepath.Rel(root, abs)
	if err != nil {
		return "", err
	}
	guard, err := os.OpenRoot(root)
	if err != nil {
		return "", err
	}
	defer guard.Close()
	if err = guard.MkdirAll(rel, 0700); err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil || !backupWithin(root, resolved) {
		return "", errors.New("backup directory escapes the configured data directory")
	}
	return resolved, nil
}
func (s *Server) backupFile(name string) (string, error) {
	if !backupFilenameRE.MatchString(name) || filepath.Base(name) != name {
		return "", errors.New("invalid backup filename")
	}
	dir, err := s.backupDirectory()
	if err != nil {
		return "", err
	}
	p := filepath.Join(dir, name)
	info, err := os.Lstat(p)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", errors.New("backup is not a regular file")
	}
	return p, nil
}
func (s *Server) backupRetention(ctx context.Context) (int, error) {
	row, err := s.Store.Get(ctx, "config", "backupRetentionCount")
	if errors.Is(err, sql.ErrNoRows) || (err == nil && row == nil) {
		return 5, nil
	}
	if err != nil {
		return 0, err
	}
	n, err := strconv.Atoi(str(row["config_value"]))
	if err != nil || n < 0 || n > 1000 {
		return 0, errors.New("backupRetentionCount must be 0..1000; zero disables pruning")
	}
	return n, nil
}

func backupAPIError(w http.ResponseWriter, err error) {
	status := 400
	if os.IsNotExist(err) {
		status = 404
	} else if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		status = 408
	}
	httpError(w, status, err.Error())
}
func infoFromReceipt(filename, hash string, size int64, receipt migrate.Receipt, full bool) backupInfo {
	counts := map[string]int64{}
	var total int64
	for k, v := range receipt.Tables {
		counts[k] = v.Rows
		total += v.Rows
	}
	version := "2.0"
	if full {
		version = "anidan-library-v1"
	}
	fileCount := 0
	for _, file := range receipt.Files {
		if file.Destination != "config.json" {
			fileCount++ // the private runtime configuration is not a library file
		}
	}
	return backupInfo{Filename: filename, Size: size, CreatedAt: receipt.SourceCreatedAt, DBType: receipt.SourceDBType, SHA256: hash, TotalRecords: total, Version: version, ContainsFiles: full, ContainsSecrets: true, FileCount: fileCount, TableRecords: counts}
}
func (s *Server) inspectBackup(ctx context.Context, p string) (backupInfo, error) {
	prepared, err := s.prepareBackup(ctx, p, true, "")
	if err != nil {
		return backupInfo{}, err
	}
	defer prepared.cleanup()
	hash, size, err := backupHash(ctx, p)
	if err != nil {
		return backupInfo{}, err
	}
	return infoFromReceipt(filepath.Base(p), hash, size, prepared.receipt, prepared.manifest != nil), nil
}
func (s *Server) createFullBackup(ctx context.Context, progress func(int, string)) (backupInfo, error) {
	for !s.backupMu.TryLock() {
		select {
		case <-ctx.Done():
			return backupInfo{}, ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
	defer s.backupMu.Unlock()
	s.fileMu.RLock()
	defer s.fileMu.RUnlock()
	retention, err := s.backupRetention(ctx)
	if err != nil {
		return backupInfo{}, err
	}
	if progress == nil {
		progress = func(int, string) {}
	}
	dir, err := s.backupDirectory()
	if err != nil {
		return backupInfo{}, err
	}
	temp, err := os.MkdirTemp("", "anidan-backup-")
	if err != nil {
		return backupInfo{}, err
	}
	defer os.RemoveAll(temp)
	progress(10, "Exporting consistent database snapshot")
	snapshot := filepath.Join(temp, "database.json")
	if err = migrate.Export(ctx, s.Config.Driver, s.Config.DSN, snapshot); err != nil {
		return backupInfo{}, err
	}
	roots, err := s.backupRoots()
	if err != nil {
		return backupInfo{}, err
	}
	progress(30, "Verifying schema, references and file checksums")
	receipt, err := migrate.Run(ctx, migrate.Config{Snapshot: snapshot, TargetDir: filepath.Join(temp, "preflight"), Roots: roots, DryRun: true, MaxBytes: maxBackupBytes})
	if err != nil {
		return backupInfo{}, err
	}
	manifest, err := makeBundleManifest(receipt, roots)
	if err != nil {
		return backupInfo{}, err
	}
	if err = s.addImportPayloads(ctx, &manifest); err != nil {
		return backupInfo{}, err
	}
	cfg := s.Config
	cfg.AdminPassword = ""
	cfg.DSN = "" // credentials for the old SQL server are not needed to restore the SQLite snapshot
	cfg.Cache = cfg.Cache.WithoutRemote()
	if err = cfg.Cache.Validate(); err != nil {
		return backupInfo{}, err
	}
	manifest.Warnings = append(manifest.Warnings, restoreCacheIsolationWarning)
	runtime := filepath.Join(temp, "runtime-config.json")
	if err = backupJSONFile(runtime, cfg); err != nil {
		return backupInfo{}, err
	}
	manifest.ConfigSHA256, _, err = backupHash(ctx, runtime)
	if err != nil {
		return backupInfo{}, err
	}
	filename := "anidan_backup_" + time.Now().UTC().Format("20060102_150405") + "_" + randomID()[:12] + ".tar.gz"
	archive := filepath.Join(dir, filename)
	pending := filepath.Join(dir, ".pending-"+filename)
	progress(55, "Copying exact referenced library files into archive")
	if err = writeBundle(ctx, pending, snapshot, runtime, manifest); err != nil {
		return backupInfo{}, err
	}
	keep, published := false, false
	defer func() {
		os.Remove(pending)
		if !keep && published {
			os.Remove(archive)
		}
	}()
	progress(85, "Verifying the completed compressed archive")
	info, err := s.inspectBackup(ctx, pending)
	if err != nil {
		return backupInfo{}, err
	}
	info.Filename = filename
	// Hard-link publication is atomic and refuses an existing destination. A
	// partial archive never matches the listing filename allowlist.
	if err = os.Link(pending, archive); err != nil {
		return backupInfo{}, err
	}
	published = true
	if err = os.Remove(pending); err != nil {
		return backupInfo{}, err
	}
	directory, err := os.Open(dir)
	if err != nil {
		return backupInfo{}, err
	}
	err = directory.Sync()
	directory.Close()
	if err != nil {
		return backupInfo{}, err
	}
	if err = backupJSONFile(archive+".info.json", info); err != nil {
		return backupInfo{}, err
	}
	keep = true
	progress(99, "Backup verified")
	// Prune only after successful full reread/import verification. No failure path
	// deletes the last known-good backup. Archive deletion remains root-contained.
	if retention > 0 && ctx.Err() == nil {
		entries, readErr := os.ReadDir(dir)
		if readErr == nil {
			type candidate struct {
				name     string
				modified time.Time
			}
			existing := []candidate{}
			for _, entry := range entries {
				if !backupFilenameRE.MatchString(entry.Name()) || !entry.Type().IsRegular() {
					continue
				}
				st, statErr := entry.Info()
				if statErr != nil {
					continue
				}
				existing = append(existing, candidate{entry.Name(), st.ModTime()})
			}
			sort.Slice(existing, func(i, j int) bool { return existing[i].modified.After(existing[j].modified) })
			for _, old := range existing[min(retention, len(existing)):] {
				if old.name == filename {
					continue
				}
				p := filepath.Join(dir, old.name)
				if removeErr := os.Remove(p); removeErr == nil {
					_ = os.Remove(p + ".info.json")
				}
			}
		}
	}
	return info, nil
}
func (s *Server) backupsList(w http.ResponseWriter, r *http.Request) {
	dir, err := s.backupDirectory()
	if err != nil {
		backupAPIError(w, err)
		return
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		backupAPIError(w, err)
		return
	}
	out := []backupInfo{}
	for _, entry := range entries {
		if !backupFilenameRE.MatchString(entry.Name()) || !entry.Type().IsRegular() {
			continue
		}
		if len(out) >= 500 {
			httpError(w, 413, "Backup directory contains more than 500 archives")
			return
		}
		p := filepath.Join(dir, entry.Name())
		st, err := entry.Info()
		if err != nil {
			backupAPIError(w, err)
			return
		}
		var info backupInfo
		f, e := openWithin(dir, entry.Name()+".info.json")
		if e == nil {
			e = json.NewDecoder(io.LimitReader(f, 1<<20)).Decode(&info)
			f.Close()
		}
		if e != nil || info.Size != st.Size() {
			info, err = s.inspectBackup(r.Context(), p)
			if err != nil {
				backupAPIError(w, fmt.Errorf("%s: %w", entry.Name(), err))
				return
			}
		}
		out = append(out, info)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt > out[j].CreatedAt })
	writeJSON(w, 200, out)
}
func (s *Server) backupCreate(w http.ResponseWriter, r *http.Request) {
	info, err := s.createFullBackup(r.Context(), nil)
	if err != nil {
		backupAPIError(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"success": true, "message": "Verified database and referenced-file library backup created; archive contains secrets", "filename": info.Filename, "size": info.Size, "records": info.TotalRecords, "sha256": info.SHA256, "contains_files": true, "contains_secrets": true, "file_count": info.FileCount})
}
func (s *Server) backupDownloadAuth(w http.ResponseWriter, r *http.Request) {
	copy := r.Clone(r.Context())
	if token := r.URL.Query().Get("token"); token != "" {
		copy.Header = r.Header.Clone()
		copy.Header.Set("Authorization", "Bearer "+token)
	}
	s.operator(s.backupDownload)(w, copy)
}
func (s *Server) backupDownload(w http.ResponseWriter, r *http.Request) {
	p, err := s.backupFile(r.PathValue("filename"))
	if err != nil {
		backupAPIError(w, err)
		return
	}
	f, err := openWithin(filepath.Dir(p), filepath.Base(p))
	if err != nil {
		backupAPIError(w, err)
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		backupAPIError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("Content-Disposition", `attachment; filename="`+filepath.Base(p)+`"`)
	http.ServeContent(w, r, st.Name(), st.ModTime(), f)
}
func (s *Server) removeBackup(name string) error {
	p, err := s.backupFile(name)
	if err != nil {
		return err
	}
	s.backupMu.Lock()
	defer s.backupMu.Unlock()
	root, err := os.OpenRoot(filepath.Dir(p))
	if err != nil {
		return err
	}
	defer root.Close()
	if err = root.Remove(name); err != nil {
		return err
	}
	_ = root.Remove(name + ".info.json")
	return nil
}
func (s *Server) backupDelete(w http.ResponseWriter, r *http.Request) {
	if err := s.removeBackup(r.PathValue("filename")); err != nil {
		backupAPIError(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"success": true, "message": "Backup deleted"})
}
func (s *Server) backupDeleteBatch(w http.ResponseWriter, r *http.Request) {
	names := r.URL.Query()["filenames"]
	if len(names) == 0 || len(names) > 100 {
		httpError(w, 400, "Provide 1..100 filenames")
		return
	}
	deleted := []string{}
	failures := []map[string]string{}
	for _, name := range names {
		if err := s.removeBackup(name); err != nil {
			failures = append(failures, map[string]string{"filename": name, "error": err.Error()})
		} else {
			deleted = append(deleted, name)
		}
	}
	writeJSON(w, 200, map[string]any{"success": len(failures) == 0, "deleted": deleted, "errors": failures})
}
func (s *Server) backupUpload(w http.ResponseWriter, r *http.Request) {
	reader, err := r.MultipartReader()
	if err != nil {
		backupAPIError(w, err)
		return
	}
	part, err := reader.NextPart()
	if err != nil {
		backupAPIError(w, err)
		return
	}
	defer part.Close()
	if part.FormName() != "file" {
		httpError(w, 400, "Multipart field must be named file")
		return
	}
	ext := ""
	if strings.HasSuffix(part.FileName(), ".tar.gz") {
		ext = ".tar.gz"
	} else if strings.HasSuffix(part.FileName(), ".json.gz") {
		ext = ".json.gz"
	}
	if ext == "" {
		httpError(w, 400, "Upload a .tar.gz full bundle or .json.gz legacy snapshot")
		return
	}
	dir, err := s.backupDirectory()
	if err != nil {
		backupAPIError(w, err)
		return
	}
	filename := "anidan_backup_" + time.Now().UTC().Format("20060102_150405") + "_" + randomID()[:12] + ext
	p := filepath.Join(dir, filename)
	f, err := os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		backupAPIError(w, err)
		return
	}
	keep := false
	defer func() {
		f.Close()
		if !keep {
			os.Remove(p)
		}
	}()
	_, err = backupCopy(r.Context(), f, part, min(s.Config.MaxBodyBytes, maxBackupBytes))
	if err == nil {
		err = f.Sync()
	}
	f.Close()
	if err != nil {
		backupAPIError(w, err)
		return
	}
	if _, err = reader.NextPart(); err != io.EOF {
		httpError(w, 400, "Upload exactly one file")
		return
	}
	info, err := s.inspectBackup(r.Context(), p)
	if err != nil {
		backupAPIError(w, fmt.Errorf("backup validation failed: %w", err))
		return
	}
	if err = backupJSONFile(p+".info.json", info); err != nil {
		backupAPIError(w, err)
		return
	}
	keep = true
	message := "Full library backup uploaded and verified"
	if !info.ContainsFiles {
		message = "Legacy database-only snapshot validated; library files are not included and must remain accessible"
	}
	writeJSON(w, 200, map[string]any{"success": true, "message": message, "filename": filename, "size": info.Size, "contains_files": info.ContainsFiles, "sha256": info.SHA256})
}
func (s *Server) backupDetail(w http.ResponseWriter, r *http.Request) {
	p, err := s.backupFile(r.PathValue("filename"))
	if err != nil {
		backupAPIError(w, err)
		return
	}
	info, err := s.inspectBackup(r.Context(), p)
	if err != nil {
		backupAPIError(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"filename": info.Filename, "size": info.Size, "sha256": info.SHA256, "metadata": map[string]any{"version": info.Version, "source_db_type": info.DBType, "created_at": info.CreatedAt}, "table_records": info.TableRecords, "total_records": info.TotalRecords, "table_count": len(info.TableRecords), "contains_files": info.ContainsFiles, "file_count": info.FileCount, "contains_secrets": true})
}
func (s *Server) backupDryRun(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Filename string   `json:"filename"`
		Tables   []string `json:"tables"`
	}
	if err := readJSON(r, &in); err != nil {
		backupAPIError(w, err)
		return
	}
	p, err := s.backupFile(in.Filename)
	if err != nil {
		backupAPIError(w, err)
		return
	}
	info, err := s.inspectBackup(r.Context(), p)
	if err != nil {
		backupAPIError(w, err)
		return
	}
	selected := map[string]bool{}
	for _, t := range in.Tables {
		if _, ok := store.Schema[t]; !ok {
			httpError(w, 400, "Unknown table "+t)
			return
		}
		selected[t] = true
	}
	comparison := []map[string]any{}
	var currentTotal, backupTotal int64
	for _, name := range store.Tables() {
		if len(selected) > 0 && !selected[name] {
			continue
		}
		current, err := s.Store.Count(r.Context(), name, nil)
		if err != nil {
			backupAPIError(w, err)
			return
		}
		count := info.TableRecords[name]
		comparison = append(comparison, map[string]any{"table": name, "current_count": current, "backup_count": count, "diff": count - current})
		currentTotal += current
		backupTotal += count
	}
	writeJSON(w, 200, map[string]any{"filename": info.Filename, "source_db_type": info.DBType, "backup_version": info.Version, "backup_created_at": info.CreatedAt, "comparison": comparison, "total_backup_records": backupTotal, "total_current_records": currentTotal, "verified": true, "contains_files": info.ContainsFiles, "restore_mode": "verified-staging; explicit restart/cutover required"})
}
func (s *Server) backupRestore(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Filename     string   `json:"filename"`
		Confirm      string   `json:"confirm"`
		Tables       []string `json:"tables"`
		AutoSnapshot bool     `json:"auto_snapshot"`
	}
	if err := readJSON(r, &in); err != nil {
		backupAPIError(w, err)
		return
	}
	if in.Confirm != "RESTORE" {
		httpError(w, 400, "Enter RESTORE to confirm staging this restore")
		return
	}
	if len(in.Tables) > 0 {
		httpError(w, 422, "Partial-table restore is not supported: parent deletion can cascade into unselected data. Restore the complete archive into a staged data root")
		return
	}
	p, err := s.backupFile(in.Filename)
	if err != nil {
		backupAPIError(w, err)
		return
	}
	dataAbs, err := filepath.Abs(s.DataDir)
	if err != nil {
		backupAPIError(w, err)
		return
	}
	parent := filepath.Join(filepath.Dir(dataAbs), filepath.Base(dataAbs)+"-restores")
	if err = os.MkdirAll(parent, 0700); err != nil {
		backupAPIError(w, err)
		return
	}
	target, err := filepath.Abs(filepath.Join(parent, "restore-"+randomID()))
	if err != nil {
		backupAPIError(w, err)
		return
	}
	prepared, err := s.prepareBackup(r.Context(), p, false, target)
	if err != nil {
		backupAPIError(w, err)
		return
	}
	defer prepared.cleanup()
	configFile := filepath.Join(target, "config.json")
	writeJSON(w, 202, map[string]any{"success": true, "staged": true, "activated": false, "recoveryReviewRequired": true, "message": "Verified restore staged in a new data directory. The live database has not changed. Stop this server and restart with the staged config.json to activate; preserve the current data directory for rollback. Restored pending jobs and schedules are blocked until you explicitly review recovery.", "filename": in.Filename, "target_dir": target, "config_file": configFile, "records": func() int64 {
		var n int64
		for _, t := range prepared.receipt.Tables {
			n += t.Rows
		}
		return n
	}(), "receipt": prepared.receipt, "warnings": prepared.receipt.Warnings, "auto_snapshot_needed": false})
}
func (s *Server) backupConfig(w http.ResponseWriter, r *http.Request) {
	dir, err := s.backupDirectory()
	if err != nil {
		backupAPIError(w, err)
		return
	}
	retention, err := s.backupRetention(r.Context())
	if err != nil {
		backupAPIError(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"backup_path": dir, "retention_count": retention, "format": "anidan-library-v1", "restore_mode": "verified-staging"})
}
func (s *Server) backupJobStatus(w http.ResponseWriter, r *http.Request) {
	items, err := s.Jobs.Schedules()
	if err != nil {
		jobHTTPError(w, err)
		return
	}
	for _, item := range items {
		if item.Kind == "databaseBackup" {
			writeJSON(w, 200, map[string]any{"exists": true, "enabled": item.IsEnabled, "cron_expression": item.CronExpression, "next_run_time": item.NextRunAt, "task_id": item.ID})
			return
		}
	}
	writeJSON(w, 200, map[string]any{"exists": false, "enabled": false})
}
