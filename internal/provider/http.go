// SPDX-License-Identifier: AGPL-3.0-only
package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/AniBakaBaka/AniDan/internal/danmaku"
	"github.com/AniBakaBaka/AniDan/internal/proxyroute"
	"github.com/AniBakaBaka/AniDan/internal/safelog"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type HTTPProvider struct {
	Client           *http.Client      `json:"-"`
	BaseURLs         map[string]string `json:"-"`
	Headers          http.Header       `json:"-"`
	MaxBytes         int64
	MaxSegments      int
	MaxComments      int
	MaxDownloadBytes int64
	SegmentWorkers   int
	logResponses     bool
	proxyURL         string
	routingIdentity  string
	routingConfig    proxyroute.Config
	sourceName       string
	acquireRequest   func(context.Context, string) (func(), error)
	responseFeedback func(string, int, string)
	flights          *commentFlights
	gate             chan struct{}
}

func newHTTP(client *http.Client) HTTPProvider {
	if client == nil {
		client = &http.Client{Timeout: 20 * time.Second, Transport: sharedProviderTransport}
	}
	copyClient := *client
	if copyClient.Timeout == 0 {
		copyClient.Timeout = 20 * time.Second
	}
	oldRedirect := copyClient.CheckRedirect
	copyClient.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return errors.New("too many redirects")
		}
		if len(via) > 0 && req.URL.Scheme != "https" && via[0].URL.Scheme == "https" {
			return errors.New("provider redirect would downgrade HTTPS")
		}
		if len(via) > 0 && req.URL.Host != via[0].URL.Host {
			if !trustedRedirect(req.URL.Hostname()) || req.URL.Port() != "" {
				return errors.New("provider redirect target is not allowlisted")
			}
			for k := range req.Header {
				req.Header.Del(k)
			}
		}
		if oldRedirect != nil {
			return oldRedirect(req, via)
		}
		return nil
	}
	return HTTPProvider{routingIdentity: proxyroute.Identity(proxyroute.Config{}), Client: &copyClient, BaseURLs: map[string]string{}, Headers: http.Header{"User-Agent": []string{"Mozilla/5.0 AniDan/0.1"}}, MaxBytes: 32 << 20, MaxSegments: 300, MaxComments: 500000, MaxDownloadBytes: 64 << 20, SegmentWorkers: 4, flights: newCommentFlights(), gate: make(chan struct{}, 4)}
}

type HTTPError struct {
	Status     int
	Host       string
	RetryAfter string
}

func (e *HTTPError) Error() string { return fmt.Sprintf("upstream %s HTTP %d", e.Host, e.Status) }
func (h *HTTPProvider) request(ctx context.Context, method, raw string, q url.Values, body io.Reader, headers http.Header) ([]byte, error) {
	if h.flights != nil && h.flights.life.isClosed() {
		return nil, ErrRegistryClosed
	}
	if _, ok := ctx.Deadline(); !ok {
		timeout := 20 * time.Second
		if h.Client != nil && h.Client.Timeout > 0 {
			timeout = h.Client.Timeout
		}
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	if h.gate != nil {
		select {
		case h.gate <- struct{}{}:
			defer func() { <-h.gate }()
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	u, e := url.Parse(raw)
	if e != nil {
		return nil, e
	}
	if q != nil {
		v := u.Query()
		for k, vs := range q {
			v[k] = vs
		}
		u.RawQuery = v.Encode()
	}
	if base, ok := h.BaseURLs[u.Host]; ok {
		b, er := url.Parse(base)
		if er != nil {
			return nil, er
		}
		u.Scheme = b.Scheme
		u.Host = b.Host
		u.Path = strings.TrimRight(b.Path, "/") + u.Path
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return nil, errors.New("unsupported provider URL scheme")
	}
	req, e := http.NewRequestWithContext(ctx, method, u.String(), body)
	if e != nil {
		return nil, e
	}
	for k, v := range h.Headers {
		req.Header[k] = append([]string(nil), v...)
	}
	for k, v := range headers {
		req.Header[k] = append([]string(nil), v...)
	}
	client := h.Client
	if client == nil {
		client = &http.Client{Timeout: 20 * time.Second}
	}
	if h.acquireRequest != nil {
		release, err := h.acquireRequest(ctx, h.sourceName)
		if err != nil {
			return nil, err
		}
		if release != nil {
			defer release()
		}
	}
	resp, e := client.Do(req)
	if e != nil {
		if h.logResponses {
			safelog.Response(ctx, "source", h.sourceName, 0, "network-error", nil, false)
		}
		return nil, &NetworkError{cause: e}
	}
	defer resp.Body.Close()
	if h.responseFeedback != nil {
		h.responseFeedback(h.sourceName, resp.StatusCode, resp.Header.Get("Retry-After"))
	}
	max := h.MaxBytes
	if max <= 0 {
		max = 32 << 20
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if h.logResponses {
			// Error bodies were previously unread. Only opt-in diagnostics consume
			// them, with the smaller of the existing response and logging limits.
			limit := min(max, int64(safelog.MaxInputBytes))
			b, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
			safelog.Response(ctx, "source", h.sourceName, resp.StatusCode, "http-error", b, err == nil && int64(len(b)) <= limit)
		}
		return nil, &HTTPError{Status: resp.StatusCode, Host: u.Host, RetryAfter: resp.Header.Get("Retry-After")}
	}
	if resp.ContentLength > max {
		if h.logResponses {
			safelog.Response(ctx, "source", h.sourceName, resp.StatusCode, "response-limit", nil, false)
		}
		return nil, danmaku.ErrLimit
	}
	b, e := io.ReadAll(io.LimitReader(resp.Body, max+1))
	if e != nil {
		if h.logResponses {
			safelog.Response(ctx, "source", h.sourceName, resp.StatusCode, "read-error", b, false)
		}
		return nil, e
	}
	if int64(len(b)) > max {
		if h.logResponses {
			safelog.Response(ctx, "source", h.sourceName, resp.StatusCode, "response-limit", b, false)
		}
		return nil, danmaku.ErrLimit
	}
	if h.logResponses {
		safelog.Response(ctx, "source", h.sourceName, resp.StatusCode, "received", b, true)
	}
	return b, nil
}
func (h *HTTPProvider) get(ctx context.Context, raw string, q url.Values, target any) error {
	b, e := h.request(ctx, http.MethodGet, raw, q, nil, nil)
	if e != nil {
		return e
	}
	return decodeJSON(b, target)
}
func decodeJSON(b []byte, target any) error {
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	if e := d.Decode(target); e != nil {
		return fmt.Errorf("invalid provider JSON: %w", e)
	}
	var extra any
	if e := d.Decode(&extra); e != io.EOF {
		return errors.New("trailing provider JSON")
	}
	return nil
}
func (h *HTTPProvider) maxSegments() int {
	if h.MaxSegments < 1 {
		return 300
	}
	return min(h.MaxSegments, 1000)
}
func (h *HTTPProvider) appendBudget(out []danmaku.Comment, total *int64, in ...danmaku.Comment) ([]danmaku.Comment, error) {
	limit := h.MaxComments
	if limit < 1 {
		limit = 500000
	}
	if len(in) > limit-len(out) {
		return nil, danmaku.ErrLimit
	}
	added := int64(0)
	for _, c := range in {
		added += int64(len(c.P) + len(c.M) + 64)
	}
	budget := h.MaxDownloadBytes
	if budget <= 0 {
		budget = 64 << 20
	}
	if added > budget-*total {
		return nil, danmaku.ErrLimit
	}
	*total += added
	return append(out, in...), nil
}

func validID(s string) error {
	if s == "" || len(s) > 256 || strings.ContainsAny(s, "/\\?#\x00\r\n") {
		return errors.New("invalid provider identifier")
	}
	return nil
}
func str(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case json.Number:
		return t.String()
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case int:
		return strconv.Itoa(t)
	case nil:
		return ""
	}
	return ""
}
func integer(v any) int { n, _ := strconv.Atoi(str(v)); return n }
func number(v any) float64 {
	n, e := strconv.ParseFloat(str(v), 64)
	if e != nil {
		return math.NaN()
	}
	return n
}
func object(v any) map[string]any { m, _ := v.(map[string]any); return m }
func array(v any) []any           { a, _ := v.([]any); return a }
func at(m map[string]any, path ...string) any {
	var v any = m
	for _, key := range path {
		v = object(v)[key]
	}
	return v
}
func textFirst(v ...any) string {
	for _, x := range v {
		if s := str(x); s != "" {
			return s
		}
	}
	return ""
}
func comment(cid any, t float64, mode, color int, text, source string) (danmaku.Comment, error) {
	id, _ := strconv.ParseInt(str(cid), 10, 64)
	return danmaku.NewComment(id, t, mode, 25, color, text, "["+source+"]")
}

func trustedRedirect(host string) bool {
	host = strings.ToLower(host)
	for _, suffix := range []string{"bilibili.com", "bilibili.cn", "dandanplay.net", "dandanplay.com", "gamer.com.tw", "mgtv.com", "hitv.com", "hiyun.tv", "zmdcq.com", "video.qq.com", "sohu.com", "le.com", "iqiyi.com", "youku.com", "rrmj.plus"} {
		if host == suffix || strings.HasSuffix(host, "."+suffix) {
			return true
		}
	}
	return false
}

var sharedProviderTransport = func() *http.Transport {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.MaxIdleConns = 128
	tr.MaxIdleConnsPerHost = 16
	tr.MaxConnsPerHost = 16
	tr.IdleConnTimeout = 90 * time.Second
	return tr
}()

// NetworkError retains cancellation/timeout identity while redacting URLs and
// proxy credentials from public errors.
type NetworkError struct{ cause error }

func (e *NetworkError) Error() string {
	if errors.Is(e.cause, proxyroute.ErrUntrustedGateway) {
		return proxyroute.ErrUntrustedGateway.Error()
	}
	if t, ok := e.cause.(interface{ Timeout() bool }); ok && t.Timeout() {
		return "provider request timed out"
	}
	return "provider network request failed"
}
func (e *NetworkError) Unwrap() error { return e.cause }
