// SPDX-License-Identifier: AGPL-3.0-only
// Legacy route/data contracts adapted from misaka_danmu_server.
package server

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/AniBakaBaka/AniDan/internal/store"
)

type libQueryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}
type libError struct {
	status  int
	message string
}

func (e *libError) Error() string             { return e.message }
func libErr(status int, message string) error { return &libError{status, message} }
func libWriteError(w http.ResponseWriter, e error) {
	var l *libError
	if errors.As(e, &l) {
		httpError(w, l.status, l.message)
	} else if errors.Is(e, sql.ErrNoRows) {
		httpError(w, 404, "Record not found")
	} else {
		httpError(w, 500, "Library operation failed")
	}
}
func (s *Server) libRows(ctx context.Context, q libQueryer, table, query string, args ...any) ([]store.Row, error) {
	rows, e := q.QueryContext(ctx, s.Store.Rebind(query), args...)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []store.Row{}
	for rows.Next() {
		v, e := store.ScanRow(rows, store.Schema[table])
		if e != nil {
			return nil, e
		}
		out = append(out, v)
	}
	return out, rows.Err()
}
func (s *Server) libOne(ctx context.Context, q libQueryer, table, where string, args ...any) (store.Row, error) {
	rows, e := s.libRows(ctx, q, table, "SELECT * FROM "+s.Store.Quote(table)+" WHERE "+where+" LIMIT 1", args...)
	if e != nil {
		return nil, e
	}
	if len(rows) == 0 {
		return nil, sql.ErrNoRows
	}
	return rows[0], nil
}
func (s *Server) libUpdate(ctx context.Context, tx *sql.Tx, table string, id any, row store.Row) error {
	v, e := store.ValidateRow(table, row, false)
	if e != nil {
		return e
	}
	keys := []string{}
	for k := range v {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if len(keys) == 0 {
		return nil
	}
	parts := []string{}
	args := []any{}
	for _, k := range keys {
		parts = append(parts, s.Store.Quote(k)+" = ?")
		args = append(args, v[k])
	}
	args = append(args, id)
	_, e = tx.ExecContext(ctx, s.Store.Rebind("UPDATE "+s.Store.Quote(table)+" SET "+strings.Join(parts, ",")+" WHERE id = ?"), args...)
	return e
}
func (s *Server) libTransaction(ctx context.Context, fn func(*sql.Tx) error) error {
	a := s.authState()
	a.mutation.Lock()
	defer a.mutation.Unlock()
	tx, e := s.Store.DB.BeginTx(ctx, nil)
	if e != nil {
		return e
	}
	defer tx.Rollback()
	if e = fn(tx); e != nil {
		return e
	}
	return tx.Commit()
}
func (s *Server) libAuthorized(w http.ResponseWriter, r *http.Request) bool {
	if strings.HasPrefix(r.URL.Path, "/api/control/") {
		if e := s.requireControl(r); e != nil {
			httpError(w, 401, e.Error())
			return false
		}
		return true
	}
	_, ok := s.authRequired(w, r)
	return ok
}
func libIsControl(r *http.Request) bool { return strings.HasPrefix(r.URL.Path, "/api/control/") }
func libAction(w http.ResponseWriter, r *http.Request, message string, ids store.Row) {
	if !libIsControl(r) {
		w.WriteHeader(204)
		return
	}
	out := store.Row{"status": "success", "message": message, "animeId": nil, "sourceId": nil}
	for k, v := range ids {
		out[k] = v
	}
	writeJSON(w, 200, out)
}
func libPage(r *http.Request, defaultSize int) (int, int, error) {
	page, size := 1, defaultSize
	var e error
	if v := r.URL.Query().Get("page"); v != "" {
		page, e = strconv.Atoi(v)
		if e != nil {
			return 0, 0, e
		}
	}
	if v := r.URL.Query().Get("pageSize"); v != "" {
		size, e = strconv.Atoi(v)
		if e != nil {
			return 0, 0, e
		}
	}
	if page < 1 || size < 1 || size > 1000 || page > 1000000 {
		return 0, 0, errors.New("invalid pagination")
	}
	return page, size, nil
}

var libAnimeFields = map[string]string{"title": "title", "type": "type", "season": "season", "year": "year", "episodeCount": "episode_count", "imageUrl": "image_url"}
var libMetaFields = map[string]string{"tmdbId": "tmdb_id", "tmdbEpisodeGroupId": "tmdb_episode_group_id", "bangumiId": "bangumi_id", "tvdbId": "tvdb_id", "doubanId": "douban_id", "imdbId": "imdb_id"}
var libAliasFields = map[string]string{"nameEn": "name_en", "nameJp": "name_jp", "nameRomaji": "name_romaji", "aliasCn1": "alias_cn_1", "aliasCn2": "alias_cn_2", "aliasCn3": "alias_cn_3", "aliasLocked": "alias_locked"}

func libSelectFields(input store.Row, mapping map[string]string, fill bool) store.Row {
	out := store.Row{}
	for api, col := range mapping {
		v, ok := input[api]
		if ok || fill {
			out[col] = v
		}
	}
	return out
}
func libAnimeValidation(b store.Row, control, update bool) error {
	title, ok := b["title"].(string)
	if !ok || strings.TrimSpace(title) == "" || utf8.RuneCountInString(title) > 500 {
		return libErr(422, "title is required (1..500 characters)")
	}
	typ := authString(b["type"])
	if typ == "" && !control && !update {
		b["type"] = "tv_series"
		typ = "tv_series"
	}
	if typ != "tv_series" && typ != "movie" && typ != "ova" && typ != "other" {
		return libErr(422, "invalid media type")
	}
	if control && (typ == "ova" || typ == "other") {
		return libErr(422, "control API type must be tv_series or movie")
	}
	if _, ok := b["season"]; !ok {
		if update || control && typ == "tv_series" {
			return libErr(422, "season is required")
		}
		b["season"] = int64(1)
	}
	if authInt(b["season"]) < 0 {
		return libErr(422, "season must be non-negative")
	}
	if control && typ == "movie" {
		b["season"] = int64(1)
	}
	if x := b["episodeCount"]; x != nil && authInt(x) < 1 {
		return libErr(422, "episodeCount must be positive")
	}
	for table, fields := range map[string]map[string]string{"anime": libAnimeFields, "anime_metadata": libMetaFields, "anime_aliases": libAliasFields} {
		data := libSelectFields(b, fields, false)
		if table == "anime_aliases" && data["alias_locked"] == nil {
			delete(data, "alias_locked")
		}
		if _, e := store.ValidateRow(table, data, false); e != nil {
			return libErr(422, e.Error())
		}
	}
	return nil
}
func (s *Server) registerLibrary(m *http.ServeMux) {
	for _, prefix := range []string{"/api/ui", "/api/control"} {
		m.HandleFunc("GET "+prefix+"/library", s.libList)
		m.HandleFunc("POST "+prefix+"/library/anime", s.libCreateAnime)
		m.HandleFunc("PUT "+prefix+"/library/anime/{animeId}", s.libEditAnime)
		m.HandleFunc("GET "+prefix+"/library/anime/{animeId}/sources", s.libSources)
		m.HandleFunc("POST "+prefix+"/library/anime/{animeId}/sources", s.libAddSource)
		m.HandleFunc("GET "+prefix+"/library/source/{sourceId}/episodes", s.libEpisodes)
		m.HandleFunc("PUT "+prefix+"/library/source/{sourceId}/favorite", s.libToggleSource)
		m.HandleFunc("PUT "+prefix+"/library/episode/{episodeId}", s.libEditEpisode)
	}
	m.HandleFunc("GET /api/ui/library/anime/{animeId}/details", s.libDetails)
	m.HandleFunc("GET /api/control/library/anime/{animeId}", s.libDetails)
	m.HandleFunc("GET /api/control/library/search", s.libList)
	m.HandleFunc("GET /api/ui/library/source/{sourceId}/details", s.libSourceDetails)
	m.HandleFunc("GET /api/ui/library/episodes-by-title", s.libEpisodeIndices)
	for _, action := range []string{"toggle-incremental-refresh", "toggle-finished"} {
		m.HandleFunc("PUT /api/ui/library/source/{sourceId}/"+action, s.libToggleSource)
	}
	m.HandleFunc("POST /api/ui/library/anime/bulk-set-finished", s.libAnimeFinished)
	s.registerLibraryGroups(m)
	s.registerLibraryRelations(m)
	s.registerLibraryTasks(m)
	s.registerLibraryIncremental(m)
	m.HandleFunc("POST /api/ui/library/anime/{animeId}/refresh-poster", s.libRefreshPoster)
}
func (s *Server) libCreateAnimeTx(ctx context.Context, tx *sql.Tx, b store.Row, custom bool) (int64, error) {
	query := "title = ? AND season = ?"
	args := []any{b["title"], b["season"]}
	if b["year"] != nil {
		query += " AND (year = ? OR year IS NULL)"
		args = append(args, b["year"])
	}
	if _, e := s.libOne(ctx, tx, "anime", query, args...); e == nil {
		return 0, libErr(409, "已存在同名同季度的作品")
	} else if !errors.Is(e, sql.ErrNoRows) {
		return 0, e
	}
	row := libSelectFields(b, libAnimeFields, false)
	row["created_at"] = s.authNow()
	id, e := s.Store.InsertTx(ctx, tx, "anime", row)
	if e != nil {
		return 0, e
	}
	meta := libSelectFields(b, libMetaFields, false)
	meta["anime_id"] = id
	if _, e = s.Store.InsertTx(ctx, tx, "anime_metadata", meta); e != nil {
		return 0, e
	}
	alias := libSelectFields(b, libAliasFields, false)
	alias["anime_id"] = id
	if alias["alias_locked"] == nil {
		alias["alias_locked"] = false
	}
	if _, e = s.Store.InsertTx(ctx, tx, "anime_aliases", alias); e != nil {
		return 0, e
	}
	if custom {
		if _, e = s.libAddSourceTx(ctx, tx, id, "custom", fmt.Sprintf("custom_%d", id)); e != nil {
			return 0, e
		}
	}
	return id, nil
}
func (s *Server) libCreateAnime(w http.ResponseWriter, r *http.Request) {
	if !s.libAuthorized(w, r) {
		return
	}
	var b store.Row
	if readJSON(r, &b) != nil {
		httpError(w, 422, "Invalid JSON")
		return
	}
	if e := libAnimeValidation(b, libIsControl(r), false); e != nil {
		libWriteError(w, e)
		return
	}
	var id int64
	e := s.libTransaction(r.Context(), func(tx *sql.Tx) error { var e error; id, e = s.libCreateAnimeTx(r.Context(), tx, b, true); return e })
	if e != nil {
		libWriteError(w, e)
		return
	}
	if libIsControl(r) {
		writeJSON(w, 201, store.Row{"status": "success", "message": "作品创建成功", "animeId": id, "sourceId": nil})
		return
	}
	row, e := s.Store.Get(r.Context(), "anime", id)
	if e != nil {
		libWriteError(w, e)
		return
	}
	out, e := s.libAnimeInfo(r.Context(), row)
	if e != nil {
		libWriteError(w, e)
		return
	}
	writeJSON(w, 201, out)
}
func (s *Server) libAnimeInfo(ctx context.Context, a store.Row) (store.Row, error) {
	sources, e := s.Store.List(ctx, "anime_sources", store.Row{"anime_id": a["id"]}, 10000, 0)
	if e != nil {
		return nil, e
	}
	brief := []store.Row{}
	var max int64
	for _, v := range sources {
		brief = append(brief, store.Row{"sourceId": v["id"], "providerName": v["provider_name"], "isFavorited": authBool(v["is_favorited"]), "incrementalRefreshEnabled": authBool(v["incremental_refresh_enabled"]), "isFinished": authBool(v["is_finished"])})
	}
	e = s.Store.DB.QueryRowContext(ctx, s.Store.Rebind("SELECT COALESCE(MAX(e.episode_index),0) FROM episode e JOIN anime_sources s ON s.id=e.source_id WHERE s.anime_id = ?"), a["id"]).Scan(&max)
	if e != nil {
		return nil, e
	}
	if authString(a["type"]) == "movie" {
		max = 1
	}
	var groupName any
	if a["group_id"] != nil {
		g, e := s.Store.Get(ctx, "anime_groups", a["group_id"])
		if e == nil {
			groupName = g["name"]
		}
	}
	return store.Row{"animeId": a["id"], "title": a["title"], "type": a["type"], "season": a["season"], "year": a["year"], "episodeCount": max, "sourceCount": len(sources), "createdAt": authDateWire(a["created_at"]), "imageUrl": a["image_url"], "localImagePath": a["local_image_path"], "groupId": a["group_id"], "groupName": groupName, "sources": brief}, nil
}
func (s *Server) libList(w http.ResponseWriter, r *http.Request) {
	if !s.libAuthorized(w, r) {
		return
	}
	page, size, e := libPage(r, 10)
	if e != nil {
		httpError(w, 422, "Invalid pagination")
		return
	}
	where := []string{"1=1"}
	args := []any{}
	q := r.URL.Query()
	keyword := strings.TrimSpace(q.Get("keyword"))
	if keyword != "" {
		value := "%" + strings.ReplaceAll(strings.ReplaceAll(strings.ToLower(keyword), "：", ":"), " ", "") + "%"
		norm := func(col string) string { return "LOWER(REPLACE(REPLACE(" + col + ",'：',':'),' ','')) LIKE ?" }
		parts := []string{norm("a.title")}
		args = append(args, value)
		for _, col := range []string{"name_en", "name_jp", "name_romaji", "alias_cn_1", "alias_cn_2", "alias_cn_3"} {
			parts = append(parts, "EXISTS (SELECT 1 FROM anime_aliases al WHERE al.anime_id=a.id AND "+norm("al."+col)+")")
			args = append(args, value)
		}
		where = append(where, "("+strings.Join(parts, " OR ")+")")
	}
	typ := q.Get("type")
	if typ == "tv" {
		where = append(where, "a.type IN ('tv_series','ova')")
	} else if typ != "" {
		where = append(where, "a.type = ?")
		args = append(args, typ)
	}
	clause := strings.Join(where, " AND ")
	var count int64
	if e = s.Store.DB.QueryRowContext(r.Context(), s.Store.Rebind("SELECT COUNT(*) FROM anime a WHERE "+clause), args...).Scan(&count); e != nil {
		libWriteError(w, e)
		return
	}
	order := "a.created_at"
	if q.Get("sortBy") == "episode_fetched" {
		order = "COALESCE((SELECT MAX(e.fetched_at) FROM episode e JOIN anime_sources ss ON ss.id=e.source_id WHERE ss.anime_id=a.id),a.created_at)"
	}
	direction := " DESC"
	if q.Get("sortOrder") == "asc" {
		direction = " ASC"
	}
	query := "SELECT a.* FROM anime a WHERE " + clause + " ORDER BY " + order + direction + ", a.id" + direction
	if !libIsControl(r) {
		query += " LIMIT ? OFFSET ?"
		args = append(args, size, (page-1)*size)
	}
	rows, e := s.libRows(r.Context(), s.Store.DB, "anime", query, args...)
	if e != nil {
		libWriteError(w, e)
		return
	}
	out := []store.Row{}
	for _, a := range rows {
		v, e := s.libAnimeInfo(r.Context(), a)
		if e != nil {
			libWriteError(w, e)
			return
		}
		out = append(out, v)
	}
	if libIsControl(r) {
		writeJSON(w, 200, out)
	} else {
		writeJSON(w, 200, store.Row{"total": count, "list": out})
	}
}
func (s *Server) libDetail(ctx context.Context, id int64) (store.Row, error) {
	a, e := s.Store.Get(ctx, "anime", id)
	if e != nil || a == nil {
		if e == nil {
			e = sql.ErrNoRows
		}
		return nil, e
	}
	out := store.Row{"animeId": a["id"], "localImagePath": a["local_image_path"]}
	for api, col := range libAnimeFields {
		out[api] = a[col]
	}
	for table, fields := range map[string]map[string]string{"anime_metadata": libMetaFields, "anime_aliases": libAliasFields} {
		v, e := s.authFind(ctx, table, store.Row{"anime_id": id})
		if e != nil && !errors.Is(e, sql.ErrNoRows) {
			return nil, e
		}
		for api, col := range fields {
			out[api] = v[col]
		}
	}
	out["aliasLocked"] = authBool(out["aliasLocked"])
	return out, nil
}
func (s *Server) libDetails(w http.ResponseWriter, r *http.Request) {
	if !s.libAuthorized(w, r) {
		return
	}
	id, e := idParam(r, "animeId")
	if e != nil {
		httpError(w, 422, "Invalid anime ID")
		return
	}
	v, e := s.libDetail(r.Context(), id)
	if e != nil {
		libWriteError(w, e)
		return
	}
	if libIsControl(r) {
		delete(v, "animeId")
		delete(v, "aliasLocked")
	}
	writeJSON(w, 200, v)
}
func (s *Server) libEditAnime(w http.ResponseWriter, r *http.Request) {
	if !s.libAuthorized(w, r) {
		return
	}
	id, e := idParam(r, "animeId")
	if e != nil {
		httpError(w, 422, "Invalid ID")
		return
	}
	var b store.Row
	if readJSON(r, &b) != nil {
		httpError(w, 422, "Invalid JSON")
		return
	}
	if e = libAnimeValidation(b, false, true); e != nil {
		libWriteError(w, e)
		return
	}
	e = s.libTransaction(r.Context(), func(tx *sql.Tx) error {
		if _, e := s.libOne(r.Context(), tx, "anime", "id = ?", id); e != nil {
			return e
		}
		if e := s.libUpdate(r.Context(), tx, "anime", id, libSelectFields(b, libAnimeFields, true)); e != nil {
			return e
		}
		for table, fields := range map[string]map[string]string{"anime_metadata": libMetaFields, "anime_aliases": libAliasFields} {
			v, e := s.libOne(r.Context(), tx, table, "anime_id = ?", id)
			data := libSelectFields(b, fields, true)
			if table == "anime_aliases" && b["aliasLocked"] == nil {
				delete(data, "alias_locked")
			}
			if errors.Is(e, sql.ErrNoRows) {
				data["anime_id"] = id
				if table == "anime_aliases" && data["alias_locked"] == nil {
					data["alias_locked"] = false
				}
				if _, e = s.Store.InsertTx(r.Context(), tx, table, data); e != nil {
					return e
				}
			} else if e != nil {
				return e
			} else if e = s.libUpdate(r.Context(), tx, table, v["id"], data); e != nil {
				return e
			}
		}
		return nil
	})
	if e != nil {
		libWriteError(w, e)
		return
	}
	libAction(w, r, "作品信息更新成功", nil)
}
func (s *Server) libAddSourceTx(ctx context.Context, tx *sql.Tx, anime int64, provider, media string) (int64, error) {
	if _, e := s.libOne(ctx, tx, "anime", "id = ?", anime); e != nil {
		return 0, e
	}
	if _, e := s.libOne(ctx, tx, "anime_sources", "anime_id = ? AND provider_name = ? AND media_id = ?", anime, provider, media); e == nil {
		return 0, libErr(409, "该数据源已存在")
	} else if !errors.Is(e, sql.ErrNoRows) {
		return 0, e
	}
	var order int64
	if e := tx.QueryRowContext(ctx, s.Store.Rebind("SELECT COALESCE(MAX(source_order),0)+1 FROM anime_sources WHERE anime_id = ?"), anime).Scan(&order); e != nil {
		return 0, e
	}
	if _, e := s.libOne(ctx, tx, "scrapers", "provider_name = ?", provider); errors.Is(e, sql.ErrNoRows) {
		_, e = s.Store.InsertTx(ctx, tx, "scrapers", store.Row{"provider_name": provider, "is_enabled": true, "display_order": 99, "use_proxy": false, "total_searches": 0, "success_count": 0, "fail_count": 0, "timeout_count": 0, "empty_count": 0, "total_duration_ms": 0, "total_result_count": 0})
		if e != nil {
			return 0, e
		}
	} else if e != nil {
		return 0, e
	}
	return s.Store.InsertTx(ctx, tx, "anime_sources", store.Row{"anime_id": anime, "source_order": order, "provider_name": provider, "media_id": media, "is_favorited": false, "incremental_refresh_enabled": false, "is_finished": false, "incremental_refresh_failures": 0, "created_at": s.authNow()})
}
func (s *Server) libSourceInfo(ctx context.Context, row store.Row) (store.Row, error) {
	n, e := s.Store.Count(ctx, "episode", store.Row{"source_id": row["id"]})
	return store.Row{"sourceId": row["id"], "providerName": row["provider_name"], "mediaId": row["media_id"], "isFavorited": authBool(row["is_favorited"]), "incrementalRefreshEnabled": authBool(row["incremental_refresh_enabled"]), "isFinished": authBool(row["is_finished"]), "episodeCount": n, "createdAt": authDateWire(row["created_at"])}, e
}
func (s *Server) libAddSource(w http.ResponseWriter, r *http.Request) {
	if !s.libAuthorized(w, r) {
		return
	}
	id, e := idParam(r, "animeId")
	if e != nil {
		httpError(w, 422, "Invalid ID")
		return
	}
	var b struct {
		Provider string `json:"providerName"`
		Media    string `json:"mediaId"`
	}
	if readJSON(r, &b) != nil || b.Provider == "" || b.Media == "" || len(b.Provider) > 500 || len(b.Media) > 255 {
		httpError(w, 422, "providerName and mediaId are required")
		return
	}
	var source int64
	e = s.libTransaction(r.Context(), func(tx *sql.Tx) error {
		var e error
		source, e = s.libAddSourceTx(r.Context(), tx, id, b.Provider, b.Media)
		return e
	})
	if e != nil {
		libWriteError(w, e)
		return
	}
	if libIsControl(r) {
		writeJSON(w, 201, store.Row{"status": "success", "message": "数据源添加成功", "animeId": nil, "sourceId": source})
		return
	}
	row, e := s.Store.Get(r.Context(), "anime_sources", source)
	if e != nil {
		libWriteError(w, e)
		return
	}
	out, e := s.libSourceInfo(r.Context(), row)
	if e != nil {
		libWriteError(w, e)
		return
	}
	writeJSON(w, 201, out)
}
func (s *Server) libSources(w http.ResponseWriter, r *http.Request) {
	if !s.libAuthorized(w, r) {
		return
	}
	id, e := idParam(r, "animeId")
	if e != nil {
		httpError(w, 422, "Invalid ID")
		return
	}
	if a, e := s.Store.Get(r.Context(), "anime", id); e != nil || a == nil {
		httpError(w, 404, "作品未找到")
		return
	}
	rows, e := s.libRows(r.Context(), s.Store.DB, "anime_sources", "SELECT * FROM anime_sources WHERE anime_id = ? ORDER BY source_order,id", id)
	if e != nil {
		libWriteError(w, e)
		return
	}
	out := []store.Row{}
	for _, row := range rows {
		v, e := s.libSourceInfo(r.Context(), row)
		if e != nil {
			libWriteError(w, e)
			return
		}
		out = append(out, v)
	}
	writeJSON(w, 200, out)
}
func (s *Server) libSourceDetails(w http.ResponseWriter, r *http.Request) {
	if !s.libAuthorized(w, r) {
		return
	}
	id, e := idParam(r, "sourceId")
	if e != nil {
		httpError(w, 422, "Invalid ID")
		return
	}
	row, e := s.Store.Get(r.Context(), "anime_sources", id)
	if e != nil || row == nil {
		httpError(w, 404, "Source not found")
		return
	}
	a, e := s.Store.Get(r.Context(), "anime", row["anime_id"])
	if e != nil {
		libWriteError(w, e)
		return
	}
	meta, e := s.authFind(r.Context(), "anime_metadata", store.Row{"anime_id": a["id"]})
	if e != nil && !errors.Is(e, sql.ErrNoRows) {
		libWriteError(w, e)
		return
	}
	writeJSON(w, 200, store.Row{"sourceId": row["id"], "animeId": a["id"], "providerName": row["provider_name"], "mediaId": row["media_id"], "title": a["title"], "type": a["type"], "season": a["season"], "tmdbId": meta["tmdb_id"], "bangumiId": meta["bangumi_id"]})
}
func libEpisodePublic(v store.Row) store.Row {
	return store.Row{"episodeId": v["id"], "title": v["title"], "episodeIndex": v["episode_index"], "sourceUrl": v["source_url"], "fetchedAt": authDateWire(v["fetched_at"]), "commentCount": authInt(v["comment_count"]), "danmakuFilePath": v["danmaku_file_path"]}
}
func (s *Server) libEpisodes(w http.ResponseWriter, r *http.Request) {
	if !s.libAuthorized(w, r) {
		return
	}
	id, e := idParam(r, "sourceId")
	if e != nil {
		httpError(w, 422, "Invalid ID")
		return
	}
	page, size, e := libPage(r, 25)
	if e != nil {
		httpError(w, 422, "Invalid pagination")
		return
	}
	if row, e := s.Store.Get(r.Context(), "anime_sources", id); e != nil || row == nil {
		httpError(w, 404, "Source not found")
		return
	}
	total, e := s.Store.Count(r.Context(), "episode", store.Row{"source_id": id})
	if e != nil {
		libWriteError(w, e)
		return
	}
	query := "SELECT * FROM episode WHERE source_id = ? ORDER BY episode_index,id"
	args := []any{id}
	if !libIsControl(r) {
		query += " LIMIT ? OFFSET ?"
		args = append(args, size, (page-1)*size)
	}
	rows, e := s.libRows(r.Context(), s.Store.DB, "episode", query, args...)
	if e != nil {
		libWriteError(w, e)
		return
	}
	out := []store.Row{}
	for _, v := range rows {
		out = append(out, libEpisodePublic(v))
	}
	if libIsControl(r) {
		writeJSON(w, 200, out)
	} else {
		writeJSON(w, 200, store.Row{"total": total, "list": out})
	}
}
func (s *Server) libToggleSource(w http.ResponseWriter, r *http.Request) {
	if !s.libAuthorized(w, r) {
		return
	}
	id, e := idParam(r, "sourceId")
	if e != nil {
		httpError(w, 422, "Invalid ID")
		return
	}
	field := "is_favorited"
	if strings.HasSuffix(r.URL.Path, "toggle-incremental-refresh") {
		field = "incremental_refresh_enabled"
	}
	if strings.HasSuffix(r.URL.Path, "toggle-finished") {
		field = "is_finished"
	}
	var enabled bool
	e = s.libTransaction(r.Context(), func(tx *sql.Tx) error {
		row, e := s.libOne(r.Context(), tx, "anime_sources", "id = ?", id)
		if e != nil {
			return e
		}
		enabled = !authBool(row[field])
		if (field == "is_favorited" || field == "incremental_refresh_enabled") && enabled {
			if _, e = tx.ExecContext(r.Context(), s.Store.Rebind("UPDATE anime_sources SET "+s.Store.Quote(field)+" = ? WHERE anime_id = ?"), false, row["anime_id"]); e != nil {
				return e
			}
		}
		changes := store.Row{field: enabled}
		if field == "incremental_refresh_enabled" && enabled {
			changes["is_finished"] = false
		}
		return s.libUpdate(r.Context(), tx, "anime_sources", id, changes)
	})
	if e != nil {
		libWriteError(w, e)
		return
	}
	libAction(w, r, "数据源状态已更新", nil)
}
func (s *Server) libAnimeFinished(w http.ResponseWriter, r *http.Request) {
	if !s.libAuthorized(w, r) {
		return
	}
	var b struct {
		IDs      []int64 `json:"animeIds"`
		Finished bool    `json:"isFinished"`
	}
	if readJSON(r, &b) != nil || len(b.IDs) == 0 || len(b.IDs) > 10000 {
		httpError(w, 422, "animeIds is required")
		return
	}
	e := s.libTransaction(r.Context(), func(tx *sql.Tx) error {
		for _, id := range b.IDs {
			if _, e := s.libOne(r.Context(), tx, "anime", "id = ?", id); e != nil {
				return e
			}
			if _, e := tx.ExecContext(r.Context(), s.Store.Rebind("UPDATE anime_sources SET is_finished = ? WHERE anime_id = ?"), b.Finished, id); e != nil {
				return e
			}
		}
		return nil
	})
	if e != nil {
		libWriteError(w, e)
		return
	}
	w.WriteHeader(204)
}
func (s *Server) libEpisodeIndices(w http.ResponseWriter, r *http.Request) {
	if !s.libAuthorized(w, r) {
		return
	}
	title := r.URL.Query().Get("title")
	if title == "" {
		httpError(w, 422, "title is required")
		return
	}
	query := "SELECT DISTINCT e.episode_index FROM episode e JOIN anime_sources s ON s.id=e.source_id JOIN anime a ON a.id=s.anime_id WHERE a.title = ?"
	args := []any{title}
	if v := r.URL.Query().Get("season"); v != "" {
		n, e := strconv.Atoi(v)
		if e != nil {
			httpError(w, 422, "Invalid season")
			return
		}
		query += " AND a.season = ?"
		args = append(args, n)
	}
	query += " ORDER BY e.episode_index"
	rows, e := s.Store.DB.QueryContext(r.Context(), s.Store.Rebind(query), args...)
	if e != nil {
		libWriteError(w, e)
		return
	}
	defer rows.Close()
	out := []int64{}
	for rows.Next() {
		var i int64
		if e = rows.Scan(&i); e != nil {
			libWriteError(w, e)
			return
		}
		out = append(out, i)
	}
	if e = rows.Err(); e != nil {
		libWriteError(w, e)
		return
	}
	writeJSON(w, 200, out)
}
