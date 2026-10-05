// SPDX-License-Identifier: AGPL-3.0-only
package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/AniBakaBaka/AniDan/internal/job"
	"github.com/AniBakaBaka/AniDan/internal/provider"
	"github.com/AniBakaBaka/AniDan/internal/store"
)

const predownloadIntentLimit = 32

type predownloadIntent struct {
	EpisodeID, SourceID, Index int64
	ProviderEpisodeID          string
}
type predownloadParams struct {
	Current              predownloadIntent `json:"current"`
	AnimeID, SourceOrder int64
	Provider, MediaID    string
	RequestedAt          time.Time
}
type predownloadRuntime struct {
	ctx                                         context.Context
	cancel                                      context.CancelFunc
	done                                        chan struct{}
	intents                                     chan predownloadIntent
	mu                                          sync.Mutex
	recent                                      map[int64]time.Time
	active                                      string
	running                                     atomic.Bool
	observed, dropped, busy, admitted, failures atomic.Uint64
}

func (s *Server) initPredownload(mux *http.ServeMux) error {
	ctx, cancel := context.WithCancel(s.ctx)
	n := &predownloadRuntime{ctx: ctx, cancel: cancel, done: make(chan struct{}), intents: make(chan predownloadIntent, predownloadIntentLimit), recent: map[int64]time.Time{}}
	s.predownload = n
	if err := s.Jobs.Register("predownload", s.runPredownload); err != nil {
		cancel()
		close(n.done) // The loop was never started; failed startup can join it.
		return err
	}
	mux.HandleFunc("GET /api/ui/predownload/status", s.operator(s.predownloadStatus))
	go s.predownloadLoop(n)
	return nil
}
func (s *Server) closePredownload() {
	if s.predownload != nil {
		s.predownload.cancel()
	}
}
func (s *Server) predownloadEnabled(ctx context.Context) (bool, bool, error) {
	read := func(key string) (bool, error) {
		var v string
		err := s.Store.DB.QueryRowContext(ctx, s.Store.Rebind("SELECT SUBSTR(config_value,1,17) FROM config WHERE config_key=?"), key).Scan(&v)
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		if v != "true" && v != "false" {
			return false, fmt.Errorf("invalid boolean setting %s", key)
		}
		return v == "true", nil
	}
	enabled, err := read("preDownloadNextEpisodeEnabled")
	if err != nil {
		return false, false, err
	}
	search, err := read("searchFallbackEnabled")
	if err != nil {
		return enabled, false, err
	}
	match, err := read("matchFallbackEnabled")
	return enabled, search || match, err
}
func predownloadIntentFor(ep store.Row) predownloadIntent {
	return predownloadIntent{number(ep["id"]), number(ep["source_id"]), number(ep["episode_index"]), str(ep["provider_episode_id"])}
}

// queuePredownload does no SQL or network work in the player response. Repeated
// playback is bounded by a small, expiring intent set; queue overflow is visible.
func (s *Server) queuePredownload(ep store.Row) {
	n := s.predownload
	if n == nil || n.ctx.Err() != nil || ep == nil {
		return
	}
	v := predownloadIntentFor(ep)
	if v.EpisodeID <= 0 || v.SourceID <= 0 || len(v.ProviderEpisodeID) > 2048 {
		return
	}
	n.observed.Add(1)
	now := time.Now()
	n.mu.Lock()
	defer n.mu.Unlock()
	if at, ok := n.recent[v.EpisodeID]; ok && now.Sub(at) < 2*time.Minute {
		return
	}
	if len(n.recent) >= 128 {
		var oldest int64
		var at time.Time
		for id, t := range n.recent {
			if at.IsZero() || t.Before(at) {
				oldest, at = id, t
			}
		}
		delete(n.recent, oldest)
	}
	select {
	case n.intents <- v:
		n.recent[v.EpisodeID] = now
	default:
		n.dropped.Add(1)
	}
}
func (s *Server) predownloadLoop(n *predownloadRuntime) {
	defer close(n.done)
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-n.ctx.Done():
			return
		case <-ticker.C:
			n.mu.Lock()
			active := n.active
			n.mu.Unlock()
			if active == "" {
				continue
			}
			ctx, cancel := context.WithTimeout(n.ctx, 2*time.Second)
			enabled, fallback, err := s.predownloadEnabled(ctx)
			if err != nil {
				n.failures.Add(1)
			}
			if err != nil || !enabled || !fallback {
				_ = s.Jobs.Cancel(active)
			}
			task, ok := s.Jobs.Get(active)
			if !ok || job.Terminal(task.Status) {
				n.mu.Lock()
				if n.active == active {
					n.active = ""
				}
				n.mu.Unlock()
			}
			cancel()
		case intent := <-n.intents:
			ctx, cancel := context.WithTimeout(n.ctx, 3*time.Second)
			// Consumption is bookkeeping only. An uncertain error retains the charged
			// reservation rather than silently increasing the proactive storage budget.
			if err := s.releasePredownloadPool(ctx, intent.EpisodeID, ""); err != nil {
				n.failures.Add(1)
			}
			enabled, fallback, err := s.predownloadEnabled(ctx)
			if err != nil {
				n.failures.Add(1)
				cancel()
				continue
			}
			if !enabled || !fallback {
				cancel()
				continue
			}
			n.mu.Lock()
			active := n.active
			n.mu.Unlock()
			if active != "" {
				t, ok := s.Jobs.Get(active)
				if ok && !job.Terminal(t.Status) {
					n.busy.Add(1)
					cancel()
					continue
				}
			}
			ep, err := s.Store.Get(ctx, "episode", intent.EpisodeID)
			if err != nil || predownloadIntentFor(ep) != intent {
				cancel()
				continue
			}
			src, err := s.Store.Get(ctx, "anime_sources", intent.SourceID)
			if err != nil || boolean(src["is_finished"]) {
				cancel()
				continue
			}
			p := predownloadParams{Current: intent, AnimeID: number(src["anime_id"]), SourceOrder: number(src["source_order"]), Provider: str(src["provider_name"]), MediaID: str(src["media_id"]), RequestedAt: time.Now().UTC()}
			id, err := s.Jobs.SubmitWithOptions("predownload", p, nil, job.SubmitOptions{Title: "预下载下一集弹幕", QueueType: "fallback", UniqueKey: "anidan-predownload-active"})
			if err == nil {
				n.admitted.Add(1)
				n.mu.Lock()
				n.active = id
				n.mu.Unlock()
			} else if errors.Is(err, job.ErrAlreadyRunning) || errors.Is(err, job.ErrQueueFull) {
				n.busy.Add(1)
			} else {
				n.failures.Add(1)
			}
			cancel()
		}
	}
}
func (s *Server) cancelDisabledPredownload(ctx context.Context) {
	n := s.predownload
	if n == nil {
		return
	}
	enabled, fallback, err := s.predownloadEnabled(ctx)
	if err == nil && enabled && fallback {
		return
	}
	n.mu.Lock()
	id := n.active
	n.mu.Unlock()
	if id != "" {
		_ = s.Jobs.Cancel(id)
	}
}
func (s *Server) runPredownload(ctx context.Context, raw json.RawMessage, progress func(int, string)) (out any, outErr error) {
	var p predownloadParams
	if err := unmarshalExactJSON(raw, &p); err != nil {
		return nil, err
	}
	if p.Current.EpisodeID <= 0 || p.Current.SourceID <= 0 || p.Current.ProviderEpisodeID == "" || len(p.Current.ProviderEpisodeID) > 2048 || p.Current.Index < 0 || p.Current.Index >= 1_000_000 || p.Provider == "" || len(p.Provider) > 500 || len(p.MediaID) > 255 || p.RequestedAt.IsZero() || time.Since(p.RequestedAt) > 24*time.Hour || p.RequestedAt.After(time.Now().Add(time.Minute)) {
		return job.DiagnosticResult{"reviewRequired": true, "reason": "Predownload intent lacks a current exact identity or is expired", "action": "Play the current episode again to create a new bounded intent; do not replay old parameters"}, errors.New("invalid or expired predownload intent")
	}
	n := s.predownload
	if n == nil || !n.running.CompareAndSwap(false, true) {
		return map[string]any{"skipped": "predownload concurrency limit"}, nil
	}
	defer n.running.Store(false)
	defer func() {
		if outErr != nil {
			n.failures.Add(1)
		}
	}()
	if info, ok := job.ExecutionInfo(ctx); ok {
		n.mu.Lock()
		n.active = info.TaskID
		n.mu.Unlock()
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	if err := job.Checkpoint(ctx); err != nil {
		return nil, err
	}
	enabled, fallback, err := s.predownloadEnabled(ctx)
	if err != nil {
		return nil, err
	}
	if !enabled || !fallback {
		return map[string]any{"skipped": "predownload disabled"}, nil
	}
	ep, err := s.Store.Get(ctx, "episode", p.Current.EpisodeID)
	if err != nil {
		return nil, err
	}
	if predownloadIntentFor(ep) != p.Current {
		return nil, errors.New("current episode changed after playback")
	}
	src, err := s.Store.Get(ctx, "anime_sources", p.Current.SourceID)
	if err != nil {
		return nil, err
	}
	if number(src["anime_id"]) != p.AnimeID || number(src["source_order"]) != p.SourceOrder || str(src["provider_name"]) != p.Provider || str(src["media_id"]) != p.MediaID {
		return nil, errors.New("source changed after predownload admission")
	}
	if boolean(src["is_finished"]) {
		return map[string]any{"skipped": "source finished"}, nil
	}
	adapter, exists := s.Providers.Get(p.Provider)
	if !exists {
		return map[string]any{"skipped": "native provider unavailable"}, nil
	}
	if advertised, ok := adapter.(interface{ Capability() provider.Capability }); ok {
		capability := advertised.Capability()
		if !capability.Episodes || !capability.Comments {
			return map[string]any{"skipped": "provider lacks native episode/comment capability"}, nil
		}
	}
	scraper, err := s.Store.Get(ctx, "scrapers", p.Provider)
	if err != nil {
		return nil, err
	}
	if !boolean(scraper["is_enabled"]) {
		return map[string]any{"skipped": "provider disabled"}, nil
	}
	current, err := s.readComments(ctx, ep)
	if err != nil {
		return nil, err
	}
	if len(current) == 0 {
		return map[string]any{"skipped": "current episode has no comments"}, nil
	}
	anime, err := s.Store.Get(ctx, "anime", p.AnimeID)
	if err != nil {
		return nil, err
	}
	if str(anime["type"]) == "movie" {
		return map[string]any{"skipped": "movie has no next episode"}, nil
	}
	progress(5, "Resolving exact next episode")
	next, err := s.resolvePredownloadEpisode(ctx, ep, src, anime)
	if err != nil {
		return nil, err
	}
	if next == nil {
		return map[string]any{"skipped": "next episode unavailable"}, nil
	}
	if comments, e := s.readComments(ctx, next); e == nil && len(comments) > 0 {
		return map[string]any{"skipped": "next episode already available", "episodeId": next["id"]}, nil
	}
	if err = job.Checkpoint(ctx); err != nil {
		return nil, err
	}
	token, err := s.reservePredownloadPool(ctx, next)
	if err != nil {
		return nil, err
	}
	// Reserve worst-case XML staging/publication bytes before a shared download. On
	// errors the reservation stays charged, including uncertain publication.
	result, err := s.fetchEpisodeSharedMissingFromSource(ctx, next, src, progress)
	if err != nil {
		if errors.Is(err, errEpisodeFetchNoPublication) {
			cleanup, stop := context.WithTimeout(s.ctx, 3*time.Second)
			_ = s.releasePredownloadPool(cleanup, number(next["id"]), token)
			stop()
		}
		return nil, err
	}
	fresh, err := s.Store.Get(ctx, "episode", next["id"])
	if err != nil {
		return nil, err
	}
	if number(fresh["comment_count"]) <= 0 {
		if wire, ok := result.(map[string]any); ok && boolean(wire["noPublication"]) {
			_ = s.releasePredownloadPool(ctx, number(next["id"]), token)
		}
		return nil, errors.New("next episode returned no comments")
	}
	if err = s.settlePredownloadPool(ctx, fresh, token); err != nil {
		return nil, err
	}
	progress(100, "Next episode saved")
	return result, nil
}
func (s *Server) predownloadStatus(w http.ResponseWriter, r *http.Request) {
	enabled, fallback, err := s.predownloadEnabled(r.Context())
	n := s.predownload
	counters := map[string]uint64{}
	if n != nil {
		counters = map[string]uint64{"observed": n.observed.Load(), "dropped": n.dropped.Load(), "busy": n.busy.Load(), "admitted": n.admitted.Load(), "errors": n.failures.Load()}
	}
	budget, budgetErr := s.predownloadPoolStats(r.Context())
	writeJSON(w, 200, map[string]any{"configuredEnabled": enabled, "fallbackEnabled": fallback, "effectiveEnabled": enabled && fallback && err == nil, "limits": map[string]any{"intentQueue": 32, "concurrency": 1, "maxEpisodeBytes": 64 << 20, "maxUnplayedBytes": 256 << 20, "maxUnplayedPools": 16, "reservationBytes": 192 << 20, "publishedChargeMultiplier": 2}, "counters": counters, "budget": budget, "degraded": err != nil || budgetErr != nil})
}
func validatePredownloadSetting(key, value string) error {
	if key == "preDownloadNextEpisodeEnabled" && value != "true" && value != "false" {
		return errors.New("preDownloadNextEpisodeEnabled must be true or false")
	}
	return nil
}
