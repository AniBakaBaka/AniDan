// SPDX-License-Identifier: AGPL-3.0-only
package server

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/AniBakaBaka/AniDan/internal/provider"
	"github.com/AniBakaBaka/AniDan/internal/store"
)

type mediaListingBatchKey struct{}
type mediaListingItemKey struct{}
type mediaListingCallKey struct{}
type mediaListingProofKey struct{}

type mediaListingItem struct {
	memo  *mediaListingSnapshot
	scope string
}

// A separate call belongs to one candidate of one item. No mutable call state
// or retained listing is transferred into a shared comment-fetch worker.
type mediaListingCall struct {
	memo          *mediaListingSnapshot
	scope         string
	force, reused bool
	proof         *mediaListingProof
}

type mediaListingProof struct{ name, media, identity string }

func mediaListingForItem(ctx context.Context, row, origin store.Row) context.Context {
	memo, _ := ctx.Value(mediaListingBatchKey{}).(*mediaListingSnapshot)
	if memo == nil {
		return ctx
	}
	scope := ""
	if number(row["server_id"]) > 0 && origin != nil && number(origin["id"]) == number(row["server_id"]) && str(row["series_id"]) != "" && row["season"] != nil && number(row["season"]) >= 0 && (str(row["media_type"]) == "tv_series" || str(row["media_type"]) == "tv") {
		fields := []any{"media-listing-scope-v1", row["server_id"], origin["provider_name"], origin["url"], origin["api_token"], row["series_id"], row["season_id"], row["title"], row["media_type"], row["season"], row["year"], row["tmdb_id"], row["tvdb_id"], row["imdb_id"]}
		bounded := true
		for _, v := range fields {
			if len(str(v)) > 4096 {
				bounded = false
				break
			}
		}
		if bounded {
			if raw, err := json.Marshal(fields); err == nil {
				scope = fmt.Sprintf("%x", sha256.Sum256(raw))
			}
		}
	}
	return context.WithValue(ctx, mediaListingItemKey{}, &mediaListingItem{memo: memo, scope: scope})
}

func mediaListingForCandidate(ctx context.Context, sourceSeason int) context.Context {
	item, _ := ctx.Value(mediaListingItemKey{}).(*mediaListingItem)
	if item == nil {
		return ctx
	}
	scope := item.scope
	if scope != "" {
		scope += ":" + strconv.Itoa(sourceSeason)
	}
	return context.WithValue(ctx, mediaListingCallKey{}, &mediaListingCall{memo: item.memo, scope: scope})
}

func mediaListingCallFromContext(ctx context.Context) *mediaListingCall {
	v, _ := ctx.Value(mediaListingCallKey{}).(*mediaListingCall)
	return v
}

func mediaListingProofFromContext(ctx context.Context) *mediaListingProof {
	v, _ := ctx.Value(mediaListingProofKey{}).(*mediaListingProof)
	return v
}

func (s *Server) loadMediaListing(ctx context.Context, call *mediaListingCall, p provider.Provider, name, media string) ([]provider.Episode, error) {
	proof := &mediaListingProof{name: name, media: media, identity: provider.CacheIdentity(p)}
	// A forced refill may discover a newly available episode, but cannot adopt
	// a different adapter halfway through this item's selection.
	if call.proof != nil && *call.proof != *proof {
		return nil, fmt.Errorf("%w: provider changed during episode selection", errMediaAcquisitionIdentity)
	}
	validate := func(c context.Context) error {
		if err := s.validateMediaListingProof(c, proof, nil); err != nil {
			return err
		}
		return provider.ValidateRouting(c, p)
	}
	eps, reused, err := call.memo.load(ctx, mediaListingKey{scope: call.scope, provider: name, media: media, identity: proof.identity}, call.force, validate, func(c context.Context) ([]provider.Episode, error) { return p.Episodes(c, media) })
	call.force, call.reused = false, reused
	if err == nil {
		call.proof = proof
	}
	return eps, err
}

// This check is safe inside SQL publication transactions: it reads only the
// immutable registry identity, not an authorization callback needing another
// SQL connection. Routing is checked outside transactions on every list use
// and by the provider transport before its actual requests.
func (s *Server) validateMediaListingProof(ctx context.Context, proof *mediaListingProof, src store.Row) error {
	if proof == nil {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if src != nil && (str(src["provider_name"]) != proof.name || str(src["media_id"]) != proof.media) {
		return fmt.Errorf("%w: episode listing source changed", errMediaAcquisitionIdentity)
	}
	p, ok := s.Providers.Get(proof.name)
	if !ok || provider.CacheIdentity(p) != proof.identity {
		return fmt.Errorf("%w: episode listing provider configuration changed", errMediaAcquisitionIdentity)
	}
	return nil
}

// Called only outside SQL transactions. A dynamically revoked route can keep
// the same configuration digest until detached providers are republished.
func (s *Server) validateMediaListingRouting(ctx context.Context, proof *mediaListingProof, src store.Row, captured provider.Provider) error {
	if proof == nil {
		return nil
	}
	if err := s.validateMediaListingProof(ctx, proof, src); err != nil {
		return err
	}
	if captured == nil || provider.CacheIdentity(captured) != proof.identity {
		return fmt.Errorf("%w: episode listing provider configuration changed", errMediaAcquisitionIdentity)
	}
	return provider.ValidateRouting(ctx, captured)
}
