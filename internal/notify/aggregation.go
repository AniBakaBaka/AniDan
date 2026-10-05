// SPDX-License-Identifier: AGPL-3.0-only
package notify

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

const AggregateMaxCount = 10
const AggregateMaxBuckets = 128
const AggregateMaxBytes = 4 << 20
const AggregateMaxTargets = 1000
const aggregatePendingTTL = 10 * time.Minute

var ErrAggregationClosed = errors.New("notification aggregation is closed")

type AggregationConfig struct {
	Enabled   bool          `json:"enabled"`
	Window    time.Duration `json:"-"`
	Threshold int           `json:"threshold"`
}

func DefaultAggregationConfig() AggregationConfig {
	return AggregationConfig{true, 30 * time.Second, 5}
}
func (c AggregationConfig) Validate() error {
	if c.Window < time.Second || c.Window > time.Hour || c.Threshold < 1 || c.Threshold > 1000 {
		return errors.New("notification window must be 1..3600 seconds and threshold 1..1000")
	}
	return nil
}

// Targets snapshot only channel identity/recipient fingerprints, never credentials.
// Delivery must still check enabled/subscription state and the current fingerprint.
type DeliveryTarget struct {
	ID          int64  `json:"id"`
	Fingerprint string `json:"fingerprint"`
}
type AggregateNotice struct {
	ID          string
	Group       string
	Event       Event
	Targets     []DeliveryTarget
	ForceWindow bool
}
type DeliveryBatch struct {
	ID        string           `json:"id"`
	Event     Event            `json:"event"`
	Targets   []DeliveryTarget `json:"targets"`
	Count     int              `json:"count"`
	CreatedAt time.Time        `json:"createdAt"`
	memberIDs []string
	size      int
	created   time.Time
}
type aggregateBucket struct {
	key     string
	created time.Time
	notices []AggregateNotice
	members []string
	size    int
}
type aggregateRecent struct {
	times []int64
	used  time.Time
}
type AggregationStats struct {
	Buckets       int    `json:"buckets"`
	Pending       int    `json:"pending"`
	BufferedBytes int    `json:"bufferedBytes"`
	RecentScopes  int    `json:"recentScopes"`
	Accepted      uint64 `json:"accepted"`
	Immediate     uint64 `json:"immediate"`
	Buffered      uint64 `json:"buffered"`
	Suppressed    uint64 `json:"suppressed"`
	Enqueued      uint64 `json:"enqueued"`
	Expired       uint64 `json:"expired"`
	Rejected      uint64 `json:"rejected"`
	Closed        bool   `json:"closed"`
}

// Aggregator contains no network/SQL work and starts no goroutine. Outputs stay
// bounded and pending until durable queue admission is acknowledged. Retrying
// admission is safe only when the caller uses each stable batch ID as a receipt.
// It never retries a remote send. Memory is intentionally not crash-durable.
type Aggregator struct {
	mu      sync.Mutex
	config  AggregationConfig
	buckets map[string]*aggregateBucket
	pending map[string]DeliveryBatch
	held    map[string]bool
	recent  map[string]aggregateRecent
	bytes   int
	stats   AggregationStats
	closed  bool
}

func NewAggregator(config AggregationConfig) (*Aggregator, error) {
	if err := config.Validate(); err != nil {
		return nil, err
	}
	return &Aggregator{config: config, buckets: map[string]*aggregateBucket{}, pending: map[string]DeliveryBatch{}, held: map[string]bool{}, recent: map[string]aggregateRecent{}}, nil
}
func (a *Aggregator) Configure(c AggregationConfig) error {
	if err := c.Validate(); err != nil {
		return err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.config = c
	return nil
}
func (a *Aggregator) Config() AggregationConfig { a.mu.Lock(); defer a.mu.Unlock(); return a.config }
func aggregationHash(parts ...string) string {
	h := sha256.New()
	for _, part := range parts {
		h.Write([]byte(part))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}
func aggregationTargets(in []DeliveryTarget) ([]DeliveryTarget, string, error) {
	if len(in) < 1 || len(in) > AggregateMaxTargets {
		return nil, "", ErrLimit
	}
	out := append([]DeliveryTarget(nil), in...)
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	parts := []string{}
	for i, target := range out {
		if target.ID < 1 || len(target.Fingerprint) != 64 {
			return nil, "", ErrLimit
		}
		if _, err := hex.DecodeString(target.Fingerprint); err != nil {
			return nil, "", ErrLimit
		}
		if i > 0 && out[i-1].ID == target.ID {
			return nil, "", errors.New("duplicate notification target")
		}
		parts = append(parts, fmt.Sprint(target.ID), target.Fingerprint)
	}
	return out, aggregationHash(parts...), nil
}
func aggregationEligible(kind string) bool {
	switch kind {
	case "import_success", "webhook_import_success", "auto_import_success", "refresh_success", "incremental_refresh_success":
		return true
	}
	return false
}
func validateAggregateNotice(n AggregateNotice) error {
	if n.ID == "" || len(n.ID) > 512 || len(n.Group) > 512 || n.Event.Type == "" || len(n.Event.Type) > 80 || len(n.Event.Title)+len(n.Event.Text) > 16000 || len(n.Event.ImageURL) > 2048 {
		return ErrLimit
	}
	for _, r := range n.Event.Type {
		if r != '_' && (r < 'a' || r > 'z') {
			return errors.New("invalid notification event type")
		}
	}
	return nil
}
func aggregateNoticeSize(n AggregateNotice) int {
	b, _ := json.Marshal(struct {
		Event   Event
		Targets []DeliveryTarget
	}{n.Event, n.Targets})
	return len(b) + len(n.ID) + len(n.Group) + 512 + 64*len(n.Targets)
}
func (a *Aggregator) Collect(now time.Time, n AggregateNotice) error {
	if err := validateAggregateNotice(n); err != nil {
		return err
	}
	targets, audience, err := aggregationTargets(n.Targets)
	if err != nil {
		return err
	}
	n.Targets = targets
	size := aggregateNoticeSize(n)
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed {
		return ErrAggregationClosed
	}
	a.expireLocked(now)
	member := aggregationHash(audience, n.ID, n.Event.Type)
	if a.held[member] {
		a.stats.Suppressed++
		return nil
	}
	if size > AggregateMaxBytes || a.bytes+size > AggregateMaxBytes {
		a.stats.Rejected++
		return ErrLimit
	}
	scope := audience + ":" + n.Event.Type
	buffer := n.ForceWindow
	if !buffer && a.config.Enabled && aggregationEligible(n.Event.Type) {
		recent, ok := a.recent[scope]
		if !ok && len(a.recent) >= AggregateMaxBuckets {
			a.stats.Rejected++
			return ErrLimit
		}
		cutoff := now.Add(-a.config.Window).UnixNano()
		keep := recent.times[:0]
		for _, v := range recent.times {
			if v >= cutoff {
				keep = append(keep, v)
			}
		}
		recent.times = keep
		recent.used = now
		if len(recent.times) >= a.config.Threshold {
			buffer = true
		} else {
			recent.times = append(recent.times, now.UnixNano())
		}
		a.recent[scope] = recent
	}
	if !buffer {
		if len(a.pending)+len(a.buckets) >= AggregateMaxBuckets {
			a.stats.Rejected++
			return ErrLimit
		}
		batch := DeliveryBatch{ID: aggregationHash("notice", member), Event: n.Event, Targets: n.Targets, Count: 1, memberIDs: []string{member}, size: size, created: now, CreatedAt: now}
		a.pending[batch.ID] = batch
		a.held[member] = true
		a.bytes += size
		a.stats.Accepted++
		a.stats.Immediate++
		return nil
	}
	group := n.Group
	if !n.ForceWindow {
		group = "surge"
	}
	key := scope + ":" + group
	bucket := a.buckets[key]
	if bucket == nil {
		if len(a.buckets) >= AggregateMaxBuckets || len(a.buckets)+len(a.pending) >= AggregateMaxBuckets {
			a.stats.Rejected++
			return ErrLimit
		}
		bucket = &aggregateBucket{key: key, created: now}
		a.buckets[key] = bucket
	}
	bucket.notices = append(bucket.notices, n)
	bucket.members = append(bucket.members, member)
	bucket.size += size
	a.held[member] = true
	a.bytes += size
	a.stats.Accepted++
	a.stats.Buffered++
	if len(bucket.notices) >= AggregateMaxCount {
		a.flushLocked(key, now)
	}
	return nil
}
func aggregateClip(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

// A common byte cap keeps summaries within WeCom's smaller text envelope too.
func aggregateByteClip(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	cut := 0
	for i := range s {
		if i > limit-40 {
			break
		}
		cut = i
	}
	return s[:cut] + "\n…详见任务列表"
}
func (a *Aggregator) flushLocked(key string, now time.Time) {
	b := a.buckets[key]
	if b == nil || len(b.notices) == 0 {
		return
	}
	if len(a.pending) >= AggregateMaxBuckets {
		return
	}
	first := b.notices[0]
	event := first.Event
	if len(b.notices) > 1 {
		lines := make([]string, 0, len(b.notices))
		for _, n := range b.notices {
			lines = append(lines, "• "+aggregateClip(n.Event.Title, 100)+": "+aggregateClip(n.Event.Text, 250))
		}
		event = Event{Type: first.Event.Type, Title: fmt.Sprintf("通知汇总 · %d项", len(b.notices)), Text: aggregateByteClip(strings.Join(lines, "\n"), 1900)}
	}
	ids := append([]string(nil), b.members...)
	sort.Strings(ids)
	id := aggregationHash(append([]string{"batch", key}, ids...)...)
	batch := DeliveryBatch{ID: id, Event: event, Targets: append([]DeliveryTarget(nil), first.Targets...), Count: len(b.notices), memberIDs: append([]string(nil), b.members...), size: b.size, created: now, CreatedAt: now}
	a.pending[id] = batch
	delete(a.buckets, key)
}
func (a *Aggregator) expireLocked(now time.Time) {
	for id, b := range a.pending {
		if !now.Before(b.created.Add(aggregatePendingTTL)) {
			for _, member := range b.memberIDs {
				delete(a.held, member)
			}
			a.bytes -= b.size
			delete(a.pending, id)
			a.stats.Expired += uint64(b.Count)
		}
	}
	for key, b := range a.buckets {
		if !now.Before(b.created.Add(a.config.Window)) {
			a.flushLocked(key, now)
		}
	}
	for key, recent := range a.recent {
		if now.Sub(recent.used) > a.config.Window {
			delete(a.recent, key)
		}
	}
}
func (a *Aggregator) Pending(now time.Time, limit int) []DeliveryBatch {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.expireLocked(now)
	if limit < 1 || limit > AggregateMaxBuckets {
		limit = AggregateMaxBuckets
	}
	out := make([]DeliveryBatch, 0, min(limit, len(a.pending)))
	ids := make([]string, 0, len(a.pending))
	for id := range a.pending {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		x, y := a.pending[ids[i]], a.pending[ids[j]]
		if x.created.Equal(y.created) {
			return ids[i] < ids[j]
		}
		return x.created.Before(y.created)
	})
	for _, id := range ids[:min(limit, len(ids))] {
		b := a.pending[id]
		b.Targets = append([]DeliveryTarget(nil), b.Targets...)
		b.memberIDs = nil
		out = append(out, b)
	}
	return out
}
func (a *Aggregator) Ack(id string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if b, ok := a.pending[id]; ok {
		delete(a.pending, id)
		for _, member := range b.memberIDs {
			delete(a.held, member)
		}
		a.bytes -= b.size
		a.stats.Enqueued += uint64(b.Count)
	}
}
func (a *Aggregator) Close(now time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.closed = true
	for key := range a.buckets {
		a.flushLocked(key, now)
	}
}
func (a *Aggregator) Stats() AggregationStats {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := a.stats
	out.Buckets = len(a.buckets)
	out.Pending = len(a.pending)
	out.BufferedBytes = a.bytes
	out.RecentScopes = len(a.recent)
	out.Closed = a.closed
	return out
}
