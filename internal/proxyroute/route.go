// SPDX-License-Identifier: AGPL-3.0-only
// Package proxyroute configures one explicit outbound route. Gateway approval is
// owned by the caller; this package never derives authorization from settings.
package proxyroute

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const maxGatewayBytes = 2048

// Config selects inherited transport, explicit direct, HTTP/SOCKS, accelerate,
// or locally blocked routing. Inheritance cannot override route authorization.
// A blocked route may retain a bounded attempted gateway for cache isolation.
// Authorize is evaluated at the last possible point before each wire request.
// AuthorizationIdentity is the caller's opaque approval generation, not a secret.
type Config struct {
	InheritTransport      bool
	ProxyURL              string
	AccelerateURL         string
	Blocked               bool
	Authorize             func(context.Context) error `json:"-"`
	AuthorizationIdentity string
}

// ErrUntrustedGateway is a fixed diagnostic; callback error details are private.
var ErrUntrustedGateway = errors.New("untrusted accelerate gateway: approval required")

// CanonicalGateway returns the exact HTTPS authority and optional clean prefix
// that may be approved. Errors never contain the supplied URL.
func CanonicalGateway(raw string) (string, error) {
	invalid := errors.New("invalid accelerate gateway: HTTPS URL with a clean optional prefix required")
	if raw == "" || len(raw) > maxGatewayBytes || strings.TrimSpace(raw) != raw || strings.ContainsAny(raw, "\\\r\n\x00") {
		return "", invalid
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.Opaque != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.Contains(raw, "#") {
		return "", invalid
	}
	host := strings.ToLower(u.Hostname())
	if strings.Contains(host, "%") || !validHost(host) {
		return "", invalid
	}
	port := u.Port()
	if strings.HasSuffix(u.Host, ":") {
		return "", invalid
	}
	if port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return "", invalid
		}
		port = strconv.Itoa(n)
	}
	// Prefixes deliberately exclude escaped/reserved components and dot segments.
	// This avoids path normalization disagreements between a gateway and origin.
	prefix := strings.TrimRight(u.Path, "/")
	if u.RawPath != "" || strings.Contains(prefix, "//") {
		return "", invalid
	}
	for _, segment := range strings.Split(prefix, "/") {
		if segment == "." || segment == ".." {
			return "", invalid
		}
		for _, r := range segment {
			if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || strings.ContainsRune("-._~", r)) {
				return "", invalid
			}
		}
	}
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	if port != "" && port != "443" {
		host += ":" + port
	}
	return "https://" + host + prefix, nil
}

func validHost(host string) bool {
	if net.ParseIP(host) != nil {
		return true
	}
	if len(host) > 253 || host == "" {
		return false
	}
	for _, label := range strings.Split(host, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, r := range label {
			if !((r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-') {
				return false
			}
		}
	}
	return true
}

func proxyURL(raw string) (*url.URL, error) {
	invalid := errors.New("invalid proxy configuration")
	if len(raw) > 65536 || strings.ContainsAny(raw, "\r\n\x00") {
		return nil, invalid
	}
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" || u.Opaque != "" || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.Contains(raw, "#") {
		return nil, invalid
	}
	switch u.Scheme {
	case "http", "https", "socks5", "socks5h":
	default:
		return nil, invalid
	}
	if u.User != nil {
		password, _ := u.User.Password()
		if strings.ContainsAny(u.User.Username()+password, "\r\n\x00") {
			return nil, invalid
		}
	}
	return u, nil
}

// Validate is structural only. Blocked routes can be installed at startup, and
// callbacks are checked using the real request context, never during validation.
func Validate(cfg Config) error {
	if cfg.InheritTransport {
		if cfg.ProxyURL != "" || cfg.AccelerateURL != "" || cfg.Blocked || cfg.Authorize != nil || cfg.AuthorizationIdentity != "" {
			return errors.New("inherited transport cannot override route or authorization")
		}
		return nil
	}
	if len(cfg.AuthorizationIdentity) > 4096 || len(cfg.AccelerateURL) > maxGatewayBytes {
		return errors.New("outbound route configuration exceeds limits")
	}
	if cfg.Blocked {
		if cfg.ProxyURL != "" {
			return errors.New("blocked route cannot select an HTTP proxy")
		}
		return nil
	}
	if cfg.ProxyURL != "" && cfg.AccelerateURL != "" {
		return errors.New("outbound route must select only one gateway or proxy")
	}
	if cfg.AccelerateURL != "" {
		_, err := CanonicalGateway(cfg.AccelerateURL)
		return err
	}
	if cfg.ProxyURL != "" {
		_, err := proxyURL(cfg.ProxyURL)
		return err
	}
	return nil
}

// Identity is stable and opaque, including for a blocked or invalid selection.
// Callers must still Validate and authorize a route before using a cache hit.
func Identity(cfg Config) string {
	gateway := cfg.AccelerateURL
	if canonical, err := CanonicalGateway(gateway); err == nil {
		gateway = canonical
	}
	b, _ := json.Marshal([]any{"outbound-route-v2", cfg.InheritTransport, cfg.ProxyURL, gateway, cfg.Blocked, cfg.AuthorizationIdentity})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// Client clones a client and its standard transport; published clients and
// in-flight requests are never changed. Only our own wrappers are unwrapped.
// Active accelerate routes require an explicit caller-owned authorizer; a URL
// alone never grants permission to forward private requests.
func Client(base *http.Client, cfg Config) (*http.Client, error) {
	if err := Validate(cfg); err != nil {
		return nil, err
	}
	if !cfg.Blocked && cfg.AccelerateURL != "" && cfg.Authorize == nil {
		return nil, ErrUntrustedGateway
	}
	if base == nil {
		base = &http.Client{}
	}
	next := *base
	if cfg.InheritTransport {
		// Preserve nil/default transport, ambient proxy policy, custom wrappers,
		// and any pre-existing authorization checks exactly as supplied.
		return &next, nil
	}
	underlying := base.Transport
	for {
		own, ok := underlying.(*routeTransport)
		if !ok {
			break
		}
		underlying = own.source
	}
	if underlying == nil {
		underlying = http.DefaultTransport
	}
	if cfg.Blocked {
		next.Transport = &routeTransport{base: underlying, source: underlying, cfg: cfg}
		return &next, nil
	}
	standard, ok := underlying.(*http.Transport)
	if !ok {
		return nil, errors.New("outbound routing requires a standard HTTP transport")
	}
	transport := standard.Clone()
	transport.Proxy = nil // direct and accelerate never consult ambient proxy env
	if transport.TLSClientConfig != nil {
		transport.TLSClientConfig = transport.TLSClientConfig.Clone()
		transport.TLSClientConfig.InsecureSkipVerify = false
		if cfg.AccelerateURL != "" {
			transport.TLSClientConfig.ServerName = "" // verify the gateway, not a pinned logical origin
		}
	}
	transport.MaxIdleConns = 128
	transport.MaxIdleConnsPerHost = 16
	transport.MaxConnsPerHost = 16
	transport.IdleConnTimeout = 90 * time.Second
	if cfg.ProxyURL != "" {
		u, _ := proxyURL(cfg.ProxyURL) // Validate already checked this value.
		transport.Proxy = http.ProxyURL(u)
	}
	if cfg.AccelerateURL != "" {
		canonical, _ := CanonicalGateway(cfg.AccelerateURL)
		gateway, _ := url.Parse(canonical)
		next.Transport = &routeTransport{base: transport, source: underlying, cfg: cfg, gateway: gateway}
	} else if cfg.Authorize != nil {
		next.Transport = &routeTransport{base: transport, source: underlying, cfg: cfg}
	} else {
		next.Transport = transport
	}
	return &next, nil
}

type routeTransport struct {
	// source retains the detached pre-route options for later reconfiguration.
	source  http.RoundTripper
	base    http.RoundTripper
	cfg     Config
	gateway *url.URL
}

func (t *routeTransport) CloseIdleConnections() {
	if closer, ok := t.base.(interface{ CloseIdleConnections() }); ok {
		closer.CloseIdleConnections()
	}
}

func (t *routeTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	dispatched := false
	defer func() {
		if !dispatched && req.Body != nil {
			req.Body.Close()
		}
	}()
	if err := req.Context().Err(); err != nil {
		return nil, err
	}
	if t.cfg.Blocked || (t.cfg.AccelerateURL != "" && t.cfg.Authorize == nil) {
		return nil, ErrUntrustedGateway
	}
	if t.cfg.Authorize != nil {
		if err := t.cfg.Authorize(req.Context()); err != nil {
			if errors.Is(err, context.Canceled) {
				return nil, context.Canceled
			}
			if errors.Is(err, context.DeadlineExceeded) {
				return nil, context.DeadlineExceeded
			}
			return nil, ErrUntrustedGateway
		}
	}
	if t.gateway == nil {
		dispatched = true
		return t.base.RoundTrip(req)
	}
	u := req.URL
	if u == nil || (u.Scheme != "https" && u.Scheme != "http") || u.Hostname() == "" || u.User != nil || u.Opaque != "" || len(u.String()) > 65536 {
		return nil, errors.New("invalid logical URL for accelerate route")
	}
	wire := req.Clone(req.Context())
	wireURL := *t.gateway
	wireURL.Path += "/" + u.Scheme + "/" + u.Host + u.Path
	authorityPath := &url.URL{Path: t.gateway.Path + "/" + u.Scheme + "/" + u.Host}
	wireURL.RawPath = authorityPath.EscapedPath() + u.EscapedPath()
	wireURL.RawQuery, wireURL.ForceQuery = u.RawQuery, u.ForceQuery
	wire.URL, wire.Host = &wireURL, ""
	wire.RequestURI = ""
	dispatched = true
	res, err := t.base.RoundTrip(wire)
	if err != nil {
		return nil, &routeError{cause: err}
	}
	if location := res.Header.Get("Location"); location != "" && redirectStatus(res.StatusCode) {
		redirect, err := url.Parse(location)
		// An absolute gateway redirect (or a rewritten gateway path) must not
		// become a new logical origin. Relative and absolute origin redirects
		// remain untouched for the client's existing redirect policy.
		if err != nil || (redirect.Host != "" && strings.EqualFold(redirect.Hostname(), t.gateway.Hostname())) || gatewayPath(redirect.Path, t.gateway.Path) {
			if res.Body != nil {
				res.Body.Close()
			}
			return nil, errors.New("accelerate gateway redirect refused")
		}
	}
	logical := *res
	logical.Request = req
	return &logical, nil
}

func redirectStatus(code int) bool {
	return code == 301 || code == 302 || code == 303 || code == 307 || code == 308
}

func gatewayPath(path, prefix string) bool {
	for _, scheme := range []string{"/http/", "/https/"} {
		if strings.HasPrefix(path, prefix+scheme) {
			return true
		}
	}
	return false
}

type routeError struct{ cause error }

func (e *routeError) Error() string { return "accelerate network request failed" }
func (e *routeError) Unwrap() error { return e.cause }
func (e *routeError) Timeout() bool {
	var timeout interface{ Timeout() bool }
	return errors.As(e.cause, &timeout) && timeout.Timeout()
}
