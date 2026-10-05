// SPDX-License-Identifier: AGPL-3.0-only
package server

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/AniBakaBaka/AniDan/internal/job"
	"github.com/AniBakaBaka/AniDan/internal/provider"
	"github.com/AniBakaBaka/AniDan/internal/store"
)

const (
	episodeFetchLimit    = 32
	episodeFetchLifetime = 2 * time.Minute
)

var errEpisodeFetchBusy = errors.New("episode download capacity reached; retry later")
var errEpisodePoolUnreadable = errors.New("existing episode comments are unreadable")

// Only a completed failure before saveCommentsSnapshot begins may carry this
// marker. A cancelled waiter or expired worker can still be draining, so those
// returns deliberately do not imply that storage was untouched.
var errEpisodeFetchNoPublication = errors.New("episode fetch did not start publication")

func episodeFetchUnpublished(err error) error {
	return fmt.Errorf("%w: %w", errEpisodeFetchNoPublication, err)
}

// The key binds the entire fetch/publication operation to immutable input
// identities, including the configured adapter. Path/count are publication
// state, not network identity: waiters must share a flight while it saves.
type episodeFetchKey struct {
	episodeID, sourceID, episodeIndex, animeID, sourceOrder int64
	providerEpisodeID, providerName, mediaID, adapter       string
	// Native imports retain caller-owned deadlines and normal publication
	// limits; they cannot join the fixed-lifetime player/speculative policy.
	nativeImport           bool
	importTarget           string
	importGroup            string
	importGroupTarget      string
	importListingIdentity  string
	importGroupedOwnership bool
}

type episodeFetchResult struct {
	episodeID, count int64
	unchanged        bool
	noPublication    bool
	fetchedCount     int
	publication      commentPublicationOutcome
}

func (r episodeFetchResult) wire() any {
	v := map[string]any{"episodeId": r.episodeID, "count": r.count}
	if r.unchanged {
		v["unchanged"] = true
	}
	if r.noPublication {
		v["noPublication"] = true
	}
	return v
}

type episodeFetchCall struct {
	done               chan struct{}
	work               context.Context
	cancel             context.CancelFunc
	publicationReady   chan struct{}
	publicationAllowed chan struct{}
	publicationOnce    sync.Once
	refs               int
	force              bool
	result             episodeFetchResult
	err                error
}

type episodeFetchPublicationKey struct{}

// Shared work must not inherit one subscriber's pause gate. Instead it asks
// the existing waiter goroutines for publication permission; any unpaused
// subscriber may provide it after checking its own task context.
func awaitEpisodePublication(ctx context.Context) error {
	call, ok := ctx.Value(episodeFetchPublicationKey{}).(*episodeFetchCall)
	if !ok {
		return errors.New("shared episode publication handshake is missing")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	close(call.publicationReady)
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-call.publicationAllowed:
		return ctx.Err()
	}
}

type episodeFetchState struct {
	mu       sync.Mutex
	ctx      context.Context
	limit    int
	lifetime time.Duration
	active   int
	closed   bool
	drained  chan struct{}
	calls    map[episodeFetchKey]*episodeFetchCall
}

func newEpisodeFetchState(ctx context.Context, limit int, lifetime time.Duration) *episodeFetchState {
	return &episodeFetchState{ctx: ctx, limit: limit, lifetime: lifetime, calls: make(map[episodeFetchKey]*episodeFetchCall), drained: make(chan struct{})}
}

func (f *episodeFetchState) close(ctx context.Context) error {
	f.mu.Lock()
	if !f.closed {
		f.closed = true
		for _, call := range f.calls {
			call.cancel()
		}
		if f.active == 0 {
			close(f.drained)
		}
	}
	done := f.drained
	f.mu.Unlock()
	select {
	case <-done:
		return nil
	default:
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return errors.Join(errStartupCleanupUncertain, ctx.Err())
	}
}

func (s *Server) episodeFetchRuntime() *episodeFetchState {
	s.episodeFetchOnce.Do(func() {
		root := s.ctx
		if root == nil {
			root = context.Background()
		}
		s.episodeFetch = newEpisodeFetchState(root, episodeFetchLimit, episodeFetchLifetime)
	})
	return s.episodeFetch
}

// do keeps cancelled workers in the active budget until they actually exit.
// An adapter ignoring cancellation can therefore exhaust, but cannot grow,
// this bounded pool. Neither the first caller's task context nor its progress
// callback is ever used by the shared worker.
func (f *episodeFetchState) do(ctx context.Context, key episodeFetchKey, missingOnly bool,
	probe func(context.Context) (episodeFetchResult, bool, error),
	load func(context.Context) (episodeFetchResult, error),
	progress func(int, string),
) (episodeFetchResult, error) {
	if err := ctx.Err(); err != nil {
		return episodeFetchResult{}, episodeFetchUnpublished(err)
	}
	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		return episodeFetchResult{}, episodeFetchUnpublished(context.Canceled)
	}
	if err := f.ctx.Err(); err != nil {
		f.mu.Unlock()
		return episodeFetchResult{}, episodeFetchUnpublished(err)
	}
	call := f.calls[key]
	if call == nil {
		if f.active >= f.limit {
			f.mu.Unlock()
			return episodeFetchResult{}, episodeFetchUnpublished(errEpisodeFetchBusy)
		}
		var work context.Context
		var cancel context.CancelFunc
		if key.nativeImport {
			// The old import path had no additional whole-operation deadline.
			// Provider deadlines and each subscriber's task context still apply;
			// the last departure cancels work, and draining work remains bounded.
			work, cancel = context.WithCancel(f.ctx)
		} else {
			work, cancel = context.WithTimeout(f.ctx, f.lifetime)
		}
		call = &episodeFetchCall{done: make(chan struct{}), work: work, cancel: cancel, publicationReady: make(chan struct{}), publicationAllowed: make(chan struct{}), refs: 1, force: !missingOnly}
		work = context.WithValue(work, episodeFetchPublicationKey{}, call)
		f.calls[key] = call
		f.active++
		go func() {
			// Shared work runs outside a job's handler goroutine and therefore
			// needs its own recovery boundary. Never expose adapter panic data.
			defer func() {
				if recover() != nil {
					f.mu.Lock()
					f.finishLocked(key, call, episodeFetchResult{}, errors.New("episode download failed unexpectedly"))
					f.mu.Unlock()
				}
			}()
			if missingOnly && probe != nil {
				result, ready, err := probe(work)
				f.mu.Lock()
				// A foreground refresh that joins before this decision upgrades
				// a warm-only probe to an actual download. Once completed, the
				// entry is gone and a later refresh starts a new operation.
				if (err != nil && (!call.force || !errors.Is(err, errEpisodePoolUnreadable))) || (ready && !call.force) {
					f.finishLocked(key, call, result, err)
					f.mu.Unlock()
					return
				}
				f.mu.Unlock()
			}
			result, err := load(work)
			f.mu.Lock()
			f.finishLocked(key, call, result, err)
			f.mu.Unlock()
		}()
	} else {
		call.refs++
		call.force = call.force || !missingOnly
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
	if progress != nil {
		progress(10, "Fetching comments")
	}
	publicationReady := call.publicationReady
	for {
		select {
		case <-ctx.Done():
			return episodeFetchResult{}, ctx.Err()
		case <-call.work.Done():
			// Completion cancels work to release the timer; prefer its
			// published result, otherwise report cancellation/lifetime.
			select {
			case <-call.done:
				goto completed
			default:
				return episodeFetchResult{}, call.work.Err()
			}
		case <-call.done:
			goto completed
		case <-publicationReady:
			publicationReady = nil
			// AfterFunc uses no dedicated waiting goroutine. Preserve this
			// subscriber's task values, while also waking its paused gate
			// when the shared lifetime ends or another caller finishes it.
			checkpointCtx, cancelCheckpoint := context.WithCancel(ctx)
			stop := context.AfterFunc(call.work, cancelCheckpoint)
			err := job.Checkpoint(checkpointCtx)
			stop()
			cancelCheckpoint()
			if err != nil {
				if ctx.Err() != nil {
					return episodeFetchResult{}, ctx.Err()
				}
				select {
				case <-call.done:
					goto completed
				default:
					return episodeFetchResult{}, call.work.Err()
				}
			}
			if call.work.Err() == nil {
				call.publicationOnce.Do(func() { close(call.publicationAllowed) })
			}
		}
	}
completed:
	if err := ctx.Err(); err != nil {
		return episodeFetchResult{}, err
	}
	if call.err != nil {
		return episodeFetchResult{}, call.err
	}
	if progress != nil {
		progress(100, "Saved comments")
	}
	return call.result, nil
}

func (f *episodeFetchState) finishLocked(key episodeFetchKey, call *episodeFetchCall, result episodeFetchResult, err error) {
	if err == nil {
		err = call.work.Err()
	}
	call.result, call.err = result, err
	if f.calls[key] == call {
		delete(f.calls, key)
	}
	f.active--
	if f.closed && f.active == 0 {
		close(f.drained)
	}
	close(call.done)
	call.cancel()
}

func (s *Server) episodeFetchSnapshot(ctx context.Context, ep, expectedSource store.Row) (store.Row, store.Row, provider.Provider, episodeFetchKey, error) {
	var key episodeFetchKey
	if ep == nil {
		return nil, nil, nil, key, errors.New("episode required")
	}
	current, err := s.Store.Get(ctx, "episode", ep["id"])
	if err != nil {
		return nil, nil, nil, key, err
	}
	if current == nil {
		return nil, nil, nil, key, errors.New("episode not found")
	}
	for _, field := range []string{"source_id", "provider_episode_id", "episode_index"} {
		if str(current[field]) != str(ep[field]) {
			return nil, nil, nil, key, fmt.Errorf("episode identity changed (%s); reload and retry", field)
		}
	}
	src, err := s.Store.Get(ctx, "anime_sources", current["source_id"])
	if err != nil {
		return nil, nil, nil, key, err
	}
	if src == nil {
		return nil, nil, nil, key, errors.New("source not found")
	}
	if expectedSource != nil {
		for _, field := range []string{"id", "anime_id", "provider_name", "media_id", "source_order"} {
			if str(src[field]) != str(expectedSource[field]) {
				return nil, nil, nil, key, fmt.Errorf("source identity changed (%s); reload and retry", field)
			}
		}
	}
	p, ok := s.Providers.Get(str(src["provider_name"]))
	if !ok {
		return nil, nil, nil, key, fmt.Errorf("provider %s has no Go adapter", str(src["provider_name"]))
	}
	key = episodeFetchKey{episodeID: number(current["id"]), sourceID: number(current["source_id"]), episodeIndex: number(current["episode_index"]), animeID: number(src["anime_id"]), sourceOrder: number(src["source_order"]), providerEpisodeID: str(current["provider_episode_id"]), providerName: str(src["provider_name"]), mediaID: str(src["media_id"]), adapter: provider.CacheIdentity(p)}
	return current, src, p, key, nil
}

// fetchEpisodeShared preserves explicit refresh semantics: completed values are
// not cached. Concurrent refresh/prefetch subscribers share one durable save.
func (s *Server) fetchEpisodeShared(ctx context.Context, ep store.Row, progress func(int, string)) (any, error) {
	return s.fetchEpisode(ctx, ep, nil, progress, false, false)
}

// fetchEpisodeSharedMissing additionally skips a readable, nonempty local pool.
// Its probe belongs only to a newly admitted flight, so it cannot race another
// flight's publication or prevent an explicit refresh joining it from fetching.
func (s *Server) fetchEpisodeSharedMissing(ctx context.Context, ep store.Row, progress func(int, string)) (any, error) {
	return s.fetchEpisode(ctx, ep, nil, progress, true, false)
}

// Predownload mapping must carry its original source snapshot through admission;
// otherwise a reattachment between mapping and fetching could adopt a different
// provider/media identity before the normal publication fence is established.
func (s *Server) fetchEpisodeSharedMissingFromSource(ctx context.Context, ep, expectedSource store.Row, progress func(int, string)) (any, error) {
	if expectedSource == nil {
		return nil, episodeFetchUnpublished(errors.New("expected source snapshot required"))
	}
	return s.fetchEpisode(ctx, ep, expectedSource, progress, true, true)
}

func (s *Server) fetchEpisode(ctx context.Context, ep, expectedSource store.Row, progress func(int, string), missingOnly, speculative bool) (any, error) {
	if err := job.Checkpoint(ctx); err != nil {
		return nil, episodeFetchUnpublished(err)
	}
	snapshot, source, adapter, key, err := s.episodeFetchSnapshot(ctx, ep, expectedSource)
	if err != nil {
		return nil, episodeFetchUnpublished(err)
	}
	revalidate := func(work context.Context) (store.Row, error) {
		if err := work.Err(); err != nil {
			return nil, err
		}
		current, _, _, fresh, err := s.episodeFetchSnapshot(work, snapshot, source)
		if err != nil {
			return nil, err
		}
		if fresh != key {
			return nil, errors.New("download identity changed; reload and retry")
		}
		return current, nil
	}
	probe := func(work context.Context) (episodeFetchResult, bool, error) {
		current, err := revalidate(work)
		if err != nil {
			return episodeFetchResult{}, false, episodeFetchUnpublished(err)
		}
		comments, err := s.readComments(work, current)
		if work.Err() != nil {
			return episodeFetchResult{}, false, episodeFetchUnpublished(work.Err())
		}
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return episodeFetchResult{}, false, episodeFetchUnpublished(fmt.Errorf("%w: %w", errEpisodePoolUnreadable, err))
		}
		return episodeFetchResult{episodeID: key.episodeID, count: int64(len(comments)), unchanged: true}, err == nil && len(comments) > 0, nil
	}
	load := func(work context.Context) (episodeFetchResult, error) {
		if speculative {
			work = context.WithValue(work, predownloadPublicationKey{}, true)
		}
		if _, err := revalidate(work); err != nil {
			return episodeFetchResult{}, episodeFetchUnpublished(err)
		}
		if _, err := s.inspectCommentPool(work, snapshot); err != nil {
			return episodeFetchResult{}, episodeFetchUnpublished(err)
		}
		comments, err := adapter.Comments(work, key.providerEpisodeID)
		if err != nil {
			return episodeFetchResult{}, episodeFetchUnpublished(err)
		}
		current, err := revalidate(work)
		if err != nil {
			return episodeFetchResult{}, episodeFetchUnpublished(err)
		}
		// An empty upstream result is not a reusable local pool. Publishing
		// empty XML would hide the missing-file signal on later playback.
		if len(comments) == 0 {
			pool, err := s.inspectCommentPool(work, current)
			if err != nil {
				return episodeFetchResult{}, episodeFetchUnpublished(err)
			}
			return episodeFetchResult{episodeID: key.episodeID, count: pool.count, unchanged: true, noPublication: true}, nil
		}
		if err = awaitEpisodePublication(work); err != nil {
			return episodeFetchResult{}, episodeFetchUnpublished(err)
		}
		// A paused-only flight can wait here until resume. Recheck the
		// captured identities before creating any object after that wait.
		current, err = revalidate(work)
		if err != nil {
			return episodeFetchResult{}, episodeFetchUnpublished(err)
		}
		published, err := s.saveCommentsSnapshotOutcome(work, snapshot, source, comments, true)
		if err != nil {
			return episodeFetchResult{}, err
		}
		return episodeFetchResult{episodeID: key.episodeID, count: published.PoolCount, fetchedCount: len(comments), unchanged: published.Disposition == commentSmallerRetained, noPublication: published.Disposition == commentSmallerRetained && !published.StagingStarted && !published.Committed && !published.CommitUncertain, publication: published}, nil
	}
	result, err := s.episodeFetchRuntime().do(ctx, key, missingOnly, probe, load, progress)
	if err != nil {
		return nil, err
	}
	return result.wire(), nil
}
