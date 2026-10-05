// SPDX-License-Identifier: AGPL-3.0-or-later
package server

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"math"
	"net/http"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/AniBakaBaka/AniDan/internal/job"
)

var capacitySamplers sync.Map

func (s *Server) registerDiagnosticMetrics(m *http.ServeMux) {
	m.HandleFunc("GET /api/ui/task-profile/summary", s.operator(s.taskProfiles))
	m.HandleFunc("GET /api/ui/task-profile/timeline", s.operator(s.taskTimeline))
	m.HandleFunc("GET /api/ui/perf/stats", s.operator(s.performanceStats))
	m.HandleFunc("GET /api/ui/trends/current", s.operator(s.capacityCurrent))
	m.HandleFunc("GET /api/ui/trends/capacity", s.operator(s.capacityTrends))
	s.startCapacitySampler()
}
func metricSuccess(status string) bool {
	return status == job.Completed || status == "成功" || status == "completed" || status == "success"
}
func metricFailed(status string) bool {
	return status == job.Failed || status == "failed" || status == "error" || status == "已取消"
}
func (s *Server) metricCutoff(r *http.Request) (string, error) {
	days, err := queryInt(r, "days", 7, 90)
	if err != nil || days < 1 {
		return "", errors.New("days must be 1..90")
	}
	loc, _ := time.LoadLocation(s.Config.Timezone)
	return time.Now().In(loc).AddDate(0, 0, -days).Format("2006-01-02 15:04:05"), nil
}
func (s *Server) metricDuration(created, finished any) (float64, error) {
	if created == nil || finished == nil {
		return 0, nil
	}
	a, err := s.authDate(created)
	if err != nil {
		return 0, err
	}
	b, err := s.authDate(finished)
	if err != nil {
		return 0, err
	}
	return math.Max(0, b.Sub(a).Seconds()), nil
}
func (s *Server) taskProfiles(w http.ResponseWriter, r *http.Request) {
	cutoff, err := s.metricCutoff(r)
	if err != nil {
		httpError(w, 400, err.Error())
		return
	}
	rows, err := s.Store.DB.QueryContext(r.Context(), s.Store.Rebind("SELECT title,status,created_at,finished_at FROM task_history WHERE "+s.sqlWallTime("created_at")+">=? ORDER BY created_at DESC LIMIT 100001"), cutoff)
	if err != nil {
		httpError(w, 500, err.Error())
		return
	}
	defer rows.Close()
	type profile struct {
		Title                             string
		Total, Success, Failed, Durations int
		Sum, Max                          float64
		Recent                            []map[string]any
	}
	groups := map[string]*profile{}
	n := 0
	for rows.Next() {
		n++
		if n > 100000 {
			httpError(w, 413, "Reduce the time window; task profile exceeds 100000 rows")
			return
		}
		var title, status string
		var created, finished any
		if err = rows.Scan(&title, &status, &created, &finished); err != nil {
			httpError(w, 500, err.Error())
			return
		}
		dur, err := s.metricDuration(created, finished)
		if err != nil {
			httpError(w, 500, "Invalid stored task time")
			return
		}
		p := groups[title]
		if p == nil {
			p = &profile{Title: title, Recent: []map[string]any{}}
			groups[title] = p
		}
		p.Total++
		if metricSuccess(status) {
			p.Success++
		}
		if metricFailed(status) {
			p.Failed++
		}
		if finished != nil {
			p.Durations++
			p.Sum += dur
			p.Max = math.Max(p.Max, dur)
		}
		if len(p.Recent) < 10 {
			p.Recent = append(p.Recent, map[string]any{"status": status, "createdAt": str(created), "finishedAt": str(finished), "durationSec": roundDiagnostic(dur)})
		}
	}
	if err = rows.Err(); err != nil {
		httpError(w, 500, err.Error())
		return
	}
	out := []map[string]any{}
	for _, p := range groups {
		out = append(out, map[string]any{"jobType": p.Title, "totalRuns": p.Total, "successCount": p.Success, "failCount": p.Failed, "avgDurationSec": roundDiagnostic(p.Sum / float64(max(1, p.Durations))), "maxDurationSec": roundDiagnostic(p.Max), "successRate": roundDiagnostic(float64(p.Success) * 100 / float64(max(1, p.Total))), "recentRuns": p.Recent})
	}
	sort.Slice(out, func(i, j int) bool { return out[i]["totalRuns"].(int) > out[j]["totalRuns"].(int) })
	writeJSON(w, 200, out)
}
func (s *Server) taskTimeline(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("task_id")
	if id == "" {
		httpError(w, 400, "task_id is required")
		return
	}
	task, err := s.Jobs.GetContext(r.Context(), id)
	if err != nil {
		jobHTTPError(w, err)
		return
	}
	steps := []map[string]any{}
	rows, err := s.Store.DB.QueryContext(r.Context(), s.Store.Rebind("SELECT step_name,duration_ms,success,details,created_at FROM task_perf_events WHERE correlation_id=? ORDER BY created_at,id LIMIT 1001"), id)
	if err != nil {
		httpError(w, 500, err.Error())
		return
	}
	defer rows.Close()
	for rows.Next() {
		if len(steps) >= 1000 {
			httpError(w, 413, "Task has more than 1000 measured steps")
			return
		}
		var name string
		var duration float64
		var success, details, created any
		if err = rows.Scan(&name, &duration, &success, &details, &created); err != nil {
			httpError(w, 500, err.Error())
			return
		}
		steps = append(steps, map[string]any{"stepName": name, "durationMs": duration, "success": boolean(success), "details": str(details), "createdAt": str(created)})
	}
	if err = rows.Err(); err != nil {
		httpError(w, 500, err.Error())
		return
	}
	duration := 0.0
	if task.FinishedAt != nil {
		duration = math.Max(0, task.FinishedAt.Sub(task.CreatedAt).Seconds())
	}
	writeJSON(w, 200, map[string]any{"taskId": task.ID, "title": task.Title, "status": task.Status, "createdAt": task.CreatedAt, "finishedAt": task.FinishedAt, "durationSec": duration, "steps": steps, "stepsAvailable": len(steps) > 0})
}
func percentile(values []float64, p float64) float64 {
	if len(values) == 0 {
		return 0
	}
	sort.Float64s(values)
	index := p / 100 * float64(len(values)-1)
	lo := int(index)
	hi := min(lo+1, len(values)-1)
	return roundDiagnostic(values[lo] + (values[hi]-values[lo])*(index-float64(lo)))
}
func (s *Server) performanceStats(w http.ResponseWriter, r *http.Request) {
	cutoff, err := s.metricCutoff(r)
	if err != nil {
		httpError(w, 400, err.Error())
		return
	}
	rows, err := s.Store.DB.QueryContext(r.Context(), s.Store.Rebind("SELECT flow_type,correlation_id,step_name,duration_ms,success,total_duration_ms FROM task_perf_events WHERE "+s.sqlWallTime("created_at")+">=? LIMIT 100001"), cutoff)
	if err != nil {
		httpError(w, 500, err.Error())
		return
	}
	defer rows.Close()
	type step struct {
		Values  []float64
		Success int
	}
	type flow struct {
		Runs  map[string]float64
		Steps map[string]*step
	}
	flows := map[string]*flow{}
	n := 0
	for rows.Next() {
		n++
		if n > 100000 {
			httpError(w, 413, "Reduce the time window; performance data exceeds 100000 events")
			return
		}
		var f, id, name string
		var duration float64
		var successful, total any
		if err = rows.Scan(&f, &id, &name, &duration, &successful, &total); err != nil {
			httpError(w, 500, err.Error())
			return
		}
		item := flows[f]
		if item == nil {
			item = &flow{Runs: map[string]float64{}, Steps: map[string]*step{}}
			flows[f] = item
		}
		st := item.Steps[name]
		if st == nil {
			st = &step{}
			item.Steps[name] = st
		}
		st.Values = append(st.Values, duration)
		if boolean(successful) {
			st.Success++
		}
		t := duration
		if total != nil {
			t, _ = strconv.ParseFloat(str(total), 64)
		}
		item.Runs[id] = math.Max(item.Runs[id], t)
	}
	if err = rows.Err(); err != nil {
		httpError(w, 500, err.Error())
		return
	}
	out := []map[string]any{}
	for name, f := range flows {
		steps := []map[string]any{}
		for key, v := range f.Steps {
			sum, maximum := 0.0, 0.0
			for _, d := range v.Values {
				sum += d
				maximum = math.Max(maximum, d)
			}
			steps = append(steps, map[string]any{"stepName": key, "avgMs": roundDiagnostic(sum / float64(len(v.Values))), "maxMs": roundDiagnostic(maximum), "callCount": len(v.Values), "successRate": roundDiagnostic(float64(v.Success) * 100 / float64(len(v.Values))), "p50Ms": percentile(v.Values, 50), "p95Ms": percentile(v.Values, 95), "p99Ms": percentile(v.Values, 99)})
		}
		sort.Slice(steps, func(i, j int) bool { return steps[i]["stepName"].(string) < steps[j]["stepName"].(string) })
		sum := 0.0
		durations := []float64{}
		for _, v := range f.Runs {
			sum += v
			durations = append(durations, v)
		}
		out = append(out, map[string]any{"flowType": name, "totalRuns": len(f.Runs), "avgTotalMs": roundDiagnostic(sum / float64(max(1, len(f.Runs)))), "steps": steps, "p50TotalMs": percentile(durations, 50), "p95TotalMs": percentile(durations, 95), "p99TotalMs": percentile(durations, 99)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i]["flowType"].(string) < out[j]["flowType"].(string) })
	writeJSON(w, 200, out)
}
func (s *Server) currentCapacity(ctx context.Context) (map[string]any, error) {
	counts := map[string]int64{}
	for _, name := range []string{"anime", "episode", "anime_sources", "task_history", "cache_data", "media_items"} {
		n, err := s.Store.Count(ctx, name, nil)
		if err != nil {
			return nil, err
		}
		counts[name] = n
	}
	var size int64
	var err error
	switch s.Store.Dialect {
	case "sqlite":
		var pages, pageSize int64
		err = s.Store.DB.QueryRowContext(ctx, "PRAGMA page_count").Scan(&pages)
		if err == nil {
			err = s.Store.DB.QueryRowContext(ctx, "PRAGMA page_size").Scan(&pageSize)
			size = pages * pageSize
		}
	case "postgres":
		err = s.Store.DB.QueryRowContext(ctx, "SELECT pg_database_size(current_database())").Scan(&size)
	case "mysql":
		err = s.Store.DB.QueryRowContext(ctx, "SELECT COALESCE(SUM(data_length+index_length),0) FROM information_schema.tables WHERE table_schema=DATABASE()").Scan(&size)
	default:
		err = errors.New("unsupported database size query")
	}
	if err != nil {
		return nil, err
	}
	return map[string]any{"tableCounts": counts, "dbSizeBytes": size, "timestamp": s.now(), "sizeScope": "database pages; filesystem WAL and external file roots excluded"}, nil
}
func (s *Server) capacityCurrent(w http.ResponseWriter, r *http.Request) {
	snapshot, err := s.currentCapacity(r.Context())
	if err != nil {
		httpError(w, 503, err.Error())
		return
	}
	writeJSON(w, 200, snapshot)
}
func (s *Server) captureCapacity(ctx context.Context) error {
	snapshot, err := s.currentCapacity(ctx)
	if err != nil {
		return err
	}
	return s.mutateDiagnosticConfig(ctx, "capacity_trend_data", "[]", func(raw string) (string, error) {
		var history []map[string]any
		if err := json.Unmarshal([]byte(raw), &history); err != nil {
			return "", err
		}
		history = append(history, snapshot)
		if len(history) > 2880 {
			history = history[len(history)-2880:]
		}
		encoded, err := json.Marshal(history)
		return string(encoded), err
	})
}
func (s *Server) startCapacitySampler() {
	if s.ctx == nil {
		return
	}
	if _, exists := capacitySamplers.LoadOrStore(s, true); exists {
		return
	}
	s.capacityDone = make(chan struct{})
	go func() {
		defer close(s.capacityDone)
		defer capacitySamplers.Delete(s)
		ticker := time.NewTicker(15 * time.Minute)
		defer ticker.Stop()
		for {
			ctx, cancel := context.WithTimeout(s.ctx, 5*time.Second)
			err := s.captureCapacity(ctx)
			cancel()
			if err != nil && s.ctx.Err() == nil {
				slog.Warn("capacity snapshot failed", "error", err)
			}
			select {
			case <-s.ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
}
func (s *Server) capacityTrends(w http.ResponseWriter, r *http.Request) {
	row, err := s.Store.Get(r.Context(), "config", "capacity_trend_data")
	if err != nil || row == nil {
		httpError(w, 503, "Capacity sampler has not recorded a snapshot yet")
		return
	}
	raw := str(row["config_value"])
	if len(raw) > 4<<20 {
		httpError(w, 413, "capacity history exceeds safety bound")
		return
	}
	var data []map[string]any
	if err = json.Unmarshal([]byte(raw), &data); err != nil {
		httpError(w, 500, "Invalid persisted capacity history")
		return
	}
	writeJSON(w, 200, data)
}
