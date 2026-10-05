// SPDX-License-Identifier: AGPL-3.0-only
package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"strconv"
	"time"

	"github.com/AniBakaBaka/AniDan/internal/recognition"
)

type playerMatchParams struct {
	Key    string             `json:"key"`
	Parsed recognition.Parsed `json:"parsed"`
}
type playerMatchCache struct {
	Status   string         `json:"status"`
	Response map[string]any `json:"response,omitempty"`
	Error    string         `json:"error,omitempty"`
}

// These are request wait limits, not cancellation deadlines for durable work.
// A zero limit returns immediately; -1 waits until completion or client cancel.
func (s *Server) fallbackWaitContext(ctx context.Context, kind string) (context.Context, context.CancelFunc) {
	def, max := 30.0, 120.0
	if kind == "match" {
		def, max = 60, 300
	}
	seconds, e := strconv.ParseFloat(s.setting(ctx, kind+"FallbackTimeout", strconv.FormatFloat(def, 'f', -1, 64)), 64)
	if e != nil || math.IsNaN(seconds) || math.IsInf(seconds, 0) || seconds < -1 || seconds > max || seconds < 0 && seconds != -1 {
		seconds = def
	}
	if seconds == -1 {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, time.Duration(seconds*float64(time.Second)))
}
func waitFallback(ctx context.Context, ready func(context.Context) (bool, error)) error {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		if e := ctx.Err(); e != nil {
			return e
		}
		done, e := ready(ctx)
		if e != nil {
			return e
		}
		if done {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}
func (s *Server) recognitionMatchFallback(ctx context.Context, request matchRequest, parsed *recognition.Parsed) (map[string]any, error) {
	token, _ := ctx.Value(recognitionPlayerToken{}).(string)
	allowed, e := s.recognitionFallbackAllowed(ctx, "match", token, request.FileName)
	if e != nil || !allowed {
		return nil, e
	}
	sum := sha256.Sum256([]byte(tokenHash(token) + "\x00" + request.FileName))
	key := "player_match_" + hex.EncodeToString(sum[:16])
	var cached playerMatchCache
	if e := s.cacheGet(ctx, key, &cached); e != nil {
		s.playerMu.Lock()
		e = s.cacheGet(ctx, key, &cached)
		if e != nil {
			cached = playerMatchCache{Status: "pending"}
			e = s.cachePut(ctx, key, "search", cached, 5*time.Minute)
			if e == nil {
				_, e = s.Jobs.SubmitRegistered("player_match", playerMatchParams{Key: key, Parsed: *parsed})
			}
		}
		s.playerMu.Unlock()
		if e != nil {
			saveCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			_ = s.cachePut(saveCtx, key, "search", playerMatchCache{Status: "failed", Error: e.Error()}, 5*time.Minute)
			cancel()
			return nil, e
		}
	}
	if cached.Status == "pending" {
		waitCtx, cancel := s.fallbackWaitContext(ctx, "match")
		defer cancel()
		e := waitFallback(waitCtx, func(ctx context.Context) (bool, error) {
			if e := s.cacheGet(ctx, key, &cached); e != nil {
				return false, e
			}
			return cached.Status != "pending", nil
		})
		if errors.Is(e, context.DeadlineExceeded) || errors.Is(e, context.Canceled) {
			return matchResponse(nil, nil), nil
		}
		if e != nil {
			return nil, e
		}
	}
	if cached.Status == "failed" {
		return matchResponse(nil, []string{cached.Error}), nil
	}
	if cached.Response == nil {
		return nil, errors.New("matching job completed without a response")
	}
	return cached.Response, nil
}
func (s *Server) runPlayerMatch(ctx context.Context, raw json.RawMessage, progress func(int, string)) (result any, outcome error) {
	var params playerMatchParams
	if e := unmarshalExactJSON(raw, &params); e != nil {
		return nil, e
	}
	defer func() {
		recovered := recover()
		if recovered != nil {
			outcome = errors.New("Matching interrupted by an internal error")
		}
		if outcome != nil {
			saveCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = s.cachePut(saveCtx, params.Key, "search", playerMatchCache{Status: "failed", Error: outcome.Error()}, 5*time.Minute)
		}
		if recovered != nil {
			panic(recovered)
		}
	}()
	progress(5, "Searching a matching source")
	response, e := s.recognitionMatchFallbackWork(ctx, &params.Parsed)
	if e != nil {
		return nil, e
	}
	if e = s.cachePut(ctx, params.Key, "search", playerMatchCache{Status: "completed", Response: response}, 5*time.Minute); e != nil {
		return nil, e
	}
	progress(100, "Matching complete")
	return response, nil
}
