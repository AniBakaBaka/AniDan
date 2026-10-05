// SPDX-License-Identifier: AGPL-3.0-only
package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"path/filepath"
	"strings"
	"time"

	"github.com/AniBakaBaka/AniDan/internal/store"
	_ "golang.org/x/image/webp"
)

const libPosterMaxBytes int64 = 10 << 20

var libPosterSlots = make(chan struct{}, 2)

func libPublicAddress(addr netip.Addr) bool {
	addr = addr.Unmap()
	if !addr.IsValid() || !addr.IsGlobalUnicast() || addr.IsPrivate() || addr.IsLoopback() || addr.IsLinkLocalUnicast() {
		return false
	}
	for _, cidr := range []string{"0.0.0.0/8", "100.64.0.0/10", "192.0.0.0/24", "192.0.2.0/24", "198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24", "240.0.0.0/4", "2001:db8::/32", "2002::/16", "64:ff9b::/96", "64:ff9b:1::/48"} {
		if netip.MustParsePrefix(cidr).Contains(addr) {
			return false
		}
	}
	return true
}
func libPosterURL(raw string) (*url.URL, error) {
	u, e := url.Parse(raw)
	if e != nil || u.Hostname() == "" || u.User != nil || u.Fragment != "" || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, errors.New("image URL must be an HTTP(S) URL without credentials")
	}
	if p := u.Port(); p != "" && p != "80" && p != "443" {
		return nil, errors.New("image URL port is not allowed")
	}
	if ip, e := netip.ParseAddr(u.Hostname()); e == nil && !libPublicAddress(ip) {
		return nil, errors.New("image URL must address a public host")
	}
	return u, nil
}
func libPosterClient() *http.Client {
	transport := &http.Transport{Proxy: nil, ForceAttemptHTTP2: true, TLSHandshakeTimeout: 10 * time.Second, ResponseHeaderTimeout: 10 * time.Second, MaxConnsPerHost: 2, DisableKeepAlives: true}
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, e := net.SplitHostPort(address)
		if e != nil {
			return nil, e
		}
		ips, e := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
		if e != nil {
			return nil, e
		}
		if len(ips) == 0 {
			return nil, errors.New("image host has no addresses")
		}
		for _, ip := range ips {
			if !libPublicAddress(ip) {
				return nil, errors.New("image host resolved to a nonpublic address")
			}
		}
		dialer := net.Dialer{Timeout: 10 * time.Second}
		var last error
		for _, ip := range ips {
			c, e := dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
			if e == nil {
				return c, nil
			}
			last = e
		}
		return nil, last
	}
	return &http.Client{Transport: transport, Timeout: 25 * time.Second, CheckRedirect: func(r *http.Request, via []*http.Request) error {
		if len(via) >= 3 {
			return errors.New("too many image redirects")
		}
		if _, e := libPosterURL(r.URL.String()); e != nil {
			return e
		}
		if len(via) > 0 && via[0].URL.Scheme == "https" && r.URL.Scheme != "https" {
			return errors.New("image HTTPS downgrade blocked")
		}
		return nil
	}}
}

type libHTTPDoer interface {
	Do(*http.Request) (*http.Response, error)
}

func libFetchPoster(ctx context.Context, raw string, client libHTTPDoer) ([]byte, string, error) {
	u, e := libPosterURL(raw)
	if e != nil {
		return nil, "", e
	}
	request, e := http.NewRequestWithContext(ctx, "GET", u.String(), nil)
	if e != nil {
		return nil, "", e
	}
	request.Header.Set("Accept", "image/jpeg,image/png,image/webp,image/gif")
	request.Header.Set("User-Agent", "AniDan/1.0")
	response, e := client.Do(request)
	if e != nil {
		return nil, "", e
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		return nil, "", fmt.Errorf("image provider returned HTTP %d", response.StatusCode)
	}
	if response.ContentLength > libPosterMaxBytes {
		return nil, "", errors.New("image exceeds 10 MiB")
	}
	b, e := io.ReadAll(io.LimitReader(response.Body, libPosterMaxBytes+1))
	if e != nil {
		return nil, "", e
	}
	if int64(len(b)) > libPosterMaxBytes {
		return nil, "", errors.New("image exceeds 10 MiB")
	}
	cfg, format, e := image.DecodeConfig(bytes.NewReader(b))
	if e != nil {
		return nil, "", errors.New("response is not a supported image")
	}
	if cfg.Width < 1 || cfg.Height < 1 || cfg.Width > 8192 || cfg.Height > 8192 || int64(cfg.Width)*int64(cfg.Height) > 20_000_000 {
		return nil, "", errors.New("image dimensions exceed safe limits")
	}
	ext := map[string]string{"jpeg": "jpg", "png": "png", "gif": "gif", "webp": "webp"}[format]
	if ext == "" {
		return nil, "", errors.New("unsupported image format")
	}
	return b, ext, nil
}
func (s *Server) libCachePoster(b []byte, ext string) (string, error) {
	return s.libCachePosterContext(context.Background(), b, ext)
}

func (s *Server) libCachePosterContext(ctx context.Context, b []byte, ext string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if ext != "jpg" && ext != "png" && ext != "gif" && ext != "webp" {
		return "", errors.New("invalid image extension")
	}
	if len(b) == 0 || int64(len(b)) > libPosterMaxBytes {
		return "", errors.New("image object exceeds cache bounds")
	}
	hash := sha256.Sum256(b)
	name := fmt.Sprintf("%x.%s", hash, ext)
	if _, err := s.writeManagedBytes(ctx, filepath.Join("image", name), b, libPosterMaxBytes); err != nil {
		return "", err
	}
	return "/data/images/" + name, nil
}

func (s *Server) libRefreshPoster(w http.ResponseWriter, r *http.Request) {
	if !s.libAuthorized(w, r) {
		return
	}
	id, e := idParam(r, "animeId")
	if e != nil {
		httpError(w, 422, "Invalid anime ID")
		return
	}
	if e = s.libExisting(r, "anime", id); e != nil {
		libWriteError(w, e)
		return
	}
	var b struct {
		URL string `json:"imageUrl"`
	}
	if readJSON(r, &b) != nil || len(b.URL) > 2048 || strings.TrimSpace(b.URL) == "" {
		httpError(w, 422, "imageUrl is required")
		return
	}
	select {
	case libPosterSlots <- struct{}{}:
		defer func() { <-libPosterSlots }()
	case <-r.Context().Done():
		return
	}
	data, ext, e := libFetchPoster(r.Context(), b.URL, libPosterClient())
	if e != nil {
		httpError(w, 400, "图片下载失败："+e.Error())
		return
	}
	path, e := s.libCachePosterContext(r.Context(), data, ext)
	if e != nil {
		httpError(w, 500, "Unable to cache poster")
		return
	}
	if e = s.Store.Update(r.Context(), "anime", id, store.Row{"image_url": b.URL, "local_image_path": path}); e != nil {
		libWriteError(w, e)
		return
	}
	writeJSON(w, 200, store.Row{"new_path": path})
}
