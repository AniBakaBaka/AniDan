// SPDX-License-Identifier: AGPL-3.0-only
// Package boundedcache stores immutable values under an explicit byte budget.
package boundedcache

import (
	"container/list"
	"context"
	"errors"
	"sync"
	"time"
)

type entry[T any] struct {
	key     string
	value   T
	bytes   int64
	expires time.Time
}
type flight[T any] struct {
	invalidated bool
	done        chan struct{}
	value       T
	err         error
}
type Cache[T any] struct {
	mu         sync.Mutex
	max, used  int64
	generation uint64
	ttl        time.Duration
	items      map[string]*list.Element
	lru        *list.List
	flights    map[string]*flight[T]
	slots      chan struct{}
}

func New[T any](max int64, ttl time.Duration) *Cache[T] {
	return &Cache[T]{max: max, ttl: ttl, items: map[string]*list.Element{}, lru: list.New(), flights: map[string]*flight[T]{}, slots: make(chan struct{}, 2)}
}
func (c *Cache[T]) Do(ctx context.Context, key string, load func() (T, int64, error)) (T, error) {
	return c.DoTTL(ctx, key, c.ttl, load)
}

// DoTTL changes only this entry lifetime. A non-positive TTL bypasses stored
// entries while still coalescing in-flight requests under the same key.
func (c *Cache[T]) DoTTL(ctx context.Context, key string, ttl time.Duration, load func() (T, int64, error)) (T, error) {
	if err := ctx.Err(); err != nil {
		var zero T
		return zero, err
	}
	c.mu.Lock()
	if e := c.items[key]; e != nil {
		v := e.Value.(entry[T])
		if ttl > 0 && time.Now().Before(v.expires) {
			c.lru.MoveToFront(e)
			c.mu.Unlock()
			return v.value, nil
		}
		c.drop(e)
	}
	if f := c.flights[key]; f != nil {
		c.mu.Unlock()
		select {
		case <-ctx.Done():
			var zero T
			return zero, ctx.Err()
		case <-f.done:
			return f.value, f.err
		}
	}
	generation := c.generation
	f := &flight[T]{done: make(chan struct{})}
	c.flights[key] = f
	c.mu.Unlock()
	var value T
	var size int64
	var err error
	var panicValue any
	select {
	case c.slots <- struct{}{}:
		func() {
			defer func() {
				panicValue = recover()
				if panicValue != nil {
					err = errors.New("cache loader panicked")
				}
			}()
			value, size, err = load()
		}()
		<-c.slots
	case <-ctx.Done():
		err = ctx.Err()
	}

	c.mu.Lock()
	f.value, f.err = value, err
	current := c.flights[key]
	if current == f {
		delete(c.flights, key)
	}
	if current == f && !f.invalidated && generation == c.generation && ttl > 0 && err == nil && size > 0 && size <= c.max {
		for c.used+size > c.max {
			c.drop(c.lru.Back())
		}
		e := c.lru.PushFront(entry[T]{key: key, value: value, bytes: size, expires: time.Now().Add(ttl)})
		c.items[key] = e
		c.used += size
	}
	close(f.done)
	c.mu.Unlock()
	if panicValue != nil {
		panic(panicValue)
	}
	return value, err
}
func (c *Cache[T]) drop(e *list.Element) {
	if e == nil {
		return
	}
	v := e.Value.(entry[T])
	delete(c.items, v.key)
	c.used -= v.bytes
	c.lru.Remove(e)
}
func (c *Cache[T]) Clear() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.generation++
	for _, f := range c.flights {
		f.invalidated = true
	}
	c.flights = map[string]*flight[T]{}
	c.items = map[string]*list.Element{}
	c.lru.Init()
	c.used = 0
}
func (c *Cache[T]) Stats() (int, int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.items), c.used
}

// Invalidate removes only matching entries and detaches matching in-flight loads.
// Requests already joined to an old load may receive its old snapshot, but later
// requests cannot join it and it cannot republish after invalidation. The match
// function runs under the cache mutex and must not call back into this cache.
func (c *Cache[T]) Invalidate(match func(string) bool) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	removed := 0
	for key, element := range c.items {
		if match(key) {
			c.drop(element)
			removed++
		}
	}
	for key, f := range c.flights {
		if match(key) {
			f.invalidated = true
			delete(c.flights, key)
		}
	}
	return removed
}
