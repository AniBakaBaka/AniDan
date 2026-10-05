// SPDX-License-Identifier: AGPL-3.0-only
// Bounded season correction based on pinned Misaka tasks/webhook.py and
// utils/season_mapper.py. Identity comes from the event's explicit TMDB ID.
package server

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/AniBakaBaka/AniDan/internal/integration"
	"github.com/AniBakaBaka/AniDan/internal/job"
	"github.com/AniBakaBaka/AniDan/internal/media"
	"github.com/AniBakaBaka/AniDan/internal/provider"
	"github.com/AniBakaBaka/AniDan/internal/recognition"
	"github.com/AniBakaBaka/AniDan/internal/store"
)

type webhookSeasonCandidate struct {
	Result             provider.SearchResult
	OriginalSeason     int
	Method             string
	Favorite, Existing bool
}

func webhookSpinoff(title string) bool {
	for _, word := range []string{"外传", "外傳", "外伝", "番外", "特别篇", "特別篇", "特別編", "剧场版", "劇場版", "spin-off", "spinoff", "side story", "gaiden"} {
		if strings.Contains(strings.ToLower(title), strings.ToLower(word)) {
			return true
		}
	}
	fields := strings.FieldsFunc(strings.ToLower(title), func(r rune) bool {
		return r == ' ' || r == '-' || r == '_' || r == ':' || r == '(' || r == ')' || r == '[' || r == ']'
	})
	for _, field := range fields {
		switch field {
		case "special", "movie", "film", "ova", "oad", "sp":
			return true
		}
	}
	return false
}
func genericSeasonName(name string, season int) bool {
	key := normalizedTitle(name)
	return key == normalizedTitle(fmt.Sprintf("Season %d", season)) || key == normalizedTitle(fmt.Sprintf("第%d季", season)) || key == strings.ToLower(fmt.Sprintf("S%02d", season)) || key == "specials" || key == "特别篇" || key == "特別篇"
}
func (s *Server) correctWebhookSeason(ctx context.Context, item provider.SearchResult, meta *integration.Metadata) (int, string, bool, error) {
	if item.Type != "tv_series" {
		return item.Season, "", false, nil
	}
	names := metadataTitles(*meta)
	parsed := recognition.ParseSearchKeyword(item.Title)
	baseMatch := false
	for _, name := range names {
		if normalizedTitle(parsed.Title) == normalizedTitle(name) {
			baseMatch = true
			break
		}
	}
	// Preserve explicit source seasons. An unrelated title with a season suffix
	// is not thereby evidence that it belongs to this TMDB series.
	if parsed.Season != nil && *parsed.Season == item.Season {
		identity := baseMatch
		for _, season := range meta.Seasons {
			if season.SeasonNumber != item.Season {
				continue
			}
			for _, alias := range append([]string{season.Name}, season.Aliases...) {
				if normalizedTitle(parsed.Title) == normalizedTitle(alias) {
					identity = true
				}
			}
		}
		return item.Season, "explicit source season", identity, nil
	}
	if webhookSpinoff(item.Title) {
		return item.Season, "spinoff preserved", false, nil
	}
	matching := map[int]bool{}
	for _, season := range meta.Seasons {
		for _, name := range append([]string{season.Name}, season.Aliases...) {
			if strings.TrimSpace(name) == "" {
				continue
			}
			exact := normalizedTitle(item.Title) == normalizedTitle(name)
			contained := !genericSeasonName(name, season.SeasonNumber) && len([]rune(name)) >= 3 && strings.Contains(normalizedTitle(item.Title), normalizedTitle(name))
			explicit := parsed.Season != nil && *parsed.Season == season.SeasonNumber && baseMatch
			if exact || contained || explicit {
				matching[season.SeasonNumber] = true
			}
		}
	}
	if len(matching) == 1 {
		for season := range matching {
			return season, "TMDB season title", true, nil
		}
	}
	if len(matching) > 1 {
		return item.Season, "", false, errors.New("provider title matches more than one TMDB season")
	}
	if baseMatch {
		for _, season := range meta.Seasons {
			if season.SeasonNumber == item.Season {
				return item.Season, "base title and source season", true, nil
			}
		}
	}
	if !strings.EqualFold(s.setting(ctx, "aiMatchEnabled", "false"), "true") {
		return item.Season, "unresolved", false, nil
	}
	options := []map[string]any{}
	for _, season := range meta.Seasons {
		options = append(options, map[string]any{"title": season.Name, "season": season.SeasonNumber, "type": "tv_series", "aliases": season.Aliases})
	}
	decision, e := s.Metadata.SelectMatch(ctx, map[string]any{"title": item.Title, "type": "tv_series", "tmdbTitle": meta.Title, "purpose": "identify the source title's season; return no match if unrelated or uncertain"}, options)
	if e != nil {
		return item.Season, "", false, e
	}
	if decision == nil || decision.Confidence < 80 {
		return item.Season, "unresolved", false, nil
	}
	return meta.Seasons[decision.Index].SeasonNumber, "configured AI season choice", true, nil
}
func (s *Server) importWebhookSeasonMapped(ctx context.Context, event media.WebhookEvent, progress func(int, string)) (any, error) {
	if e := job.Checkpoint(ctx); e != nil {
		return nil, e
	}
	progress(5, "读取 TMDB 季度信息")
	metadata, e := s.Metadata.Details(ctx, "tmdb", event.TMDBID, "tv", integration.Credential{})
	if e != nil {
		return nil, e
	}
	if metadata == nil || metadata.TMDBID != event.TMDBID {
		return nil, errors.New("TMDB details did not identify the requested series")
	}
	if len(metadata.Seasons) == 0 {
		return nil, errors.New("TMDB returned no seasons for the requested series")
	}
	if len(metadata.Seasons) > 100 {
		return nil, errors.New("TMDB season list exceeds 100")
	}
	plan, e := s.prepareSearch(ctx, event.Title)
	if e != nil {
		return nil, e
	}
	// Source adapters frequently expose season 1 until the metadata correction.
	// Filtering by the target season before correction would discard them all.
	plan.Season = nil
	plan.Aliases = uniqueTitles(append(plan.Aliases, metadataTitles(*metadata)...)...)
	for _, season := range metadata.Seasons {
		if season.SeasonNumber == event.Season && !genericSeasonName(season.Name, season.SeasonNumber) {
			plan.Aliases = uniqueTitles(append([]string{season.Name}, plan.Aliases...)...)
		}
	}
	results, searchErr := s.searchRecognition(ctx, plan)
	if searchErr != nil && len(results) == 0 {
		return nil, searchErr
	}
	warnings := append([]string{}, plan.Warnings...)
	if searchErr != nil {
		warnings = append(warnings, searchErr.Error())
	}
	if len(results) > 100 {
		return nil, errors.New("webhook season mapping search exceeds 100 candidates; narrow the title")
	}
	candidates := []webhookSeasonCandidate{}
	for _, item := range results {
		if e := job.Checkpoint(ctx); e != nil {
			return nil, e
		}
		season, method, identity, e := s.correctWebhookSeason(ctx, item, metadata)
		if e != nil {
			warnings = append(warnings, item.Provider+": "+e.Error())
			continue
		}
		if !identity || season != event.Season {
			continue
		}
		candidate := webhookSeasonCandidate{Result: item, OriginalSeason: item.Season, Method: method}
		candidate.Result.Season = season
		sources, e := s.Store.List(ctx, "anime_sources", store.Row{"provider_name": item.Provider, "media_id": item.ID}, 1000, 0)
		if e != nil {
			return nil, e
		}
		for _, source := range sources {
			candidate.Existing = true
			candidate.Favorite = candidate.Favorite || boolean(source["is_favorited"])
		}
		candidates = append(candidates, candidate)
	}
	if len(candidates) == 0 {
		return nil, fmt.Errorf("no verified provider title maps to TMDB season %d; %s", event.Season, strings.Join(warnings, "; "))
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].Favorite != candidates[j].Favorite {
			return candidates[i].Favorite
		}
		if candidates[i].Existing != candidates[j].Existing {
			return candidates[i].Existing
		}
		return false
	})
	// Multiple media IDs from one provider are not interchangeable merely because
	// an algorithm assigned the same season. Require explicit selection then.
	groups := map[string][]webhookSeasonCandidate{}
	for _, candidate := range candidates {
		groups[candidate.Result.Provider] = append(groups[candidate.Result.Provider], candidate)
	}
	verified := make([]webhookSeasonCandidate, 0, len(candidates))
	for _, candidate := range candidates {
		group := groups[candidate.Result.Provider]
		if len(group) == 1 {
			verified = append(verified, candidate)
			continue
		}
		favorites := 0
		for _, member := range group {
			if member.Favorite {
				favorites++
			}
		}
		if favorites > 1 || !candidates[0].Favorite {
			return nil, errors.New("multiple titles from one provider map to this season; manual selection required")
		}
		if favorites == 1 && candidate.Favorite {
			verified = append(verified, candidate)
			continue
		}
		// A favorite resolves only its own exact identity. It does not authorize an
		// arbitrary later media ID from this or another provider during fallback.
		warnings = append(warnings, candidate.Result.Provider+": ambiguous mapped fallback excluded")
	}
	candidates = verified
	fallback := strings.EqualFold(s.setting(ctx, "webhookFallbackEnabled", "false"), "true")
	failures := []error{}
	groupIntent := &mediaGroupIntent{}
	for _, candidate := range candidates {
		item := candidate.Result
		row := store.Row{"title": item.Title, "media_type": item.Type, "season": event.Season, "year": nil, "episode": nil, "tmdb_id": event.TMDBID, "tvdb_id": event.TVDBID, "imdb_id": event.IMDBID, "media_server_type": event.Source, "series_id": event.SeriesID, "season_id": event.SeasonID, "episode_id": event.EpisodeID}
		if event.Episode != nil {
			row["episode"] = *event.Episode
		}
		if item.Year > 0 {
			row["year"] = item.Year
		} else if event.Year != nil {
			row["year"] = *event.Year
		}
		progress(25, "导入已映射季度: "+item.Title)
		if e = s.importMediaCandidateWithIntent(ctx, row, item, progress, groupIntent); e == nil {
			return map[string]any{"imported": true, "title": item.Title, "season": event.Season, "provider": item.Provider, "mapping": map[string]any{"tmdbId": event.TMDBID, "originalSeason": candidate.OriginalSeason, "correctedSeason": item.Season, "method": candidate.Method}, "warnings": warnings}, nil
		}
		failures = append(failures, fmt.Errorf("%s: %w", item.Provider, e))
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if errors.Is(e, errMediaAcquisitionIdentity) {
			return nil, errors.Join(failures...)
		}
		if !fallback {
			break
		}
	}
	return nil, errors.Join(failures...)
}
