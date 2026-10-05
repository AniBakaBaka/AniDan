// SPDX-License-Identifier: AGPL-3.0-or-later
// Protocol mappings are derived from Misaka 01751526f6e4154bcc8f517481d02b68cb2684a9.
package integration

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/AniBakaBaka/AniDan/internal/boundedcache"
	"github.com/AniBakaBaka/AniDan/internal/cachebackend"
	"github.com/AniBakaBaka/AniDan/internal/proxyroute"
	"github.com/AniBakaBaka/AniDan/internal/safelog"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type Settings func(context.Context, string, string) string
type Credential struct {
	AccessToken, RefreshToken, ClientID, ClientSecret, RedirectURI string
	ExpiresAt                                                      time.Time
}
type Season struct {
	AirDate      *string  `json:"airDate"`
	EpisodeCount int      `json:"episodeCount"`
	ID           int64    `json:"id"`
	Name         string   `json:"name"`
	SeasonNumber int      `json:"seasonNumber"`
	PosterPath   *string  `json:"posterPath"`
	Aliases      []string `json:"aliases"`
}
type Metadata struct {
	ID, Title, Type, TMDBID, IMDBID, TVDBID, DoubanID, BangumiID, NameEn, NameJp, NameRomaji, ImageURL, Details, Provider string
	Year                                                                                                                  int
	AliasesCn, AliasesJp                                                                                                  []string
	SupportsEpisodeURLs                                                                                                   *bool
	Seasons                                                                                                               []Season
	Extra                                                                                                                 map[string]any
}

func nullable(v string) any {
	if v == "" {
		return nil
	}
	return v
}
func (m Metadata) MarshalJSON() ([]byte, error) {
	cn, jp := m.AliasesCn, m.AliasesJp
	if cn == nil {
		cn = []string{}
	}
	if jp == nil {
		jp = []string{}
	}
	var year any
	if m.Year != 0 {
		year = m.Year
	}
	return json.Marshal(map[string]any{"id": m.ID, "title": m.Title, "type": nullable(m.Type), "tmdbId": nullable(m.TMDBID), "imdbId": nullable(m.IMDBID), "tvdbId": nullable(m.TVDBID), "doubanId": nullable(m.DoubanID), "bangumiId": nullable(m.BangumiID), "nameEn": nullable(m.NameEn), "nameJp": nullable(m.NameJp), "nameRomaji": nullable(m.NameRomaji), "aliasesCn": cn, "aliasesJp": jp, "imageUrl": nullable(m.ImageURL), "details": nullable(m.Details), "year": year, "supportsEpisodeUrls": m.SupportsEpisodeURLs, "seasons": m.Seasons, "extra": m.Extra, "provider": nullable(m.Provider)})
}

type Error struct {
	Provider string
	Status   int
	Kind     string
}

func (e *Error) Error() string { return fmt.Sprintf("%s: %s (HTTP %d)", e.Provider, e.Kind, e.Status) }
func Status(err error) int {
	var x *Error
	if errors.As(err, &x) {
		return x.Status
	}
	if errors.Is(err, context.Canceled) {
		return 499
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return 504
	}
	return 502
}

type tokenCache struct {
	value, key string
	expiry     time.Time
}
type Client struct {
	// Cache is shared and owned by Server; assign before first use. Close does not close it.
	Cache                *cachebackend.Service
	responseLogMask      atomic.Uint64
	metadataCacheOnce    sync.Once
	metadataSearchCache  *boundedcache.Cache[[]byte]
	metadataDetailsCache *boundedcache.Cache[[]byte]

	SetSettings func(context.Context, map[string]string) error
	tvdbMu      sync.Mutex
	HTTP        *http.Client
	Settings    Settings
	mu          sync.Mutex
	tvdb        tokenCache
	aiCache     map[string]aiCacheEntry
	aiMetrics   []AIMetric
	OnAIMetric  func(AIMetric)
	// Routing is supplied by the server's trusted route policy. It takes
	// precedence over the legacy ProxyURL callback and never reads trust from settings.
	Routing         func(context.Context, string) (proxyroute.Config, error)
	ProxyURL        func(context.Context, string) (string, error)
	oauthLocks      sync.Map
	sourceItems     map[string]sourceCacheItem
	proxyClients    map[string]*http.Client
	proxyTransports map[string]interface{ CloseIdleConnections() }
}

func NewClient(settings Settings) *Client {
	return &Client{Settings: settings, HTTP: &http.Client{Timeout: 0, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) > 4 {
			return errors.New("too many redirects")
		}
		if len(via) > 0 && (req.URL.Scheme != via[0].URL.Scheme || req.URL.Host != via[0].URL.Host) {
			return errors.New("cross-origin redirect refused")
		}
		return nil
	}}, aiCache: map[string]aiCacheEntry{}}
}
func (c *Client) setting(ctx context.Context, key, def string) string {
	if c.Settings == nil {
		return def
	}
	return c.Settings(ctx, key, def)
}
func (c *Client) base(ctx context.Context, key, def string) string {
	return strings.TrimRight(c.setting(ctx, key, def), "/")
}
func str(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case json.Number:
		return x.String()
	default:
		return fmt.Sprint(x)
	}
}
func integer(v any) int { n, _ := strconv.Atoi(str(v)); return n }
func object(v any) map[string]any {
	m, _ := v.(map[string]any)
	if m == nil {
		return map[string]any{}
	}
	return m
}
func array(v any) []any { a, _ := v.([]any); return a }
func nested(v any, keys ...string) any {
	for _, key := range keys {
		v = object(v)[key]
	}
	return v
}
func first(values ...any) string {
	for _, v := range values {
		if s := str(v); s != "" {
			return s
		}
	}
	return ""
}
func ptr(v string) *string {
	if v == "" {
		return nil
	}
	return &v
}
func unique(v []string) []string {
	out := []string{}
	seen := map[string]bool{}
	for _, s := range v {
		s = strings.TrimSpace(s)
		if s != "" && !seen[s] {
			out = append(out, s)
			seen[s] = true
		}
	}
	return out
}
func stringsOf(v any) []string {
	out := []string{}
	if s, ok := v.(string); ok {
		return []string{s}
	}
	for _, x := range array(v) {
		if s, ok := x.(string); ok {
			out = append(out, s)
		}
	}
	return out
}
func year(v any) int {
	s := str(v)
	if len(s) >= 4 {
		n, _ := strconv.Atoi(s[:4])
		if n > 1800 && n < 3000 {
			return n
		}
	}
	return 0
}
func decode(data []byte, v any) error {
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	if err := d.Decode(v); err != nil {
		return err
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return errors.New("trailing JSON")
	}
	return nil
}
func (c *Client) request(ctx context.Context, provider, method, raw string, q url.Values, headers map[string]string, body any, maxBytes int64) ([]byte, error) {
	deadline, hasDeadline := ctx.Deadline()
	if !hasDeadline || time.Until(deadline) > 5*time.Minute {
		timeout := 30 * time.Second
		if hasDeadline {
			timeout = 5 * time.Minute
		}
		bounded, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		ctx = bounded
	}
	ctx, route, err := c.metadataRouteContext(ctx, provider)
	if err != nil {
		return nil, err
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil {
		return nil, &Error{provider, 400, "invalid configured endpoint"}
	}
	values := u.Query()
	for k, v := range q {
		values[k] = v
	}
	u.RawQuery = values.Encode()
	var r io.Reader
	if body != nil {
		switch x := body.(type) {
		case url.Values:
			r = strings.NewReader(x.Encode())
		default:
			b, e := json.Marshal(body)
			if e != nil {
				return nil, e
			}
			r = bytes.NewReader(b)
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), r)
	if err != nil {
		return nil, &Error{provider, 400, "invalid request"}
	}
	req.Header.Set("User-Agent", "AniDan/0.1 (+https://github.com/AniBakaBaka/AniDan)")
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
		if _, ok := body.(url.Values); ok {
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		}
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	client := c.HTTP
	if client == nil {
		client = NewClient(nil).HTTP
	}
	if route.enabled {
		client, err = c.routingClient(client, route.config)
		if err != nil {
			return nil, err
		}
	}
	logResponse := c.ResponseLoggingEnabled(provider)
	res, err := client.Do(req)
	if err != nil {
		if logResponse {
			safelog.Response(ctx, "metadata", provider, 0, "network-error", nil, false)
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if errors.Is(err, proxyroute.ErrUntrustedGateway) {
			return nil, &Error{provider, 412, proxyroute.ErrUntrustedGateway.Error()}
		}
		if errors.Is(err, context.Canceled) {
			return nil, context.Canceled
		}
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, context.DeadlineExceeded
		}
		return nil, &Error{provider, 502, "network request failed"}
	}
	defer res.Body.Close()
	if maxBytes <= 0 {
		maxBytes = 16 << 20
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		if logResponse {
			limit := min(maxBytes, int64(safelog.MaxInputBytes))
			b, err := io.ReadAll(io.LimitReader(res.Body, limit+1))
			safelog.Response(ctx, "metadata", provider, res.StatusCode, "http-error", b, err == nil && int64(len(b)) <= limit)
		}
		return nil, &Error{provider, res.StatusCode, "upstream request failed"}
	}
	b, err := io.ReadAll(io.LimitReader(res.Body, maxBytes+1))
	if err != nil {
		if logResponse {
			safelog.Response(ctx, "metadata", provider, res.StatusCode, "read-error", b, false)
		}
		return nil, &Error{provider, 502, "response read failed"}
	}
	if int64(len(b)) > maxBytes {
		if logResponse {
			safelog.Response(ctx, "metadata", provider, res.StatusCode, "response-limit", b, false)
		}
		return nil, &Error{provider, 502, "response too large"}
	}
	if logResponse {
		safelog.Response(ctx, "metadata", provider, res.StatusCode, "received", b, true)
	}
	return b, nil
}
func (c *Client) json(ctx context.Context, provider, method, endpoint string, q url.Values, h map[string]string, body any) (any, error) {
	b, e := c.request(ctx, provider, method, endpoint, q, h, body, 16<<20)
	if e != nil {
		return nil, e
	}
	var v any
	if decode(b, &v) != nil {
		return nil, &Error{provider, 502, "invalid upstream JSON"}
	}
	return v, nil
}
func safeID(id string) error {
	if id == "" || len(id) > 200 || strings.ContainsAny(id, "/\\?#\x00\r\n") {
		return &Error{"metadata", 400, "invalid item ID"}
	}
	return nil
}

var Providers = []string{"bangumi", "tmdb", "tvdb", "imdb", "douban", "trakt", "anibt", "360"}

func KnownProvider(p string) bool {
	for _, v := range Providers {
		if p == v {
			return true
		}
	}
	return false
}
func (c *Client) searchUncached(ctx context.Context, p, keyword, mediaType string, cred Credential) ([]Metadata, error) {
	keyword = strings.TrimSpace(keyword)
	if keyword == "" || len(keyword) > 2048 {
		return nil, &Error{p, 400, "keyword must be non-empty and bounded"}
	}
	switch p {
	case "bangumi":
		return c.bangumiSearch(ctx, keyword, cred)
	case "tmdb":
		return c.tmdbSearch(ctx, keyword, mediaType)
	case "tvdb":
		return c.tvdbSearch(ctx, keyword, mediaType)
	case "imdb":
		return c.imdbSearch(ctx, keyword, mediaType)
	case "douban":
		return c.doubanSearch(ctx, keyword)
	case "trakt":
		return c.traktSearch(ctx, keyword, cred)
	case "anibt":
		return c.anibtSearch(ctx, keyword, mediaType)
	case "360":
		return c.so360Search(ctx, keyword)
	}
	return nil, &Error{p, 404, "unknown metadata provider"}
}
func (c *Client) detailsUncached(ctx context.Context, p, id, mediaType string, cred Credential) (*Metadata, error) {
	if e := safeID(id); e != nil {
		return nil, e
	}
	switch p {
	case "bangumi":
		return c.bangumiDetails(ctx, id, cred)
	case "tmdb":
		return c.tmdbDetails(ctx, id, mediaType)
	case "tvdb":
		return c.tvdbDetails(ctx, id, mediaType)
	case "imdb":
		return c.imdbDetails(ctx, id, mediaType)
	case "douban":
		return c.doubanDetails(ctx, id)
	case "trakt":
		return c.traktDetails(ctx, id, cred)
	case "anibt":
		return c.anibtDetails(ctx, id)
	case "360":
		return c.so360Details(ctx, id)
	}
	return nil, &Error{p, 404, "unknown metadata provider"}
}

type metadataRouteKey struct{}

// A route is frozen for one logical metadata operation, including cache loading
// and provider flows that need multiple HTTP requests. The policy is checked
// again before cache access and sending so revocation remains effective.
type metadataRoute struct {
	client   *Client
	provider string
	config   proxyroute.Config
	enabled  bool
}

// WithRoutingContext binds one route selection for a caller-owned operation.
// Pass the returned context to ResponseCacheIdentity and the cache loader so
// both use the same selection. Binding never bypasses later authorization checks.
func (c *Client) WithRoutingContext(ctx context.Context, provider string) (context.Context, error) {
	bound, _, err := c.metadataRouteContext(ctx, provider)
	return bound, err
}

// RoutingConfig exposes the selected route without borrowing the metadata
// client's transport, cookies or headers. Bind ctx with WithRoutingContext
// first when the route must match a subsequent metadata operation. A bound
// route remains fixed, but its authorization is checked again on every call.
func (c *Client) RoutingConfig(ctx context.Context, provider string) (proxyroute.Config, error) {
	_, route, err := c.metadataRouteContext(ctx, provider)
	return route.config, err
}

func (c *Client) metadataRouteContext(ctx context.Context, provider string) (context.Context, metadataRoute, error) {
	if err := ctx.Err(); err != nil {
		return ctx, metadataRoute{}, err
	}
	route, bound := ctx.Value(metadataRouteKey{}).(metadataRoute)
	if !bound || route.client != c || route.provider != provider {
		route = metadataRoute{client: c, provider: provider}
		var err error
		if c.Routing != nil {
			route.enabled = true
			route.config, err = c.Routing(ctx, provider)
		} else if c.ProxyURL != nil {
			route.enabled = true
			route.config.ProxyURL, err = c.ProxyURL(ctx, provider)
		}
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return ctx, route, context.Canceled
			}
			if errors.Is(err, context.DeadlineExceeded) {
				return ctx, route, context.DeadlineExceeded
			}
			status := 502
			var typed *Error
			if errors.As(err, &typed) {
				status = typed.Status
			}
			return ctx, route, &Error{provider, status, "proxy configuration unavailable"}
		}
		// The legacy empty-proxy result left the caller's HTTP client intact.
		// Explicit Routing delegates direct and inherited client construction to
		// proxyroute, which decides whether the base transport should be retained.
		if c.Routing == nil && route.config.ProxyURL == "" {
			route.config.InheritTransport = true
			route.enabled = false
		}
		ctx = context.WithValue(ctx, metadataRouteKey{}, route)
	}
	if route.config.Blocked {
		return ctx, route, &Error{provider, 412, proxyroute.ErrUntrustedGateway.Error()}
	}
	if err := proxyroute.Validate(route.config); err != nil {
		return ctx, route, &Error{provider, 400, "invalid proxy configuration"}
	}
	if route.config.AccelerateURL != "" && route.config.Authorize == nil {
		return ctx, route, &Error{provider, 412, proxyroute.ErrUntrustedGateway.Error()}
	}
	if route.config.Authorize != nil {
		if err := route.config.Authorize(ctx); err != nil {
			if errors.Is(err, context.Canceled) {
				return ctx, route, context.Canceled
			}
			if errors.Is(err, context.DeadlineExceeded) {
				return ctx, route, context.DeadlineExceeded
			}
			return ctx, route, &Error{provider, 412, proxyroute.ErrUntrustedGateway.Error()}
		}
	}
	return ctx, route, nil
}

func (c *Client) proxyClient(base *http.Client, rawProxy string) (*http.Client, error) {
	return c.routingClient(base, proxyroute.Config{ProxyURL: rawProxy})
}

func (c *Client) routingClient(base *http.Client, config proxyroute.Config) (*http.Client, error) {
	if err := proxyroute.Validate(config); err != nil {
		return nil, &Error{"proxy", 400, "invalid proxy configuration"}
	}
	// Authorize is deliberately excluded from the opaque route identity, so
	// enforce its presence before a previous authorized client can be reused.
	if !config.Blocked && config.AccelerateURL != "" && config.Authorize == nil {
		return nil, &Error{"proxy", 412, proxyroute.ErrUntrustedGateway.Error()}
	}
	hash := sha256.Sum256([]byte(proxyroute.Identity(config) + fmt.Sprintf(":%p:%p:%d:%p:%p", base, base.Transport, base.Timeout, base.Jar, base.CheckRedirect)))
	signature := hex.EncodeToString(hash[:])
	c.mu.Lock()
	defer c.mu.Unlock()
	if client := c.proxyClients[signature]; client != nil {
		return client, nil
	}
	client, err := proxyroute.Client(base, config)
	if err != nil {
		return nil, &Error{"proxy", 502, "proxy requires a standard HTTP transport"}
	}
	if c.proxyClients == nil {
		c.proxyClients = map[string]*http.Client{}
		c.proxyTransports = map[string]interface{ CloseIdleConnections() }{}
	}
	if len(c.proxyClients) >= 16 {
		for key, t := range c.proxyTransports {
			t.CloseIdleConnections()
			delete(c.proxyTransports, key)
			delete(c.proxyClients, key)
			break
		}
	}
	c.proxyClients[signature] = client
	c.proxyTransports[signature] = client
	return client, nil
}
func (c *Client) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, t := range c.proxyTransports {
		t.CloseIdleConnections()
	}
	c.proxyClients = nil
	c.proxyTransports = nil
	if c.HTTP != nil {
		c.HTTP.CloseIdleConnections()
	}
}
