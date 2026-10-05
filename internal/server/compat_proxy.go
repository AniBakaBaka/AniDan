// SPDX-License-Identifier: AGPL-3.0-only
package server

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/AniBakaBaka/AniDan/internal/integration"
	"github.com/AniBakaBaka/AniDan/internal/notify"
	"github.com/AniBakaBaka/AniDan/internal/store"
	"golang.org/x/net/proxy"
)

type compatProxyInput struct {
	Mode          string `json:"proxy_mode"`
	URL           string `json:"proxy_url"`
	AccelerateURL string `json:"accelerate_proxy_url"`
}
type compatSingleInput struct {
	compatProxyInput
	Target    string `json:"url"`
	CheckDNS  bool   `json:"check_dns"`
	CheckHTTP bool   `json:"check_http"`
}
type compatProbeResult struct {
	Status     string   `json:"status"`
	Latency    *float64 `json:"latency"`
	Error      *string  `json:"error"`
	DNSStatus  *string  `json:"dns_status"`
	DNSLatency *float64 `json:"dns_latency"`
	ResolvedIP *string  `json:"resolved_ip"`
	DNSError   *string  `json:"dns_error"`
	HTTPStatus int      `json:"http_status,omitempty"`
}
type compatProbeDependencies struct {
	roots *x509.CertPool // fixture-only trust roots; production uses system verification

	lookup func(context.Context, string) ([]netip.Addr, error)
	dial   func(context.Context, string, string) (net.Conn, error)
}

func compatProbeDefaults() compatProbeDependencies {
	return compatProbeDependencies{
		lookup: func(ctx context.Context, host string) ([]netip.Addr, error) {
			return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
		},
		dial: (&net.Dialer{Timeout: 3 * time.Second}).DialContext,
	}
}
func textPointer(s string) *string { return &s }
func millis(start time.Time) *float64 {
	value := float64(time.Since(start).Microseconds()) / 1000
	return &value
}
func (s *Server) registerCompatProxy(m *http.ServeMux) {
	m.HandleFunc("POST /api/ui/proxy/test", s.operator(s.compatProxyFull))
	m.HandleFunc("POST /api/ui/proxy/test-single", s.operator(s.compatProxySingle))
}

var compatSpecialNetworks = []netip.Prefix{
	netip.MustParsePrefix("192.88.99.0/24"), netip.MustParsePrefix("::/96"),
	netip.MustParsePrefix("100::/64"), netip.MustParsePrefix("fec0::/10"),
	netip.MustParsePrefix("2001:10::/28"), netip.MustParsePrefix("2001:20::/28"),
	netip.MustParsePrefix("64:ff9b:1::/48"),
}

func compatPublicIP(ip netip.Addr) bool {
	ip = ip.Unmap()
	if !notify.PublicIP(ip) {
		return false
	}
	for _, prefix := range compatSpecialNetworks {
		if prefix.Contains(ip) {
			return false
		}
	}
	return true
}
func compatProbeURL(raw string) (*url.URL, error) {
	raw = strings.TrimSpace(raw)
	if len(raw) == 0 || len(raw) > 2048 || strings.ContainsAny(raw, "\x00\r\n\\") {
		return nil, errors.New("invalid target URL")
	}
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil || u.Opaque != "" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, errors.New("target must be an HTTP(S) URL without credentials, query or fragment")
	}
	host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	if len(host) > 253 || strings.ContainsAny(host, " \t/%") {
		return nil, errors.New("invalid target host")
	}
	if ip, err := netip.ParseAddr(host); err == nil {
		if !compatPublicIP(ip) {
			return nil, errors.New("target must be publicly routable")
		}
	} else if !strings.Contains(host, ".") || strings.HasSuffix(host, ".localhost") || strings.HasSuffix(host, ".local") || strings.HasSuffix(host, ".internal") {
		return nil, errors.New("target must be a public hostname")
	}
	port := u.Port()
	if port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return nil, errors.New("invalid target port")
		}
		u.Host = net.JoinHostPort(host, port)
	} else {
		u.Host = host
		if strings.Contains(host, ":") {
			u.Host = "[" + host + "]"
		}
	}
	return u, nil
}
func compatProxyURL(raw string) (*url.URL, error) {
	if len(raw) > 4096 || strings.ContainsAny(raw, "\x00\r\n") {
		return nil, errors.New("invalid proxy URL")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" || u.Opaque != "" || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return nil, errors.New("proxy URL must specify a proxy origin")
	}
	switch u.Scheme {
	case "http", "https", "socks5", "socks5h":
	default:
		return nil, errors.New("proxy scheme must be HTTP, HTTPS, SOCKS5 or SOCKS5H")
	}
	if port := u.Port(); port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return nil, errors.New("invalid proxy port")
		}
	}
	if u.User != nil {
		password, _ := u.User.Password()
		if len(u.User.Username()) > 255 || len(password) > 255 || strings.ContainsAny(u.User.Username()+password, "\r\n\x00") {
			return nil, errors.New("invalid proxy credentials")
		}
	}
	return u, nil
}
func (s *Server) compatProxyChoice(ctx context.Context, input compatProxyInput) (*url.URL, bool, error) {
	if input.Mode == "" {
		input.Mode = "none"
	}
	if input.AccelerateURL != "" || input.Mode == "accelerate" {
		return nil, false, errors.New("accelerate_proxy_url requires accelerate mode and saved gateway approval")
	}
	if input.Mode == "none" {
		if input.URL != "" {
			return nil, false, errors.New("proxy_url requires http_socks mode")
		}
		return nil, false, nil
	}
	if input.Mode != "http_socks" {
		return nil, false, errors.New("proxy_mode must be none or http_socks")
	}
	stored, err := s.proxyURL(ctx)
	if err != nil {
		stored = ""
	}
	raw := input.URL
	if raw == "" {
		raw = stored
	}
	if raw == "" {
		return nil, false, errors.New("HTTP/SOCKS mode requires an explicit proxy URL")
	}
	p, err := compatProxyURL(raw)
	if err != nil {
		return nil, false, err
	}
	// A mask may preserve credentials only at the exact previously saved endpoint.
	if p.User != nil {
		password, _ := p.User.Password()
		if password == compatSecretMask {
			saved, e := compatProxyURL(stored)
			if e != nil || saved.User == nil || p.Scheme != saved.Scheme || p.Host != saved.Host || p.User.Username() != saved.User.Username() {
				return nil, false, errors.New("masked proxy password does not match the saved proxy")
			}
			raw = stored
			p = saved
		}
	}
	return p, raw == stored && stored != "", nil
}
func (s *Server) compatProxySingle(w http.ResponseWriter, r *http.Request) {
	input := compatSingleInput{compatProxyInput: compatProxyInput{Mode: "none"}, CheckDNS: true, CheckHTTP: true}
	if compatDecode(r, &input) != nil {
		httpError(w, 422, "Invalid single-target test request")
		return
	}
	target, err := compatProbeURL(input.Target)
	if err != nil {
		httpError(w, 400, err.Error())
		return
	}
	if input.Mode == "accelerate" {
		route, err := s.compatAccelerateChoice(r.Context(), input.compatProxyInput)
		if err != nil {
			httpError(w, 422, err.Error())
			return
		}
		result := compatAccelerateProbe(r.Context(), target, route, input.CheckDNS, input.CheckHTTP, false, compatProbeDefaults())
		writeJSON(w, 200, map[string]any{"url": target.String(), "host": target.Hostname(), "result": result})
		return
	}
	proxyURL, privateProxy, err := s.compatProxyChoice(r.Context(), input.compatProxyInput)
	if err != nil {
		httpError(w, 422, err.Error())
		return
	}
	result := compatProbe(r.Context(), target, proxyURL, privateProxy, input.CheckDNS, input.CheckHTTP, false, compatProbeDefaults())
	writeJSON(w, 200, map[string]any{"url": target.String(), "host": target.Hostname(), "result": result})
}
func compatAddresses(ctx context.Context, host string, allowPrivate bool, deps compatProbeDependencies) ([]netip.Addr, error) {
	ips, err := deps.lookup(ctx, host)
	if err != nil || len(ips) == 0 || len(ips) > 64 {
		return nil, errors.New("DNS resolution failed")
	}
	for _, ip := range ips {
		ip = ip.Unmap()
		if !compatPublicIP(ip) && (!allowPrivate || !ip.IsValid() || (!ip.IsGlobalUnicast() && !ip.IsLoopback())) {
			return nil, errors.New("address is not publicly routable")
		}
	}
	return ips, nil
}
func compatProbe(ctx context.Context, target, p *url.URL, privateProxy, checkDNS, checkHTTP, expect204 bool, deps compatProbeDependencies) compatProbeResult {
	out := compatProbeResult{Status: "skipped"}
	if !checkDNS && !checkHTTP {
		return out
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	start := time.Now()
	dnsCtx, dnsCancel := context.WithTimeout(ctx, 3*time.Second)
	ips, err := compatAddresses(dnsCtx, target.Hostname(), false, deps)
	dnsCancel()
	if checkDNS {
		out.DNSLatency = millis(start)
		out.DNSStatus = textPointer("success")
		if err != nil {
			out.DNSStatus = textPointer("failure")
			out.DNSError = textPointer("Target DNS failed or returned a nonpublic address")
		} else {
			out.ResolvedIP = textPointer(ips[0].String())
		}
	}
	if err != nil {
		out.Status = "failure"
		if checkHTTP {
			out.Error = textPointer("Target DNS failed or returned a nonpublic address")
		}
		return out
	}
	if !checkHTTP {
		out.Status = "success"
		return out
	}
	var proxyIPs []netip.Addr
	if p != nil {
		proxyCtx, proxyCancel := context.WithTimeout(ctx, 3*time.Second)
		proxyIPs, err = compatAddresses(proxyCtx, p.Hostname(), privateProxy, deps)
		proxyCancel()
		if err != nil {
			out.Status = "failure"
			out.Error = textPointer("Proxy DNS failed or its address is not permitted; LAN proxies must exactly match saved configuration")
			return out
		}
	}
	port := target.Port()
	if port == "" {
		port = "443"
		if target.Scheme == "http" {
			port = "80"
		}
	}
	transport := &http.Transport{Proxy: nil, DisableKeepAlives: true, TLSHandshakeTimeout: 4 * time.Second, ResponseHeaderTimeout: 5 * time.Second, MaxResponseHeaderBytes: 32 << 10, TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: deps.roots}}
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		host, requestedPort, err := net.SplitHostPort(address)
		if err != nil || !strings.EqualFold(host, target.Hostname()) || requestedPort != port {
			return nil, errors.New("unexpected probe destination")
		}
		for _, ip := range ips {
			destination := net.JoinHostPort(ip.String(), port)
			var conn net.Conn
			if p == nil {
				conn, err = deps.dial(ctx, network, destination)
			} else {
				conn, err = compatProxyDial(ctx, p, proxyIPs, destination, deps)
			}
			if err == nil {
				return conn, nil
			}
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
		}
		return nil, errors.New("probe connection failed")
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	method := "HEAD"
	if expect204 {
		method = "GET"
	}
	req, err := http.NewRequestWithContext(ctx, method, target.String(), nil)
	if err != nil {
		out.Status = "failure"
		out.Error = textPointer("Invalid probe request")
		return out
	}
	req.Header.Set("User-Agent", "AniDan-connectivity-probe")
	start = time.Now()
	response, err := client.Do(req)
	out.Latency = millis(start)
	if err != nil {
		out.Status = "failure"
		out.Error = textPointer("HTTP probe failed; TLS verification was not bypassed")
		return out
	}
	defer response.Body.Close()
	out.HTTPStatus = response.StatusCode
	if expect204 && response.StatusCode != 204 {
		out.Status = "failure"
		out.Error = textPointer("Proxy connectivity endpoint did not return HTTP 204")
		return out
	}
	// Any received HTTP response proves transport reachability, not application
	// health or authenticated API availability. The actual status is also returned.
	out.Status = "success"
	return out
}

type compatDialer struct {
	dial func(context.Context, string, string) (net.Conn, error)
}

func (d compatDialer) Dial(network, address string) (net.Conn, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return d.dial(ctx, network, address)
}
func (d compatDialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	return d.dial(ctx, network, address)
}

type compatBufferedConn struct {
	net.Conn
	reader io.Reader
}

func (c *compatBufferedConn) Read(p []byte) (int, error) { return c.reader.Read(p) }
func compatProxyDial(ctx context.Context, p *url.URL, ips []netip.Addr, destination string, deps compatProbeDependencies) (net.Conn, error) {
	port := p.Port()
	if port == "" {
		port = "1080"
		if p.Scheme == "http" {
			port = "80"
		} else if p.Scheme == "https" {
			port = "443"
		}
	}
	dialProxy := func(ctx context.Context, network, _ string) (net.Conn, error) {
		for _, ip := range ips {
			conn, err := deps.dial(ctx, network, net.JoinHostPort(ip.String(), port))
			if err == nil {
				return conn, nil
			}
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
		}
		return nil, errors.New("proxy connection failed")
	}
	if p.Scheme == "socks5" || p.Scheme == "socks5h" {
		var auth *proxy.Auth
		if p.User != nil {
			password, _ := p.User.Password()
			auth = &proxy.Auth{User: p.User.Username(), Password: password}
		}
		dialer, err := proxy.SOCKS5("tcp", net.JoinHostPort(ips[0].String(), port), auth, compatDialer{dialProxy})
		if err != nil {
			return nil, errors.New("invalid SOCKS proxy")
		}
		contextual, ok := dialer.(proxy.ContextDialer)
		if !ok {
			return nil, errors.New("SOCKS proxy lacks bounded dialing")
		}
		return contextual.DialContext(ctx, "tcp", destination)
	}
	conn, err := dialProxy(ctx, "tcp", "")
	if err != nil {
		return nil, err
	}
	failed := true
	defer func() {
		if failed {
			conn.Close()
		}
	}()
	rawConn := conn
	stop := context.AfterFunc(ctx, func() { rawConn.Close() })
	defer stop()
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	} else {
		_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	}
	if p.Scheme == "https" {
		tlsConn := tls.Client(conn, &tls.Config{ServerName: p.Hostname(), MinVersion: tls.VersionTLS12, RootCAs: deps.roots})
		if err = tlsConn.HandshakeContext(ctx); err != nil {
			return nil, errors.New("HTTPS proxy TLS verification failed")
		}
		conn = tlsConn
	}
	connect := &http.Request{Method: "CONNECT", URL: &url.URL{Opaque: destination}, Host: destination, Header: make(http.Header)}
	if p.User != nil {
		password, _ := p.User.Password()
		connect.Header.Set("Proxy-Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(p.User.Username()+":"+password)))
	}
	if err = connect.Write(conn); err != nil {
		return nil, errors.New("proxy CONNECT failed")
	}
	bounded := &io.LimitedReader{R: conn, N: 32 << 10}
	buffer := bufio.NewReader(bounded)
	response, err := http.ReadResponse(buffer, connect)
	if err != nil || response.StatusCode != 200 || bounded.N <= 0 {
		return nil, errors.New("proxy CONNECT was not accepted")
	}
	var prefix []byte
	if n := buffer.Buffered(); n > 0 {
		v, _ := buffer.Peek(n)
		prefix = append(prefix, v...)
	}
	failed = false
	_ = conn.SetDeadline(time.Time{})
	return &compatBufferedConn{Conn: conn, reader: io.MultiReader(bytes.NewReader(prefix), conn)}, nil
}
func (s *Server) compatProxyTargets(ctx context.Context, useProxy bool) (map[string]map[string]string, error) {
	targets := map[string]map[string]string{}
	add := func(raw, group, source string) {
		u, err := url.Parse(raw)
		if err != nil || u.User != nil || u.Host == "" {
			return
		}
		u.Path = ""
		u.RawPath = ""
		u.RawQuery = ""
		u.Fragment = ""
		if normalized, err := compatProbeURL(u.String()); err == nil {
			targets[normalized.String()] = map[string]string{"group": group, "source": source}
		}
	}
	native := map[string][]string{
		"bilibili":   {"https://www.bilibili.com", "https://api.bilibili.com"},
		"dandanplay": {"https://api.dandanplay.net"},
		"tencent":    {"https://v.qq.com", "https://dm.video.qq.com"},
		"iqiyi":      {"https://pcw-api.iqiyi.com", "https://cmts.iqiyi.com"},
		"youku":      {"https://v.youku.com", "https://openapi.youku.com"},
		"mgtv":       {"https://www.mgtv.com", "https://pcweb.api.mgtv.com"},
		"gamer":      {"https://ani.gamer.com.tw"},
		"hanjutv":    {"https://hanjutv.com"},
		"sohu":       {"https://api.danmu.tv.sohu.com"},
		"le":         {"https://www.le.com"},
		"renren":     {"https://static-dm.rrmj.plus"},
		"migu":       {"https://www.miguvideo.com", "https://webapi.miguvideo.com"},
		"xigua":      {"https://m.ixigua.com", "https://ib.snssdk.com"},
	}
	rows, err := s.Store.List(ctx, "scrapers", nil, 100, 0)
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		if !boolean(row["is_enabled"]) || useProxy && !boolean(row["use_proxy"]) {
			continue
		}
		name := str(row["provider_name"])
		for _, raw := range native[name] {
			add(raw, "弹幕源", name)
		}
	}
	rows, err = s.Store.List(ctx, "metadata_sources", nil, 100, 0)
	if err != nil {
		return nil, err
	}
	settings := map[string]store.Row{}
	for _, row := range rows {
		settings[str(row["provider_name"])] = row
	}
	metadata := map[string]string{"bangumi": s.setting(ctx, "bangumiApiBaseUrl", "https://api.bgm.tv"), "tmdb": s.setting(ctx, "tmdbApiBaseUrl", "https://api.themoviedb.org/3"), "anibt": s.setting(ctx, "anibtApiBaseUrl", "https://anibt.net"), "douban": "https://movie.douban.com", "imdb": "https://www.imdb.com", "trakt": "https://api.trakt.tv", "360": "https://so.360kan.com", "tvdb": "https://api4.thetvdb.com"}
	for _, name := range integration.Providers {
		row, exists := settings[name]
		if exists && !boolean(row["is_enabled"]) || useProxy && (!exists || !boolean(row["use_proxy"])) {
			continue
		}
		add(metadata[name], "元数据源", name)
	}
	for _, raw := range []string{"https://github.com", "https://api.github.com", "https://raw.githubusercontent.com"} {
		add(raw, "资源下载", "GitHub")
	}
	for _, v := range []struct{ url, group, source string }{{"https://api.telegram.org", "通知服务", "Telegram"}, {"https://qyapi.weixin.qq.com", "通知服务", "企业微信"}, {"https://api.openai.com", "AI 服务", "OpenAI"}, {"https://api.deepseek.com", "AI 服务", "DeepSeek"}, {"https://api.siliconflow.cn", "AI 服务", "SiliconFlow"}, {"https://generativelanguage.googleapis.com", "AI 服务", "Google Gemini"}, {"https://cdn.jsdelivr.net", "资源下载", "jsDelivr"}, {"https://webservice.fanart.tv", "图片服务", "FanArt"}} {
		add(v.url, v.group, v.source)
	}
	if len(targets) > 64 {
		return nil, errors.New("too many proxy test targets")
	}
	return targets, nil
}
func (s *Server) compatProxyFull(w http.ResponseWriter, r *http.Request) {
	input := compatProxyInput{Mode: "none"}
	if compatDecode(r, &input) != nil {
		httpError(w, 422, "Invalid proxy test request")
		return
	}
	if input.Mode == "accelerate" {
		s.compatAccelerateFull(w, r, input)
		return
	}
	proxyURL, privateProxy, err := s.compatProxyChoice(r.Context(), input)
	if err != nil {
		httpError(w, 422, err.Error())
		return
	}
	domains, err := s.compatProxyTargets(r.Context(), proxyURL != nil)
	if err != nil {
		httpError(w, 500, "Proxy target configuration could not be read")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	deps := compatProbeDefaults()
	connectivity := compatProbeResult{Status: "skipped", Error: textPointer("Direct mode does not use a proxy")}
	if proxyURL != nil {
		target, _ := compatProbeURL("https://www.gstatic.com/generate_204")
		connectivity = compatProbe(ctx, target, proxyURL, privateProxy, true, true, true, deps)
	}
	results := compatProbeMany(ctx, domains, proxyURL, privateProxy, deps)
	writeJSON(w, 200, map[string]any{"proxy_connectivity": connectivity, "target_sites": results, "domain_map": domains})
}
func compatProbeMany(ctx context.Context, domains map[string]map[string]string, p *url.URL, privateProxy bool, deps compatProbeDependencies) map[string]compatProbeResult {
	results := map[string]compatProbeResult{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	slots := make(chan struct{}, 4)
	names := make([]string, 0, len(domains))
	for raw := range domains {
		names = append(names, raw)
	}
	sort.Strings(names)
	for _, raw := range names {
		if ctx.Err() != nil {
			mu.Lock()
			results[raw] = compatProbeResult{Status: "failure", Error: textPointer("Probe deadline or cancellation reached")}
			mu.Unlock()
			continue
		}
		select {
		case slots <- struct{}{}:
		case <-ctx.Done():
			mu.Lock()
			results[raw] = compatProbeResult{Status: "failure", Error: textPointer("Probe deadline or cancellation reached")}
			mu.Unlock()
			continue
		}
		wg.Go(func() {
			defer func() { <-slots }()
			target, err := compatProbeURL(raw)
			result := compatProbeResult{Status: "failure", Error: textPointer("Invalid target configuration")}
			if err == nil {
				result = compatProbe(ctx, target, p, privateProxy, true, true, false, deps)
			}
			mu.Lock()
			results[raw] = result
			mu.Unlock()
		})
	}
	wg.Wait()
	return results
}
