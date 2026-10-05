// SPDX-License-Identifier: AGPL-3.0-only
package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/AniBakaBaka/AniDan/internal/boundedcache"
	"github.com/AniBakaBaka/AniDan/internal/provider"
)

func (s *Server) initProviderCaches() {
	s.providerCacheOnce.Do(func() {
		s.providerSearchCache = boundedcache.New[[]provider.SearchResult](8<<20, 3*time.Hour)
		s.providerEpisodeCache = boundedcache.New[[]provider.Episode](8<<20, 3*time.Hour)
	})
}
func (s *Server) cacheTTL(ctx context.Context, key string) (time.Duration, error) {
	n, e := strconv.Atoi(s.setting(ctx, key, "10800"))
	if e != nil || n < 0 || n > 604800 {
		return 0, fmt.Errorf("%s must be 0..604800 seconds", key)
	}
	return time.Duration(n) * time.Second, nil
}

// Persistent identities bind the exact provider snapshot to private configuration
// digests. Registry pointers/revisions remain only in unknown in-process adapter
// identities; native adapters can reuse valid response caches after restart.
func providerResponseKey(operation, value string, names []string, settings map[string]string, ttl time.Duration) string {
	raw, _ := json.Marshal([]any{"provider-response-v1", Version, operation, strings.TrimSpace(value), names, settings, ttl})
	return fmt.Sprintf("%x", sha256.Sum256(raw))
}
func (s *Server) cachedProviderSearch(ctx context.Context, keyword string, names ...string) ([]provider.SearchResult, error) {
	s.initProviderCaches()
	ttl, e := s.cacheTTL(ctx, "searchTtlSeconds")
	if e != nil {
		return nil, e
	}
	snapshot, settings := s.Providers.CacheSnapshot(names...)
	if len(names) == 0 {
		names = snapshot.Names()
	}
	checkRouting := func() error {
		for _, name := range names {
			p, _ := snapshot.Get(name)
			if err := provider.ValidateRouting(ctx, p); err != nil {
				return err
			}
		}
		return nil
	}
	if err := checkRouting(); err != nil {
		// Preserve ordinary partial-source behavior without serving a previously
		// authorized cached response for a revoked provider snapshot.
		return snapshot.Search(ctx, keyword, names...)
	}
	key := providerResponseKey("search", keyword, names, settings, ttl)
	if s.Cache != nil {
		var validated []provider.SearchResult
		validatedThisCall := false
		validate := func(raw json.RawMessage) error {
			validatedThisCall = false
			if err := checkRouting(); err != nil {
				return err
			}
			value, err := decodeProviderSearchJSON(raw)
			if err == nil {
				validated = value
				validatedThisCall = true
			}
			return err
		}
		raw, loadErr := s.Cache.DoValidated(ctx, "provider_search", key, ttl, validate, func(loadCtx context.Context) (json.RawMessage, error) {
			rows, err := snapshot.Search(loadCtx, keyword, names...)
			data, encodeErr := json.Marshal(rows)
			return data, errors.Join(err, encodeErr)
		})
		if len(raw) == 0 {
			return nil, loadErr
		}
		// A hit/leader was already decoded by this call's validator. Followers
		// validate/decode their own returned bytes, so caller mutation can never
		// alias another request's typed slice. No raw JSON bytes are retained.
		if validatedThisCall {
			return validated, loadErr
		}
		out, err := decodeProviderSearchJSON(raw)
		return out, errors.Join(loadErr, err)
	}
	out, e := s.providerSearchCache.DoTTL(ctx, key, ttl, func() ([]provider.SearchResult, int64, error) {
		out, e := snapshot.Search(ctx, keyword, names...)
		size := int64(64)
		for _, v := range out {
			size += int64(len(v.ID) + len(v.Title) + len(v.Type) + len(v.ImageURL) + len(v.Provider) + 96)
		}
		return out, size, e
	})
	return append([]provider.SearchResult{}, out...), e
}
func (s *Server) cachedProviderEpisodes(ctx context.Context, p provider.Provider, mediaID string) ([]provider.Episode, error) {
	if err := provider.ValidateRouting(ctx, p); err != nil {
		return nil, err
	}
	s.initProviderCaches()
	ttl, e := s.cacheTTL(ctx, "episodesTtlSeconds")
	if e != nil {
		return nil, e
	}
	key := providerResponseKey("episodes", mediaID, []string{p.Name()}, map[string]string{p.Name(): provider.CacheIdentity(p)}, ttl)
	if s.Cache != nil {
		var validated []provider.Episode
		validatedThisCall := false
		validate := func(raw json.RawMessage) error {
			validatedThisCall = false
			if err := provider.ValidateRouting(ctx, p); err != nil {
				return err
			}
			value, err := decodeProviderEpisodeJSON(raw)
			if err == nil {
				validated = value
				validatedThisCall = true
			}
			return err
		}
		raw, loadErr := s.Cache.DoValidated(ctx, "provider_episodes", key, ttl, validate, func(loadCtx context.Context) (json.RawMessage, error) {
			rows, err := p.Episodes(loadCtx, mediaID)
			data, encodeErr := json.Marshal(rows)
			return data, errors.Join(err, encodeErr)
		})
		if len(raw) == 0 {
			return nil, loadErr
		}
		if validatedThisCall {
			return validated, loadErr
		}
		out, err := decodeProviderEpisodeJSON(raw)
		return out, errors.Join(loadErr, err)
	}
	out, e := s.providerEpisodeCache.DoTTL(ctx, key, ttl, func() ([]provider.Episode, int64, error) {
		out, e := p.Episodes(ctx, mediaID)
		size := int64(64)
		for _, v := range out {
			size += int64(len(v.ID) + len(v.Title) + len(v.URL) + 48)
		}
		return out, size, e
	})
	return append([]provider.Episode{}, out...), e
}

func (s *Server) clearRuntimeCaches(ctx context.Context) error {
	// Local files/output are always cleared, even when the optional remote
	// backend is unavailable. Callers must report that remote failure.
	s.clearLocalRuntimeCaches()
	if s.Cache != nil {
		_, err := s.Cache.Clear(ctx, "")
		return err
	}
	return nil
}
func (s *Server) clearLocalRuntimeCaches() {
	s.initProviderCaches()
	s.providerSearchCache.Clear()
	s.providerEpisodeCache.Clear()
	if s.parsedCache != nil {
		s.parsedCache.Clear()
	}
	if s.outputCache != nil {
		s.outputCache.Clear()
	}
	if s.Metadata != nil {
		s.Metadata.ClearMetadataCaches()
	}
}

func isCacheTTLKey(key string) bool {
	switch key {
	case "searchTtlSeconds", "episodesTtlSeconds", "baseInfoTtlSeconds", "metadataSearchTtlSeconds":
		return true
	}
	return false
}
func (s *Server) runtimeCacheStats() map[string]any {
	s.initProviderCaches()
	result := map[string]any{}
	add := func(key string, n int, b int64, max int64) {
		result[key] = map[string]any{"items": n, "bytes": b, "maxBytes": max}
	}
	n, b := s.providerSearchCache.Stats()
	add("providerSearch", n, b, 8<<20)
	n, b = s.providerEpisodeCache.Stats()
	add("providerEpisodes", n, b, 8<<20)
	if s.parsedCache != nil {
		n, b = s.parsedCache.Stats()
		add("parsedComments", n, b, 16<<20)
	}
	if s.outputCache != nil {
		n, b = s.outputCache.Stats()
		add("playerOutput", n, b, 16<<20)
	}
	if s.Metadata != nil {
		result["metadata"] = s.Metadata.MetadataCacheStats()
	}
	if s.Cache != nil {
		result["responseBackend"] = s.Cache.Health()
	}
	return result
}

func decodeProviderSearchJSON(raw json.RawMessage) ([]provider.SearchResult, error) {
	var results []provider.SearchResult
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&results); err != nil {
		return nil, errors.New("cached provider search shape is invalid")
	}
	for _, row := range results {
		if row.ID == "" || row.Provider == "" {
			return nil, errors.New("cached provider search lacks identity")
		}
	}
	return results, nil
}
func decodeProviderEpisodeJSON(raw json.RawMessage) ([]provider.Episode, error) {
	var results []provider.Episode
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&results); err != nil {
		return nil, errors.New("cached provider episode shape is invalid")
	}
	for _, row := range results {
		if row.ID == "" {
			return nil, errors.New("cached provider episode lacks identity")
		}
	}
	return results, nil
}
