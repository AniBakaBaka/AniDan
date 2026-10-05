// SPDX-License-Identifier: AGPL-3.0-only
package server

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/AniBakaBaka/AniDan/internal/store"
)

// Group selection can precede creation of the actual library target. Keep that
// admitted target as separate immutable evidence rather than rebasing the group.
type mediaGroupTargetSnapshot struct {
	animeID                      int64
	identity                     store.Row
	groupID, tmdbID, fingerprint string
}

type mediaGroupTargetContextKey struct{}

func mediaGroupTargetFromContext(ctx context.Context) *mediaGroupTargetSnapshot {
	v, _ := ctx.Value(mediaGroupTargetContextKey{}).(*mediaGroupTargetSnapshot)
	return v
}

func mediaGroupPlanContext(ctx context.Context, p *mediaImportProjection) context.Context {
	if p.listingProof != nil {
		ctx = context.WithValue(ctx, mediaListingProofKey{}, p.listingProof)
	}
	if p.groupCandidate != nil || p.group != nil {
		ctx = context.WithValue(ctx, mediaGroupOwnershipContextKey{}, true)
	}
	if p.group != nil {
		ctx = context.WithValue(ctx, mediaEpisodeGroupContextKey{}, p.group)
	}
	if p.groupTarget != nil {
		ctx = context.WithValue(ctx, mediaGroupTargetContextKey{}, p.groupTarget)
	}
	return ctx
}

func (s *Server) readMediaGroupTarget(ctx context.Context, q libQueryer, animeID int64) (*mediaGroupTargetSnapshot, error) {
	suffix := ""
	if _, ok := q.(*sql.Tx); ok && s.Store.Dialect != "sqlite" {
		suffix = " FOR UPDATE"
	}
	// The caller has already locked all relevant anime rows before metadata.
	rows, err := s.libRows(ctx, q, "anime", "SELECT id,title,type,season,year,created_at FROM "+s.Store.Quote("anime")+" WHERE id=?"+suffix, animeID)
	if err != nil {
		return nil, err
	}
	if len(rows) != 1 {
		return nil, mediaEpisodeGroupIdentity("admitted target is missing")
	}
	t := &mediaGroupTargetSnapshot{animeID: animeID, identity: rows[0]}
	rows, err = s.libRows(ctx, q, "anime_metadata", "SELECT anime_id,tmdb_id,tmdb_episode_group_id FROM "+s.Store.Quote("anime_metadata")+" WHERE anime_id=? LIMIT 2"+suffix, animeID)
	if err != nil {
		return nil, err
	}
	if len(rows) > 1 {
		return nil, mediaEpisodeGroupIdentity("admitted target metadata is ambiguous")
	}
	if len(rows) == 1 {
		t.groupID, t.tmdbID = str(rows[0]["tmdb_episode_group_id"]), str(rows[0]["tmdb_id"])
	}
	return t, nil
}

func (s *Server) captureMediaGroupTarget(ctx context.Context, p *mediaImportProjection, req ImportRequest, src store.Row) error {
	if p.group == nil {
		return nil
	}
	return s.libTransaction(ctx, func(tx *sql.Tx) error {
		if err := s.validateSourceSnapshot(ctx, tx, src); err != nil {
			return err
		}
		id := number(src["anime_id"])
		if err := s.validateMediaEpisodeGroupForTarget(ctx, tx, p.group, id); err != nil {
			return mediaGroupVerificationError(ctx, err)
		}
		t, err := s.readMediaGroupTarget(ctx, tx, id)
		if err != nil {
			return mediaGroupVerificationError(ctx, err)
		}
		if str(t.identity["title"]) != req.Title || str(t.identity["type"]) != req.Type || number(t.identity["season"]) != int64(req.Season) {
			return mediaEpisodeGroupIdentity("admitted target does not match the requested identity")
		}
		if req.Year != nil && *req.Year > 0 && number(t.identity["year"]) > 0 && number(t.identity["year"]) != int64(*req.Year) {
			return mediaEpisodeGroupIdentity("admitted target has a different year")
		}
		if t.groupID != "" && t.groupID != p.group.groupID || t.tmdbID != "" && t.tmdbID != strconv.FormatInt(p.group.tvID, 10) {
			return mediaEpisodeGroupIdentity("admitted target has a conflicting association")
		}
		b, err := json.Marshal([]any{t.identity, t.groupID, t.tmdbID})
		if err != nil {
			return err
		}
		t.fingerprint = fmt.Sprintf("%x", sha256.Sum256(b))
		p.groupTarget = t
		return nil
	})
}

// Runs in every admission/publication/final-linkage transaction. The original
// group snapshot and the subsequently admitted target must both remain valid.
func (s *Server) validateMediaGroupContext(ctx context.Context, q libQueryer) error {
	g := mediaEpisodeGroupFromContext(ctx)
	if g == nil {
		return nil
	}
	t := mediaGroupTargetFromContext(ctx)
	var id int64
	if t != nil {
		id = t.animeID
	}
	if err := s.validateMediaEpisodeGroupForTarget(ctx, q, g, id); err != nil {
		return mediaGroupVerificationError(ctx, err)
	}
	if t == nil {
		return nil
	}
	current, err := s.readMediaGroupTarget(ctx, q, id)
	if err != nil {
		return mediaGroupVerificationError(ctx, err)
	}
	for key, value := range t.identity {
		if (value == nil) != (current.identity[key] == nil) || str(value) != str(current.identity[key]) {
			return mediaEpisodeGroupIdentity("admitted target identity changed")
		}
	}
	if current.groupID != t.groupID || t.tmdbID != "" && current.tmdbID != t.tmdbID || current.tmdbID != "" && current.tmdbID != strconv.FormatInt(g.tvID, 10) {
		return mediaEpisodeGroupIdentity("admitted target association changed")
	}
	return nil
}
