// SPDX-License-Identifier: AGPL-3.0-only
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/AniBakaBaka/AniDan/internal/provider"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/AniBakaBaka/AniDan/internal/recognition"
	"github.com/AniBakaBaka/AniDan/internal/store"
)

type recognitionLocalMatch struct {
	Wire         map[string]any
	Favorite     bool
	Priority     int64
	SourceOrder  int64
	EpisodeIndex int64
}

func matchWire(a, ep store.Row) map[string]any {
	typ, desc := playerType(a)
	return map[string]any{"episodeId": ep["id"], "animeId": a["id"], "animeTitle": a["title"], "episodeTitle": ep["title"], "type": typ, "typeDescription": desc, "shift": 0, "imageUrl": a["image_url"]}
}
func (s *Server) recognitionLocalMatches(ctx context.Context, title string, season, episode *int, source string) ([]recognitionLocalMatch, error) {
	rows, e := s.localSearch(ctx, title)
	if e != nil {
		return nil, e
	}
	out := []recognitionLocalMatch{}
	for _, a := range rows {
		if season != nil && number(a["season"]) != int64(*season) {
			continue
		}
		aliases, e := s.animeAliases(ctx, a["id"])
		if e != nil {
			return nil, e
		}
		exact := false
		for _, v := range append([]string{str(a["title"])}, aliases...) {
			if normalizedTitle(v) == normalizedTitle(title) {
				exact = true
				break
			}
		}
		if !exact {
			continue
		}
		sources, e := s.Store.List(ctx, "anime_sources", store.Row{"anime_id": a["id"]}, 1000, 0)
		if e != nil {
			return nil, e
		}
		for _, src := range sources {
			if source != "" && source != str(src["provider_name"]) {
				continue
			}
			filter := store.Row{"source_id": src["id"]}
			if episode != nil {
				filter["episode_index"] = *episode
			}
			eps, e := s.Store.List(ctx, "episode", filter, 10000, 0)
			if e != nil {
				return nil, e
			}
			// Unknown episode is only auto-matchable if this source contains one episode.
			for _, ep := range eps {
				out = append(out, s.recognitionMatchRow(ctx, a, ep, src, episode != nil || len(eps) == 1))
			}
		}
	}
	return out, nil
}
func matchResponse(matches []recognitionLocalMatch, warnings []string) map[string]any {
	sort.SliceStable(matches, func(i, j int) bool {
		a, b := matches[i], matches[j]
		if a.Favorite != b.Favorite {
			return a.Favorite
		}
		if a.Priority != b.Priority {
			return a.Priority < b.Priority
		}
		return a.SourceOrder < b.SourceOrder
	})
	unique := []recognitionLocalMatch{}
	seen := map[string]bool{}
	for _, m := range matches {
		key := str(m.Wire["episodeId"])
		if !seen[key] {
			unique = append(unique, m)
			seen[key] = true
		}
	}
	favorites := []recognitionLocalMatch{}
	for _, m := range unique {
		if m.Favorite {
			favorites = append(favorites, m)
		}
	}
	if len(favorites) > 0 {
		unique = favorites[:1]
	} else if len(unique) > 1 {
		same := true
		for _, m := range unique[1:] {
			if str(m.Wire["animeId"]) != str(unique[0].Wire["animeId"]) || m.EpisodeIndex != unique[0].EpisodeIndex {
				same = false
				break
			}
		}
		if same {
			unique = unique[:1]
		}
	}
	wire := []any{}
	for _, m := range unique {
		wire = append(wire, m.Wire)
	}
	out := playerOK(map[string]any{"isMatched": len(unique) == 1, "matches": wire})
	if len(warnings) > 0 {
		out["warnings"] = warnings
	}
	return out
}
func (s *Server) recognitionTMDBMatches(ctx context.Context, title string, season, episode *int) ([]recognitionLocalMatch, error) {
	if episode == nil {
		return nil, nil
	}
	candidates, e := s.localSearch(ctx, title)
	if e != nil {
		return nil, e
	}
	out := []recognitionLocalMatch{}
	seenGroup := map[string]bool{}
	for _, a := range candidates {
		aliases, e := s.animeAliases(ctx, a["id"])
		if e != nil {
			return nil, e
		}
		exact := false
		for _, v := range append([]string{str(a["title"])}, aliases...) {
			if normalizedTitle(v) == normalizedTitle(title) {
				exact = true
				break
			}
		}
		if !exact {
			continue
		}
		metas, e := s.Store.List(ctx, "anime_metadata", store.Row{"anime_id": a["id"]}, 1, 0)
		if e != nil {
			return nil, e
		}
		for _, meta := range metas {
			tmdb, group := str(meta["tmdb_id"]), str(meta["tmdb_episode_group_id"])
			if tmdb != "" && group == "" && strings.EqualFold(s.setting(ctx, "aiEpisodeGroupEnabled", "false"), "true") {
				group, e = s.recognitionSelectTMDBGroup(ctx, a, meta, season, episode)
				if e != nil {
					return nil, e
				}
			}
			if tmdb == "" || group == "" || seenGroup[tmdb+"\x00"+group] {
				continue
			}
			seenGroup[tmdb+"\x00"+group] = true
			mappings, e := s.Store.List(ctx, "tmdb_episode_mapping", store.Row{"tmdb_tv_id": tmdb, "tmdb_episode_group_id": group}, 100000, 0)
			if e != nil {
				return nil, e
			}
			targets := map[string]bool{}
			for _, m := range mappings {
				custom := number(m["custom_episode_number"]) == int64(*episode) && (season == nil && number(m["custom_season_number"]) > 0 || season != nil && number(m["custom_season_number"]) == int64(*season))
				official := number(m["tmdb_episode_number"]) == int64(*episode) && (season == nil && number(m["tmdb_season_number"]) > 0 || season != nil && number(m["tmdb_season_number"]) == int64(*season))
				if custom || official {
					targets[str(m["custom_season_number"])+":"+str(m["custom_episode_number"])] = true
				}
			}
			linked, e := s.Store.List(ctx, "anime_metadata", store.Row{"tmdb_id": tmdb}, 10000, 0)
			if e != nil {
				return nil, e
			}
			for _, link := range linked {
				if str(link["tmdb_episode_group_id"]) != group {
					continue
				}
				anime, e := s.Store.Get(ctx, "anime", link["anime_id"])
				if e != nil {
					return nil, e
				}
				if anime == nil {
					continue
				}
				sources, e := s.Store.List(ctx, "anime_sources", store.Row{"anime_id": anime["id"]}, 1000, 0)
				if e != nil {
					return nil, e
				}
				for _, src := range sources {
					eps, e := s.Store.List(ctx, "episode", store.Row{"source_id": src["id"]}, 10000, 0)
					if e != nil {
						return nil, e
					}
					for _, ep := range eps {
						if targets[str(anime["season"])+":"+str(ep["episode_index"])] {
							out = append(out, s.recognitionMatchRow(ctx, anime, ep, src, true))
						}
					}
				}
			}
		}
	}
	return out, nil
}

func (s *Server) matchRecognized(ctx context.Context, req matchRequest) (map[string]any, error) {
	if strings.TrimSpace(req.FileName) == "" || len(req.FileName) > recognition.MaxText {
		return nil, errors.New("fileName required, maximum 64 KiB")
	}
	raw := filepath.Base(strings.ReplaceAll(req.FileName, "\\", "/"))
	parsed := recognition.ParseFilename(raw)
	if parsed == nil {
		return matchResponse(nil, nil), nil
	}
	matches, e := s.recognitionLocalMatches(ctx, parsed.Title, parsed.Season, parsed.Episode, "")
	if e != nil {
		return nil, e
	}
	if len(matches) > 0 {
		return matchResponse(matches, nil), nil
	}
	rules, w, e := s.loadRecognition(ctx)
	if e != nil {
		return nil, e
	}
	warnings := recognitionWarnings(w)
	raw = strings.TrimSuffix(raw, filepath.Ext(raw))
	pre := rules.Preprocess(recognition.Input{Text: raw, Season: parsed.Season, Episode: parsed.Episode})
	if pre.Changed {
		if next := recognition.ParseFilename(pre.Text); next != nil {
			parsed.Title = next.Title
		}
		parsed.Season, parsed.Episode = pre.Season, pre.Episode
	} else {
		pre = rules.Preprocess(recognition.Input{Text: parsed.Title, Season: parsed.Season, Episode: parsed.Episode})
		parsed.Title, parsed.Season, parsed.Episode = pre.Text, pre.Season, pre.Episode
	}
	warnings = append(warnings, recognitionWarnings(pre.Warnings)...)
	titles := []string{parsed.Title}
	if strings.EqualFold(s.setting(ctx, "nameConversionEnabled", "false"), "true") {
		p := &recognitionSearch{Title: parsed.Title, Season: parsed.Season, Episode: parsed.Episode, Aliases: []string{}, Warnings: []string{}}
		s.convertRecognitionTitle(ctx, p)
		titles = uniqueTitles(append([]string{p.Title, parsed.Title}, p.Aliases...)...)
		warnings = append(warnings, p.Warnings...)
	}
	contexts := []string{"__unscoped__"}
	for _, rule := range rules.Items {
		if rule.Provider != "" && rule.Provider != "all" {
			contexts = append(contexts, rule.Provider)
		}
	}
	contexts = uniqueTitles(contexts...)
	for _, title := range titles {
		found, e := s.recognitionLocalMatches(ctx, title, parsed.Season, parsed.Episode, "")
		if e != nil {
			return nil, e
		}
		matches = append(matches, found...)
		for _, source := range contexts {
			post := rules.Postprocess(recognition.Input{Text: title, Season: parsed.Season, Episode: parsed.Episode, Provider: source})
			target := post.Text
			if v := str(post.Metadata["title"]); v != "" {
				target = v
			}
			season := post.Season
			if v := str(post.Metadata["s"]); v != "" {
				n, err := strconv.Atoi(v)
				if err != nil || n < 0 {
					return nil, errors.New("invalid recognition season")
				}
				season = recognition.Int(n)
			}
			if !post.Changed {
				continue
			}
			provider := source
			if provider == "__unscoped__" {
				provider = ""
			}
			found, e := s.recognitionLocalMatches(ctx, target, season, post.Episode, provider)
			if e != nil {
				return nil, e
			}
			matches = append(matches, found...)
			warnings = append(warnings, recognitionWarnings(post.Warnings)...)
		}
	}
	if len(matches) > 0 {
		return matchResponse(matches, warnings), nil
	}
	for _, title := range titles {
		found, e := s.recognitionTMDBMatches(ctx, title, parsed.Season, parsed.Episode)
		if e != nil {
			return nil, e
		}
		matches = append(matches, found...)
	}
	if len(matches) == 0 && strings.EqualFold(s.setting(ctx, "matchFallbackEnabled", "false"), "true") {
		fallback, e := s.recognitionMatchFallback(ctx, req, parsed)
		if e != nil {
			return nil, e
		}
		if fallback != nil {
			return fallback, nil
		}
	}
	return matchResponse(matches, warnings), nil
}

type recognitionPlayerToken struct{}

func (s *Server) recognitionFallbackAllowed(ctx context.Context, kind, token, input string) (bool, error) {
	if pattern := strings.TrimSpace(s.setting(ctx, kind+"FallbackBlacklist", "")); pattern != "" {
		re, e := recognition.CompileRegex(pattern)
		if e != nil {
			return false, e
		}
		if re.MatchString(input) {
			return false, nil
		}
	}
	var allowed []json.Number
	decoder := json.NewDecoder(strings.NewReader(s.setting(ctx, kind+"FallbackTokens", "[]")))
	decoder.UseNumber()
	if e := decoder.Decode(&allowed); e != nil {
		return false, fmt.Errorf("invalid %sFallbackTokens JSON", kind)
	}
	if len(allowed) == 0 {
		return true, nil
	}
	if token == "" {
		return false, nil
	}
	rows, e := s.Store.List(ctx, "api_tokens", store.Row{"token": token}, 1, 0)
	if e != nil {
		return false, e
	}
	if len(rows) != 1 {
		return false, nil
	}
	for _, id := range allowed {
		if id.String() == str(rows[0]["id"]) {
			return true, nil
		}
	}
	return false, nil
}
func (s *Server) recognitionMatchFallbackWork(ctx context.Context, parsed *recognition.Parsed) (map[string]any, error) {
	keyword := parsed.Title
	if parsed.Season != nil {
		keyword += fmt.Sprintf(" S%02d", *parsed.Season)
		if parsed.Episode != nil {
			keyword += fmt.Sprintf("E%02d", *parsed.Episode)
		}
	}
	var year *int
	if parsed.Year != nil {
		if n, e := strconv.Atoi(*parsed.Year); e == nil {
			year = recognition.Int(n)
		}
	}
	mediaType := "tv_series"
	if parsed.IsMovie {
		mediaType = "movie"
	}
	req, candidates, e := s.selectImportCandidate(ctx, keyword, nil, parsed.Episode, year, mediaType)
	if e != nil {
		out := matchResponse(nil, []string{e.Error()})
		out["sourceCandidates"] = candidates
		return out, nil
	}
	result := provider.SearchResult{Provider: req.Provider, Result: provider.Result{ID: req.MediaID, Title: req.Title, Type: req.Type, Season: req.Season, ImageURL: req.ImageURL}}
	if req.Year != nil {
		result.Year = *req.Year
	}
	aid, e := s.reserveRecognitionResult(ctx, result)
	if e != nil {
		return nil, e
	}
	stored, rules, warnings, e := s.prepareStorage(ctx, req)
	if e != nil {
		return nil, e
	}
	episode := req.CurrentEpisodeIndex
	if episode != nil {
		post := rules.Postprocess(recognition.Input{Text: req.Title, Season: recognition.Int(req.Season), Episode: episode, Provider: req.Provider})
		episode = post.Episode
	}
	// Reservation exposes only IDs of rows actually created from verified episodes.
	matches, e := s.recognitionLocalMatches(ctx, stored.Title, recognition.Int(stored.Season), episode, req.Provider)
	if e != nil {
		return nil, e
	}
	own := matches[:0]
	for _, m := range matches {
		if str(m.Wire["animeId"]) == strconv.FormatInt(aid, 10) {
			own = append(own, m)
		}
	}
	return matchResponse(own, warnings), nil
}

func (s *Server) recognitionMatchRow(ctx context.Context, a, ep, src store.Row, favoriteEligible bool) recognitionLocalMatch {
	priority := int64(1 << 30)
	if scraper, e := s.Store.Get(ctx, "scrapers", str(src["provider_name"])); e == nil && scraper != nil {
		priority = number(scraper["display_order"])
	}
	return recognitionLocalMatch{Wire: matchWire(a, ep), Favorite: boolean(src["is_favorited"]) && favoriteEligible, Priority: priority, SourceOrder: number(src["source_order"]), EpisodeIndex: number(ep["episode_index"])}
}

func (s *Server) recognitionSelectTMDBGroup(ctx context.Context, anime, meta store.Row, season, episode *int) (string, error) {
	tv, e := strconv.ParseInt(str(meta["tmdb_id"]), 10, 64)
	if e != nil || tv <= 0 {
		return "", errors.New("invalid TMDB ID for episode-group selection")
	}
	groups, e := s.Metadata.TMDBGroups(ctx, tv)
	if e != nil {
		return "", e
	}
	if len(groups) == 0 {
		return "", nil
	}
	if len(groups) > 100 {
		return "", errors.New("TMDB episode-group list exceeds 100")
	}
	selected := 0
	if len(groups) > 1 {
		for i := range groups {
			groups[i]["index"] = i
		}
		decision, e := s.Metadata.AITransform(ctx, "episode_group", map[string]any{"title": anime["title"], "season": season, "episode": episode, "episode_groups": groups})
		if e != nil {
			return "", e
		}
		rawIndex, hasIndex := decision["index"]
		rawConfidence, hasConfidence := decision["confidence"]
		index, indexErr := strconv.Atoi(str(rawIndex))
		confidence, confidenceErr := strconv.Atoi(str(rawConfidence))
		if !hasIndex || !hasConfidence || indexErr != nil || confidenceErr != nil || index < -1 || index >= len(groups) || confidence < 0 || confidence > 100 {
			return "", errors.New("AI returned an invalid episode-group decision")
		}
		if index < 0 || confidence < 80 {
			return "", nil
		}
		selected = index
	}
	id := str(groups[selected]["id"])
	if e := validateGroupID(id); e != nil {
		return "", e
	}
	rows, e := s.episodeGroupRows(ctx, id)
	if e != nil {
		return "", e
	}
	if len(rows) > 0 {
		if str(rows[0]["tmdb_tv_id"]) != strconv.FormatInt(tv, 10) {
			return "", errors.New("episode group belongs to a different TMDB title")
		}
		if e := s.Store.Update(ctx, "anime_metadata", meta["id"], store.Row{"tmdb_episode_group_id": id}); e != nil {
			return "", e
		}
		return id, nil
	}
	group, e := s.Metadata.TMDBEpisodeGroup(ctx, id)
	if e != nil {
		return "", e
	}
	aid := number(anime["id"])
	if e = s.episodeGroupSave(ctx, tv, group, "create", &aid); e != nil {
		return "", e
	}
	return id, nil
}
