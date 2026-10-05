// SPDX-License-Identifier: AGPL-3.0-only
package server

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/AniBakaBaka/AniDan/internal/proxyroute"
)

var errLocalTMDBPosterRouteUnsupported = errors.New("TMDB poster image route is unsupported")
var errLocalTMDBPosterConfigurationChanged = errors.New("TMDB poster configuration changed")
var errLocalTMDBPosterBoundary = errors.New("TMDB poster image URL is outside its configured boundary")

// Only package-local synthetic fixtures supply this context value. Production
// has no settings or Server field that can replace DNS, dialing, or TLS roots.
type localTMDBPosterDependenciesKey struct{}

// net/http may detach dialing from a request's cancellation. Explicit proxy
// handshakes must still stop with the image request that authorized them.
type localTMDBPosterRequestContextKey struct{}

func (s *Server) localTMDBPosterImageBase(ctx context.Context) string {
	base := "https://image.tmdb.org/t/p/w500"
	if s.Metadata != nil && s.Metadata.Settings != nil {
		base = s.Metadata.Settings(ctx, "tmdbImageBaseUrl", base)
	}
	base = strings.TrimRight(base, "/")
	if !strings.Contains(base, "/t/p/") {
		base += "/t/p/w500"
	}
	return base
}

func localTMDBPosterURL(raw string) (*url.URL, error) {
	u, err := libPosterURL(raw)
	if err != nil || len(raw) > 2048 || strings.TrimSpace(raw) != raw || strings.ContainsAny(raw, "\\\r\n\x00#") || u.Opaque != "" || u.RawQuery != "" || u.ForceQuery || strings.HasSuffix(u.Host, ":") {
		return nil, errLocalTMDBPosterBoundary
	}
	for _, segment := range strings.Split(u.Path, "/") {
		if segment == "." || segment == ".." || strings.Contains(segment, "\\") {
			return nil, errLocalTMDBPosterBoundary
		}
	}
	return u, nil
}

func localTMDBPosterSameOrigin(a, b *url.URL) bool {
	port := func(u *url.URL) string {
		if p := u.Port(); p != "" {
			return p
		}
		if u.Scheme == "https" {
			return "443"
		}
		return "80"
	}
	return a.Scheme == b.Scheme && strings.EqualFold(a.Hostname(), b.Hostname()) && port(a) == port(b)
}

// This is deliberately separate from libPosterClient: only a metadata-derived
// TMDB URL can use its already-selected proxy or still-authorized accelerate
// route. Inherited active proxies and opaque wrappers remain unsupported.
func (s *Server) localTMDBPosterClient(ctx context.Context, route proxyroute.Config, imageBase, imageURL string) (*localTMDBImageClient, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	origin, err := localTMDBPosterURL(imageBase)
	if err != nil {
		return nil, errLocalTMDBPosterBoundary
	}
	initial, err := localTMDBPosterURL(imageURL)
	if err != nil || !localTMDBPosterSameOrigin(origin, initial) || !strings.HasPrefix(imageURL, imageBase+"/") {
		return nil, errLocalTMDBPosterBoundary
	}
	if err := proxyroute.Validate(route); err != nil {
		return nil, errLocalTMDBPosterRouteUnsupported
	}
	if route.Blocked || route.AccelerateURL != "" && (route.Authorize == nil || route.AuthorizationIdentity == "") {
		return nil, proxyroute.ErrUntrustedGateway
	}
	metadata := s.Metadata
	if metadata == nil {
		return nil, errLocalTMDBPosterConfigurationChanged
	}
	identity := proxyroute.Identity(route)
	validate := func(ctx context.Context, target *url.URL) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if s.Metadata != metadata || s.localTMDBPosterImageBase(ctx) != imageBase {
			return errLocalTMDBPosterConfigurationChanged
		}
		current := proxyroute.Config{InheritTransport: true}
		var err error
		if metadata.Routing != nil {
			current, err = metadata.Routing(ctx, "tmdb")
		} else if metadata.ProxyURL != nil {
			current.ProxyURL, err = metadata.ProxyURL(ctx, "tmdb")
			current.InheritTransport = current.ProxyURL == ""
		}
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return errLocalTMDBPosterConfigurationChanged
		}
		if proxyroute.Identity(current) != identity {
			return errLocalTMDBPosterConfigurationChanged
		}
		if err := proxyroute.Validate(current); err != nil {
			return errLocalTMDBPosterConfigurationChanged
		}
		if current.AccelerateURL != "" && (current.Authorize == nil || current.AuthorizationIdentity == "") {
			return proxyroute.ErrUntrustedGateway
		}
		if route.ProxyURL != "" && route.Authorize != nil && current.Authorize == nil {
			return proxyroute.ErrUntrustedGateway
		}
		// The route identity deliberately excludes callbacks. Recheck both the
		// bound approval and its current policy, including callback removal.
		for _, authorize := range []func(context.Context) error{route.Authorize, current.Authorize} {
			if authorize == nil {
				continue
			}
			if err := authorize(ctx); err != nil {
				if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
					return err
				}
				return proxyroute.ErrUntrustedGateway
			}
		}
		if route.InheritTransport {
			var transport http.RoundTripper = http.DefaultTransport
			if metadata.HTTP != nil && metadata.HTTP.Transport != nil {
				transport = metadata.HTTP.Transport
			}
			standard, ok := transport.(*http.Transport)
			if !ok {
				return errLocalTMDBPosterRouteUnsupported
			}
			if standard.Proxy != nil {
				request, _ := http.NewRequestWithContext(ctx, "GET", target.String(), nil)
				proxy, err := standard.Proxy(request)
				if err != nil || proxy != nil {
					return errLocalTMDBPosterRouteUnsupported
				}
			}
		}
		return nil
	}
	if err := validate(ctx, initial); err != nil {
		return nil, err
	}
	deps, ok := ctx.Value(localTMDBPosterDependenciesKey{}).(compatProbeDependencies)
	if !ok {
		deps = compatProbeDefaults()
	}
	wireOrigin := origin
	if route.AccelerateURL != "" {
		canonical, _ := proxyroute.CanonicalGateway(route.AccelerateURL)
		wireOrigin, _ = url.Parse(canonical)
	} else if route.ProxyURL != "" {
		wireOrigin, _ = url.Parse(route.ProxyURL) // proxyroute.Validate checked it.
	}
	port := wireOrigin.Port()
	if port == "" {
		port = "80"
		if wireOrigin.Scheme == "https" {
			port = "443"
		} else if wireOrigin.Scheme == "socks5" || wireOrigin.Scheme == "socks5h" {
			port = "1080"
		}
	}
	transport := &http.Transport{Proxy: nil, ForceAttemptHTTP2: true, DisableKeepAlives: true, TLSHandshakeTimeout: 10 * time.Second, ResponseHeaderTimeout: 10 * time.Second, MaxConnsPerHost: 2, MaxResponseHeaderBytes: 32 << 10, TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: deps.roots}}
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		if route.ProxyURL != "" {
			requestContext, ok := ctx.Value(localTMDBPosterRequestContextKey{}).(context.Context)
			if !ok {
				return nil, errLocalTMDBPosterBoundary
			}
			ctx = requestContext
		}
		if err := validate(ctx, initial); err != nil {
			return nil, err
		}
		host, requestedPort, err := net.SplitHostPort(address)
		if err != nil || !strings.EqualFold(host, wireOrigin.Hostname()) || requestedPort != port {
			return nil, errLocalTMDBPosterBoundary
		}
		// Only the exact selected proxy or approved gateway may resolve
		// privately. The logical image origin must pass a separate public check.
		ips, err := compatAddresses(ctx, host, route.AccelerateURL != "" || route.ProxyURL != "", deps)
		if err != nil {
			return nil, errors.New("TMDB poster DNS safety check failed")
		}
		for _, ip := range ips {
			if err := validate(ctx, initial); err != nil {
				return nil, err
			}
			conn, err := deps.dial(ctx, network, net.JoinHostPort(ip.String(), port))
			if err == nil {
				if route.ProxyURL != "" {
					if err := validate(ctx, initial); err != nil {
						conn.Close()
						return nil, err
					}
					return &localTMDBPosterProxyConn{Conn: conn, validate: func() error { return validate(ctx, initial) }, stop: context.AfterFunc(ctx, func() { conn.Close() })}, nil
				}
				return conn, nil
			}
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
		}
		return nil, errors.New("TMDB poster connection failed")
	}
	base := &http.Client{Transport: transport, Timeout: 25 * time.Second}
	// Inheritance has already been inspected. Never import its client, Jar,
	// headers, dialer, TLS options or ambient proxy into the image transport.
	selected := route
	selected.InheritTransport = false
	client, err := proxyroute.Client(base, selected)
	if err != nil {
		return nil, errLocalTMDBPosterRouteUnsupported
	}
	client.CheckRedirect = func(request *http.Request, via []*http.Request) error {
		if len(via) >= 3 {
			return errors.New("too many image redirects")
		}
		target, err := localTMDBPosterURL(request.URL.String())
		if err != nil || len(via) == 0 || via[0].URL.String() != imageURL || !localTMDBPosterSameOrigin(origin, target) || !strings.HasPrefix(target.String(), imageBase+"/") {
			return errLocalTMDBPosterBoundary
		}
		return nil
	}
	client.Transport = &localTMDBPosterTransport{base: client.Transport, imageBase: imageBase, origin: origin, validate: validate, accelerate: route.AccelerateURL != "", proxy: route.ProxyURL != "", deps: deps}
	return &localTMDBImageClient{client: client, imageURL: imageURL}, nil
}

type localTMDBImageClient struct {
	client   *http.Client
	imageURL string
}

func (c *localTMDBImageClient) CloseIdleConnections() { c.client.CloseIdleConnections() }

func (c *localTMDBImageClient) Do(request *http.Request) (*http.Response, error) {
	if request.URL == nil || request.URL.String() != c.imageURL || request.Method != "GET" || request.Body != nil {
		if request.Body != nil {
			request.Body.Close()
		}
		return nil, errLocalTMDBPosterBoundary
	}
	response, err := c.client.Do(request)
	if err == nil {
		return response, nil
	}
	// http.Client errors embed URLs; optional job diagnostics must never carry
	// a metadata-derived path, query, or a transport callback's private details.
	if request.Context().Err() != nil {
		return nil, request.Context().Err()
	}
	for _, known := range []error{context.Canceled, context.DeadlineExceeded, errLocalTMDBPosterRouteUnsupported, errLocalTMDBPosterConfigurationChanged, errLocalTMDBPosterBoundary, proxyroute.ErrUntrustedGateway} {
		if errors.Is(err, known) {
			return nil, known
		}
	}
	return nil, errors.New("TMDB poster image request failed")
}

type localTMDBPosterTransport struct {
	base       http.RoundTripper
	imageBase  string
	origin     *url.URL
	validate   func(context.Context, *url.URL) error
	accelerate bool
	proxy      bool
	deps       compatProbeDependencies
}

func (t *localTMDBPosterTransport) CloseIdleConnections() {
	if closer, ok := t.base.(interface{ CloseIdleConnections() }); ok {
		closer.CloseIdleConnections()
	}
}

func (t *localTMDBPosterTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	dispatched := false
	defer func() {
		if !dispatched && request.Body != nil {
			request.Body.Close()
		}
	}()
	if request.URL == nil || request.Method != "GET" || request.Body != nil {
		return nil, errLocalTMDBPosterBoundary
	}
	target, err := localTMDBPosterURL(request.URL.String())
	if err != nil || !localTMDBPosterSameOrigin(t.origin, target) || !strings.HasPrefix(target.String(), t.imageBase+"/") {
		return nil, errLocalTMDBPosterBoundary
	}
	if err := t.validate(request.Context(), target); err != nil {
		return nil, err
	}
	if t.accelerate || t.proxy {
		// The selected intermediary resolves the logical origin itself. Go's
		// SOCKS5 and SOCKS5h both send the hostname to the proxy. This local
		// public-address safety check does not pin the remote origin's IP.
		if _, err := compatAddresses(request.Context(), target.Hostname(), false, t.deps); err != nil {
			if request.Context().Err() != nil {
				return nil, request.Context().Err()
			}
			return nil, errors.New("TMDB poster DNS safety check failed")
		}
		if err := t.validate(request.Context(), target); err != nil {
			return nil, err
		}
	}
	// Construct a fresh GET instead of cloning request fields: metadata header,
	// cookie, trailer, body-replay and transfer state must never follow the URL.
	wireContext := request.Context()
	if t.proxy {
		wireContext = context.WithValue(wireContext, localTMDBPosterRequestContextKey{}, wireContext)
	}
	wire, err := http.NewRequestWithContext(wireContext, "GET", target.String(), nil)
	if err != nil {
		return nil, errLocalTMDBPosterBoundary
	}
	wire.Header.Set("Accept", "image/jpeg,image/png,image/webp,image/gif")
	wire.Header.Set("User-Agent", "AniDan/1.0")
	dispatched = true
	response, err := t.base.RoundTrip(wire)
	if err == nil && t.proxy && response.Body != nil {
		response.Body = &localTMDBPosterProxyBody{ReadCloser: response.Body, ctx: request.Context()}
	}
	return response, err
}

// Cancellation can close the proxy socket after headers have been returned.
// Keep body-read failures under the same cancellation and redaction boundary.
type localTMDBPosterProxyBody struct {
	io.ReadCloser
	ctx context.Context
}

func (b *localTMDBPosterProxyBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if err != nil {
		if b.ctx.Err() != nil {
			return n, b.ctx.Err()
		}
		if !errors.Is(err, io.EOF) {
			return n, errors.New("TMDB poster image response failed")
		}
	}
	return n, err
}

// Keep route approval and cancellation attached to the proxy handshake and
// subsequent image writes, including CONNECT and TLS inside that connection.
type localTMDBPosterProxyConn struct {
	net.Conn
	validate func() error
	stop     func() bool
}

func (c *localTMDBPosterProxyConn) Write(b []byte) (int, error) {
	if err := c.validate(); err != nil {
		c.Close()
		return 0, err
	}
	return c.Conn.Write(b)
}

func (c *localTMDBPosterProxyConn) Close() error {
	c.stop()
	return c.Conn.Close()
}
