// SPDX-License-Identifier: AGPL-3.0-only
package provider

import (
	"context"
	"errors"
	"github.com/AniBakaBaka/AniDan/internal/danmaku"
	"sync"
	"sync/atomic"
	"time"
)

type segmentResult struct {
	index    int
	comments []danmaku.Comment
	err      error
}

// segments bounds fan-out, preserves segment order, enforces aggregate limits,
// cancels sibling requests on the first failure and never returns partial success.
func (h *HTTPProvider) segments(ctx context.Context, n int, fetch func(context.Context, int) ([]danmaku.Comment, error)) ([]danmaku.Comment, error) {
	if n < 0 || n > h.maxSegments() {
		return nil, danmaku.ErrLimit
	}
	if n == 0 {
		return []danmaku.Comment{}, nil
	}
	workers := h.SegmentWorkers
	if workers < 1 {
		workers = 4
	}
	workers = min(workers, 4, n)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	results := make(chan segmentResult, workers)
	var next atomic.Int64
	var wg sync.WaitGroup
	for range workers {
		wg.Go(func() {
			for {
				index := int(next.Add(1) - 1)
				if index >= n {
					return
				}
				if ctx.Err() != nil {
					return
				}
				comments, e := fetch(ctx, index)
				select {
				case results <- segmentResult{index, comments, e}:
				case <-ctx.Done():
					return
				}
				if e != nil {
					return
				}
			}
		})
	}
	go func() { wg.Wait(); close(results) }()
	parts := make([][]danmaku.Comment, n)
	count, received := 0, 0
	totalBytes := int64(0)
	byteLimit := h.MaxDownloadBytes
	if byteLimit <= 0 {
		byteLimit = 64 << 20
	}
	limit := h.MaxComments
	if limit < 1 {
		limit = 500000
	}
	var first error
	for r := range results {
		received++
		if r.err != nil {
			if first == nil {
				first = r.err
				cancel()
			}
			continue
		}
		if len(r.comments) > limit-count {
			if first == nil {
				first = danmaku.ErrLimit
				cancel()
			}
			continue
		}
		added := int64(0)
		for _, c := range r.comments {
			added += int64(len(c.P) + len(c.M) + 64)
		}
		if added > byteLimit-totalBytes {
			if first == nil {
				first = danmaku.ErrLimit
				cancel()
			}
			continue
		}
		totalBytes += added
		count += len(r.comments)
		parts[r.index] = r.comments
	}
	if first != nil {
		return nil, first
	}
	if e := ctx.Err(); e != nil {
		return nil, e
	}
	if received != n {
		return nil, errors.New("incomplete segment download")
	}
	out := make([]danmaku.Comment, 0, count)
	for _, part := range parts {
		out = append(out, part...)
	}
	return out, nil
}

type commentCall struct {
	done   chan struct{}
	cancel context.CancelFunc
	refs   int
	result []danmaku.Comment
	err    error
}
type commentFlights struct {
	mu    sync.Mutex
	calls map[string]*commentCall
	life  *flightLifetime
}

func newCommentFlights() *commentFlights {
	return &commentFlights{calls: map[string]*commentCall{}, life: newFlightLifetime()}
}

// coalesce shares a first download with concurrent callers of the same episode.
// One caller cancelling does not cancel other subscribers. The underlying work
// is cancelled when its last subscriber leaves; completed results are not cached
// here (the server's parsed/output cache owns warm reuse).
func (h *HTTPProvider) coalesce(ctx context.Context, key string, fetch func(context.Context) ([]danmaku.Comment, error)) ([]danmaku.Comment, error) {
	if e := ctx.Err(); e != nil {
		return nil, e
	}
	if len(key) > 512 {
		return nil, danmaku.ErrLimit
	}
	f := h.flights
	if f == nil {
		return fetch(ctx)
	}
	f.mu.Lock()
	if f.life.isClosed() {
		f.mu.Unlock()
		return nil, ErrRegistryClosed
	}
	call, exists := f.calls[key]
	if exists {
		call.refs++
	} else {
		if len(f.calls) >= 128 {
			f.mu.Unlock()
			return nil, danmaku.ErrLimit
		}
		work, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		call = &commentCall{done: make(chan struct{}), cancel: cancel, refs: 1}
		if !f.life.begin(call) {
			cancel()
			f.mu.Unlock()
			return nil, ErrRegistryClosed
		}
		f.calls[key] = call
		go func() {
			defer f.life.end(call)
			call.result, call.err = fetch(work)
			cancel()
			f.mu.Lock()
			if f.calls[key] == call {
				delete(f.calls, key)
			}
			close(call.done)
			f.mu.Unlock()
		}()
	}
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		call.refs--
		if call.refs == 0 {
			if f.calls[key] == call {
				delete(f.calls, key)
			}
			call.cancel()
		}
		f.mu.Unlock()
	}()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-call.done:
		if call.err != nil {
			return nil, call.err
		}
		return append([]danmaku.Comment(nil), call.result...), nil
	}
}
