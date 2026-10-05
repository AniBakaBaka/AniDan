// SPDX-License-Identifier: AGPL-3.0-or-later
package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/AniBakaBaka/AniDan/internal/ratelimit"
)

const localRatePolicyKey = "anidanLocalRateLimitPolicy"

func (s *Server) initRequestLimiter() error {
	policy := ratelimit.Defaults()
	row, err := s.Store.Get(s.ctx, "config", localRatePolicyKey)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if row != nil {
		raw := str(row["config_value"])
		if len(raw) > 1<<20 {
			return errors.New("local rate policy exceeds safety bound")
		}
		d := json.NewDecoder(strings.NewReader(raw))
		d.DisallowUnknownFields()
		if err = d.Decode(&policy); err != nil {
			return err
		}
	}
	limiter, err := ratelimit.New(policy)
	if err != nil {
		return err
	}
	s.RequestLimits = limiter
	s.Providers.SetRequestLimiter(limiter.Acquire, limiter.Feedback)
	s.limiterDone = make(chan struct{})
	go func() {
		defer close(s.limiterDone)
		<-s.ctx.Done()
		limiter.Close()
	}()
	return nil
}
func (s *Server) registerDiagnosticRate(m *http.ServeMux) {
	m.HandleFunc("GET /api/ui/rate-limit/policy", s.operator(s.ratePolicyGet))
	m.HandleFunc("PUT /api/ui/rate-limit/policy", s.operator(s.ratePolicyPut))
	m.HandleFunc("GET /api/control/rate-limit/status", s.operator(s.providerRateLimitStatus))
}
func (s *Server) ratePolicyGet(w http.ResponseWriter, r *http.Request) {
	if s.RequestLimits == nil {
		httpError(w, 503, "Local request limiter has not initialized")
		return
	}
	writeJSON(w, 200, map[string]any{"policy": s.RequestLimits.Policy(), "model": "anidan-local-v1", "scope": "Additional local token-bucket/concurrency controls; remote entitlements and HTTP429 remain authoritative", "restartCounters": true})
}
func (s *Server) ratePolicyPut(w http.ResponseWriter, r *http.Request) {
	if s.RequestLimits == nil {
		httpError(w, 503, "Local request limiter has not initialized")
		return
	}
	policy := ratelimit.Defaults()
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if err := d.Decode(&policy); err != nil {
		httpError(w, 400, err.Error())
		return
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		httpError(w, 400, "Provide one JSON policy object")
		return
	}
	if err := policy.Validate(); err != nil {
		httpError(w, 400, err.Error())
		return
	}
	old := s.RequestLimits.Policy()
	if err := s.RequestLimits.Configure(policy); err != nil {
		httpError(w, 409, err.Error())
		return
	}
	raw, err := json.Marshal(policy)
	if err != nil {
		_ = s.RequestLimits.Configure(old)
		httpError(w, 400, err.Error())
		return
	}
	if err = s.setSettingWithHistory(r.Context(), localRatePolicyKey, string(raw), "rate-limit-policy"); err != nil {
		rollback := s.RequestLimits.Configure(old)
		if rollback != nil {
			httpError(w, 500, "Policy persistence failed and the active policy could not be rolled back; inspect /rate-limit/policy before proceeding")
			return
		}
		httpError(w, 500, "Policy persistence failed; previous active policy restored")
		return
	}
	writeJSON(w, 200, map[string]any{"success": true, "policy": policy, "model": "anidan-local-v1"})
}
func (s *Server) localRateStatus(ctx context.Context) (map[string]any, error) {
	if s.RequestLimits == nil {
		return nil, errors.New("local request limiter has not initialized")
	}
	status := s.RequestLimits.Status()
	policy := s.RequestLimits.Policy()
	rows, err := s.Store.List(ctx, "rate_limit_state", nil, 1000, 0)
	if err != nil {
		return nil, err
	}
	legacy := []map[string]any{}
	for _, row := range rows {
		legacy = append(legacy, map[string]any{"providerName": row["provider_name"], "requestCount": row["request_count"], "lastResetTime": row["last_reset_time"]})
	}
	byName := map[string]ratelimit.BucketStatus{}
	for _, item := range status.Providers {
		byName[item.Name] = item
	}
	providers := []map[string]any{}
	for _, name := range s.Providers.Names() {
		rule := policy.DefaultProvider
		if override, ok := policy.Providers[name]; ok {
			rule = override
		}
		item, ok := byName[name]
		if !ok {
			item = ratelimit.BucketStatus{Name: name, RequestsPerSecond: rule.Rate, Burst: rule.Burst, Concurrency: rule.Concurrency, AvailableTokens: float64(rule.Burst)}
		}
		providers = append(providers, map[string]any{"providerName": name, "displayName": name, "requestCount": max(0, item.Burst-int(math.Floor(item.AvailableTokens))), "quota": item.Burst, "totalAdmissions": item.Requests, "active": item.Active, "concurrency": item.Concurrency, "requestsPerSecond": item.RequestsPerSecond, "retryAfterSeconds": item.RetryAfterSeconds, "blockedUntil": item.BlockedUntil})
	}
	reset := 0
	if status.Global.RequestsPerSecond > 0 {
		reset = int(math.Ceil((float64(status.Global.Burst) - status.Global.AvailableTokens) / status.Global.RequestsPerSecond))
	}
	return map[string]any{"enabled": status.Enabled, "globalEnabled": status.Enabled, "verificationFailed": false, "verificationModel": "validated local config; upstream signed policy not represented", "globalRequestCount": max(0, status.Global.Burst-int(math.Floor(status.Global.AvailableTokens))), "globalLimit": status.Global.Burst, "globalPeriod": fmt.Sprintf("token bucket %.2f requests/second; burst %d", status.Global.RequestsPerSecond, status.Global.Burst), "secondsUntilReset": max(0, reset), "providers": providers, "fallback": nil, "fallbackPolicy": "Fallback and direct requests share each provider's local bucket", "model": "anidan-local-v1", "local": status, "legacyCounters": legacy, "legacyCountersEnforced": false, "apiTokenQuotasEnforced": true, "requestCountMeaning": "currently consumed burst tokens; totalAdmissions are since startup"}, nil
}

var activeRateStreams atomic.Int32

func (s *Server) providerRateLimitStatus(w http.ResponseWriter, r *http.Request) {
	payload, err := s.localRateStatus(r.Context())
	if err != nil {
		httpError(w, 503, err.Error())
		return
	}
	if !strings.EqualFold(r.URL.Query().Get("stream"), "true") {
		writeJSON(w, 200, payload)
		return
	}
	if activeRateStreams.Add(1) > 32 {
		activeRateStreams.Add(-1)
		httpError(w, 503, "Rate status stream capacity reached")
		return
	}
	defer activeRateStreams.Add(-1)
	if _, ok := w.(http.Flusher); !ok {
		httpError(w, 500, "Streaming unavailable")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache, no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	controller := http.NewResponseController(w)
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		_ = controller.SetWriteDeadline(time.Now().Add(5 * time.Second))
		raw, err := json.Marshal(payload)
		if err != nil {
			return
		}
		if _, err = fmt.Fprintf(w, "data: %s\n\n", raw); err != nil {
			return
		}
		if err = controller.Flush(); err != nil {
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-s.ctx.Done():
			return
		case <-ticker.C:
		}
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		payload, err = s.localRateStatus(ctx)
		cancel()
		if err != nil {
			return
		}
	}
}
