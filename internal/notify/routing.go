// SPDX-License-Identifier: AGPL-3.0-only
package notify

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// RoutingConfig is an operation-local snapshot of trusted runtime settings.
// Never copy it into stored/public Channel.Config or return it in status errors.
type RoutingConfig struct {
	ProxyMode    string
	ProxyEnabled bool
	ProxyURL     string
	RelayKey     string
}

type routeContextKey struct{}
type outboundRoute struct {
	owner       string
	fingerprint string
	forward     *url.URL
	relay       *url.URL
	relayKey    string
}
type routeTransport struct {
	fingerprint string
	transport   *http.Transport
}

const maxRouteTransports = 64

func boundedTransport() *http.Transport {
	return &http.Transport{Proxy: nil, DialContext: (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2: true, MaxIdleConns: 16, MaxIdleConnsPerHost: 4, MaxConnsPerHost: 8,
		IdleConnTimeout: 60 * time.Second, TLSHandshakeTimeout: 5 * time.Second, ExpectContinueTimeout: time.Second,
		TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12}, MaxResponseHeaderBytes: 64 << 10}
}
func constrainTransport(t *http.Transport) *http.Transport {
	out := t.Clone()
	out.Proxy = nil
	out.MaxIdleConns = 16
	out.MaxIdleConnsPerHost = 4
	out.MaxConnsPerHost = 8
	out.IdleConnTimeout = 60 * time.Second
	out.TLSHandshakeTimeout = 5 * time.Second
	out.ExpectContinueTimeout = time.Second
	out.MaxResponseHeaderBytes = 64 << 10
	if out.DialContext == nil {
		out.DialContext = (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext
	}
	// The caller may inject trusted test roots, but never turn TLS verification off.
	if out.TLSClientConfig == nil {
		out.TLSClientConfig = &tls.Config{}
	}
	out.TLSClientConfig = out.TLSClientConfig.Clone()
	out.TLSClientConfig.InsecureSkipVerify = false
	if out.TLSClientConfig.MinVersion < tls.VersionTLS12 {
		out.TLSClientConfig.MinVersion = tls.VersionTLS12
	}
	return out
}
func routeOwner(c Channel) string { return strconv.FormatInt(c.ID, 10) + ":" + c.Type }
func digest(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(sum[:])
}
func relaySetting(c Channel) string {
	switch c.Type {
	case "telegram":
		return strings.TrimSpace(Str(c.Config["telegram_api_proxy"]))
	case "serverchan3":
		return strings.TrimSpace(Str(c.Config["sc3_api_proxy"]))
	case "wechat":
		return strings.TrimSpace(Str(c.Config["wecom_proxy"]))
	}
	return ""
}

// relayURL intentionally narrows upstream's HTTP/LAN relay support: relays see
// provider credentials and message bytes, so only a public verified HTTPS origin
// is accepted. DNS is revalidated and pinned whenever a new connection is made.
func relayURL(raw string) (*url.URL, error) {
	if len(raw) > 4096 || strings.ContainsAny(raw, "\x00\r\n\\") {
		return nil, errors.New("invalid notification relay URL")
	}
	u, err := url.Parse(strings.TrimRight(strings.TrimSpace(raw), "/"))
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || (u.Port() != "" && u.Port() != "443") || u.RawPath != "" {
		return nil, errors.New("notification relay requires public HTTPS on port 443 without credentials, query or fragment")
	}
	if _, err = PublicDomain("https://" + u.Host); err != nil {
		return nil, errors.New("notification relay must use a public host")
	}
	for _, segment := range strings.Split(u.Path, "/") {
		if segment == "." || segment == ".." {
			return nil, errors.New("invalid notification relay path")
		}
	}
	return u, nil
}
func forwardURL(raw string) (*url.URL, error) {
	if len(raw) > 4096 || strings.ContainsAny(raw, "\x00\r\n\\") {
		return nil, errors.New("invalid notification forward proxy URL")
	}
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Hostname() == "" || u.Opaque != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return nil, errors.New("invalid notification forward proxy URL")
	}
	switch u.Scheme {
	case "http", "https", "socks5", "socks5h":
	default:
		return nil, errors.New("notification forward proxy must use HTTP, HTTPS or SOCKS5")
	}
	if p := u.Port(); p != "" {
		n, err := strconv.Atoi(p)
		if err != nil || n < 1 || n > 65535 {
			return nil, errors.New("invalid notification forward proxy port")
		}
	}
	if u.User != nil {
		password, _ := u.User.Password()
		if len(u.User.Username()) > 255 || len(password) > 255 || strings.ContainsAny(u.User.Username()+password, "\x00\r\n") {
			return nil, errors.New("invalid notification forward proxy credentials")
		}
	}
	return u, nil
}
func validateRouting(c Channel) error {
	if c.UseProxy && c.Type != "telegram" {
		return fmt.Errorf("%w: global forward proxy is supported only for Telegram; configure the channel HTTPS relay instead", ErrUnsupported)
	}
	for _, key := range []string{"telegram_api_proxy", "sc3_api_proxy", "wecom_proxy"} {
		value := c.Config[key]
		if value == nil {
			continue
		}
		// A malformed stored value is not an absent route. Reject it before
		// relaySetting/Str could interpret an object or array as direct mode.
		raw, ok := value.(string)
		if !ok {
			return errors.New("notification relay URL must be a string")
		}
		if raw = strings.TrimSpace(raw); raw != "" {
			if _, err := relayURL(raw); err != nil {
				return err
			}
		}
	}
	return nil
}
func (s *Service) withRoute(ctx context.Context, c Channel) (context.Context, error) {
	if err := ctx.Err(); err != nil {
		return ctx, err
	}
	s.mu.Lock()
	closed := s.closed
	s.mu.Unlock()
	if closed {
		return ctx, errors.New("notification service is closed")
	}
	if route, ok := ctx.Value(routeContextKey{}).(*outboundRoute); ok && route.owner == routeOwner(c) {
		return ctx, nil
	}
	cfg := RoutingConfig{}
	if s.Routing != nil {
		var err error
		cfg, err = s.Routing(ctx, c)
		if err != nil {
			return ctx, errors.New("notification routing settings unavailable")
		}
	}
	route := &outboundRoute{owner: routeOwner(c)}
	if err := validateRouting(c); err != nil {
		return ctx, err
	}
	if raw := relaySetting(c); raw != "" {
		u, err := relayURL(raw)
		if err != nil {
			return ctx, err
		}
		route.relay = u
		if c.Type == "serverchan3" || (c.Type == "wechat" && strings.EqualFold(Str(c.Config["wecom_proxy_relay_auth"]), "true")) {
			route.relayKey = cfg.RelayKey
		}
		if len(route.relayKey) > 16384 || strings.ContainsAny(route.relayKey, "\x00\r\n") {
			return ctx, errors.New("invalid notification relay authentication configuration")
		}
	} else if c.UseProxy {
		mode := cfg.ProxyMode
		if mode == "" {
			mode = "none"
		}
		if mode == "none" && cfg.ProxyEnabled {
			mode = "http_socks"
		}
		if mode != "http_socks" {
			return ctx, fmt.Errorf("%w: selected notification forward proxy is not active in HTTP/SOCKS mode; request was not sent directly", ErrUnsupported)
		}
		if strings.TrimSpace(cfg.ProxyURL) == "" {
			return ctx, errors.New("selected notification forward proxy has no URL; request was not sent directly")
		}
		u, err := forwardURL(cfg.ProxyURL)
		if err != nil {
			return ctx, err
		}
		route.forward = u
	}
	forward, relay := "", ""
	if route.forward != nil {
		forward = route.forward.String()
	}
	if route.relay != nil {
		relay = route.relay.String()
	}
	route.fingerprint = digest(route.owner, forward, relay, route.relayKey, Str(c.Config["bot_token"]), Str(c.Config["corp_id"]), Str(c.Config["corp_secret"]))
	return context.WithValue(ctx, routeContextKey{}, route), nil
}
func (s *Service) publicRelayDial(host string) func(context.Context, string, string) (net.Conn, error) {
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		ctx, cancel := proxyDialContext(ctx)
		defer cancel()
		requested, port, err := net.SplitHostPort(address)
		if err != nil || !strings.EqualFold(requested, host) || port != "443" {
			return nil, ErrForbidden
		}
		lookup := s.lookupIP
		if lookup == nil {
			lookup = net.DefaultResolver.LookupNetIP
		}
		ips, err := lookup(ctx, "ip", host)
		if err != nil || len(ips) == 0 {
			return nil, transportFailure(ctx, err, "notification relay DNS lookup failed")
		}
		for _, ip := range ips {
			if !PublicIP(ip) {
				return nil, ErrForbidden
			}
		}
		dial := s.dialContext
		if dial == nil {
			dial = (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext
		}
		for _, ip := range ips {
			conn, err := dial(ctx, network, net.JoinHostPort(ip.String(), "443"))
			if err == nil {
				return conn, nil
			}
			if failure := cancellationError(ctx, err); failure != nil {
				return nil, failure
			}
		}
		return nil, transportFailure(ctx, nil, "notification relay connection failed")
	}
}
func (s *Service) routeClient(route *outboundRoute, base *http.Client) (*http.Client, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, errors.New("notification service is closed")
	}
	entry, ok := s.routes[route.owner]
	if ok && entry.fingerprint != route.fingerprint {
		entry.transport.CloseIdleConnections()
		delete(s.routes, route.owner)
		ok = false
	}
	if route.forward == nil && route.relay == nil {
		return base, nil
	}
	if !ok {
		tr := boundedTransport()
		if configured, ok := s.Client.Transport.(*http.Transport); ok {
			tr = constrainTransport(configured)
		}
		// Routing is always handled here, never by caller/ambient proxy callbacks.
		tr.DialTLSContext = nil
		tr.DialTLS = nil
		if route.forward != nil {
			tr.Proxy = nil
			tr.TLSClientConfig.ServerName = ""
			tr.DialContext = forwardProxyDial(route.forward, tr.TLSClientConfig)
		} else {
			tr.Proxy = nil
			tr.DialContext = s.publicRelayDial(route.relay.Hostname())
			tr.TLSClientConfig.ServerName = ""
		}
		if len(s.routes) >= maxRouteTransports {
			for key, old := range s.routes {
				old.transport.CloseIdleConnections()
				delete(s.routes, key)
				break
			}
		}
		entry = routeTransport{route.fingerprint, tr}
		s.routes[route.owner] = entry
	}
	cp := *base
	cp.Transport = entry.transport
	return &cp, nil
}
func routedEndpoint(route *outboundRoute, u *url.URL, c Channel) (*url.URL, http.Header) {
	out := *u
	headers := make(http.Header)
	if route.relay == nil {
		return &out, headers
	}
	out.Scheme = route.relay.Scheme
	out.Host = route.relay.Host
	base := strings.TrimRight(route.relay.Path, "/")
	if c.Type == "wechat" {
		if i := strings.Index(base, "/cgi-bin"); i >= 0 {
			base = base[:i] + "/cgi-bin"
		} else if strings.HasSuffix(base, "/out") {
			base += "/qyapi.weixin.qq.com/cgi-bin"
		} else {
			base += "/cgi-bin"
		}
		out.Path = base + strings.TrimPrefix(u.Path, "/cgi-bin")
	} else {
		out.Path = base + "/out/" + u.Host + u.Path
	}
	out.RawPath = ""
	if route.relayKey != "" {
		headers.Set("X-Relay-Key", route.relayKey)
	}
	return &out, headers
}

// Close releases idle pooled connections. It does not interrupt in-flight calls;
// callers cancel their request/worker contexts before shutdown.
func (s *Service) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	for key, entry := range s.routes {
		entry.transport.CloseIdleConnections()
		delete(s.routes, key)
	}
	s.tokens = map[string]tokenEntry{}
	if s.Client != nil {
		s.Client.CloseIdleConnections()
	}
}

// Private fixture hooks exercise DNS rebinding controls without weakening the
// runtime configuration surface. They must be assigned before concurrent use.
type relayLookup func(context.Context, string, string) ([]netip.Addr, error)
