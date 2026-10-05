// SPDX-License-Identifier: AGPL-3.0-only
package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

const cacheRegionSQL = "COALESCE(NULLIF(cache_provider,''),'default')"

// These SQL records are protocol state, not replaceable cached computations.
// They expire through the scheduled TTL cleanup, never a manual cache clear.
const disposableCacheSQL = "LOWER(" + cacheRegionSQL + ") NOT IN ('notification_events','notification_offsets','anidan_ephemeral_v1','anidan_ephemeral_meta_v1')" +
	" AND SUBSTR(LOWER(cache_key),1,15) NOT IN ('player_virtual_','control_search_','player_history_','player_command_')" +
	" AND SUBSTR(LOWER(cache_key),1,14)<>'player_search_' AND SUBSTR(LOWER(cache_key),1,13) NOT IN ('player_match_','notification-')"

func protectedCacheRecord(key, region string) bool {
	if strings.EqualFold(region, "notification_events") || strings.EqualFold(region, "notification_offsets") || strings.EqualFold(region, "anidan_ephemeral_v1") || strings.EqualFold(region, "anidan_ephemeral_meta_v1") {
		return true
	}
	for _, prefix := range []string{"player_virtual_", "control_search_", "player_history_", "player_command_", "player_search_", "player_match_", "notification-"} {
		if strings.HasPrefix(strings.ToLower(key), prefix) {
			return true
		}
	}
	return false
}

// Manual UI/player clear always applies the shared protocol-state exclusion,
// even when the caller supplied no filter. TTL maintenance is separate.
func (s *Server) clearDisposableCaches(ctx context.Context, where string, args ...any) (int64, error) {
	filter := "1=1"
	if where != "" {
		if !strings.HasPrefix(where, " WHERE ") {
			return 0, errors.New("invalid internal cache filter")
		}
		filter = strings.TrimPrefix(where, " WHERE ")
	}
	// Group the entire optional filter: a future OR clause must not weaken
	// protocol-state protection.
	where = " WHERE " + disposableCacheSQL + " AND (" + filter + ")"
	result, err := s.Store.DB.ExecContext(ctx, s.Store.Rebind("DELETE FROM cache_data"+where), args...)
	if err != nil {
		return 0, err
	}
	count, err := result.RowsAffected()
	if err != nil && !errors.Is(err, http.ErrNotSupported) {
		return 0, err
	}
	if err = s.clearRuntimeCaches(ctx); err != nil {
		return count, errors.New("SQL/local cache clearing completed, but the response cache backend could not be cleared")
	}
	return count, nil
}

func (s *Server) cacheWhere(r *http.Request) (string, []any) {
	where := " WHERE " + s.sqlWallTime("expires_at") + ">=? AND " + disposableCacheSQL
	args := []any{strings.ReplaceAll(s.now(), "T", " ")}
	if region := r.URL.Query().Get("region"); region != "" && region != "all" {
		where += " AND " + cacheRegionSQL + "=?"
		args = append(args, region)
	}
	if search := r.URL.Query().Get("search"); search != "" {
		switch s.Store.Dialect {
		case "postgres":
			where += " AND STRPOS(cache_key,?)>0"
		case "mysql":
			where += " AND LOCATE(CAST(? AS BINARY),CAST(cache_key AS BINARY))>0"
		default:
			where += " AND INSTR(cache_key,?)>0"
		}
		args = append(args, search)
	}
	return where, args
}
func (s *Server) cacheStats(w http.ResponseWriter, r *http.Request) {
	where, args := s.cacheWhere(r)
	rows, e := s.Store.DB.QueryContext(r.Context(), s.Store.Rebind("SELECT "+cacheRegionSQL+",COUNT(*) FROM cache_data"+where+" GROUP BY "+cacheRegionSQL+" LIMIT 1001"), args...)
	if e != nil {
		httpError(w, 503, e.Error())
		return
	}
	defer rows.Close()
	regions := map[string]int64{}
	var total int64
	for rows.Next() {
		var name string
		var n int64
		if e = rows.Scan(&name, &n); e != nil {
			httpError(w, 500, e.Error())
			return
		}
		regions[name] = n
		total += n
	}
	if e = rows.Err(); e != nil {
		httpError(w, 500, e.Error())
		return
	}
	if len(regions) > 1000 {
		httpError(w, 413, "Cache region count exceeds diagnostic limit")
		return
	}
	// Release SQL resources before runtime stats calls can query configuration.
	rows.Close()
	writeJSON(w, 200, map[string]any{"total": total, "regions": regions, "failedRegions": []any{}, "runtimeCaches": s.runtimeCacheStats()})
}
func (s *Server) cacheList(w http.ResponseWriter, r *http.Request) {
	where, args := s.cacheWhere(r)
	var total int64
	if e := s.Store.DB.QueryRowContext(r.Context(), s.Store.Rebind("SELECT COUNT(*) FROM cache_data"+where), args...).Scan(&total); e != nil {
		httpError(w, 503, e.Error())
		return
	}
	page, size := pageParams(r)
	offset := int64(page-1) * int64(size)
	if offset > total {
		offset = total
	}
	// Read only 201 characters per entry, never every cached document into RAM.
	query := "SELECT cache_key," + cacheRegionSQL + ",SUBSTR(cache_value,1,201) FROM cache_data" + where + fmt.Sprintf(" ORDER BY cache_key LIMIT %d OFFSET %d", size, offset)
	rows, e := s.Store.DB.QueryContext(r.Context(), s.Store.Rebind(query), args...)
	if e != nil {
		httpError(w, 503, e.Error())
		return
	}
	defer rows.Close()
	items := []any{}
	for rows.Next() {
		var key, region, preview string
		if e = rows.Scan(&key, &region, &preview); e != nil {
			httpError(w, 500, e.Error())
			return
		}
		runes := []rune(preview)
		if len(runes) > 200 {
			preview = string(runes[:200]) + "..."
		}
		items = append(items, map[string]any{"key": key, "region": region, "value_preview": preview})
	}
	if e = rows.Err(); e != nil {
		httpError(w, 500, e.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"total": total, "page": page, "pageSize": size, "region": r.URL.Query().Get("region"), "items": items})
}
func (s *Server) cacheDetail(w http.ResponseWriter, r *http.Request) {
	key := r.URL.Query().Get("key")
	var raw, region string
	// A bounded substring detects oversized values without materializing them.
	e := s.Store.DB.QueryRowContext(r.Context(), s.Store.Rebind("SELECT SUBSTR(cache_value,1,1048577),"+cacheRegionSQL+" FROM cache_data WHERE cache_key=? AND "+disposableCacheSQL), key).Scan(&raw, &region)
	if errors.Is(e, sql.ErrNoRows) {
		httpError(w, 404, "Cache key not found")
		return
	}
	if e != nil {
		httpError(w, 500, "Cache read failed")
		return
	}
	if len(raw) > 1<<20 {
		httpError(w, 413, "Cache detail exceeds the 1 MiB diagnostic response limit")
		return
	}
	var value any
	d := json.NewDecoder(strings.NewReader(raw))
	d.UseNumber()
	if d.Decode(&value) != nil {
		value = raw
	}
	writeJSON(w, 200, map[string]any{"key": key, "region": region, "value": value})
}
func (s *Server) cacheDelete(w http.ResponseWriter, r *http.Request) {
	key := r.URL.Query().Get("key")
	var region string
	if e := s.Store.DB.QueryRowContext(r.Context(), s.Store.Rebind("SELECT "+cacheRegionSQL+" FROM cache_data WHERE cache_key=?"), key).Scan(&region); e != nil && !errors.Is(e, sql.ErrNoRows) {
		httpError(w, 500, "Cache read failed")
		return
	}
	if protectedCacheRecord(key, region) {
		httpError(w, 403, "Delivery receipts, receive offsets and player protocol state are not disposable cache entries")
		return
	}
	if _, e := s.Store.DB.ExecContext(r.Context(), s.Store.Rebind("DELETE FROM cache_data WHERE cache_key=? AND "+disposableCacheSQL), key); e != nil {
		httpError(w, 500, e.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"success": true})
}
func (s *Server) cacheClear(w http.ResponseWriter, r *http.Request) {
	where, args := s.cacheWhere(r)
	n, e := s.clearDisposableCaches(r.Context(), where, args...)
	if e != nil {
		httpError(w, 500, e.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"success": true, "deleted": n, "runtimeCachesCleared": true})
}
