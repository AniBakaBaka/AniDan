// SPDX-License-Identifier: AGPL-3.0-or-later
// Package ratelimit implements independent local resource protection. It neither
// replaces nor bypasses remote paid entitlement checks or upstream signed policy.
package ratelimit

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Rule struct {
	Rate        float64 `json:"requestsPerSecond"`
	Burst       int     `json:"burst"`
	Concurrency int     `json:"concurrency"`
}
type Config struct {
	Enabled         bool            `json:"enabled"`
	Global          Rule            `json:"global"`
	DefaultProvider Rule            `json:"defaultProvider"`
	Providers       map[string]Rule `json:"providers"`
	MaxProviders    int             `json:"maxProviders"`
}

func Defaults() Config {
	return Config{Enabled: true, Global: Rule{32, 64, 16}, DefaultProvider: Rule{8, 16, 4}, Providers: map[string]Rule{}, MaxProviders: 256}
}
func (c Config) Validate() error {
	if c.MaxProviders < 1 || c.MaxProviders > 1024 || len(c.Providers) > c.MaxProviders {
		return errors.New("maxProviders must be 1..1024 and cover all overrides")
	}
	validate := func(r Rule) error {
		if math.IsNaN(r.Rate) || math.IsInf(r.Rate, 0) || r.Rate < 0 || r.Rate > 10000 || r.Burst < 1 || r.Burst > 100000 || r.Concurrency < 1 || r.Concurrency > 256 {
			return errors.New("rule requires rate 0..10000, burst 1..100000, concurrency 1..256")
		}
		return nil
	}
	if err := validate(c.Global); err != nil {
		return fmt.Errorf("global: %w", err)
	}
	if err := validate(c.DefaultProvider); err != nil {
		return fmt.Errorf("default provider: %w", err)
	}
	for name, r := range c.Providers {
		if strings.TrimSpace(name) == "" || len(name) > 128 {
			return errors.New("invalid provider name")
		}
		if err := validate(r); err != nil {
			return fmt.Errorf("provider %s: %w", name, err)
		}
	}
	return nil
}
func copyConfig(c Config) Config {
	out := c
	out.Providers = map[string]Rule{}
	for k, v := range c.Providers {
		out.Providers[k] = v
	}
	return out
}

type bucket struct {
	tokens          float64
	updated         time.Time
	active          int
	requests, waits uint64
	blocked         time.Time
	backoffs        uint
}
type Limiter struct {
	mu        sync.Mutex
	config    Config
	global    bucket
	providers map[string]*bucket
	notify    chan struct{}
	closed    bool
}

var ErrClosed = errors.New("request limiter is closed")
var ErrCapacity = errors.New("request limiter provider capacity reached")

func New(config Config) (*Limiter, error) {
	if err := config.Validate(); err != nil {
		return nil, err
	}
	return &Limiter{config: copyConfig(config), global: bucket{tokens: float64(config.Global.Burst), updated: time.Now()}, providers: map[string]*bucket{}, notify: make(chan struct{})}, nil
}
func (l *Limiter) rule(name string) Rule {
	if r, ok := l.config.Providers[name]; ok {
		return r
	}
	return l.config.DefaultProvider
}
func refill(b *bucket, r Rule, now time.Time) {
	if b.updated.IsZero() {
		b.updated = now
		b.tokens = float64(r.Burst)
		return
	}
	seconds := now.Sub(b.updated).Seconds()
	if seconds > 0 {
		b.tokens = math.Min(float64(r.Burst), b.tokens+seconds*r.Rate)
		b.updated = now
	}
	if r.Rate == 0 {
		b.tokens = float64(r.Burst)
	}
}
func tokenDelay(b *bucket, r Rule) time.Duration {
	if r.Rate == 0 || b.tokens >= 1 {
		return 0
	}
	return time.Duration(math.Ceil((1 - b.tokens) / r.Rate * float64(time.Second)))
}
func (l *Limiter) broadcast() { close(l.notify); l.notify = make(chan struct{}) }
func (l *Limiter) Acquire(ctx context.Context, name string) (func(), error) {
	if ctx == nil {
		return nil, errors.New("context is required")
	}
	if name == "" || len(name) > 128 {
		return nil, errors.New("invalid provider name")
	}
	waitCounted := false
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		l.mu.Lock()
		if l.closed {
			l.mu.Unlock()
			return nil, ErrClosed
		}
		now := time.Now()
		r := l.rule(name)
		b := l.providers[name]
		if b == nil {
			if len(l.providers) >= l.config.MaxProviders {
				l.mu.Unlock()
				return nil, ErrCapacity
			}
			b = &bucket{tokens: float64(r.Burst), updated: now}
			l.providers[name] = b
		}
		refill(&l.global, l.config.Global, now)
		refill(b, r, now)
		cooldown := b.blocked.Sub(now)
		delay := time.Duration(0)
		if cooldown > 0 {
			delay = cooldown
		}
		if l.config.Enabled {
			delay = max(delay, tokenDelay(&l.global, l.config.Global), tokenDelay(b, r))
		}
		if delay <= 0 && l.global.active < l.config.Global.Concurrency && b.active < r.Concurrency {
			if l.config.Enabled {
				if l.config.Global.Rate > 0 {
					l.global.tokens--
				}
				if r.Rate > 0 {
					b.tokens--
				}
			}
			l.global.active++
			b.active++
			l.global.requests++
			b.requests++
			l.mu.Unlock()
			var once sync.Once
			return func() {
				once.Do(func() { l.mu.Lock(); defer l.mu.Unlock(); l.global.active--; b.active--; l.broadcast() })
			}, nil
		}
		if !waitCounted {
			l.global.waits++
			b.waits++
			waitCounted = true
		}
		wake := l.notify
		l.mu.Unlock()
		if delay > 0 {
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				return nil, ctx.Err()
			case <-wake:
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
			case <-timer.C:
			}
		} else {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-wake:
			}
		}
	}
}

// RetryAfter accepts HTTP delta seconds or an HTTP-date, with a 24h safety bound.
func RetryAfter(value string, now time.Time) (time.Duration, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, false
	}
	if n, err := strconv.ParseInt(value, 10, 64); err == nil {
		if n < 0 {
			return 0, false
		}
		n = min(n, int64(86400))
		return time.Duration(n) * time.Second, true
	}
	when, err := http.ParseTime(value)
	if err != nil {
		return 0, false
	}
	return min(max(time.Duration(0), when.Sub(now)), 24*time.Hour), true
}
func (l *Limiter) Feedback(name string, status int, value string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	b := l.providers[name]
	if b == nil || l.closed {
		return
	}
	if status != 429 && (status != 503 || value == "") {
		if status >= 200 && status < 400 {
			b.backoffs = 0
		}
		return
	}
	delay, ok := RetryAfter(value, time.Now())
	if !ok {
		b.backoffs = min(b.backoffs+1, 6)
		delay = time.Second * time.Duration(1<<min(b.backoffs-1, 5))
	}
	until := time.Now().Add(delay)
	if until.After(b.blocked) {
		b.blocked = until
	}
	l.broadcast()
}
func (l *Limiter) Configure(config Config) error {
	if err := config.Validate(); err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return ErrClosed
	}
	if len(l.providers) > config.MaxProviders {
		return errors.New("new provider cap is below currently allocated state")
	}
	now := time.Now()
	refill(&l.global, l.config.Global, now)
	for name, b := range l.providers {
		refill(b, l.rule(name), now)
	}
	l.config = copyConfig(config)
	l.global.tokens = math.Min(l.global.tokens, float64(config.Global.Burst))
	for name, b := range l.providers {
		b.tokens = math.Min(b.tokens, float64(l.rule(name).Burst))
	}
	l.broadcast()
	return nil
}
func (l *Limiter) Policy() Config { l.mu.Lock(); defer l.mu.Unlock(); return copyConfig(l.config) }

type BucketStatus struct {
	Name              string     `json:"providerName,omitempty"`
	RequestsPerSecond float64    `json:"requestsPerSecond"`
	Burst             int        `json:"burst"`
	Concurrency       int        `json:"concurrency"`
	Active            int        `json:"active"`
	Requests          uint64     `json:"requestCount"`
	Waits             uint64     `json:"throttledWaits"`
	AvailableTokens   float64    `json:"availableTokens"`
	RetryAfterSeconds float64    `json:"retryAfterSeconds"`
	BlockedUntil      *time.Time `json:"blockedUntil,omitempty"`
}
type Status struct {
	Enabled                bool           `json:"enabled"`
	Closed                 bool           `json:"closed"`
	Policy                 string         `json:"policy"`
	Global                 BucketStatus   `json:"global"`
	Providers              []BucketStatus `json:"providers"`
	ConcurrencyEnforced    bool           `json:"concurrencyEnforced"`
	RemoteCooldownEnforced bool           `json:"remoteCooldownEnforced"`
}

func statusOf(name string, b *bucket, r Rule, now time.Time) BucketStatus {
	refill(b, r, now)
	out := BucketStatus{Name: name, RequestsPerSecond: r.Rate, Burst: r.Burst, Concurrency: r.Concurrency, Active: b.active, Requests: b.requests, Waits: b.waits, AvailableTokens: math.Round(b.tokens*100) / 100}
	if b.blocked.After(now) {
		value := b.blocked
		out.BlockedUntil = &value
		out.RetryAfterSeconds = math.Ceil(b.blocked.Sub(now).Seconds()*10) / 10
	}
	return out
}
func (l *Limiter) Status() Status {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	out := Status{Enabled: l.config.Enabled, Closed: l.closed, Policy: "anidan-local-v1", Global: statusOf("", &l.global, l.config.Global, now), Providers: []BucketStatus{}, ConcurrencyEnforced: true, RemoteCooldownEnforced: true}
	for name, b := range l.providers {
		out.Providers = append(out.Providers, statusOf(name, b, l.rule(name), now))
	}
	sort.Slice(out.Providers, func(i, j int) bool { return out.Providers[i].Name < out.Providers[j].Name })
	return out
}
func (l *Limiter) Close() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.closed {
		l.closed = true
		l.broadcast()
	}
}
