// SPDX-License-Identifier: AGPL-3.0-only
package server

import (
	"context"
	"strings"
	"time"

	"github.com/AniBakaBaka/AniDan/internal/provider"
)

const (
	mediaListingMaxAge             = 60 * time.Second
	mediaListingMaxEntries         = 10000
	mediaListingMaxBytes     int64 = 4 << 20
	mediaListingFixedBytes   int64 = 128
	mediaListingEpisodeBytes int64 = 64
)

// All key components are exact, caller-established identities. In particular,
// scope must identify one series/season and identity the current immutable
// adapter configuration. An empty component disables retention.
type mediaListingKey struct {
	scope, provider, media, identity string
}

// mediaListingSnapshot belongs to one sequential multi-item job. It holds only
// the last successful raw listing, never a provider, client, or validator. It
// is intentionally not safe for concurrent use or sharing between jobs.
type mediaListingSnapshot struct {
	key      mediaListingKey
	episodes []provider.Episode
	loadedAt time.Time
	now      func() time.Time
}

func newMediaListingSnapshot() *mediaListingSnapshot {
	return &mediaListingSnapshot{now: time.Now}
}

func (s *mediaListingSnapshot) clear() {
	if s != nil {
		s.key = mediaListingKey{}
		s.episodes = nil
		s.loadedAt = time.Time{}
	}
}

func (s *mediaListingSnapshot) clock() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}

func validateMediaListing(ctx context.Context, validate func(context.Context) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if validate != nil {
		if err := validate(ctx); err != nil {
			return err
		}
	}
	return ctx.Err()
}

// load returns reused=true only for a validated snapshot hit. Reloads, including
// forced or expired ones, discard the preceding entry before calling loader;
// errors and unretained results cannot expose an older listing on a later call.
// A nil snapshot or validator preserves ordinary loading without retention.
func (s *mediaListingSnapshot) load(ctx context.Context, key mediaListingKey, force bool, validate func(context.Context) error, loader func(context.Context) ([]provider.Episode, error)) ([]provider.Episode, bool, error) {
	if s != nil && s.key != key {
		s.clear()
	}
	if err := validateMediaListing(ctx, validate); err != nil {
		s.clear()
		return nil, false, err
	}
	if s != nil && validate != nil && !force && len(s.episodes) != 0 {
		age := s.clock().Sub(s.loadedAt)
		if age >= 0 && age < mediaListingMaxAge {
			// Episode fields are values and immutable strings. Only the slice and
			// structs need copying on a hit; the owned strings can be shared.
			return append([]provider.Episode(nil), s.episodes...), true, nil
		}
	}
	s.clear()
	rows, err := loader(ctx)
	if err != nil {
		return rows, false, err
	}
	var completed time.Time
	if s != nil {
		completed = s.clock()
	}
	if err := validateMediaListing(ctx, validate); err != nil {
		return nil, false, err
	}
	if s == nil || validate == nil || !mediaListingRetainable(key, rows) {
		return rows, false, nil
	}
	owned := make([]provider.Episode, len(rows))
	for i, row := range rows {
		owned[i] = provider.Episode{ID: strings.Clone(row.ID), Title: strings.Clone(row.Title), URL: strings.Clone(row.URL), Index: row.Index}
	}
	s.key = mediaListingKey{scope: strings.Clone(key.scope), provider: strings.Clone(key.provider), media: strings.Clone(key.media), identity: strings.Clone(key.identity)}
	s.episodes = owned
	s.loadedAt = completed
	return rows, false, nil
}

// This bounds an estimate of owned payload, not process RSS or allocator
// overhead: fixed snapshot/key storage, a conservative 64 bytes per Episode,
// and all logical key/episode string bytes. strings.Clone prevents small
// substrings from retaining larger backing strings. No serialization is needed.
func mediaListingRetainable(key mediaListingKey, rows []provider.Episode) bool {
	if key.scope == "" || key.provider == "" || key.media == "" || key.identity == "" || len(rows) == 0 || len(rows) > mediaListingMaxEntries {
		return false
	}
	size := mediaListingFixedBytes
	for _, value := range []string{key.scope, key.provider, key.media, key.identity} {
		size = mediaListingAddBytes(size, int64(len(value)))
	}
	for _, row := range rows {
		if row.ID == "" {
			return false
		}
		size = mediaListingAddBytes(size, mediaListingEpisodeBytes)
		for _, value := range []string{row.ID, row.Title, row.URL} {
			size = mediaListingAddBytes(size, int64(len(value)))
		}
		if size > mediaListingMaxBytes {
			return false
		}
	}
	return size <= mediaListingMaxBytes
}

// Saturate at the first over-budget value instead of allowing arithmetic to
// wrap. Each input length is converted separately before any addition.
func mediaListingAddBytes(size, additional int64) int64 {
	if size < 0 || size > mediaListingMaxBytes || additional < 0 || additional > mediaListingMaxBytes-size {
		return mediaListingMaxBytes + 1
	}
	return size + additional
}
