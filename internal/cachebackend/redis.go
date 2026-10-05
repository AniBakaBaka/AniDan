// SPDX-License-Identifier: AGPL-3.0-only
package cachebackend

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

type redisAddress struct {
	address, host, user, password string
	db                            int
	tls                           bool
	auth                          bool
}

func parseRedisURL(raw string) (redisAddress, error) {
	bad := func() (redisAddress, error) {
		return redisAddress{}, errors.New("invalid Redis URL: require redis/rediss/valkey/valkeys host, optional port and numeric database; query/fragment options are unsupported")
	}
	if raw == "" || len(raw) > 4096 {
		return bad()
	}
	u, e := url.Parse(raw)
	if e != nil || u.Opaque != "" || u.Host == "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawFragment != "" || u.RawPath != "" {
		return bad()
	}
	secure := false
	switch u.Scheme {
	case "redis", "valkey":
	case "rediss", "valkeys":
		secure = true
	default:
		return bad()
	}
	host := u.Hostname()
	if host == "" || len(host) > 253 || strings.ContainsAny(host, " \t\r\n/%@\\") {
		return bad()
	}
	if net.ParseIP(host) == nil {
		for _, r := range host {
			if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '.' || r == '-') {
				return bad()
			}
		}
	}
	port := u.Port()
	if strings.HasSuffix(u.Host, ":") {
		return bad()
	}
	if port == "" {
		port = "6379"
	}
	n, e := strconv.Atoi(port)
	if e != nil || n < 1 || n > 65535 {
		return bad()
	}
	path := strings.TrimPrefix(u.Path, "/")
	db := 0
	if path != "" {
		if u.Path != "/"+path || len(path) > 9 {
			return bad()
		}
		for _, c := range path {
			if c < '0' || c > '9' {
				return bad()
			}
		}
		db, e = strconv.Atoi(path)
		if e != nil || db < 0 {
			return bad()
		}
	}
	a := redisAddress{address: net.JoinHostPort(host, port), host: host, db: db, tls: secure}
	if u.User != nil {
		a.user = u.User.Username()
		a.password, a.auth = u.User.Password()
		if a.user != "" {
			a.auth = true
		}
		if len(a.user) > 1024 || len(a.password) > 1024 {
			return bad()
		}
	}
	return a, nil
}

// ValidateRedisURL deliberately never returns the input URL or credentials.
func ValidateRedisURL(raw string) error { _, e := parseRedisURL(raw); return e }

type redisConn struct {
	net.Conn
	reader *bufio.Reader
	writer *bufio.Writer
}
type redisBackend struct {
	options Options
	address redisAddress
	prefix  string
	slots   chan struct{}
	idle    chan *redisConn
	mu      sync.Mutex
	closed  bool
	active  map[*redisConn]bool
}

func newRedis(ctx context.Context, o Options) (backend, error) {
	a, e := parseRedisURL(o.RedisURL)
	if e != nil {
		return nil, e
	}
	r := &redisBackend{options: o, address: a, prefix: namespacePrefix(o.Namespace), slots: make(chan struct{}, o.RedisPoolSize), idle: make(chan *redisConn, o.RedisPoolSize), active: make(map[*redisConn]bool)}
	v, e := r.command(ctx, 128, "PING")
	if e != nil {
		_ = r.close()
		return nil, e
	}
	if textReply(v) != "PONG" {
		_ = r.close()
		return nil, ErrCorrupt
	}
	if _, e = r.generation(ctx); e != nil {
		_ = r.close()
		return nil, e
	}
	return r, nil
}
func (r *redisBackend) dial(ctx context.Context) (*redisConn, error) {
	d := &net.Dialer{Timeout: r.options.ConnectTimeout}
	raw, e := d.DialContext(ctx, "tcp", r.address.address)
	if e != nil {
		return nil, cacheError("Redis connect", preferContext(ctx, e))
	}
	if r.address.tls {
		c := tls.Client(raw, &tls.Config{ServerName: r.address.host, MinVersion: tls.VersionTLS12})
		hctx, cancel := context.WithTimeout(ctx, r.options.ConnectTimeout)
		e = c.HandshakeContext(hctx)
		cancel()
		if e != nil {
			_ = raw.Close()
			return nil, cacheError("Redis verified TLS", preferContext(ctx, e))
		}
		raw = c
	}
	c := &redisConn{Conn: raw, reader: bufio.NewReaderSize(raw, 4096), writer: bufio.NewWriterSize(raw, 4096)}
	finished := make(chan struct{})
	stop := context.AfterFunc(ctx, func() { _ = c.SetDeadline(time.Now()); close(finished) })
	defer func() {
		if !stop() {
			<-finished
		}
	}()

	if deadline, ok := ctx.Deadline(); ok {
		_ = c.SetDeadline(deadline)
	}
	if r.address.auth {
		args := []string{"AUTH", r.address.password}
		if r.address.user != "" {
			args = []string{"AUTH", r.address.user, r.address.password}
		}
		v, e := exchange(c, 128, args...)
		if e != nil || textReply(v) != "OK" {
			_ = c.Close()
			return nil, cacheError("Redis authentication", preferContext(ctx, ErrCorrupt))
		}
	}
	if r.address.db != 0 {
		v, e := exchange(c, 128, "SELECT", strconv.Itoa(r.address.db))
		if e != nil || textReply(v) != "OK" {
			_ = c.Close()
			return nil, cacheError("Redis database selection", preferContext(ctx, ErrCorrupt))
		}
	}
	return c, nil
}
func (r *redisBackend) command(ctx context.Context, maxReply int, args ...string) (any, error) {
	ctx, cancel := context.WithTimeout(ctx, r.options.SocketTimeout)
	defer cancel()
	if e := ctx.Err(); e != nil {
		return nil, e
	}
	select {
	case r.slots <- struct{}{}:
		defer func() { <-r.slots }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	r.mu.Lock()
	closed := r.closed
	r.mu.Unlock()
	if closed {
		return nil, ErrClosed
	}
	var c *redisConn
	select {
	case c = <-r.idle:
	default:
	}
	var e error
	if c == nil {
		c, e = r.dial(ctx)
		if e != nil {
			return nil, e
		}
	}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		_ = c.Close()
		return nil, ErrClosed
	}
	r.active[c] = true
	r.mu.Unlock()
	if deadline, ok := ctx.Deadline(); ok {
		_ = c.SetDeadline(deadline)
	}
	finished := make(chan struct{})
	stop := context.AfterFunc(ctx, func() { _ = c.SetDeadline(time.Now()); close(finished) })
	v, e := exchange(c, maxReply, args...)
	if !stop() {
		<-finished
	}
	if ctx.Err() != nil {
		e = ctx.Err()
	}
	r.mu.Lock()
	delete(r.active, c)
	if e == nil && !r.closed {
		_ = c.SetDeadline(time.Time{})
		r.idle <- c
	} else {
		_ = c.Close()
	}
	r.mu.Unlock()
	if e != nil {
		if errors.Is(e, ErrTooLarge) || errors.Is(e, ErrCorrupt) {
			return nil, e
		}
		return nil, cacheError("Redis transport", preferContext(ctx, e))
	}
	return v, nil
}
func exchange(c *redisConn, maxReply int, args ...string) (any, error) {
	if _, e := fmt.Fprintf(c.writer, "*%d\r\n", len(args)); e != nil {
		return nil, e
	}
	for _, a := range args {
		if _, e := fmt.Fprintf(c.writer, "$%d\r\n", len(a)); e != nil {
			return nil, e
		}
		if _, e := c.writer.WriteString(a); e != nil {
			return nil, e
		}
		if _, e := c.writer.WriteString("\r\n"); e != nil {
			return nil, e
		}
	}
	if e := c.writer.Flush(); e != nil {
		return nil, e
	}
	budget := maxReply
	return readRESP(c.reader, &budget, 0)
}
func readRESP(rd *bufio.Reader, budget *int, depth int) (any, error) {
	if depth > 4 || *budget <= 0 {
		return nil, ErrTooLarge
	}
	line, e := rd.ReadSlice('\n')
	if e != nil {
		if errors.Is(e, bufio.ErrBufferFull) {
			return nil, ErrTooLarge
		}
		return nil, e
	}
	if len(line) < 3 || len(line) > 256 || line[len(line)-2] != '\r' {
		return nil, ErrCorrupt
	}
	*budget -= len(line)
	if *budget < 0 {
		return nil, ErrTooLarge
	}
	body := line[1 : len(line)-2]
	switch line[0] {
	case '+':
		return string(body), nil
	case '-':
		return nil, errors.New("Redis rejected cache operation")
	case ':':
		n, e := strconv.ParseInt(string(body), 10, 64)
		if e != nil {
			return nil, ErrCorrupt
		}
		return n, nil
	case '$':
		n, e := strconv.ParseInt(string(body), 10, 64)
		if e != nil || n < -1 {
			return nil, ErrCorrupt
		}
		if n == -1 {
			return nil, nil
		}
		if n > int64(*budget)-2 {
			return nil, ErrTooLarge
		}
		b := make([]byte, int(n)+2)
		if _, e = io.ReadFull(rd, b); e != nil {
			return nil, e
		}
		if b[n] != '\r' || b[n+1] != '\n' {
			return nil, ErrCorrupt
		}
		*budget -= len(b)
		return b[:n], nil
	case '*':
		n, e := strconv.ParseInt(string(body), 10, 64)
		if e != nil || n < -1 {
			return nil, ErrCorrupt
		}
		if n == -1 {
			return nil, nil
		}
		if n > 512 || n*8 > int64(*budget) {
			return nil, ErrTooLarge
		}
		*budget -= int(n) * 8
		out := make([]any, int(n))
		for i := range out {
			out[i], e = readRESP(rd, budget, depth+1)
			if e != nil {
				return nil, e
			}
		}
		return out, nil
	default:
		return nil, ErrCorrupt
	}
}
func textReply(v any) string {
	switch v := v.(type) {
	case []byte:
		return string(v)
	case string:
		return v
	}
	return ""
}
func intReply(v any) (int64, error) {
	switch v := v.(type) {
	case int64:
		return v, nil
	case []byte:
		n, e := strconv.ParseInt(string(v), 10, 64)
		if e != nil {
			return 0, ErrCorrupt
		}
		return n, nil
	case string:
		n, e := strconv.ParseInt(v, 10, 64)
		if e != nil {
			return 0, ErrCorrupt
		}
		return n, nil
	}
	return 0, ErrCorrupt
}
func (r *redisBackend) eval(ctx context.Context, operation, key string, payload []byte, expires time.Time, expected, region, after string, limit int) ([]any, error) {
	ms := int64(0)
	if !expires.IsZero() {
		ms = expires.UnixMilli()
	}
	args := []string{"EVAL", redisScript, "2", r.prefix + "!index", r.prefix + "!meta", operation, r.prefix, nonce(), strconv.Itoa(r.options.MaxEntries), strconv.FormatInt(r.options.MaxBytes, 10), strconv.Itoa(maxEnvelope(r.options)), key, string(payload), strconv.FormatInt(ms, 10), expected, region, after, strconv.Itoa(limit)}
	budget := 128 * 1024
	if operation == "get" {
		budget = maxEnvelope(r.options) + 1024
	}
	v, e := r.command(ctx, budget, args...)
	if e != nil {
		return nil, e
	}
	out, ok := v.([]any)
	if !ok || len(out) < 1 {
		return nil, ErrCorrupt
	}
	switch textReply(out[0]) {
	case "OK":
		return out[1:], nil
	case "MISS":
		return nil, ErrMiss
	case "QUOTA":
		return nil, ErrQuota
	case "LARGE":
		return nil, ErrTooLarge
	case "CORRUPT":
		return nil, ErrCorrupt
	default:
		return nil, ErrCorrupt
	}
}
func (r *redisBackend) get(ctx context.Context, key string) ([]byte, error) {
	v, e := r.eval(ctx, "get", key, nil, time.Time{}, "", "", "", 0)
	if e != nil {
		return nil, e
	}
	if len(v) != 1 {
		return nil, ErrCorrupt
	}
	b, ok := v[0].([]byte)
	if !ok {
		return nil, ErrCorrupt
	}
	return b, nil
}
func (r *redisBackend) set(ctx context.Context, key string, b []byte, expires time.Time, expected string) (bool, error) {
	v, e := r.eval(ctx, "set", key, b, expires, expected, "", "", 0)
	if e != nil {
		return false, e
	}
	if len(v) != 1 {
		return false, ErrCorrupt
	}
	n, e := intReply(v[0])
	return n == 1, e
}
func (r *redisBackend) delete(ctx context.Context, key string) error {
	_, e := r.eval(ctx, "delete", key, nil, time.Time{}, "", "", "", 0)
	return e
}
func (r *redisBackend) clear(ctx context.Context, region string) (int, error) {
	v, e := r.eval(ctx, "clear", "", nil, time.Time{}, "", region, "", 0)
	if e != nil {
		return 0, e
	}
	if len(v) != 1 {
		return 0, ErrCorrupt
	}
	n, e := intReply(v[0])
	return int(n), e
}
func (r *redisBackend) generation(ctx context.Context) (string, error) {
	v, e := r.eval(ctx, "generation", "", nil, time.Time{}, "", "", "", 0)
	if e != nil {
		return "", e
	}
	if len(v) != 1 || !isNonce(textReply(v[0])) {
		return "", ErrCorrupt
	}
	return textReply(v[0]), nil
}
func (r *redisBackend) stats(ctx context.Context, region string) (Stats, error) {
	v, e := r.eval(ctx, "stats", "", nil, time.Time{}, "", region, "", 0)
	if e != nil {
		return Stats{}, e
	}
	if len(v) != 2 {
		return Stats{}, ErrCorrupt
	}
	n, e := intReply(v[0])
	if e != nil {
		return Stats{}, e
	}
	b, e := intReply(v[1])
	if e != nil {
		return Stats{}, e
	}
	return Stats{Entries: int(n), Bytes: b}, nil
}
func (r *redisBackend) list(ctx context.Context, region, after string, limit int) (Page, error) {
	v, e := r.eval(ctx, "list", "", nil, time.Time{}, "", region, after, limit)
	if e != nil {
		return Page{}, e
	}
	if len(v) < 1 || (len(v)-1)%3 != 0 || (len(v)-1)/3 > limit {
		return Page{}, ErrCorrupt
	}
	out := Page{Items: []Item{}, Next: textReply(v[len(v)-1])}
	for i := 0; i < len(v)-1; i += 3 {
		key := textReply(v[i])
		if !strings.HasPrefix(key, r.prefix) {
			return Page{}, ErrCorrupt
		}
		n, e := intReply(v[i+1])
		if e != nil {
			return Page{}, e
		}
		expires, e := intReply(v[i+2])
		if e != nil {
			return Page{}, e
		}
		it, e := itemFromKey(key, n, time.UnixMilli(expires))
		if e != nil {
			return Page{}, e
		}
		out.Items = append(out.Items, it)
	}
	if out.Next != "" {
		if !strings.HasPrefix(out.Next, r.prefix) {
			return Page{}, ErrCorrupt
		}
		if _, e := itemFromKey(out.Next, 0, time.Time{}); e != nil {
			return Page{}, e
		}
	}
	return out, nil
}
func (r *redisBackend) close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil
	}
	r.closed = true
	for c := range r.active {
		_ = c.Close()
	}
	for {
		select {
		case c := <-r.idle:
			_ = c.Close()
		default:
			return nil
		}
	}
}
