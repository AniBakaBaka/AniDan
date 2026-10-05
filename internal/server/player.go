// SPDX-License-Identifier: AGPL-3.0-only
// Player wire contracts derived from Misaka v2.8.9 (see THIRD_PARTY_NOTICES.md).
package server

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/AniBakaBaka/AniDan/internal/danmaku"
	"github.com/AniBakaBaka/AniDan/internal/provider"
	"github.com/AniBakaBaka/AniDan/internal/recognition"
	"github.com/AniBakaBaka/AniDan/internal/store"
)

func (s *Server) registerPlayer(mux *http.ServeMux) {
	if s.Jobs != nil {
		if e := s.Jobs.Register("player_match", s.runPlayerMatch); e != nil {
			panic(e)
		}
	}
	for _, prefix := range []string{"/api/v1/{token}", "/api/v1/{token}/api/v2"} {
		mux.HandleFunc("GET "+prefix+"/search/anime", s.playerAuth(s.playerSearchAnime))
		mux.HandleFunc("GET "+prefix+"/search/episodes", s.playerAuth(s.playerSearchEpisodes))
		mux.HandleFunc("GET "+prefix+"/bangumi/{bangumiId}", s.playerAuth(s.playerBangumi))
		mux.HandleFunc("POST "+prefix+"/match", s.playerAuth(s.playerMatch))
		mux.HandleFunc("POST "+prefix+"/match/batch", s.playerAuth(s.playerMatchBatch))
		mux.HandleFunc("GET "+prefix+"/comment/{episodeId}", s.playerAuth(s.playerComments))
		mux.HandleFunc("GET "+prefix+"/extcomment", s.playerAuth(s.playerExternalComments))
		mux.HandleFunc("GET "+prefix+"/taskcomment/{taskId}", s.playerAuth(s.playerTask))
		mux.HandleFunc("GET "+prefix+"/version", func(w http.ResponseWriter, r *http.Request) {
			if e := s.validatePlayer(r); e != nil {
				httpError(w, 403, "Invalid API token")
				return
			}
			writeJSON(w, 200, map[string]any{"success": true, "errorCode": 0, "errorMessage": "", "serverName": "AniDan", "version": Version, "serverTime": s.now()})
		})
	}
}
func playerError(w http.ResponseWriter, status int, msg string) {
	code := 1003
	if status == 400 || status == 422 {
		code = 1001
	}
	if status >= 500 {
		code = 500
	}
	if code == 1003 {
		msg = "请求的资源不可用或您没有权限访问。"
	}
	writeJSON(w, 200, map[string]any{"success": false, "errorCode": code, "errorMessage": msg})
}
func playerOK(m map[string]any) map[string]any {
	m["success"] = true
	m["errorCode"] = 0
	m["errorMessage"] = ""
	return m
}
func (s *Server) playerAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if e := s.validatePlayer(r); e != nil {
			playerError(w, 403, e.Error())
			return
		}
		next(w, r)
	}
}
func (s *Server) validatePlayer(r *http.Request) error {
	token := r.PathValue("token")
	if token == "" {
		return errors.New("token required")
	}
	rows, e := s.Store.List(r.Context(), "api_tokens", store.Row{"token": token}, 1, 0)
	if e != nil {
		return e
	}
	if len(rows) == 0 {
		return errors.New("invalid token")
	}
	t := rows[0]
	if subtle.ConstantTimeCompare([]byte(str(t["token"])), []byte(token)) != 1 || !boolean(t["is_enabled"]) {
		return errors.New("disabled token")
	}
	now := s.now()
	if exp := str(t["expires_at"]); exp != "" && normalizeDate(exp) < normalizeDate(now) {
		return errors.New("expired token")
	}
	mode := s.setting(r.Context(), "uaFilterMode", "off")
	if mode != "off" {
		rules, e := s.Store.List(r.Context(), "ua_rules", nil, 10000, 0)
		if e != nil {
			return e
		}
		matched := false
		for _, rule := range rules {
			if strings.Contains(r.UserAgent(), str(rule["ua_string"])) {
				matched = true
				break
			}
		}
		if mode == "blacklist" && matched || mode == "whitelist" && !matched {
			return errors.New("user agent forbidden")
		}
	}
	// Atomic reset + quota + increment prevents concurrent requests exceeding quota.
	q := s.Store.Quote
	p := s.Store.Placeholder
	day := now[:10]
	oldDay := "(" + q("last_call_at") + " IS NULL OR " + q("last_call_at") + " < " + p(1) + ")"
	query := "UPDATE " + q("api_tokens") + " SET " + q("daily_call_count") + " = CASE WHEN " + oldDay + " THEN 1 ELSE " + q("daily_call_count") + " + 1 END, " + q("last_call_at") + " = " + p(2) + " WHERE " + q("id") + " = " + p(3) + " AND (" + q("daily_call_limit") + " = -1 OR (" + q("daily_call_limit") + " > 0 AND (" + q("daily_call_count") + " < " + q("daily_call_limit") + " OR " + q("last_call_at") + " IS NULL OR " + q("last_call_at") + " < " + p(4) + ")))"
	result, e := s.Store.DB.ExecContext(r.Context(), query, day, now, t["id"], day)
	if e != nil {
		return e
	}
	n, e := result.RowsAffected()
	if e != nil {
		return e
	}
	if n != 1 {
		return fmt.Errorf("daily call limit exceeded")
	}
	return nil
}
func normalizeDate(s string) string { return strings.ReplaceAll(s, " ", "T") }
func (s *Server) localSearch(ctx context.Context, keyword string) ([]store.Row, error) {
	// Filtering in Go deliberately uses Unicode case folding rather than relying on
	// database-specific collations. The bounded scan is paginated, not all-at-once.
	needle := normalizedTitle(keyword)
	out := []store.Row{}
	for offset := 0; ; offset += 500 {
		batch, e := s.Store.List(ctx, "anime", nil, 500, offset)
		if e != nil {
			return nil, e
		}
		for _, a := range batch {
			if strings.Contains(normalizedTitle(str(a["title"])), needle) {
				out = append(out, a)
			}
		}
		if len(batch) < 500 {
			break
		}
		if e := ctx.Err(); e != nil {
			return nil, e
		}
	}
	aliases, e := s.Store.List(ctx, "anime_aliases", nil, 10000, 0)
	if e != nil {
		return nil, e
	}
	seen := map[int64]bool{}
	for _, a := range out {
		seen[number(a["id"])] = true
	}
	for _, a := range aliases {
		if seen[number(a["anime_id"])] {
			continue
		}
		for _, k := range []string{"name_en", "name_jp", "name_romaji", "alias_cn_1", "alias_cn_2", "alias_cn_3"} {
			if v := normalizedTitle(str(a[k])); v != "" && strings.Contains(v, needle) {
				row, e := s.Store.Get(ctx, "anime", a["anime_id"])
				if e == nil && row != nil {
					out = append(out, row)
					seen[number(a["anime_id"])] = true
				}
				break
			}
		}
	}
	return out, nil
}
func playerType(a store.Row) (string, string) {
	t := str(a["type"])
	names := map[string]string{"tv_series": "tvseries", "movie": "movie", "ova": "ova", "other": "other"}
	desc := map[string]string{"tv_series": "TV动画", "movie": "电影/剧场版", "ova": "OVA", "other": "其他"}
	if names[t] == "" {
		return "other", "其他"
	}
	return names[t], desc[t]
}
func (s *Server) playerSearchAnime(w http.ResponseWriter, r *http.Request) {
	kw := r.URL.Query().Get("keyword")
	if kw == "" {
		kw = r.URL.Query().Get("anime")
	}
	if strings.TrimSpace(kw) == "" {
		playerError(w, 400, "keyword or anime required")
		return
	}
	if strings.HasPrefix(kw, "@") {
		s.playerCommand(w, r, kw)
		return
	}
	rows, plan, e := s.searchLibraryRecognized(r.Context(), kw)
	if e != nil {
		playerError(w, 500, "Search failed")
		return
	}
	if len(rows) == 0 && s.fallbackAnime(w, r, kw) {
		return
	}
	out := []any{}
	for _, a := range rows {
		typ, desc := playerType(a)
		sources, _ := s.Store.List(r.Context(), "anime_sources", store.Row{"anime_id": a["id"]}, 1000, 0)
		fav := false
		for _, v := range sources {
			fav = fav || boolean(v["is_favorited"])
		}
		var year, start any
		if a["year"] != nil {
			year = a["year"]
			start = fmt.Sprintf("%04d-01-01", number(a["year"]))
		}
		out = append(out, map[string]any{"animeId": a["id"], "bangumiId": "A" + str(a["id"]), "animeTitle": a["title"], "type": typ, "typeDescription": desc, "imageUrl": a["image_url"], "startDate": start, "year": year, "episodeCount": number(a["episode_count"]), "rating": 0, "isFavorited": fav, "recognitionTitle": nil})
	}
	writeJSON(w, 200, playerOK(map[string]any{"animes": out, "warnings": plan.Warnings}))
}
func (s *Server) playerSearchEpisodes(w http.ResponseWriter, r *http.Request) {
	kw := r.URL.Query().Get("anime")
	if kw == "" {
		playerError(w, 400, "anime is required")
		return
	}
	rows, plan, e := s.searchLibraryRecognized(r.Context(), kw)
	if e != nil {
		playerError(w, 500, "Search failed")
		return
	}
	out := []any{}
	for _, a := range rows {
		typ, desc := playerType(a)
		sources, e := s.Store.List(r.Context(), "anime_sources", store.Row{"anime_id": a["id"]}, 1000, 0)
		if e != nil {
			playerError(w, 500, "Source query failed")
			return
		}
		for _, src := range sources {
			eps, e := s.Store.List(r.Context(), "episode", store.Row{"source_id": src["id"]}, 10000, 0)
			if e != nil {
				playerError(w, 500, "Episode query failed")
				return
			}
			sort.Slice(eps, func(i, j int) bool { return number(eps[i]["episode_index"]) < number(eps[j]["episode_index"]) })
			episodes := []any{}
			for _, ep := range eps {
				if plan.Episode != nil && number(ep["episode_index"]) != int64(*plan.Episode) {
					continue
				}
				episodes = append(episodes, map[string]any{"episodeId": ep["id"], "episodeTitle": ep["title"], "isLibrary": true, "episodeIndex": nil})
			}
			out = append(out, map[string]any{"animeId": a["id"], "animeTitle": a["title"], "imageUrl": str(a["image_url"]), "searchKeyword": kw, "type": typ, "typeDescription": desc, "isOnAir": false, "airDay": 0, "isFavorited": boolean(src["is_favorited"]), "rating": 0, "episodes": episodes, "isParallelResult": false, "parallelProvider": "", "parallelYear": nil})
		}
	}
	writeJSON(w, 200, playerOK(map[string]any{"hasMore": false, "animes": out, "warnings": plan.Warnings}))
}
func (s *Server) playerBangumi(w http.ResponseWriter, r *http.Request) {
	raw := r.PathValue("bangumiId")
	if raw == "999999999" {
		writeJSON(w, 200, map[string]any{"success": false, "errorCode": 0, "errorMessage": "搜索正在进行，请耐心等待", "bangumi": nil})
		return
	}
	var a store.Row
	var e error
	if strings.HasPrefix(raw, "A") {
		id, err := strconv.ParseInt(raw[1:], 10, 64)
		if err != nil {
			playerError(w, 400, "Invalid anime ID")
			return
		}
		if id >= 900000 && id < 1000000 {
			local, err := s.Store.Get(r.Context(), "anime", id)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				playerError(w, 500, "Library query failed")
				return
			}
			if local != nil {
				unambiguous, err := s.playerVirtualMatchesStored(r.Context(), r.PathValue("token"), id)
				if err != nil {
					playerError(w, 500, "Virtual identity could not be verified")
					return
				}
				if !unambiguous {
					playerError(w, 422, "Identifier matches different stored and virtual results; request a fresh search result")
					return
				}
			} else {
				if mapped, err := s.resolveVirtual(r.Context(), r.PathValue("token"), id); err == nil {
					id = mapped
				} else {
					playerError(w, 404, err.Error())
					return
				}
			}
		}
		a, e = s.Store.Get(r.Context(), "anime", id)
	} else {
		metadata, err := s.Store.List(r.Context(), "anime_metadata", store.Row{"bangumi_id": raw}, 1, 0)
		if err != nil {
			e = err
		} else if len(metadata) > 0 {
			a, e = s.Store.Get(r.Context(), "anime", metadata[0]["anime_id"])
		}
	}
	if e != nil || a == nil {
		playerError(w, 404, "Anime not found")
		return
	}
	sources, e := s.Store.List(r.Context(), "anime_sources", store.Row{"anime_id": a["id"]}, 1000, 0)
	if e != nil {
		playerError(w, 500, "Source query failed")
		return
	}
	sort.Slice(sources, func(i, j int) bool { return number(sources[i]["source_order"]) < number(sources[j]["source_order"]) })
	eps := []any{}
	for _, src := range sources {
		rows, e := s.Store.List(r.Context(), "episode", store.Row{"source_id": src["id"]}, 10000, 0)
		if e != nil {
			playerError(w, 500, "Episode query failed")
			return
		}
		sort.Slice(rows, func(i, j int) bool { return number(rows[i]["episode_index"]) < number(rows[j]["episode_index"]) })
		for _, ep := range rows {
			eps = append(eps, map[string]any{"seasonId": nil, "episodeId": ep["id"], "episodeTitle": ep["title"], "episodeNumber": str(ep["episode_index"]), "lastWatched": nil, "airDate": nil})
		}
	}
	typ, desc := playerType(a)
	writeJSON(w, 200, playerOK(map[string]any{"bangumi": map[string]any{"animeId": a["id"], "bangumiId": raw, "animeTitle": a["title"], "imageUrl": a["image_url"], "searchKeyword": a["title"], "isOnAir": false, "airDay": 0, "isRestricted": false, "rating": 0, "type": typ, "typeDescription": desc, "titles": []any{}, "seasons": []any{}, "episodes": eps, "summary": "", "metadata": []any{}, "year": a["year"], "userRating": 0, "favoriteStatus": nil, "comment": nil, "ratingDetails": map[string]any{}, "relateds": []any{}, "similars": []any{}, "tags": []any{}, "onlineDatabases": []any{}, "trailers": []any{}}}))
}

type matchRequest struct {
	FileName      string  `json:"fileName"`
	FileHash      *string `json:"fileHash"`
	FileSize      *int64  `json:"fileSize"`
	VideoDuration *int    `json:"videoDuration"`
	MatchMode     *string `json:"matchMode"`
}

func parseFilename(name string) (string, int, int) {
	p := recognition.ParseFilename(filepath.Base(strings.ReplaceAll(name, "\\", "/")))
	if p == nil {
		return "", 1, 1
	}
	season, episode := 1, 1
	if p.Season != nil {
		season = *p.Season
	}
	if p.Episode != nil {
		episode = *p.Episode
	}
	return p.Title, season, episode
}
func (s *Server) match(ctx context.Context, req matchRequest) (map[string]any, error) {
	return s.matchRecognized(ctx, req)
}
func (s *Server) playerMatch(w http.ResponseWriter, r *http.Request) {
	var req matchRequest
	if e := readJSON(r, &req); e != nil {
		playerError(w, 400, e.Error())
		return
	}
	out, e := s.match(context.WithValue(r.Context(), recognitionPlayerToken{}, r.PathValue("token")), req)
	if e != nil {
		playerError(w, 400, e.Error())
		return
	}
	writeJSON(w, 200, out)
}
func (s *Server) playerMatchBatch(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Requests []matchRequest `json:"requests"`
	}
	if e := readJSON(r, &req); e != nil {
		playerError(w, 400, e.Error())
		return
	}
	if len(req.Requests) > 32 {
		playerError(w, 400, "A maximum of 32 matches is allowed")
		return
	}
	out := []any{}
	for _, item := range req.Requests {
		x, e := s.match(context.WithValue(r.Context(), recognitionPlayerToken{}, r.PathValue("token")), item)
		if e != nil {
			x = map[string]any{"success": false, "errorCode": 1001, "errorMessage": e.Error(), "isMatched": false, "matches": []any{}}
		}
		out = append(out, x)
	}
	writeJSON(w, 200, out)
}
func (s *Server) resolveDanmaku(path string) (*os.File, error) {
	path = s.normalizeStoredPath(path)
	if err := s.guardWebMigrationPath(path); err != nil {
		return nil, err
	}
	roots := append([]string{s.DataDir}, s.Config.ReadRoots...)
	roots = append(roots, s.Config.WriteRoots...)
	for _, root := range roots {
		absolute, e := filepath.Abs(root)
		if e != nil {
			continue
		}
		r, e := filepath.Rel(absolute, path)
		if e == nil && filepath.IsLocal(r) {
			return openWithin(absolute, r)
		}
	}
	return nil, errors.New("danmaku path is outside configured read roots")
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if e := r.ctx.Err(); e != nil {
		return 0, e
	}
	return r.reader.Read(p)
}
func (s *Server) readComments(ctx context.Context, ep store.Row) ([]danmaku.Comment, error) {
	s.fileMu.RLock()
	defer s.fileMu.RUnlock()
	if s.fileFault.Load() {
		return nil, errors.New("storage recovery required")
	}
	if e := ctx.Err(); e != nil {
		return nil, e
	}
	path := str(ep["danmaku_file_path"])
	if path == "" {
		return nil, os.ErrNotExist
	}
	opened, missing, e := s.openCommentPool(path)
	if e != nil {
		return nil, e
	}
	if missing {
		return nil, os.ErrNotExist
	}
	defer opened.close()
	return s.parseCommentFile(ctx, path, opened.info, opened.file)
}

func (s *Server) fetchComments(ctx context.Context, ep store.Row, progress func(int, string)) (any, error) {
	return s.fetchEpisodeShared(ctx, ep, progress)
}

func (s *Server) playerComments(w http.ResponseWriter, r *http.Request) {
	id, e := idParam(r, "episodeId")
	if e != nil {
		playerError(w, 400, e.Error())
		return
	}
	ep, e := s.Store.Get(r.Context(), "episode", id)
	if e != nil || ep == nil {
		playerError(w, 404, "Episode not found")
		return
	}
	comments, e := s.readComments(r.Context(), ep)
	if errors.Is(e, os.ErrNotExist) {
		s.playerMu.Lock()
		taskID := s.playerFetch[id]
		if taskID != "" {
			t, ok := s.Jobs.Get(taskID)
			if !ok || t.Status == "失败" || t.Status == "已完成" {
				taskID = ""
			}
		}
		if taskID == "" {
			taskID, e = s.Jobs.SubmitRegistered("fetch_comments", store.Row{"episodeId": id, "missingOnly": true, "playback": true})
			if e == nil {
				s.playerFetch[id] = taskID
			}
		}
		s.playerMu.Unlock()
		if e != nil {
			playerError(w, 500, e.Error())
			return
		}
		if r.URL.Query().Get("async") == "1" {
			writeJSON(w, 200, map[string]any{"count": 0, "comments": []any{}, "status": "pending", "taskId": taskID})
			return
		}
		timer := time.NewTimer(15 * time.Second)
		defer timer.Stop()
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
	wait:
		for {
			select {
			case <-r.Context().Done():
				return
			case <-timer.C:
				break wait
			case <-ticker.C:
				t, ok := s.Jobs.Get(taskID)
				if !ok || t.Status == "失败" {
					e = errors.New("comment retrieval failed; inspect the task status before retrying")
					break wait
				}
				if t.Status == "已完成" {
					ep, _ = s.Store.Get(r.Context(), "episode", id)
					comments, e = s.readComments(r.Context(), ep)
					break wait
				}
			}
		}
	}
	if e != nil {
		if errors.Is(e, os.ErrNotExist) {
			comments = []danmaku.Comment{}
		} else {
			playerError(w, 500, "Cannot read comments: "+e.Error())
			return
		}
	}
	if len(comments) > 0 {
		s.queuePredownload(ep)
	}
	comments, e = s.mergedComments(r.Context(), ep, comments)
	if e != nil {
		playerError(w, 500, e.Error())
		return
	}
	out, e := s.playerOutput(r.Context(), comments, r.URL.Query().Get("chConvert"))
	if e != nil {
		playerError(w, 400, e.Error())
		return
	}
	_ = s.cachePut(r.Context(), "player_history_"+tokenHash(r.PathValue("token")), "default", store.Row{"episodeId": id}, 24*time.Hour)
	writeJSON(w, 200, map[string]any{"count": len(out), "comments": out})
}
func (s *Server) playerExternalComments(w http.ResponseWriter, r *http.Request) {
	name, media, episode, e := provider.ResolveURL(r.URL.Query().Get("url"))
	if e != nil {
		playerError(w, 400, e.Error())
		return
	}
	p, ok := s.Providers.Get(name)
	if !ok {
		playerError(w, 404, "Provider unavailable")
		return
	}
	if episode == "" {
		eps, e := p.Episodes(r.Context(), media)
		if e != nil {
			playerError(w, 500, e.Error())
			return
		}
		if len(eps) != 1 {
			playerError(w, 400, "URL must identify exactly one episode")
			return
		}
		episode = eps[0].ID
	}
	comments, e := s.providerComments(r.Context(), name, episode)
	if e != nil {
		playerError(w, 500, e.Error())
		return
	}
	out, e := s.playerOutput(r.Context(), comments, r.URL.Query().Get("chConvert"))
	if e != nil {
		playerError(w, 400, e.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"count": len(out), "comments": out, "status": nil, "taskId": nil, "episodeId": nil, "progress": nil, "description": nil})
}
func (s *Server) playerTask(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("taskId")
	t, ok := s.Jobs.Get(id)
	if !ok {
		writeJSON(w, 200, map[string]any{"status": "failed", "taskId": id, "description": "任务不存在或已过期"})
		return
	}
	status := map[string]string{"排队中": "pending", "运行中": "pending", "已暂停": "pending", "已完成": "completed", "失败": "failed"}[t.Status]
	if status == "" {
		status = "failed"
	}
	out := map[string]any{"status": status, "taskId": id, "progress": t.Progress, "description": t.Description}
	var params struct {
		EpisodeID int64 `json:"episodeId"`
	}
	if unmarshalExactJSON(t.Params, &params) == nil && params.EpisodeID != 0 {
		out["episodeId"] = params.EpisodeID
	}
	s.playerMu.Lock()
	for eid, tid := range s.playerFetch {
		if tid == id {
			out["episodeId"] = eid
			break
		}
	}
	s.playerMu.Unlock()
	writeJSON(w, 200, out)
}
func (s *Server) playerCommand(w http.ResponseWriter, r *http.Request, kw string) {
	cmd := strings.ToUpper(strings.TrimSpace(kw))
	message := "Unknown command. @HELP: help, @CXRW: task status, @CXLK: source status, @QLHC: clear cache, @SXDM: refresh last episode"
	key := "player_command_" + tokenHash(r.PathValue("token")) + "_" + cmd
	s.playerMu.Lock()
	var recent bool
	if s.cacheGet(r.Context(), key, &recent) == nil {
		message = "指令冷却中，请稍后重试"
		s.playerMu.Unlock()
		s.writeCommand(w, message)
		return
	}
	e := s.cachePut(r.Context(), key, "default", true, 5*time.Second)
	s.playerMu.Unlock()
	if e != nil {
		playerError(w, 500, e.Error())
		return
	}
	switch cmd {
	case "@", "@HELP":
		message = "@HELP 帮助 | @QLHC 清理缓存 | @CXLK 源限流状态 | @CXRW 后台任务 | @SXDM 刷新最近播放集"
	case "@CXRW":
		counts := map[string]int{}
		for _, task := range s.Jobs.List() {
			counts[task.Status]++
		}
		message = fmt.Sprintf("任务: 排队%d 运行%d 暂停%d 完成%d 失败%d", counts["排队中"], counts["运行中"], counts["已暂停"], counts["已完成"], counts["失败"])
	case "@CXLK":
		message = s.playerRateCommand(r.Context())
	case "@QLHC":
		n, e := s.clearDisposableCaches(r.Context(), "")
		if e != nil {
			playerError(w, 500, e.Error())
			return
		}
		message = fmt.Sprintf("已清理 %d 个缓存项，已保留接收去重与播放器状态", n)
	case "@SXDM":
		var history struct {
			EpisodeID int64 `json:"episodeId"`
		}
		if e := s.cacheGet(r.Context(), "player_history_"+tokenHash(r.PathValue("token")), &history); e != nil {
			message = "没有最近播放记录"
			break
		}
		id, e := s.Jobs.SubmitRegistered("fetch_comments", store.Row{"episodeId": history.EpisodeID})
		if e != nil {
			playerError(w, 500, e.Error())
			return
		}
		message = "刷新任务已提交: " + id
	}
	s.writeCommand(w, message)
}
func (s *Server) writeCommand(w http.ResponseWriter, message string) {
	writeJSON(w, 200, playerOK(map[string]any{"animes": []any{map[string]any{"animeId": nil, "bangumiId": "", "animeTitle": message, "type": "other", "typeDescription": "命令", "imageUrl": nil, "startDate": nil, "year": nil, "episodeCount": 0, "rating": 0, "isFavorited": false, "recognitionTitle": nil}}}))
}
