// SPDX-License-Identifier: AGPL-3.0-only
package server

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/AniBakaBaka/AniDan/internal/job"
	"github.com/AniBakaBaka/AniDan/internal/media"
	"github.com/AniBakaBaka/AniDan/internal/provider"
	"github.com/AniBakaBaka/AniDan/internal/recognition"
	"github.com/AniBakaBaka/AniDan/internal/store"
)

func (s *Server) registerMedia(m *http.ServeMux) {
	for _, p := range []struct {
		route string
		h     http.HandlerFunc
	}{{"GET /api/ui/media-servers", s.mediaServers}, {"POST /api/ui/media-servers", s.mediaServerCreate}, {"PUT /api/ui/media-servers/{server_id}", s.mediaServerUpdate}, {"DELETE /api/ui/media-servers/{server_id}", s.mediaServerDelete}, {"POST /api/ui/media-servers/{server_id}/test", s.mediaServerTest}, {"GET /api/ui/media-servers/{server_id}/libraries", s.mediaLibraries}, {"POST /api/ui/media-servers/{server_id}/scan", s.mediaScan}, {"GET /api/ui/media-servers/{server_id}/image", s.mediaImage}, {"GET /api/ui/media-items", s.mediaItems}, {"GET /api/ui/media-works", s.mediaWorks}, {"GET /api/ui/shows/{title}/seasons", s.mediaSeasons}, {"GET /api/ui/shows/{title}/seasons/{season}/episodes", s.mediaEpisodes}, {"PUT /api/ui/media-items/{item_id}", s.mediaItemUpdate}, {"DELETE /api/ui/media-items/{item_id}", s.mediaItemDelete}, {"POST /api/ui/media-items/batch-delete", s.mediaBatchDelete}, {"POST /api/ui/media-items/import", s.mediaImport}, {"GET /api/ui/media-items/unimported-count", s.mediaUnimportedCount}, {"POST /api/ui/media-items/import-all-unimported", s.mediaImportAll}} {
		m.HandleFunc(p.route, s.operator(p.h))
	}
	if s.Jobs != nil {
		for k, h := range map[string]job.Handler{"scan_media_server": s.runMediaScan, "import_media_items": s.runMediaImport} {
			if e := s.Jobs.Register(k, h); e != nil {
				panic(e)
			}
		}
	}
	s.registerLocal(m)
	s.registerWebhook(m)
}
func mediaCamelRow(r store.Row) map[string]any {
	out := map[string]any{}
	for k, v := range r {
		parts := strings.Split(k, "_")
		name := parts[0]
		for _, p := range parts[1:] {
			if p != "" {
				name += strings.ToUpper(p[:1]) + p[1:]
			}
		}
		out[name] = v
	}
	return out
}
func mediaServerJSON(r store.Row) map[string]any {
	v := mediaCamelRow(r)
	var libs []string
	var rules map[string]any
	_ = json.Unmarshal([]byte(str(r["selected_libraries"])), &libs)
	_ = json.Unmarshal([]byte(str(r["filter_rules"])), &rules)
	if libs == nil {
		libs = []string{}
	}
	if rules == nil {
		rules = map[string]any{}
	}
	v["selectedLibraries"] = libs
	v["filterRules"] = rules
	return v
}
func (s *Server) mediaServers(w http.ResponseWriter, r *http.Request) {
	rows, e := s.Store.List(r.Context(), "media_servers", nil, 10000, 0)
	if e != nil {
		httpError(w, 500, e.Error())
		return
	}
	out := []any{}
	for _, row := range rows {
		out = append(out, mediaServerJSON(row))
	}
	writeJSON(w, 200, out)
}
func mediaServerFields(in map[string]any, creating bool) (store.Row, error) {
	mapping := map[string]string{"name": "name", "providerName": "provider_name", "url": "url", "apiToken": "api_token", "isEnabled": "is_enabled", "selectedLibraries": "selected_libraries", "filterRules": "filter_rules"}
	out := store.Row{}
	for k, v := range in {
		col, ok := mapping[k]
		if !ok {
			return nil, fmt.Errorf("unknown media server field %s", k)
		}
		if k == "selectedLibraries" || k == "filterRules" {
			if k == "selectedLibraries" {
				a, ok := v.([]any)
				if !ok {
					return nil, errors.New("selectedLibraries must be an array")
				}
				for _, id := range a {
					if _, ok = id.(string); !ok {
						return nil, errors.New("library IDs must be strings")
					}
				}
			} else {
				if _, ok := v.(map[string]any); !ok {
					return nil, errors.New("filterRules must be an object")
				}
			}
			b, e := json.Marshal(v)
			if e != nil {
				return nil, e
			}
			out[col] = string(b)
		} else {
			out[col] = v
		}
	}
	if creating {
		for _, k := range []string{"name", "provider_name", "url", "api_token"} {
			if strings.TrimSpace(str(out[k])) == "" {
				return nil, fmt.Errorf("%s required", k)
			}
		}
		if _, ok := out["selected_libraries"]; !ok {
			out["selected_libraries"] = "[]"
		}
		if _, ok := out["filter_rules"]; !ok {
			out["filter_rules"] = "{}"
		}
	}
	return out, nil
}
func (s *Server) mediaServerCreate(w http.ResponseWriter, r *http.Request) {
	var in map[string]any
	if e := readJSON(r, &in); e != nil {
		httpError(w, 422, e.Error())
		return
	}
	row, e := mediaServerFields(in, true)
	if e != nil {
		httpError(w, 422, e.Error())
		return
	}
	if _, e = media.NewClient(str(row["provider_name"]), str(row["url"]), str(row["api_token"]), nil); e != nil {
		httpError(w, 422, e.Error())
		return
	}
	row["created_at"] = s.now()
	row["updated_at"] = s.now()
	id, e := s.Store.Insert(r.Context(), "media_servers", row)
	if e != nil {
		httpError(w, 409, e.Error())
		return
	}
	saved, e := s.Store.Get(r.Context(), "media_servers", id)
	if e != nil {
		httpError(w, 500, e.Error())
		return
	}
	writeJSON(w, 201, mediaServerJSON(saved))
}
func (s *Server) mediaServerRow(r *http.Request) (store.Row, error) {
	id, e := idParam(r, "server_id")
	if e != nil {
		return nil, e
	}
	return s.Store.Get(r.Context(), "media_servers", id)
}
func (s *Server) mediaServerUpdate(w http.ResponseWriter, r *http.Request) {
	old, e := s.mediaServerRow(r)
	if e != nil || old == nil {
		httpError(w, 404, "Media server not found")
		return
	}
	var in map[string]any
	if e = readJSON(r, &in); e != nil {
		httpError(w, 422, e.Error())
		return
	}
	row, e := mediaServerFields(in, false)
	if e != nil {
		httpError(w, 422, e.Error())
		return
	}
	merged := store.Row{}
	for k, v := range old {
		merged[k] = v
	}
	for k, v := range row {
		merged[k] = v
	}
	if _, e = media.NewClient(str(merged["provider_name"]), str(merged["url"]), str(merged["api_token"]), nil); e != nil {
		httpError(w, 422, e.Error())
		return
	}
	row["updated_at"] = s.now()
	if e = s.Store.Update(r.Context(), "media_servers", old["id"], row); e != nil {
		httpError(w, 409, e.Error())
		return
	}
	saved, e := s.Store.Get(r.Context(), "media_servers", old["id"])
	if e != nil {
		httpError(w, 500, e.Error())
		return
	}
	writeJSON(w, 200, mediaServerJSON(saved))
}
func (s *Server) mediaServerDelete(w http.ResponseWriter, r *http.Request) {
	row, e := s.mediaServerRow(r)
	if e != nil || row == nil {
		httpError(w, 404, "Media server not found")
		return
	}
	if e = s.Store.Delete(r.Context(), "media_servers", row["id"]); e != nil {
		httpError(w, 500, e.Error())
		return
	}
	writeJSON(w, 204, nil)
}
func (s *Server) mediaClient(row store.Row) (*media.Client, error) {
	return media.NewClient(str(row["provider_name"]), str(row["url"]), str(row["api_token"]), nil)
}
func (s *Server) mediaServerTest(w http.ResponseWriter, r *http.Request) {
	row, e := s.mediaServerRow(r)
	if e != nil || row == nil {
		httpError(w, 404, "Media server not found")
		return
	}
	c, e := s.mediaClient(row)
	if e != nil {
		httpError(w, 422, e.Error())
		return
	}
	info, e := c.Info(r.Context())
	if e != nil {
		writeJSON(w, 200, map[string]any{"success": false, "message": e.Error(), "serverInfo": nil})
		return
	}
	writeJSON(w, 200, map[string]any{"success": true, "message": "连接成功", "serverInfo": info})
}
func (s *Server) mediaLibraries(w http.ResponseWriter, r *http.Request) {
	row, e := s.mediaServerRow(r)
	if e != nil || row == nil {
		httpError(w, 404, "Media server not found")
		return
	}
	c, e := s.mediaClient(row)
	if e != nil {
		httpError(w, 422, e.Error())
		return
	}
	libs, e := c.Libraries(r.Context())
	if e != nil {
		httpError(w, 502, e.Error())
		return
	}
	writeJSON(w, 200, libs)
}
func (s *Server) mediaImage(w http.ResponseWriter, r *http.Request) {
	row, e := s.mediaServerRow(r)
	if e != nil || row == nil {
		httpError(w, 404, "Media server not found")
		return
	}
	c, e := s.mediaClient(row)
	if e != nil {
		httpError(w, 422, e.Error())
		return
	}
	body, typ, e := c.Image(r.Context(), r.URL.Query().Get("path"))
	if e != nil {
		httpError(w, 502, e.Error())
		return
	}
	defer body.Close()
	data, e := io.ReadAll(io.LimitReader(body, (16<<20)+1))
	if e != nil || len(data) > 16<<20 {
		httpError(w, 502, "Media poster exceeds limit or could not be read")
		return
	}
	w.Header().Set("Content-Type", typ)
	w.Header().Set("Cache-Control", "private, max-age=300")
	_, _ = w.Write(data)
}

type mediaTaskParams struct {
	ServerID   int64    `json:"serverId,omitempty"`
	LibraryIDs []string `json:"library_ids,omitempty"`
	MediaType  string   `json:"mediaType,omitempty"`
	ItemIDs    []int64  `json:"itemIds,omitempty"`
	All        bool     `json:"all,omitempty"`
}

func (s *Server) mediaSubmit(w http.ResponseWriter, r *http.Request, kind, title string, params any, unique string) {
	id, e := s.Jobs.SubmitWithOptions(kind, params, nil, job.SubmitOptions{Title: title, QueueType: "management", UniqueKey: unique})
	if e != nil {
		httpError(w, 409, e.Error())
		return
	}
	writeJSON(w, 202, map[string]any{"message": title + "任务已提交", "taskId": id})
}
func (s *Server) mediaScan(w http.ResponseWriter, r *http.Request) {
	row, e := s.mediaServerRow(r)
	if e != nil || row == nil {
		httpError(w, 404, "Media server not found")
		return
	}
	if !boolean(row["is_enabled"]) {
		httpError(w, 409, "Media server is disabled")
		return
	}
	var p mediaTaskParams
	if e = readJSON(r, &p); e != nil {
		httpError(w, 422, e.Error())
		return
	}
	p.ServerID = number(row["id"])
	s.mediaSubmit(w, r, "scan_media_server", "扫描媒体服务器: "+str(row["name"]), p, fmt.Sprintf("scan-media-server-%d", p.ServerID))
}
func mediaItemRow(item media.Item) store.Row {
	r := store.Row{"media_id": item.MediaID, "title": item.Title, "media_type": item.MediaType, "library_id": nil, "series_id": nil, "season_id": nil, "episode_id": nil, "season": nil, "episode": nil, "year": nil, "tmdb_id": nil, "tvdb_id": nil, "imdb_id": nil, "poster_url": nil}
	for k, v := range map[string]string{"library_id": item.LibraryID, "series_id": item.SeriesID, "season_id": item.SeasonID, "episode_id": item.EpisodeID, "tmdb_id": item.TMDBID, "tvdb_id": item.TVDBID, "imdb_id": item.IMDBID, "poster_url": item.PosterURL} {
		if v != "" {
			r[k] = v
		}
	}
	for k, v := range map[string]*int{"season": item.Season, "episode": item.Episode, "year": item.Year} {
		if v != nil {
			r[k] = *v
		}
	}
	return r
}
func (s *Server) runMediaScan(ctx context.Context, raw json.RawMessage, progress func(int, string)) (any, error) {
	var p mediaTaskParams
	if e := unmarshalExactJSON(raw, &p); e != nil {
		return nil, e
	}
	row, e := s.Store.Get(ctx, "media_servers", p.ServerID)
	if e != nil {
		return nil, e
	}
	if row == nil || !boolean(row["is_enabled"]) {
		return nil, errors.New("media server missing or disabled")
	}
	c, e := s.mediaClient(row)
	if e != nil {
		return nil, e
	}
	libs := p.LibraryIDs
	if len(libs) == 0 {
		if e = json.Unmarshal([]byte(str(row["selected_libraries"])), &libs); e != nil {
			return nil, e
		}
	}
	if len(libs) == 0 {
		available, e := c.Libraries(ctx)
		if e != nil {
			return nil, e
		}
		for _, lib := range available {
			libs = append(libs, lib.ID)
		}
	}
	count := 0
	for i, library := range libs {
		if e = job.Checkpoint(ctx); e != nil {
			return nil, e
		}
		progress(i*100/max(1, len(libs)), "扫描媒体库 "+library)
		e = c.Scan(ctx, library, p.MediaType, func(item media.Item) error {
			if e := job.Checkpoint(ctx); e != nil {
				return e
			}
			if item.MediaID == "" || item.Title == "" {
				return errors.New("media server returned item without identity/title")
			}
			rec := mediaItemRow(item)
			rec["server_id"] = p.ServerID
			rec["updated_at"] = s.now()
			if item.PosterURL != "" {
				rec["poster_url"] = fmt.Sprintf("/api/ui/media-servers/%d/image?path=%s", p.ServerID, url.QueryEscape(item.PosterURL))
			}
			existing, e := s.Store.List(ctx, "media_items", store.Row{"server_id": p.ServerID, "media_id": item.MediaID}, 1, 0)
			if e != nil {
				return e
			}
			if len(existing) > 0 {
				e = s.Store.Update(ctx, "media_items", existing[0]["id"], rec)
			} else {
				rec["created_at"] = s.now()
				_, e = s.Store.Insert(ctx, "media_items", rec)
			}
			if e == nil {
				count++
			}
			return e
		})
		if e != nil {
			return map[string]any{"scanned": count}, e
		}
	}
	return map[string]any{"scanned": count, "serverId": p.ServerID}, nil
}
func mediaPage(r *http.Request) (int, int) {
	p, _ := strconv.Atoi(r.URL.Query().Get("page"))
	size, _ := strconv.Atoi(r.URL.Query().Get("page_size"))
	if size <= 0 {
		size, _ = strconv.Atoi(r.URL.Query().Get("pageSize"))
	}
	if p < 1 {
		p = 1
	}
	if size < 1 {
		size = 100
	}
	if size > 500 {
		size = 500
	}
	return p, size
}
func mediaFilters(r *http.Request, local bool) (store.Row, error) {
	f := store.Row{}
	q := r.URL.Query()
	if !local && q.Get("server_id") != "" {
		id, e := strconv.ParseInt(q.Get("server_id"), 10, 64)
		if e != nil {
			return nil, e
		}
		f["server_id"] = id
	}
	if typ := q.Get("media_type"); typ != "" {
		if typ != "movie" && typ != "tv_series" {
			return nil, errors.New("media_type must be movie or tv_series")
		}
		f["media_type"] = typ
	}
	if q.Get("is_imported") != "" {
		v, e := strconv.ParseBool(q.Get("is_imported"))
		if e != nil {
			return nil, e
		}
		f["is_imported"] = v
	}
	return f, nil
}
func (s *Server) mediaQueryable(table string) string {
	if table != "media_items" {
		return s.Store.Quote(table)
	}
	parts := []string{}
	for _, c := range store.Schema[table].Columns {
		if c.Name == "is_imported" {
			parts = append(parts, "EXISTS(SELECT 1 FROM anime a JOIN anime_sources src ON src.anime_id=a.id JOIN episode ep ON ep.source_id=src.id WHERE REPLACE(a.title,' ','')=REPLACE(m.title,' ','') AND a.type=m.media_type AND (m.media_type<>'tv_series' OR a.season=COALESCE(m.season,1)) AND (m.media_type<>'tv_series' OR ep.episode_index=COALESCE(m.episode,1)) AND ep.comment_count>0) AS "+s.Store.Quote(c.Name))
		} else {
			parts = append(parts, "m."+s.Store.Quote(c.Name))
		}
	}
	return "(SELECT " + strings.Join(parts, ",") + " FROM " + s.Store.Quote(table) + " m) media_view"
}
func (s *Server) mediaList(w http.ResponseWriter, r *http.Request, table string, extra store.Row) {
	local := table == "local_danmaku_items"
	where, args, e := s.mediaWhere(r, table)
	if e != nil {
		httpError(w, 422, e.Error())
		return
	}
	keys := []string{}
	for k := range extra {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if where == "" {
			where = " WHERE "
		} else {
			where += " AND "
		}
		where += s.Store.Quote(k) + "=?"
		args = append(args, extra[k])
	}
	page, size := mediaPage(r)
	from := " FROM " + s.mediaQueryable(table) + where
	var count int64
	if e = s.Store.DB.QueryRowContext(r.Context(), s.Store.Rebind("SELECT COUNT(*)"+from), args...).Scan(&count); e != nil {
		httpError(w, 500, e.Error())
		return
	}
	order := "created_at DESC,id DESC"
	if strings.HasSuffix(r.URL.Path, "/episodes") {
		order = "episode,id"
	}
	a := append(append([]any{}, args...), size, (page-1)*size)
	rows, e := s.Store.DB.QueryContext(r.Context(), s.Store.Rebind("SELECT *"+from+" ORDER BY "+order+" LIMIT ? OFFSET ?"), a...)
	if e != nil {
		httpError(w, 500, e.Error())
		return
	}
	out := []any{}
	for rows.Next() {
		row, err := store.ScanRow(rows, store.Schema[table])
		if err != nil {
			rows.Close()
			httpError(w, 500, err.Error())
			return
		}
		v := mediaCamelRow(row)
		if local && str(row["poster_url"]) != "" && !strings.HasPrefix(str(row["poster_url"]), "http") {
			v["posterUrl"] = fmt.Sprintf("/api/ui/local-items/%s/poster", str(row["id"]))
		}
		out = append(out, v)
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		httpError(w, 500, e.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"total": count, "list": out, "items": out, "page": page, "pageSize": size, "page_size": size})
}
func (s *Server) mediaItems(w http.ResponseWriter, r *http.Request) {
	s.mediaList(w, r, "media_items", nil)
}
func (s *Server) mediaEpisodes(w http.ResponseWriter, r *http.Request) {
	season, e := strconv.ParseInt(r.PathValue("season"), 10, 64)
	if e != nil {
		httpError(w, 422, "Invalid season")
		return
	}
	s.mediaList(w, r, "media_items", store.Row{"title": r.PathValue("title"), "season": season, "media_type": "tv_series"})
}
func (s *Server) mediaItemUpdate(w http.ResponseWriter, r *http.Request) {
	s.mediaUpdateItem(w, r, "media_items", 200)
}
func (s *Server) mediaUpdateItem(w http.ResponseWriter, r *http.Request, table string, status int) {
	id, e := idParam(r, "item_id")
	if e != nil {
		httpError(w, 422, e.Error())
		return
	}
	row, e := s.Store.Get(r.Context(), table, id)
	if e != nil || row == nil {
		httpError(w, 404, "Item not found")
		return
	}
	var in map[string]any
	if e = readJSON(r, &in); e != nil {
		httpError(w, 422, e.Error())
		return
	}
	allowed := map[string]string{"title": "title", "mediaType": "media_type", "season": "season", "episode": "episode", "year": "year", "tmdbId": "tmdb_id", "tvdbId": "tvdb_id", "imdbId": "imdb_id", "posterUrl": "poster_url"}
	if table == "local_danmaku_items" {
		allowed["filePath"] = "file_path"
	}
	changes := store.Row{}
	for k, v := range in {
		col, ok := allowed[k]
		if !ok {
			httpError(w, 422, "Unknown field: "+k)
			return
		}
		if col == "file_path" {
			if _, e = s.localAllowedPath(str(v), false); e != nil {
				httpError(w, 403, e.Error())
				return
			}
		}
		changes[col] = v
	}
	if len(changes) == 0 {
		httpError(w, 400, "No fields to update")
		return
	}
	changes["updated_at"] = s.now()
	if e = s.Store.Update(r.Context(), table, id, changes); e != nil {
		httpError(w, 422, e.Error())
		return
	}
	saved, e := s.Store.Get(r.Context(), table, id)
	if e != nil {
		httpError(w, 500, e.Error())
		return
	}
	writeJSON(w, status, mediaCamelRow(saved))
}
func (s *Server) mediaItemDelete(w http.ResponseWriter, r *http.Request) {
	s.mediaDeleteItem(w, r, "media_items")
}
func (s *Server) mediaDeleteItem(w http.ResponseWriter, r *http.Request, table string) {
	id, e := idParam(r, "item_id")
	if e != nil {
		httpError(w, 422, e.Error())
		return
	}
	row, e := s.Store.Get(r.Context(), table, id)
	if e != nil || row == nil {
		httpError(w, 404, "Item not found")
		return
	}
	if e = s.Store.Delete(r.Context(), table, id); e != nil {
		httpError(w, 500, e.Error())
		return
	}
	writeJSON(w, 204, nil)
}

type itemSelection struct {
	ItemIDs []json.RawMessage `json:"itemIds"`
	Shows   []struct {
		ServerID int64  `json:"serverId"`
		Title    string `json:"title"`
	} `json:"shows"`
	Seasons []struct {
		ServerID int64  `json:"serverId"`
		Title    string `json:"title"`
		Season   int    `json:"season"`
	} `json:"seasons"`
	Items []struct {
		ItemID   int64  `json:"itemId"`
		Provider string `json:"provider"`
		MediaID  string `json:"mediaId"`
	} `json:"items"`
}

func (s *Server) selectedItemIDs(ctx context.Context, table string, p itemSelection) ([]int64, error) {
	ids := map[int64]bool{}
	for _, raw := range p.ItemIDs {
		var id int64
		if unmarshalExactJSON(raw, &id) == nil {
			ids[id] = true
			continue
		}
		var list []int64
		if e := unmarshalExactJSON(raw, &list); e != nil {
			return nil, errors.New("itemIds must contain integer IDs or arrays of IDs")
		}
		for _, v := range list {
			ids[v] = true
		}
	}
	for _, v := range p.Items {
		ids[v.ItemID] = true
	}
	filters := []store.Row{}
	for _, show := range p.Shows {
		f := store.Row{"title": show.Title, "media_type": "tv_series"}
		if table == "media_items" {
			f["server_id"] = show.ServerID
		}
		filters = append(filters, f)
	}
	for _, season := range p.Seasons {
		f := store.Row{"title": season.Title, "season": season.Season, "media_type": "tv_series"}
		if table == "media_items" {
			f["server_id"] = season.ServerID
		}
		filters = append(filters, f)
	}
	for _, f := range filters {
		for offset := 0; ; offset += 500 {
			rows, e := s.Store.List(ctx, table, f, 500, offset)
			if e != nil {
				return nil, e
			}
			for _, row := range rows {
				ids[number(row["id"])] = true
			}
			if len(rows) < 500 {
				break
			}
		}
	}
	if len(ids) > 10000 {
		return nil, errors.New("select at most 10000 items per task")
	}
	out := []int64{}
	for id := range ids {
		if id < 1 {
			return nil, errors.New("invalid item ID")
		}
		out = append(out, id)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out, nil
}
func (s *Server) mediaBatchDelete(w http.ResponseWriter, r *http.Request) {
	s.mediaDeleteBatch(w, r, "media_items")
}
func (s *Server) mediaDeleteBatch(w http.ResponseWriter, r *http.Request, table string) {
	var p itemSelection
	if e := readJSON(r, &p); e != nil {
		httpError(w, 422, e.Error())
		return
	}
	ids, e := s.selectedItemIDs(r.Context(), table, p)
	if e != nil {
		httpError(w, 422, e.Error())
		return
	}
	tx, e := s.Store.DB.BeginTx(r.Context(), nil)
	if e != nil {
		httpError(w, 500, e.Error())
		return
	}
	defer tx.Rollback()
	var count int64
	for _, id := range ids {
		res, e := tx.ExecContext(r.Context(), s.Store.Rebind("DELETE FROM "+s.Store.Quote(table)+" WHERE id=?"), id)
		if e != nil {
			httpError(w, 500, e.Error())
			return
		}
		n, _ := res.RowsAffected()
		count += n
	}
	if e = tx.Commit(); e != nil {
		httpError(w, 500, e.Error())
		return
	}
	if table == "local_danmaku_items" {
		writeJSON(w, 204, nil)
	} else {
		writeJSON(w, 200, map[string]any{"message": fmt.Sprintf("成功删除 %d 个媒体项", count)})
	}
}
func (s *Server) mediaImport(w http.ResponseWriter, r *http.Request) {
	var p itemSelection
	if e := readJSON(r, &p); e != nil {
		httpError(w, 422, e.Error())
		return
	}
	ids, e := s.selectedItemIDs(r.Context(), "media_items", p)
	if e != nil {
		httpError(w, 422, e.Error())
		return
	}
	b, _ := json.Marshal(ids)
	h := sha256.Sum256(b)
	s.mediaSubmit(w, r, "import_media_items", "导入媒体项", mediaTaskParams{ItemIDs: ids}, fmt.Sprintf("media-import-%x", h[:16]))
}
func (s *Server) mediaUnimportedCount(w http.ResponseWriter, r *http.Request) {
	where, args, e := s.mediaWhere(r, "media_items")
	if e != nil {
		httpError(w, 422, e.Error())
		return
	}
	if where == "" {
		where = " WHERE "
	} else {
		where += " AND "
	}
	where += "NOT is_imported"
	var n int64
	e = s.Store.DB.QueryRowContext(r.Context(), s.Store.Rebind("SELECT COUNT(*) FROM "+s.mediaQueryable("media_items")+where), args...).Scan(&n)
	if e != nil {
		httpError(w, 500, e.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"count": n})
}
func (s *Server) mediaImportAll(w http.ResponseWriter, r *http.Request) {
	var p mediaTaskParams
	if e := readJSON(r, &p); e != nil || p.ServerID < 1 {
		httpError(w, 422, "serverId required")
		return
	}
	p.All = true
	s.mediaSubmit(w, r, "import_media_items", "一键导入全部未导入", p, fmt.Sprintf("media-import-all-%d-%s", p.ServerID, p.MediaType))
}
func (s *Server) runMediaImport(ctx context.Context, raw json.RawMessage, progress func(int, string)) (any, error) {
	var p mediaTaskParams
	if e := unmarshalExactJSON(raw, &p); e != nil {
		return nil, e
	}
	ids := p.ItemIDs
	if p.All {
		where := " WHERE server_id=? AND NOT is_imported"
		args := []any{p.ServerID}
		if p.MediaType != "" {
			where += " AND media_type=?"
			args = append(args, p.MediaType)
		}
		rows, e := s.Store.DB.QueryContext(ctx, s.Store.Rebind("SELECT id FROM "+s.mediaQueryable("media_items")+where+" ORDER BY id LIMIT 10001"), args...)
		if e != nil {
			return nil, e
		}
		for rows.Next() {
			var id int64
			if e = rows.Scan(&id); e != nil {
				rows.Close()
				return nil, e
			}
			ids = append(ids, id)
		}
		e = rows.Err()
		rows.Close()
		if e != nil {
			return nil, e
		}
		if len(ids) > 10000 {
			return nil, errors.New("more than 10000 unimported items; select smaller batches")
		}
	}
	if len(ids) > 1 {
		memo := newMediaListingSnapshot()
		defer memo.clear()
		ctx = context.WithValue(ctx, mediaListingBatchKey{}, memo)
	}
	success := 0
	for i, id := range ids {
		if e := job.Checkpoint(ctx); e != nil {
			return nil, e
		}
		row, e := s.Store.Get(ctx, "media_items", id)
		if e != nil {
			return nil, e
		}
		if row == nil {
			return nil, sql.ErrNoRows
		}
		progress(i*100/max(1, len(ids)), "搜索弹幕: "+str(row["title"]))
		if e = s.importMatchedMedia(ctx, row, func(int, string) {}); e != nil {
			return map[string]any{"imported": success, "failedItemId": id}, e
		}

		success++
	}
	return map[string]any{"imported": success}, nil
}
func (s *Server) importMatchedMedia(ctx context.Context, row store.Row, progress func(int, string)) error {
	origin, e := s.captureMediaOrigin(ctx, row)
	if e != nil {
		return e
	}
	ctx = mediaListingForItem(ctx, row, origin)
	candidates, plan, e := s.prepareMediaCandidates(ctx, row)
	if e != nil {
		return e
	}
	fallback := strings.EqualFold(s.authConfig(ctx, "webhookFallbackEnabled", "false"), "true")
	failures := []error{}
	groupIntent := &mediaGroupIntent{}
	for i, x := range candidates {
		if e := job.Checkpoint(ctx); e != nil {
			return e
		}
		if i > 0 {
			progress(0, "尝试下一弹幕源: "+x.Provider)
		}
		err := s.importMediaCandidatePlan(ctx, row, x, plan, origin, progress, groupIntent)
		if err == nil {
			return nil
		}
		failures = append(failures, fmt.Errorf("%s: %w", x.Provider, err))
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if errors.Is(err, errMediaAcquisitionIdentity) {
			return errors.Join(failures...)
		}
		if !fallback {
			break
		}
	}
	return errors.Join(failures...)
}
func (s *Server) importMediaCandidate(ctx context.Context, row store.Row, x provider.SearchResult, progress func(int, string)) error {
	return s.importMediaCandidateWithIntent(ctx, row, x, progress, nil)
}
func (s *Server) importMediaCandidateWithIntent(ctx context.Context, row store.Row, x provider.SearchResult, progress func(int, string), groupIntent *mediaGroupIntent) error {
	origin, e := s.captureMediaOrigin(ctx, row)
	if e != nil {
		return e
	}
	return s.importMediaCandidatePlan(ctx, row, x, nil, origin, progress, groupIntent)
}
func (s *Server) importMediaCandidatePlan(ctx context.Context, row store.Row, x provider.SearchResult, searchPlan *recognitionSearch, origin store.Row, progress func(int, string), groupIntent *mediaGroupIntent) error {
	if err := s.libTransaction(ctx, func(tx *sql.Tx) error {
		if e := s.validateMediaOrigin(ctx, tx, origin); e != nil {
			return e
		}
		return s.validateMediaSnapshot(ctx, tx, row)
	}); err != nil {
		return err
	}
	var rules *recognition.Rules
	var e error
	if searchPlan != nil {
		rules = searchPlan.rules
	} else {
		rules, _, e = s.loadRecognition(ctx)
	}
	if e != nil {
		return e
	}
	plan := &mediaImportProjection{origin: origin, rules: rules, title: str(row["title"]), mediaType: str(row["media_type"]), season: 1, sourceSeason: x.Season, groupIntent: groupIntent}
	req := ImportRequest{Provider: x.Provider, MediaID: x.ID, Title: x.Title, Type: x.Type, Season: x.Season, ImageURL: x.ImageURL, Metadata: map[string]string{"tmdbId": str(row["tmdb_id"]), "tvdbId": str(row["tvdb_id"]), "imdbId": str(row["imdb_id"])}}
	projected, _, e := prepareStorageWithRules(req, rules, nil)
	if e != nil {
		return e
	}
	plan.season = projected.Season
	if plan.mediaType == "" {
		plan.mediaType = projected.Type
	}
	if plan.mediaType == "tv" {
		plan.mediaType = "tv_series"
	}
	if plan.mediaType == "" {
		plan.mediaType = "tv_series"
	}
	if x.Year > 0 {
		value := x.Year
		req.Year = &value
	}
	if row["season"] != nil {
		plan.season = int(number(row["season"]))
		plan.explicitSeason = true
	}
	if row["episode"] != nil {
		n := int(number(row["episode"]))
		plan.episode = &n
	}
	if row["year"] != nil {
		n := int(number(row["year"]))
		req.Year = &n
		plan.year = &n
	}
	if searchPlan != nil {
		req.RecognitionWarnings = append([]string{}, searchPlan.Warnings...)
		if searchPlan.Metadata != nil {
			req.Aliases = metadataAliasFields(*searchPlan.Metadata)
			for k, v := range mediaMetadataIDs(*searchPlan.Metadata) {
				if req.Metadata[k] == "" && v != "" {
					req.Metadata[k] = v
				}
			}
		}
	}
	for k, v := range req.Metadata {
		if v == "" {
			delete(req.Metadata, k)
		}
	}
	b, e := json.Marshal(req)
	if e != nil {
		return e
	}
	ctx = mediaListingForCandidate(ctx, plan.sourceSeason)
	result, e := s.runImportRequest(ctx, b, progress, plan)
	if e != nil {
		return e
	}
	ids, ok := result.(map[string]any)
	if !ok {
		return errors.New("import result missing identities")
	}
	animeID, sourceID := number(ids["animeId"]), number(ids["sourceId"])
	return s.finishMediaAcquisition(ctx, row, plan, animeID, sourceID)
}
