// SPDX-License-Identifier: AGPL-3.0-only
package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/AniBakaBaka/AniDan/internal/danmaku"
	"github.com/AniBakaBaka/AniDan/internal/job"
	"github.com/AniBakaBaka/AniDan/internal/media"
	"github.com/AniBakaBaka/AniDan/internal/store"
)

func (s *Server) registerLocal(m *http.ServeMux) {
	for _, p := range []struct {
		route string
		h     http.HandlerFunc
	}{{"POST /api/ui/local-scan/browse", s.localBrowse}, {"POST /api/ui/local-scan/create-folder", s.localCreateFolder}, {"DELETE /api/ui/local-scan/delete-folder", s.localDeleteFolder}, {"GET /api/ui/local-scan/last-path", s.localLastPath}, {"POST /api/ui/local-scan/save-path", s.localSavePath}, {"POST /api/ui/local-scan", s.localScan}, {"GET /api/ui/local-items", s.localItems}, {"GET /api/ui/local-works", s.localWorks}, {"GET /api/ui/local-movies/{title}/files", s.localMovies}, {"GET /api/ui/local-shows/{title}/seasons", s.localSeasons}, {"GET /api/ui/local-shows/{title}/seasons/{season}/episodes", s.localEpisodes}, {"PUT /api/ui/local-items/{item_id}", s.localItemUpdate}, {"DELETE /api/ui/local-items/{item_id}", s.localItemDelete}, {"POST /api/ui/local-items/batch-delete", s.localBatchDelete}, {"POST /api/ui/local-items/import", s.localImport}, {"POST /api/ui/local-items/{item_id}/binding-preview", s.localBindingPreview}, {"POST /api/ui/local-items/{item_id}/binding-confirm", s.localBindingConfirm}, {"GET /api/ui/local-items/{item_id}/poster", s.localPoster}} {
		m.HandleFunc(p.route, s.operator(p.h))
	}
	if s.Jobs != nil {
		if e := s.Jobs.Register("import_local_items", s.runLocalImport); e != nil {
			panic(e)
		}
	}
}
func localContained(root, p string) bool {
	rel, e := filepath.Rel(root, p)
	return e == nil && (rel == "." || filepath.IsLocal(rel))
}
func (s *Server) localAllowedPath(p string, write bool) (string, error) {
	if p == "" {
		p = s.DataDir
	}
	abs, e := filepath.Abs(p)
	if e != nil {
		return "", e
	}
	resolved, e := filepath.EvalSymlinks(abs)
	if e != nil {
		return "", e
	}
	if e = s.guardWebMigrationPath(resolved); e != nil {
		return "", e
	}
	roots := []string{s.DataDir}
	if !write {
		roots = append(roots, s.Config.ReadRoots...)
	}
	for _, root := range roots {
		base, e := filepath.Abs(root)
		if e != nil {
			continue
		}
		base, e = filepath.EvalSymlinks(base)
		if e == nil && localContained(base, resolved) {
			return resolved, nil
		}
	}
	return "", errors.New("path is outside configured data/read roots")
}
func (s *Server) localBrowse(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Path string `json:"path"`
	}
	if e := readJSON(r, &in); e != nil {
		httpError(w, 422, e.Error())
		return
	}
	if in.Path == "/" {
		out := []map[string]any{}
		for _, configured := range append([]string{s.DataDir}, s.Config.ReadRoots...) {
			p, e := filepath.Abs(configured)
			if e != nil {
				continue
			}
			p, e = filepath.EvalSymlinks(p)
			if e != nil {
				continue
			}
			if s.guardWebMigrationPath(p) != nil {
				continue
			}
			out = append(out, map[string]any{"storage": "local", "type": "dir", "path": p, "name": filepath.Base(p), "basename": filepath.Base(p), "extension": nil, "size": 0, "modify_time": nil})
		}
		writeJSON(w, 200, out)
		return
	}
	p, e := s.localAllowedPath(in.Path, false)
	if e != nil {
		httpError(w, 403, e.Error())
		return
	}
	d, e := s.localOpenAllowed(p)
	if e != nil {
		httpError(w, 404, e.Error())
		return
	}
	defer d.Close()
	entries, e := d.ReadDir(5001)
	if e != nil && e != io.EOF {
		httpError(w, 400, e.Error())
		return
	}
	if len(entries) > 5000 {
		httpError(w, 422, "Directory exceeds 5000 entries; select a narrower path")
		return
	}
	out := []map[string]any{}
	for _, entry := range entries {
		if s.guardWebMigrationPath(filepath.Join(p, entry.Name())) != nil {
			continue
		}
		if entry.Type()&os.ModeSymlink != 0 {
			continue
		}
		info, e := entry.Info()
		if e != nil {
			continue
		}
		typ := "file"
		if entry.IsDir() {
			typ = "dir"
		}
		out = append(out, map[string]any{"name": entry.Name(), "path": filepath.Join(p, entry.Name()), "type": typ, "isDirectory": entry.IsDir(), "is_dir": entry.IsDir(), "size": info.Size(), "modified": info.ModTime().Unix(), "modifiedTime": info.ModTime().Format("2006-01-02T15:04:05"), "modify_time": info.ModTime().Format("2006-01-02T15:04:05"), "storage": "local", "basename": strings.TrimSuffix(entry.Name(), filepath.Ext(entry.Name())), "extension": strings.TrimPrefix(filepath.Ext(entry.Name()), ".")})
	}
	sort.Slice(out, func(i, j int) bool {
		if boolean(out[i]["isDirectory"]) != boolean(out[j]["isDirectory"]) {
			return boolean(out[i]["isDirectory"])
		}
		if r.URL.Query().Get("sort") == "time" {
			return number(out[i]["modified"]) > number(out[j]["modified"])
		}
		return str(out[i]["name"]) < str(out[j]["name"])
	})
	writeJSON(w, 200, out)
}
func (s *Server) localCreateFolder(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Parent string `json:"parentPath"`
		Name   string `json:"folderName"`
	}
	if e := readJSON(r, &in); e != nil {
		httpError(w, 422, e.Error())
		return
	}
	if !filepath.IsLocal(in.Name) || filepath.Base(in.Name) != in.Name || in.Name == "." {
		httpError(w, 422, "folderName must be one path component")
		return
	}
	if in.Name == ".web-migrations" || in.Name == migrationActivationFile || in.Name == ".anidan-migration-activation-witness.json" {
		httpError(w, 403, "This name is reserved for private migration state")
		return
	}
	parent, e := s.localAllowedPath(in.Parent, true)
	if e != nil {
		httpError(w, 403, e.Error())
		return
	}
	root, e := os.OpenRoot(parent)
	if e != nil {
		httpError(w, 400, e.Error())
		return
	}
	defer root.Close()
	if e = root.Mkdir(in.Name, 0700); e != nil {
		httpError(w, 409, e.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"message": "文件夹已创建", "path": filepath.Join(parent, in.Name)})
}
func (s *Server) localDeleteFolder(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Path string `json:"folderPath"`
	}
	if e := readJSON(r, &in); e != nil {
		httpError(w, 422, e.Error())
		return
	}
	p, e := s.localAllowedPath(in.Path, true)
	if e != nil {
		httpError(w, 403, e.Error())
		return
	}
	base, _ := filepath.Abs(s.DataDir)
	base, _ = filepath.EvalSymlinks(base)
	if p == base || p == filepath.Join(base, "danmaku") || p == filepath.Join(base, "image") {
		httpError(w, 409, "Cannot remove a managed root")
		return
	}
	st, e := os.Stat(p)
	if e != nil || !st.IsDir() {
		httpError(w, 400, "Directory not found")
		return
	}
	if e = os.Remove(p); e != nil {
		httpError(w, 409, "Only empty directories can be removed safely")
		return
	}
	writeJSON(w, 200, map[string]any{"message": "文件夹已删除"})
}
func (s *Server) localLastPath(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{"path": s.authConfig(r.Context(), "local_scan_last_path", "")})
}
func (s *Server) localSavePath(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Path string `json:"scanPath"`
	}
	if e := readJSON(r, &in); e != nil {
		httpError(w, 422, e.Error())
		return
	}
	p, e := s.localAllowedPath(in.Path, false)
	if e != nil {
		httpError(w, 403, e.Error())
		return
	}
	if e = s.localStoreLastPath(r.Context(), p); e != nil {
		httpError(w, 500, e.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"message": "路径已保存"})
}
func (s *Server) localStoreLastPath(ctx context.Context, p string) error {
	row, e := s.Store.Get(ctx, "config", "local_scan_last_path")
	if e != nil && !errors.Is(e, sql.ErrNoRows) {
		return e
	}
	if row != nil {
		return s.Store.Update(ctx, "config", "local_scan_last_path", store.Row{"config_value": p})
	}
	_, e = s.Store.Insert(ctx, "config", store.Row{"config_key": "local_scan_last_path", "config_value": p})
	return e
}
func (s *Server) localScan(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Path string `json:"scanPath"`
	}
	if e := readJSON(r, &in); e != nil {
		httpError(w, 422, e.Error())
		return
	}
	p, e := s.localAllowedPath(in.Path, false)
	if e != nil {
		httpError(w, 403, e.Error())
		return
	}
	if e = s.localStoreLastPath(r.Context(), p); e != nil {
		httpError(w, 500, e.Error())
		return
	}
	s.importMu.Lock()
	defer s.importMu.Unlock()
	report, e := media.ScanLocal(r.Context(), p, media.MaxScanItems, func(item media.LocalItem) error {
		row := mediaItemRow(item.Item)
		for _, k := range []string{"media_id", "library_id", "series_id", "season_id", "episode_id"} {
			delete(row, k)
		}
		row["file_path"] = item.FilePath
		row["nfo_path"] = nil
		if item.NFOPath != "" {
			row["nfo_path"] = item.NFOPath
		}
		row["updated_at"] = s.now()
		old, e := s.Store.List(r.Context(), "local_danmaku_items", store.Row{"file_path": item.FilePath}, 1, 0)
		if e != nil {
			return e
		}
		if len(old) > 0 {
			return s.Store.Update(r.Context(), "local_danmaku_items", old[0]["id"], row)
		}
		row["created_at"] = s.now()
		_, e = s.Store.Insert(r.Context(), "local_danmaku_items", row)
		return e
	})
	if e != nil {
		httpError(w, 400, e.Error())
		return
	}
	if e = s.compatRecordScanIndex(r.Context(), p); e != nil {
		httpError(w, 500, "Scan stored, but index snapshot failed: "+e.Error())
		return
	}
	writeJSON(w, 202, map[string]any{"message": fmt.Sprintf("扫描完成: 找到 %d 个文件, 成功 %d 个", report.Total, report.Success), "result": report})
}
func (s *Server) localItems(w http.ResponseWriter, r *http.Request) {
	s.mediaList(w, r, "local_danmaku_items", nil)
}
func (s *Server) localWorks(w http.ResponseWriter, r *http.Request) {
	s.mediaGrouped(w, r, "local_danmaku_items")
}
func (s *Server) localMovies(w http.ResponseWriter, r *http.Request) {
	f := store.Row{"title": r.PathValue("title"), "media_type": "movie"}
	if y := r.URL.Query().Get("year"); y != "" {
		n, e := strconv.Atoi(y)
		if e != nil {
			httpError(w, 422, "Invalid year")
			return
		}
		f["year"] = n
	}
	s.mediaList(w, r, "local_danmaku_items", f)
}
func (s *Server) localSeasons(w http.ResponseWriter, r *http.Request) {
	s.mediaSeasonList(w, r, "local_danmaku_items")
}
func (s *Server) localEpisodes(w http.ResponseWriter, r *http.Request) {
	n, e := strconv.Atoi(r.PathValue("season"))
	if e != nil {
		httpError(w, 422, "Invalid season")
		return
	}
	s.mediaList(w, r, "local_danmaku_items", store.Row{"title": r.PathValue("title"), "season": n, "media_type": "tv_series"})
}
func (s *Server) localItemUpdate(w http.ResponseWriter, r *http.Request) {
	s.mediaUpdateItem(w, r, "local_danmaku_items", 204)
}
func (s *Server) localItemDelete(w http.ResponseWriter, r *http.Request) {
	s.mediaDeleteItem(w, r, "local_danmaku_items")
}
func (s *Server) localBatchDelete(w http.ResponseWriter, r *http.Request) {
	s.mediaDeleteBatch(w, r, "local_danmaku_items")
}
func (s *Server) localPoster(w http.ResponseWriter, r *http.Request) {
	id, e := idParam(r, "item_id")
	if e != nil {
		httpError(w, 422, e.Error())
		return
	}
	row, e := s.Store.Get(r.Context(), "local_danmaku_items", id)
	if e != nil || row == nil {
		httpError(w, 404, "Item not found")
		return
	}
	p := str(row["poster_url"])
	if !filepath.IsAbs(p) {
		p = filepath.Join(filepath.Dir(str(row["file_path"])), p)
	}
	p, e = s.localAllowedPath(p, false)
	if e != nil {
		httpError(w, 403, e.Error())
		return
	}
	switch strings.ToLower(filepath.Ext(p)) {
	case ".jpg", ".jpeg", ".png", ".gif", ".webp":
	default:
		httpError(w, 400, "Unsupported poster file type")
		return
	}
	f, e := s.localOpenAllowed(p)
	if e != nil {
		httpError(w, 403, e.Error())
		return
	}
	defer f.Close()
	st, e := f.Stat()
	if e != nil {
		httpError(w, 500, e.Error())
		return
	}
	http.ServeContent(w, r, st.Name(), st.ModTime(), f)
}

type localTaskParams struct {
	IDs     []int64                     `json:"itemIds"`
	Options map[int64]localImportOption `json:"options"`
}
type localImportOption struct {
	Provider string `json:"provider"`
	MediaID  string `json:"mediaId"`
}

func (s *Server) localImport(w http.ResponseWriter, r *http.Request) {
	var selection itemSelection
	if e := readJSON(r, &selection); e != nil {
		httpError(w, 422, e.Error())
		return
	}
	ids, e := s.selectedItemIDs(r.Context(), "local_danmaku_items", selection)
	if e != nil {
		httpError(w, 422, e.Error())
		return
	}
	if len(ids) == 0 {
		writeJSON(w, 202, map[string]any{"message": "没有要导入的项目"})
		return
	}
	p := localTaskParams{IDs: ids, Options: map[int64]localImportOption{}}
	for _, v := range selection.Items {
		p.Options[v.ItemID] = localImportOption{v.Provider, v.MediaID}
	}
	b, _ := json.Marshal(p)
	h := sha256.Sum256(b)
	s.mediaSubmit(w, r, "import_local_items", "导入本地弹幕", p, fmt.Sprintf("local-import-%x", h[:16]))
}
func (s *Server) runLocalImport(ctx context.Context, raw json.RawMessage, progress func(int, string)) (any, error) {
	var p localTaskParams
	if e := unmarshalExactJSON(raw, &p); e != nil {
		return nil, e
	}
	done := 0
	warnings := localImportWarnings{}
	for i, id := range p.IDs {
		if e := job.Checkpoint(ctx); e != nil {
			return warnings.result(done, id), e
		}
		row, e := s.Store.Get(ctx, "local_danmaku_items", id)
		if e != nil || row == nil {
			if cancelled := job.Checkpoint(ctx); cancelled != nil {
				return warnings.result(done, id), cancelled
			}
			return warnings.result(done, id), fmt.Errorf("local item %d not found", id)
		}
		progress(i*100/max(1, len(p.IDs)), "导入本地弹幕: "+str(row["title"]))
		if e = s.importLocalRowWarnings(ctx, row, p.Options[id], warnings.add); e != nil {
			return warnings.result(done, id), e
		}
		done++
	}
	return warnings.result(done, 0), nil
}
func (s *Server) importLocalRow(ctx context.Context, row store.Row, opt localImportOption) error {
	return s.importLocalRowWarnings(ctx, row, opt, nil)
}
func (s *Server) importLocalRowWarnings(ctx context.Context, row store.Row, opt localImportOption, warn func(localImportWarning)) error {
	if e := job.Checkpoint(ctx); e != nil {
		return e
	}
	source, e := s.localAllowedPath(str(row["file_path"]), false)
	if e != nil {
		return e
	}
	f, e := s.localOpenAllowed(source)
	if e != nil {
		return e
	}
	content, e := io.ReadAll(contextReader{ctx, io.LimitReader(f, (32<<20)+1)})
	f.Close()
	if e != nil {
		return e
	}
	if len(content) > 32<<20 {
		return errors.New("local XML exceeds 32 MiB")
	}
	comments, e := danmaku.Parse(contextReader{ctx, bytes.NewReader(content)})
	if e != nil {
		return e
	}
	if len(comments) == 0 {
		return errors.New("local XML contains no valid comments")
	}
	if opt.Provider == "" {
		opt.Provider = "custom"
	}
	if len(opt.Provider) > 500 {
		return errors.New("provider name too long")
	}
	season := int64(1)
	episode := int64(1)
	if row["season"] != nil {
		season = number(row["season"])
	}
	if row["episode"] != nil {
		episode = number(row["episode"])
	}
	if season < 0 || episode < 0 {
		return errors.New("negative season or episode")
	}
	assignment, e := s.localImportSourceAssignment(source)
	if e != nil {
		return e
	}
	binding, e := s.localReadImportBinding(ctx, s.Store.DB, number(row["id"]), false)
	if e != nil {
		return e
	}
	var selected localImportTarget
	var poolBefore store.Row
	if binding == nil {
		// Pre-binding historical imports cannot prove ownership merely from an
		// editable provider_episode_id string. Preserve them for explicit review.
		if boolean(row["is_imported"]) {
			return errLocalImportBindingReview
		}
		selected, e = s.localImportTarget(ctx, s.Store.DB, row, season, false)
	} else {
		if e = localCheckImportBindingInput(binding, row, opt, assignment, season, episode); e != nil {
			return e
		}
		e = s.libTransaction(ctx, func(tx *sql.Tx) error {
			if err := s.localCheckImportBindingRows(ctx, tx, binding); err != nil {
				return err
			}
			var err error
			selected, err = s.localImportTargetByID(ctx, tx, binding.AnimeID, true)
			if err != nil {
				return err
			}
			poolBefore, err = s.libOne(ctx, tx, "episode", "id=?", binding.EpisodeID)
			return err
		})
	}
	if e != nil {
		return e
	}
	posterPath, posterURL, e := s.localCopyPosterContext(ctx, row, source)
	if e != nil {
		return e
	}
	enrichmentAttempted := false
	pending, e := s.localFetchTMDBPoster(ctx, row, selected, &enrichmentAttempted, warn)
	if e != nil {
		return e
	}
	defer pending.release()
	if e = job.Checkpoint(ctx); e != nil {
		return e
	}
	s.importMu.Lock()
	defer s.importMu.Unlock()
	s.fileMu.Lock()
	defer s.fileMu.Unlock()
	if e = job.Checkpoint(ctx); e != nil {
		return e
	}
	tx, e := s.Store.DB.BeginTx(ctx, nil)
	if e != nil {
		return e
	}
	defer tx.Rollback()
	if e = s.localRecheckImportRow(ctx, tx, row); e != nil {
		return e
	}
	var current localImportTarget
	if binding != nil {
		if e = s.localCheckImportBindingRows(ctx, tx, binding); e != nil {
			return e
		}
		if e = s.localCheckRepeatPoolSnapshot(ctx, tx, poolBefore); e != nil {
			return e
		}
		current, e = s.localImportTargetByID(ctx, tx, binding.AnimeID, true)
	} else {
		if now, err := s.localReadImportBinding(ctx, tx, number(row["id"]), true); err != nil {
			return err
		} else if now != nil {
			return errLocalImportBindingReview
		}
		current, e = s.localImportTarget(ctx, tx, row, season, true)
	}
	if e != nil {
		return e
	}
	if (enrichmentAttempted || selected.ID != 0) && !selected.sameIdentity(current) {
		return errors.New("local import library target was deleted or changed")
	}
	animeID := current.ID
	if animeID == 0 {
		animeID, e = s.Store.InsertTx(ctx, tx, "anime", store.Row{"title": row["title"], "type": row["media_type"], "season": season, "year": row["year"], "created_at": s.now(), "local_image_path": optionalMediaString(posterPath), "image_url": optionalMediaString(posterURL)})
	}
	if e != nil {
		return e
	}
	if posterPath != "" || posterURL != "" {
		if _, e = tx.ExecContext(ctx, s.Store.Rebind("UPDATE anime SET local_image_path=COALESCE(NULLIF(local_image_path,''),?), image_url=COALESCE(NULLIF(image_url,''),?) WHERE id=?"), optionalMediaString(posterPath), optionalMediaString(posterURL), animeID); e != nil {
			return e
		}
	}
	if opt.MediaID == "" {
		opt.MediaID = fmt.Sprintf("custom_%d_%v", animeID, row["id"])
	}
	var sourceID, order int64
	e = tx.QueryRowContext(ctx, s.Store.Rebind("SELECT id,source_order FROM anime_sources WHERE anime_id=? AND provider_name=? AND media_id=?"), animeID, opt.Provider, opt.MediaID).Scan(&sourceID, &order)
	if errors.Is(e, sql.ErrNoRows) {
		if e = tx.QueryRowContext(ctx, s.Store.Rebind("SELECT COALESCE(MAX(source_order),0)+1 FROM anime_sources WHERE anime_id=?"), animeID).Scan(&order); e != nil {
			return e
		}
		sourceID, e = s.Store.InsertTx(ctx, tx, "anime_sources", store.Row{"anime_id": animeID, "source_order": order, "provider_name": opt.Provider, "media_id": opt.MediaID, "is_favorited": false, "incremental_refresh_enabled": false, "is_finished": false, "incremental_refresh_failures": 0, "created_at": s.now()})
	}
	if e != nil {
		return e
	}
	if binding != nil && (animeID != binding.AnimeID || sourceID != binding.SourceID) {
		return errLocalImportBindingReview
	}
	storedSource, e := s.libOne(ctx, tx, "anime_sources", "id=?", sourceID)
	if e != nil {
		return e
	}
	if str(storedSource["provider_name"]) != opt.Provider || str(storedSource["media_id"]) != opt.MediaID || number(storedSource["anime_id"]) != animeID || number(storedSource["source_order"]) != order {
		return errLocalImportBindingReview
	}
	var episodeID int64
	e = tx.QueryRowContext(ctx, s.Store.Rebind("SELECT id FROM episode WHERE source_id=? AND episode_index=?"), sourceID, episode).Scan(&episodeID)
	fresh := errors.Is(e, sql.ErrNoRows)
	if e != nil && !fresh {
		return e
	}
	if !fresh && (binding == nil || episodeID != binding.EpisodeID) {
		return errLocalImportBindingReview
	}
	if binding != nil && fresh {
		return errLocalImportBindingReview
	}
	if fresh {
		episodeID, e = episodeIdentifier(animeID, order, episode)
		if e != nil {
			return e
		}
	}
	hash := sha256.Sum256(content)
	relative := filepath.Join("danmaku", strconv.FormatInt(animeID, 10), fmt.Sprintf("%d-%s.xml", episodeID, hex.EncodeToString(hash[:])))
	dest, e := s.writeManagedBytes(ctx, relative, content, 32<<20)
	if e != nil {
		return e
	}
	if fresh {
		_, e = s.Store.InsertTx(ctx, tx, "episode", store.Row{"id": episodeID, "source_id": sourceID, "title": fmt.Sprintf("第%d集", episode), "episode_index": episode, "provider_episode_id": fmt.Sprintf("local_%v_%d", row["id"], episode), "danmaku_file_path": dest, "fetched_at": s.now(), "comment_count": len(comments)})
	} else {
		_, e = tx.ExecContext(ctx, s.Store.Rebind("UPDATE episode SET danmaku_file_path=?,comment_count=?,fetched_at=? WHERE id=?"), dest, len(comments), s.now(), episodeID)
	}
	if e != nil {
		return e
	}
	if e = s.localFillImportMetadata(ctx, tx, animeID, row); e != nil {
		return e
	}
	if e = job.Checkpoint(ctx); e != nil {
		return e
	}
	// Image persistence is delayed until XML, linkage and identity checks have
	// succeeded. A poster installed during network I/O always wins.
	if pending != nil && !current.hasPoster() && !current.conflicts(row) {
		path, err := s.libCachePosterContext(ctx, pending.data, pending.ext)
		if err != nil {
			if cancelled := ctx.Err(); cancelled != nil {
				return cancelled
			}
			return errors.New("unable to store TMDB poster")
		}
		if e = job.Checkpoint(ctx); e != nil {
			return e
		}
		if _, e = tx.ExecContext(ctx, s.Store.Rebind("UPDATE anime SET local_image_path=? WHERE id=? AND (local_image_path IS NULL OR local_image_path='') AND (image_url IS NULL OR image_url='')"), path, animeID); e != nil {
			return e
		}
	}
	if e = s.localWriteImportBinding(ctx, tx, binding, row, opt, assignment, season, episode, animeID, sourceID, episodeID); e != nil {
		return e
	}
	_, e = tx.ExecContext(ctx, s.Store.Rebind("UPDATE local_danmaku_items SET is_imported=?,updated_at=? WHERE id=?"), true, s.now(), row["id"])
	if e != nil {
		return e
	}
	if e = job.Checkpoint(ctx); e != nil {
		return e
	}
	return tx.Commit()
}

// Kept for the opt-in historical assessment; production uses an anchored
// DataDir-relative path and the caller's explicit limit through writeManagedBytes.
func localWriteImmutable(dest string, b []byte) error {
	root, err := os.OpenRoot(filepath.Dir(dest))
	if err != nil {
		return err
	}
	defer root.Close()
	return publishImmutableBytes(context.Background(), root, filepath.Base(dest), b, 32<<20)
}

func (s *Server) localOpenAllowed(p string) (*os.File, error) {
	resolved, e := s.localAllowedPath(p, false)
	if e != nil {
		return nil, e
	}
	for _, configured := range append([]string{s.DataDir}, s.Config.ReadRoots...) {
		base, e := filepath.Abs(configured)
		if e != nil {
			continue
		}
		base, e = filepath.EvalSymlinks(base)
		if e != nil || !localContained(base, resolved) {
			continue
		}
		root, e := os.OpenRoot(base)
		if e != nil {
			return nil, e
		}
		rel, e := filepath.Rel(base, resolved)
		if e != nil {
			root.Close()
			return nil, e
		}
		before, e := root.Lstat(rel)
		if e != nil || (!before.Mode().IsRegular() && !before.IsDir()) {
			root.Close()
			if e != nil {
				return nil, e
			}
			return nil, errors.New("local path is not a regular file or directory")
		}
		f, e := root.OpenFile(rel, regularReadFlags, 0)
		if e != nil {
			root.Close()
			return nil, e
		}
		opened, statErr := f.Stat()
		named, nameErr := root.Lstat(rel)
		root.Close()
		if statErr != nil || nameErr != nil || !os.SameFile(before, opened) || !os.SameFile(opened, named) || (!opened.Mode().IsRegular() && !opened.IsDir()) || named.Mode()&os.ModeSymlink != 0 {
			f.Close()
			return nil, errors.New("local path changed while opening")
		}
		return f, nil
	}
	return nil, errors.New("path outside read roots")
}

func optionalMediaString(v string) any {
	if v == "" {
		return nil
	}
	return v
}
func (s *Server) localCopyPoster(row store.Row, xmlPath string) (string, string, error) {
	return s.localCopyPosterContext(context.Background(), row, xmlPath)
}
func (s *Server) localCopyPosterContext(ctx context.Context, row store.Row, xmlPath string) (string, string, error) {
	if err := ctx.Err(); err != nil {
		return "", "", err
	}
	p := str(row["poster_url"])
	if p == "" {
		return "", "", nil
	}
	if strings.HasPrefix(p, "http://") || strings.HasPrefix(p, "https://") {
		return "", p, nil
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(filepath.Dir(xmlPath), p)
	}
	f, e := s.localOpenAllowed(p)
	if e != nil {
		return "", "", e
	}
	data, e := io.ReadAll(contextReader{ctx, io.LimitReader(f, (16<<20)+1)})
	f.Close()
	if e != nil {
		return "", "", e
	}
	if len(data) > 16<<20 {
		return "", "", errors.New("local poster exceeds 16 MiB")
	}
	typ := http.DetectContentType(data)
	ext := ""
	switch typ {
	case "image/jpeg":
		ext = ".jpg"
	case "image/png":
		ext = ".png"
	case "image/gif":
		ext = ".gif"
	case "image/webp":
		ext = ".webp"
	default:
		return "", "", errors.New("local poster is not a supported image")
	}
	sum := sha256.Sum256(data)
	name := "local-" + hex.EncodeToString(sum[:]) + ext
	if _, e = s.writeManagedBytes(ctx, filepath.Join("image", name), data, 16<<20); e != nil {
		return "", "", e
	}
	return "/data/images/" + name, "", nil
}
