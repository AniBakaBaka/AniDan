// SPDX-License-Identifier: AGPL-3.0-only
package server

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/AniBakaBaka/AniDan/internal/job"
	"github.com/AniBakaBaka/AniDan/internal/provider"
	"github.com/AniBakaBaka/AniDan/internal/recognition"
	"github.com/AniBakaBaka/AniDan/internal/store"
)

// predownloadCandidate projects source indices forward, then checks the current
// provider-ID anchor. It never guesses an inverse for arithmetic/range rules.
// A stored next row is authoritative, but its exact ID must survive the filters.
func predownloadCandidate(ctx context.Context, rules *recognition.Rules, eps []provider.Episode, current, next, anime store.Row, name string) (*provider.Episode, error) {
	if len(eps) > 10000 {
		return nil, errors.New("predownload episode listing exceeds 10000")
	}
	anchors := []provider.Episode{}
	ids := map[string]bool{}
	for _, e := range eps {
		if e.ID == "" || len(e.ID) > 2048 || ids[e.ID] {
			return nil, errors.New("ambiguous or invalid provider episode identity")
		}
		ids[e.ID] = true
		if e.ID == str(current["provider_episode_id"]) {
			anchors = append(anchors, e)
		}
	}
	if len(anchors) != 1 {
		return nil, errors.New("current provider episode is absent or excluded")
	}
	if next != nil {
		for _, e := range eps {
			if e.ID == str(next["provider_episode_id"]) {
				if e.ID == anchors[0].ID {
					return nil, errors.New("next episode repeats current provider identity")
				}
				v := e
				return &v, nil
			}
		}
		return nil, errors.New("stored next provider episode is absent or excluded")
	}
	title := str(anime["title"])
	season := int(number(anime["season"]))
	titles := []string{title}
	seasons := []int{season}
	addTitle := func(v string) {
		if v == "" || len(v) > 2000 {
			return
		}
		for _, old := range titles {
			if old == v {
				return
			}
		}
		titles = append(titles, v)
	}
	addSeason := func(v int) {
		if v < 0 || v > 1000000 {
			return
		}
		for _, old := range seasons {
			if old == v {
				return
			}
		}
		seasons = append(seasons, v)
	}
	for _, rule := range rules.Items {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if len(titles) > 32 || len(seasons) > 32 {
			return nil, errors.New("predownload recognition mapping exceeds bounded candidates")
		}
		if rule.Stage != "postprocess" || (rule.Provider != "" && rule.Provider != "all" && rule.Provider != name) {
			continue
		}
		addTitle(rule.Source)
		if offset, ok := rule.Metadata["season_offset"].(string); ok {
			if strings.HasPrefix(offset, "*+") {
				if n, e := strconv.Atoi(offset[2:]); e == nil {
					addSeason(season - n)
				}
			} else if strings.HasPrefix(offset, "*-") {
				if n, e := strconv.Atoi(offset[2:]); e == nil {
					addSeason(season + n)
				}
			} else {
				i := strings.IndexAny(offset, "+->")
				if i > 0 {
					if n, e := strconv.Atoi(offset[:i]); e == nil {
						addSeason(n)
					}
				}
			}
		}
	}
	if len(titles) > 32 || len(seasons) > 32 {
		return nil, errors.New("predownload recognition mapping exceeds bounded candidates")
	}
	budget := 2_000_000
	apply := func(in recognition.Input) (recognition.Result, error) {
		if err := ctx.Err(); err != nil {
			return recognition.Result{}, err
		}
		budget -= max(1, len(rules.Items))
		if budget < 0 {
			return recognition.Result{}, errors.New("predownload recognition work budget exceeded")
		}
		return rules.Postprocess(in), nil
	}
	target := number(current["episode_index"]) + 1
	found := map[string]provider.Episode{}
	for _, rawTitle := range titles {
		for _, rawSeason := range seasons {
			anchor, applyErr := apply(recognition.Input{Text: rawTitle, Season: recognition.Int(rawSeason), Episode: recognition.Int(anchors[0].Index), Provider: name})
			if applyErr != nil {
				return nil, applyErr
			}
			matches := func(p recognition.Result, index int64) bool {
				t := p.Text
				if v := str(p.Metadata["title"]); v != "" {
					t = v
				}
				sn := p.Season
				if v := str(p.Metadata["s"]); v != "" {
					n, e := strconv.Atoi(v)
					if e != nil {
						return false
					}
					sn = recognition.Int(n)
				}
				return len(p.Warnings) == 0 && t == title && sn != nil && *sn == season && p.Episode != nil && int64(*p.Episode) == index
			}
			if !matches(anchor, number(current["episode_index"])) {
				continue
			}
			count := 0
			var selected provider.Episode
			for _, candidate := range eps {
				p, applyErr := apply(recognition.Input{Text: rawTitle, Season: recognition.Int(rawSeason), Episode: recognition.Int(candidate.Index), Provider: name})
				if applyErr != nil {
					return nil, applyErr
				}
				if matches(p, target) {
					count++
					selected = candidate
				}
			}
			if count > 1 {
				return nil, errors.New("multiple source episodes map to next storage index")
			}
			if count == 1 {
				found[selected.ID] = selected
			}
		}
	}
	if len(found) > 1 {
		return nil, errors.New("recognition mappings disagree on next episode")
	}
	for _, v := range found {
		if v.ID == anchors[0].ID {
			return nil, errors.New("next mapping repeats current episode")
		}
		return &v, nil
	}
	return nil, nil
}
func (s *Server) resolvePredownloadEpisode(ctx context.Context, current, src, anime store.Row) (store.Row, error) {
	nextIndex := number(current["episode_index"]) + 1
	rows, err := s.Store.List(ctx, "episode", store.Row{"source_id": src["id"], "episode_index": nextIndex}, 2, 0)
	if err != nil {
		return nil, err
	}
	if len(rows) > 1 {
		return nil, errors.New("duplicate next storage episode index")
	}
	var next store.Row
	if len(rows) == 1 {
		next = rows[0]
		comments, e := s.readComments(ctx, next)
		if e == nil && len(comments) > 0 {
			return next, nil
		}
	}
	if next == nil {
		meta, e := s.Store.List(ctx, "anime_metadata", store.Row{"anime_id": anime["id"]}, 1, 0)
		if e != nil {
			return nil, e
		}
		if len(meta) > 0 && str(meta[0]["tmdb_episode_group_id"]) != "" {
			return nil, errors.New("missing next episode has custom grouping; explicit import is required")
		}
		if str(anime["type"]) == "tv_series" {
			group, e := s.loadMediaEpisodeGroup(ctx, anime, "", int(number(anime["season"])))
			if e != nil {
				return nil, mediaGroupVerificationError(ctx, e)
			}
			if group != nil {
				return nil, errors.New("missing next episode has inherited custom grouping; explicit import is required")
			}
		}
	}
	rules, warnings, err := s.loadRecognition(ctx)
	if err != nil {
		return nil, err
	}
	if len(warnings) > 0 {
		return nil, errors.New("invalid recognition rules prevent safe predownload")
	}
	eps, _, err := s.sourceEpisodes(ctx, str(src["provider_name"]), str(src["media_id"]))
	if err != nil {
		return nil, err
	}
	// Use stored aliases only: speculative download must not initiate unrelated
	// metadata/AI searches simply to infer a missing source title.
	aliases := []string{str(anime["title"])}
	ar, err := s.Store.List(ctx, "anime_aliases", store.Row{"anime_id": anime["id"]}, 1, 0)
	if err != nil {
		return nil, err
	}
	if len(ar) > 0 {
		for _, k := range []string{"name_en", "name_jp", "name_romaji", "alias_cn_1", "alias_cn_2", "alias_cn_3"} {
			if v := str(ar[0][k]); v != "" {
				aliases = append(aliases, v)
			}
		}
	}
	eps, err = s.filterEpisodes(ctx, eps, str(anime["title"]), str(src["provider_name"]), str(src["media_id"]), aliases)
	if err != nil {
		return nil, err
	}
	chosen, err := predownloadCandidate(ctx, rules, eps, current, next, anime, str(src["provider_name"]))
	if err != nil || chosen == nil {
		return nil, err
	}
	if next != nil {
		return next, nil
	}
	if err = job.Checkpoint(ctx); err != nil {
		return nil, err
	}
	id, err := episodeIdentifier(number(anime["id"]), number(src["source_order"]), nextIndex)
	if err != nil {
		return nil, err
	}
	err = s.libTransaction(ctx, func(tx *sql.Tx) error {
		if e := s.validateDownloadIdentity(ctx, tx, current, src); e != nil {
			return e
		}
		sourceRows, e := s.libRows(ctx, tx, "anime_sources", "SELECT * FROM anime_sources WHERE id=?", src["id"])
		if e != nil {
			return e
		}
		if len(sourceRows) != 1 || boolean(sourceRows[0]["is_finished"]) {
			return errors.New("source finished during next-episode resolution")
		}
		existing, e := s.libRows(ctx, tx, "episode", "SELECT * FROM episode WHERE source_id=? AND episode_index=?", src["id"], nextIndex)
		if e != nil {
			return e
		}
		if len(existing) > 0 {
			if len(existing) != 1 || str(existing[0]["provider_episode_id"]) != chosen.ID {
				return errors.New("next episode changed during resolution")
			}
			next = existing[0]
			return nil
		}
		collisions, e := s.libRows(ctx, tx, "episode", "SELECT * FROM episode WHERE id=?", id)
		if e != nil {
			return e
		}
		if len(collisions) > 0 {
			return fmt.Errorf("next episode ID %d belongs to another record", id)
		}
		next = store.Row{"id": id, "source_id": src["id"], "episode_index": nextIndex, "provider_episode_id": chosen.ID, "title": chosen.Title, "source_url": chosen.URL, "comment_count": 0}
		_, e = s.Store.InsertTx(ctx, tx, "episode", next)
		return e
	})
	if err != nil {
		return nil, err
	}
	return s.Store.Get(ctx, "episode", next["id"])
}
