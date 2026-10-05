// SPDX-License-Identifier: AGPL-3.0-only
package server

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/AniBakaBaka/AniDan/internal/provider"
	"github.com/AniBakaBaka/AniDan/internal/store"
)

// Old libraries may contain real IDs in the historical virtual-result range.
// A stored row wins if no token-scoped virtual binding exists, or both bindings
// identify one of that anime's exact provider/media sources. Conflicting live
// bindings are ambiguous and must never trigger a provider request or select a
// different library target merely because they share the same integer.
func (s *Server) playerVirtualMatchesStored(ctx context.Context, token string, id int64) (bool, error) {
	var result provider.SearchResult
	err := s.cacheGet(ctx, fmt.Sprintf("player_virtual_%s_%d", tokenHash(token), id), &result)
	if errors.Is(err, sql.ErrNoRows) || err != nil && err.Error() == "cache missing or expired" {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	if result.Provider == "" || result.ID == "" {
		return false, nil
	}
	sources, err := s.Store.List(ctx, "anime_sources", store.Row{"anime_id": id}, 1000, 0)
	if err != nil {
		return false, err
	}
	for _, source := range sources {
		if str(source["provider_name"]) == result.Provider && str(source["media_id"]) == result.ID {
			return true, nil
		}
	}
	return false, nil
}

func (s *Server) playerOccupiedVirtualRange(ctx context.Context) (map[int64]bool, error) {
	rows, err := s.Store.DB.QueryContext(ctx, "SELECT id FROM "+s.Store.Quote("anime")+" WHERE id >= 900000 AND id < 1000000")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	occupied := map[int64]bool{}
	for rows.Next() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		occupied[id] = true
	}
	return occupied, rows.Err()
}
