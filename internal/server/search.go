// SPDX-License-Identifier: AGPL-3.0-only
package server

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/AniBakaBaka/AniDan/internal/job"
	"github.com/AniBakaBaka/AniDan/internal/provider"
	"github.com/AniBakaBaka/AniDan/internal/recognition"
	"github.com/AniBakaBaka/AniDan/internal/store"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

func (s *Server) registerSearch(m *http.ServeMux) {
	m.HandleFunc("GET /api/ui/search/anime", s.uiLocalSearch)
	m.HandleFunc("GET /api/ui/search/provider", s.operator(s.searchProviders))
	m.HandleFunc("GET /api/ui/search/episodes", s.operator(s.searchEpisodes))
	m.HandleFunc("GET /api/control/search", s.operator(s.searchProviders))
	m.HandleFunc("GET /api/control/episodes", s.operator(s.searchEpisodes))
	m.HandleFunc("POST /api/ui/import", s.operator(s.importProvider))
	m.HandleFunc("POST /api/ui/import/edited", s.operator(s.importProvider))
	m.HandleFunc("POST /api/control/import/direct", s.operator(s.importProvider))
	m.HandleFunc("POST /api/control/import/edited", s.operator(s.importProvider))
}
func (s *Server) uiLocalSearch(w http.ResponseWriter, r *http.Request) {
	kw := r.URL.Query().Get("keyword")
	if strings.TrimSpace(kw) == "" {
		httpError(w, 422, "keyword is required")
		return
	}
	rows, e := s.localSearch(r.Context(), kw)
	if e != nil {
		httpError(w, 500, e.Error())
		return
	}
	out := []any{}
	for _, a := range rows {
		out = append(out, map[string]any{"animeId": a["id"], "animeTitle": a["title"], "type": a["type"], "rating": 0, "imageUrl": nil})
	}
	writeJSON(w, 200, map[string]any{"hasMore": false, "animes": out})
}
func (s *Server) rawProviderSearch(ctx context.Context, kw string) ([]provider.SearchResult, error) {
	rows, e := s.Store.List(ctx, "scrapers", nil, 1000, 0)
	if e != nil {
		return nil, e
	}
	sort.Slice(rows, func(i, j int) bool { return number(rows[i]["display_order"]) < number(rows[j]["display_order"]) })
	names := []string{}
	for _, v := range rows {
		if boolean(v["is_enabled"]) {
			if _, ok := s.Providers.Get(str(v["provider_name"])); ok {
				names = append(names, str(v["provider_name"]))
			}
		}
	}
	if len(names) == 0 {
		return []provider.SearchResult{}, nil
	}
	return s.cachedProviderSearch(ctx, kw, names...)
}
func providerResult(v provider.SearchResult) map[string]any {
	var year, image any
	if v.Year != 0 {
		year = v.Year
	}
	if v.ImageURL != "" {
		image = v.ImageURL
	}
	season := v.Season
	typ := v.Type
	if typ == "" {
		typ = "tv_series"
	}
	return map[string]any{"provider": v.Provider, "mediaId": v.ID, "title": v.Title, "type": typ, "season": season, "year": year, "imageUrl": image, "episodeCount": nil, "currentEpisodeIndex": nil, "url": nil, "supportsEpisodeUrls": nil, "supplementSource": nil, "recognitionTitle": nil, "sourceType": nil, "typeSuggestion": nil, "typeDecision": nil, "typeDecisionReason": nil}
}
func (s *Server) cachePut(ctx context.Context, key, region string, value any, ttl time.Duration) error {
	b, e := json.Marshal(value)
	if e != nil {
		return e
	}
	loc, _ := time.LoadLocation(s.Config.Timezone)
	row := store.Row{"cache_value": string(b), "cache_provider": region, "expires_at": time.Now().In(loc).Add(ttl).Format("2006-01-02T15:04:05")}
	old, e := s.Store.Get(ctx, "cache_data", key)
	if e == nil && old != nil {
		return s.Store.Update(ctx, "cache_data", key, row)
	}
	row["cache_key"] = key
	_, e = s.Store.Insert(ctx, "cache_data", row)
	return e
}
func (s *Server) cacheGet(ctx context.Context, key string, v any) error {
	row, e := s.Store.Get(ctx, "cache_data", key)
	if e != nil {
		return e
	}
	if row == nil || normalizeDate(str(row["expires_at"])) < normalizeDate(s.now()) {
		return errors.New("cache missing or expired")
	}
	d := json.NewDecoder(strings.NewReader(str(row["cache_value"])))
	d.UseNumber()
	return d.Decode(v)
}
func (s *Server) searchProviders(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	kw := q.Get("keyword")
	if kw == "" {
		kw = q.Get("searchTerm")
	}
	if strings.TrimSpace(kw) == "" {
		httpError(w, 422, "keyword required")
		return
	}
	plan, e := s.prepareSearch(r.Context(), kw)
	if e != nil {
		httpError(w, 422, e.Error())
		return
	}
	results, e := s.searchRecognition(r.Context(), plan)
	if e != nil && len(results) == 0 {
		httpError(w, 503, e.Error())
		return
	}
	warnings := append([]string{}, plan.Warnings...)
	if e != nil {
		warnings = append(warnings, e.Error())
	}
	out := []map[string]any{}
	yearset := map[int]bool{}
	providerset := map[string]bool{}
	typeset := map[string]bool{}
	for _, v := range results {
		if v.Year > 0 {
			yearset[v.Year] = true
		}
		providerset[v.Provider] = true
		typeset[v.Type] = true
		row := plan.result(v)
		if x := q.Get("typeFilter"); x != "" && x != v.Type {
			continue
		}
		if x := q.Get("yearFilter"); x != "" && x != strconv.Itoa(v.Year) {
			continue
		}
		if x := q.Get("providerFilter"); x != "" && x != v.Provider {
			continue
		}
		if x := strings.ToLower(q.Get("titleFilter")); x != "" && !strings.Contains(strings.ToLower(v.Title), x) {
			continue
		}
		out = append(out, row)
	}
	if strings.HasPrefix(r.URL.Path, "/api/control/") {
		id := randomID()
		if e = s.cachePut(r.Context(), "control_search_"+id, "search", out, time.Hour); e != nil {
			httpError(w, 500, e.Error())
			return
		}
		for i := range out {
			out[i]["result_index"] = i
		}
		writeJSON(w, 200, map[string]any{"searchId": id, "results": out, "warnings": warnings})
		return
	}
	years := []int{}
	providers := []string{}
	types := []string{}
	for v := range yearset {
		years = append(years, v)
	}
	for v := range providerset {
		providers = append(providers, v)
	}
	for v := range typeset {
		types = append(types, v)
	}
	sort.Sort(sort.Reverse(sort.IntSlice(years)))
	sort.Strings(providers)
	sort.Strings(types)
	page, size := pageParams(r)
	if q.Get("pageSize") == "" {
		size = 10
	}
	if size < 10 {
		size = 10
	}
	if size > 100 {
		size = 100
	}
	total := len(out)
	start := (page - 1) * size
	if start > total {
		start = total
	}
	end := start + size
	if end > total {
		end = total
	}
	writeJSON(w, 200, map[string]any{"results": out[start:end], "search_season": plan.Season, "search_episode": plan.Episode, "supplemental_results": []any{}, "total": total, "page": page, "pageSize": size, "available_years": years, "available_providers": providers, "available_types": types, "warnings": warnings})
}
func (s *Server) searchEpisodes(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	name := q.Get("provider")
	title := q.Get("animeTitle")
	if title == "" {
		title = q.Get("title")
	}
	media := q.Get("media_id")
	if media == "" {
		media = q.Get("mediaId")
	}
	if id := q.Get("searchId"); id != "" {
		var results []map[string]any
		if e := s.cacheGet(r.Context(), "control_search_"+id, &results); e != nil {
			httpError(w, 404, "Search expired")
			return
		}
		index := q.Get("result_index")
		if index == "" {
			index = q.Get("resultIndex")
		}
		n, e := strconv.Atoi(index)
		if e != nil {
			httpError(w, 422, "result_index is required and must be an integer")
			return
		}
		if n < 0 || n >= len(results) {
			httpError(w, 400, "result index out of range")
			return
		}
		name = str(results[n]["provider"])
		media = str(results[n]["mediaId"])
		title = str(results[n]["title"])
	}
	_, ok := s.Providers.Get(name)
	if !ok {
		httpError(w, 404, "Provider unavailable")
		return
	}
	eps, excluded, e := s.recognitionEpisodesPreview(r.Context(), name, media, title, nil)
	if e != nil {
		httpError(w, 503, e.Error())
		return
	}
	out := []any{}
	for _, ep := range eps {
		out = append(out, map[string]any{"provider": name, "episodeId": ep.ID, "title": ep.Title, "episodeIndex": ep.Index, "url": ep.URL})
	}
	if strings.HasPrefix(r.URL.Path, "/api/control/") {
		writeJSON(w, 200, out)
	} else {
		writeJSON(w, 200, map[string]any{"episodes": out, "excludedEpisodes": excluded})
	}
}

type ImportEpisode struct {
	ID    string `json:"episodeId"`
	Title string `json:"title"`
	Index int    `json:"episodeIndex"`
	URL   string `json:"url"`
}
type ImportRequest struct {
	RecognitionWarnings []string           `json:"recognitionWarnings,omitempty"`
	Aliases             map[string]*string `json:"aliases,omitempty"`
	Edited              bool               `json:"edited,omitempty"`
	Provider            string             `json:"provider"`
	MediaID             string             `json:"mediaId"`
	Title               string             `json:"animeTitle"`
	Type                string             `json:"type"`
	Season              int                `json:"season"`
	Year                *int               `json:"year"`
	ImageURL            string             `json:"imageUrl"`
	CurrentEpisodeIndex *int               `json:"currentEpisodeIndex"`
	Episodes            []ImportEpisode    `json:"episodes,omitempty"`
	Metadata            map[string]string  `json:"metadata,omitempty"`
}

func (s *Server) importProvider(w http.ResponseWriter, r *http.Request) {
	var in map[string]any
	if e := readJSON(r, &in); e != nil {
		httpError(w, 400, e.Error())
		return
	}
	if id := str(in["searchId"]); id != "" {
		var results []map[string]any
		if e := s.cacheGet(r.Context(), "control_search_"+id, &results); e != nil {
			httpError(w, 404, "Search expired")
			return
		}
		rawIndex := in["result_index"]
		if rawIndex == nil {
			rawIndex = in["resultIndex"]
		}
		index, e := strconv.ParseInt(str(rawIndex), 10, 64)
		if e != nil {
			httpError(w, 422, "result_index is required and must be an integer")
			return
		}
		if index < 0 || index >= int64(len(results)) {
			httpError(w, 400, "result index out of range")
			return
		}
		for k, v := range results[index] {
			if _, ok := in[k]; !ok {
				in[k] = v
			}
		}
		if in["animeTitle"] == nil {
			in["animeTitle"] = in["title"]
		}
	}
	if in["animeTitle"] == nil {
		in["animeTitle"] = in["title"]
	}
	if in["type"] == nil {
		in["type"] = in["mediaType"]
	}
	b, _ := json.Marshal(in)
	var req ImportRequest
	if e := unmarshalExactJSON(b, &req); e != nil {
		httpError(w, 400, e.Error())
		return
	}
	if req.Provider == "" || req.MediaID == "" || strings.TrimSpace(req.Title) == "" {
		httpError(w, 422, "provider, mediaId and animeTitle required")
		return
	}
	if _, ok := s.Providers.Get(req.Provider); !ok {
		httpError(w, 404, "Provider unavailable")
		return
	}
	if req.Type == "" {
		req.Type = "tv_series"
	}
	if value, present := in["season"]; !present || value == nil {
		req.Season = 1
	}
	if req.CurrentEpisodeIndex != nil && *req.CurrentEpisodeIndex < 0 {
		httpError(w, 422, "currentEpisodeIndex must be nonnegative")
		return
	}
	if req.Season < 0 {
		httpError(w, 422, "season must be nonnegative")
		return
	}
	req.Edited = strings.HasSuffix(r.URL.Path, "/edited")
	if req.Edited && len(req.Episodes) == 0 {
		httpError(w, 422, "edited import requires episodes")
		return
	}
	if req.Metadata == nil {
		req.Metadata = map[string]string{}
	}
	for _, k := range []string{"tmdbId", "imdbId", "tvdbId", "bangumiId", "doubanId", "tmdbEpisodeGroupId"} {
		if v := str(in[k]); v != "" {
			req.Metadata[k] = v
		}
	}
	for _, ep := range req.Episodes {
		if ep.ID == "" || ep.Index < 0 {
			httpError(w, 422, "episodes require episodeId and nonnegative episodeIndex")
			return
		}
	}
	encoded, _ := json.Marshal(req)
	sum := sha256.Sum256(encoded)
	id, e := s.Jobs.SubmitWithOptions("generic_import", req, nil, job.SubmitOptions{Title: "导入: " + req.Title, UniqueKey: fmt.Sprintf("import:%x", sum)})
	if e != nil {
		httpError(w, 503, e.Error())
		return
	}
	out := map[string]any{"message": "导入任务已提交", "taskId": id}
	if strings.HasPrefix(r.URL.Path, "/api/control/") {
		out["status"] = "success"
	}
	writeJSON(w, 202, out)
}
func episodeIdentifier(animeID, sourceOrder, index int64) (int64, error) {
	if animeID < 0 || sourceOrder < 0 || index < 0 {
		return 0, errors.New("negative episode ID component")
	}
	return strconv.ParseInt(fmt.Sprintf("25%06d%02d%04d", animeID, sourceOrder, index), 10, 64)
}
func (s *Server) runImport(ctx context.Context, raw json.RawMessage, progress func(int, string)) (any, error) {
	return s.runImportRequest(ctx, raw, progress, nil)
}

// The private media plan fixes the target identity and projects source episodes
// from one filtered listing after the normal source/episode snapshot capture.
// Public/manual imports retain the nil-plan semantics.
func (s *Server) runImportRequest(ctx context.Context, raw json.RawMessage, progress func(int, string), mediaPlan *mediaImportProjection) (result any, outcome error) {
	var req ImportRequest
	if e := unmarshalExactJSON(raw, &req); e != nil {
		return nil, e
	}
	_, ok := s.Providers.Get(req.Provider)
	if !ok {
		return nil, errors.New("provider unavailable")
	}
	originalTitle := req.Title
	var prepared ImportRequest
	var rules *recognition.Rules
	var warnings []string
	var e error
	if mediaPlan == nil {
		prepared, rules, warnings, e = s.prepareStorage(ctx, req)
	} else {
		rules = mediaPlan.rules
		prepared, warnings, e = prepareStorageWithRules(req, rules, nil)
		if e == nil {
			prepared.Title, prepared.Type, prepared.Season = mediaPlan.title, mediaPlan.mediaType, mediaPlan.season
			if mediaPlan.year != nil {
				prepared.Year = mediaPlan.year
			}
		}
	}

	if e != nil {
		return nil, e
	}
	req = prepared
	warnings = append(append([]string{}, req.RecognitionWarnings...), warnings...)
	req, metadataWarnings := s.enrichRecognitionImport(ctx, req)
	warnings = append(warnings, metadataWarnings...)
	// Bind an existing source before remote episode discovery. A source move
	// during listing must not make this request adopt a new library target.
	var expectedSource store.Row
	var initialEpisodes map[int64]store.Row
	foundAnime, err := s.Store.List(ctx, "anime", store.Row{"title": req.Title, "type": req.Type, "season": req.Season}, 1, 0)
	if err != nil {
		return nil, err
	}
	if len(foundAnime) > 0 && mediaPlan != nil {
		if e := s.validateMediaLibraryTarget(ctx, s.Store.DB, foundAnime[0], req); e != nil {
			return nil, e
		}
	}
	if len(foundAnime) > 0 {
		foundSource, err := s.Store.List(ctx, "anime_sources", store.Row{"anime_id": foundAnime[0]["id"], "provider_name": req.Provider, "media_id": req.MediaID}, 1, 0)
		if err != nil {
			return nil, err
		}
		if len(foundSource) > 0 {
			// Remote SQL collations may consider differently cased identifiers
			// equal. Provider IDs are opaque strings and must match exactly.
			if str(foundSource[0]["provider_name"]) != req.Provider || str(foundSource[0]["media_id"]) != req.MediaID {
				return nil, errors.New("import source identifier is not an exact match; select the stored source identity or a separate source")
			}
			expectedSource = foundSource[0]
		}
	}
	if expectedSource != nil {
		initialEpisodes, e = s.captureWorkflowEpisodes(ctx, expectedSource)
		if e != nil {
			return nil, e
		}
	}
	if mediaPlan != nil {
		var target store.Row
		if len(foundAnime) > 0 {
			target = foundAnime[0]
		}
		s.captureMediaImportGroup(ctx, mediaPlan, req, target)
	}
	loadEpisodes := func() ([]ImportEpisode, error) {
		source, _, err := s.recognitionEpisodes(ctx, req.Provider, req.MediaID, originalTitle, []string{req.Title})
		if err != nil {
			return nil, err
		}
		out := make([]ImportEpisode, 0, len(source))
		for _, ep := range source {
			out = append(out, ImportEpisode{ID: ep.ID, Title: ep.Title, Index: ep.Index, URL: ep.URL})
		}
		return out, nil
	}
	eps := req.Episodes
	if len(eps) == 0 {
		eps, e = loadEpisodes()
		if e != nil {
			return nil, e
		}
	}
	selected := []ImportEpisode{}
	if mediaPlan != nil {
		selected, e = s.selectMediaImportEpisodes(ctx, mediaPlan, req, originalTitle, eps)
		call := mediaListingCallFromContext(ctx)
		if errors.Is(e, errMediaEpisodeUnavailable) && call != nil && call.reused && len(req.Episodes) == 0 && ctx.Err() == nil {
			// One fresh refill before admission preserves newly available episodes.
			// Ambiguous mappings/identity errors never trigger this fallback.
			call.force = true
			eps, e = loadEpisodes()
			if e == nil {
				selected, e = s.selectMediaImportEpisodes(ctx, mediaPlan, req, originalTitle, eps)
			}
		}
		if e != nil {
			return nil, e
		}
		if call != nil && call.proof != nil {
			mediaPlan.listingProof = call.proof
			ctx = context.WithValue(ctx, mediaListingProofKey{}, call.proof)
		}
		if mediaPlan.groupCandidate != nil {
			ctx = context.WithValue(ctx, mediaGroupOwnershipContextKey{}, true)
		}
		if mediaPlan.group != nil {
			ctx = context.WithValue(ctx, mediaEpisodeGroupContextKey{}, mediaPlan.group)
		}
	} else {
		for _, ep := range eps {
			if req.CurrentEpisodeIndex != nil && ep.Index != *req.CurrentEpisodeIndex {
				continue
			}
			if !req.Edited {
				post := rules.Postprocess(recognition.Input{Text: originalTitle, Season: recognition.Int(req.Season), Episode: recognition.Int(ep.Index), Provider: req.Provider})
				warnings = append(warnings, recognitionWarnings(post.Warnings)...)
				if post.Episode != nil {
					ep.Index = *post.Episode
				}
			}
			selected = append(selected, ep)
		}
	}
	eps = selected
	if len(eps) == 0 {
		return nil, errors.New("no requested episodes remain after filtering")
	}
	seenIndices := map[int]bool{}
	for _, ep := range eps {
		if ep.Index < 0 || seenIndices[ep.Index] {
			return nil, fmt.Errorf("duplicate or invalid storage episode index %d", ep.Index)
		}
		seenIndices[ep.Index] = true
	}
	if e := job.Checkpoint(ctx); e != nil {
		return nil, e
	}
	var animeID, sourceID, sourceOrder int64
	var sourceSnapshot store.Row
	imported := 0
	completedIndices, emptyIndices, unchangedIndices := []int{}, []int{}, []int{}
	failedIndex := -1
	defer func() {
		if outcome == nil {
			return
		}
		// A provider's own deadline is a failed episode, not cancellation of
		// the job. Preserve its bounded partial result for operator review.
		if ctx.Err() == nil && (errors.Is(outcome, context.Canceled) || errors.Is(outcome, context.DeadlineExceeded)) {
			outcome = errors.New("import episode request interrupted; earlier completed episodes remain committed")
		}
		done := map[int]bool{}
		for _, index := range completedIndices {
			done[index] = true
		}
		for _, index := range emptyIndices {
			done[index] = true
		}
		remaining := []int{}
		for _, ep := range eps {
			if !done[ep.Index] && ep.Index != failedIndex {
				remaining = append(remaining, ep.Index)
			}
		}
		capList := func(v []int) []int { return append([]int{}, v[:min(len(v), 256)]...) }
		var failed any
		if failedIndex >= 0 {
			failed = failedIndex
		}
		result = job.DiagnosticResult{"operation": "import", "animeId": animeID, "sourceId": sourceID, "imported": imported, "completedEpisodeIndices": capList(completedIndices), "emptyEpisodeIndices": capList(emptyIndices), "unchangedEpisodeIndices": capList(unchangedIndices), "failedEpisodeIndex": failed, "unprocessedEpisodeIndices": capList(remaining), "unprocessedCount": len(remaining), "completedCount": len(completedIndices), "emptyCount": len(emptyIndices), "diagnosticsTruncated": len(completedIndices) > 256 || len(emptyIndices) > 256 || len(unchangedIndices) > 256 || len(remaining) > 256, "partial": len(completedIndices) > 0}
	}()
	if mediaPlan != nil && mediaPlan.group != nil {
		if e = s.validateMediaEpisodeGroup(ctx, s.Store.DB, mediaPlan.group); e != nil {
			return nil, mediaGroupVerificationError(ctx, e)
		}
	}
	if e = s.validateMediaListingProof(ctx, mediaListingProofFromContext(ctx), nil); e != nil {
		return nil, e
	}
	s.importMu.Lock()
	if expectedSource != nil {
		e = s.libTransaction(ctx, func(tx *sql.Tx) error { return s.validateSourceSnapshot(ctx, tx, expectedSource) })
		if e == nil {
			sourceSnapshot = expectedSource
			animeID = number(expectedSource["anime_id"])
			sourceID = number(expectedSource["id"])
			sourceOrder = number(expectedSource["source_order"])
		}
	} else {
		animes, err := s.Store.List(ctx, "anime", store.Row{"title": req.Title, "type": req.Type, "season": req.Season}, 1, 0)
		e = err
		if e == nil {
			if len(animes) > 0 {
				animeID = number(animes[0]["id"])
				if mediaPlan != nil {
					e = s.validateMediaLibraryTarget(ctx, s.Store.DB, animes[0], req)
				}
			} else {
				row := store.Row{"title": req.Title, "type": req.Type, "season": req.Season, "image_url": req.ImageURL, "created_at": s.now(), "episode_count": len(eps)}
				if req.Year != nil {
					row["year"] = *req.Year
				}
				animeID, e = s.Store.Insert(ctx, "anime", row)
			}
		}
		if e == nil {
			sources, err := s.Store.List(ctx, "anime_sources", store.Row{"anime_id": animeID}, 1000, 0)
			e = err
			for _, src := range sources {
				if number(src["source_order"]) >= sourceOrder {
					sourceOrder = number(src["source_order"]) + 1
				}
				if str(src["provider_name"]) == req.Provider && str(src["media_id"]) == req.MediaID {
					sourceID = number(src["id"])
					sourceOrder = number(src["source_order"])
					sourceSnapshot = src
					break
				}
			}
			if e == nil && sourceID == 0 {
				if sourceOrder < 1 {
					sourceOrder = 1
				}
				sourceID, e = s.Store.Insert(ctx, "anime_sources", store.Row{"anime_id": animeID, "source_order": sourceOrder, "provider_name": req.Provider, "media_id": req.MediaID, "created_at": s.now()})
			}
		}
		if e == nil && sourceSnapshot == nil {
			sourceSnapshot, e = s.Store.Get(ctx, "anime_sources", sourceID)
		}
		if e == nil && (sourceSnapshot == nil || number(sourceSnapshot["anime_id"]) != animeID || number(sourceSnapshot["source_order"]) != sourceOrder || str(sourceSnapshot["provider_name"]) != req.Provider || str(sourceSnapshot["media_id"]) != req.MediaID) {
			e = errors.New("import source changed during admission; reload and retry")
		}
	}
	s.importMu.Unlock()
	if e != nil {
		return nil, e
	}
	if mediaPlan != nil && mediaPlan.group != nil {
		if e = s.captureMediaGroupTarget(ctx, mediaPlan, req, sourceSnapshot); e != nil {
			return nil, e
		}
		ctx = mediaGroupPlanContext(ctx, mediaPlan)
	}
	if initialEpisodes == nil {
		initialEpisodes, e = s.captureWorkflowEpisodes(ctx, sourceSnapshot)
		if e != nil {
			return nil, e
		}
	}
	if err := s.validateMediaListingProof(ctx, mediaListingProofFromContext(ctx), sourceSnapshot); err != nil {
		return nil, err
	}
	// Read-only binding preflight prevents a known later mismatch from changing
	// any old pool or ancillary metadata. Each episode is checked again before
	// download and publication; this is not a promise of global batch rollback.
	e = s.libTransaction(ctx, func(tx *sql.Tx) error {
		if err := s.validateSourceSnapshot(ctx, tx, sourceSnapshot); err != nil {
			return err
		}
		for _, ep := range eps {
			existing := initialEpisodes[int64(ep.Index)]
			if existing != nil {
				if err := s.validateDownloadIdentity(ctx, tx, existing, sourceSnapshot); err != nil {
					failedIndex = ep.Index
					return err
				}
			} else if err := s.validateMediaGroupProviderOwnership(ctx, tx, sourceSnapshot, int64(ep.Index), ep.ID); err != nil {
				failedIndex = ep.Index
				return err
			}
			current, err := s.libOne(ctx, tx, "episode", "source_id = ? AND episode_index = ?", sourceID, ep.Index)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			if current != nil && str(current["provider_episode_id"]) != ep.ID {
				failedIndex = ep.Index
				return errWorkflowEpisodeConflict
			}
		}
		return nil
	})
	if e != nil {
		return nil, e
	}
	emptyEpisodes := []int{}
	for i, ep := range eps {
		if e := job.Checkpoint(ctx); e != nil {
			return nil, e
		}
		failedIndex = ep.Index
		existing, e := s.prepareWorkflowEpisodeExpected(ctx, sourceSnapshot, initialEpisodes[int64(ep.Index)], int64(ep.Index), ep.Title, ep.URL, ep.ID, true)
		if e != nil {
			return nil, e
		}
		progress(i*100/len(eps), fmt.Sprintf("Fetching episode %d", ep.Index))
		fetched, e := s.fetchNativeImport(ctx, existing, sourceSnapshot)
		if e != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			if errors.Is(e, errImportProviderFetch) {
				return nil, fmt.Errorf("import stopped at episode %d: provider request failed; earlier completed episodes remain committed", ep.Index)
			}
			return nil, e
		}
		// Another unpaused subscriber may have published shared work. This
		// caller still owns its bookkeeping and ancillary metadata checkpoint.
		if e = job.Checkpoint(ctx); e != nil {
			return nil, e
		}
		if fetched.fetchedCount == 0 {
			emptyEpisodes = append(emptyEpisodes, ep.Index)
			emptyIndices = append(emptyIndices, ep.Index)
			failedIndex = -1
			continue
		}
		if mediaPlan != nil {
			mediaPlan.published = append(mediaPlan.published, existing)
		}
		imported++
		completedIndices = append(completedIndices, ep.Index)
		if fetched.unchanged {
			unchangedIndices = append(unchangedIndices, ep.Index)
		}
		failedIndex = -1
	}
	if imported == 0 {
		return nil, errors.New("requested episodes were missing or returned no comments; nothing was marked imported")
	}
	if mediaPlan != nil && mediaPlan.group != nil {
		// Keep group evidence unchanged until the final linkage transaction.
		// Enrichment there follows validation, so it cannot invalidate its own
		// captured initially-empty metadata identity before finalization.
		mediaPlan.source = sourceSnapshot
		mediaPlan.deferredMetadata = &req
		progress(100, "Import completed")
		return map[string]any{"animeId": animeID, "sourceId": sourceID, "episodes": imported, "emptyEpisodes": emptyEpisodes, "unchangedEpisodes": unchangedIndices, "warnings": warnings}, nil
	}
	// Defer ancillary metadata until every requested episode has been handled.
	// A failed or stale download cannot leave its uncommitted enrichment behind.
	if e = s.libTransaction(ctx, func(tx *sql.Tx) error {
		if e := s.validateSourceSnapshot(ctx, tx, sourceSnapshot); e != nil {
			return e
		}
		if mediaPlan != nil {
			anime, err := s.libOne(ctx, tx, "anime", "id=?", animeID)
			if err != nil {
				return err
			}
			if err = s.validateMediaLibraryTarget(ctx, tx, anime, req); err != nil {
				return err
			}
		}
		return s.storeRecognitionMetadataTx(ctx, tx, animeID, req)
	}); e != nil {
		return nil, e
	}
	if mediaPlan != nil {
		mediaPlan.source = sourceSnapshot
	}
	progress(100, "Import completed")
	return map[string]any{"animeId": animeID, "sourceId": sourceID, "episodes": imported, "emptyEpisodes": emptyEpisodes, "unchangedEpisodes": unchangedIndices, "warnings": warnings}, nil
}
