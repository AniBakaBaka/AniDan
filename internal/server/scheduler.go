// SPDX-License-Identifier: AGPL-3.0-or-later
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/AniBakaBaka/AniDan/internal/job"
)

func (s *Server) registerScheduler(m *http.ServeMux) {
	m.HandleFunc("GET /api/ui/scheduled-tasks/available-jobs", s.operator(s.availableJobs))
	m.HandleFunc("POST /api/ui/tasks/recovery/approve", s.operator(s.approveRestoredJobs))
	m.HandleFunc("GET /api/ui/tasks/recovery/status", s.operator(s.restoredJobsStatus))
	m.HandleFunc("GET /api/ui/scheduled-tasks", s.operator(s.schedulesList))
	m.HandleFunc("POST /api/ui/scheduled-tasks", s.operator(s.scheduleCreate))
	m.HandleFunc("PUT /api/ui/scheduled-tasks/{taskId}", s.operator(s.scheduleUpdate))
	m.HandleFunc("DELETE /api/ui/scheduled-tasks/{taskId}", s.operator(s.scheduleDelete))
	m.HandleFunc("POST /api/ui/scheduled-tasks/{taskId}/run", s.operator(s.scheduleRun))
	m.HandleFunc("GET /api/control/scheduler/tasks", s.operator(s.schedulesList))
	m.HandleFunc("GET /api/control/scheduler/{taskId}/last_result", s.operator(s.scheduleLastResult))
}
func jobHTTPError(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	switch {
	case errors.Is(err, job.ErrNotFound):
		status = 404
	case errors.Is(err, job.ErrQueueFull), errors.Is(err, job.ErrClosed):
		status = 503
	case errors.Is(err, job.ErrAlreadyRunning), errors.Is(err, job.ErrInvalidState), errors.Is(err, job.ErrNotRetryable), errors.Is(err, job.ErrRecoveryReview):
		status = 409
	case errors.Is(err, job.ErrUnknownHandler):
		status = 422
	}
	httpError(w, status, err.Error())
}
func (s *Server) availableJobs(w http.ResponseWriter, r *http.Request) {
	out := []map[string]any{}
	for _, kind := range s.Jobs.HandlerKinds() {
		name, description := kind, "Registered Go job handler"
		switch kind {
		case "databaseBackup":
			name = "完整数据备份"
			description = "Snapshot all legacy SQL tables and copy referenced XML, image and NFO files with SHA-256 verification"
		case "databaseMaintenance":
			name = "缓存日志清理任务"
			description = "Clean expired database cache, prune terminal job/access/metrics history, and refresh database statistics; does not purge MySQL binlogs or delete media files"
		case "cacheCleanup":
			name = "过期缓存清理"
			description = "Delete expired application database cache entries"
		}
		out = append(out, map[string]any{"jobType": kind, "name": name, "name_en": name, "name_tw": name, "description": description, "description_en": description, "description_tw": description, "isSystemTask": false, "configSchema": []any{}})
	}
	writeJSON(w, 200, out)
}
func (s *Server) schedulesList(w http.ResponseWriter, r *http.Request) {
	items, err := s.Jobs.Schedules()
	if err != nil {
		jobHTTPError(w, err)
		return
	}
	writeJSON(w, 200, items)
}
func (s *Server) scheduleCreate(w http.ResponseWriter, r *http.Request) {
	in := job.ScheduledTask{IsEnabled: true}
	if err := readJSON(r, &in); err != nil {
		httpError(w, 400, err.Error())
		return
	}
	if in.ID != "" {
		httpError(w, 400, "taskId is assigned by the server")
		return
	}
	// Upstream creation defaults isEnabled to true when omitted.
	result, err := s.Jobs.SaveSchedule(in)
	if err != nil {
		if errors.Is(err, job.ErrUnknownHandler) {
			jobHTTPError(w, err)
		} else {
			httpError(w, 400, err.Error())
		}
		return
	}
	writeJSON(w, 201, result)
}
func (s *Server) scheduleUpdate(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("taskId")
	existing, err := s.Jobs.Schedule(id)
	if err != nil {
		jobHTTPError(w, err)
		return
	}
	var in struct {
		Name           string          `json:"name"`
		CronExpression string          `json:"cronExpression"`
		IsEnabled      bool            `json:"isEnabled"`
		TaskConfig     json.RawMessage `json:"taskConfig"`
	}
	if err = readJSON(r, &in); err != nil {
		httpError(w, 400, err.Error())
		return
	}
	if existing.IsSystemTask {
		existing.IsEnabled = in.IsEnabled
	} else {
		existing.Name = in.Name
		existing.CronExpression = in.CronExpression
		existing.IsEnabled = in.IsEnabled
		existing.TaskConfig = in.TaskConfig
	}
	updated, err := s.Jobs.SaveSchedule(existing)
	if err != nil {
		httpError(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, updated)
}
func (s *Server) scheduleDelete(w http.ResponseWriter, r *http.Request) {
	item, err := s.Jobs.Schedule(r.PathValue("taskId"))
	if err != nil {
		jobHTTPError(w, err)
		return
	}
	if item.IsSystemTask {
		httpError(w, 400, "System tasks cannot be deleted")
		return
	}
	if err = s.Jobs.DeleteSchedule(item.ID); err != nil {
		jobHTTPError(w, err)
		return
	}
	w.WriteHeader(204)
}
func (s *Server) scheduleRun(w http.ResponseWriter, r *http.Request) {
	id, err := s.Jobs.RunSchedule(r.PathValue("taskId"))
	if err != nil {
		jobHTTPError(w, err)
		return
	}
	writeJSON(w, 202, map[string]any{"message": "任务已触发运行", "taskId": id})
}
func (s *Server) scheduleLastResult(w http.ResponseWriter, r *http.Request) {
	var id string
	err := s.Store.DB.QueryRowContext(r.Context(), s.Store.Rebind("SELECT id FROM task_history WHERE scheduled_task_id = ? ORDER BY created_at DESC, id DESC LIMIT 1"), r.PathValue("taskId")).Scan(&id)
	if err != nil {
		httpError(w, 404, "No execution history for this schedule")
		return
	}
	task, err := s.Jobs.GetContext(r.Context(), id)
	if err != nil {
		jobHTTPError(w, err)
		return
	}
	writeJSON(w, 200, task)
}

// initJobHandlers registers real Go implementations, with no success placeholders.
// Server.New calls this after the domain-specific import/fetch handlers exist.
func (s *Server) initJobHandlers() error {
	handlers := map[string]job.Handler{
		"cacheCleanup":        s.runCacheCleanup,
		"databaseMaintenance": s.runDatabaseMaintenance,
		"databaseBackup": func(ctx context.Context, params json.RawMessage, p func(int, string)) (any, error) {
			return s.createFullBackup(ctx, p)
		},
	}
	for _, kind := range []string{"cacheCleanup", "databaseMaintenance", "databaseBackup"} {
		if err := s.Jobs.Register(kind, handlers[kind]); err != nil {
			return err
		}
	}
	return nil
}
func (s *Server) runCacheCleanup(ctx context.Context, _ json.RawMessage, p func(int, string)) (any, error) {
	p(10, "Deleting expired database cache")
	if err := job.Checkpoint(ctx); err != nil {
		return nil, err
	}
	res, err := s.Store.DB.ExecContext(ctx, s.Store.Rebind("DELETE FROM cache_data WHERE "+s.sqlWallTime("expires_at")+" <= ? AND LOWER("+cacheRegionSQL+") NOT IN ('anidan_ephemeral_v1','anidan_ephemeral_meta_v1')"), strings.ReplaceAll(s.now(), "T", " "))
	if err != nil {
		return nil, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return nil, err
	}
	p(99, "Expired cache cleanup complete")
	return map[string]any{"deleted": n}, nil
}
func (s *Server) runDatabaseMaintenance(ctx context.Context, params json.RawMessage, p func(int, string)) (any, error) {
	var opts struct {
		RetentionDays *int `json:"retentionDays"`
	}
	if len(params) > 0 && string(params) != "null" {
		if err := json.Unmarshal(params, &opts); err != nil {
			return nil, err
		}
	}
	days, err := strconv.Atoi(s.setting(ctx, "logRetentionDays", "30"))
	if err != nil {
		return nil, errors.New("logRetentionDays must be an integer")
	}
	if opts.RetentionDays != nil {
		days = *opts.RetentionDays
	}
	if days < 0 || days > 3650 {
		return nil, errors.New("retentionDays must be 0..3650")
	}
	result := map[string]any{}
	cache, err := s.runCacheCleanup(ctx, nil, func(int, string) {})
	if err != nil {
		return nil, err
	}
	result["cache"] = cache
	if days > 0 {
		loc, _ := time.LoadLocation(s.Config.Timezone)
		cutoff := time.Now().In(loc).AddDate(0, 0, -days).Format("2006-01-02 15:04:05")
		tables := []struct{ name, column string }{{"token_access_logs", "access_time"}, {"external_api_logs", "access_time"}, {"ai_metrics_log", "timestamp"}, {"task_perf_events", "created_at"}, {"task_history", "created_at"}}
		for i, t := range tables {
			p(20+i*12, "Pruning "+t.name)
			if err = job.Checkpoint(ctx); err != nil {
				return nil, err
			}
			// Preserve every active task regardless of age. Results and state-cache rows
			// are deleted transactionally with terminal history, not left orphaned.
			tx, e := s.Store.DB.BeginTx(ctx, nil)
			if e != nil {
				return nil, e
			}
			where := s.sqlWallTime(t.column) + " < ?"
			args := []any{cutoff}
			if t.name == "task_history" {
				where += " AND status IN (?, ?, ?, ?)"
				args = append(args, job.Completed, job.Failed, "成功", "已取消")
				for _, prefix := range []string{"anidan.job.result.", job.ParentMetadataPrefix} {
					q := "DELETE FROM config WHERE config_key IN (SELECT "
					if s.Store.Dialect == "mysql" {
						q += "CONCAT('" + prefix + "', id)"
					} else {
						q += "'" + prefix + "' || id"
					}
					q += " FROM task_history WHERE " + where + ")"
					if _, e = tx.ExecContext(ctx, s.Store.Rebind(q), args...); e != nil {
						break
					}
				}
				if e == nil {
					_, e = tx.ExecContext(ctx, s.Store.Rebind("DELETE FROM task_state_cache WHERE task_id IN (SELECT id FROM task_history WHERE "+where+")"), args...)
				}
			}
			var n int64
			if e == nil {
				var res interface{ RowsAffected() (int64, error) }
				res, e = tx.ExecContext(ctx, s.Store.Rebind("DELETE FROM "+s.Store.Quote(t.name)+" WHERE "+where), args...)
				if e == nil {
					n, e = res.RowsAffected()
				}
			}
			if e != nil {
				tx.Rollback()
				return nil, e
			}
			if e = tx.Commit(); e != nil {
				return nil, e
			}
			result[t.name] = n
		}
	}
	p(90, "Refreshing database statistics")
	if err = job.Checkpoint(ctx); err != nil {
		return nil, err
	}
	switch s.Store.Dialect {
	case "sqlite":
		_, err = s.Store.DB.ExecContext(ctx, "PRAGMA optimize")
	case "postgres":
		_, err = s.Store.DB.ExecContext(ctx, "ANALYZE")
	case "mysql":
		for _, table := range []string{"task_history", "token_access_logs", "external_api_logs"} {
			if _, err = s.Store.DB.ExecContext(ctx, "ANALYZE TABLE "+s.Store.Quote(table)); err != nil {
				break
			}
		}
	default:
		err = fmt.Errorf("unsupported maintenance dialect %q", s.Store.Dialect)
	}
	if err != nil {
		return nil, err
	}
	result["retentionDays"] = days
	result["mediaFilesDeleted"] = 0
	result["binlogsPurged"] = false
	p(99, "Maintenance complete")
	return result, nil
}

func (s *Server) sqlWallTime(column string) string {
	quoted := s.Store.Quote(column)
	if s.Store.Dialect == "sqlite" {
		return "REPLACE(" + quoted + ", 'T', ' ')"
	}
	return quoted
}

func (s *Server) restoredJobsStatus(w http.ResponseWriter, r *http.Request) {
	migration, err := s.migrationReviewStatus(r.Context())
	if err != nil {
		httpError(w, 409, "Migration review evidence is missing or changed; restore its verified receipt before approving recovery")
		return
	}
	out := map[string]any{"recoveryReviewRequired": s.Jobs.RecoveryReviewRequired(), "message": "Historical pending jobs may already have executed after the snapshot. Review their external effects before retrying or approving all pending jobs and enabled schedules."}
	if migration != nil {
		out["migration"] = migration
	}
	writeJSON(w, 200, out)
}
func (s *Server) approveRestoredJobs(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Confirm string `json:"confirm"`
	}
	if err := readJSON(r, &in); err != nil {
		httpError(w, 400, err.Error())
		return
	}
	if in.Confirm != "RECOVER_PENDING_AND_SCHEDULES" {
		httpError(w, 400, "Review pending tasks and external side effects, then confirm RECOVER_PENDING_AND_SCHEDULES to enable pending recovery and configured schedules")
		return
	}
	if _, err := s.migrationReviewStatus(r.Context()); err != nil {
		httpError(w, 409, "Migration review evidence is missing or changed; recovery was not approved")
		return
	}
	if err := s.Jobs.ApproveRecovery(r.Context()); err != nil {
		jobHTTPError(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"recoveryReviewRequired": false, "message": "Pending recovery and enabled registered schedules approved"})
}
