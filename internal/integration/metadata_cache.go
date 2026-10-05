// SPDX-License-Identifier: AGPL-3.0-only
package integration

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/AniBakaBaka/AniDan/internal/boundedcache"
	"github.com/AniBakaBaka/AniDan/internal/proxyroute"
)

func (c *Client) initMetadataCaches() {
	c.metadataCacheOnce.Do(func() {
		c.metadataSearchCache = boundedcache.New[[]byte](8<<20, 3*time.Hour)
		c.metadataDetailsCache = boundedcache.New[[]byte](8<<20, 3*time.Hour)
	})
}

// ClearMetadataCaches clears only standalone in-process caches. Server callers
// separately clear the shared backend with their request context.
func (c *Client) ClearMetadataCaches() {
	c.initMetadataCaches()
	c.metadataSearchCache.Clear()
	c.metadataDetailsCache.Clear()
}

// ClearMetadataCachesContext clears both metadata regions and local caches.
// Remote failures are reported, including when only one region was cleared.
func (c *Client) ClearMetadataCachesContext(ctx context.Context) error {
	c.ClearMetadataCaches()
	if c.Cache == nil {
		return ctx.Err()
	}
	_, searchErr := c.Cache.Clear(ctx, "metadata_search")
	_, detailsErr := c.Cache.Clear(ctx, "metadata_details")
	return errors.Join(searchErr, detailsErr)
}

func (c *Client) metadataCacheKey(ctx context.Context, p, kind, value, mediaType string, cred Credential) (string, time.Duration, error) {
	ctx, route, err := c.metadataRouteContext(ctx, p)
	if err != nil {
		return "", 0, err
	}
	key := "metadataSearchTtlSeconds"
	if kind == "details" {
		key = "baseInfoTtlSeconds"
	}
	n, e := strconv.Atoi(c.setting(ctx, key, "10800"))
	if e != nil || n < 0 || n > 604800 {
		return "", 0, &Error{p, 422, key + " must be 0..604800 seconds"}
	}
	settings := map[string]string{}
	config := MetadataConfigs()[p]
	for _, v := range array(config["config_keys"]) {
		k := str(v)
		settings[k] = c.setting(ctx, k, "")
	}
	for _, k := range []string{p + "ApiBaseUrl", p + "ImageBaseUrl", p + "ApiKey", p + "ClientId", "tmdbLanguage", "proxyMode", "proxyUrl", "proxyEnabled", "imdbSuggestionBaseUrl", "imdbWebBaseUrl", "so360ApiBaseUrl", "so360WebBaseUrl"} {
		settings[k] = c.setting(ctx, k, "")
	}
	// Route identities cover the gateway and its current approval generation.
	// Validation above must happen even if a direct response is already cached.
	settings["selected_route"] = proxyroute.Identity(route.config)
	b, e := json.Marshal([]any{p, kind, value, mediaType, cred, settings, n})
	if e != nil {
		return "", 0, e
	}
	return fmt.Sprintf("%x", sha256.Sum256(b)), time.Duration(n) * time.Second, nil
}
func (c *Client) Search(ctx context.Context, p, keyword, mediaType string, cred Credential) ([]Metadata, error) {
	ctx, route, e := c.metadataRouteContext(ctx, p)
	if e != nil {
		return nil, e
	}
	c.initMetadataCaches()
	key, ttl, e := c.metadataCacheKey(ctx, p, "search", keyword, mediaType, cred)
	if e != nil {
		return nil, e
	}
	var out []Metadata
	decoded := false
	validate := func(raw json.RawMessage) error {
		var err error
		out, err = decodeMetadataSearch(raw)
		decoded = err == nil
		return err
	}
	data, e := c.metadataCacheDo(ctx, "metadata_search", c.metadataSearchCache, key, ttl, validate, func(loadCtx context.Context) (json.RawMessage, error) {
		loadCtx = context.WithValue(loadCtx, metadataRouteKey{}, route)
		v, e := c.searchUncached(loadCtx, p, keyword, mediaType, cred)
		if e != nil {
			return nil, e
		}
		b, e := json.Marshal(v)
		return b, e
	})
	if e != nil {
		return nil, e
	}
	if !decoded {
		out, e = decodeMetadataSearch(data)
	}
	return out, e
}
func (c *Client) Details(ctx context.Context, p, id, mediaType string, cred Credential) (*Metadata, error) {
	ctx, route, e := c.metadataRouteContext(ctx, p)
	if e != nil {
		return nil, e
	}
	c.initMetadataCaches()
	key, ttl, e := c.metadataCacheKey(ctx, p, "details", id, mediaType, cred)
	if e != nil {
		return nil, e
	}
	var out *Metadata
	decoded := false
	validate := func(raw json.RawMessage) error {
		var err error
		out, err = decodeMetadataDetails(raw)
		decoded = err == nil
		return err
	}
	data, e := c.metadataCacheDo(ctx, "metadata_details", c.metadataDetailsCache, key, ttl, validate, func(loadCtx context.Context) (json.RawMessage, error) {
		loadCtx = context.WithValue(loadCtx, metadataRouteKey{}, route)
		v, e := c.detailsUncached(loadCtx, p, id, mediaType, cred)
		if e != nil {
			return nil, e
		}
		b, e := json.Marshal(v)
		return b, e
	})
	if e != nil {
		return nil, e
	}
	if !decoded {
		out, e = decodeMetadataDetails(data)
	}
	return out, e
}

func (c *Client) MetadataCacheStats() map[string]any {
	c.initMetadataCaches()
	sn, sb := c.metadataSearchCache.Stats()
	dn, db := c.metadataDetailsCache.Stats()
	result := map[string]any{"search": map[string]any{"items": sn, "bytes": sb, "maxBytes": 8 << 20}, "details": map[string]any{"items": dn, "bytes": db, "maxBytes": 8 << 20}, "scope": "local"}
	if c.Cache != nil {
		result["backend"] = c.Cache.Health()
	}
	return result
}

// metadataCacheDo retains the standalone bounded cache while server clients use
// the configured shared backend. Keys are already hashed over provider settings,
// credentials, selected route, request identity and TTL.
func (c *Client) metadataCacheDo(ctx context.Context, region string, local *boundedcache.Cache[[]byte], key string, ttl time.Duration, validate func(json.RawMessage) error, load func(context.Context) (json.RawMessage, error)) ([]byte, error) {
	if c.Cache != nil {
		return c.Cache.DoValidated(ctx, region, key, ttl, validate, load)
	}
	return local.DoTTL(ctx, key, ttl, func() ([]byte, int64, error) {
		b, err := load(ctx)
		return b, int64(len(b)), err
	})
}

// MetadataCacheStatsContext reports the configured backend's region counters.
// The context-free method intentionally reports only local cache counters.
func (c *Client) MetadataCacheStatsContext(ctx context.Context) (map[string]any, error) {
	if c.Cache == nil {
		return c.MetadataCacheStats(), ctx.Err()
	}
	search, searchErr := c.Cache.Stats(ctx, "metadata_search")
	details, detailsErr := c.Cache.Stats(ctx, "metadata_details")
	if err := errors.Join(searchErr, detailsErr); err != nil {
		return nil, err
	}
	return map[string]any{"scope": "backend", "backend": c.Cache.Health(), "search": map[string]any{"items": search.Entries, "bytes": search.Bytes}, "details": map[string]any{"items": details.Entries, "bytes": details.Bytes}}, nil
}

// ResponseCacheIdentity returns an opaque identity covering provider settings,
// credentials and selected route. Callers retain control of their own cache TTL.
// Route validation occurs even when a response might already be cached.
// Use WithRoutingContext first to keep the identity and cache loader on one route.
func (c *Client) ResponseCacheIdentity(ctx context.Context, provider, operation, value, mediaType string, cred Credential) (string, error) {
	key, _, err := c.metadataCacheKey(ctx, provider, operation, value, mediaType, cred)
	return key, err
}

func decodeMetadataSearch(raw json.RawMessage) ([]Metadata, error) {
	var values []Metadata
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	if err := d.Decode(&values); err != nil {
		return nil, err
	}
	for _, value := range values {
		if value.ID == "" {
			return nil, errors.New("metadata cache entry has no ID")
		}
	}
	return values, nil
}
func decodeMetadataDetails(raw json.RawMessage) (*Metadata, error) {
	var value *Metadata
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	if err := d.Decode(&value); err != nil {
		return nil, err
	}
	if value == nil || value.ID == "" {
		return nil, errors.New("metadata details cache entry has no ID")
	}
	return value, nil
}
