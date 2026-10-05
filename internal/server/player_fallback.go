// SPDX-License-Identifier: AGPL-3.0-only
package server

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/AniBakaBaka/AniDan/internal/job"
	"github.com/AniBakaBaka/AniDan/internal/provider"
	"github.com/AniBakaBaka/AniDan/internal/recognition"
	"github.com/AniBakaBaka/AniDan/internal/store"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type playerSearchParams struct{ Key, TokenHash, Keyword string }
type playerSearchCache struct {
	Status     string                  `json:"status"`
	VirtualIDs []int64                 `json:"virtualIds,omitempty"`
	Results    []provider.SearchResult `json:"results"`
	Error      string                  `json:"error,omitempty"`
	Warnings   []string                `json:"warnings,omitempty"`
}

func tokenHash(t string) string { v := sha256.Sum256([]byte(t)); return hex.EncodeToString(v[:12]) }
func (s *Server) fallbackAnime(w http.ResponseWriter, r *http.Request, keyword string) bool {
	if !strings.EqualFold(s.setting(r.Context(), "searchFallbackEnabled", "false"), "true") {
		return false
	}
	allowed, err := s.recognitionFallbackAllowed(r.Context(), "search", r.PathValue("token"), keyword)
	if err != nil {
		playerError(w, 400, err.Error())
		return true
	}
	if !allowed {
		return false
	}
	hash := tokenHash(r.PathValue("token"))
	kh := sha256.Sum256([]byte(keyword))
	key := "player_search_" + hash + "_" + hex.EncodeToString(kh[:12])
	var cached playerSearchCache
	if e := s.cacheGet(r.Context(), key, &cached); e != nil {
		s.playerMu.Lock()
		e = s.cacheGet(r.Context(), key, &cached)
		if e != nil {
			cached = playerSearchCache{Status: "pending", Results: []provider.SearchResult{}}
			e = s.cachePut(r.Context(), key, "search", cached, time.Hour)
			if e == nil {
				_, e = s.Jobs.SubmitRegistered("player_search", playerSearchParams{key, hash, keyword})
			}
		}
		s.playerMu.Unlock()
		if e != nil {
			saveCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			_ = s.cachePut(saveCtx, key, "search", playerSearchCache{Status: "failed", Results: []provider.SearchResult{}, Error: e.Error()}, time.Hour)
			cancel()
			playerError(w, 500, "Unable to submit provider search")
			return true
		}
	}
	if cached.Status == "pending" {
		waitCtx, cancel := s.fallbackWaitContext(r.Context(), "search")
		err := waitFallback(waitCtx, func(ctx context.Context) (bool, error) {
			if e := s.cacheGet(ctx, key, &cached); e != nil {
				return false, e
			}
			return cached.Status != "pending", nil
		})
		cancel()
		if err != nil {
			writeJSON(w, 200, playerOK(map[string]any{"animes": []any{}}))
			return true
		}
	}
	if cached.Status == "failed" {
		playerError(w, 500, cached.Error)
		return true
	}
	out := []any{}
	occupied, err := s.playerOccupiedVirtualRange(r.Context())
	if err != nil {
		playerError(w, 500, "Library identity query failed")
		return true
	}
	for i, v := range cached.Results {
		vid := int64(0)
		if i < len(cached.VirtualIDs) {
			vid = cached.VirtualIDs[i]
		}
		if vid < 900000 || vid >= 1000000 || occupied[vid] {
			vid, err = s.allocatePlayerVirtual(r.Context(), hash, v)
			if err != nil {
				playerError(w, 500, "Virtual identity allocation failed")
				return true
			}
		}
		m := providerResult(v)
		typ, desc := playerType(store.Row{"type": v.Type})
		var year any
		if v.Year > 0 {
			year = v.Year
		}
		out = append(out, map[string]any{"animeId": vid, "bangumiId": "A" + strconv.FormatInt(vid, 10), "animeTitle": v.Title, "type": typ, "typeDescription": desc, "imageUrl": m["imageUrl"], "startDate": nil, "year": year, "episodeCount": 0, "rating": 0, "isFavorited": false, "recognitionTitle": nil})
	}
	writeJSON(w, 200, playerOK(map[string]any{"animes": out, "warnings": cached.Warnings}))
	return true
}
func (s *Server) runPlayerSearch(ctx context.Context, raw json.RawMessage, progress func(int, string)) (result any, outcome error) {
	var p playerSearchParams
	if e := unmarshalExactJSON(raw, &p); e != nil {
		return nil, e
	}
	var results []provider.SearchResult
	defer func() {
		recovered := recover()
		if recovered != nil {
			outcome = errors.New("Provider search interrupted by an internal error")
		}
		if outcome != nil {
			saveCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = s.cachePut(saveCtx, p.Key, "search", playerSearchCache{Status: "failed", Results: []provider.SearchResult{}, Error: outcome.Error()}, time.Hour)
		}
		if recovered != nil {
			panic(recovered)
		}
	}()
	progress(5, "Searching enabled providers")
	plan, e := s.prepareSearch(ctx, p.Keyword)
	if e == nil {
		results, e = s.searchRecognition(ctx, plan)
	}
	cached := playerSearchCache{Status: "completed", Results: results}
	if plan != nil {
		cached.Warnings = plan.Warnings
	}
	if e != nil {
		cached.Warnings = append(cached.Warnings, e.Error())
	}
	if e != nil && len(results) == 0 {
		cached.Status = "failed"
		cached.Error = e.Error()
	}
	if len(results) > 100000 {
		results = results[:100000]
		cached.Results = results
	}
	for _, v := range results {
		if e := job.Checkpoint(ctx); e != nil {
			return nil, e
		}
		virtual, err := s.allocatePlayerVirtual(ctx, p.TokenHash, v)
		if err != nil {
			return nil, err
		}
		cached.VirtualIDs = append(cached.VirtualIDs, virtual)
	}
	if err := s.cachePut(ctx, p.Key, "search", cached, time.Hour); err != nil {
		return nil, err
	}
	if cached.Status == "failed" {
		return nil, e
	}
	progress(100, "Search complete")
	return map[string]any{"count": len(results), "notification": notificationFallbackPayload(p.Keyword, results, nil)}, nil
}

// Selecting a virtual result reserves real rows before exposing episode IDs. This
// fixes race-prone max(id)+1 predictions; IDs never depend on future allocation.
func (s *Server) resolveVirtual(ctx context.Context, token string, virtual int64) (int64, error) {
	var result provider.SearchResult
	if e := s.cacheGet(ctx, fmt.Sprintf("player_virtual_%s_%d", tokenHash(token), virtual), &result); e != nil {
		return 0, e
	}
	return s.reserveRecognitionResult(ctx, result)
}
func (s *Server) reserveRecognitionResult(ctx context.Context, result provider.SearchResult) (int64, error) {
	originalTitle := result.Title
	prepared, rules, _, e := s.prepareStorage(ctx, ImportRequest{Provider: result.Provider, MediaID: result.ID, Title: result.Title, Season: result.Season, Type: result.Type, ImageURL: result.ImageURL})
	if e != nil {
		return 0, e
	}
	eps, _, e := s.recognitionEpisodes(ctx, result.Provider, result.ID, originalTitle, []string{prepared.Title})
	if e != nil {
		return 0, e
	}
	if len(eps) == 0 {
		return 0, fmt.Errorf("provider returned no episodes after filtering")
	}
	for i := range eps {
		post := rules.Postprocess(recognition.Input{Text: originalTitle, Season: recognition.Int(result.Season), Episode: recognition.Int(eps[i].Index), Provider: result.Provider})
		if post.Episode != nil {
			eps[i].Index = *post.Episode
		}
	}
	seen := map[int]bool{}
	for _, ep := range eps {
		if ep.Index < 0 || seen[ep.Index] {
			return 0, fmt.Errorf("duplicate or invalid storage episode index %d", ep.Index)
		}
		seen[ep.Index] = true
	}
	result.Title = prepared.Title
	result.Type = prepared.Type
	s.importMu.Lock()
	defer s.importMu.Unlock()
	season := prepared.Season
	typ := result.Type
	if typ == "" {
		typ = "tv_series"
	}
	animes, e := s.Store.List(ctx, "anime", store.Row{"title": result.Title, "season": season, "type": typ}, 1, 0)
	if e != nil {
		return 0, e
	}
	var animeID int64
	if len(animes) > 0 {
		animeID = number(animes[0]["id"])
	} else {
		row := store.Row{"title": result.Title, "season": season, "type": typ, "image_url": result.ImageURL, "created_at": s.now(), "episode_count": len(eps)}
		if result.Year > 0 {
			row["year"] = result.Year
		}
		animeID, e = s.Store.Insert(ctx, "anime", row)
		if e != nil {
			return 0, e
		}
	}
	sources, e := s.Store.List(ctx, "anime_sources", store.Row{"anime_id": animeID}, 1000, 0)
	if e != nil {
		return 0, e
	}
	var sourceID int64
	order := int64(1)
	for _, src := range sources {
		if str(src["provider_name"]) == result.Provider && str(src["media_id"]) == result.ID {
			sourceID = number(src["id"])
			order = number(src["source_order"])
			break
		}
		if number(src["source_order"]) >= order {
			order = number(src["source_order"]) + 1
		}
	}
	if sourceID == 0 {
		sourceID, e = s.Store.Insert(ctx, "anime_sources", store.Row{"anime_id": animeID, "source_order": order, "provider_name": result.Provider, "media_id": result.ID, "created_at": s.now()})
		if e != nil {
			return 0, e
		}
	}
	source, e := s.Store.Get(ctx, "anime_sources", sourceID)
	if e != nil {
		return 0, e
	}
	if number(source["anime_id"]) != animeID || str(source["provider_name"]) != result.Provider || str(source["media_id"]) != result.ID {
		return 0, errors.New("virtual selection source changed during admission")
	}
	initial, e := s.captureWorkflowEpisodes(ctx, source)
	if e != nil {
		return 0, e
	}
	for _, ep := range eps {
		if old := initial[int64(ep.Index)]; old != nil && str(old["provider_episode_id"]) != ep.ID {
			return 0, errWorkflowEpisodeConflict
		}
	}
	for _, ep := range eps {
		if _, e = s.prepareWorkflowEpisodeExpected(ctx, source, initial[int64(ep.Index)], int64(ep.Index), ep.Title, ep.URL, ep.ID, true); e != nil {
			return 0, e
		}
	}
	if e = s.libTransaction(ctx, func(tx *sql.Tx) error {
		if e := s.validateSourceSnapshot(ctx, tx, source); e != nil {
			return e
		}
		return s.storeRecognitionMetadataTx(ctx, tx, animeID, prepared)
	}); e != nil {
		return 0, e
	}
	return animeID, nil
}

// A token may keep multiple search pages open. Never replace one live virtual
// ID with a different provider/media identity when another search completes.
func (s *Server) allocatePlayerVirtual(ctx context.Context, hash string, result provider.SearchResult) (int64, error) {
	if result.Provider == "" || result.ID == "" {
		return 0, errors.New("provider result lacks identity")
	}
	sum := sha256.Sum256([]byte(result.Provider + "\x00" + result.ID))
	start := int64(binary.BigEndian.Uint32(sum[:4]) % 100000)
	s.playerMu.Lock()
	defer s.playerMu.Unlock()
	occupied, e := s.playerOccupiedVirtualRange(ctx)
	if e != nil {
		return 0, e
	}
	for offset := int64(0); offset < 100000; offset++ {
		id := 900000 + (start+offset)%100000
		if occupied[id] {
			continue
		}
		key := fmt.Sprintf("player_virtual_%s_%d", hash, id)
		var previous provider.SearchResult
		e := s.cacheGet(ctx, key, &previous)
		if e == nil {
			if previous.Provider == result.Provider && previous.ID == result.ID {
				if e = s.cachePut(ctx, key, "search", result, 65*time.Minute); e != nil {
					return 0, e
				}
				return id, nil
			}
			continue
		}
		if !errors.Is(e, sql.ErrNoRows) && e.Error() != "cache missing or expired" {
			return 0, e
		}
		if e = s.cachePut(ctx, key, "search", result, 65*time.Minute); e != nil {
			return 0, e
		}
		return id, nil
	}
	return 0, errors.New("token virtual-result capacity exhausted; clear expired search caches")
}
