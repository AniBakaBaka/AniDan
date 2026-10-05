// SPDX-License-Identifier: AGPL-3.0-only
// Search/storage stage ordering follows pinned Misaka search.py/import_core.py.
package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode"

	"github.com/AniBakaBaka/AniDan/internal/danmaku"
	"github.com/AniBakaBaka/AniDan/internal/integration"
	"github.com/AniBakaBaka/AniDan/internal/provider"
	"github.com/AniBakaBaka/AniDan/internal/recognition"
	"github.com/AniBakaBaka/AniDan/internal/store"
)

type recognitionSearch struct {
	Original string                `json:"original"`
	Title    string                `json:"title"`
	Season   *int                  `json:"season"`
	Episode  *int                  `json:"episode"`
	Aliases  []string              `json:"aliases"`
	Warnings []string              `json:"warnings"`
	Mapping  *recognition.Mapping  `json:"mapping"`
	Trace    []recognition.Trace   `json:"rules"`
	Metadata *integration.Metadata `json:"metadata,omitempty"`
	rules    *recognition.Rules
}

func normalizedTitle(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return -1
		}
		if r == '：' {
			return ':'
		}
		return unicode.ToLower(r)
	}, strings.TrimSpace(s))
}
func uniqueTitles(values ...string) []string {
	out := []string{}
	seen := map[string]bool{}
	for _, v := range values {
		v = strings.TrimSpace(v)
		key := normalizedTitle(v)
		if key != "" && len(v) <= 1024 && !seen[key] {
			seen[key] = true
			out = append(out, v)
		}
	}
	return out
}
func chineseTitle(s string) bool {
	han := false
	for _, r := range s {
		if unicode.In(r, unicode.Hiragana, unicode.Katakana, unicode.Hangul) {
			return false
		}
		han = han || unicode.In(r, unicode.Han)
	}
	return han
}
func metadataTitles(m integration.Metadata) []string {
	return uniqueTitles(append([]string{m.Title, m.NameEn, m.NameJp, m.NameRomaji}, append(m.AliasesCn, m.AliasesJp...)...)...)
}
func (s *Server) animeAliases(ctx context.Context, id any) ([]string, error) {
	rows, e := s.Store.List(ctx, "anime_aliases", store.Row{"anime_id": id}, 1, 0)
	if e != nil {
		return nil, e
	}
	out := []string{}
	for _, row := range rows {
		for _, k := range []string{"name_en", "name_jp", "name_romaji", "alias_cn_1", "alias_cn_2", "alias_cn_3"} {
			if v, ok := row[k].(string); ok {
				out = append(out, v)
			}
		}
	}
	return uniqueTitles(out...), nil
}
func (s *Server) recognitionAliases(ctx context.Context, title string) ([]string, error) {
	if strings.TrimSpace(title) == "" {
		return []string{}, nil
	}
	rows, e := s.localSearch(ctx, title)
	if e != nil {
		return nil, e
	}
	out := []string{}
	for _, row := range rows {
		a, e := s.animeAliases(ctx, row["id"])
		if e != nil {
			return nil, e
		}
		a = append([]string{str(row["title"])}, a...)
		exact := false
		for _, v := range a {
			if normalizedTitle(v) == normalizedTitle(title) {
				exact = true
				break
			}
		}
		if exact {
			out = append(out, a...)
		}
	}
	return uniqueTitles(out...), nil
}

// prepareSearch never changes persisted metadata. Explicit reverse rules take
// precedence over automatic conversion, then the search-only preprocessing runs.
func (s *Server) prepareSearch(ctx context.Context, keyword string) (*recognitionSearch, error) {
	return s.prepareSearchWithIdentity(ctx, keyword, nil)
}

// Media catalog titles are authoritative text, not free-text search shorthand.
// Their separate coordinates must not turn a literal sequel number into an alias
// for a different work. All later recognition/conversion stages stay shared.
func (s *Server) prepareSearchWithIdentity(ctx context.Context, keyword string, identity *recognition.Parsed) (*recognitionSearch, error) {
	if strings.TrimSpace(keyword) == "" || len(keyword) > recognition.MaxText {
		return nil, errors.New("nonempty keyword required, maximum 64 KiB")
	}
	rules, warnings, e := s.loadRecognition(ctx)
	if e != nil {
		return nil, e
	}
	var parsed recognition.Parsed
	if identity == nil {
		parsed = recognition.ParseSearchKeyword(keyword)
	} else {
		parsed = *identity
	}
	p := &recognitionSearch{Original: keyword, Title: parsed.Title, Season: parsed.Season, Episode: parsed.Episode, Aliases: []string{}, Warnings: recognitionWarnings(warnings), Trace: []recognition.Trace{}, rules: rules}
	p.Mapping = rules.SearchMapping(strings.TrimSpace(keyword))
	if p.Mapping == nil {
		p.Mapping = rules.SearchMapping(parsed.Title)
	}
	if p.Mapping != nil {
		p.Title = p.Mapping.SearchTitle
		p.Season = p.Mapping.SearchSeason
		return p, nil
	}
	local, e := s.recognitionAliases(ctx, p.Title)
	if e != nil {
		return nil, e
	}
	p.Aliases = local
	if strings.EqualFold(s.setting(ctx, "nameConversionT2SEnabled", "false"), "true") {
		converted, e := danmaku.Convert(p.Title, 1)
		if e != nil {
			return nil, e
		}
		p.Title = converted
	}
	if strings.EqualFold(s.setting(ctx, "nameConversionEnabled", "false"), "true") && !chineseTitle(p.Title) {
		s.convertRecognitionTitle(ctx, p)
	}
	pre := rules.Preprocess(recognition.Input{Text: p.Title, Season: p.Season, Episode: p.Episode})
	p.Title, p.Season, p.Episode, p.Trace = pre.Text, pre.Season, pre.Episode, pre.Trace
	p.Warnings = append(p.Warnings, recognitionWarnings(pre.Warnings)...)
	if strings.TrimSpace(p.Title) == "" {
		return nil, errors.New("recognition removed the entire search title")
	}
	return p, nil
}
func (s *Server) convertRecognitionTitle(ctx context.Context, p *recognitionSearch) {
	if s.Metadata == nil {
		p.Warnings = append(p.Warnings, "metadata client unavailable")
		return
	}
	var priorities []struct {
		Key     string `json:"key"`
		Enabled *bool  `json:"enabled"`
	}
	raw := s.setting(ctx, "nameConversionSourcePriority", `[{"key":"bangumi","enabled":true},{"key":"tmdb","enabled":true},{"key":"tvdb","enabled":true},{"key":"douban","enabled":true},{"key":"imdb","enabled":true}]`)
	if e := json.Unmarshal([]byte(raw), &priorities); e != nil {
		p.Warnings = append(p.Warnings, "invalid nameConversionSourcePriority JSON")
		return
	}
	if len(priorities) > 8 {
		p.Warnings = append(p.Warnings, "name conversion source list exceeds 8")
		return
	}
	original := p.Title
	for _, source := range priorities {
		if source.Enabled != nil && !*source.Enabled {
			continue
		}
		if !integration.KnownProvider(source.Key) {
			p.Warnings = append(p.Warnings, "unknown metadata provider: "+source.Key)
			continue
		}
		rows, e := s.Metadata.Search(ctx, source.Key, original, "multi", integration.Credential{})
		if e != nil {
			p.Warnings = append(p.Warnings, e.Error())
			continue
		}
		candidate := -1
		for i, m := range rows {
			for _, title := range metadataTitles(m) {
				if normalizedTitle(title) == normalizedTitle(original) {
					if candidate >= 0 && candidate != i {
						candidate = -2
						break
					}
					candidate = i
					break
				}
			}
			if candidate == -2 {
				break
			}
		}
		if candidate == -1 && len(rows) == 1 {
			candidate = 0
		}
		if candidate < 0 {
			if len(rows) > 1 {
				p.Warnings = append(p.Warnings, source.Key+": ambiguous metadata results; no automatic conversion")
			}
			continue
		}
		m := rows[candidate]
		details, e := s.Metadata.Details(ctx, source.Key, m.ID, m.Type, integration.Credential{})
		if e != nil {
			p.Warnings = append(p.Warnings, e.Error())
		} else if details != nil {
			m = *details
		}
		p.Metadata = &m
		p.Aliases = uniqueTitles(append(p.Aliases, metadataTitles(m)...)...)
		// A named season is used only when it is distinct from the generic season label.
		for _, season := range m.Seasons {
			if p.Season != nil && season.SeasonNumber == *p.Season {
				p.Aliases = uniqueTitles(append(p.Aliases, season.Aliases...)...)
			}
		}
		for _, title := range metadataTitles(m) {
			if chineseTitle(title) {
				p.Title = title
				return
			}
		}
	}
	if strings.EqualFold(s.setting(ctx, "aiNameConversionEnabled", "false"), "true") {
		v, e := s.Metadata.AITransform(ctx, "name_conversion", map[string]any{"query": original, "type": nil})
		if e != nil {
			p.Warnings = append(p.Warnings, e.Error())
		} else if name, ok := v["chinese_name"].(string); ok && chineseTitle(name) && len(name) <= 1024 && (str(v["confidence"]) == "high" || str(v["confidence"]) == "medium") {
			p.Title = name
		}
	}
	if strings.EqualFold(s.setting(ctx, "aiAliasExpansionEnabled", "false"), "true") {
		v, e := s.Metadata.AITransform(ctx, "alias_expansion", map[string]any{"title": original, "existing_aliases": p.Aliases})
		if e != nil {
			p.Warnings = append(p.Warnings, e.Error())
		} else if rows, ok := v["aliases"].([]any); ok {
			for i, row := range rows {
				if i >= 5 {
					break
				}
				m, ok := row.(map[string]any)
				if ok && str(m["confidence"]) == "high" {
					if name, ok := m["name"].(string); ok {
						p.Aliases = uniqueTitles(append(p.Aliases, name)...)
					}
				}
			}
		}
	}
}

func (s *Server) searchRecognition(ctx context.Context, p *recognitionSearch) ([]provider.SearchResult, error) {
	keywords := uniqueTitles(append([]string{p.Title}, p.Aliases...)...)
	if len(keywords) > 6 {
		keywords = keywords[:6]
		p.Warnings = append(p.Warnings, "supplemental alias search limited to 6 distinct keywords")
	}
	out := []provider.SearchResult{}
	seen := map[string]bool{}
	var errs []error
	for _, kw := range keywords {
		rows, e := s.rawProviderSearch(ctx, kw)
		if e != nil {
			errs = append(errs, e)
		}
		for _, v := range rows {
			key := v.Provider + "\x00" + v.ID
			if seen[key] {
				continue
			}
			seen[key] = true
			if p.Season != nil && v.Season != *p.Season {
				continue
			}
			out = append(out, v)
		}
		if ctx.Err() != nil {
			break
		}
	}
	filtered, e := s.filterSearchResults(ctx, out)
	if e != nil {
		return nil, e
	}
	limit, parseErr := strconv.Atoi(s.setting(ctx, "searchMaxResultsPerSource", "30"))
	if parseErr != nil || limit < 1 || limit > 100 {
		limit = 30
		p.Warnings = append(p.Warnings, "invalid searchMaxResultsPerSource; using 30 (allowed range 1..100)")
	}
	perSource := map[string]int{}
	capped := make([]provider.SearchResult, 0, len(filtered))
	for _, row := range filtered {
		if perSource[row.Provider] >= limit {
			continue
		}
		perSource[row.Provider]++
		capped = append(capped, row)
	}
	return capped, errors.Join(errs...)
}
func (s *Server) providerSearch(ctx context.Context, keyword string) ([]provider.SearchResult, error) {
	p, e := s.prepareSearch(ctx, keyword)
	if e != nil {
		return nil, e
	}
	rows, e := s.searchRecognition(ctx, p)
	if len(p.Warnings) > 0 {
		e = errors.Join(e, errors.New(strings.Join(p.Warnings, "; ")))
	}
	return rows, e
}
func (p *recognitionSearch) result(v provider.SearchResult) map[string]any {
	row := providerResult(v)
	row["currentEpisodeIndex"] = p.Episode
	if hint := p.rules.HintForResult(v.Title, v.Provider); hint != nil {
		row["recognitionTitle"] = hint["recognition_title"]
	}
	return row
}

// Source exclusions apply first. Title and alias filters use both the raw source
// title and its storage name, without mutating provider episode identifiers.
func (s *Server) recognitionEpisodes(ctx context.Context, name, media, title string, aliases []string) ([]provider.Episode, []map[string]any, error) {
	return s.recognitionEpisodesWithCache(ctx, name, media, title, aliases, false)
}
func (s *Server) recognitionEpisodesPreview(ctx context.Context, name, media, title string, aliases []string) ([]provider.Episode, []map[string]any, error) {
	return s.recognitionEpisodesWithCache(ctx, name, media, title, aliases, true)
}
func (s *Server) recognitionEpisodesWithCache(ctx context.Context, name, media, title string, aliases []string, preview bool) ([]provider.Episode, []map[string]any, error) {
	eps, excluded, e := s.sourceEpisodesWithCache(ctx, name, media, preview)
	if e != nil {
		return nil, nil, e
	}
	rules, _, e := s.loadRecognition(ctx)
	if e != nil {
		return nil, nil, e
	}
	post := rules.Postprocess(recognition.Input{Text: title, Provider: name})
	aliases = append(aliases, post.Text)
	local, e := s.recognitionAliases(ctx, title)
	if e != nil {
		return nil, nil, e
	}
	aliases = append(aliases, local...)
	filtered, e := s.filterEpisodes(ctx, eps, title, name, media, aliases)
	if e != nil {
		return nil, nil, e
	}
	kept := map[string]bool{}
	for _, ep := range filtered {
		kept[ep.ID+"\x00"+strconv.Itoa(ep.Index)] = true
	}
	for _, ep := range eps {
		if !kept[ep.ID+"\x00"+strconv.Itoa(ep.Index)] {
			excluded = append(excluded, map[string]any{"provider": name, "episodeId": ep.ID, "title": ep.Title, "episodeIndex": ep.Index, "url": ep.URL, "filterReason": "全局或单剧分集标题过滤"})
		}
	}
	return filtered, excluded, nil
}

// prepareStorage applies postprocessing exactly once to source title/season.
// Caller-provided metadata IDs win over rule and metadata-service suggestions.
func (s *Server) prepareStorage(ctx context.Context, req ImportRequest) (ImportRequest, *recognition.Rules, []string, error) {
	rules, w, e := s.loadRecognition(ctx)
	if e != nil {
		return req, nil, nil, e
	}
	prepared, warnings, e := prepareStorageWithRules(req, rules, w)
	return prepared, rules, warnings, e
}

// Pure storage projection is shared by selection and execution. It never performs
// metadata I/O and caller-provided metadata IDs retain precedence.
func prepareStorageWithRules(req ImportRequest, rules *recognition.Rules, w []recognition.Warning) (ImportRequest, []string, error) {
	cloned := map[string]string{}
	for key, value := range req.Metadata {
		cloned[key] = value
	}
	req.Metadata = cloned
	out := rules.Postprocess(recognition.Input{Text: req.Title, Season: recognition.Int(req.Season), Provider: req.Provider})
	warnings := recognitionWarnings(append(w, out.Warnings...))
	req.Title = out.Text
	if out.Season != nil {
		req.Season = *out.Season
	}
	if req.Metadata == nil {
		req.Metadata = map[string]string{}
	}
	for k, target := range map[string]string{"tmdbid": "tmdbId", "doubanid": "doubanId"} {
		if req.Metadata[target] == "" && out.Metadata[k] != nil {
			req.Metadata[target] = str(out.Metadata[k])
		}
	}
	if v := str(out.Metadata["title"]); v != "" {
		req.Title = v
	}
	if v := str(out.Metadata["s"]); v != "" {
		n, e := strconv.Atoi(v)
		if e != nil || n < 0 {
			return req, warnings, fmt.Errorf("invalid storage season %q", v)
		}
		req.Season = n
	}
	if v := str(out.Metadata["type"]); v != "" {
		if v == "tv" {
			v = "tv_series"
		}
		req.Type = v
	}
	if strings.TrimSpace(req.Title) == "" {
		return req, warnings, errors.New("recognition removed storage title")
	}
	return req, warnings, nil
}

// selectImportCandidate shares the same pipeline as UI/provider/player search.
// It returns reviewable candidates for competing identities; equivalent sources
// retain the configured source priority used by the pinned implementation.
func (s *Server) selectImportCandidate(ctx context.Context, keyword string, season, episode, year *int, mediaType string) (ImportRequest, []map[string]any, error) {
	var zero ImportRequest
	p, e := s.prepareSearch(ctx, keyword)
	if e != nil {
		return zero, nil, e
	}
	if season != nil && p.Mapping == nil {
		p.Season = season
	}
	if episode != nil {
		p.Episode = episode
	}
	results, e := s.searchRecognition(ctx, p)
	if e != nil {
		return zero, nil, e
	}
	candidates := []provider.SearchResult{}
	wire := []map[string]any{}
	fav := []int{}
	titles := uniqueTitles(append([]string{keyword, recognition.ParseSearchKeyword(keyword).Title, p.Title}, p.Aliases...)...)
	for _, v := range results {
		if p.Mapping != nil {
			wanted := season
			if wanted == nil {
				wanted = recognition.ParseSearchKeyword(keyword).Season
			}
			if wanted != nil {
				post := p.rules.Postprocess(recognition.Input{Text: v.Title, Season: recognition.Int(v.Season), Provider: v.Provider})
				effective := post.Season
				if value := str(post.Metadata["s"]); value != "" {
					if n, err := strconv.Atoi(value); err == nil {
						effective = recognition.Int(n)
					}
				}
				if effective == nil || *effective != *wanted {
					continue
				}
			}
		}
		if mediaType != "" && mediaType != "unknown" && mediaType != v.Type && !(mediaType == "tv" && v.Type == "tv_series") {
			continue
		}
		if year != nil && v.Year != 0 && v.Year != *year {
			continue
		}
		exact := false
		for _, title := range titles {
			if normalizedTitle(title) == normalizedTitle(v.Title) {
				exact = true
				break
			}
		}
		hint := p.rules.HintForResult(v.Title, v.Provider)
		if hint != nil {
			for _, title := range titles {
				if normalizedTitle(title) == normalizedTitle(str(hint["recognition_title"])) {
					exact = true
					break
				}
			}
		}
		row := p.result(v)
		row["matchesRecognitionRule"] = hint != nil
		row["exactTitleMatch"] = exact
		row["inLibrary"] = false
		row["isFavorited"] = false
		sources, err := s.Store.List(ctx, "anime_sources", store.Row{"provider_name": v.Provider, "media_id": v.ID}, 1000, 0)
		if err != nil {
			return zero, nil, err
		}
		for _, src := range sources {
			row["inLibrary"] = true
			if boolean(src["is_favorited"]) {
				row["isFavorited"] = true
			}
		}
		candidates = append(candidates, v)
		wire = append(wire, row)
		if row["isFavorited"] == true && exact {
			fav = append(fav, len(candidates)-1)
		}
	}
	chosen := -1
	if len(fav) == 1 {
		chosen = fav[0]
	} else if len(fav) > 1 {
		return zero, wire, errors.New("multiple favorited sources match; select a provider and mediaId explicitly")
	}
	if chosen < 0 && strings.EqualFold(s.setting(ctx, "aiMatchEnabled", "false"), "true") && len(wire) > 0 {
		decision, err := s.Metadata.SelectMatch(ctx, map[string]any{"title": keyword, "season": p.Season, "episode": p.Episode, "year": year, "type": mediaType}, wire)
		if err != nil {
			if !strings.EqualFold(s.setting(ctx, "aiFallbackEnabled", "true"), "true") {
				return zero, wire, err
			}
			p.Warnings = append(p.Warnings, err.Error())
		} else if decision != nil && decision.Confidence >= 80 {
			chosen = decision.Index
		}
	}
	if chosen < 0 {
		identities := map[string]bool{}
		providers := map[string]bool{}
		knownYear := 0
		for i, row := range wire {
			if row["exactTitleMatch"] != true {
				continue
			}
			v := candidates[i]
			post := p.rules.Postprocess(recognition.Input{Text: v.Title, Season: recognition.Int(v.Season), Provider: v.Provider})
			title := post.Text
			if value := str(post.Metadata["title"]); value != "" {
				title = value
			}
			identities[normalizedTitle(title)+"|"+recognitionSeasonKey(post.Season)+"|"+v.Type] = true
			if providers[v.Provider] {
				return zero, wire, errors.New("multiple matching titles from one provider; select a mediaId explicitly")
			}
			providers[v.Provider] = true
			if v.Year > 0 {
				if knownYear != 0 && knownYear != v.Year {
					return zero, wire, errors.New("matching results have different release years; specify a year or mediaId")
				}
				knownYear = v.Year
			}
			if chosen < 0 {
				chosen = i
			}
		}
		if len(identities) > 1 {
			return zero, wire, errors.New("matching results describe different titles or seasons; select a provider and mediaId explicitly")
		}
	}
	if chosen < 0 {
		return zero, wire, errors.New("no unambiguous matching source found")
	}
	v := candidates[chosen]
	req := ImportRequest{Provider: v.Provider, MediaID: v.ID, Title: v.Title, Type: v.Type, Season: v.Season, ImageURL: v.ImageURL, CurrentEpisodeIndex: p.Episode, Metadata: map[string]string{}}
	req.RecognitionWarnings = append([]string{}, p.Warnings...)
	if req.Type == "" {
		req.Type = "tv_series"
	}
	if p.Episode != nil {
		episodes, _, err := s.recognitionEpisodes(ctx, v.Provider, v.ID, v.Title, p.Aliases)
		if err != nil {
			return zero, wire, err
		}
		var sourceIndex *int
		rawExists := false
		for _, ep := range episodes {
			if ep.Index == *p.Episode {
				rawExists = true
			}
			post := p.rules.Postprocess(recognition.Input{Text: v.Title, Season: recognition.Int(v.Season), Episode: recognition.Int(ep.Index), Provider: v.Provider})
			if post.Episode != nil && *post.Episode == *p.Episode {
				if sourceIndex != nil {
					return zero, wire, errors.New("multiple provider episodes map to the requested storage index")
				}
				sourceIndex = recognition.Int(ep.Index)
			}
		}
		if sourceIndex == nil && p.Mapping == nil && rawExists {
			sourceIndex = recognition.Int(*p.Episode)
		}
		if sourceIndex == nil {
			return zero, wire, errors.New("requested episode has no verified provider episode after filtering")
		}
		req.CurrentEpisodeIndex = sourceIndex
	}
	if v.Year != 0 {
		req.Year = recognition.Int(v.Year)
	}
	if p.Metadata != nil {
		m := p.Metadata
		req.Aliases = metadataAliasFields(*m)
		for k, id := range map[string]string{"tmdbId": m.TMDBID, "imdbId": m.IMDBID, "tvdbId": m.TVDBID, "doubanId": m.DoubanID, "bangumiId": m.BangumiID} {
			if id != "" {
				req.Metadata[k] = id
			}
		}
	}
	return req, wire, nil
}

func (s *Server) searchLibraryRecognized(ctx context.Context, keyword string) ([]store.Row, *recognitionSearch, error) {
	p, e := s.prepareSearch(ctx, keyword)
	if e != nil {
		return nil, nil, e
	}
	parsed := recognition.ParseSearchKeyword(keyword)
	type lookup struct {
		title  string
		season *int
	}
	lookups := []lookup{{parsed.Title, parsed.Season}, {p.Title, p.Season}}
	for _, title := range p.Aliases {
		lookups = append(lookups, lookup{title, p.Season})
	}
	if p.Mapping != nil {
		lookups = append(lookups, lookup{p.Mapping.RecognitionTitle, parsed.Season})
	}
	contexts := []string{"__unscoped__"}
	for _, rule := range p.rules.Items {
		if rule.Provider != "" && rule.Provider != "all" {
			contexts = append(contexts, rule.Provider)
		}
	}
	for _, source := range uniqueTitles(contexts...) {
		post := p.rules.Postprocess(recognition.Input{Text: p.Title, Season: p.Season, Provider: source})
		if post.Changed {
			season := post.Season
			if value := str(post.Metadata["s"]); value != "" {
				if n, err := strconv.Atoi(value); err == nil {
					season = recognition.Int(n)
				}
			}
			lookups = append(lookups, lookup{post.Text, season})
			if title := str(post.Metadata["title"]); title != "" {
				lookups = append(lookups, lookup{title, season})
			}
		}
	}
	out := []store.Row{}
	seen := map[string]bool{}
	seenLookup := map[string]bool{}
	for _, q := range lookups {
		key := normalizedTitle(q.title) + "|" + recognitionSeasonKey(q.season)
		if strings.TrimSpace(q.title) == "" || seenLookup[key] {
			continue
		}
		seenLookup[key] = true
		rows, e := s.localSearch(ctx, q.title)
		if e != nil {
			return nil, nil, e
		}
		for _, row := range rows {
			if seen[str(row["id"])] || q.season != nil && number(row["season"]) != int64(*q.season) {
				continue
			}
			out = append(out, row)
			seen[str(row["id"])] = true
		}
	}
	return out, p, nil
}

func recognitionSeasonKey(season *int) string {
	if season == nil {
		return "?"
	}
	return strconv.Itoa(*season)
}

func metadataAliasFields(m integration.Metadata) map[string]*string {
	out := map[string]*string{"nameEn": recognition.String(m.NameEn), "nameJp": recognition.String(m.NameJp), "nameRomaji": recognition.String(m.NameRomaji)}
	for i, v := range m.AliasesCn {
		if i >= 3 {
			break
		}
		out[fmt.Sprintf("aliasCn%d", i+1)] = recognition.String(v)
	}
	return out
}

// Metadata and aliases change together. Automatic enrichment fills aliases only;
// locked/user values win.
func (s *Server) storeRecognitionMetadata(ctx context.Context, animeID int64, req ImportRequest) error {
	return s.libTransaction(ctx, func(tx *sql.Tx) error { return s.storeRecognitionMetadataTx(ctx, tx, animeID, req) })
}
func (s *Server) storeRecognitionMetadataTx(ctx context.Context, tx *sql.Tx, animeID int64, req ImportRequest) error {
	keys := map[string]string{"tmdbId": "tmdb_id", "imdbId": "imdb_id", "tvdbId": "tvdb_id", "bangumiId": "bangumi_id", "doubanId": "douban_id", "tmdbEpisodeGroupId": "tmdb_episode_group_id"}
	row := store.Row{}
	for key, value := range req.Metadata {
		if column, ok := keys[key]; ok && value != "" {
			row[column] = value
		}
	}
	if len(row) > 0 {
		rows, e := s.libRows(ctx, tx, "anime_metadata", "SELECT * FROM "+s.Store.Quote("anime_metadata")+" WHERE anime_id=?", animeID)
		if e != nil {
			return e
		}
		if len(rows) > 0 {
			e = s.libUpdate(ctx, tx, "anime_metadata", number(rows[0]["id"]), row)
		} else {
			row["anime_id"] = animeID
			_, e = s.Store.InsertTx(ctx, tx, "anime_metadata", row)
		}
		if e != nil {
			return e
		}
	}
	if len(req.Aliases) == 0 {
		return nil
	}
	rows, e := s.libRows(ctx, tx, "anime_aliases", "SELECT * FROM "+s.Store.Quote("anime_aliases")+" WHERE anime_id=?", animeID)
	if e != nil {
		return e
	}
	if len(rows) > 0 && boolean(rows[0]["alias_locked"]) {
		return nil
	}
	fields := map[string]string{"nameEn": "name_en", "nameJp": "name_jp", "nameRomaji": "name_romaji", "aliasCn1": "alias_cn_1", "aliasCn2": "alias_cn_2", "aliasCn3": "alias_cn_3"}
	row = store.Row{}
	for key, value := range req.Aliases {
		column, ok := fields[key]
		if !ok || value == nil || strings.TrimSpace(*value) == "" {
			continue
		}
		if len(rows) > 0 && str(rows[0][column]) != "" {
			continue
		}
		row[column] = *value
	}
	if len(row) == 0 {
		return nil
	}
	if len(rows) > 0 {
		return s.libUpdate(ctx, tx, "anime_aliases", number(rows[0]["id"]), row)
	}
	row["anime_id"] = animeID
	_, e = s.Store.InsertTx(ctx, tx, "anime_aliases", row)
	return e
}

// Metadata I/O belongs only to the actual import, never storage previews.
func (s *Server) enrichRecognitionImport(ctx context.Context, req ImportRequest) (ImportRequest, []string) {
	warnings := []string{}
	if id := req.Metadata["tmdbId"]; id != "" && s.Metadata != nil && s.setting(ctx, "tmdbApiKey", "") != "" {
		typ := "tv"
		if req.Type == "movie" {
			typ = "movie"
		}
		metadata, e := s.Metadata.Details(ctx, "tmdb", id, typ, integration.Credential{})
		if e != nil {
			warnings = append(warnings, e.Error())
		} else if metadata != nil {
			if req.ImageURL == "" {
				req.ImageURL = metadata.ImageURL
			}
			if req.Year == nil && metadata.Year != 0 {
				req.Year = recognition.Int(metadata.Year)
			}
			for key, value := range map[string]string{"imdbId": metadata.IMDBID, "tvdbId": metadata.TVDBID} {
				if req.Metadata[key] == "" && value != "" {
					req.Metadata[key] = value
				}
			}
			if req.Aliases == nil {
				req.Aliases = map[string]*string{}
			}
			for key, value := range metadataAliasFields(*metadata) {
				if _, explicit := req.Aliases[key]; !explicit && value != nil {
					req.Aliases[key] = value
				}
			}
		}
	}

	return req, warnings
}
