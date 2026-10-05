// SPDX-License-Identifier: AGPL-3.0-only
package provider

import (
	"context"
	"errors"
	"sync"
)

var ErrRegistryClosed = errors.New("provider registry is closed")

// flightLifetime survives adapter reconfiguration without retaining an adapter,
// its credentials, or completed data. It tracks workers even after the final
// subscriber leaves and removes their keys from the coalescing map.
type flightLifetime struct {
	mu      sync.Mutex
	closed  bool
	workers map[*commentCall]struct{}
	done    chan struct{}
}

func newFlightLifetime() *flightLifetime {
	return &flightLifetime{workers: make(map[*commentCall]struct{}), done: make(chan struct{})}
}
func (l *flightLifetime) isClosed() bool { l.mu.Lock(); defer l.mu.Unlock(); return l.closed }
func (l *flightLifetime) begin(c *commentCall) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return false
	}
	l.workers[c] = struct{}{}
	return true
}
func (l *flightLifetime) end(c *commentCall) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.workers, c)
	if l.closed && len(l.workers) == 0 {
		close(l.done)
	}
}
func (l *flightLifetime) stop() <-chan struct{} {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.closed {
		l.closed = true
		for c := range l.workers {
			c.cancel()
		}
		if len(l.workers) == 0 {
			close(l.done)
		}
	}
	return l.done
}
func nextCommentFlights(previous *commentFlights) *commentFlights {
	next := newCommentFlights()
	if previous != nil {
		next.life = previous.life
	}
	return next
}

// Shared with cache snapshots. Configuration replacements reuse their existing
// lifetime; only explicitly registered adapters add a small lifetime record.
type registryDrain struct {
	mu     sync.Mutex
	closed bool
	lives  map[*flightLifetime]struct{}
}

func newRegistryDrain() *registryDrain {
	return &registryDrain{lives: make(map[*flightLifetime]struct{})}
}
func (d *registryDrain) isClosed() bool { d.mu.Lock(); defer d.mu.Unlock(); return d.closed }
func (d *registryDrain) track(p Provider) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return false
	}
	var h *HTTPProvider
	switch v := p.(type) {
	case *Bilibili:
		h = &v.HTTPProvider
	case *Dandanplay:
		h = &v.HTTPProvider
	case *Legacy:
		h = &v.HTTPProvider
	}
	if h != nil && h.flights != nil {
		d.lives[h.flights.life] = struct{}{}
	}
	return true
}

// CloseContext permanently rejects new registry/coalesced work and waits for
// every owned coalesced worker, including retired configurations. A deadline
// error means workers are not proven stopped; callers must not switch storage.
// Synchronous provider calls remain owned and joined by their calling server.
func (r *Registry) CloseContext(ctx context.Context) error {
	r.mu.Lock()
	if r.drain == nil {
		r.drain = newRegistryDrain()
	}
	d := r.drain
	r.mu.Unlock()
	d.mu.Lock()
	d.closed = true
	lives := make([]*flightLifetime, 0, len(d.lives))
	for l := range d.lives {
		lives = append(lives, l)
	}
	d.mu.Unlock()
	done := make([]<-chan struct{}, 0, len(lives))
	for _, l := range lives {
		done = append(done, l.stop())
	}
	for _, ch := range done {
		select {
		case <-ch:
			continue
		default:
		}
		select {
		case <-ch:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}
