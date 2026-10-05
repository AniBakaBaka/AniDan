// SPDX-License-Identifier: AGPL-3.0-only
package provider

import (
	"context"
	"errors"
	"fmt"
	"github.com/AniBakaBaka/AniDan/internal/danmaku"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

type Result struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	Type     string `json:"type"`
	ImageURL string `json:"imageUrl"`
	Year     int    `json:"year"`
	Season   int    `json:"season"`
}
type Episode struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	URL   string `json:"url"`
	Index int    `json:"index"`
}
type Provider interface {
	Name() string
	Search(context.Context, string) ([]Result, error)
	Episodes(context.Context, string) ([]Episode, error)
	Comments(context.Context, string) ([]danmaku.Comment, error)
}
type SearchResult struct {
	Result
	Provider string `json:"provider"`
}
type Capability struct {
	Name     string `json:"name"`
	Search   bool   `json:"search"`
	Episodes bool   `json:"episodes"`
	Comments bool   `json:"comments"`
	Status   string `json:"status"`
	Blocker  string `json:"blocker,omitempty"`
}
type UnsupportedError struct{ Provider, Operation, Reason string }

func (e *UnsupportedError) Error() string {
	return fmt.Sprintf("provider %s: %s unavailable: %s", e.Provider, e.Operation, e.Reason)
}

type SourceError struct {
	Provider string
	Err      error
}

func (e SourceError) Error() string { return e.Provider + ": " + e.Err.Error() }
func (e SourceError) Unwrap() error { return e.Err }

type Registry struct {
	drain          *registryDrain
	revision       uint64
	mu             sync.RWMutex
	providers      map[string]Provider
	Workers        int
	Timeout        time.Duration
	searchObserver func(context.Context, string, time.Duration, int, error)
}

func NewRegistry(workers int, ps ...Provider) *Registry {
	if workers < 1 {
		workers = 4
	}
	if workers > 32 {
		workers = 32
	}
	r := &Registry{drain: newRegistryDrain(), providers: map[string]Provider{}, Workers: workers, Timeout: 30 * time.Second}
	for _, p := range ps {
		r.Register(p)
	}
	return r
}
func (r *Registry) Register(p Provider) {
	if p == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.drain == nil {
		r.drain = newRegistryDrain()
	}
	if !r.drain.track(p) {
		return
	}
	if r.providers == nil {
		r.providers = map[string]Provider{}
	}
	r.providers[strings.ToLower(p.Name())] = p
	r.revision++
}
func (r *Registry) Get(name string) (Provider, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.drain != nil && r.drain.isClosed() {
		return nil, false
	}
	p, ok := r.providers[strings.ToLower(name)]
	return p, ok
}
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.providers))
	for n := range r.providers {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// Search returns ordered partial results plus joined source errors; callers must
// not present partial data as complete success. Worker count bounds fan-out.
func (r *Registry) Search(ctx context.Context, keyword string, names ...string) ([]SearchResult, error) {
	r.mu.RLock()
	closed := r.drain != nil && r.drain.isClosed()
	r.mu.RUnlock()
	if closed {
		return nil, ErrRegistryClosed
	}
	if strings.TrimSpace(keyword) == "" {
		return nil, errors.New("empty search keyword")
	}
	if len(keyword) > 1024 {
		return nil, errors.New("search keyword too long")
	}
	if len(names) == 0 {
		names = r.Names()
	}
	if len(names) > 64 {
		return nil, errors.New("too many providers")
	}
	type slot struct {
		items []SearchResult
		err   error
	}
	slots := make([]slot, len(names))
	jobs := make(chan int)
	var wg sync.WaitGroup
	workers := r.Workers
	if workers < 1 {
		workers = 4
	}
	if workers > 32 {
		workers = 32
	}
	workers = min(workers, len(names))
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				if e := ctx.Err(); e != nil {
					slots[i].err = e
					continue
				}
				p, ok := r.Get(names[i])
				if !ok {
					slots[i].err = fmt.Errorf("unknown provider %q", names[i])
					continue
				}
				c := ctx
				cancel := func() {}
				timeout := r.Timeout
				if timeout <= 0 {
					timeout = 30 * time.Second
				}
				c, cancel = context.WithTimeout(c, timeout)
				started := time.Now()
				items, e := p.Search(c, keyword)
				elapsed := time.Since(started)
				cancel()
				if len(items) > 1000 && e == nil {
					e = danmaku.ErrLimit
				}
				r.mu.RLock()
				observer := r.searchObserver
				r.mu.RUnlock()
				if observer != nil {
					observer(ctx, p.Name(), elapsed, len(items), e)
				}
				if len(items) > 1000 {
					slots[i].err = danmaku.ErrLimit
					continue
				}
				for _, v := range items {
					slots[i].items = append(slots[i].items, SearchResult{v, p.Name()})
				}
				if e != nil {
					slots[i].err = SourceError{p.Name(), e}
				}
			}
		}()
	}
	for i := range names {
		select {
		case jobs <- i:
		case <-ctx.Done():
			slots[i].err = ctx.Err()
		}
	}
	close(jobs)
	wg.Wait()
	out := make([]SearchResult, 0)
	errs := []error{}
	for _, s := range slots {
		out = append(out, s.items...)
		if s.err != nil {
			errs = append(errs, s.err)
		}
	}
	return out, errors.Join(errs...)
}
func (r *Registry) Catalog() []Capability {
	out := []Capability{}
	for _, n := range r.Names() {
		p, _ := r.Get(n)
		if c, ok := p.(interface{ Capability() Capability }); ok {
			out = append(out, c.Capability())
		} else {
			out = append(out, Capability{Name: n, Search: true, Episodes: true, Comments: true, Status: "custom"})
		}
	}
	return out
}
func DefaultRegistry() *Registry { return DefaultRegistryWithClient(nil) }
func DefaultRegistryWithClient(client *http.Client) *Registry {
	r := NewRegistry(4, NewBilibili(client), NewDandanplay(client, "", ""))
	for _, n := range []string{"gamer", "hanjutv", "sohu", "le", "mgtv", "iqiyi", "tencent", "youku", "renren", "ezdmw", "girigirilove", "hongguo", "mddcloud", "migu", "xigua"} {
		r.Register(NewLegacy(n, client))
	}
	return r
}

// SetSearchObserver receives per-provider duration/count/error only. It is called
// synchronously once after each individual search; no query or result text is
// provided. Observers should perform bounded work and handle cancelled contexts.
func (r *Registry) SetSearchObserver(fn func(context.Context, string, time.Duration, int, error)) {
	r.mu.Lock()
	r.searchObserver = fn
	r.mu.Unlock()
}

// SetRequestLimiter atomically wires built-in adapter HTTP acquisition and
// response feedback. Release is invoked once even on cancellation/read errors.
// Feedback exposes only source name, HTTP status and Retry-After, never URLs,
// queries, headers containing credentials or response bodies.
func (r *Registry) SetRequestLimiter(acquire func(context.Context, string) (func(), error), feedback func(string, int, string)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.drain != nil && r.drain.isClosed() {
		return
	}
	for key, p := range r.providers {
		var next Provider
		var h *HTTPProvider
		switch x := p.(type) {
		case *Bilibili:
			v := *x
			next = &v
			h = &v.HTTPProvider
		case *Dandanplay:
			v := *x
			next = &v
			h = &v.HTTPProvider
		case *Legacy:
			v := *x
			next = &v
			h = &v.HTTPProvider
		default:
			continue
		}
		h.acquireRequest = acquire
		h.responseFeedback = feedback
		h.flights = nextCommentFlights(h.flights)
		r.providers[key] = next
	}
}

// Revision changes whenever an adapter or its private configuration is replaced.
// Consumers can invalidate cached data without inspecting or hashing secrets.
func (r *Registry) Revision() uint64 { r.mu.RLock(); defer r.mu.RUnlock(); return r.revision }
