// SPDX-License-Identifier: AGPL-3.0-only
package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/AniBakaBaka/AniDan/internal/job"
	"github.com/AniBakaBaka/AniDan/internal/store"
)

func (s *Server) registerLibraryIncremental(m *http.ServeMux) {
	p := "/api/ui/library/incremental-refresh"
	m.HandleFunc("GET "+p+"/sources", s.libIncrementalSources)
	m.HandleFunc("GET "+p+"/task-status", s.libIncrementalStatus)
	for _, v := range []string{"batch-toggle", "batch-favorite", "batch-unfavorite", "batch-set-finished", "batch-unset-finished"} {
		m.HandleFunc("POST "+p+"/"+v, s.libBatchFlag)
	}
	if s.Jobs != nil {
		if e := s.Jobs.Register("incrementalRefresh", s.libIncrementalJob); e != nil {
			panic(e)
		}
	}
}
func (s *Server) libBatchFlag(w http.ResponseWriter, r *http.Request) {
	if !s.libAuthorized(w, r) {
		return
	}
	var b struct {
		IDs     []int64 `json:"sourceIds"`
		Enabled *bool   `json:"enabled"`
	}
	if readJSON(r, &b) != nil || len(b.IDs) > 10000 {
		httpError(w, 422, "Invalid sourceIds")
		return
	}
	action := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
	field, enabled := "is_favorited", true
	switch action {
	case "batch-toggle":
		field = "incremental_refresh_enabled"
		if b.Enabled == nil {
			httpError(w, 422, "enabled is required")
			return
		}
		enabled = *b.Enabled
	case "batch-unfavorite":
		enabled = false
	case "batch-set-finished":
		field = "is_finished"
	case "batch-unset-finished":
		field = "is_finished"
		enabled = false
	}
	count := 0
	e := s.libTransaction(r.Context(), func(tx *sql.Tx) error {
		seen := map[int64]bool{}
		for _, id := range b.IDs {
			if seen[id] {
				continue
			}
			seen[id] = true
			src, e := s.libOne(r.Context(), tx, "anime_sources", "id = ?", id)
			if errors.Is(e, sql.ErrNoRows) {
				continue
			}
			if e != nil {
				return e
			}
			if enabled && (field == "is_favorited" || field == "incremental_refresh_enabled") {
				if _, e = tx.ExecContext(r.Context(), s.Store.Rebind("UPDATE anime_sources SET "+s.Store.Quote(field)+" = ? WHERE anime_id = ?"), false, src["anime_id"]); e != nil {
					return e
				}
			}
			if e = s.libUpdate(r.Context(), tx, "anime_sources", id, store.Row{field: enabled}); e != nil {
				return e
			}
			count++
		}
		return nil
	})
	if e != nil {
		libWriteError(w, e)
		return
	}
	writeJSON(w, 200, store.Row{"message": fmt.Sprintf("已更新 %d 个数据源", count), "count": count})
}
func (s *Server) libIncrementalSources(w http.ResponseWriter, r *http.Request) {
	if !s.libAuthorized(w, r) {
		return
	}
	page, size, e := libPage(r, 20)
	if e != nil || size > 100 {
		httpError(w, 422, "Invalid pagination (pageSize maximum 100)")
		return
	}
	q := r.URL.Query()
	where := []string{"1=1"}
	args := []any{}
	if key := q.Get("keyword"); key != "" {
		where = append(where, "(LOWER(a.title) LIKE ? OR LOWER(ss.provider_name) LIKE ?)")
		args = append(args, "%"+strings.ToLower(key)+"%", "%"+strings.ToLower(key)+"%")
	}
	for param, spec := range map[string][]string{"favoriteFilter": {"all", "favorited", "unfavorited", "ss.is_favorited"}, "refreshFilter": {"all", "enabled", "disabled", "ss.incremental_refresh_enabled"}, "finishedFilter": {"all", "finished", "unfinished", "ss.is_finished"}} {
		value := q.Get(param)
		if value == "" || value == "all" {
			continue
		}
		if value != spec[1] && value != spec[2] {
			httpError(w, 422, "Invalid "+param)
			return
		}
		where = append(where, spec[3]+" = ?")
		args = append(args, value == spec[1])
	}
	typ := q.Get("typeFilter")
	if typ != "" && typ != "all" {
		if typ != "movie" && typ != "tv_series" {
			httpError(w, 422, "Invalid typeFilter")
			return
		}
		where = append(where, "a.type = ?")
		args = append(args, typ)
	}
	sortBy := q.Get("sortBy")
	if sortBy == "" {
		sortBy = "created"
	}
	if sortBy != "created" && sortBy != "title" {
		httpError(w, 422, "Invalid sortBy")
		return
	}
	direction := q.Get("sortOrder")
	if direction == "" {
		direction = "desc"
	}
	if direction != "asc" && direction != "desc" {
		httpError(w, 422, "Invalid sortOrder")
		return
	}
	base := " FROM anime_sources ss JOIN anime a ON a.id = ss.anime_id WHERE " + strings.Join(where, " AND ")
	var total, totalSources, enabled, favorited int64
	e = s.Store.DB.QueryRowContext(r.Context(), s.Store.Rebind("SELECT COUNT(DISTINCT a.id),COUNT(*),COALESCE(SUM(CASE WHEN ss.incremental_refresh_enabled THEN 1 ELSE 0 END),0),COALESCE(SUM(CASE WHEN ss.is_favorited THEN 1 ELSE 0 END),0)"+base), args...).Scan(&total, &totalSources, &enabled, &favorited)
	if e != nil {
		libWriteError(w, e)
		return
	}
	order := "a.created_at"
	if sortBy == "title" {
		order = "a.title"
	}
	pageArgs := append(append([]any{}, args...), size, (page-1)*size)
	animes, e := s.libRows(r.Context(), s.Store.DB, "anime", "SELECT a.* FROM anime a WHERE a.id IN (SELECT ss.anime_id"+base+") ORDER BY "+order+" "+direction+",a.id LIMIT ? OFFSET ?", pageArgs...)
	if e != nil {
		libWriteError(w, e)
		return
	}
	out := []store.Row{}
	for _, a := range animes {
		sourceArgs := append(append([]any{}, args...), a["id"])
		sources, e := s.libRows(r.Context(), s.Store.DB, "anime_sources", "SELECT ss.*"+base+" AND a.id = ? ORDER BY ss.provider_name,ss.source_order", sourceArgs...)
		if e != nil {
			libWriteError(w, e)
			return
		}
		list := []store.Row{}
		for _, src := range sources {
			n, e := s.Store.Count(r.Context(), "episode", store.Row{"source_id": src["id"]})
			if e != nil {
				libWriteError(w, e)
				return
			}
			list = append(list, store.Row{"sourceId": src["id"], "providerName": src["provider_name"], "isFavorited": authBool(src["is_favorited"]), "incrementalRefreshEnabled": authBool(src["incremental_refresh_enabled"]), "incrementalRefreshFailures": authInt(src["incremental_refresh_failures"]), "lastRefreshLatestEpisodeAt": authDateWire(src["last_refresh_latest_episode_at"]), "episodeCount": n, "isFinished": authBool(src["is_finished"])})
		}
		out = append(out, store.Row{"animeId": a["id"], "animeTitle": a["title"], "animeType": a["type"], "season": a["season"], "imageUrl": a["image_url"], "localImagePath": a["local_image_path"], "sources": list})
	}
	max, e := strconv.Atoi(s.authConfig(r.Context(), "incrementalRefreshMaxFailures", "10"))
	if e != nil || max < 1 {
		max = 10
	}
	writeJSON(w, 200, store.Row{"total": total, "totalSources": totalSources, "refreshEnabled": enabled, "favorited": favorited, "maxFailures": max, "list": out})
}
func (s *Server) libIncrementalStatus(w http.ResponseWriter, r *http.Request) {
	if !s.libAuthorized(w, r) {
		return
	}
	out := store.Row{"exists": false, "enabled": false, "cronExpression": nil, "nextRunTime": nil, "taskId": nil}
	if s.Jobs != nil {
		tasks, e := s.Jobs.Schedules()
		if e != nil {
			libWriteError(w, e)
			return
		}
		for _, t := range tasks {
			if t.Kind == "incrementalRefresh" {
				out = store.Row{"exists": true, "enabled": t.IsEnabled, "cronExpression": t.CronExpression, "nextRunTime": t.NextRunAt, "taskId": t.ID}
				break
			}
		}
	}
	writeJSON(w, 200, out)
}
func (s *Server) libIncrementalJob(ctx context.Context, raw json.RawMessage, progress func(int, string)) (any, error) {
	total, e := s.Store.Count(ctx, "anime_sources", store.Row{"incremental_refresh_enabled": true, "is_finished": false})
	if e != nil {
		return nil, e
	}
	if total < 1 {
		total = 1
	}
	maxFailures, e := strconv.Atoi(s.authConfig(ctx, "incrementalRefreshMaxFailures", "10"))
	if e != nil || maxFailures < 1 {
		maxFailures = 10
	}
	var cursor int64
	done, failed := 0, 0
	examples := []string{}
	for {
		sources, e := s.libRows(ctx, s.Store.DB, "anime_sources", "SELECT * FROM anime_sources WHERE incremental_refresh_enabled = ? AND is_finished = ? AND id > ? ORDER BY id LIMIT 256", true, false, cursor)
		if e != nil {
			return nil, e
		}
		if len(sources) == 0 {
			break
		}
		for _, src := range sources {
			cursor = authInt(src["id"])
			if e = job.Checkpoint(ctx); e != nil {
				return nil, e
			}
			params, _ := json.Marshal(libTaskParams{Source: cursor, Mode: "incremental"})
			position := done + failed
			_, err := s.libSourceRefreshJob(ctx, params, func(n int, text string) {
				value := int((int64(position)*100 + int64(n)) / total)
				if value > 99 {
					value = 99
				}
				progress(value, text)
			})
			if err != nil {
				if ctx.Err() != nil {
					return nil, ctx.Err()
				}
				failed++
				if len(examples) < 20 {
					examples = append(examples, fmt.Sprintf("%d (%s)", cursor, authString(src["provider_name"])))
				}
				_, e = s.Store.DB.ExecContext(ctx, s.Store.Rebind("UPDATE anime_sources SET incremental_refresh_enabled = CASE WHEN incremental_refresh_failures + 1 >= ? THEN ? ELSE incremental_refresh_enabled END, incremental_refresh_failures = incremental_refresh_failures + 1 WHERE id = ?"), maxFailures, false, cursor)
			} else {
				done++
				e = s.Store.Update(ctx, "anime_sources", cursor, store.Row{"incremental_refresh_failures": 0})
			}
			if e != nil {
				return nil, e
			}
		}
	}
	result := store.Row{"updatedSources": done, "failedSources": failed}
	if failed > 0 {
		return result, fmt.Errorf("incremental refresh incomplete: %d failed source(s); examples: %s", failed, strings.Join(examples, ", "))
	}
	progress(100, "增量追更完成")
	return result, nil
}
