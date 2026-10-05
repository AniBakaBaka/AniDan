// SPDX-License-Identifier: AGPL-3.0-only
package server

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/AniBakaBaka/AniDan/internal/danmaku"
	"github.com/AniBakaBaka/AniDan/internal/job"
	"github.com/AniBakaBaka/AniDan/internal/provider"
	"github.com/AniBakaBaka/AniDan/internal/store"
)

type libTaskParams struct {
	Table       string          `json:"table,omitempty"`
	IDs         []int64         `json:"ids,omitempty"`
	Source      int64           `json:"sourceId,omitempty"`
	DeleteFiles bool            `json:"deleteFiles,omitempty"`
	Offset      int64           `json:"offset,omitempty"`
	Reorder     bool            `json:"reorder,omitempty"`
	Mode        string          `json:"mode,omitempty"`
	Items       []libImportItem `json:"items,omitempty"`
}
type libImportItem struct {
	Title                     string `json:"title"`
	Index                     int64  `json:"episodeIndex"`
	Content                   string `json:"content"`
	ContentRef                string `json:"contentRef,omitempty"`
	URL                       string `json:"sourceUrl"`
	URLProvider               string `json:"urlProvider"`
	ExpectedProviderEpisodeID string `json:"expectedProviderEpisodeId,omitempty"`
}

func (s *Server) registerLibraryTasks(m *http.ServeMux) {
	handlers := map[string]job.Handler{"library_delete": s.libDeleteJob, "library_refresh": s.libRefreshJob, "library_reindex": s.libReindexJob, "library_import": s.libImportJob, "library_source_refresh": s.libSourceRefreshJob}
	if s.Jobs != nil {
		for kind, h := range handlers {
			if e := s.Jobs.Register(kind, h); e != nil {
				panic(e)
			}
		}
	}
	for _, prefix := range []string{"/api/ui", "/api/control"} {
		m.HandleFunc("DELETE "+prefix+"/library/anime/{animeId}", s.libDelete)
		m.HandleFunc("DELETE "+prefix+"/library/source/{sourceId}", s.libDelete)
		m.HandleFunc("DELETE "+prefix+"/library/episode/{episodeId}", s.libDelete)
		m.HandleFunc("POST "+prefix+"/library/episode/{episodeId}/refresh", s.libRefresh)
	}
	m.HandleFunc("POST /api/ui/library/sources/delete-bulk", s.libDeleteBulk)
	m.HandleFunc("POST /api/ui/library/episodes/delete-bulk", s.libDeleteBulk)
	m.HandleFunc("POST /api/ui/library/episodes/refresh-bulk", s.libRefreshBulk)
	m.HandleFunc("POST /api/ui/library/episodes/offset", s.libOffset)
	m.HandleFunc("POST /api/ui/library/source/{sourceId}/reorder-episodes", s.libReorder)
	m.HandleFunc("POST /api/ui/library/source/{sourceId}/refresh", s.libSourceRefresh)
	m.HandleFunc("POST /api/ui/library/source/{sourceId}/manual-import", s.libManualImport)
	m.HandleFunc("POST /api/ui/library/source/{sourceId}/batch-import", s.libBatchImport)
	m.HandleFunc("POST /api/ui/library/source/{sourceId}/import-collection", s.libCollectionImport)
}
func (s *Server) libSubmit(w http.ResponseWriter, r *http.Request, kind, title string, p libTaskParams) {
	if s.Jobs == nil {
		httpError(w, 503, "Task manager unavailable")
		return
	}
	b, e := json.Marshal(p)
	if e != nil {
		httpError(w, 422, "Invalid parameters")
		return
	}
	if len(b) > job.MaxParamsBytes && len(p.Items) > 0 {
		if e = s.libSpoolImports(r.Context(), &p); e != nil {
			httpError(w, 507, "Unable to persist import payload")
			return
		}
	}
	hash := sha256.Sum256(b)
	id, e := s.Jobs.SubmitWithOptions(kind, p, nil, job.SubmitOptions{Title: title, QueueType: "management", UniqueKey: fmt.Sprintf("%s-%x", kind, hash[:16])})
	if e != nil {
		httpError(w, 409, e.Error())
		return
	}
	out := store.Row{"message": title + "任务已提交", "taskId": id}
	if libIsControl(r) {
		out["status"] = "success"
	}
	writeJSON(w, 202, out)
}
func libDecodeTask(raw json.RawMessage) (libTaskParams, error) {
	var p libTaskParams
	e := unmarshalExactJSON(raw, &p)
	return p, e
}
func (s *Server) libExisting(r *http.Request, table string, id int64) error {
	v, e := s.Store.Get(r.Context(), table, id)
	if e != nil {
		return e
	}
	if v == nil {
		return sql.ErrNoRows
	}
	return nil
}
func (s *Server) libDelete(w http.ResponseWriter, r *http.Request) {
	if !s.libAuthorized(w, r) {
		return
	}
	table, key := "episode", "episodeId"
	if r.PathValue("animeId") != "" {
		table, key = "anime", "animeId"
	} else if r.PathValue("sourceId") != "" {
		table, key = "anime_sources", "sourceId"
	}
	id, e := idParam(r, key)
	if e != nil {
		httpError(w, 422, "Invalid ID")
		return
	}
	if e = s.libExisting(r, table, id); e != nil {
		libWriteError(w, e)
		return
	}
	s.libSubmit(w, r, "library_delete", "删除条目", libTaskParams{Table: table, IDs: []int64{id}, DeleteFiles: r.URL.Query().Get("deleteFiles") != "false"})
}
func (s *Server) libDeleteBulk(w http.ResponseWriter, r *http.Request) {
	if !s.libAuthorized(w, r) {
		return
	}
	var b struct {
		Episodes      []int64 `json:"episodeIds"`
		EpisodesAlias []int64 `json:"episode_ids"`
		Sources       []int64 `json:"sourceIds"`
		SourcesAlias  []int64 `json:"source_ids"`
		DeleteFiles   *bool   `json:"deleteFiles"`
	}
	if readJSON(r, &b) != nil {
		httpError(w, 422, "Invalid JSON")
		return
	}
	table, ids := "episode", b.Episodes
	if len(ids) == 0 {
		ids = b.EpisodesAlias
	}
	if strings.Contains(r.URL.Path, "/sources/") {
		table, ids = "anime_sources", b.Sources
		if len(ids) == 0 {
			ids = b.SourcesAlias
		}
	}
	if len(ids) == 0 || len(ids) > 10000 {
		httpError(w, 422, "IDs must contain 1..10000 entries")
		return
	}
	s.libSubmit(w, r, "library_delete", "批量删除", libTaskParams{Table: table, IDs: ids, DeleteFiles: b.DeleteFiles == nil || *b.DeleteFiles})
}
func (s *Server) libDeleteFileRel(path string) (string, error) {
	if path == "" {
		return "", nil
	}
	base, e := filepath.Abs(s.DataDir)
	if e != nil {
		return "", e
	}
	p := filepath.FromSlash(path)
	if strings.HasPrefix(path, "/app/config/") {
		p = filepath.Join(base, filepath.FromSlash(strings.TrimPrefix(path, "/app/config/")))
	} else if strings.HasPrefix(path, "config/") {
		p = filepath.Join(base, filepath.FromSlash(strings.TrimPrefix(path, "config/")))
	} else if strings.HasPrefix(path, "danmaku/") {
		p = filepath.Join(base, p)
	} else if !filepath.IsAbs(p) {
		p, e = filepath.Abs(p)
		if e != nil {
			return "", e
		}
	}
	rel, e := filepath.Rel(base, p)
	if e != nil || !filepath.IsLocal(rel) {
		return "", libErr(409, "XML is outside writable data directory; delete with deleteFiles=false to retain external files")
	}
	if !strings.HasPrefix(filepath.ToSlash(rel), "danmaku/") {
		return "", libErr(409, "Only managed danmaku XML files can be deleted")
	}
	if !strings.HasSuffix(strings.ToLower(rel), ".xml") {
		return "", libErr(409, "Only XML files can be deleted")
	}
	return rel, nil
}
func (s *Server) libDeleteJob(ctx context.Context, raw json.RawMessage, progress func(int, string)) (any, error) {
	p, e := libDecodeTask(raw)
	if e != nil {
		return nil, e
	}
	if p.Table != "anime" && p.Table != "anime_sources" && p.Table != "episode" {
		return nil, errors.New("invalid deletion table")
	}
	s.fileMu.Lock()
	defer s.fileMu.Unlock()
	progress(5, "正在检查关联数据和文件")
	files := map[string]bool{}
	deleted := int64(0)
	e = s.libTransaction(ctx, func(tx *sql.Tx) error {
		seen := map[int64]bool{}
		for _, id := range p.IDs {
			if seen[id] {
				continue
			}
			seen[id] = true
			if e := job.Checkpoint(ctx); e != nil {
				return e
			}
			query := "SELECT e.* FROM episode e WHERE e.id = ?"
			if p.Table == "anime_sources" {
				query = "SELECT e.* FROM episode e WHERE e.source_id = ?"
			} else if p.Table == "anime" {
				query = "SELECT e.* FROM episode e JOIN anime_sources s ON s.id=e.source_id WHERE s.anime_id = ?"
			}
			rows, e := s.libRows(ctx, tx, "episode", query, id)
			if e != nil {
				return e
			}
			if p.DeleteFiles {
				for _, ep := range rows {
					rel, e := s.libDeleteFileRel(authString(ep["danmaku_file_path"]))
					if e != nil {
						return e
					}
					if rel != "" {
						files[rel] = true
					}
				}
			}
			if p.Table == "anime" {
				if _, e = tx.ExecContext(ctx, s.Store.Rebind("UPDATE external_calendar_item SET local_anime_id = NULL, local_source_id = NULL WHERE local_anime_id = ?"), id); e != nil {
					return e
				}
			} else if p.Table == "anime_sources" {
				if _, e = tx.ExecContext(ctx, s.Store.Rebind("UPDATE external_calendar_item SET local_source_id = NULL WHERE local_source_id = ?"), id); e != nil {
					return e
				}
			}
			res, e := tx.ExecContext(ctx, s.Store.Rebind("DELETE FROM "+s.Store.Quote(p.Table)+" WHERE id = ?"), id)
			if e != nil {
				return e
			}
			n, _ := res.RowsAffected()
			deleted += n
		}
		if len(files) > 0 {
			rows, e := tx.QueryContext(ctx, "SELECT danmaku_file_path FROM episode WHERE danmaku_file_path IS NOT NULL")
			if e != nil {
				return e
			}
			defer rows.Close()
			for rows.Next() {
				var path string
				if e = rows.Scan(&path); e != nil {
					return e
				}
				if rel, e := s.libDeleteFileRel(path); e == nil {
					delete(files, rel)
				}
			}
			if e = rows.Err(); e != nil {
				return e
			}
		}
		return nil
	})
	if e != nil {
		return nil, e
	}
	progress(80, "数据库删除已提交，正在清理未引用XML")
	removed := 0
	if len(files) > 0 {
		root, e := os.OpenRoot(s.DataDir)
		if e != nil {
			return nil, e
		}
		defer root.Close()
		for rel := range files {
			if e = job.Checkpoint(ctx); e != nil {
				return nil, e
			}
			before, _ := root.Stat(rel)
			if e = root.Remove(rel); e != nil && !errors.Is(e, os.ErrNotExist) {
				return nil, fmt.Errorf("database deletion committed; XML cleanup incomplete: %w", e)
			}
			s.invalidateCommentFile(filepath.Join(s.DataDir, rel), before, nil)
			removed++
		}
	}
	progress(100, "删除完成")
	return store.Row{"deleted": deleted, "removedFiles": removed}, nil
}
func (s *Server) libRefresh(w http.ResponseWriter, r *http.Request) {
	if !s.libAuthorized(w, r) {
		return
	}
	id, e := idParam(r, "episodeId")
	if e != nil {
		httpError(w, 422, "Invalid ID")
		return
	}
	if e = s.libExisting(r, "episode", id); e != nil {
		libWriteError(w, e)
		return
	}
	s.libSubmit(w, r, "library_refresh", "刷新分集弹幕", libTaskParams{IDs: []int64{id}})
}
func (s *Server) libRefreshBulk(w http.ResponseWriter, r *http.Request) {
	if !s.libAuthorized(w, r) {
		return
	}
	var b struct {
		IDs []int64 `json:"episodeIds"`
	}
	if readJSON(r, &b) != nil || len(b.IDs) == 0 || len(b.IDs) > 10000 {
		httpError(w, 422, "episodeIds is required")
		return
	}
	s.libSubmit(w, r, "library_refresh", "批量刷新", libTaskParams{IDs: b.IDs})
}
func (s *Server) libRefreshJob(ctx context.Context, raw json.RawMessage, progress func(int, string)) (any, error) {
	p, e := libDecodeTask(raw)
	if e != nil {
		return nil, e
	}
	done := 0
	for _, id := range p.IDs {
		if e = job.Checkpoint(ctx); e != nil {
			return nil, e
		}
		ep, e := s.Store.Get(ctx, "episode", id)
		if e != nil || ep == nil {
			return nil, sql.ErrNoRows
		}
		if _, e = s.fetchComments(ctx, ep, func(n int, text string) { progress((done*100+n)/len(p.IDs), text) }); e != nil {
			return nil, e
		}
		done++
	}
	return store.Row{"refreshed": done}, nil
}
func (s *Server) libReorder(w http.ResponseWriter, r *http.Request) {
	if !s.libAuthorized(w, r) {
		return
	}
	id, e := idParam(r, "sourceId")
	if e != nil {
		httpError(w, 422, "Invalid ID")
		return
	}
	if e = s.libExisting(r, "anime_sources", id); e != nil {
		libWriteError(w, e)
		return
	}
	s.libSubmit(w, r, "library_reindex", "重整分集顺序", libTaskParams{Source: id, Reorder: true})
}
func (s *Server) libOffset(w http.ResponseWriter, r *http.Request) {
	if !s.libAuthorized(w, r) {
		return
	}
	var b struct {
		IDs    []int64 `json:"episodeIds"`
		Offset int64   `json:"offset"`
	}
	if readJSON(r, &b) != nil || len(b.IDs) == 0 || len(b.IDs) > 10000 {
		httpError(w, 422, "episodeIds is required")
		return
	}
	for _, id := range b.IDs {
		v, e := s.Store.Get(r.Context(), "episode", id)
		if e != nil || v == nil {
			httpError(w, 404, "Episode not found")
			return
		}
		if authInt(v["episode_index"])+b.Offset < 1 {
			httpError(w, 400, "Adjusted episode index must be positive")
			return
		}
	}
	s.libSubmit(w, r, "library_reindex", "集数偏移", libTaskParams{IDs: b.IDs, Offset: b.Offset})
}
func (s *Server) libReindexJob(ctx context.Context, raw json.RawMessage, progress func(int, string)) (any, error) {
	p, e := libDecodeTask(raw)
	if e != nil {
		return nil, e
	}
	progress(5, "正在验证分集重编号")
	count := 0
	e = s.libTransaction(ctx, func(tx *sql.Tx) error {
		episodes := []store.Row{}
		if p.Reorder {
			var e error
			episodes, e = s.libEpisodesQ(ctx, tx, p.Source)
			if e != nil {
				return e
			}
		} else {
			seen := map[int64]bool{}
			for _, id := range p.IDs {
				if seen[id] {
					return libErr(422, "Duplicate episode ID")
				}
				seen[id] = true
				v, e := s.libOne(ctx, tx, "episode", "id = ?", id)
				if e != nil {
					return e
				}
				episodes = append(episodes, v)
			}
		}
		return s.libReindexTx(ctx, tx, episodes, func(i int, ep store.Row) int64 {
			if p.Reorder {
				return int64(i + 1)
			}
			return authInt(ep["episode_index"]) + p.Offset
		}, &count)
	})
	if e != nil {
		return nil, e
	}
	progress(100, "重编号完成")
	return store.Row{"updated": count}, nil
}
func (s *Server) libReindexTx(ctx context.Context, tx *sql.Tx, episodes []store.Row, indexFn func(int, store.Row) int64, count *int) error {
	selected := map[int64]bool{}
	for _, ep := range episodes {
		selected[authInt(ep["id"])] = true
	}
	type change struct {
		row             store.Row
		old, new, index int64
	}
	changes := []change{}
	dest := map[int64]bool{}
	for i, ep := range episodes {
		if e := job.Checkpoint(ctx); e != nil {
			return e
		}
		source, e := s.libOne(ctx, tx, "anime_sources", "id = ?", ep["source_id"])
		if e != nil {
			return e
		}
		index := indexFn(i, ep)
		id, e := libEpisodeID(authInt(source["anime_id"]), authInt(source["source_order"]), index)
		if e != nil {
			return e
		}
		if dest[id] {
			return libErr(409, "Conflicting target episode IDs")
		}
		dest[id] = true
		if other, e := s.libOne(ctx, tx, "episode", "source_id = ? AND episode_index = ?", ep["source_id"], index); e == nil && !selected[authInt(other["id"])] {
			return libErr(409, "Target episode index already exists")
		} else if e != nil && !errors.Is(e, sql.ErrNoRows) {
			return e
		}
		if other, e := s.libOne(ctx, tx, "episode", "id = ?", id); e == nil && !selected[authInt(other["id"])] {
			return libErr(409, "Target episode ID already exists")
		} else if e != nil && !errors.Is(e, sql.ErrNoRows) {
			return e
		}
		changes = append(changes, change{ep, authInt(ep["id"]), id, index})
	}
	for _, c := range changes {
		if _, e := tx.ExecContext(ctx, s.Store.Rebind("DELETE FROM episode WHERE id = ?"), c.old); e != nil {
			return e
		}
	}
	for _, c := range changes {
		c.row["id"] = c.new
		c.row["episode_index"] = c.index
		if _, e := s.Store.InsertTx(ctx, tx, "episode", c.row); e != nil {
			return e
		}
		*count++
	}
	return nil
}
func (s *Server) libEditEpisode(w http.ResponseWriter, r *http.Request) {
	if !s.libAuthorized(w, r) {
		return
	}
	id, e := idParam(r, "episodeId")
	var b struct {
		Title string  `json:"title"`
		Index int64   `json:"episodeIndex"`
		URL   *string `json:"sourceUrl"`
		Path  *string `json:"danmakuFilePath"`
	}
	if e != nil || readJSON(r, &b) != nil || b.Title == "" || b.Index < 1 {
		httpError(w, 422, "title and positive episodeIndex are required")
		return
	}
	if b.Path != nil && *b.Path != "" {
		f, e := s.resolveDanmaku(*b.Path)
		if e != nil {
			httpError(w, 422, "danmakuFilePath must be a readable file in an allowed root")
			return
		}
		f.Close()
	}
	e = s.libTransaction(r.Context(), func(tx *sql.Tx) error {
		ep, e := s.libOne(r.Context(), tx, "episode", "id = ?", id)
		if e != nil {
			return e
		}
		src, e := s.libOne(r.Context(), tx, "anime_sources", "id = ?", ep["source_id"])
		if e != nil {
			return e
		}
		if !libIsControl(r) && authString(src["provider_name"]) != "custom" && (b.URL == nil || *b.URL == "") {
			return libErr(422, "对于非自定义源，sourceUrl 是必需的")
		}
		ep["title"] = b.Title
		ep["source_url"] = nil
		if b.URL != nil {
			ep["source_url"] = *b.URL
		}
		if b.Path != nil {
			ep["danmaku_file_path"] = *b.Path
		}
		if authInt(ep["episode_index"]) == b.Index {
			return s.libUpdate(r.Context(), tx, "episode", id, store.Row{"title": ep["title"], "source_url": ep["source_url"], "danmaku_file_path": ep["danmaku_file_path"]})
		}
		n := 0
		return s.libReindexTx(r.Context(), tx, []store.Row{ep}, func(int, store.Row) int64 { return b.Index }, &n)
	})
	if e != nil {
		libWriteError(w, e)
		return
	}
	libAction(w, r, "分集信息更新成功", nil)
}
func (s *Server) libManualImport(w http.ResponseWriter, r *http.Request) {
	if !s.libAuthorized(w, r) {
		return
	}
	id, e := idParam(r, "sourceId")
	var item libImportItem
	if e != nil || readJSON(r, &item) != nil || item.Index < 1 || (item.Content == "" && item.URL == "") {
		httpError(w, 422, "episodeIndex and content or sourceUrl are required")
		return
	}
	if e = s.libExisting(r, "anime_sources", id); e != nil {
		libWriteError(w, e)
		return
	}
	item.ContentRef = ""
	s.libSubmit(w, r, "library_import", "手动导入分集", libTaskParams{Source: id, Items: []libImportItem{item}})
}
func (s *Server) libBatchImport(w http.ResponseWriter, r *http.Request) {
	if !s.libAuthorized(w, r) {
		return
	}
	id, e := idParam(r, "sourceId")
	var b struct {
		Items []libImportItem `json:"items"`
	}
	if e != nil || readJSON(r, &b) != nil || len(b.Items) == 0 || len(b.Items) > 1000 {
		httpError(w, 422, "items is required (max 1000)")
		return
	}
	for _, v := range b.Items {
		if v.Index < 1 || v.Content == "" && v.URL == "" {
			httpError(w, 422, "Each item requires episodeIndex and content")
			return
		}
	}
	if e = s.libExisting(r, "anime_sources", id); e != nil {
		libWriteError(w, e)
		return
	}
	for i := range b.Items {
		b.Items[i].ContentRef = ""
	}
	s.libSubmit(w, r, "library_import", "批量手动导入", libTaskParams{Source: id, Items: b.Items})
}
func libParseCustom(content string) ([]danmaku.Comment, error) {
	content = strings.TrimSpace(content)
	if strings.HasPrefix(content, "<") {
		out, e := danmaku.Parse(strings.NewReader(content))
		if e != nil {
			return nil, e
		}
		for i := range out {
			p := strings.Split(out[i].P, ",")
			if len(p) >= 4 {
				out[i].P = strings.Join(p[:4], ",") + ",[custom_xml]"
			}
		}
		return out, nil
	}
	var b strings.Builder
	b.WriteString("<i>")
	for _, line := range strings.Split(content, "\n") {
		p, text, ok := strings.Cut(line, "|")
		if !ok {
			continue
		}
		parts := strings.Split(p, ",")
		if len(parts) < 4 {
			continue
		}
		param := strings.Join(parts[:4], ",") + ",[custom_xml]"
		b.WriteString(`<d p="`)
		xml.EscapeText(&b, []byte(param))
		b.WriteString(`">`)
		xml.EscapeText(&b, []byte(strings.TrimSpace(text)))
		b.WriteString("</d>")
	}
	b.WriteString("</i>")
	return danmaku.Parse(strings.NewReader(b.String()))
}
func (s *Server) libResolveURL(ctx context.Context, url string) (provider.Provider, provider.Episode, error) {
	name, media, epid, e := provider.ResolveURL(url)
	if e != nil {
		return nil, provider.Episode{}, e
	}
	p, ok := s.Providers.Get(name)
	if !ok {
		return nil, provider.Episode{}, errors.New("provider unavailable")
	}
	if resolver, ok := p.(interface {
		ResolveEpisode(context.Context, string) (provider.Episode, error)
	}); ok {
		ep, e := resolver.ResolveEpisode(ctx, url)
		return p, ep, e
	}
	if epid != "" {
		return p, provider.Episode{ID: epid, URL: url}, nil
	}
	episodes, e := p.Episodes(ctx, media)
	if e != nil {
		return nil, provider.Episode{}, e
	}
	for _, ep := range episodes {
		if ep.URL == url {
			return p, ep, nil
		}
	}
	if len(episodes) == 1 {
		return p, episodes[0], nil
	}
	return nil, provider.Episode{}, errors.New("URL does not identify exactly one episode")
}
func (s *Server) libUpsertEpisode(ctx context.Context, source store.Row, index int64, title, url, providerID string) (store.Row, error) {
	return s.prepareWorkflowEpisode(ctx, source, index, title, url, providerID, true)
}

// libWorkflowEpisodeSnapshot captures an existing target before any provider
// request. A source move during URL resolution or listing must not supply a new
// identity for an old request. Missing rows are created only after preflight.
func (s *Server) libWorkflowEpisodeSnapshot(ctx context.Context, source store.Row, index int64) (store.Row, error) {
	if index < 1 {
		return nil, libErr(422, "Episode index must be positive")
	}
	var episode store.Row
	err := s.libTransaction(ctx, func(tx *sql.Tx) error {
		if err := s.validateSourceSnapshot(ctx, tx, source); err != nil {
			return err
		}
		query := "SELECT * FROM " + s.Store.Quote("episode") + " WHERE source_id = ? AND episode_index = ?"
		if s.Store.Dialect != "sqlite" {
			query += " FOR UPDATE"
		}
		rows, err := s.libRows(ctx, tx, "episode", query, source["id"], index)
		if err != nil {
			return err
		}
		if len(rows) > 1 {
			return errors.New("Ambiguous existing episode index")
		}
		if len(rows) == 1 {
			episode = rows[0]
		}
		return nil
	})
	return episode, err
}

func (s *Server) libPrepareImportEpisode(ctx context.Context, source, existing store.Row, index int64, title, url, providerID string, bindProvider bool) (store.Row, error) {
	if index < 1 {
		return nil, libErr(422, "Episode index must be positive")
	}
	return s.prepareWorkflowEpisodeExpected(ctx, source, existing, index, title, url, providerID, bindProvider)
}

// Only fixed reason codes and local episode indices enter failed-task results.
// Earlier publications stay committed; remaining episodes have not been tried.
func libWorkflowFailure(ctx context.Context, operation string, indices []int64, at int, completed []int64, published, skipped, empty int, reason string, cause error) (job.DiagnosticResult, error) {
	failed := []int64{indices[at]}
	remaining := indices[at+1:]
	capList := func(v []int64) []int64 { return append([]int64{}, v[:min(len(v), 256)]...) }
	if errors.Is(cause, errWorkflowEpisodeConflict) {
		reason = "episode_provider_mapping_conflict"
	}
	out := job.DiagnosticResult{
		"operation": operation, "status": "partial_failure", "reason": reason, "partial": len(completed) > 0,
		"processed": len(completed), "completedCount": len(completed), "completedIndices": capList(completed),
		"failedCount": 1, "failedIndices": failed,
		"unprocessedCount": len(remaining), "unprocessedIndices": capList(remaining),
		"diagnosticsTruncated": len(completed) > 256 || len(remaining) > 256,
		"skipped":              skipped, "empty": empty,
	}
	if operation == "import" {
		out["imported"] = published
	} else {
		out["refreshed"] = published
	}
	// A provider-local timeout does not cancel the whole job. Keep its partial
	// outcome persistable; the manager drops diagnostics for actual cancellation.
	if err := ctx.Err(); err != nil {
		return out, err
	}
	if errors.Is(cause, errWorkflowEpisodeConflict) {
		return out, fmt.Errorf("Library %s stopped at episode %d: existing provider identity conflicts; deliberate remapping or a separate source is required; earlier completed episodes remain committed", operation, indices[at])
	}
	return out, fmt.Errorf("Library %s stopped at episode %d (%s); earlier completed episodes remain committed", operation, indices[at], reason)
}

func (s *Server) libImportJob(ctx context.Context, raw json.RawMessage, progress func(int, string)) (any, error) {
	p, e := libDecodeTask(raw)
	if e != nil {
		return nil, e
	}
	source, e := s.Store.Get(ctx, "anime_sources", p.Source)
	if e != nil || source == nil {
		return nil, errors.New("source missing")
	}
	initial, e := s.captureWorkflowEpisodes(ctx, source)
	if e != nil {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return nil, errors.New("Source episode snapshot failed; no episodes were processed")
	}
	indices := make([]int64, len(p.Items))
	for i, item := range p.Items {
		indices[i] = item.Index
	}
	completed := []int64{}
	imported, empty := 0, 0
	for i, item := range p.Items {
		fail := func(reason string, err error) (any, error) {
			return libWorkflowFailure(ctx, "import", indices, i, completed, imported, 0, empty, reason, err)
		}
		if e = job.Checkpoint(ctx); e != nil {
			return fail("interrupted", e)
		}
		existing := initial[item.Index]
		content := item.Content
		if item.ContentRef != "" {
			content, e = s.libReadImport(item.ContentRef)
			if e != nil {
				return fail("invalid_import_payload", e)
			}
		}
		content = strings.TrimSpace(content)
		inline := (item.Content != "" || item.ContentRef != "") && item.URLProvider == "" && !strings.HasPrefix(content, "https://") && !strings.HasPrefix(content, "http://")
		if content == "" {
			content = strings.TrimSpace(item.URL)
			inline = false
		}
		var comments []danmaku.Comment
		var record store.Row
		providerID, url := "custom_xml", "from_xml"
		title := item.Title
		if inline {
			comments, e = libParseCustom(content)
			if e != nil {
				return fail("invalid_import_payload", e)
			}
			if title == "" {
				title = fmt.Sprintf("第 %d 集", item.Index)
			}
			if len(comments) > 0 {
				record, e = s.libPrepareImportEpisode(ctx, source, existing, item.Index, title, url, providerID, false)
			}
		} else {
			pr, ep, err := s.libResolveURL(ctx, content)
			if err != nil {
				return fail("url_resolution_failed", err)
			}
			if (item.URLProvider != "" && pr.Name() != item.URLProvider) || (authString(source["provider_name"]) != "custom" && pr.Name() != authString(source["provider_name"])) {
				return fail("url_provider_mismatch", errors.New("URL provider does not match target"))
			}
			providerID, url = ep.ID, content
			if item.ExpectedProviderEpisodeID != "" && providerID != item.ExpectedProviderEpisodeID {
				return fail("collection_episode_identity_changed", errors.New("Collection primary pool changed after listing; preview and submit again"))
			}
			if providerID == "" {
				return fail("missing_provider_episode_id", errors.New("URL did not resolve an episode identity"))
			}
			if title == "" {
				title = ep.Title
			}
			if title == "" {
				title = fmt.Sprintf("第 %d 集", item.Index)
			}
			record, e = s.libPrepareImportEpisode(ctx, source, existing, item.Index, title, url, providerID, true)
			if e != nil {
				return fail("target_identity_conflict", e)
			}
			if authString(source["provider_name"]) == "custom" {
				origin, _, _, originErr := provider.ResolveURL(authString(record["source_url"]))
				if originErr != nil || origin != pr.Name() {
					return fail("custom_source_origin_conflict", errors.New("Existing custom-source episode origin cannot be matched to this provider"))
				}
			}
			comments, e = pr.Comments(ctx, ep.ID)
		}
		if e != nil {
			return fail("episode_import_failed", e)
		}
		if len(comments) > 0 {
			if e = s.saveCommentsSnapshot(ctx, record, source, comments, true); e != nil {
				return fail("publication_rejected", e)
			}
			imported++
		} else {
			// Empty results still must not turn a changed target into a success.
			if record == nil && existing != nil {
				_, e = s.libPrepareImportEpisode(ctx, source, existing, item.Index, title, url, providerID, false)
			} else if record == nil {
				_, e = s.libWorkflowEpisodeSnapshot(ctx, source, item.Index)
			} else {
				_, e = s.libPrepareImportEpisode(ctx, source, record, item.Index, title, url, providerID, !inline)
			}
			if e != nil {
				return fail("target_identity_changed", e)
			}
			empty++
		}
		completed = append(completed, item.Index)
		progress(len(completed)*100/len(p.Items), fmt.Sprintf("已处理 %d/%d 个分集", len(completed), len(p.Items)))
	}
	return store.Row{"processed": len(completed), "imported": imported, "empty": empty}, nil
}
func (s *Server) libSourceRefresh(w http.ResponseWriter, r *http.Request) {
	if !s.libAuthorized(w, r) {
		return
	}
	id, e := idParam(r, "sourceId")
	if e != nil {
		httpError(w, 422, "Invalid ID")
		return
	}
	mode := r.URL.Query().Get("mode")
	if mode == "" {
		mode = "full"
	}
	if mode != "full" && mode != "incremental" {
		httpError(w, 422, "mode must be full or incremental")
		return
	}
	if e = s.libExisting(r, "anime_sources", id); e != nil {
		libWriteError(w, e)
		return
	}
	s.libSubmit(w, r, "library_source_refresh", "刷新数据源", libTaskParams{Source: id, Mode: mode})
}
func (s *Server) libSourceRefreshJob(ctx context.Context, raw json.RawMessage, progress func(int, string)) (any, error) {
	// Refresh has only the provider's native coordinates. Preserve any existing
	// provider-to-storage assignment at admission and again at publication.
	ctx = context.WithValue(ctx, mediaGroupOwnershipContextKey{}, true)
	p, e := libDecodeTask(raw)
	if e != nil {
		return nil, e
	}
	src, e := s.Store.Get(ctx, "anime_sources", p.Source)
	if e != nil || src == nil {
		return nil, errors.New("source missing")
	}
	pr, ok := s.Providers.Get(authString(src["provider_name"]))
	if !ok {
		return nil, errors.New("provider unavailable")
	}
	if e = s.validateSourceRefreshGrouping(ctx, src); e != nil {
		return nil, e
	}
	initial, e := s.captureWorkflowEpisodes(ctx, src)
	if e != nil {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return nil, errors.New("Source episode snapshot failed; no episodes were processed")
	}
	eps, e := pr.Episodes(ctx, authString(src["media_id"]))
	if e != nil {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return nil, errors.New("Source episode listing failed; no episodes were processed")
	}
	indices := make([]int64, len(eps))
	seen := make(map[int64]bool, len(eps))
	for i, ep := range eps {
		index := int64(ep.Index)
		if index < 0 || seen[index] || (index == 0 && initial[0] == nil) {
			return nil, errors.New("Source listing contains invalid or duplicate episode indices; no episodes were processed")
		}
		seen[index] = true
		indices[i] = index
	}
	if e = validateSourceRefreshProviderOwnership(ctx, initial, eps); e != nil {
		return nil, e
	}
	completed := []int64{}
	done, skipped := 0, 0
	for i, ep := range eps {
		fail := func(reason string, err error) (any, error) {
			return libWorkflowFailure(ctx, "refresh", indices, i, completed, done, skipped, 0, reason, err)
		}
		if e = job.Checkpoint(ctx); e != nil {
			return fail("interrupted", e)
		}
		existing := initial[int64(ep.Index)]
		if ep.ID == "" {
			return fail("missing_provider_episode_id", errors.New("Provider episode identity is missing"))
		}
		// Existing episode zero can originate from a movie import. Refresh
		// preserves it; the listing preflight forbids creating a new zero row.
		record, e := s.prepareWorkflowEpisodeExpected(ctx, src, existing, int64(ep.Index), ep.Title, ep.URL, ep.ID, true)
		if e != nil {
			return fail("target_identity_conflict", e)
		}
		if p.Mode == "incremental" && existing != nil {
			skipped++
		} else {
			comments, e := pr.Comments(ctx, ep.ID)
			if e != nil {
				return fail("provider_comments_failed", e)
			}
			if e = s.saveCommentsSnapshot(ctx, record, src, comments, true); e != nil {
				return fail("publication_rejected", e)
			}
			done++
		}
		completed = append(completed, int64(ep.Index))
		progress((i+1)*100/len(eps), fmt.Sprintf("已处理 %d/%d 集", i+1, len(eps)))
	}
	// Even an empty listing must retain the source observed before the request.
	if len(eps) == 0 {
		if e = s.libTransaction(ctx, func(tx *sql.Tx) error { return s.validateSourceSnapshot(ctx, tx, src) }); e != nil {
			return nil, errors.New("Source identity changed during episode listing; reload and retry")
		}
	}
	return store.Row{"refreshed": done}, nil
}
func (s *Server) libCollectionImport(w http.ResponseWriter, r *http.Request) {
	if !s.libAuthorized(w, r) {
		return
	}
	id, e := idParam(r, "sourceId")
	var b struct {
		URL              string `json:"url"`
		Start            int64  `json:"startEpisodeIndex"`
		Title            string `json:"title"`
		CollectionSeason string `json:"collection_season_id"`
		CollectionMid    string `json:"collection_mid"`
	}
	if e != nil || readJSON(r, &b) != nil || b.URL == "" {
		httpError(w, 422, "url is required")
		return
	}
	if err := validateCollectionSelection(b.CollectionSeason, b.CollectionMid); err != nil {
		libWriteError(w, err)
		return
	}
	if e = s.libExisting(r, "anime_sources", id); e != nil {
		libWriteError(w, e)
		return
	}
	name, _, _, e := provider.ResolveURL(b.URL)
	if e != nil {
		httpError(w, 400, e.Error())
		return
	}
	pr, ok := s.Providers.Get(name)
	if !ok {
		httpError(w, 503, "Provider unavailable")
		return
	}
	collection, ok := pr.(interface {
		Collection(context.Context, string) ([]provider.Episode, error)
	})
	if !ok {
		httpError(w, 501, "This provider does not yet implement collection import in Go")
		return
	}
	var eps []provider.Episode
	if b.CollectionSeason != "" {
		_, eps, e = resolveCollectionSelection(r.Context(), pr, b.URL, b.CollectionSeason, b.CollectionMid)
		if e != nil {
			libWriteError(w, e)
			return
		}
	} else {
		eps, e = collection.Collection(r.Context(), b.URL)
		if e != nil {
			httpError(w, 502, e.Error())
			return
		}
	}
	if len(eps) == 0 {
		httpError(w, 400, "No collection videos found")
		return
	}
	if b.Start < 1 {
		b.Start = 1
	}
	items := []libImportItem{}
	for i, ep := range eps {
		if ep.URL == "" || ep.ID == "" {
			httpError(w, 502, "Provider returned an episode without a URL")
			return
		}
		items = append(items, libImportItem{Title: ep.Title, Index: b.Start + int64(i), Content: ep.URL, URLProvider: name, ExpectedProviderEpisodeID: ep.ID})
	}
	s.libSubmit(w, r, "library_import", "导入合集", libTaskParams{Source: id, Items: items})
}
