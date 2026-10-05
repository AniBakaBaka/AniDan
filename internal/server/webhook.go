// SPDX-License-Identifier: AGPL-3.0-only
package server

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/AniBakaBaka/AniDan/internal/job"
	"github.com/AniBakaBaka/AniDan/internal/media"
	"github.com/AniBakaBaka/AniDan/internal/notify"
	"github.com/AniBakaBaka/AniDan/internal/store"
)

func (s *Server) registerWebhook(m *http.ServeMux) {
	m.HandleFunc("POST /api/webhook/{webhook_type}", s.webhookIngress)
	for _, p := range []struct {
		route string
		h     http.HandlerFunc
	}{{"GET /api/ui/webhooks/available", s.webhookAvailable}, {"GET /api/ui/webhook-tasks", s.webhookTasks}, {"POST /api/ui/webhook-tasks/delete-bulk", s.webhookDeleteBulk}, {"DELETE /api/ui/webhook-tasks/clear-all", s.webhookClear}, {"POST /api/ui/webhook-tasks/run-now", s.webhookRunNow}} {
		m.HandleFunc(p.route, s.operator(p.h))
	}
	if s.Jobs != nil {
		if e := s.Jobs.Register("webhook_record", s.runWebhookRecord); e != nil {
			panic(e)
		}
		if e := s.Jobs.Register("webhook_search", s.runLegacyWebhook); e != nil {
			panic(e)
		}
		if s.ctx != nil {
			s.webhookDone = make(chan struct{})
			go func() {
				defer close(s.webhookDone)
				s.webhookPoll()
			}()
		}
	}
}
func (s *Server) webhookAvailable(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, []string{"emby", "jellyfin", "plex"})
}
func (s *Server) webhookIngress(w http.ResponseWriter, r *http.Request) {
	key := s.Config.WebhookAPIKey
	if key == "" {
		key = s.authConfig(r.Context(), "webhookApiKey", "")
	}
	provided := r.URL.Query().Get("api_key")
	if key == "" || subtle.ConstantTimeCompare([]byte(key), []byte(provided)) != 1 {
		httpError(w, 401, "Invalid Webhook API Key")
		return
	}
	kind := strings.ToLower(r.PathValue("webhook_type"))
	if kind != "emby" && kind != "jellyfin" && kind != "plex" {
		httpError(w, 404, "Unsupported webhook type")
		return
	}
	var payload map[string]any
	ct := r.Header.Get("Content-Type")
	if strings.Contains(ct, "multipart/form-data") {
		if e := r.ParseMultipartForm(1 << 20); e != nil {
			httpError(w, 400, "Invalid multipart webhook")
			return
		}
		if r.MultipartForm != nil {
			defer r.MultipartForm.RemoveAll()
		}
		d := json.NewDecoder(strings.NewReader(r.FormValue("payload")))
		d.UseNumber()
		if e := d.Decode(&payload); e != nil {
			httpError(w, 400, "Invalid payload JSON")
			return
		}
	} else if strings.Contains(ct, "application/x-www-form-urlencoded") {
		if e := r.ParseForm(); e != nil {
			httpError(w, 400, "Invalid webhook form")
			return
		}
		d := json.NewDecoder(strings.NewReader(r.FormValue("payload")))
		d.UseNumber()
		if e := d.Decode(&payload); e != nil {
			httpError(w, 400, "Invalid payload JSON")
			return
		}
	} else if e := readJSON(r, &payload); e != nil {
		httpError(w, 400, "Invalid webhook JSON")
		return
	}
	if strings.EqualFold(s.authConfig(r.Context(), "webhookLogRawRequest", "false"), "true") {
		writeWebhookPayloadLog(r.Context(), slog.Default(), kind, payload)
	}
	events, e := media.ParseWebhook(kind, payload)
	if e != nil {
		httpError(w, 422, e.Error())
		return
	}
	if strings.ToLower(s.authConfig(r.Context(), "webhookEnabled", "true")) != "true" {
		writeJSON(w, 202, map[string]any{"message": "Webhook disabled", "accepted": 0})
		return
	}
	accepted := []int64{}
	for _, event := range events {
		allowed, e := s.webhookAllowed(r.Context(), event)
		if e != nil {
			httpError(w, 422, e.Error())
			return
		}
		if !allowed {
			continue
		}
		b, _ := json.Marshal(event)
		h := sha256.Sum256(b)
		unique := fmt.Sprintf("anidan-webhook-%x", h[:])
		old, e := s.Store.List(r.Context(), "webhook_tasks", store.Row{"unique_key": unique}, 1, 0)
		if e != nil {
			httpError(w, 500, e.Error())
			return
		}
		if len(old) > 0 {
			accepted = append(accepted, number(old[0]["id"]))
			continue
		}
		now := s.now()
		execute := now
		if strings.EqualFold(s.authConfig(r.Context(), "webhookDelayedImportEnabled", "false"), "true") && !event.Delete {
			hours, e := strconv.Atoi(s.authConfig(r.Context(), "webhookDelayedImportHours", "24"))
			if e != nil || hours < 0 || hours > 8760 {
				httpError(w, 422, "Invalid webhook delay hours")
				return
			}
			loc, _ := time.LoadLocation(s.Config.Timezone)
			execute = time.Now().In(loc).Add(time.Duration(hours) * time.Hour).Format("2006-01-02T15:04:05.999999")
		}
		id, e := s.Store.Insert(r.Context(), "webhook_tasks", store.Row{"reception_time": now, "execute_time": execute, "webhook_source": kind, "status": "pending", "payload": string(b), "unique_key": unique, "task_title": event.Title})
		created := e == nil
		if e != nil {
			reread, err := s.Store.List(r.Context(), "webhook_tasks", store.Row{"unique_key": unique}, 1, 0)
			if err == nil && len(reread) > 0 {
				id = number(reread[0]["id"])
			} else {
				httpError(w, 409, e.Error())
				return
			}
		}
		accepted = append(accepted, id)
		if created {
			if err := s.collectNotificationEvent(r.Context(), notify.Event{Type: "webhook_triggered", Title: notificationMediaTitle(event.Title), Text: "Webhook已持久记录，等待处理。"}, fmt.Sprintf("webhook:%d", id), "", false); err != nil {
				slog.Warn("webhook notification admission failed")
			}
		}
		if normalizeDate(execute) <= normalizeDate(s.now()) {
			if _, e = s.submitWebhook(r.Context(), id, false); e != nil {
				slog.Warn("webhook retained pending after submission failure", "id", id, "error", e.Error())
			}
		}
	}
	writeJSON(w, 202, map[string]any{"message": "Webhook accepted", "accepted": len(accepted), "ids": accepted})
}
func (s *Server) webhookAllowed(ctx context.Context, event media.WebhookEvent) (bool, error) {
	if event.Delete {
		return strings.EqualFold(s.authConfig(ctx, "webhookDeleteSyncEnabled", "false"), "true"), nil
	}
	pattern := s.authConfig(ctx, "webhookFilterRegex", "")
	if pattern == "" {
		return true, nil
	}
	re, e := regexp.Compile("(?i)" + pattern)
	if e != nil {
		return false, errors.New("configured webhook filter regex is invalid")
	}
	match := re.MatchString(event.Title)
	if s.authConfig(ctx, "webhookFilterMode", "blacklist") == "whitelist" {
		return match, nil
	}
	return !match, nil
}
func (s *Server) submitWebhook(ctx context.Context, id int64, manual bool) (bool, error) {
	row, e := s.Store.Get(ctx, "webhook_tasks", id)
	if e != nil || row == nil {
		return false, errors.New("webhook record not found")
	}
	status := str(row["status"])
	if status != "pending" && !(manual && status == "failed") {
		return false, nil
	}
	if !manual && normalizeDate(str(row["execute_time"])) > normalizeDate(s.now()) {
		return false, nil
	}
	res, e := s.Store.DB.ExecContext(ctx, s.Store.Rebind("UPDATE webhook_tasks SET status='dispatching' WHERE id=? AND status=?"), id, status)
	if e != nil {
		return false, e
	}
	n, e := res.RowsAffected()
	if e != nil || n == 0 {
		return false, e
	}
	_, e = s.Jobs.SubmitWithOptions("webhook_record", map[string]any{"id": id}, nil, job.SubmitOptions{Title: "Webhook: " + str(row["task_title"]), QueueType: "download", UniqueKey: fmt.Sprintf("webhook-record-%d", id)})
	if e != nil {
		_, _ = s.Store.DB.ExecContext(ctx, s.Store.Rebind("UPDATE webhook_tasks SET status='pending' WHERE id=? AND status='dispatching'"), id)
		return false, e
	}
	return true, nil
}
func (s *Server) webhookPoll() {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-ticker.C:
			s.reconcileWebhookRecords(s.ctx)
			rows, e := s.Store.DB.QueryContext(s.ctx, s.Store.Rebind("SELECT id FROM webhook_tasks WHERE status='pending' AND execute_time<=? ORDER BY execute_time,id LIMIT 500"), s.now())
			if e != nil {
				continue
			}
			ids := []int64{}
			for rows.Next() {
				var id int64
				if rows.Scan(&id) == nil {
					ids = append(ids, id)
				}
			}
			rows.Close()
			for _, id := range ids {
				if s.ctx.Err() != nil {
					return
				}
				_, _ = s.submitWebhook(s.ctx, id, false)
			}
		}
	}
}

// Interrupted running jobs require manual retry; a crash between database claim
// and durable job submission can safely be returned to the pending queue.
func (s *Server) reconcileWebhookRecords(ctx context.Context) {
	rows, e := s.Store.DB.QueryContext(ctx, "SELECT id,status FROM webhook_tasks WHERE status IN ('running','dispatching') ORDER BY id LIMIT 500")
	if e != nil {
		return
	}
	type record struct {
		id     int64
		status string
	}
	records := []record{}
	for rows.Next() {
		var v record
		if rows.Scan(&v.id, &v.status) == nil {
			records = append(records, v)
		}
	}
	rows.Close()
	for _, v := range records {
		var taskStatus string
		e = s.Store.DB.QueryRowContext(ctx, s.Store.Rebind("SELECT status FROM task_history WHERE unique_key=? ORDER BY created_at DESC,id DESC LIMIT 1"), fmt.Sprintf("webhook-record-%d", v.id)).Scan(&taskStatus)
		replacement := ""
		if errors.Is(e, sql.ErrNoRows) {
			if v.status == "dispatching" {
				replacement = "pending"
			} else {
				replacement = "failed"
			}
		} else if e == nil && job.Terminal(taskStatus) {
			if taskStatus == job.Completed {
				replacement = "completed"
			} else {
				replacement = "failed"
			}
		}
		if replacement != "" {
			_, _ = s.Store.DB.ExecContext(ctx, s.Store.Rebind("UPDATE webhook_tasks SET status=? WHERE id=? AND status=?"), replacement, v.id, v.status)
		}
	}
}
func (s *Server) runWebhookRecord(ctx context.Context, raw json.RawMessage, progress func(int, string)) (result any, err error) {
	var p struct {
		ID int64 `json:"id"`
	}
	if err = unmarshalExactJSON(raw, &p); err != nil {
		return nil, err
	}
	row, e := s.Store.Get(ctx, "webhook_tasks", p.ID)
	if e != nil || row == nil {
		return nil, errors.New("webhook record missing")
	}
	if str(row["status"]) == "completed" {
		return map[string]any{"alreadyCompleted": true}, nil
	}
	if e = s.Store.Update(ctx, "webhook_tasks", p.ID, store.Row{"status": "running"}); e != nil {
		return nil, e
	}
	defer func() {
		recovered := recover()
		status := "completed"
		if err != nil || recovered != nil {
			status = "failed"
		}
		finishCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if e := s.Store.Update(finishCtx, "webhook_tasks", p.ID, store.Row{"status": status}); err == nil && e != nil {
			err = e
		}
		if recovered != nil {
			panic(recovered)
		}
	}()
	var event media.WebhookEvent
	if err = json.Unmarshal([]byte(str(row["payload"])), &event); err != nil {
		return nil, err
	}
	if event.Source == "" {
		event.Source = str(row["webhook_source"])
	}
	return s.executeWebhook(ctx, event, progress)
}
func (s *Server) runLegacyWebhook(ctx context.Context, raw json.RawMessage, progress func(int, string)) (any, error) {
	var event media.WebhookEvent
	if e := unmarshalExactJSON(raw, &event); e != nil {
		return nil, e
	}
	return s.executeWebhook(ctx, event, progress)
}
func (s *Server) executeWebhook(ctx context.Context, event media.WebhookEvent, progress func(int, string)) (any, error) {
	if !strings.EqualFold(s.authConfig(ctx, "webhookEnabled", "true"), "true") {
		return nil, errors.New("webhook disabled before execution")
	}
	allowed, e := s.webhookAllowed(ctx, event)
	if e != nil {
		return nil, e
	}
	if !allowed {
		return map[string]any{"filtered": true}, nil
	}
	if event.Delete {
		return s.deleteWebhookMedia(ctx, event, progress)
	}
	if event.TMDBID != "" && event.MediaType == "tv_series" && strings.EqualFold(s.authConfig(ctx, "webhookEnableTmdbSeasonMapping", "false"), "true") {
		return s.importWebhookSeasonMapped(ctx, event, progress)
	}
	row := store.Row{"title": event.Title, "media_type": event.MediaType, "season": event.Season, "year": nil, "episode": nil, "tmdb_id": event.TMDBID, "tvdb_id": event.TVDBID, "imdb_id": event.IMDBID, "media_server_type": event.Source, "series_id": event.SeriesID, "season_id": event.SeasonID, "episode_id": event.EpisodeID}
	if event.Episode != nil {
		row["episode"] = *event.Episode
	}
	if event.Year != nil {
		row["year"] = *event.Year
	}
	if e = s.importMatchedMedia(ctx, row, progress); e != nil {
		return nil, e
	}
	return map[string]any{"imported": true, "title": event.Title}, nil
}
func (s *Server) deleteWebhookMedia(ctx context.Context, event media.WebhookEvent, progress func(int, string)) (any, error) {
	var q string
	args := []any{event.Source}
	table := "anime"
	switch strings.ToLower(event.ItemType) {
	case "episode":
		if strings.TrimSpace(event.EpisodeID) == "" {
			return nil, errors.New("episode deletion requires exact episode ID")
		}
		table = "episode"
		q = "SELECT e.id FROM episode e JOIN anime_sources s ON e.source_id=s.id JOIN anime_metadata m ON s.anime_id=m.anime_id WHERE m.media_server_type=? AND e.media_server_episode_id=?"
		args = append(args, event.EpisodeID)
	case "season":
		if strings.TrimSpace(event.SeasonID) == "" {
			return nil, errors.New("season deletion requires exact season ID")
		}
		q = "SELECT anime_id FROM anime_metadata WHERE media_server_type=? AND media_server_season_id=?"
		args = append(args, event.SeasonID)
	case "movie", "series":
		if strings.TrimSpace(event.SeriesID) == "" {
			return nil, errors.New("series deletion requires exact series ID")
		}
		q = "SELECT anime_id FROM anime_metadata WHERE media_server_type=? AND media_server_series_id=?"
		args = append(args, event.SeriesID)
	default:
		return nil, errors.New("unsupported media deletion type")
	}
	rows, e := s.Store.DB.QueryContext(ctx, s.Store.Rebind(q), args...)
	if e != nil {
		return nil, e
	}
	ids := []int64{}
	for rows.Next() {
		var id int64
		if e = rows.Scan(&id); e != nil {
			rows.Close()
			return nil, e
		}
		ids = append(ids, id)
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return nil, e
	}
	if len(ids) == 0 {
		return map[string]any{"deleted": 0}, nil
	}
	b, _ := json.Marshal(libTaskParams{Table: table, IDs: ids, DeleteFiles: false})
	return s.libDeleteJob(ctx, b, progress)
}
func (s *Server) webhookTasks(w http.ResponseWriter, r *http.Request) {
	page, size := mediaPage(r)
	where := ""
	args := []any{}
	if search := r.URL.Query().Get("search"); search != "" {
		where = " WHERE LOWER(task_title) LIKE ?"
		args = append(args, "%"+strings.ToLower(search)+"%")
	}
	var total int64
	if e := s.Store.DB.QueryRowContext(r.Context(), s.Store.Rebind("SELECT COUNT(*) FROM webhook_tasks"+where), args...).Scan(&total); e != nil {
		httpError(w, 500, e.Error())
		return
	}
	query := "SELECT * FROM webhook_tasks" + where + " ORDER BY reception_time DESC,id DESC LIMIT ? OFFSET ?"
	args = append(args, size, (page-1)*size)
	rows, e := s.Store.DB.QueryContext(r.Context(), s.Store.Rebind(query), args...)
	if e != nil {
		httpError(w, 500, e.Error())
		return
	}
	out := []any{}
	for rows.Next() {
		row, e := store.ScanRow(rows, store.Schema["webhook_tasks"])
		if e != nil {
			rows.Close()
			httpError(w, 500, e.Error())
			return
		}
		v := map[string]any{"id": row["id"], "receptionTime": row["reception_time"], "executeTime": row["execute_time"], "webhookSource": row["webhook_source"], "status": row["status"], "taskTitle": row["task_title"]}
		out = append(out, v)
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		httpError(w, 500, e.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"total": total, "list": out, "items": out, "page": page, "pageSize": size})
}
func webhookIDs(r *http.Request) ([]int64, error) {
	var p struct {
		IDs []int64 `json:"ids"`
	}
	e := readJSON(r, &p)
	if e != nil {
		return nil, e
	}
	if len(p.IDs) > 10000 {
		return nil, errors.New("select at most 10000 webhook records")
	}
	return p.IDs, nil
}
func (s *Server) webhookDeleteBulk(w http.ResponseWriter, r *http.Request) {
	ids, e := webhookIDs(r)
	if e != nil {
		httpError(w, 422, e.Error())
		return
	}
	count := int64(0)
	for _, id := range ids {
		res, e := s.Store.DB.ExecContext(r.Context(), s.Store.Rebind("DELETE FROM webhook_tasks WHERE id=? AND status NOT IN ('running','dispatching')"), id)
		if e != nil {
			httpError(w, 500, e.Error())
			return
		}
		n, _ := res.RowsAffected()
		count += n
	}
	writeJSON(w, 200, map[string]any{"message": fmt.Sprintf("成功删除 %d 个任务。", count), "deletedCount": count})
}
func (s *Server) webhookClear(w http.ResponseWriter, r *http.Request) {
	res, e := s.Store.DB.ExecContext(r.Context(), "DELETE FROM webhook_tasks WHERE status NOT IN ('running','dispatching')")
	if e != nil {
		httpError(w, 500, e.Error())
		return
	}
	n, _ := res.RowsAffected()
	writeJSON(w, 200, map[string]any{"message": fmt.Sprintf("已清空 %d 个任务。", n), "deletedCount": n})
}
func (s *Server) webhookRunNow(w http.ResponseWriter, r *http.Request) {
	ids, e := webhookIDs(r)
	if e != nil {
		httpError(w, 422, e.Error())
		return
	}
	n := 0
	for _, id := range ids {
		submitted, err := s.submitWebhook(r.Context(), id, true)
		if err != nil {
			httpError(w, 409, err.Error())
			return
		}
		if submitted {
			n++
		}
	}
	writeJSON(w, 200, map[string]any{"message": fmt.Sprintf("已成功提交 %d 个任务到执行队列。", n), "submittedCount": n})
}
