// SPDX-License-Identifier: AGPL-3.0-only
package server

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"hash"
	"strings"
	"unicode/utf8"

	"github.com/AniBakaBaka/AniDan/internal/provider"
)

const (
	notificationImportMaxEpisodes       = 1000
	notificationImportMaxBytes    int64 = 256 << 10
	// Account conservatively for the snapshot, request, year allocation, slice
	// headers, hashes, allocator rounding, and each retained episode struct.
	notificationImportSnapshotOverhead int64 = 1024
	notificationImportEpisodeOverhead  int64 = 160
)

var (
	errNotificationImportSnapshot = errors.New("invalid or oversized import preview; start a new search")
	errNotificationImportChanged  = errors.New("source or recognition preview changed; start a new search")
)

// A session owns this immutable snapshot. Only the separate selection, type,
// and season draft may be edited. No provider response backing strings survive
// capture, and Request has no episode payload until confirmation succeeds.
type notificationImportSnapshot struct {
	Request          ImportRequest
	Episodes         []ImportEpisode
	ProviderIdentity string
	Bytes            int64
	listing          [sha256.Size]byte
	seal             [sha256.Size]byte
}

func (s *Server) buildNotificationImportSnapshot(ctx context.Context, result provider.SearchResult) (*notificationImportSnapshot, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	typ := result.Type
	if typ == "" {
		typ = "tv_series"
	} // Same default as providerResult.
	// Search results have no season-presence bit. Match the existing whole-result
	// bot default; an explicit season zero is accepted separately at confirmation.
	request := ImportRequest{Provider: result.Provider, MediaID: result.ID, Title: result.Title, Type: typ, Season: max(result.Season, 1), ImageURL: result.ImageURL}
	if result.Year > 0 {
		year := result.Year
		request.Year = &year
	}
	if _, err := notificationImportRequestBytes(request); err != nil {
		return nil, err
	}
	p, err := s.notificationImportProvider(ctx, request.Provider, "")
	if err != nil {
		return nil, err
	}
	identity := provider.CacheIdentity(p)
	episodes, excluded, err := s.recognitionEpisodesPreview(ctx, request.Provider, request.MediaID, request.Title, nil)
	if err != nil {
		return nil, err
	}
	if _, err = s.notificationImportProvider(ctx, request.Provider, identity); err != nil {
		return nil, err
	}
	snapshot, err := captureNotificationImportSnapshot(request, identity, episodes, excluded)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return snapshot, nil
}

func (s *Server) notificationImportProvider(ctx context.Context, name, identity string) (provider.Provider, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if s.Providers == nil {
		return nil, errNotificationImportChanged
	}
	p, ok := s.Providers.Get(name)
	if !ok || (identity != "" && provider.CacheIdentity(p) != identity) {
		return nil, errNotificationImportChanged
	}
	if err := provider.ValidateRouting(ctx, p); err != nil {
		return nil, err
	}
	return p, nil
}

func notificationImportRequestBytes(request ImportRequest) (int64, error) {
	size := notificationImportSnapshotOverhead
	for _, value := range []string{request.Provider, request.MediaID, request.Title, request.Type, request.ImageURL} {
		if int64(len(value)) > notificationImportMaxBytes-size || !utf8.ValidString(value) {
			return 0, errNotificationImportSnapshot
		}
		size += int64(len(value))
	}
	if strings.TrimSpace(request.Provider) == "" || strings.TrimSpace(request.MediaID) == "" || strings.TrimSpace(request.Title) == "" ||
		(request.Type != "tv_series" && request.Type != "movie") || request.Season < 0 || request.Season > 9999 ||
		request.Edited || len(request.Episodes) != 0 || len(request.Aliases) != 0 || len(request.Metadata) != 0 || len(request.RecognitionWarnings) != 0 || request.CurrentEpisodeIndex != nil {
		return 0, errNotificationImportSnapshot
	}
	return size, nil
}

func captureNotificationImportSnapshot(request ImportRequest, identity string, episodes []provider.Episode, excluded []map[string]any) (*notificationImportSnapshot, error) {
	size, err := notificationImportRequestBytes(request)
	if err != nil {
		return nil, err
	}
	if len(episodes) == 0 || len(episodes) > notificationImportMaxEpisodes || len(excluded) > notificationImportMaxEpisodes-len(episodes) ||
		identity == "" || int64(len(identity)) > notificationImportMaxBytes-size || !utf8.ValidString(identity) {
		return nil, errNotificationImportSnapshot
	}
	size += int64(len(identity))
	// Excluded rows are also bounded and fingerprinted: hiding a changed source
	// row behind a filter must not make a stale confirmation look current.
	listing := sha256.New()
	ids, indices := make(map[string]bool, len(episodes)+len(excluded)), make(map[int]bool, len(episodes)+len(excluded))
	add := func(row provider.Episode, reason string) error {
		if notificationImportEpisodeOverhead > notificationImportMaxBytes-size {
			return errNotificationImportSnapshot
		}
		size += notificationImportEpisodeOverhead
		for _, value := range []string{row.ID, row.Title, row.URL, reason} {
			if int64(len(value)) > notificationImportMaxBytes-size || !utf8.ValidString(value) {
				return errNotificationImportSnapshot
			}
			size += int64(len(value))
			notificationImportHashString(listing, value)
		}
		if strings.TrimSpace(row.ID) == "" || row.Index < 0 || ids[row.ID] || indices[row.Index] {
			return errNotificationImportSnapshot
		}
		notificationImportHashInt(listing, row.Index)
		ids[row.ID], indices[row.Index] = true, true
		return nil
	}
	notificationImportHashInt(listing, len(episodes))
	for _, row := range episodes {
		if err := add(row, ""); err != nil {
			return nil, err
		}
	}
	notificationImportHashInt(listing, len(excluded))
	for _, row := range excluded {
		// Do not coerce values via reflection, float64, or str/number helpers.
		// These exact types are supplied by the existing recognition pipeline.
		name, nameOK := row["provider"].(string)
		id, idOK := row["episodeId"].(string)
		title, titleOK := row["title"].(string)
		url, urlOK := row["url"].(string)
		index, indexOK := row["episodeIndex"].(int)
		reason, reasonOK := row["filterReason"].(string)
		if !nameOK || name != request.Provider || !idOK || !titleOK || !urlOK || !indexOK || !reasonOK || reason == "" {
			return nil, errNotificationImportSnapshot
		}
		if err := add(provider.Episode{ID: id, Title: title, URL: url, Index: index}, reason); err != nil {
			return nil, err
		}
	}
	request.Provider, request.MediaID = strings.Clone(request.Provider), strings.Clone(request.MediaID)
	request.Title, request.Type, request.ImageURL = strings.Clone(request.Title), strings.Clone(request.Type), strings.Clone(request.ImageURL)
	if request.Year != nil {
		year := *request.Year
		request.Year = &year
	}
	snapshot := &notificationImportSnapshot{Request: request, ProviderIdentity: strings.Clone(identity), Bytes: size, Episodes: make([]ImportEpisode, len(episodes))}
	for i, row := range episodes {
		snapshot.Episodes[i] = ImportEpisode{ID: strings.Clone(row.ID), Title: strings.Clone(row.Title), URL: strings.Clone(row.URL), Index: row.Index}
	}
	copy(snapshot.listing[:], listing.Sum(nil))
	snapshot.seal = notificationImportSnapshotSeal(snapshot)
	return snapshot, nil
}

// Validation obtains a fresh provider listing through the same recognition and
// source-filter pipeline, then emits only explicitly selected original rows.
// Edited suppresses episode renumbering; normal storage title/type/season
// recognition and metadata enrichment still apply in the generic import job.
func (s *Server) validateNotificationImportSnapshot(ctx context.Context, snapshot *notificationImportSnapshot, selected []bool, mediaType string, season int) (ImportRequest, error) {
	if err := ctx.Err(); err != nil {
		return ImportRequest{}, err
	}
	if snapshot == nil || len(snapshot.Episodes) == 0 || len(snapshot.Episodes) > notificationImportMaxEpisodes || len(selected) != len(snapshot.Episodes) ||
		(mediaType != "movie" && mediaType != "tv_series") || season < 0 || season > 9999 || snapshot.Bytes <= 0 || snapshot.Bytes > notificationImportMaxBytes {
		return ImportRequest{}, errNotificationImportSnapshot
	}
	retained, err := notificationImportRequestBytes(snapshot.Request)
	if err != nil {
		return ImportRequest{}, err
	}
	// Check retained lengths before hashing even an accidentally mutated draft.
	if int64(len(snapshot.ProviderIdentity)) > snapshot.Bytes-retained {
		return ImportRequest{}, errNotificationImportSnapshot
	}
	retained += int64(len(snapshot.ProviderIdentity))
	for _, row := range snapshot.Episodes {
		if notificationImportEpisodeOverhead > snapshot.Bytes-retained {
			return ImportRequest{}, errNotificationImportSnapshot
		}
		retained += notificationImportEpisodeOverhead
		for _, value := range []string{row.ID, row.Title, row.URL} {
			if int64(len(value)) > snapshot.Bytes-retained {
				return ImportRequest{}, errNotificationImportSnapshot
			}
			retained += int64(len(value))
		}
	}
	if snapshot.seal != notificationImportSnapshotSeal(snapshot) {
		return ImportRequest{}, errNotificationImportSnapshot
	}
	selection := append([]bool(nil), selected...)
	count := 0
	for _, chosen := range selection {
		if chosen {
			count++
		}
	}
	if count == 0 {
		return ImportRequest{}, errNotificationImportSnapshot
	}
	if _, err := s.notificationImportProvider(ctx, snapshot.Request.Provider, snapshot.ProviderIdentity); err != nil {
		return ImportRequest{}, err
	}
	// A bot confirmation must not inherit a batch's opportunistic listing reuse.
	freshContext := context.WithValue(ctx, mediaListingCallKey{}, (*mediaListingCall)(nil))
	episodes, excluded, err := s.recognitionEpisodes(freshContext, snapshot.Request.Provider, snapshot.Request.MediaID, snapshot.Request.Title, nil)
	if err != nil {
		return ImportRequest{}, err
	}
	if _, err = s.notificationImportProvider(ctx, snapshot.Request.Provider, snapshot.ProviderIdentity); err != nil {
		return ImportRequest{}, err
	}
	fresh, err := captureNotificationImportSnapshot(snapshot.Request, snapshot.ProviderIdentity, episodes, excluded)
	if err != nil {
		return ImportRequest{}, err
	}
	if fresh.seal != snapshot.seal {
		return ImportRequest{}, errNotificationImportChanged
	}
	request := snapshot.Request
	if request.Year != nil {
		year := *request.Year
		request.Year = &year
	}
	request.Type, request.Season, request.Edited = strings.Clone(mediaType), season, true
	request.Episodes = make([]ImportEpisode, 0, count)
	for i, row := range snapshot.Episodes {
		if selection[i] {
			request.Episodes = append(request.Episodes, row)
		}
	}
	if err := ctx.Err(); err != nil {
		return ImportRequest{}, err
	}
	return request, nil
}

func notificationImportHashString(h hash.Hash, value string) {
	notificationImportHashInt(h, len(value))
	_, _ = h.Write([]byte(value))
}
func notificationImportHashInt(h hash.Hash, value int) {
	var raw [8]byte
	binary.LittleEndian.PutUint64(raw[:], uint64(value))
	_, _ = h.Write(raw[:])
}
func notificationImportSnapshotSeal(snapshot *notificationImportSnapshot) [sha256.Size]byte {
	h := sha256.New()
	request := snapshot.Request
	for _, value := range []string{request.Provider, request.MediaID, request.Title, request.Type, request.ImageURL, snapshot.ProviderIdentity} {
		notificationImportHashString(h, value)
	}
	notificationImportHashInt(h, request.Season)
	if request.Year == nil {
		notificationImportHashString(h, "no-year")
	} else {
		notificationImportHashString(h, "year")
		notificationImportHashInt(h, *request.Year)
	}
	notificationImportHashInt(h, int(snapshot.Bytes))
	notificationImportHashInt(h, len(snapshot.Episodes))
	for _, row := range snapshot.Episodes {
		for _, value := range []string{row.ID, row.Title, row.URL} {
			notificationImportHashString(h, value)
		}
		notificationImportHashInt(h, row.Index)
	}
	_, _ = h.Write(snapshot.listing[:])
	var seal [sha256.Size]byte
	copy(seal[:], h.Sum(nil))
	return seal
}
