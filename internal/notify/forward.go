// SPDX-License-Identifier: AGPL-3.0-only
package notify

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"time"

	"golang.org/x/net/proxy"
)

// net/http detaches cancellation while dialing reusable connections. Retain the
// actual request context as a value so proxy negotiation cannot outlive a canceled
// request, or leave an indefinitely stalled SOCKS/CONNECT socket behind.
type requestContextKey struct{}

func proxyDialContext(ctx context.Context) (context.Context, context.CancelFunc) {
	source := ctx
	if request, ok := ctx.Value(requestContextKey{}).(context.Context); ok {
		source = request
	}
	bounded, cancel := context.WithTimeout(source, 5*time.Second)
	stop := context.AfterFunc(ctx, cancel)
	return bounded, func() { stop(); cancel() }
}

type establishedProxyConn struct{ net.Conn }

func (c establishedProxyConn) Dial(string, string) (net.Conn, error) { return c.Conn, nil }
func (c establishedProxyConn) DialContext(context.Context, string, string) (net.Conn, error) {
	return c.Conn, nil
}

type proxyBufferedConn struct {
	net.Conn
	reader io.Reader
}

func (c *proxyBufferedConn) Read(p []byte) (int, error) { return c.reader.Read(p) }

func forwardProxyDial(p *url.URL, tlsConfig *tls.Config) func(context.Context, string, string) (net.Conn, error) {
	return func(ctx context.Context, network, target string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(target)
		if err != nil || port != "443" || host != "api.telegram.org" {
			return nil, ErrForbidden
		}
		ctx, cancel := proxyDialContext(ctx)
		defer cancel()
		proxyPort := p.Port()
		if proxyPort == "" {
			proxyPort = "1080"
			if p.Scheme == "http" {
				proxyPort = "80"
			} else if p.Scheme == "https" {
				proxyPort = "443"
			}
		}
		raw, err := (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext(ctx, network, net.JoinHostPort(p.Hostname(), proxyPort))
		if err != nil {
			return nil, transportFailure(ctx, err, "notification proxy connection failed")
		}
		success := false
		defer func() {
			if !success {
				raw.Close()
			}
		}()
		stop := context.AfterFunc(ctx, func() { raw.Close() })
		defer stop()
		deadline, _ := ctx.Deadline()
		_ = raw.SetDeadline(deadline)
		var conn net.Conn = raw
		if p.Scheme == "socks5" || p.Scheme == "socks5h" {
			var auth *proxy.Auth
			if p.User != nil {
				password, _ := p.User.Password()
				auth = &proxy.Auth{User: p.User.Username(), Password: password}
			}
			d, err := proxy.SOCKS5("tcp", net.JoinHostPort(p.Hostname(), proxyPort), auth, establishedProxyConn{raw})
			if err != nil {
				return nil, errors.New("notification SOCKS proxy configuration failed")
			}
			conn, err = d.(proxy.ContextDialer).DialContext(ctx, network, target)
			if err != nil {
				return nil, transportFailure(ctx, err, "notification SOCKS proxy negotiation failed")
			}
		} else {
			if p.Scheme == "https" {
				cfg := tlsConfig.Clone()
				cfg.ServerName = p.Hostname()
				cfg.InsecureSkipVerify = false
				cfg.NextProtos = nil
				secure := tls.Client(raw, cfg)
				if err = secure.HandshakeContext(ctx); err != nil {
					return nil, transportFailure(ctx, err, "notification HTTPS proxy TLS verification failed")
				}
				conn = secure
			}
			request := &http.Request{Method: "CONNECT", URL: &url.URL{Opaque: target}, Host: target, Header: make(http.Header)}
			if p.User != nil {
				password, _ := p.User.Password()
				request.Header.Set("Proxy-Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(p.User.Username()+":"+password)))
			}
			if err = request.Write(conn); err != nil {
				return nil, transportFailure(ctx, err, "notification proxy CONNECT failed")
			}
			limited := &io.LimitedReader{R: conn, N: 32 << 10}
			reader := bufio.NewReader(limited)
			response, err := http.ReadResponse(reader, request)
			if err != nil || response.StatusCode != http.StatusOK || limited.N <= 0 {
				return nil, transportFailure(ctx, err, "notification proxy CONNECT was not accepted")
			}
			var prefix []byte
			if n := reader.Buffered(); n > 0 {
				b, _ := reader.Peek(n)
				prefix = append(prefix, b...)
			}
			conn = &proxyBufferedConn{Conn: conn, reader: io.MultiReader(bytes.NewReader(prefix), conn)}
		}
		if err := cancellationError(ctx, nil); err != nil {
			return nil, err
		}
		_ = conn.SetDeadline(time.Time{})
		success = true
		return conn, nil
	}
}
