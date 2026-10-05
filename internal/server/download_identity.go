// SPDX-License-Identifier: AGPL-3.0-only
package server

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/AniBakaBaka/AniDan/internal/store"
)

// validateDownloadIdentity runs while fileMu and the publication transaction are
// held. A source reattachment or provider-ID change must never turn an in-flight
// old download into comments for a different episode. Mutable display/health
// fields do not affect the network identity and are deliberately excluded.
func (s *Server) validateDownloadIdentity(ctx context.Context, tx *sql.Tx, ep, src store.Row) error {
	if ep == nil || src == nil || number(ep["source_id"]) != number(src["id"]) {
		return errors.New("download source identity is invalid")
	}
	if err := s.validateSourceSnapshot(ctx, tx, src); err != nil {
		return err
	}
	if err := s.validateMediaListingProof(ctx, mediaListingProofFromContext(ctx), src); err != nil {
		return err
	}
	query := "SELECT * FROM " + s.Store.Quote("episode") + " WHERE id=?"
	if s.Store.Dialect != "sqlite" {
		query += " FOR UPDATE"
	}
	rows, err := s.libRows(ctx, tx, "episode", query, ep["id"])
	if err != nil {
		return err
	}
	if len(rows) != 1 {
		return errors.New("download episode identity no longer exists")
	}
	keys := []string{"source_id", "provider_episode_id", "episode_index"}
	// Custom URL imports have no source-level native adapter identity. Their
	// stored origin URL is therefore part of the publication binding.
	if str(src["provider_name"]) == "custom" {
		keys = append(keys, "source_url")
	}
	for _, key := range keys {
		if str(rows[0][key]) != str(ep[key]) {
			return fmt.Errorf("download episode identity changed (%s); reload and retry", key)
		}
	}

	if err := s.validateMediaGroupContext(ctx, tx); err != nil {
		return err
	}
	return s.validateMediaGroupProviderOwnership(ctx, tx, src, number(ep["episode_index"]), str(ep["provider_episode_id"]))
}
