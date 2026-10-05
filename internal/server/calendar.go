// SPDX-License-Identifier: AGPL-3.0-only
package server

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/AniBakaBaka/AniDan/internal/cachebackend"
	"github.com/AniBakaBaka/AniDan/internal/integration"
	"github.com/AniBakaBaka/AniDan/internal/job"
	"github.com/AniBakaBaka/AniDan/internal/store"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
)

func (s *Server) registerCalendar(m *http.ServeMux) {
	m.HandleFunc("GET /api/ui/calendar/weekly", s.operator(s.calendarWeekly))
	m.HandleFunc("GET /api/ui/calendar/discover", s.operator(s.calendarDiscover))
	m.HandleFunc("GET /api/ui/calendar/tmdb-poster/{tmdb_id}", s.calendarPoster)
	m.HandleFunc("GET /api/ui/calendar/tmdb-title/{tmdb_id}", s.calendarTitle)
	m.HandleFunc("POST /api/ui/calendar/clear-cache", s.operator(s.calendarClear))
	m.HandleFunc("POST /api/ui/calendar/sync-bangumi-schedule", s.operator(s.calendarSync))
	m.HandleFunc("POST /api/ui/calendar/subscribe", s.operator(s.calendarSubscribeHTTP))
	m.HandleFunc("POST /api/ui/calendar/subscribe/batch", s.operator(s.calendarSubscribeBatch))
	m.HandleFunc("POST /api/ui/calendar/unsubscribe", s.operator(s.calendarUnsubscribe))
	m.HandleFunc("GET /api/ui/calendar/upcoming", s.operator(s.calendarUpcoming))
	m.HandleFunc("GET /api/ui/calendar/stale-episodes", s.operator(s.calendarStale))
}
func (s *Server) calQuery(ctx context.Context, query string, args ...any) ([]map[string]any, error) {
	rows, e := s.Store.DB.QueryContext(ctx, s.Store.Rebind(query), args...)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	cols, e := rows.Columns()
	if e != nil {
		return nil, e
	}
	out := []map[string]any{}
	for rows.Next() {
		v := make([]any, len(cols))
		p := make([]any, len(cols))
		for i := range v {
			p[i] = &v[i]
		}
		if e = rows.Scan(p...); e != nil {
			return nil, e
		}
		row := map[string]any{}
		for i, k := range cols {
			if b, ok := v[i].([]byte); ok {
				v[i] = string(b)
			}
			row[k] = v[i]
		}
		out = append(out, row)
		if len(out) > 100000 {
			return nil, errors.New("calendar response exceeds100,000 row safety limit")
		}
	}
	return out, rows.Err()
}
func (s *Server) localCalendar(ctx context.Context) ([]map[string]any, error) {
	q := `SELECT a.id AS anime_id,a.title AS anime_title,a.type AS anime_type,a.season,a.image_url,a.local_image_path,a.episode_count,s.id AS source_id,s.provider_name,s.media_id,m.bangumi_id,m.trakt_id,m.tmdb_id,m.air_weekday,m.air_time,(SELECT MAX(e.episode_index) FROM episode e WHERE e.source_id=s.id) AS latest_episode_index FROM anime_sources s JOIN anime a ON a.id=s.anime_id LEFT JOIN anime_metadata m ON m.anime_id=a.id WHERE s.incremental_refresh_enabled=? AND s.is_finished=? ORDER BY a.id,s.source_order`
	rows, e := s.calQuery(ctx, q, true, false)
	if e != nil {
		return nil, e
	}
	out := []map[string]any{}
	for _, row := range rows {
		v := map[string]any{}
		for k, x := range row {
			v[camel(k)] = x
		}
		v["isLocal"] = true
		v["isSubscribed"] = true
		v["origin"] = "local"
		v["scheduleSource"] = "local"
		v["provider"] = row["provider_name"]
		v["externalId"] = row["media_id"]
		v["availableSources"] = []any{}
		out = append(out, v)
	}
	return out, nil
}
func calendarTitleKey(v string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsNumber(r) {
			return unicode.ToLower(r)
		}
		return -1
	}, v)
}
func (s *Server) calendarFetch(ctx context.Context, name string, uid int64, force bool) ([]map[string]any, error) {
	if s.Metadata == nil {
		return nil, errors.New("metadata client unavailable")
	}
	cred, e := s.metadataCredential(ctx, name, uid)
	if e != nil {
		return nil, e
	}
	ctx, e = s.Metadata.WithRoutingContext(ctx, name)
	if e != nil {
		return nil, e
	}
	now := s.calNowTime()
	identity, e := s.Metadata.ResponseCacheIdentity(ctx, name, "calendar", fmt.Sprintf("%s:%d:7", now.Format("2006-01-02"), uid), "", cred)
	if e != nil {
		return nil, e
	}
	load := func(loadCtx context.Context) (json.RawMessage, error) {
		data, err := s.Metadata.Calendar(loadCtx, name, now, 7, cred)
		if err != nil {
			return nil, err
		}
		if len(data) > 10000 {
			return nil, errors.New("calendar provider returned too many entries")
		}
		for _, entry := range data {
			id := str(entry["bangumiId"])
			if name == "trakt" {
				id = str(entry["traktTmdbId"])
				if id == "" {
					id = str(entry["traktId"])
				}
			}
			if id == "" {
				return nil, errors.New("provider calendar entry lacks identity")
			}
		}
		return json.Marshal(data)
	}
	var raw json.RawMessage
	if s.Cache != nil {
		if force {
			generation, generationErr := s.Cache.Generation(ctx, "calendar")
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			raw, e = load(ctx)
			if e == nil && generationErr == nil {
				_, _ = s.Cache.SetIfGeneration(ctx, "calendar", identity, cachebackend.Entry{JSON: raw, ExpiresAt: time.Now().Add(30 * time.Minute)}, generation)
			}
		} else {
			raw, e = s.Cache.DoValidated(ctx, "calendar", identity, 30*time.Minute, func(raw json.RawMessage) error { return validateCalendarCache(raw, name) }, load)
		}
	} else {
		raw, e = load(ctx)
	}
	if e != nil {
		return nil, e
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var data []map[string]any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if e = decoder.Decode(&data); e != nil {
		return nil, e
	}
	// A cache hit may be shared across responses or survive restart. Domain
	// calendar rows are still upserted in this database, preserving user intent.

	s.importMu.Lock()
	tx, e := s.Store.DB.BeginTx(ctx, nil)
	if e != nil {
		s.importMu.Unlock()
		return nil, e
	}
	for _, v := range data {
		id := str(v["bangumiId"])
		if name == "trakt" {
			id = str(v["traktTmdbId"])
			if id == "" {
				id = str(v["traktId"])
			}
		}
		if id == "" {
			e = errors.New("provider calendar entry lacks identity")
			break
		}
		patch := store.Row{"anime_title": str(v["animeTitle"]), "fetched_at": s.now()}
		for _, k := range []string{"year", "season", "rating"} {
			if v[k] != nil {
				patch[k] = v[k]
			}
		}
		for a, b := range map[string]string{"imageUrl": "image_url", "airWeekday": "air_weekday", "airDate": "air_date", "airTime": "air_time", "episodeCount": "episode_count", "latestEpisodeIndex": "latest_episode_index", "bangumiId": "bangumi_id", "traktId": "trakt_id", "traktTmdbId": "tmdb_id", "traktImdbId": "imdb_id"} {
			if v[a] != nil {
				patch[b] = v[a]
			}
		}
		if _, e = s.calUpsertTx(ctx, tx, name, id, patch, nil); e != nil {
			break
		}
	}
	if e == nil {
		e = tx.Commit()
	} else {
		_ = tx.Rollback()
	}
	s.importMu.Unlock()
	if e != nil {
		return nil, e
	}
	return data, nil
}
func (s *Server) calendarWeekly(w http.ResponseWriter, r *http.Request) {
	warnings := []string{}
	for _, name := range []string{"bangumi", "trakt"} {
		setting, e := s.Store.Get(r.Context(), "metadata_sources", name)
		if e != nil && !errors.Is(e, sql.ErrNoRows) {
			libWriteError(w, e)
			return
		}
		if setting != nil && !boolean(setting["is_enabled"]) {
			continue
		}
		if _, e = s.calendarFetch(r.Context(), name, s.metadataUserID(r), false); e != nil {
			warnings = append(warnings, name+": "+e.Error())
		}
	}
	local, e := s.localCalendar(r.Context())
	if e != nil {
		libWriteError(w, e)
		return
	}
	weekly := map[int][]map[string]any{}
	for i := 1; i <= 7; i++ {
		weekly[i] = []map[string]any{}
	}
	unscheduled := []map[string]any{}
	byKey := map[string]map[string]any{}
	total, scheduled := 0, 0
	add := func(v map[string]any) {
		day := int(number(v["airWeekday"]))
		if day >= 1 && day <= 7 {
			weekly[day] = append(weekly[day], v)
			scheduled++
		} else {
			unscheduled = append(unscheduled, v)
		}
		total++
	}
	for _, v := range local {
		key := calendarTitleKey(str(v["animeTitle"]))
		byKey[key] = v
		add(v)
	}
	counts := map[string]int{}
	e = s.subscriptionEach(r.Context(), nil, func(row store.Row) error {
		v := subscriptionWire(row)
		if v["extraDataError"] != nil {
			return errors.New("invalid calendar extraData")
		}
		if str(v["parentExternalId"]) != "" || v["animeType"] == "subscription" {
			return nil
		}
		key := calendarTitleKey(str(v["animeTitle"]))
		source := map[string]any{"provider": v["provider"], "externalId": v["externalId"], "title": v["animeTitle"], "subscriptionType": v["subscriptionType"], "isSubscribed": v["isSubscribed"]}
		if old, ok := byKey[key]; ok && key != "" {
			a, _ := old["availableSources"].([]any)
			old["availableSources"] = append(a, source)
			return nil
		}
		v["sourceId"] = nil
		v["animeId"] = nil
		v["isLocal"] = false
		v["origin"] = v["provider"]
		v["providerName"] = nil
		v["scheduleSource"] = v["provider"]
		v["availableSources"] = []any{source}
		byKey[key] = v
		counts[str(v["provider"])]++
		add(v)
		return nil
	})
	if e != nil {
		libWriteError(w, e)
		return
	}
	for _, rows := range weekly {
		sort.SliceStable(rows, func(i, j int) bool {
			a, b := boolean(rows[i]["isLocal"]), boolean(rows[j]["isLocal"])
			if a != b {
				return a
			}
			return str(rows[i]["airTime"]) < str(rows[j]["airTime"])
		})
	}
	stats := map[string]any{"total": total, "local": len(local), "scheduled": scheduled, "unscheduled": len(unscheduled)}
	for k, v := range counts {
		stats[k] = v
	}
	writeJSON(w, 200, map[string]any{"weekly": weekly, "unscheduled": unscheduled, "stats": stats, "warnings": warnings})
}
func (s *Server) calendarDiscover(w http.ResponseWriter, r *http.Request) {
	data, e := s.calendarFetch(r.Context(), "bangumi", s.metadataUserID(r), false)
	if e != nil {
		httpError(w, 502, e.Error())
		return
	}
	ids, e := s.calQuery(r.Context(), "SELECT bangumi_id FROM anime_metadata WHERE bangumi_id IS NOT NULL")
	if e != nil {
		libWriteError(w, e)
		return
	}
	local := map[string]bool{}
	for _, v := range ids {
		local[str(v["bangumi_id"])] = true
	}
	weekly := map[int][]map[string]any{}
	count := 0
	for _, v := range data {
		day := int(number(v["airWeekday"]))
		isLocal := local[str(v["bangumiId"])]
		if isLocal {
			count++
		}
		weekly[day] = append(weekly[day], map[string]any{"bangumiId": v["bangumiId"], "title": v["animeTitle"], "titleJp": v["animeTitle"], "airDate": v["airDate"], "airWeekday": day, "imageUrl": v["imageUrl"], "rating": v["rating"], "rank": v["rank"], "isLocal": isLocal})
	}
	writeJSON(w, 200, map[string]any{"weekly": weekly, "stats": map[string]any{"total": len(data), "localCount": count}})
}
func (s *Server) calendarTMDB(r *http.Request) (*integration.Metadata, error) {
	id := r.PathValue("tmdb_id")
	if n, e := strconv.ParseInt(id, 10, 64); e != nil || n < 1 {
		return nil, libErr(422, "invalid TMDB ID")
	}
	if s.Metadata == nil {
		return nil, errors.New("metadata unavailable")
	}
	return s.Metadata.Details(r.Context(), "tmdb", id, "tv", integration.Credential{})
}
func (s *Server) calendarPoster(w http.ResponseWriter, r *http.Request) {
	v, e := s.calendarTMDB(r)
	if e != nil || v == nil || v.ImageURL == "" {
		w.Header().Set("Cache-Control", "public, max-age=3600")
		httpError(w, 404, "Poster unavailable")
		return
	}
	u, e := url.Parse(v.ImageURL)
	if e != nil || u.Scheme != "https" || u.User != nil {
		httpError(w, 502, "Invalid poster URL")
		return
	}
	w.Header().Set("Cache-Control", "public, max-age=604800, immutable")
	http.Redirect(w, r, v.ImageURL, 302)
}
func (s *Server) calendarTitle(w http.ResponseWriter, r *http.Request) {
	v, e := s.calendarTMDB(r)
	if e != nil || v == nil {
		writeJSON(w, 200, map[string]any{"title": nil, "year": nil})
		return
	}
	writeJSON(w, 200, map[string]any{"title": v.Title, "year": v.Year})
}
func (s *Server) calendarClear(w http.ResponseWriter, r *http.Request) {
	s.importMu.Lock()
	defer s.importMu.Unlock()
	tx, e := s.Store.DB.BeginTx(r.Context(), nil)
	if e != nil {
		libWriteError(w, e)
		return
	}
	defer tx.Rollback()
	rows, e := s.libRows(r.Context(), tx, "external_calendar_item", "SELECT * FROM external_calendar_item WHERE is_subscribed=?", false)
	if e != nil {
		libWriteError(w, e)
		return
	}
	deleted := int64(0)
	for _, row := range rows {
		extra, e := subscriptionExtra(row)
		if e != nil {
			libWriteError(w, e)
			return
		}
		if str(extra["parentExternalId"]) != "" {
			continue
		}
		res, e := tx.ExecContext(r.Context(), s.Store.Rebind("DELETE FROM external_calendar_item WHERE id=?"), row["id"])
		if e != nil {
			libWriteError(w, e)
			return
		}
		n, _ := res.RowsAffected()
		deleted += n
	}
	res, e := tx.ExecContext(r.Context(), s.Store.Rebind("DELETE FROM cache_data WHERE "+disposableCacheSQL+" AND (cache_provider=? OR cache_key LIKE ? OR cache_key LIKE ? OR cache_key LIKE ?)"), "external_calendar", "anidan_calendar_%", "trakt_calendar_%", "bangumi_calendar_%")
	if e != nil {
		libWriteError(w, e)
		return
	}
	n, _ := res.RowsAffected()
	deleted += n
	if e = tx.Commit(); e != nil {
		libWriteError(w, e)
		return
	}
	if s.Cache != nil {
		count, err := s.Cache.Clear(r.Context(), "calendar")
		if err != nil {
			writeJSON(w, 503, map[string]any{"message": "日历记录已清理，但响应缓存后端清理失败；订阅与候选状态已保留", "deletedCount": deleted, "partial": true, "responseCacheCleared": false})
			return
		}
		deleted += int64(count)
	}
	writeJSON(w, 200, map[string]any{"message": "日历缓存已清除，保留订阅与候选状态", "deletedCount": deleted})
}
func (s *Server) calendarSync(w http.ResponseWriter, r *http.Request) {
	data := []map[string]any{}
	warnings := []string{}
	for _, name := range []string{"bangumi", "trakt"} {
		v, e := s.calendarFetch(r.Context(), name, s.metadataUserID(r), true)
		if e != nil {
			warnings = append(warnings, name+": "+e.Error())
			continue
		}
		data = append(data, v...)
	}
	if len(data) == 0 && len(warnings) > 0 {
		httpError(w, 502, strings.Join(warnings, "; "))
		return
	}
	local, e := s.localCalendar(r.Context())
	if e != nil {
		libWriteError(w, e)
		return
	}
	updated, bound := 0, 0
	seen := map[int64]bool{}
	for _, row := range local {
		aid := number(row["animeId"])
		if seen[aid] {
			continue
		}
		seen[aid] = true
		for _, v := range data {
			matched := str(row["bangumiId"]) != "" && str(row["bangumiId"]) == str(v["bangumiId"])
			matched = matched || (str(row["traktId"]) != "" && str(row["traktId"]) == str(v["traktId"]))
			if !matched && calendarTitleKey(str(row["animeTitle"])) != calendarTitleKey(str(v["animeTitle"])) {
				continue
			}
			patch := store.Row{"air_weekday": v["airWeekday"], "air_time": v["airTime"]}
			if str(row["bangumiId"]) == "" && str(v["bangumiId"]) != "" {
				patch["bangumi_id"] = v["bangumiId"]
				bound++
			}
			if str(row["traktId"]) == "" && str(v["traktId"]) != "" {
				patch["trakt_id"] = v["traktId"]
				bound++
			}
			meta, e := s.Store.List(r.Context(), "anime_metadata", store.Row{"anime_id": aid}, 1, 0)
			if e != nil {
				libWriteError(w, e)
				return
			}
			if len(meta) == 0 {
				patch["anime_id"] = aid
				_, e = s.Store.Insert(r.Context(), "anime_metadata", patch)
			} else {
				e = s.Store.Update(r.Context(), "anime_metadata", meta[0]["id"], patch)
			}
			if e != nil {
				libWriteError(w, e)
				return
			}
			updated++
			break
		}
	}
	writeJSON(w, 200, map[string]any{"updated": updated, "bound": bound, "details": []any{}, "warnings": warnings, "message": "已同步可验证的日历条目；未对模糊标题强制绑定"})
}
func (s *Server) calendarUpcoming(w http.ResponseWriter, r *http.Request) {
	days := 7
	if raw := r.URL.Query().Get("days"); raw != "" {
		n, e := strconv.Atoi(raw)
		if e != nil || n < 1 || n > 30 {
			httpError(w, 422, "days must be1..30")
			return
		}
		days = n
	}
	local, e := s.localCalendar(r.Context())
	if e != nil {
		libWriteError(w, e)
		return
	}
	out := []map[string]any{}
	seen := map[int64]bool{}
	now := int(s.calNowTime().Weekday())
	if now == 0 {
		now = 7
	}
	for _, v := range local {
		aid := number(v["animeId"])
		if seen[aid] {
			continue
		}
		day := int(number(v["airWeekday"]))
		if day < 1 || day > 7 {
			continue
		}
		diff := (day - now + 7) % 7
		if diff > days {
			continue
		}
		seen[aid] = true
		label := fmt.Sprintf("in_%d_days", diff)
		if diff == 0 {
			label = "today"
		} else if diff == 1 {
			label = "tomorrow"
		}
		has := false
		rows, e := s.Store.List(r.Context(), "episode", store.Row{"source_id": v["sourceId"], "episode_index": v["latestEpisodeIndex"]}, 1, 0)
		if e != nil {
			libWriteError(w, e)
			return
		}
		if len(rows) > 0 {
			has = number(rows[0]["comment_count"]) > 0
		}
		out = append(out, map[string]any{"animeId": aid, "title": v["animeTitle"], "season": v["season"], "airWeekday": day, "airTime": v["airTime"], "dayLabel": label, "daysUntil": diff, "latestEpisode": v["latestEpisodeIndex"], "latestHasDanmaku": has, "imageUrl": v["imageUrl"]})
	}
	sort.SliceStable(out, func(i, j int) bool { return number(out[i]["daysUntil"]) < number(out[j]["daysUntil"]) })
	writeJSON(w, 200, out)
}
func (s *Server) calendarStale(w http.ResponseWriter, r *http.Request) {
	n, e := strconv.Atoi(s.setting(r.Context(), "stale_episode_threshold", "5"))
	if e != nil || n < 0 {
		httpError(w, 400, "invalid stale episode threshold")
		return
	}
	rows, e := s.calQuery(r.Context(), `SELECT e.id AS episode_id,e.title,e.episode_index,e.comment_count,a.title AS anime_title,a.id AS anime_id FROM episode e JOIN anime_sources s ON s.id=e.source_id JOIN anime a ON a.id=s.anime_id WHERE s.incremental_refresh_enabled=? AND e.comment_count<=? ORDER BY a.id,e.episode_index LIMIT 100`, true, n)
	if e != nil {
		libWriteError(w, e)
		return
	}
	out := []map[string]any{}
	for _, v := range rows {
		m := map[string]any{}
		for k, x := range v {
			m[camel(k)] = x
		}
		out = append(out, m)
	}
	writeJSON(w, 200, out)
}

type calendarSubscribeRequest struct {
	Title      string   `json:"animeTitle"`
	MediaType  string   `json:"mediaType"`
	Season     *int     `json:"season"`
	TMDBID     string   `json:"traktTmdbId"`
	TraktID    string   `json:"traktId"`
	BangumiID  string   `json:"bangumiId"`
	Provider   string   `json:"provider"`
	ExternalID string   `json:"externalId"`
	RunNow     *bool    `json:"runNow"`
	Selected   []string `json:"selectedEpisodes"`
}

func (v calendarSubscribeRequest) identity() (string, string) {
	if v.Provider != "" && v.ExternalID != "" {
		return v.Provider, v.ExternalID
	}
	if v.BangumiID != "" {
		return "bangumi", v.BangumiID
	}
	if v.TMDBID != "" {
		return "trakt", v.TMDBID
	}
	return "", ""
}
func (s *Server) calendarSubscribe(ctx context.Context, v calendarSubscribeRequest) (map[string]any, error) {
	name, id := v.identity()
	if name == "" || id == "" || strings.TrimSpace(v.Title) == "" {
		return nil, libErr(400, "animeTitle and external identity required")
	}
	typ := v.MediaType
	if typ == "" {
		typ = "tv_series"
	}
	if typ != "tv_series" && typ != "movie" {
		return nil, libErr(422, "unsupported media type")
	}
	patch := store.Row{"anime_title": v.Title, "anime_type": typ, "season": nil, "is_subscribed": true, "subscription_status": "pending"}
	if v.Season != nil {
		patch["season"] = *v.Season
	}
	for k, val := range map[string]string{"bangumi_id": v.BangumiID, "trakt_id": v.TraktID, "tmdb_id": v.TMDBID} {
		if val != "" {
			patch[k] = val
		}
	}
	row, e := s.calUpsert(ctx, name, id, patch, map[string]any{"selectedEpisodes": v.Selected, "enabled": true})
	if e != nil {
		return nil, e
	}
	if v.RunNow != nil && !*v.RunNow {
		return map[string]any{"message": "已保存订阅，等待订阅扫描任务", "taskId": nil, "subscriptionStatus": "pending"}, nil
	}
	task, e := s.Jobs.SubmitWithOptions("calendar_import", map[string]any{"targetId": row["id"]}, nil, job.SubmitOptions{Title: "导入日历订阅", UniqueKey: fmt.Sprintf("calendar:%v", row["id"])})
	if e != nil {
		return nil, e
	}
	return map[string]any{"message": "订阅任务已提交", "taskId": task, "subscriptionStatus": "importing"}, nil
}
func (s *Server) calendarSubscribeHTTP(w http.ResponseWriter, r *http.Request) {
	var v calendarSubscribeRequest
	if e := readJSON(r, &v); e != nil {
		httpError(w, 422, e.Error())
		return
	}
	out, e := s.calendarSubscribe(r.Context(), v)
	if e != nil {
		libWriteError(w, e)
		return
	}
	writeJSON(w, 200, out)
}
func (s *Server) calendarSubscribeBatch(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Items  []calendarSubscribeRequest `json:"items"`
		RunNow *bool                      `json:"runNow"`
	}
	if e := readJSON(r, &in); e != nil {
		httpError(w, 422, e.Error())
		return
	}
	if len(in.Items) > 200 {
		httpError(w, 422, "at most200 items per batch")
		return
	}
	out := []map[string]any{}
	ok := 0
	for _, v := range in.Items {
		v.RunNow = in.RunNow
		res, e := s.calendarSubscribe(r.Context(), v)
		if e != nil {
			out = append(out, map[string]any{"animeTitle": v.Title, "success": false, "error": e.Error()})
		} else {
			res["animeTitle"] = v.Title
			res["success"] = true
			out = append(out, res)
			ok++
		}
	}
	writeJSON(w, 200, map[string]any{"successCount": ok, "failureCount": len(out) - ok, "results": out})
}
func (s *Server) calendarUnsubscribe(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Provider   string `json:"provider"`
		ExternalID string `json:"externalId"`
		SourceID   int64  `json:"sourceId"`
		BangumiID  string `json:"bangumiId"`
		TraktID    string `json:"traktId"`
		TMDBID     string `json:"traktTmdbId"`
	}
	if e := readJSON(r, &in); e != nil {
		httpError(w, 422, e.Error())
		return
	}
	changed := false
	if in.SourceID > 0 {
		row, e := s.Store.Get(r.Context(), "anime_sources", in.SourceID)
		if e == nil {
			e = s.Store.Update(r.Context(), "anime_sources", row["id"], store.Row{"is_finished": true})
			if e != nil {
				libWriteError(w, e)
				return
			}
			changed = true
		} else if !errors.Is(e, sql.ErrNoRows) {
			libWriteError(w, e)
			return
		}
	}
	if in.Provider != "" && in.ExternalID != "" {
		row, e := s.libOne(r.Context(), s.Store.DB, "external_calendar_item", "provider=? AND external_id=?", in.Provider, in.ExternalID)
		if e == nil {
			e = s.Store.Update(r.Context(), "external_calendar_item", row["id"], store.Row{"is_subscribed": false, "subscription_status": nil, "subscription_failure_count": 0, "subscription_last_attempt_at": nil})
			if e != nil {
				libWriteError(w, e)
				return
			}
			changed = true
		} else if !errors.Is(e, sql.ErrNoRows) {
			libWriteError(w, e)
			return
		}
	}
	if !changed {
		for k, v := range map[string]string{"bangumi_id": in.BangumiID, "trakt_id": in.TraktID, "tmdb_id": in.TMDBID} {
			if v == "" {
				continue
			}
			meta, e := s.Store.List(r.Context(), "anime_metadata", store.Row{k: v}, 1000, 0)
			if e != nil {
				libWriteError(w, e)
				return
			}
			for _, m := range meta {
				rows, e := s.Store.List(r.Context(), "anime_sources", store.Row{"anime_id": m["anime_id"]}, 1000, 0)
				if e != nil {
					libWriteError(w, e)
					return
				}
				for _, row := range rows {
					if e = s.Store.Update(r.Context(), "anime_sources", row["id"], store.Row{"is_finished": true}); e != nil {
						libWriteError(w, e)
						return
					}
					changed = true
				}
			}
		}
	}
	writeJSON(w, 200, map[string]any{"success": changed, "message": "取消订阅已处理"})
}
func (s *Server) initCalendarJobs() error {
	return s.Jobs.Register("calendar_import", func(ctx context.Context, raw json.RawMessage, progress func(int, string)) (any, error) {
		var in struct {
			ID int64 `json:"targetId"`
		}
		if e := unmarshalExactJSON(raw, &in); e != nil {
			return nil, e
		}
		target, e := s.Store.Get(ctx, "external_calendar_item", in.ID)
		if e != nil {
			return nil, e
		}
		if !boolean(target["is_subscribed"]) {
			return nil, errors.New("subscription cancelled")
		}
		extra, e := subscriptionExtra(target)
		if e != nil {
			return nil, e
		}
		if extra["enabled"] == false {
			return nil, errors.New("subscription paused")
		}
		req := ImportRequest{Title: str(target["anime_title"]), Type: str(target["anime_type"]), Season: int(number(target["season"]))}
		if req.Season < 1 {
			req.Season = 1
		}
		name, id := str(target["provider"]), str(target["external_id"])
		if s.subscriptionCapable(name) {
			req.Provider = name
			req.MediaID = id
		} else {
			results, e := s.providerSearch(ctx, req.Title)
			if e != nil && len(results) == 0 {
				return nil, e
			}
			for _, v := range results {
				if calendarTitleKey(v.Title) == calendarTitleKey(req.Title) {
					req.Provider = v.Provider
					req.MediaID = v.ID
					break
				}
			}
			if req.Provider == "" {
				return nil, errors.New("no exact title match; manual source selection required")
			}
		}
		if values, ok := extra["selectedEpisodes"].([]any); ok && len(values) > 0 {
			p, _ := s.Providers.Get(req.Provider)
			eps, e := p.Episodes(ctx, req.MediaID)
			if e != nil {
				return nil, e
			}
			set := map[string]bool{}
			for _, v := range values {
				set[str(v)] = true
			}
			for _, ep := range eps {
				if set[ep.ID] {
					req.Episodes = append(req.Episodes, ImportEpisode{ID: ep.ID, Title: ep.Title, Index: ep.Index, URL: ep.URL})
				}
			}
			if len(req.Episodes) != len(set) {
				return nil, errors.New("selected episodes no longer resolve exactly")
			}
		}
		b, _ := json.Marshal(req)
		result, e := s.runImport(ctx, b, progress)
		if e != nil {
			_, _ = s.calUpsert(ctx, name, id, store.Row{"subscription_status": "failed", "subscription_failure_count": number(target["subscription_failure_count"]) + 1}, map[string]any{"lastError": e.Error()})
			return nil, e
		}
		var values map[string]any
		b, _ = json.Marshal(result)
		dec := json.NewDecoder(strings.NewReader(string(b)))
		dec.UseNumber()
		if e = dec.Decode(&values); e != nil {
			return nil, e
		}
		_, e = s.calUpsert(ctx, name, id, store.Row{"subscription_status": "active", "local_anime_id": values["animeId"], "local_source_id": values["sourceId"]}, nil)
		if e != nil {
			return nil, e
		}
		if values["sourceId"] != nil {
			e = s.Store.Update(ctx, "anime_sources", values["sourceId"], store.Row{"incremental_refresh_enabled": true})
		}
		return result, e
	})
}
func (s *Server) syncAniBTSubscription(ctx context.Context, raw string) error {
	if raw == "" {
		row, e := s.libOne(ctx, s.Store.DB, "external_calendar_item", "provider=? AND external_id=?", "anibt", "rss-feed")
		if errors.Is(e, sql.ErrNoRows) {
			return nil
		}
		if e != nil {
			return e
		}
		return s.Store.Update(ctx, "external_calendar_item", row["id"], store.Row{"is_subscribed": false, "subscription_status": nil})
	}
	if s.Metadata == nil {
		return errors.New("metadata client unavailable")
	}
	if e := s.Metadata.ValidateAniBTRSSURL(ctx, raw); e != nil {
		return e
	}
	_, e := s.calUpsert(ctx, "anibt", "rss-feed", store.Row{"anime_title": "AniBT RSS", "anime_type": "subscription", "is_subscribed": true, "subscription_status": "pending"}, map[string]any{"subscriptionType": "anibt_rss_feed", "url": raw, "enabled": true})
	return e
}

func (s *Server) scanAniBTRSS(ctx context.Context, target store.Row, extra map[string]any) (int, error) {
	if s.Metadata == nil {
		return 0, errors.New("metadata client unavailable")
	}
	items, e := s.Metadata.AniBTRSS(ctx, str(extra["url"]))
	if e != nil {
		return 0, e
	}
	s.importMu.Lock()
	defer s.importMu.Unlock()
	tx, e := s.Store.DB.BeginTx(ctx, nil)
	if e != nil {
		return 0, e
	}
	defer tx.Rollback()
	n := 0
	for _, v := range items {
		if e = ctx.Err(); e != nil {
			return 0, e
		}
		id := str(v["externalId"])
		if id == "" {
			return 0, errors.New("RSS entry missing subject identity")
		}
		old, err := s.libOne(ctx, tx, "external_calendar_item", "provider=? AND external_id=?", "anibt", id)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return 0, err
		}
		if old != nil {
			continue
		} // Respect prior manual cancellation/completion.
		ex, _ := v["extraData"].(map[string]any)
		if ex == nil {
			ex = map[string]any{}
		}
		ex["subscriptionType"] = "anibt_subject"
		ex["enabled"] = true
		ex["rssParentId"] = target["external_id"]
		row := store.Row{"anime_title": str(v["title"]), "anime_type": "tv_series", "is_subscribed": true, "subscription_status": "pending"}
		if ex["bangumiId"] != nil {
			row["bangumi_id"] = ex["bangumiId"]
		}
		if _, e = s.calUpsertTx(ctx, tx, "anibt", id, row, ex); e != nil {
			return 0, e
		}
		n++
	}
	_, e = s.calUpsertTx(ctx, tx, "anibt", str(target["external_id"]), nil, map[string]any{"lastScanAt": s.now(), "nextScanAt": s.calNowTime().Add(15 * time.Minute).Format("2006-01-02T15:04:05"), "lastError": nil})
	if e != nil {
		return 0, e
	}
	return n, tx.Commit()
}

func validateCalendarCache(raw json.RawMessage, name string) error {
	var data []map[string]any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&data); err != nil {
		return errors.New("cached calendar shape is invalid")
	}
	if len(data) > 10000 {
		return errors.New("cached calendar is too large")
	}
	for _, row := range data {
		id := str(row["bangumiId"])
		if name == "trakt" {
			id = str(row["traktTmdbId"])
			if id == "" {
				id = str(row["traktId"])
			}
		}
		if id == "" {
			return errors.New("cached calendar lacks identity")
		}
	}
	return nil
}
