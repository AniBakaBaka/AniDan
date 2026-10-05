// SPDX-License-Identifier: AGPL-3.0-only
package server

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/AniBakaBaka/AniDan/internal/store"
)

var errWorkflowEpisodeConflict = errors.New("existing episode has a different provider identity; explicit remapping or a separate source is required")

// captureWorkflowEpisodes retains the library mapping from before any remote
// listing/resolution. Later per-episode checks must not adopt a reindex or
// deletion committed while another episode's network work was in flight.
func (s *Server) captureWorkflowEpisodes(ctx context.Context, src store.Row) (map[int64]store.Row, error) {
	result := map[int64]store.Row{}
	err := s.libTransaction(ctx, func(tx *sql.Tx) error {
		if e := s.validateSourceSnapshot(ctx, tx, src); e != nil {
			return e
		}
		query := "SELECT * FROM " + s.Store.Quote("episode") + " WHERE source_id=? ORDER BY id LIMIT 100001"
		if s.Store.Dialect != "sqlite" {
			query += " FOR UPDATE"
		}
		rows, e := s.libRows(ctx, tx, "episode", query, src["id"])
		if e != nil {
			return e
		}
		if len(rows) > 100000 {
			return errors.New("source mapping exceeds the 100000 episode workflow snapshot limit")
		}
		for _, row := range rows {
			if e := ctx.Err(); e != nil {
				return e
			}
			index := number(row["episode_index"])
			if _, exists := result[index]; exists {
				return errors.New("ambiguous existing episode index")
			}
			result[index] = row
		}
		return nil
	})
	return result, err
}

// Source rows are locked before episode rows in both admission and publication.
// The caller owns a short transaction; no network work is performed here.
func (s *Server) validateSourceSnapshot(ctx context.Context, tx *sql.Tx, src store.Row) error {
	if src == nil {
		return errors.New("expected source snapshot required")
	}
	query := "SELECT * FROM " + s.Store.Quote("anime_sources") + " WHERE id=?"
	if s.Store.Dialect != "sqlite" {
		query += " FOR UPDATE"
	}
	rows, err := s.libRows(ctx, tx, "anime_sources", query, src["id"])
	if err != nil {
		return err
	}
	if len(rows) != 1 {
		return errors.New("download source no longer exists")
	}
	for _, key := range []string{"anime_id", "provider_name", "media_id", "source_order"} {
		if str(rows[0][key]) != str(src[key]) {
			return fmt.Errorf("download source identity changed (%s); reload and retry", key)
		}
	}
	return nil
}

// prepareWorkflowEpisode preserves the existing source/index row's exact legacy
// ID. A remote listing ID is a binding, not permission to rewrite that row's
// identity. Inline custom content uses bindProvider=false and changes no existing
// metadata; explicit provider remapping must be a separate deliberate operation.
func (s *Server) prepareWorkflowEpisode(ctx context.Context, src store.Row, index int64, title, url, providerID string, bindProvider bool) (store.Row, error) {
	if index < 0 {
		return nil, errors.New("negative episode index")
	}
	if bindProvider && providerID == "" {
		return nil, errors.New("provider episode identity required")
	}
	var result store.Row
	err := s.libTransaction(ctx, func(tx *sql.Tx) error {
		if e := s.validateSourceSnapshot(ctx, tx, src); e != nil {
			return e
		}
		if e := s.validateMediaListingProof(ctx, mediaListingProofFromContext(ctx), src); e != nil {
			return e
		}
		if e := s.validateMediaGroupContext(ctx, tx); e != nil {
			return e
		}
		if e := s.validateMediaGroupProviderOwnership(ctx, tx, src, index, providerID); e != nil {
			return e
		}
		query := "SELECT * FROM " + s.Store.Quote("episode") + " WHERE source_id=? AND episode_index=?"
		if s.Store.Dialect != "sqlite" {
			query += " FOR UPDATE"
		}
		rows, e := s.libRows(ctx, tx, "episode", query, src["id"], index)
		if e != nil {
			return e
		}
		if len(rows) > 1 {
			return errors.New("ambiguous existing episode index")
		}
		if len(rows) == 1 {
			if bindProvider && str(rows[0]["provider_episode_id"]) != providerID {
				return errWorkflowEpisodeConflict
			}
			result = rows[0]
			return nil
		}
		id, e := episodeIdentifier(number(src["anime_id"]), number(src["source_order"]), index)
		if e != nil {
			return e
		}
		collision, e := s.libOne(ctx, tx, "episode", "id = ?", id)
		if e != nil && !errors.Is(e, sql.ErrNoRows) {
			return e
		}
		if collision != nil {
			return errors.New("generated episode ID collides with an existing library episode; explicit mapping is required")
		}
		_, e = s.Store.InsertTx(ctx, tx, "episode", store.Row{"id": id, "source_id": src["id"], "title": title, "episode_index": index, "provider_episode_id": providerID, "source_url": url, "comment_count": 0})
		if e != nil {
			return e
		}
		result, e = s.libOne(ctx, tx, "episode", "id = ?", id)
		return e
	})
	return result, err
}

// prepareWorkflowEpisodeExpected retains an original mapping without recreating
// it after deletion/reindexing. Episode zero is valid for generic movie/special
// imports; stricter route-specific limits remain at their callers.
func (s *Server) prepareWorkflowEpisodeExpected(ctx context.Context, src, existing store.Row, index int64, title, url, providerID string, bindProvider bool) (store.Row, error) {
	if index < 0 {
		return nil, errors.New("negative episode index")
	}
	if existing == nil {
		return s.prepareWorkflowEpisode(ctx, src, index, title, url, providerID, bindProvider)
	}
	if number(existing["episode_index"]) != index {
		return nil, errors.New("expected episode index does not match requested mapping")
	}
	if bindProvider && (providerID == "" || str(existing["provider_episode_id"]) != providerID) {
		return nil, errWorkflowEpisodeConflict
	}
	err := s.libTransaction(ctx, func(tx *sql.Tx) error { return s.validateDownloadIdentity(ctx, tx, existing, src) })
	return existing, err
}
