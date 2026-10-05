// SPDX-License-Identifier: AGPL-3.0-only
package notify

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"
)

var blockedPrefixes = []netip.Prefix{netip.MustParsePrefix("0.0.0.0/8"), netip.MustParsePrefix("100.64.0.0/10"), netip.MustParsePrefix("192.0.0.0/24"), netip.MustParsePrefix("192.0.2.0/24"), netip.MustParsePrefix("198.18.0.0/15"), netip.MustParsePrefix("198.51.100.0/24"), netip.MustParsePrefix("203.0.113.0/24"), netip.MustParsePrefix("240.0.0.0/4"), netip.MustParsePrefix("2001:db8::/32"), netip.MustParsePrefix("2001::/32"), netip.MustParsePrefix("64:ff9b::/96"), netip.MustParsePrefix("2002::/16")}

func PublicIP(ip netip.Addr) bool {
	ip = ip.Unmap()
	if !ip.IsValid() || !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
		return false
	}
	for _, p := range blockedPrefixes {
		if p.Contains(ip) {
			return false
		}
	}
	return true
}
func PublicDomain(raw string) (string, error) {
	u, e := url.Parse(strings.TrimSpace(raw))
	if e != nil || u.User != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || (u.Path != "" && u.Path != "/") || (u.Port() != "" && u.Port() != "80" && u.Port() != "443") {
		return "", errors.New("public domain must be an HTTP(S) origin without credentials, path, query or custom port")
	}
	host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	if host == "localhost" || strings.HasSuffix(host, ".localhost") || strings.HasSuffix(host, ".local") || !strings.Contains(host, ".") {
		return "", ErrForbidden
	}
	if ip, e := netip.ParseAddr(host); e == nil && !PublicIP(ip) {
		return "", ErrForbidden
	}
	return strings.TrimRight(u.String(), "/"), nil
}

// ProbePublicDomain resolves once, rejects all nonpublic answers, and dials a
// pinned validated address. It uses no ambient proxy and never follows redirects.
func ProbePublicDomain(ctx context.Context, raw, path string, expected []byte) (map[string]any, error) {
	origin, e := PublicDomain(raw)
	if e != nil {
		return nil, e
	}
	u, _ := url.Parse(origin)
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	ips, e := net.DefaultResolver.LookupNetIP(ctx, "ip", u.Hostname())
	if e != nil || len(ips) == 0 {
		return nil, errors.New("public domain DNS lookup failed")
	}
	for _, ip := range ips {
		if !PublicIP(ip) {
			return nil, ErrForbidden
		}
	}
	port := u.Port()
	if port == "" {
		port = "443"
		if u.Scheme == "http" {
			port = "80"
		}
	}
	tr := &http.Transport{Proxy: nil, DisableKeepAlives: true, TLSHandshakeTimeout: 5 * time.Second, DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
		host, _, err := net.SplitHostPort(address)
		if err != nil || !strings.EqualFold(host, u.Hostname()) {
			return nil, ErrForbidden
		}
		var last error
		for _, ip := range ips {
			conn, err := (&net.Dialer{Timeout: 3 * time.Second}).DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
			if err == nil {
				return conn, nil
			}
			last = err
		}
		return nil, last
	}}
	defer tr.CloseIdleConnections()
	client := &http.Client{Transport: tr, Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return ErrForbidden }}
	if !strings.HasPrefix(path, "/data/images/") || strings.Contains(path, "..") {
		return nil, ErrForbidden
	}
	r, e := http.NewRequestWithContext(ctx, "GET", origin+path, nil)
	if e != nil {
		return nil, e
	}
	resp, e := client.Do(r)
	if e != nil {
		return nil, errors.New("public domain probe failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("public domain probe HTTP %d", resp.StatusCode)
	}
	b, e := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if e != nil || !bytes.Equal(b, expected) {
		return nil, errors.New("public domain returned unexpected probe content")
	}
	return map[string]any{"ok": true, "detail": "Public domain serves this instance's probe image", "domain": origin, "url": origin + path}, nil
}
