// SPDX-License-Identifier: AGPL-3.0-only
// Package cachebackend provides bounded, disposable JSON response caches. It is
// never an authority for jobs, notification receipts, cursors, or virtual IDs.
package cachebackend

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/AniBakaBaka/AniDan/internal/store"
	"golang.org/x/sync/semaphore"
)

const MaxTTL = 7 * 24 * time.Hour

// Response-path budgets are deliberately shorter than explicit management I/O.
const ResponseLookupBudget = 250 * time.Millisecond
const ResponsePublishBudget = 100 * time.Millisecond

var (
	ErrMiss      = errors.New("cache miss")
	ErrCorrupt   = errors.New("invalid cache envelope")
	ErrTooLarge  = errors.New("cache value exceeds limit")
	ErrQuota     = errors.New("cache namespace quota exceeded")
	ErrClosed    = errors.New("cache is closed")
	ErrOwnership = errors.New("cache ownership conflict")
)

type Options struct {
	Backend, Namespace, RedisURL, Timezone    string
	MaxEntries, MaxValueBytes, RedisPoolSize  int
	MaxBytes                                  int64
	DefaultTTL, SocketTimeout, ConnectTimeout time.Duration
	RedisFallback                             bool
}

func DefaultOptions() Options {
	return Options{Backend: "hybrid", Namespace: "default", Timezone: "UTC", MaxEntries: 1024, MaxValueBytes: 1 << 20, MaxBytes: 32 << 20, DefaultTTL: 600 * time.Second, SocketTimeout: 30 * time.Second, ConnectTimeout: 5 * time.Second, RedisPoolSize: 4, RedisFallback: true}
}
func (o Options) Validate() error {
	switch o.Backend {
	case "memory", "database", "hybrid", "redis", "valkey":
	default:
		return errors.New("cache backend must be memory, database, hybrid, or redis")
	}
	if !safePart(o.Namespace, 40) {
		return errors.New("cache namespace must be 1..40 lowercase letters, digits, underscore or hyphen")
	}
	if o.MaxEntries < 1 || o.MaxEntries > 65536 {
		return errors.New("cache max entries must be 1..65536")
	}
	if o.MaxValueBytes < 1024 || o.MaxValueBytes > 16<<20 {
		return errors.New("cache max value bytes must be 1024..16777216")
	}
	if o.MaxBytes < 1<<20 || o.MaxBytes > 256<<20 || o.MaxBytes < int64(o.MaxValueBytes)+256 {
		return errors.New("cache max bytes must be 1MiB..256MiB and exceed max value bytes by 256")
	}
	if o.DefaultTTL < time.Second || o.DefaultTTL > MaxTTL {
		return errors.New("cache default TTL must be 1 second..7 days")
	}
	if o.SocketTimeout < time.Second || o.SocketTimeout > 30*time.Second || o.ConnectTimeout < time.Second || o.ConnectTimeout > 5*time.Second {
		return errors.New("cache Redis timeouts must be 1..30 and 1..5 seconds")
	}
	if o.RedisPoolSize < 1 || o.RedisPoolSize > 16 {
		return errors.New("cache Redis pool size must be 1..16")
	}
	if _, err := time.LoadLocation(o.Timezone); err != nil {
		return errors.New("invalid cache timezone")
	}
	if o.Backend == "redis" || o.Backend == "valkey" || o.RedisURL != "" {
		return ValidateRedisURL(o.RedisURL)
	}
	return nil
}
func safePart(s string, max int) bool {
	if len(s) < 1 || len(s) > max {
		return false
	}
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '_' || c == '-') {
			return false
		}
	}
	return true
}
func namespacePrefix(ns string) string { return "anidan:cache:v1:" + ns + ":" }
func maxEnvelope(o Options) int        { return o.MaxValueBytes + 128 }
func nonce() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("cache generation entropy unavailable")
	}
	return hex.EncodeToString(b[:])
}

type Entry struct {
	JSON      json.RawMessage `json:"json"`
	ExpiresAt time.Time       `json:"expiresAt"`
}
type Item struct {
	Key       string    `json:"key"`
	Region    string    `json:"region"`
	Bytes     int64     `json:"bytes"`
	ExpiresAt time.Time `json:"expiresAt"`
}
type Page struct {
	Items []Item `json:"items"`
	Next  string `json:"next,omitempty"`
}
type Stats struct {
	Entries int   `json:"entries"`
	Bytes   int64 `json:"bytes"`
}
type Health struct {
	Configured              string    `json:"configured"`
	Effective               string    `json:"effective"`
	Degraded                bool      `json:"degraded"`
	Reason                  string    `json:"reason,omitempty"`
	Closed                  bool      `json:"closed"`
	LastSuccessfulOperation time.Time `json:"lastSuccessfulOperation,omitempty"`
	LastFailedOperation     time.Time `json:"lastFailedOperation,omitempty"`
}

// Token is opaque, scoped to this service, and valid until clear/delete/close.
// Region clearing conservatively fences publishers in every region.
type Token struct {
	owner  *Service
	epoch  uint64
	remote string
}
type backend interface {
	get(context.Context, string) ([]byte, error)
	set(context.Context, string, []byte, time.Time, string) (bool, error)
	delete(context.Context, string) error
	clear(context.Context, string) (int, error)
	generation(context.Context) (string, error)
	list(context.Context, string, string, int) (Page, error)
	stats(context.Context, string) (Stats, error)
	close() error
}
type flight struct {
	done    chan struct{}
	value   json.RawMessage
	err     error
	invalid bool
}
type Service struct {
	options Options
	engine  backend
	// Management takes an exclusive gate; reads/publishers share it.
	// All gate acquisition honors cancellation and a finite cache-attempt budget.
	gate          *semaphore.Weighted
	maxOps        int64
	mu            sync.Mutex
	closed        bool
	epoch         uint64
	health        Health
	flights       map[string]*flight
	slots         chan struct{}
	flightChanged chan struct{}
	activeFlights int
	closedCh      chan struct{}
}

func New(ctx context.Context, o Options, db *store.Store) (*Service, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := o.Validate(); err != nil {
		return nil, err
	}
	s := &Service{options: o, gate: semaphore.NewWeighted(int64(o.RedisPoolSize)), maxOps: int64(o.RedisPoolSize), flights: make(map[string]*flight), slots: make(chan struct{}, 4), flightChanged: make(chan struct{}), closedCh: make(chan struct{}), health: Health{Configured: o.Backend, Effective: o.Backend}}
	var err error
	switch o.Backend {
	case "memory":
		s.engine = newMemory(o)
	case "database", "hybrid":
		if db == nil {
			return nil, errors.New("SQL store is required for database/hybrid cache")
		}
		s.engine, err = newSQL(ctx, o, db)
		if err == nil && o.Backend == "hybrid" {
			s.engine = &hybridBackend{memory: newMemory(o), database: s.engine, ttl: o.DefaultTTL}
		}
	case "redis", "valkey":
		s.health.Effective = "redis"
		s.engine, err = newRedis(ctx, o)
		if err != nil && o.RedisFallback && ctx.Err() == nil {
			s.health.Degraded = true
			s.health.Reason = "Redis unavailable at startup; using fallback"
			s.health.LastFailedOperation = time.Now().UTC()
			if db != nil {
				s.engine, err = newSQL(ctx, o, db)
				if err == nil {
					s.engine = &hybridBackend{memory: newMemory(o), database: s.engine, ttl: o.DefaultTTL}
					s.health.Effective = "hybrid"
				}
			} else {
				s.engine = newMemory(o)
				err = nil
				s.health.Effective = "memory"
			}
		}
	}
	if err != nil {
		return nil, err
	}
	return s, nil
}
func (s *Service) key(region, key string) (string, error) {
	if !safePart(region, 48) {
		return "", errors.New("cache region must be 1..48 lowercase letters, digits, underscore or hyphen")
	}
	if key == "" || len(key) > 4096 || !utf8.ValidString(key) {
		return "", errors.New("cache key must be valid UTF-8 and 1..4096 bytes")
	}
	sum := sha256.Sum256([]byte(key))
	return namespacePrefix(s.options.Namespace) + region + ":" + hex.EncodeToString(sum[:]), nil
}
func itemFromKey(key string, n int64, expires time.Time) (Item, error) {
	if len(key) > 256 || !utf8.ValidString(key) {
		return Item{}, ErrCorrupt
	}
	p := strings.Split(key, ":")
	if len(p) != 6 || p[0] != "anidan" || p[1] != "cache" || p[2] != "v1" || !safePart(p[3], 40) || !safePart(p[4], 48) || len(p[5]) != 64 {
		return Item{}, ErrCorrupt
	}
	if p[5] != strings.ToLower(p[5]) {
		return Item{}, ErrCorrupt
	}
	if _, e := hex.DecodeString(p[5]); e != nil {
		return Item{}, ErrCorrupt
	}
	return Item{Key: key, Region: p[4], Bytes: n, ExpiresAt: expires}, nil
}
func validJSON(b []byte) bool { return utf8.Valid(b) && json.Valid(b) }
func encodeEntry(e Entry, o Options) ([]byte, error) {
	if len(e.JSON) > o.MaxValueBytes {
		return nil, ErrTooLarge
	}
	if !validJSON(e.JSON) {
		return nil, ErrCorrupt
	}
	remaining := time.Until(e.ExpiresAt)
	if remaining <= 0 || remaining > MaxTTL {
		return nil, errors.New("cache expiry must be in the next 7 days")
	}
	stamp, _ := e.ExpiresAt.UTC().MarshalJSON()
	b := make([]byte, 0, len(e.JSON)+96)
	b = append(b, `{"v":1,"expiresAt":`...)
	b = append(b, stamp...)
	b = append(b, `,"json":`...)
	b = append(b, e.JSON...)
	b = append(b, '}')
	return b, nil
}
func decodeEntry(b []byte, o Options) (Entry, error) {
	if len(b) > maxEnvelope(o) {
		return Entry{}, ErrTooLarge
	}
	if !utf8.Valid(b) {
		return Entry{}, ErrCorrupt
	}
	var v struct {
		Version   int             `json:"v"`
		ExpiresAt time.Time       `json:"expiresAt"`
		JSON      json.RawMessage `json:"json"`
	}
	d := json.NewDecoder(bytes.NewReader(b))
	opening, err := d.Token()
	if err != nil || opening != json.Delim('{') {
		return Entry{}, ErrCorrupt
	}
	seen := make(map[string]bool, 3)
	for d.More() {
		field, err := d.Token()
		if err != nil {
			return Entry{}, ErrCorrupt
		}
		name, ok := field.(string)
		if !ok || seen[name] {
			return Entry{}, ErrCorrupt
		}
		seen[name] = true
		switch name {
		case "v":
			err = d.Decode(&v.Version)
		case "expiresAt":
			err = d.Decode(&v.ExpiresAt)
		case "json":
			err = d.Decode(&v.JSON)
		default:
			return Entry{}, ErrCorrupt
		}
		if err != nil {
			return Entry{}, ErrCorrupt
		}
	}
	closing, err := d.Token()
	if err != nil || closing != json.Delim('}') || len(seen) != 3 {
		return Entry{}, ErrCorrupt
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return Entry{}, ErrCorrupt
	}
	if v.Version != 1 || v.ExpiresAt.IsZero() || len(v.JSON) > o.MaxValueBytes || !validJSON(v.JSON) {
		return Entry{}, ErrCorrupt
	}
	if !v.ExpiresAt.After(time.Now()) {
		return Entry{}, ErrMiss
	}
	if time.Until(v.ExpiresAt) > MaxTTL {
		return Entry{}, ErrCorrupt
	}
	// json.RawMessage.UnmarshalJSON already allocated an independent slice.
	return Entry{JSON: v.JSON, ExpiresAt: v.ExpiresAt}, nil
}
func (s *Service) check(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosed
	}
	return nil
}
func (s *Service) acquire(ctx context.Context) error { return s.lock(ctx, 1) }
func (s *Service) lock(ctx context.Context, n int64) error {
	if err := s.check(ctx); err != nil {
		return err
	}
	if err := s.gate.Acquire(ctx, n); err != nil {
		return err
	}
	if err := s.check(ctx); err != nil {
		s.gate.Release(n)
		return err
	}
	return nil
}
func (s *Service) release() { s.gate.Release(1) }
func (s *Service) record(err error) {
	if err == nil || errors.Is(err, ErrMiss) {
		s.mu.Lock()
		s.health.LastSuccessfulOperation = time.Now().UTC()
		s.mu.Unlock()
		return
	}
	if errors.Is(err, context.Canceled) {
		return
	}
	s.mu.Lock()
	s.health.Degraded = true
	s.health.LastFailedOperation = time.Now().UTC()
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		s.health.Reason = "cache operation timed out"
	case errors.Is(err, ErrCorrupt):
		s.health.Reason = "cache contains invalid data"
	case errors.Is(err, ErrTooLarge):
		s.health.Reason = "cache value exceeded bounded size"
	case errors.Is(err, ErrQuota):
		s.health.Reason = "cache namespace quota reached"
	default:
		s.health.Reason = "cache operation failed"
	}
	s.mu.Unlock()
}
func (s *Service) Get(ctx context.Context, region, key string) (Entry, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, s.options.SocketTimeout)
	defer cancel()
	if err := s.acquire(ctx); err != nil {
		return Entry{}, false, err
	}
	defer s.release()
	if err := s.check(ctx); err != nil {
		return Entry{}, false, err
	}
	k, err := s.key(region, key)
	if err != nil {
		return Entry{}, false, err
	}
	b, err := s.engine.get(ctx, k)
	if errors.Is(err, ErrMiss) {
		s.record(nil)
		return Entry{}, false, nil
	}
	if err != nil {
		s.record(err)
		return Entry{}, false, err
	}
	entry, err := decodeEntry(b, s.options)
	if errors.Is(err, ErrMiss) {
		s.record(nil)
		return Entry{}, false, nil
	}
	s.record(err)
	if err := ctx.Err(); err != nil {
		return Entry{}, false, err
	}
	return entry, err == nil, err
}
func (s *Service) Set(ctx context.Context, region, key string, e Entry) error {
	_, err := s.set(ctx, region, key, e, nil)
	return err
}
func (s *Service) SetIfGeneration(ctx context.Context, region, key string, e Entry, t Token) (bool, error) {
	return s.set(ctx, region, key, e, &t)
}
func (s *Service) set(ctx context.Context, region, key string, e Entry, t *Token) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, s.options.SocketTimeout)
	defer cancel()
	k, err := s.key(region, key)
	if err != nil {
		return false, err
	}
	if err = s.acquire(ctx); err != nil {
		return false, err
	}
	defer s.release()
	remote := ""
	if t != nil {
		s.mu.Lock()
		valid := t.owner == s && t.epoch == s.epoch
		s.mu.Unlock()
		if !valid {
			return false, nil
		}
		remote = t.remote
	}
	b, err := encodeEntry(e, s.options)
	if err != nil {
		return false, err
	}
	ok, err := s.engine.set(ctx, k, b, e.ExpiresAt, remote)
	s.record(err)
	return ok, err
}
func (s *Service) Generation(ctx context.Context, region string) (Token, error) {
	ctx, cancel := context.WithTimeout(ctx, s.options.SocketTimeout)
	defer cancel()
	if !safePart(region, 48) {
		return Token{}, errors.New("invalid cache region")
	}
	if err := s.acquire(ctx); err != nil {
		return Token{}, err
	}
	defer s.release()
	remote, err := s.engine.generation(ctx)
	s.record(err)
	if err != nil {
		return Token{}, err
	}
	s.mu.Lock()
	t := Token{owner: s, epoch: s.epoch, remote: remote}
	s.mu.Unlock()
	return t, nil
}
func (s *Service) invalidate(region, key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.epoch++
	close(s.flightChanged)
	s.flightChanged = make(chan struct{})
	for k, f := range s.flights {
		if region == "" || strings.HasPrefix(k, region+":") {
			if key == "" || k == region+":"+key {
				f.invalid = true
				delete(s.flights, k)
			}
		}
	}
}
func (s *Service) Delete(ctx context.Context, region, key string) error {
	ctx, cancel := context.WithTimeout(ctx, s.options.SocketTimeout)
	defer cancel()
	k, err := s.key(region, key)
	if err != nil {
		return err
	}
	if err = s.lock(ctx, s.maxOps); err != nil {
		return err
	}
	defer s.gate.Release(s.maxOps)
	s.invalidate(region, key)
	err = s.engine.delete(ctx, k)
	s.record(err)
	return err
}
func (s *Service) Clear(ctx context.Context, region string) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, s.options.SocketTimeout)
	defer cancel()
	if region != "" && !safePart(region, 48) {
		return 0, errors.New("invalid cache region")
	}
	if err := s.lock(ctx, s.maxOps); err != nil {
		return 0, err
	}
	defer s.gate.Release(s.maxOps)
	s.invalidate(region, "")
	n, err := s.engine.clear(ctx, region)
	s.record(err)
	return n, err
}
func (s *Service) Stats(ctx context.Context, region string) (Stats, error) {
	ctx, cancel := context.WithTimeout(ctx, s.options.SocketTimeout)
	defer cancel()
	if err := s.acquire(ctx); err != nil {
		return Stats{}, err
	}
	defer s.release()
	if err := s.check(ctx); err != nil {
		return Stats{}, err
	}
	if region != "" && !safePart(region, 48) {
		return Stats{}, errors.New("invalid cache region")
	}
	v, err := s.engine.stats(ctx, region)
	s.record(err)
	return v, err
}
func (s *Service) List(ctx context.Context, region, after string, limit int) (Page, error) {
	ctx, cancel := context.WithTimeout(ctx, s.options.SocketTimeout)
	defer cancel()
	if err := s.acquire(ctx); err != nil {
		return Page{}, err
	}
	defer s.release()
	if err := s.check(ctx); err != nil {
		return Page{}, err
	}
	if region != "" && !safePart(region, 48) {
		return Page{}, errors.New("invalid cache region")
	}
	if limit < 1 || limit > 100 {
		return Page{}, errors.New("cache list limit must be 1..100")
	}
	if after != "" {
		if len(after) > 256 || !strings.HasPrefix(after, namespacePrefix(s.options.Namespace)) {
			return Page{}, errors.New("invalid cache cursor")
		}
		if _, err := itemFromKey(after, 0, time.Time{}); err != nil {
			return Page{}, errors.New("invalid cache cursor")
		}
	}
	v, err := s.engine.list(ctx, region, after, limit)
	s.record(err)
	return v, err
}
func (s *Service) Health() Health { s.mu.Lock(); defer s.mu.Unlock(); return s.health }
func (s *Service) Close() error {
	if err := s.gate.Acquire(context.Background(), s.maxOps); err != nil {
		return err
	}
	defer s.gate.Release(s.maxOps)
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	close(s.closedCh)
	s.health.Closed = true
	s.epoch++
	close(s.flightChanged)
	s.flightChanged = make(chan struct{})
	for k, f := range s.flights {
		f.invalid = true
		delete(s.flights, k)
	}
	s.mu.Unlock()
	return s.engine.close()
}

// Do coalesces same-key origin loads locally. Cache faults fail open, but origin
// and caller errors are preserved. TTL zero bypasses stored values. At most four
// origins and MaxEntries flight records are admitted; callers wait contextually.
// Origin callbacks must obey their supplied context. Panics release waiters and
// are rethrown in the initiating caller. No background writes are started.
func (s *Service) Do(ctx context.Context, region, key string, ttl time.Duration, load func(context.Context) (json.RawMessage, error)) (json.RawMessage, error) {
	return s.do(ctx, region, key, ttl, nil, load)
}

// DoValidated additionally treats cache values that fail the caller's typed
// schema validation as misses. The same validation is applied to origin JSON
// before publication. Validators must not retain or mutate their input.
func (s *Service) DoValidated(ctx context.Context, region, key string, ttl time.Duration, validate func(json.RawMessage) error, load func(context.Context) (json.RawMessage, error)) (json.RawMessage, error) {
	return s.do(ctx, region, key, ttl, validate, load)
}
func (s *Service) do(ctx context.Context, region, key string, ttl time.Duration, validate func(json.RawMessage) error, load func(context.Context) (json.RawMessage, error)) (json.RawMessage, error) {
	cacheCtx, cacheCancel := context.WithTimeout(ctx, min(ResponseLookupBudget, s.options.SocketTimeout))
	defer cacheCancel()
	if err := s.check(ctx); err != nil {
		return nil, err
	}
	if ttl < 0 || ttl > MaxTTL {
		return nil, errors.New("cache TTL must be 0..7 days")
	}
	if load == nil {
		return nil, errors.New("cache loader is required")
	}
	if _, err := s.key(region, key); err != nil {
		return nil, err
	}
	if ttl > 0 {
		v, hit, _ := s.Get(cacheCtx, region, key)
		if err := s.check(ctx); err != nil {
			return nil, err
		}
		if hit {
			if validate == nil || validate(v.JSON) == nil {
				if err := s.check(ctx); err != nil {
					return nil, err
				}
				return v.JSON, nil
			}
			s.record(ErrCorrupt)
			if err := s.check(ctx); err != nil {
				return nil, err
			}
			s.evictCorrupt(cacheCtx, region, key)
		}
	}
	fk := region + ":" + key
	// Admission happens before allocating a flight, bounding retained key/value
	// records even if arbitrarily many distinct callers are waiting.
	for {
		if err := s.check(ctx); err != nil {
			return nil, err
		}
		s.mu.Lock()
		if f := s.flights[fk]; f != nil {
			s.mu.Unlock()
			select {
			case <-s.closedCh:
				return nil, ErrClosed
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-f.done:
				if err := ctx.Err(); err != nil {
					return nil, err
				}
				return append(json.RawMessage(nil), f.value...), f.err
			}
		}
		if s.activeFlights < s.options.MaxEntries {
			s.activeFlights++
			f := &flight{done: make(chan struct{})}
			s.flights[fk] = f
			s.mu.Unlock()
			return s.run(ctx, cacheCtx, region, key, fk, ttl, validate, load, f)
		}
		wait := s.flightChanged
		s.mu.Unlock()
		select {
		case <-s.closedCh:
			return nil, ErrClosed
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-wait:
		}
	}
}

func (s *Service) run(ctx context.Context, cacheCtx context.Context, region, key, fk string, ttl time.Duration, validate func(json.RawMessage) error, load func(context.Context) (json.RawMessage, error), f *flight) (value json.RawMessage, err error) {
	var panicValue any
	defer func() {
		if r := recover(); r != nil {
			panicValue = r
			err = errors.New("cache loader panicked")
		}
		s.mu.Lock()
		f.value = append(json.RawMessage(nil), value...)
		f.err = err
		s.activeFlights--
		close(s.flightChanged)
		s.flightChanged = make(chan struct{})
		if s.flights[fk] == f {
			delete(s.flights, fk)
		}
		close(f.done)
		s.mu.Unlock()
		if panicValue != nil {
			panic(panicValue)
		}
	}()
	select {
	case <-s.closedCh:
		return nil, ErrClosed
	case s.slots <- struct{}{}:
		defer func() { <-s.slots }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if err = s.check(ctx); err != nil {
		return nil, err
	}
	var token Token
	canPublish := false
	if ttl > 0 {
		token, err = s.Generation(cacheCtx, region)
		canPublish = err == nil
		if e := s.check(ctx); e != nil {
			return nil, e
		}
		if v, hit, _ := s.Get(cacheCtx, region, key); hit {
			if validate == nil || validate(v.JSON) == nil {
				if err := s.check(ctx); err != nil {
					return nil, err
				}
				return v.JSON, nil
			}
			s.record(ErrCorrupt)
			if e := s.check(ctx); e != nil {
				return nil, e
			}
			s.evictCorrupt(cacheCtx, region, key)
			token, err = s.Generation(cacheCtx, region)
			canPublish = err == nil
		}
		if e := s.check(ctx); e != nil {
			return nil, e
		}
	}
	value, err = load(ctx)
	if e := ctx.Err(); e != nil {
		return nil, e
	}
	if err != nil {
		return value, err
	}
	value = append(json.RawMessage(nil), value...)
	if !validJSON(value) {
		return nil, ErrCorrupt
	}
	if validate != nil {
		if e := validate(value); e != nil {
			return nil, e
		}
	}
	s.mu.Lock()
	publish := canPublish && !f.invalid && s.flights[fk] == f
	s.mu.Unlock()
	if publish {
		publishCtx, cancel := context.WithTimeout(ctx, min(ResponsePublishBudget, s.options.SocketTimeout))
		_, _ = s.SetIfGeneration(publishCtx, region, key, Entry{JSON: value, ExpiresAt: time.Now().Add(ttl)}, token)
		cancel()
	}
	if e := ctx.Err(); e != nil {
		return nil, e
	}
	return value, nil
}

// Corrupt eviction preserves the current origin flight. Backend generation is
// still advanced atomically, so any earlier publisher is fenced. This is not a
// user-requested clear; detaching the just-admitted repair flight would duplicate
// origin work and prevent its replacement value from being cached.
func (s *Service) evictCorrupt(ctx context.Context, region, key string) {
	k, err := s.key(region, key)
	if err != nil {
		return
	}
	if err = s.lock(ctx, s.maxOps); err != nil {
		return
	}
	defer s.gate.Release(s.maxOps)
	err = s.engine.delete(ctx, k)
	s.record(err)
}

func cacheError(operation string, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return fmt.Errorf("cache %s failed", operation)
}
