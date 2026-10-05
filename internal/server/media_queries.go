// SPDX-License-Identifier: AGPL-3.0-only
package server

import (
	"database/sql"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/AniBakaBaka/AniDan/internal/store"
)

func (s *Server) mediaWhere(r *http.Request, table string) (string, []any, error) {
	f, e := mediaFilters(r, table == "local_danmaku_items")
	if e != nil {
		return "", nil, e
	}
	keys := []string{}
	for k := range f {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := []string{}
	args := []any{}
	for _, k := range keys {
		parts = append(parts, s.Store.Quote(k)+"=?")
		args = append(args, f[k])
	}
	if title := r.PathValue("title"); title != "" {
		parts = append(parts, "title=?")
		args = append(args, title)
	}
	q := r.URL.Query()
	for _, k := range []string{"year_from", "year_to"} {
		if q.Get(k) != "" {
			n, e := strconv.Atoi(q.Get(k))
			if e != nil {
				return "", nil, e
			}
			op := ">="
			if k == "year_to" {
				op = "<="
			}
			parts = append(parts, "year"+op+"?")
			args = append(args, n)
		}
	}
	if search := q.Get("search"); search != "" {
		parts = append(parts, "LOWER(title) LIKE ? ESCAPE '!'")
		search = strings.NewReplacer("!", "!!", "%", "!%", "_", "!_").Replace(strings.ToLower(search))
		args = append(args, "%"+search+"%")
	}
	if len(parts) == 0 {
		return "", args, nil
	}
	return " WHERE " + strings.Join(parts, " AND "), args, nil
}
func (s *Server) mediaWorks(w http.ResponseWriter, r *http.Request) {
	s.mediaGrouped(w, r, "media_items")
}
func (s *Server) mediaGrouped(w http.ResponseWriter, r *http.Request, table string) {
	where, args, e := s.mediaWhere(r, table)
	if e != nil {
		httpError(w, 422, e.Error())
		return
	}
	local := table == "local_danmaku_items"
	serverSelect := "MIN(server_id)"
	groups := "title,media_type,server_id,CASE WHEN media_type='movie' THEN id ELSE 0 END"
	if local {
		serverSelect = "0"
		groups = "title,media_type,CASE WHEN media_type='movie' THEN COALESCE(year,0) ELSE 0 END"
	}
	from := " FROM " + s.mediaQueryable(table) + where
	var total int64
	if e = s.Store.DB.QueryRowContext(r.Context(), s.Store.Rebind("SELECT COUNT(*) FROM (SELECT 1"+from+" GROUP BY "+groups+") grouped"), args...).Scan(&total); e != nil {
		httpError(w, 500, e.Error())
		return
	}
	page, size := mediaPage(r)
	selectSQL := "SELECT MIN(id),title,media_type," + serverSelect + ",MIN(year),MIN(tmdb_id),MIN(tvdb_id),MIN(imdb_id),MIN(poster_url),MAX(created_at),COUNT(*),SUM(CASE WHEN is_imported THEN 1 ELSE 0 END),COUNT(DISTINCT season)" + from + " GROUP BY " + groups + " ORDER BY MAX(created_at) DESC, MIN(id) DESC LIMIT ? OFFSET ?"
	a := append(append([]any{}, args...), size, (page-1)*size)
	rows, e := s.Store.DB.QueryContext(r.Context(), s.Store.Rebind(selectSQL), a...)
	if e != nil {
		httpError(w, 500, e.Error())
		return
	}
	out := []map[string]any{}
	for rows.Next() {
		var id, server, count, imported, seasons int64
		var title, typ string
		var year sql.NullInt64
		var tmdb, tvdb, imdb, poster sql.NullString
		var created any
		if e = rows.Scan(&id, &title, &typ, &server, &year, &tmdb, &tvdb, &imdb, &poster, &created, &count, &imported, &seasons); e != nil {
			rows.Close()
			httpError(w, 500, e.Error())
			return
		}
		kind := typ
		if typ == "tv_series" {
			kind = "tv_show"
		}
		item := map[string]any{"id": id, "title": title, "type": kind, "mediaType": kind, "serverId": server, "year": nullableInt(year), "tmdbId": nullableString(tmdb), "tvdbId": nullableString(tvdb), "imdbId": nullableString(imdb), "posterUrl": nullableString(poster), "createdAt": str(created), "episodeCount": count, "importedCount": imported, "seasonCount": seasons, "isImported": imported == count}
		if local && poster.Valid && poster.String != "" && !strings.HasPrefix(poster.String, "http") {
			item["posterUrl"] = fmt.Sprintf("/api/ui/local-items/%d/poster", id)
		}
		out = append(out, item)
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		httpError(w, 500, e.Error())
		return
	}
	// One batch query for page-level seasons avoids a query for every show.
	pairs := []string{}
	pairArgs := []any{}
	for _, x := range out {
		if x["type"] != "tv_show" {
			continue
		}
		if local {
			pairs = append(pairs, "title=?")
			pairArgs = append(pairArgs, x["title"])
		} else {
			pairs = append(pairs, "(title=? AND server_id=?)")
			pairArgs = append(pairArgs, x["title"], x["serverId"])
		}
	}
	byShow := map[string][]any{}
	if len(pairs) > 0 {
		serverCol := "server_id"
		groupServer := ",server_id"
		if local {
			serverCol = "0"
			groupServer = ""
		}
		q := "SELECT title," + serverCol + ",season,COUNT(*),SUM(CASE WHEN is_imported THEN 1 ELSE 0 END),MIN(year),MIN(poster_url) FROM " + s.mediaQueryable(table) + " WHERE media_type='tv_series' AND (" + strings.Join(pairs, " OR ") + ") GROUP BY title,season" + groupServer + " ORDER BY season"
		rs, err := s.Store.DB.QueryContext(r.Context(), s.Store.Rebind(q), pairArgs...)
		if err != nil {
			httpError(w, 500, err.Error())
			return
		}
		for rs.Next() {
			var title string
			var server, count, imported int64
			var season, year sql.NullInt64
			var poster sql.NullString
			if err = rs.Scan(&title, &server, &season, &count, &imported, &year, &poster); err != nil {
				rs.Close()
				httpError(w, 500, err.Error())
				return
			}
			key := fmt.Sprintf("%d:%s", server, title)
			byShow[key] = append(byShow[key], map[string]any{"season": nullableInt(season), "episodeCount": count, "importedCount": imported, "year": nullableInt(year), "posterUrl": nullableString(poster)})
			if len(byShow[key]) > 10000 {
				rs.Close()
				httpError(w, 422, "Too many grouped seasons")
				return
			}
		}
		err = rs.Err()
		rs.Close()
		if err != nil {
			httpError(w, 500, err.Error())
			return
		}
	}
	for _, x := range out {
		if x["type"] == "tv_show" {
			x["seasons"] = byShow[fmt.Sprintf("%v:%v", x["serverId"], x["title"])]
			continue
		}
		if local {
			f := store.Row{"title": x["title"], "media_type": "movie", "year": x["year"]}
			ids := []int64{}
			for offset := 0; ; offset += 500 {
				records, err := s.Store.List(r.Context(), table, f, 500, offset)
				if err != nil {
					httpError(w, 500, err.Error())
					return
				}
				for _, row := range records {
					ids = append(ids, number(row["id"]))
				}
				if len(ids) > 10000 {
					httpError(w, 422, "A movie group exceeds 10000 files")
					return
				}
				if len(records) < 500 {
					break
				}
			}
			x["ids"] = ids
			x["fileCount"] = len(ids)
		} else {
			record, err := s.Store.Get(r.Context(), table, x["id"])
			if err != nil {
				httpError(w, 500, err.Error())
				return
			}
			x["mediaId"] = record["media_id"]
			x["libraryId"] = record["library_id"]
			x["updatedAt"] = record["updated_at"]
		}
	}
	writeJSON(w, 200, map[string]any{"total": total, "list": out})
}
func nullableInt(v sql.NullInt64) any {
	if v.Valid {
		return v.Int64
	}
	return nil
}
func nullableString(v sql.NullString) any {
	if v.Valid {
		return v.String
	}
	return nil
}
func (s *Server) mediaSeasons(w http.ResponseWriter, r *http.Request) {
	s.mediaSeasonList(w, r, "media_items")
}
func (s *Server) mediaSeasonList(w http.ResponseWriter, r *http.Request, table string) {
	where, args, e := s.mediaWhere(r, table)
	if e != nil {
		httpError(w, 422, e.Error())
		return
	}
	if where == "" {
		where = " WHERE "
	} else {
		where += " AND "
	}
	where += "media_type='tv_series'"
	q := "SELECT season,COUNT(*),SUM(CASE WHEN is_imported THEN 1 ELSE 0 END),MIN(year),MIN(poster_url) FROM " + s.mediaQueryable(table) + where + " GROUP BY season ORDER BY season"
	rows, e := s.Store.DB.QueryContext(r.Context(), s.Store.Rebind(q), args...)
	if e != nil {
		httpError(w, 500, e.Error())
		return
	}
	defer rows.Close()
	out := []any{}
	for rows.Next() {
		var season, year sql.NullInt64
		var n, imported int64
		var poster sql.NullString
		if e = rows.Scan(&season, &n, &imported, &year, &poster); e != nil {
			httpError(w, 500, e.Error())
			return
		}
		out = append(out, map[string]any{"season": nullableInt(season), "episodeCount": n, "importedCount": imported, "year": nullableInt(year), "posterUrl": nullableString(poster)})
	}
	if e = rows.Err(); e != nil {
		httpError(w, 500, e.Error())
		return
	}
	writeJSON(w, 200, out)
}
