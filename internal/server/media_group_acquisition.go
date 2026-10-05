// SPDX-License-Identifier: AGPL-3.0-only
package server

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/AniBakaBaka/AniDan/internal/store"
)

type mediaGroupIntent struct{ snapshot *mediaEpisodeGroupSnapshot }

type mediaEpisodeGroupContextKey struct{}
type mediaGroupOwnershipContextKey struct{}

func mediaGroupOwnershipRequired(ctx context.Context) bool {
	v, _ := ctx.Value(mediaGroupOwnershipContextKey{}).(bool)
	return v
}

func mediaEpisodeGroupFromContext(ctx context.Context) *mediaEpisodeGroupSnapshot {
	v, _ := ctx.Value(mediaEpisodeGroupContextKey{}).(*mediaEpisodeGroupSnapshot)
	return v
}

// Capture before the provider listing. A group is only a fallback; invalid or
// changed group data must not change an otherwise unique direct projection.
func (s *Server) captureMediaImportGroup(ctx context.Context, p *mediaImportProjection, req ImportRequest, anime store.Row) {
	if p.mediaType != "tv_series" || !p.explicitSeason || p.episode == nil {
		return
	}
	p.groupCandidate, p.groupError = s.loadMediaEpisodeGroup(ctx, anime, req.Metadata["tmdbId"], p.season)
	if p.groupError != nil {
		p.groupError = mediaGroupVerificationError(ctx, p.groupError)
	}
	if p.groupIntent != nil && p.groupIntent.snapshot != nil {
		expected := p.groupIntent.snapshot
		if p.groupError != nil {
			return
		}
		if p.groupCandidate == nil || p.groupCandidate.groupID != expected.groupID {
			p.groupError = fmt.Errorf("%w: episode group changed between source candidates", errMediaAcquisitionIdentity)
			return
		}
		if err := s.validateMediaEpisodeGroup(ctx, s.Store.DB, expected); err != nil {
			p.groupError = mediaGroupVerificationError(ctx, err)
			return
		}
		// An earlier failed attempt may have created an unpopulated target.
		// Keep the original group evidence, rather than adopting a fresh plan.
		p.groupCandidate = expected
	}
}

// Group coordinates identify a provider episode. They never replace the
// caller's original storage coordinate, source identity, or media-server ID.
func (s *Server) selectMediaImportEpisodes(ctx context.Context, p *mediaImportProjection, req ImportRequest, sourceTitle string, eps []ImportEpisode) ([]ImportEpisode, error) {
	if p.groupIntent != nil && p.groupIntent.snapshot != nil {
		if p.groupError != nil {
			return nil, p.groupError
		}
		if err := s.validateMediaEpisodeGroup(ctx, s.Store.DB, p.groupIntent.snapshot); err != nil {
			return nil, mediaGroupVerificationError(ctx, err)
		}
		p.group = p.groupIntent.snapshot
	}
	projected := make([]ImportEpisode, len(eps))
	selected := []ImportEpisode{}
	for i, ep := range eps {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		v, err := p.project(ep, req.Provider, sourceTitle)
		if err != nil {
			return nil, err
		}
		projected[i] = v
		if p.episode == nil || v.Index == *p.episode {
			selected = append(selected, v)
		}
	}
	if len(selected) != 0 || p.episode == nil {
		if p.groupCandidate != nil {
			counts := make(map[string]int, len(eps))
			for _, ep := range eps {
				counts[ep.ID]++
			}
			for _, ep := range selected {
				if counts[ep.ID] != 1 {
					return nil, errors.New("provider episode identity is repeated in the grouped source listing")
				}
			}
		}
		return selected, nil
	}
	if p.groupError != nil {
		return nil, p.groupError
	}
	if p.groupCandidate == nil {
		return nil, errMediaEpisodeUnavailable
	}
	mapped, ok, err := p.groupCandidate.lookup(*p.episode)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, errors.New("episode group has no mapping for the requested custom episode")
	}
	if mapped.officialSeason != p.sourceSeason {
		return nil, errors.New("episode group official season does not match the eligible source season")
	}
	chosen := -1
	for i, ep := range eps {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if ep.Index == mapped.providerEpisode {
			if chosen >= 0 {
				return nil, errors.New("episode group resolves to multiple source episodes")
			}
			chosen = i
		}
	}
	if chosen < 0 {
		return nil, fmt.Errorf("%w: mapped source episode is missing or filtered", errMediaEpisodeUnavailable)
	}
	for i, ep := range eps {
		if i != chosen && ep.ID == eps[chosen].ID {
			return nil, errors.New("mapped provider episode identity is repeated in the source listing")
		}
	}
	if projected[chosen].Index != eps[chosen].Index {
		return nil, fmt.Errorf("%w: episode group fallback conflicts with a nonidentity episode rule", errMediaAcquisitionIdentity)
	}
	if err := s.validateMediaEpisodeGroup(ctx, s.Store.DB, p.groupCandidate); err != nil {
		return nil, mediaGroupVerificationError(ctx, err)
	}
	p.group = p.groupCandidate
	if p.groupIntent != nil {
		p.groupIntent.snapshot = p.group
	}
	v := eps[chosen]
	v.Index = *p.episode
	return []ImportEpisode{v}, nil
}

// The source-row lock taken by admission/publication serializes this check with
// other grouped admissions. One opaque provider episode cannot silently acquire
// a second grouped storage coordinate, including a direct/group fallback clash.
func (s *Server) validateMediaGroupProviderOwnership(ctx context.Context, tx *sql.Tx, src store.Row, index int64, providerID string) error {
	if !mediaGroupOwnershipRequired(ctx) {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	query := "SELECT id,provider_episode_id,episode_index FROM " + s.Store.Quote("episode") + " WHERE source_id=? AND provider_episode_id=? ORDER BY id LIMIT 100001"
	if s.Store.Dialect != "sqlite" {
		query += " FOR UPDATE"
	}
	rows, err := s.libRows(ctx, tx, "episode", query, src["id"], providerID)
	if err != nil {
		return err
	}
	if len(rows) > 100000 {
		return fmt.Errorf("%w: grouped provider ownership exceeds the source limit", errMediaAcquisitionIdentity)
	}
	exact := 0
	for _, row := range rows {
		if err := ctx.Err(); err != nil {
			return err
		}
		if str(row["provider_episode_id"]) != providerID {
			continue
		}
		exact++
		if number(row["episode_index"]) != index || exact > 1 {
			return fmt.Errorf("%w: grouped provider episode has another storage assignment; explicit review is required", errMediaAcquisitionIdentity)
		}
	}
	return nil
}

func mediaGroupVerificationError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if errors.Is(err, errMediaAcquisitionIdentity) {
		return err
	}
	return fmt.Errorf("%w: episode group identity could not be verified", errMediaAcquisitionIdentity)
}
