// SPDX-License-Identifier: AGPL-3.0-only
package server

import (
	"context"
	"strconv"

	"github.com/AniBakaBaka/AniDan/internal/provider"
)

func validateCollectionSelection(season, mid string) error {
	if (season == "") != (mid == "") {
		return libErr(422, "Both collection identifiers are required together")
	}
	for _, id := range []string{season, mid} {
		if id != "" {
			n, e := strconv.ParseInt(id, 10, 64)
			if e != nil || n <= 0 || strconv.FormatInt(n, 10) != id {
				return libErr(422, "Invalid collection identifier")
			}
		}
	}
	return nil
}

// The preview identifiers are expectations only. Resolve from the member URL
// first, so a forged ID cannot select an unrelated collection/API destination.
func resolveCollectionSelection(ctx context.Context, p provider.Provider, raw, season, mid string) (provider.CollectionInfo, []provider.Episode, error) {
	zero := provider.CollectionInfo{}
	if e := validateCollectionSelection(season, mid); e != nil {
		return zero, nil, e
	}
	resolver, ok := p.(interface {
		CollectionMedia(context.Context, string) (provider.CollectionInfo, []provider.Episode, error)
	})
	if !ok {
		return zero, nil, libErr(422, "Verified collection media import is unavailable for this provider")
	}
	mismatch := func() error {
		return libErr(409, "Collection membership changed or does not match the selected URL; preview again")
	}
	if season != "" {
		discovery, ok := p.(interface {
			CollectionMetadata(context.Context, string) (*provider.CollectionInfo, error)
		})
		if !ok {
			return zero, nil, libErr(422, "Collection membership discovery is unavailable")
		}
		expected, e := discovery.CollectionMetadata(ctx, raw)
		if e != nil {
			return zero, nil, libErr(422, e.Error())
		}
		if expected == nil || expected.SeasonID != season || expected.Mid != mid {
			return zero, nil, mismatch()
		}
	}
	info, eps, e := resolver.CollectionMedia(ctx, raw)
	if e != nil {
		return zero, nil, libErr(422, e.Error())
	}
	if season != "" && (info.SeasonID != season || info.Mid != mid) {
		return zero, nil, mismatch()
	}
	if len(eps) == 0 || len(eps) != info.Total {
		return zero, nil, libErr(422, "Collection listing is incomplete")
	}
	return info, eps, nil
}
