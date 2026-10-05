// SPDX-License-Identifier: AGPL-3.0-or-later
package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/AniBakaBaka/AniDan/internal/store"
)

func (s *Server) registerDiagnosticHealth(m *http.ServeMux) {
	for route, h := range map[string]http.HandlerFunc{
		"GET /api/ui/system-health/scraper-stats":         s.sourceHealth,
		"POST /api/ui/system-health/scraper-stats/reset":  s.sourceHealthReset,
		"GET /api/ui/system-health/summary":               s.healthSummary,
		"GET /api/ui/system-health/config-score":          s.configScore,
		"GET /api/ui/system-health/anime-priority":        s.animePriorityGet,
		"POST /api/ui/system-health/anime-priority":       s.animePrioritySet,
		"POST /api/ui/system-health/anime-priority/batch": s.animePrioritySet,
		"GET /api/ui/data-check/scan":                     s.dataCheck,
		"POST /api/ui/data-check/fix-orphans":             s.dataFixOrphans,
		"POST /api/ui/data-check/clear-mapping":           s.dataClearMapping,
		"GET /api/ui/rate-limit/status":                   s.providerRateLimitStatus,
	} {
		m.HandleFunc(route, s.operator(h))
	}
}
func roundDiagnostic(v float64) float64 { return math.Round(v*10) / 10 }
func healthScore(total, success, timeouts int64, duration float64) (int, string) {
	if total == 0 {
		return 100, "excellent"
	}
	score := 100 - int(math.Max(0, (1-float64(success)/float64(total))*60)) - int(math.Max(0, float64(timeouts)/float64(total)*20))
	if duration > 5000 {
		score -= 15
	} else if duration > 3000 {
		score -= 8
	} else if duration > 1500 {
		score -= 3
	}
	score = max(0, min(100, score))
	level := "bad"
	if score >= 80 {
		level = "excellent"
	} else if score >= 60 {
		level = "good"
	} else if score >= 40 {
		level = "unstable"
	}
	return score, level
}
func (s *Server) sourceHealthRows(ctx context.Context) ([]map[string]any, error) {
	rows, err := s.Store.List(ctx, "scrapers", nil, 1000, 0)
	if err != nil {
		return nil, err
	}
	out := []map[string]any{}
	for _, row := range rows {
		total, success := number(row["total_searches"]), number(row["success_count"])
		duration := float64(number(row["total_duration_ms"])) / float64(max(int64(1), total))
		count := float64(number(row["total_result_count"])) / float64(max(int64(1), success))
		score, level := healthScore(total, success, number(row["timeout_count"]), duration)
		out = append(out, map[string]any{"providerName": row["provider_name"], "displayName": row["provider_name"], "isEnabled": boolean(row["is_enabled"]), "totalSearches": total, "successCount": success, "failCount": number(row["fail_count"]), "timeoutCount": number(row["timeout_count"]), "emptyCount": number(row["empty_count"]), "avgDurationMs": roundDiagnostic(duration), "avgResultCount": roundDiagnostic(count), "healthScore": score, "healthLevel": level, "lastSearchAt": row["last_search_at"], "lastError": row["last_error"], "hasSamples": total > 0})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i]["healthScore"].(int) < out[j]["healthScore"].(int) })
	return out, nil
}
func (s *Server) sourceHealth(w http.ResponseWriter, r *http.Request) {
	items, err := s.sourceHealthRows(r.Context())
	if err != nil {
		httpError(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, items)
}
func (s *Server) sourceHealthReset(w http.ResponseWriter, r *http.Request) {
	_, err := s.Store.DB.ExecContext(r.Context(), "UPDATE scrapers SET total_searches=0,success_count=0,fail_count=0,timeout_count=0,empty_count=0,total_duration_ms=0,total_result_count=0,last_search_at=NULL,last_error=NULL")
	if err != nil {
		httpError(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]string{"message": "ok"})
}

// recordProviderSearch collects actual per-provider outcomes. It excludes queries,
// result titles, URLs and credentials from telemetry and error descriptions.
func (s *Server) recordProviderSearch(ctx context.Context, name string, duration time.Duration, count int, callErr error) {
	if len(name) > 500 {
		return
	}
	success, failed, timeout, empty := 0, 0, 0, 0
	var lastError any
	if callErr == nil {
		success = 1
		if count == 0 {
			empty = 1
		}
	} else {
		failed = 1
		lastError = "provider search failed"
		if errors.Is(callErr, context.DeadlineExceeded) {
			timeout = 1
			lastError = "provider search timed out"
		} else if errors.Is(callErr, context.Canceled) {
			lastError = "provider search cancelled"
		}
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	defer cancel()
	_, err := s.Store.DB.ExecContext(ctx, s.Store.Rebind("UPDATE scrapers SET total_searches=COALESCE(total_searches,0)+1,success_count=COALESCE(success_count,0)+?,fail_count=COALESCE(fail_count,0)+?,timeout_count=COALESCE(timeout_count,0)+?,empty_count=COALESCE(empty_count,0)+?,total_duration_ms=COALESCE(total_duration_ms,0)+?,total_result_count=COALESCE(total_result_count,0)+?,last_search_at=?,last_error=? WHERE provider_name=?"), success, failed, timeout, empty, max(int64(0), duration.Milliseconds()), max(0, count), s.now(), lastError, name)
	if err != nil {
		slog.Warn("provider statistics persistence failed", "provider", name, "error", err)
	}
}
func (s *Server) calculateConfigScore(ctx context.Context) (map[string]any, error) {
	items := []map[string]any{}
	total, maximum := 0, 0
	add := func(key, label string, score, weight int, configured bool, detail string) {
		total += score
		maximum += weight
		item := map[string]any{"key": key, "label": label, "configured": configured, "score": score, "maxScore": weight}
		if detail != "" {
			item["detail"] = detail
		}
		items = append(items, item)
	}
	for _, check := range []struct {
		key, setting, label string
		weight              int
	}{{"proxy", "proxyUrl", "代理配置", 10}, {"ai", "aiMatcherEnabled", "AI匹配", 10}, {"webhook", "webhookApiKey", "Webhook", 10}, {"danmaku_path", "danmakuBasePath", "弹幕输出路径", 15}} {
		fallback := ""
		if check.key == "webhook" {
			fallback = s.Config.WebhookAPIKey
		}
		if check.key == "danmaku_path" {
			fallback = filepath.Join(s.DataDir, "danmaku")
		}
		value := strings.TrimSpace(s.setting(ctx, check.setting, fallback))
		configured := value != ""
		if check.key == "ai" {
			configured = strings.EqualFold(value, "true")
		}
		score := 0
		if configured {
			score = check.weight
		}
		add(check.key, check.label, score, check.weight, configured, "")
	}
	for _, check := range []struct {
		key, table, label string
		weight            int
		multi             bool
	}{{"media_server", "media_servers", "媒体服务器", 15, false}, {"scrapers", "scrapers", "弹幕源", 15, true}, {"notification", "notification_channels", "通知渠道", 10, false}, {"backup", "scheduled_tasks", "定期备份", 10, false}, {"metadata", "metadata_sources", "元数据源", 15, true}} {
		filter := store.Row{"is_enabled": true}
		if check.key == "backup" {
			filter["job_type"] = "databaseBackup"
		}
		n, err := s.Store.Count(ctx, check.table, filter)
		if err != nil {
			return nil, err
		}
		score := 0
		if n > 0 {
			score = check.weight
			if check.multi && n == 1 {
				score = 8
			}
		}
		if check.key == "backup" && s.Jobs.RecoveryReviewRequired() {
			score = 0
		}
		add(check.key, check.label, score, check.weight, n > 0, fmt.Sprintf("%d enabled", n))
	}
	return map[string]any{"totalScore": total, "maxScore": maximum, "percentage": total * 100 / max(1, maximum), "items": items}, nil
}
func (s *Server) configScore(w http.ResponseWriter, r *http.Request) {
	out, err := s.calculateConfigScore(r.Context())
	if err != nil {
		httpError(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, out)
}
func (s *Server) healthSummary(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	sources, err := s.sourceHealthRows(ctx)
	if err != nil {
		httpError(w, 500, err.Error())
		return
	}
	enabled, unhealthy := 0, 0
	for _, v := range sources {
		if v["isEnabled"] == true {
			enabled++
		}
		if v["healthScore"].(int) < 60 {
			unhealthy++
		}
	}
	loc, _ := time.LoadLocation(s.Config.Timezone)
	now := time.Now().In(loc)
	cutoff := now.Add(-24 * time.Hour).Format("2006-01-02 15:04:05")
	rows, err := s.Store.DB.QueryContext(ctx, s.Store.Rebind("SELECT status,COUNT(*) FROM task_history WHERE "+s.sqlWallTime("created_at")+">=? GROUP BY status"), cutoff)
	if err != nil {
		httpError(w, 500, err.Error())
		return
	}
	tasks := map[string]int64{}
	for rows.Next() {
		var status string
		var n int64
		if err = rows.Scan(&status, &n); err != nil {
			rows.Close()
			httpError(w, 500, err.Error())
			return
		}
		tasks[status] = n
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		httpError(w, 500, err.Error())
		return
	}
	var missing, today int64
	if err = s.Store.DB.QueryRowContext(ctx, "SELECT COUNT(*) FROM episode WHERE comment_count=0").Scan(&missing); err != nil {
		httpError(w, 500, err.Error())
		return
	}
	start := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc).Format("2006-01-02 15:04:05")
	if err = s.Store.DB.QueryRowContext(ctx, s.Store.Rebind("SELECT COUNT(*) FROM episode WHERE "+s.sqlWallTime("fetched_at")+">=?"), start).Scan(&today); err != nil {
		httpError(w, 500, err.Error())
		return
	}
	backups := map[string]any{}
	if dir, e := s.backupDirectory(); e == nil {
		entries, e := os.ReadDir(dir)
		if e != nil {
			backups["error"] = e.Error()
		} else {
			n := 0
			var newest os.FileInfo
			for _, entry := range entries {
				if !backupFilenameRE.MatchString(entry.Name()) || !entry.Type().IsRegular() {
					continue
				}
				st, e := entry.Info()
				if e != nil {
					continue
				}
				n++
				if newest == nil || st.ModTime().After(newest.ModTime()) {
					newest = st
				}
			}
			backups["totalBackups"] = n
			if newest != nil {
				backups["lastBackup"] = newest.ModTime().Format(time.RFC3339)
				backups["latestSize"] = newest.Size()
			}
		}
	} else {
		backups["error"] = e.Error()
	}
	score, err := s.calculateConfigScore(ctx)
	if err != nil {
		httpError(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"scraperSummary": map[string]int{"enabled": enabled, "total": len(sources), "unhealthy": unhealthy}, "taskSummary": tasks, "backupStatus": backups, "missingEpisodes": missing, "todayNewDanmaku": today, "configScore": score["percentage"], "recoveryReviewRequired": s.Jobs.RecoveryReviewRequired()})
}
func (s *Server) animePriorityGet(w http.ResponseWriter, r *http.Request) {
	raw := s.setting(r.Context(), "anime_priority_map", "{}")
	if len(raw) > 1<<20 {
		httpError(w, 413, "priority configuration too large")
		return
	}
	var value map[string]string
	if err := json.Unmarshal([]byte(raw), &value); err != nil {
		httpError(w, 500, "Invalid stored priority configuration")
		return
	}
	writeJSON(w, 200, value)
}
func (s *Server) animePrioritySet(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ID       int64   `json:"animeId"`
		IDs      []int64 `json:"animeIds"`
		Priority string  `json:"priority"`
	}
	if err := readJSON(r, &in); err != nil {
		httpError(w, 400, err.Error())
		return
	}
	if in.Priority != "high" && in.Priority != "normal" && in.Priority != "ignore" {
		httpError(w, 400, "priority must be high, normal or ignore")
		return
	}
	if in.ID > 0 {
		in.IDs = append(in.IDs, in.ID)
	}
	if len(in.IDs) < 1 || len(in.IDs) > 1000 {
		httpError(w, 400, "Provide 1..1000 anime IDs")
		return
	}
	err := s.mutateDiagnosticConfig(r.Context(), "anime_priority_map", "{}", func(raw string) (string, error) {
		var value map[string]string
		if err := json.Unmarshal([]byte(raw), &value); err != nil {
			return "", err
		}
		for _, id := range in.IDs {
			if id < 1 {
				return "", errors.New("invalid anime ID")
			}
			if in.Priority == "normal" {
				delete(value, strconv.FormatInt(id, 10))
			} else {
				value[strconv.FormatInt(id, 10)] = in.Priority
			}
		}
		out, err := json.Marshal(value)
		return string(out), err
	})
	if err != nil {
		httpError(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"message": "ok", "count": len(in.IDs)})
}
func (s *Server) dataCheck(w http.ResponseWriter, r *http.Request) {
	limit, err := queryInt(r, "limit", 50, 200)
	if err != nil || limit < 1 {
		httpError(w, 400, "limit must be 1..200")
		return
	}
	out := []map[string]any{}
	checks := []struct {
		category, severity, query, suggestion string
		fields                                []string
	}{
		{"missing_metadata", "warning", "SELECT a.id,a.title,a.season FROM anime a LEFT JOIN anime_metadata m ON m.anime_id=a.id WHERE m.id IS NULL OR (m.tmdb_id IS NULL AND m.bangumi_id IS NULL)", "Add metadata IDs or rematch these titles", []string{"id", "title", "season"}},
		{"zero_danmaku", "warning", "SELECT id,title,episode_index,source_id FROM episode WHERE comment_count=0", "Refresh these episodes or verify that their source has comments", []string{"id", "title", "episodeIndex", "sourceId"}},
		{"orphan_episodes", "error", "SELECT e.id,e.title,e.source_id FROM episode e LEFT JOIN anime_sources s ON s.id=e.source_id WHERE s.id IS NULL", "Remove orphan database rows; file deletion is a separate explicit operation", []string{"id", "title", "sourceId"}},
		{"duplicate_anime", "warning", "SELECT title,season,COUNT(*) AS count FROM anime GROUP BY title,season HAVING COUNT(*)>1", "Review and merge duplicate title/season records", []string{"title", "season", "count"}},
		{"broken_mapping", "warning", "SELECT anime_id,media_server_type FROM anime_metadata WHERE media_server_type IS NOT NULL AND media_server_series_id IS NULL", "Review or clear incomplete media-server mappings", []string{"animeId", "serverType"}},
	}
	for _, check := range checks {
		var count int64
		if err = s.Store.DB.QueryRowContext(r.Context(), "SELECT COUNT(*) FROM ("+check.query+") diagnostic_count").Scan(&count); err != nil {
			httpError(w, 500, err.Error())
			return
		}
		if count == 0 {
			continue
		}
		rows, err := s.Store.DB.QueryContext(r.Context(), s.Store.Rebind(check.query+" LIMIT ?"), limit)
		if err != nil {
			httpError(w, 500, err.Error())
			return
		}
		items := []map[string]any{}
		for rows.Next() {
			values := make([]any, len(check.fields))
			ptrs := make([]any, len(values))
			for i := range values {
				ptrs[i] = &values[i]
			}
			if err = rows.Scan(ptrs...); err != nil {
				rows.Close()
				httpError(w, 500, err.Error())
				return
			}
			item := map[string]any{}
			for i, key := range check.fields {
				if v, ok := values[i].([]byte); ok {
					item[key] = string(v)
				} else {
					item[key] = values[i]
				}
			}
			items = append(items, item)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			httpError(w, 500, err.Error())
			return
		}
		out = append(out, map[string]any{"category": check.category, "severity": check.severity, "count": count, "items": items, "suggestion": check.suggestion})
	}
	writeJSON(w, 200, out)
}
func (s *Server) dataFixOrphans(w http.ResponseWriter, r *http.Request) {
	res, err := s.Store.DB.ExecContext(r.Context(), "DELETE FROM episode WHERE NOT EXISTS (SELECT 1 FROM anime_sources WHERE anime_sources.id=episode.source_id)")
	if err != nil {
		httpError(w, 500, err.Error())
		return
	}
	n, err := res.RowsAffected()
	if err != nil {
		httpError(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"message": "ok", "deleted": n, "filesDeleted": 0})
}
func (s *Server) dataClearMapping(w http.ResponseWriter, r *http.Request) {
	res, err := s.Store.DB.ExecContext(r.Context(), "UPDATE anime_metadata SET media_server_type=NULL,media_server_series_id=NULL,media_server_season_id=NULL WHERE media_server_type IS NOT NULL AND media_server_series_id IS NULL")
	if err != nil {
		httpError(w, 500, err.Error())
		return
	}
	n, err := res.RowsAffected()
	if err != nil {
		httpError(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"message": "ok", "fixed": n})
}

// mutateDiagnosticConfig serializes read/modify/write of bounded JSON config.
func (s *Server) mutateDiagnosticConfig(ctx context.Context, key, initial string, change func(string) (string, error)) error {
	for attempt := 0; attempt < 5; attempt++ {
		tx, err := s.Store.DB.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		query := "SELECT config_value FROM config WHERE config_key=?"
		if s.Store.Dialect != "sqlite" {
			query += " FOR UPDATE"
		}
		var raw string
		err = tx.QueryRowContext(ctx, s.Store.Rebind(query), key).Scan(&raw)
		missing := errors.Is(err, sql.ErrNoRows)
		if err != nil && !missing {
			tx.Rollback()
			return err
		}
		if missing || raw == "" {
			raw = initial
		}
		if len(raw) > 4<<20 {
			tx.Rollback()
			return errors.New("diagnostic configuration exceeds safety bound")
		}
		value, err := change(raw)
		if err != nil {
			tx.Rollback()
			return err
		}
		if len(value) > 4<<20 {
			tx.Rollback()
			return errors.New("updated diagnostic configuration exceeds safety bound")
		}
		if missing {
			q := "INSERT INTO config (config_key,config_value,description) VALUES (?,?,?) ON CONFLICT (config_key) DO NOTHING"
			if s.Store.Dialect == "mysql" {
				q = "INSERT IGNORE INTO config (config_key,config_value,description) VALUES (?,?,?)"
			}
			res, e := tx.ExecContext(ctx, s.Store.Rebind(q), key, value, "Diagnostics state")
			err = e
			if err == nil {
				n, e := res.RowsAffected()
				if e != nil {
					err = e
				} else if n == 0 {
					tx.Rollback()
					continue
				}
			}
		} else {
			_, err = tx.ExecContext(ctx, s.Store.Rebind("UPDATE config SET config_value=? WHERE config_key=?"), value, key)
		}
		if err != nil {
			tx.Rollback()
			return err
		}
		return tx.Commit()
	}
	return errors.New("concurrent configuration updates did not settle")
}
