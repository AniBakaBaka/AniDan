// SPDX-License-Identifier: AGPL-3.0-only
package server

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/AniBakaBaka/AniDan/internal/integration"
	"github.com/AniBakaBaka/AniDan/internal/job"
	"github.com/AniBakaBaka/AniDan/internal/provider"
	"github.com/AniBakaBaka/AniDan/internal/recognition"
	"github.com/AniBakaBaka/AniDan/internal/store"
)

var errMediaAcquisitionIdentity = errors.New("media acquisition identity changed")
var errMediaEpisodeUnavailable = errors.New("no requested episodes remain after filtering")

// Media coordinates are storage coordinates. This private plan cannot be
// enabled by an API payload; it preserves the source season for projection and
// the explicit media identity for persistence and deletion linkage.
type mediaImportProjection struct {
	origin                store.Row
	source                store.Row
	published             []store.Row
	rules                 *recognition.Rules
	title, mediaType      string
	season, sourceSeason  int
	episode, year         *int
	explicitSeason        bool
	groupCandidate, group *mediaEpisodeGroupSnapshot
	groupError            error
	deferredMetadata      *ImportRequest
	groupIntent           *mediaGroupIntent
	groupTarget           *mediaGroupTargetSnapshot
	listingProof          *mediaListingProof
}

func (p *mediaImportProjection) project(ep ImportEpisode, providerName, sourceTitle string) (ImportEpisode, error) {
	if ep.ID == "" || ep.Index < 0 {
		return ep, errors.New("media source episode identity is invalid")
	}
	out := p.rules.Postprocess(recognition.Input{Text: sourceTitle, Provider: providerName, Season: recognition.Int(p.sourceSeason), Episode: recognition.Int(ep.Index)})
	if len(out.Warnings) > 0 {
		return ep, fmt.Errorf("media episode projection failed: %s", out.Warnings[0].String())
	}
	if out.Episode == nil || *out.Episode < 0 {
		return ep, errors.New("media episode projection has no valid storage index")
	}
	ep.Index = *out.Episode
	return ep, nil
}

// Selection returns ordered equivalent candidates rather than one chosen source,
// so a candidate-local listing/download failure can still use configured fallback.
func (s *Server) prepareMediaCandidates(ctx context.Context, row store.Row) ([]provider.SearchResult, *recognitionSearch, error) {
	declared := recognition.Parsed{Title: strings.TrimSpace(str(row["title"]))}
	if row["season"] != nil {
		declared.Season = recognition.Int(int(number(row["season"])))
	}
	if row["episode"] != nil {
		declared.Episode = recognition.Int(int(number(row["episode"])))
	}
	p, e := s.prepareSearchWithIdentity(ctx, str(row["title"]), &declared)
	if e != nil {
		return nil, nil, e
	}
	if p.Metadata != nil {
		for key, col := range map[string]string{"tmdbId": "tmdb_id", "tvdbId": "tvdb_id", "imdbId": "imdb_id"} {
			inferred := mediaMetadataIDs(*p.Metadata)[key]
			if explicit := str(row[col]); explicit != "" && inferred != "" && explicit != inferred {
				return nil, p, errors.New("name conversion metadata conflicts with the explicit media identity")
			}
		}
	}
	// Compare complete storage projections below; source seasons can differ from
	// requested seasons and adapters can return an unspecified zero season.
	searchSeason := p.Season
	p.Season = nil
	results, searchErr := s.searchRecognition(ctx, p)
	p.Season = searchSeason
	if searchErr != nil {
		if len(results) == 0 {
			return nil, p, searchErr
		}
		p.Warnings = append(p.Warnings, searchErr.Error())
	}
	if len(results) > 1000 {
		return nil, p, errors.New("media acquisition search exceeds 1000 candidates; narrow the title")
	}
	rows, e := s.Store.List(ctx, "scrapers", nil, 1000, 0)
	if e != nil {
		return nil, p, e
	}
	sort.SliceStable(rows, func(i, j int) bool { return number(rows[i]["display_order"]) < number(rows[j]["display_order"]) })
	priority := map[string]int{}
	for i, v := range rows {
		priority[str(v["provider_name"])] = i
	}
	sort.SliceStable(results, func(i, j int) bool { return priority[results[i].Provider] < priority[results[j].Provider] })
	titles := uniqueTitles(append([]string{str(row["title"]), p.Title}, p.Aliases...)...)
	matchesTitle := func(title string) bool {
		for _, known := range titles {
			if normalizedTitle(title) == normalizedTitle(known) {
				return true
			}
		}
		return false
	}
	type choice struct {
		result          provider.SearchResult
		projected       ImportRequest
		exact, favorite bool
	}
	options := []choice{}
	wire := []map[string]any{}
	targetType := str(row["media_type"])
	if targetType == "tv" {
		targetType = "tv_series"
	}
	aiEnabled := strings.EqualFold(s.setting(ctx, "aiMatchEnabled", "false"), "true")
	for _, v := range results {
		if err := job.Checkpoint(ctx); err != nil {
			return nil, p, err
		}
		if v.ID == "" || v.Provider == "" {
			continue
		}
		if p.Mapping != nil && p.Mapping.Source != "" && p.Mapping.Source != "all" && p.Mapping.Source != v.Provider {
			continue
		}
		if p.Mapping != nil && p.Mapping.SearchSeason != nil && v.Season != *p.Mapping.SearchSeason {
			continue
		}
		projected, warnings, err := prepareStorageWithRules(ImportRequest{Provider: v.Provider, MediaID: v.ID, Title: v.Title, Type: v.Type, Season: v.Season}, p.rules, nil)
		if err != nil {
			return nil, p, err
		}
		if len(warnings) > 0 {
			return nil, p, errors.New("media storage projection has invalid recognition rules")
		}
		if projected.Type == "tv" {
			projected.Type = "tv_series"
		}
		if targetType != "" && projected.Type != "" && projected.Type != targetType {
			continue
		}
		if row["season"] != nil && projected.Season != int(number(row["season"])) && !(v.Season == 0 && projected.Season == 0) {
			continue
		}
		if row["year"] != nil && v.Year > 0 && v.Year != int(number(row["year"])) {
			continue
		}
		exact := matchesTitle(v.Title) || matchesTitle(projected.Title)
		if hint := p.rules.HintForResult(v.Title, v.Provider); hint != nil && matchesTitle(str(hint["recognition_title"])) {
			exact = true
		}
		if !exact && !aiEnabled {
			continue
		}
		existing, err := s.Store.List(ctx, "anime_sources", store.Row{"provider_name": v.Provider, "media_id": v.ID}, 1000, 0)
		if err != nil {
			return nil, p, err
		}
		favorite := false
		for _, src := range existing {
			if str(src["provider_name"]) == v.Provider && str(src["media_id"]) == v.ID {
				favorite = favorite || boolean(src["is_favorited"])
			}
		}
		options = append(options, choice{v, projected, exact, favorite})
		item := p.result(v)
		item["exactTitleMatch"], item["isFavorited"] = exact, favorite
		item["inLibrary"] = len(existing) > 0
		wire = append(wire, item)
	}
	chosen := -1
	for i, c := range options {
		if c.exact && c.favorite {
			if chosen >= 0 {
				return nil, p, errors.New("multiple favorited media sources match; manual selection required")
			}
			chosen = i
		}
	}
	if chosen < 0 && len(options) > 0 && aiEnabled {
		if s.Metadata == nil {
			return nil, p, errors.New("configured AI matching client unavailable")
		}
		decision, err := s.Metadata.SelectMatch(ctx, map[string]any{"title": row["title"], "season": row["season"], "episode": row["episode"], "year": row["year"], "type": targetType}, wire)
		if err != nil {
			if !strings.EqualFold(s.setting(ctx, "aiFallbackEnabled", "true"), "true") {
				return nil, p, err
			}
			p.Warnings = append(p.Warnings, err.Error())
		} else if decision != nil && decision.Confidence >= 80 && decision.Index >= 0 && decision.Index < len(options) {
			chosen = decision.Index
		}
	}
	// Aliases explicitly supplied by the recognition plan identify this requested
	// work. An AI-only nonexact title has its own projected identity; fallback does
	// not reopen other unproven titles merely because a selection was made.
	identity := func(c choice) string {
		title := normalizedTitle(c.projected.Title)
		if matchesTitle(c.projected.Title) {
			title = normalizedTitle(str(row["title"]))
		}
		typ := c.projected.Type
		if typ == "" {
			typ = targetType
		}
		season := c.projected.Season
		if row["season"] != nil && season == 0 {
			season = int(number(row["season"]))
		}
		return fmt.Sprintf("%s\x00%s\x00%d", title, typ, season)
	}
	eligible := []int{}
	knownYear := 0
	key := ""
	if chosen >= 0 {
		eligible = append(eligible, chosen)
		key = identity(options[chosen])
		knownYear = options[chosen].result.Year
	}
	ambiguousFallbackYears := false
	if chosen >= 0 && knownYear == 0 {
		years := map[int]bool{}
		for i, c := range options {
			if i != chosen && c.exact && identity(c) == key && c.result.Year > 0 {
				years[c.result.Year] = true
			}
		}
		ambiguousFallbackYears = len(years) > 1
		if ambiguousFallbackYears {
			p.Warnings = append(p.Warnings, "conflicting known fallback years excluded for an undated selected source")
		}
	}
	for i, c := range options {
		if ambiguousFallbackYears && c.result.Year > 0 {
			continue
		}
		if i == chosen {
			continue
		}
		if !c.exact {
			continue
		}
		if chosen >= 0 && c.result.Provider == options[chosen].result.Provider {
			continue
		}
		if key != "" && identity(c) != key {
			if chosen >= 0 {
				continue
			}
			return nil, p, errors.New("media candidates have competing storage identities; manual selection required")
		}
		if key == "" {
			key = identity(c)
		}
		if c.result.Year > 0 {
			if knownYear > 0 && c.result.Year != knownYear {
				if chosen >= 0 {
					continue
				}
				return nil, p, errors.New("media candidates have different release years; manual selection required")
			}
			knownYear = c.result.Year
		}
		eligible = append(eligible, i)
	}
	if len(eligible) == 0 {
		return nil, p, errors.New("no verified title/type/season/year media match; manual search selection is required")
	}
	counts := map[string]int{}
	for _, i := range eligible {
		counts[options[i].result.Provider]++
	}
	out := []provider.SearchResult{}
	for _, i := range eligible {
		c := options[i]
		if counts[c.result.Provider] > 1 {
			if chosen < 0 {
				return nil, p, errors.New("multiple matching media IDs from one provider; manual selection required")
			}
			p.Warnings = append(p.Warnings, c.result.Provider+": ambiguous fallback excluded")
			continue
		}
		out = append(out, c.result)
	}
	return out, p, nil
}

func mediaMetadataIDs(m integration.Metadata) map[string]string {
	return map[string]string{"tmdbId": m.TMDBID, "tvdbId": m.TVDBID, "imdbId": m.IMDBID, "doubanId": m.DoubanID, "bangumiId": m.BangumiID}
}

// Media rows can be edited while a provider is being queried. A committed pool
// keeps its original identity, but changed rows must not receive old linkage or
// a successful imported flag. No whole-operation rollback is implied.
func (s *Server) validateMediaSnapshot(ctx context.Context, tx *sql.Tx, expected store.Row) error {
	if number(expected["id"]) <= 0 {
		return nil
	}
	query := "SELECT * FROM " + s.Store.Quote("media_items") + " WHERE id=?"
	if s.Store.Dialect != "sqlite" {
		query += " FOR UPDATE"
	}
	rows, e := s.libRows(ctx, tx, "media_items", query, expected["id"])
	if e != nil {
		return e
	}
	if len(rows) != 1 {
		return fmt.Errorf("%w: media item was removed; committed pools require review", errMediaAcquisitionIdentity)
	}
	for _, key := range []string{"server_id", "media_id", "title", "media_type", "season", "episode", "year", "tmdb_id", "tvdb_id", "imdb_id", "series_id", "season_id", "episode_id"} {
		if (rows[0][key] == nil) != (expected[key] == nil) || str(rows[0][key]) != str(expected[key]) {
			return fmt.Errorf("%w: media item field %s; committed pools require review", errMediaAcquisitionIdentity, key)
		}
	}
	return nil
}
func (s *Server) finishMediaAcquisition(ctx context.Context, row store.Row, plan *mediaImportProjection, animeID, sourceID int64) error {
	if plan.source == nil || number(plan.source["id"]) != sourceID || number(plan.source["anime_id"]) != animeID {
		return errors.New("import result source identity is missing")
	}
	ctx = mediaGroupPlanContext(ctx, plan)
	if plan.listingProof != nil {
		captured, _ := s.Providers.Get(plan.listingProof.name)
		if err := s.validateMediaListingRouting(ctx, plan.listingProof, plan.source, captured); err != nil {
			return err
		}
	}
	serverType := str(row["media_server_type"])
	if serverType == "" && plan.origin != nil {
		serverType = str(plan.origin["provider_name"])
	}

	return s.libTransaction(ctx, func(tx *sql.Tx) error {
		if e := s.validateSourceSnapshot(ctx, tx, plan.source); e != nil {
			return e
		}
		if e := s.validateMediaGroupContext(ctx, tx); e != nil {
			return e
		}
		if e := s.validateMediaOrigin(ctx, tx, plan.origin); e != nil {
			return e
		}
		if e := s.validateMediaSnapshot(ctx, tx, row); e != nil {
			return e
		}
		markImported := func() error {
			if number(row["id"]) <= 0 {
				return nil
			}
			return s.libUpdate(ctx, tx, "media_items", row["id"], store.Row{"is_imported": true, "updated_at": s.now()})
		}
		for _, ep := range plan.published {
			if e := s.validateDownloadIdentity(ctx, tx, ep, plan.source); e != nil {
				return e
			}
		}
		if plan.deferredMetadata != nil {
			anime, err := s.libOne(ctx, tx, "anime", "id=?", animeID)
			if err != nil {
				return err
			}
			if err = s.validateMediaLibraryTarget(ctx, tx, anime, *plan.deferredMetadata); err != nil {
				return err
			}
			if err = s.storeRecognitionMetadataTx(ctx, tx, animeID, *plan.deferredMetadata); err != nil {
				return err
			}
		}
		if serverType == "" {
			return markImported()
		}
		metadata, e := s.libOne(ctx, tx, "anime_metadata", "anime_id=?", animeID)
		if e != nil && !errors.Is(e, sql.ErrNoRows) {
			return e
		}
		changes := store.Row{"media_server_type": serverType}
		if str(row["series_id"]) != "" {
			changes["media_server_series_id"] = str(row["series_id"])
		}
		if str(row["season_id"]) != "" {
			changes["media_server_season_id"] = str(row["season_id"])
		}
		if metadata != nil {
			if e = s.libUpdate(ctx, tx, "anime_metadata", metadata["id"], changes); e != nil {
				return e
			}
		} else {
			changes["anime_id"] = animeID
			if _, e = s.Store.InsertTx(ctx, tx, "anime_metadata", changes); e != nil {
				return e
			}
		}
		if str(row["episode_id"]) != "" && plan.episode != nil {
			matches := []store.Row{}
			for _, ep := range plan.published {
				if number(ep["episode_index"]) == int64(*plan.episode) {
					matches = append(matches, ep)
				}
			}
			if len(matches) != 1 {
				return errors.New("committed media episode identity is not unique; no linkage was changed")
			}
			if e = s.libUpdate(ctx, tx, "episode", matches[0]["id"], store.Row{"media_server_episode_id": str(row["episode_id"])}); e != nil {
				return e
			}
		}
		return markImported()
	})
}

func (s *Server) validateMediaLibraryTarget(ctx context.Context, q libQueryer, anime store.Row, req ImportRequest) error {
	if anime == nil {
		return errors.New("media library target is missing")
	}
	if req.Year != nil && *req.Year > 0 && number(anime["year"]) > 0 && number(anime["year"]) != int64(*req.Year) {
		return fmt.Errorf("%w: existing library target has a different release year; manual selection required", errMediaAcquisitionIdentity)
	}
	metadata, e := s.libOne(ctx, q, "anime_metadata", "anime_id=?", anime["id"])
	if errors.Is(e, sql.ErrNoRows) {
		return nil
	}
	if e != nil {
		return e
	}
	for key, col := range map[string]string{"tmdbId": "tmdb_id", "tvdbId": "tvdb_id", "imdbId": "imdb_id", "doubanId": "douban_id", "bangumiId": "bangumi_id"} {
		if incoming := req.Metadata[key]; incoming != "" && str(metadata[col]) != "" && incoming != str(metadata[col]) {
			return fmt.Errorf("%w: existing library target has a conflicting metadata identity; manual selection required", errMediaAcquisitionIdentity)
		}
	}
	return nil
}

func (s *Server) captureMediaOrigin(ctx context.Context, row store.Row) (store.Row, error) {
	var origin store.Row
	err := s.libTransaction(ctx, func(tx *sql.Tx) error {
		if number(row["server_id"]) <= 0 {
			return s.validateMediaSnapshot(ctx, tx, row)
		}
		query := "SELECT * FROM " + s.Store.Quote("media_servers") + " WHERE id=?"
		if s.Store.Dialect != "sqlite" {
			query += " FOR UPDATE"
		}
		rows, e := s.libRows(ctx, tx, "media_servers", query, row["server_id"])
		if e != nil {
			return e
		}
		if len(rows) != 1 {
			return fmt.Errorf("%w: media server removed", errMediaAcquisitionIdentity)
		}
		origin = rows[0]
		return s.validateMediaSnapshot(ctx, tx, row)
	})
	return origin, err
}
func (s *Server) validateMediaOrigin(ctx context.Context, tx *sql.Tx, expected store.Row) error {
	if expected == nil {
		return nil
	}
	query := "SELECT * FROM " + s.Store.Quote("media_servers") + " WHERE id=?"
	if s.Store.Dialect != "sqlite" {
		query += " FOR UPDATE"
	}
	rows, e := s.libRows(ctx, tx, "media_servers", query, expected["id"])
	if e != nil {
		return e
	}
	if len(rows) != 1 {
		return fmt.Errorf("%w: media server removed; committed pools require review", errMediaAcquisitionIdentity)
	}
	for _, key := range []string{"provider_name", "url", "api_token"} {
		if str(rows[0][key]) != str(expected[key]) {
			return fmt.Errorf("%w: media server configuration changed; committed pools require review", errMediaAcquisitionIdentity)
		}
	}
	return nil
}
