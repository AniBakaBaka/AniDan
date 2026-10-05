// SPDX-License-Identifier: AGPL-3.0-only
package server

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/AniBakaBaka/AniDan/internal/proxyroute"
)

const compatAccelerateApprovalError = "Save and explicitly trust the selected accelerate gateway before testing"

// Diagnostics may use only the currently saved local approval. A request cannot
// authorize an unsaved gateway, even if it supplies a trust-looking boolean.
func (s *Server) compatAccelerateChoice(ctx context.Context, input compatProxyInput) (proxyroute.Config, error) {
	denied := errors.New(compatAccelerateApprovalError)
	if input.Mode != "accelerate" || input.URL != "" {
		return proxyroute.Config{}, denied
	}
	gateway, err := proxyroute.CanonicalGateway(input.AccelerateURL)
	if err != nil {
		return proxyroute.Config{}, denied
	}
	state := s.proxyRouting.Load()
	if state == nil || state.mode != "accelerate" || state.blocked != "" || state.approvalID == "" || state.gateway != gateway {
		return proxyroute.Config{}, denied
	}
	route := s.selectedProxyRouting(true)
	if route.AccelerateURL != gateway || route.AuthorizationIdentity != state.approvalID || compatAccelerateAuthorized(ctx, route) != nil {
		return proxyroute.Config{}, denied
	}
	return route, nil
}

func compatAccelerateAuthorized(ctx context.Context, route proxyroute.Config) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if route.Blocked || route.ProxyURL != "" || route.AccelerateURL == "" || route.AuthorizationIdentity == "" || route.Authorize == nil {
		return proxyroute.ErrUntrustedGateway
	}
	if err := proxyroute.Validate(route); err != nil {
		return proxyroute.ErrUntrustedGateway
	}
	if err := route.Authorize(ctx); err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return err
		}
		return proxyroute.ErrUntrustedGateway
	}
	return nil
}

func compatAccelerateProbe(ctx context.Context, target *url.URL, route proxyroute.Config, checkDNS, checkHTTP, expect204 bool, deps compatProbeDependencies) compatProbeResult {
	out := compatProbeResult{Status: "skipped"}
	if compatAccelerateAuthorized(ctx, route) != nil {
		out.Status, out.Error = "failure", textPointer(compatAccelerateApprovalError)
		return out
	}
	if !checkDNS && !checkHTTP {
		return out
	}
	if target == nil {
		out.Status, out.Error = "failure", textPointer("Invalid probe request")
		return out
	}
	validated, err := compatProbeURL(target.String())
	if err != nil {
		out.Status, out.Error = "failure", textPointer("Invalid probe request")
		return out
	}
	target = validated
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
	if compatAccelerateAuthorized(ctx, route) != nil {
		out.Status, out.Error = "failure", textPointer(compatAccelerateApprovalError)
		return out
	}
	gateway, _ := url.Parse(route.AccelerateURL) // Structural validation preceded DNS.
	gatewayCtx, gatewayCancel := context.WithTimeout(ctx, 3*time.Second)
	gatewayIPs, err := compatAddresses(gatewayCtx, gateway.Hostname(), true, deps)
	gatewayCancel()
	if err != nil {
		out.Status, out.Error = "failure", textPointer("Approved gateway DNS failed or returned an invalid address")
		return out
	}
	port := gateway.Port()
	if port == "" {
		port = "443"
	}
	transport := &http.Transport{Proxy: nil, DisableKeepAlives: true, TLSHandshakeTimeout: 4 * time.Second, ResponseHeaderTimeout: 5 * time.Second, MaxResponseHeaderBytes: 32 << 10, TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: deps.roots}}
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		host, requestedPort, err := net.SplitHostPort(address)
		if err != nil || !strings.EqualFold(host, gateway.Hostname()) || requestedPort != port {
			return nil, errors.New("unexpected probe destination")
		}
		for _, ip := range gatewayIPs {
			if err := compatAccelerateAuthorized(ctx, route); err != nil {
				return nil, err
			}
			conn, err := deps.dial(ctx, network, net.JoinHostPort(ip.String(), port))
			if err == nil {
				return conn, nil
			}
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
		}
		return nil, errors.New("gateway probe connection failed")
	}
	// The local target lookup is a public-address safety check, not an origin-IP
	// pin: this protocol sends the logical hostname to the trusted gateway. Its
	// remote DNS and onward connection cannot be pinned or verified locally.
	// Only the gateway's own TLS connection is resolved and pinned by this probe.
	base := &http.Client{Transport: transport, Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	client, err := proxyroute.Client(base, route)
	if err != nil {
		out.Status, out.Error = "failure", textPointer(compatAccelerateApprovalError)
		return out
	}
	defer client.CloseIdleConnections()
	method := "HEAD"
	if expect204 {
		method = "GET"
	}
	req, err := http.NewRequestWithContext(ctx, method, target.String(), nil)
	if err != nil {
		out.Status, out.Error = "failure", textPointer("Invalid probe request")
		return out
	}
	req.Header.Set("User-Agent", "AniDan-connectivity-probe")
	start = time.Now()
	response, err := client.Do(req)
	out.Latency = millis(start)
	if err != nil {
		out.Status = "failure"
		if errors.Is(err, proxyroute.ErrUntrustedGateway) {
			out.Error = textPointer(compatAccelerateApprovalError)
		} else {
			out.Error = textPointer("HTTP probe failed; TLS verification was not bypassed")
		}
		return out
	}
	defer response.Body.Close()
	out.HTTPStatus = response.StatusCode
	if expect204 && response.StatusCode != http.StatusNoContent {
		out.Status, out.Error = "failure", textPointer("Proxy connectivity endpoint did not return HTTP 204")
		return out
	}
	// A received status proves transport reachability only, not application health.
	// Do not read response bodies; diagnostic traffic and memory remain bounded.
	out.Status = "success"
	return out
}

func (s *Server) compatAccelerateFull(w http.ResponseWriter, r *http.Request, input compatProxyInput) {
	route, err := s.compatAccelerateChoice(r.Context(), input)
	if err != nil {
		httpError(w, 422, err.Error())
		return
	}
	domains, err := s.compatProxyTargets(r.Context(), true)
	if err != nil {
		httpError(w, 500, "Proxy target configuration could not be read")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	deps := compatProbeDefaults()
	target, _ := compatProbeURL("https://www.gstatic.com/generate_204")
	connectivity := compatAccelerateProbe(ctx, target, route, true, true, true, deps)
	results := compatAccelerateProbeMany(ctx, domains, route, deps)
	writeJSON(w, 200, map[string]any{"proxy_connectivity": connectivity, "target_sites": results, "domain_map": domains})
}

func compatAccelerateProbeMany(ctx context.Context, domains map[string]map[string]string, route proxyroute.Config, deps compatProbeDependencies) map[string]compatProbeResult {
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
				result = compatAccelerateProbe(ctx, target, route, true, true, false, deps)
			}
			mu.Lock()
			results[raw] = result
			mu.Unlock()
		})
	}
	wg.Wait()
	return results
}
