// SPDX-License-Identifier: AGPL-3.0-only
package cachebackend

import (
	"container/list"
	"context"
	"sort"
	"strings"
	"sync"
	"time"
)

type memoryEntry struct {
	key     string
	data    []byte
	expires time.Time
	size    int64
}
type memoryBackend struct {
	mu      sync.Mutex
	options Options
	items   map[string]*list.Element
	lru     *list.List
	used    int64
	epoch   string
	closed  bool
}

func newMemory(o Options) *memoryBackend {
	return &memoryBackend{options: o, items: make(map[string]*list.Element), lru: list.New(), epoch: nonce()}
}
func (m *memoryBackend) check(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if m.closed {
		return ErrClosed
	}
	return nil
}
func (m *memoryBackend) drop(e *list.Element) {
	v := e.Value.(memoryEntry)
	delete(m.items, v.key)
	m.used -= v.size
	m.lru.Remove(e)
}
func (m *memoryBackend) purge() {
	now := time.Now()
	for _, e := range m.items {
		if !e.Value.(memoryEntry).expires.After(now) {
			m.drop(e)
		}
	}
}
func (m *memoryBackend) get(ctx context.Context, key string) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if e := m.check(ctx); e != nil {
		return nil, e
	}
	v := m.items[key]
	if v == nil {
		return nil, ErrMiss
	}
	e := v.Value.(memoryEntry)
	if !e.expires.After(time.Now()) {
		m.drop(v)
		return nil, ErrMiss
	}
	m.lru.MoveToFront(v)
	// Internal only: encoded envelopes are immutable. Service decoding allocates
	// the exported RawMessage; replacement/eviction never mutate this byte slice.
	return e.data, nil
}
func (m *memoryBackend) set(ctx context.Context, key string, b []byte, expires time.Time, expected string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if e := m.check(ctx); e != nil {
		return false, e
	}
	if expected != "" && expected != m.epoch {
		return false, nil
	}
	n := int64(len(key) + len(b))
	if len(b) > maxEnvelope(m.options) {
		return false, ErrTooLarge
	}
	if n > m.options.MaxBytes {
		return false, ErrQuota
	}
	if !expires.After(time.Now()) {
		return false, ErrMiss
	}
	m.purge()
	if e := m.items[key]; e != nil {
		m.drop(e)
	}
	for len(m.items) >= m.options.MaxEntries || m.used+n > m.options.MaxBytes {
		m.drop(m.lru.Back())
	}
	e := m.lru.PushFront(memoryEntry{key: key, data: append([]byte(nil), b...), expires: expires, size: n})
	m.items[key] = e
	m.used += n
	return true, nil
}
func (m *memoryBackend) delete(ctx context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if e := m.check(ctx); e != nil {
		return e
	}
	m.epoch = nonce()
	if e := m.items[key]; e != nil {
		m.drop(e)
	}
	return nil
}
func (m *memoryBackend) matches(key, region string) bool {
	return region == "" || strings.HasPrefix(key, namespacePrefix(m.options.Namespace)+region+":")
}
func (m *memoryBackend) clear(ctx context.Context, region string) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if e := m.check(ctx); e != nil {
		return 0, e
	}
	m.epoch = nonce()
	m.purge()
	n := 0
	for key, e := range m.items {
		if m.matches(key, region) {
			m.drop(e)
			n++
		}
	}
	return n, nil
}
func (m *memoryBackend) generation(ctx context.Context) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if e := m.check(ctx); e != nil {
		return "", e
	}
	return m.epoch, nil
}
func (m *memoryBackend) list(ctx context.Context, region, after string, limit int) (Page, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if e := m.check(ctx); e != nil {
		return Page{}, e
	}
	m.purge()
	keys := make([]string, 0, len(m.items))
	for k := range m.items {
		if k > after && m.matches(k, region) {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	p := Page{Items: []Item{}}
	for i, k := range keys {
		if i == limit {
			p.Next = keys[i-1]
			break
		}
		e := m.items[k].Value.(memoryEntry)
		it, err := itemFromKey(k, e.size, e.expires)
		if err != nil {
			return Page{}, err
		}
		p.Items = append(p.Items, it)
	}
	return p, nil
}
func (m *memoryBackend) stats(ctx context.Context, region string) (Stats, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if e := m.check(ctx); e != nil {
		return Stats{}, e
	}
	m.purge()
	var s Stats
	for k, e := range m.items {
		if m.matches(k, region) {
			s.Entries++
			s.Bytes += e.Value.(memoryEntry).size
		}
	}
	return s, nil
}
func (m *memoryBackend) close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closed = true
	m.items = make(map[string]*list.Element)
	m.lru.Init()
	m.used = 0
	return nil
}

type hybridBackend struct {
	memory   *memoryBackend
	database backend
	ttl      time.Duration
}

func (h *hybridBackend) get(ctx context.Context, key string) ([]byte, error) {
	if b, e := h.memory.get(ctx, key); e == nil {
		return b, nil
	}
	b, e := h.database.get(ctx, key)
	if e != nil {
		return nil, e
	}
	entry, e := decodeEntry(b, h.memory.options)
	if e != nil {
		return nil, e
	}
	expires := entry.ExpiresAt
	if cap := time.Now().Add(h.ttl); cap.Before(expires) {
		expires = cap
	}
	_, _ = h.memory.set(ctx, key, b, expires, "")
	return b, nil
}
func (h *hybridBackend) set(ctx context.Context, key string, b []byte, expires time.Time, expected string) (bool, error) {
	ok, e := h.database.set(ctx, key, b, expires, expected)
	if e != nil || !ok {
		return ok, e
	}
	if cap := time.Now().Add(h.ttl); cap.Before(expires) {
		expires = cap
	}
	_, _ = h.memory.set(ctx, key, b, expires, "")
	return true, nil
}
func (h *hybridBackend) delete(ctx context.Context, key string) error {
	_ = h.memory.delete(ctx, key)
	return h.database.delete(ctx, key)
}
func (h *hybridBackend) clear(ctx context.Context, region string) (int, error) {
	_, _ = h.memory.clear(ctx, region)
	return h.database.clear(ctx, region)
}
func (h *hybridBackend) generation(ctx context.Context) (string, error) {
	return h.database.generation(ctx)
}
func (h *hybridBackend) list(ctx context.Context, region, after string, limit int) (Page, error) {
	return h.database.list(ctx, region, after, limit)
}
func (h *hybridBackend) stats(ctx context.Context, region string) (Stats, error) {
	return h.database.stats(ctx, region)
}
func (h *hybridBackend) close() error { _ = h.memory.close(); return h.database.close() }
