// SPDX-License-Identifier: AGPL-3.0-only
package cachebackend

import (
	"context"
	"errors"
	"strings"
	"time"
)

// SQLProvider and SQLMetaProvider reserve native disposable cache ownership.
// Legacy management/expiry paths must exclude BOTH markers, globally, so they
// cannot bypass fencing or alter another deployment's cache metadata.
const SQLProvider = sqlProvider
const SQLMetaProvider = sqlMetaProvider

func OwnsSQLRecord(key, provider string) bool {
	if len(key) > 256 {
		return false
	}
	if provider != SQLProvider && provider != SQLMetaProvider {
		return false
	}
	parts := strings.Split(key, ":")
	return len(parts) >= 5 && parts[0] == "anidan" && parts[1] == "cache" && parts[2] == "v1" && safePart(parts[3], 40)
}
func (s *Service) physicalKey(key string) (Item, error) {
	if !strings.HasPrefix(key, namespacePrefix(s.options.Namespace)) {
		return Item{}, errors.New("cache item is outside the configured namespace")
	}
	item, err := itemFromKey(key, 0, time.Time{})
	if err != nil {
		return Item{}, errors.New("invalid cache item key")
	}
	return item, nil
}

// GetItem reads a physical key previously returned by List. Values are bounded
// and cloned exactly as Get; raw request keys never need to be persisted.
func (s *Service) GetItem(ctx context.Context, key string) (Entry, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, s.options.SocketTimeout)
	defer cancel()
	if _, err := s.physicalKey(key); err != nil {
		return Entry{}, false, err
	}
	if err := s.acquire(ctx); err != nil {
		return Entry{}, false, err
	}
	defer s.release()
	b, err := s.engine.get(ctx, key)
	if errors.Is(err, ErrMiss) {
		s.record(nil)
		return Entry{}, false, nil
	}
	if err != nil {
		s.record(err)
		return Entry{}, false, err
	}
	value, err := decodeEntry(b, s.options)
	if errors.Is(err, ErrMiss) {
		s.record(nil)
		return Entry{}, false, nil
	}
	s.record(err)
	if e := ctx.Err(); e != nil {
		return Entry{}, false, e
	}
	return value, err == nil, err
}

// DeleteItem fences all publishers because a hash cannot be reversed to detach
// one original-key flight. This conservative invalidation never publishes stale
// data, and does not persist potentially sensitive unhashed queries.
func (s *Service) DeleteItem(ctx context.Context, key string) error {
	ctx, cancel := context.WithTimeout(ctx, s.options.SocketTimeout)
	defer cancel()
	if _, err := s.physicalKey(key); err != nil {
		return err
	}
	if err := s.lock(ctx, s.maxOps); err != nil {
		return err
	}
	defer s.gate.Release(s.maxOps)
	s.invalidate("", "")
	err := s.engine.delete(ctx, key)
	s.record(err)
	return err
}
