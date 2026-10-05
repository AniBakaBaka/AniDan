// SPDX-License-Identifier: AGPL-3.0-only
package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"regexp"
	"strings"
	"time"
)

var cacheBackendRegion = regexp.MustCompile(`^[a-z0-9_-]{1,48}$`)

func (s *Server) registerCacheBackend(m *http.ServeMux) {
	m.HandleFunc("GET /api/ui/cache/backend/stats", s.operator(s.cacheBackendStats))
	m.HandleFunc("GET /api/ui/cache/backend/list", s.operator(s.cacheBackendList))
	m.HandleFunc("GET /api/ui/cache/backend/detail", s.operator(s.cacheBackendDetail))
	m.HandleFunc("DELETE /api/ui/cache/backend/key", s.operator(s.cacheBackendDelete))
	m.HandleFunc("DELETE /api/ui/cache/backend/clear", s.operator(s.cacheBackendClear))
}
func (s *Server) backendContext(w http.ResponseWriter, r *http.Request) (context.Context, context.CancelFunc, string, bool) {
	if s.Cache == nil {
		httpError(w, 503, "Response cache backend is not initialized")
		return nil, nil, "", false
	}
	region := r.URL.Query().Get("region")
	if region == "all" {
		region = ""
	}
	if region != "" && !cacheBackendRegion.MatchString(region) {
		httpError(w, 422, "Invalid cache region")
		return nil, nil, "", false
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	w.Header().Set("Cache-Control", "no-store")
	return ctx, cancel, region, true
}
func (s *Server) cacheBackendStats(w http.ResponseWriter, r *http.Request) {
	ctx, cancel, region, ok := s.backendContext(w, r)
	if !ok {
		return
	}
	defer cancel()
	stats, err := s.Cache.Stats(ctx, region)
	if err != nil {
		writeJSON(w, 503, map[string]any{"detail": "Response cache statistics unavailable", "health": s.Cache.Health()})
		return
	}
	writeJSON(w, 200, map[string]any{"scope": "ephemeral_responses", "namespace": s.Config.Cache.Namespace, "health": s.Cache.Health(), "stats": stats, "limits": map[string]any{"entries": s.Config.Cache.MemoryMaxsize, "bytes": s.Config.Cache.MaxBytes, "valueBytes": s.Config.Cache.MaxValueBytes}, "localCaches": s.runtimeCacheStats(), "durableState": "SQL protocol state is excluded from response cache operations", "configurationNotes": s.Config.Cache.Diagnostics()})
}
func (s *Server) cacheBackendList(w http.ResponseWriter, r *http.Request) {
	ctx, cancel, region, ok := s.backendContext(w, r)
	if !ok {
		return
	}
	defer cancel()
	limit, err := queryInt(r, "limit", 20, 100)
	if err != nil || limit < 1 {
		httpError(w, 422, "limit must be 1..100")
		return
	}
	page, err := s.Cache.List(ctx, region, r.URL.Query().Get("after"), limit)
	if err != nil {
		httpError(w, 503, "Response cache listing unavailable or cursor invalid")
		return
	}
	writeJSON(w, 200, page)
}
func (s *Server) cacheBackendDetail(w http.ResponseWriter, r *http.Request) {
	ctx, cancel, _, ok := s.backendContext(w, r)
	if !ok {
		return
	}
	defer cancel()
	key := r.URL.Query().Get("key")
	entry, hit, err := s.Cache.GetItem(ctx, key)
	if err != nil {
		httpError(w, 503, "Response cache entry unavailable or outside the configured namespace")
		return
	}
	if !hit {
		httpError(w, 404, "Response cache entry missing or expired")
		return
	}
	if len(entry.JSON) > 1<<20 {
		httpError(w, 413, "Response cache detail exceeds 1 MiB")
		return
	}
	var value any
	decoder := json.NewDecoder(bytes.NewReader(entry.JSON))
	decoder.UseNumber()
	if err = decoder.Decode(&value); err != nil {
		httpError(w, 503, "Response cache entry is invalid")
		return
	}
	parts := strings.Split(key, ":")
	writeJSON(w, 200, map[string]any{"key": key, "region": parts[4], "value": value, "expiresAt": entry.ExpiresAt})
}
func (s *Server) cacheBackendDelete(w http.ResponseWriter, r *http.Request) {
	ctx, cancel, _, ok := s.backendContext(w, r)
	if !ok {
		return
	}
	defer cancel()
	if err := s.Cache.DeleteItem(ctx, r.URL.Query().Get("key")); err != nil {
		httpError(w, 503, "Response cache deletion failed or key is outside the configured namespace")
		return
	}
	writeJSON(w, 200, map[string]any{"success": true})
}
func (s *Server) cacheBackendClear(w http.ResponseWriter, r *http.Request) {
	ctx, cancel, region, ok := s.backendContext(w, r)
	if !ok {
		return
	}
	defer cancel()
	n, err := s.Cache.Clear(ctx, region)
	if err != nil {
		writeJSON(w, 503, map[string]any{"success": false, "detail": "Response cache clear failed", "health": s.Cache.Health()})
		return
	}
	writeJSON(w, 200, map[string]any{"success": true, "deleted": n, "region": region, "scope": "configured_ephemeral_namespace"})
}
