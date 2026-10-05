// SPDX-License-Identifier: AGPL-3.0-only
package server

import (
	"context"
	"fmt"

	"github.com/AniBakaBaka/AniDan/internal/provider"
	"github.com/AniBakaBaka/AniDan/internal/store"
)

// Refresh cannot infer a continuation of a custom episode group, including a
// group inherited from another TV association with the same known TMDB ID.
// Refuse before listing: a listing may contain only unseen raw coordinates and
// therefore cannot establish their intended storage assignment from old rows.
func (s *Server) validateSourceRefreshGrouping(ctx context.Context, src store.Row) error {
	anime, err := s.Store.Get(ctx, "anime", src["anime_id"])
	if err != nil {
		return mediaGroupVerificationError(ctx, err)
	}
	if anime == nil {
		return mediaEpisodeGroupIdentity("source target is missing; no episodes were processed")
	}
	// Movie and TV TMDB identities have separate namespaces.
	if str(anime["type"]) != "tv_series" {
		return nil
	}
	group, err := s.loadMediaEpisodeGroup(ctx, anime, "", int(number(anime["season"])))
	if canceled := ctx.Err(); canceled != nil {
		return canceled
	}
	if err != nil || group != nil {
		return mediaEpisodeGroupIdentity("source refresh is unsupported for configured or unverified grouping; existing pools are retained; use an explicit episode import")
	}
	return nil
}

// Native source listings cannot prove the inverse of a stored group or offset
// projection. Check every listed identity before admitting even an earlier new
// episode; the source-locked workflow checks also reject assignments made after
// this snapshot. Opaque IDs are compared byte-for-byte, independent of SQL
// collation, and never inferred from titles, list order, or episode numbers.
func validateSourceRefreshProviderOwnership(ctx context.Context, initial map[int64]store.Row, episodes []provider.Episode) error {
	type assignment struct {
		index     int64
		ambiguous bool
	}
	owners := make(map[string]assignment, len(initial))
	for index, row := range initial {
		if err := ctx.Err(); err != nil {
			return err
		}
		id := str(row["provider_episode_id"])
		if id == "" {
			continue
		}
		_, exists := owners[id]
		owners[id] = assignment{index: index, ambiguous: exists}
	}
	listed := make(map[string]bool, len(episodes))
	for _, episode := range episodes {
		if err := ctx.Err(); err != nil {
			return err
		}
		if episode.ID == "" {
			continue
		}
		if listed[episode.ID] {
			return fmt.Errorf("%w: source listing repeats a provider episode identity; no episodes were processed", errMediaAcquisitionIdentity)
		}
		listed[episode.ID] = true
		if owner, exists := owners[episode.ID]; exists && (owner.ambiguous || owner.index != int64(episode.Index)) {
			return fmt.Errorf("%w: source listing conflicts with an existing provider episode storage assignment; explicit review is required; no episodes were processed", errMediaAcquisitionIdentity)
		}
	}
	return nil
}
