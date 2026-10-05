// SPDX-License-Identifier: AGPL-3.0-only
package server

import (
	"context"
	"fmt"
	"github.com/AniBakaBaka/AniDan/internal/danmaku"
)

// providerComments relies on each adapter's cancellation-aware request
// coalescing. Explicit refreshes are not hidden behind a stale source cache.
func (s *Server) providerComments(ctx context.Context, name, id string) ([]danmaku.Comment, error) {
	p, ok := s.Providers.Get(name)
	if !ok {
		return nil, fmt.Errorf("provider unavailable: %s", name)
	}
	return p.Comments(ctx, id)
}
