// SPDX-License-Identifier: AGPL-3.0-only
package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unicode"

	"github.com/AniBakaBaka/AniDan/internal/job"
	"github.com/AniBakaBaka/AniDan/internal/media"
	"github.com/AniBakaBaka/AniDan/internal/notify"
	"github.com/AniBakaBaka/AniDan/internal/store"
)

var notificationAutomaticEvents = []string{"import_success", "import_failed", "refresh_success", "refresh_failed", "auto_import_success", "auto_import_failed", "webhook_triggered", "webhook_import_success", "webhook_import_failed", "incremental_refresh_success", "incremental_refresh_failed", "media_scan_complete", "scheduled_task_complete", "scheduled_task_failed", "system_start", "fallback_search_complete", "match_fallback_complete", "predownload_complete"}

func (s *Server) installNotificationLifecycle() error {
	return s.Jobs.SetLifecycleObserver(s.notificationLifecycle, "notification_delivery", "notification_command", "notification_fallback_search")
}
func notificationTaskResult(t job.Task) store.Row {
	var out store.Row
	_ = unmarshalExactJSON(t.Result, &out)
	return out
}
func notificationJobOutcome(t job.Task) (string, string, bool) {
	result := notificationTaskResult(t)
	// A dispatcher that admitted an independent child has not completed the import.
	// Child metadata carries the category/schedule; only real child execution emits.
	if str(result["executionTaskId"]) != "" || boolean(result["alreadyCompleted"]) {
		return "", "", false
	}
	switch t.Kind {
	case "library_delete", "library_reindex":
		return "", "", false
	}
	scheduled := t.ScheduledTaskID != "" || (t.Parent != nil && t.Parent.ScheduledTaskID != "")
	success := t.Status == job.Completed
	suffix := "failed"
	if success {
		suffix = "success"
	}
	if scheduled {
		if success {
			return "scheduled_task_complete", "定时任务完成", false
		}
		return "scheduled_task_failed", "定时任务失败", false
	}
	switch t.Kind {
	case "predownload":
		if success && number(result["count"]) > 0 && !boolean(result["unchanged"]) {
			return "predownload_complete", "预下载下一集完成", false
		}
		return "", "", false
	case "control_auto_import":
		return "auto_import_" + suffix, "自动导入", false
	case "generic_import":
		if t.Parent != nil && t.Parent.Kind == "control_auto_import" {
			return "auto_import_" + suffix, "自动导入", false
		}
		return "import_" + suffix, "导入", false
	case "library_import", "import_media_items", "import_local_items", "calendar_import":
		return "import_" + suffix, "导入", false
	case "fetch_comments", "library_refresh":
		return "refresh_" + suffix, "弹幕刷新", false
	case "library_source_refresh":
		var p libTaskParams
		_ = unmarshalExactJSON(t.Params, &p)
		if p.Mode == "incremental" {
			return "incremental_refresh_" + suffix, "增量追更", !success
		}
		return "refresh_" + suffix, "数据源刷新", false
	case "incrementalRefresh", "subscription_target", "subscriptionScan":
		return "incremental_refresh_" + suffix, "增量追更", !success
	case "webhook_record", "webhook_search":
		if _, deleted := result["deleted"]; deleted || boolean(result["filtered"]) {
			return "", "", false
		}
		return "webhook_import_" + suffix, "Webhook导入", false
	case "scan_media_server":
		if success {
			return "media_scan_complete", "媒体库扫描完成", false
		}
	case "player_match":
		return "match_fallback_complete", "匹配后备处理", false
	}
	return "", "", false
}
func notificationMediaTitle(value string) string {
	// Only selected media-title fields are permitted. Never forward task error,
	// arbitrary filenames/paths, URLs, credentials or the complete parameter bag.
	if strings.Contains(value, "://") || strings.Contains(value, "\\") || strings.HasPrefix(value, "/") || strings.HasPrefix(value, "./") || strings.HasPrefix(value, "../") {
		return ""
	}
	return notificationClip(strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || (r >= 0x202a && r <= 0x202e) || (r >= 0x2066 && r <= 0x2069) {
			return ' '
		}
		return r
	}, value), 100)
}
func (s *Server) notificationLifecycle(ctx context.Context, e job.LifecycleEvent) {
	progressAudience, progressBlocked := s.observeNotificationProgress(ctx, e)
	if progressBlocked {
		// An unreadable progress receipt is not permission for a replacement
		// completion message to an unknown or newly configured audience.
		return
	}
	n := s.notificationEvents
	if n == nil || n.closed.Load() || e.Phase != job.LifecycleFinished || e.Cancelled {
		return
	}
	t := e.Task
	if t.Status != job.Completed && t.Status != job.Failed {
		return
	}
	if t.Kind == "player_search" && t.ScheduledTaskID == "" && (t.Parent == nil || t.Parent.ScheduledTaskID == "") {
		var p playerSearchParams
		if unmarshalExactJSON(t.Params, &p) != nil {
			return
		}
		payload := notificationFallbackParams{Keyword: notificationKeyword(p.Keyword), Success: t.Status == job.Completed}
		if payload.Success {
			var result struct {
				Count  int                        `json:"count"`
				Notice notificationFallbackParams `json:"notification"`
			}
			if unmarshalExactJSON(t.Result, &result) == nil {
				payload = result.Notice
				payload.Keyword = notificationKeyword(p.Keyword)
				payload.Success = true
				payload.Total = result.Count
			}
		}
		if err := s.enqueueFallbackNoticeExcept(ctx, "job:"+t.ID+":fallback_search_complete", payload, progressAudience); err != nil {
			n.enqueueFailures.Add(1)
			slog.Warn("notification lifecycle admission failed", "kind", "fallback_search_complete")
		}
		return
	}
	kind, label, window := notificationJobOutcome(t)
	if kind == "" {
		return
	}
	result := notificationTaskResult(t)
	title := ""
	group := ""
	switch t.Kind {
	case "generic_import":
		var p ImportRequest
		if unmarshalExactJSON(t.Params, &p) == nil {
			title = notificationMediaTitle(p.Title)
		}
	case "predownload":
		if row, err := s.Store.Get(ctx, "episode", result["episodeId"]); err == nil {
			title = notificationMediaTitle(str(row["title"]))
			group = fmt.Sprintf("source:%v", row["source_id"])
		}
	case "fetch_comments":
		var p struct {
			EpisodeID int64 `json:"episodeId"`
		}
		if unmarshalExactJSON(t.Params, &p) == nil {
			if row, err := s.Store.Get(ctx, "episode", p.EpisodeID); err == nil {
				title = notificationMediaTitle(str(row["title"]))
				group = fmt.Sprintf("source:%v", row["source_id"])
			}
		}
	case "library_source_refresh", "library_import":
		var p libTaskParams
		if unmarshalExactJSON(t.Params, &p) == nil {
			if source, err := s.Store.Get(ctx, "anime_sources", p.Source); err == nil {
				group = fmt.Sprintf("anime:%v", source["anime_id"])
				if anime, err := s.Store.Get(ctx, "anime", source["anime_id"]); err == nil {
					title = notificationMediaTitle(str(anime["title"]))
				}
			}
		}
	case "player_match":
		var p playerMatchParams
		if unmarshalExactJSON(t.Params, &p) == nil {
			title = notificationMediaTitle(p.Parsed.Title)
		}
	case "webhook_record":
		var p struct {
			ID int64 `json:"id"`
		}
		if unmarshalExactJSON(t.Params, &p) == nil {
			if row, err := s.Store.Get(ctx, "webhook_tasks", p.ID); err == nil {
				var event media.WebhookEvent
				if json.Unmarshal([]byte(str(row["payload"])), &event) == nil {
					if event.Delete {
						return
					}
					title = notificationMediaTitle(event.Title)
				}
			}
		}
	case "webhook_search":
		var p media.WebhookEvent
		if unmarshalExactJSON(t.Params, &p) == nil {
			if p.Delete {
				return
			}
			title = notificationMediaTitle(p.Title)
		}
	}
	if title != "" {
		label += " · " + title
	}
	text := "操作已完成。"
	if t.Status != job.Completed {
		text = "操作失败；可能存在部分结果，请在已登录管理界面查看任务详情。"
	} else {
		for _, k := range []string{"imported", "processed", "refreshed", "scanned", "updatedSources", "count"} {
			if v, ok := result[k]; ok {
				text += fmt.Sprintf("\n%s: %d", k, number(v))
			}
		}
		if t.Kind == "player_match" {
			text += fmt.Sprintf("\n是否匹配: %t", boolean(result["isMatched"]))
		}
	}
	text += "\n任务ID: " + t.ID
	event := notify.Event{Type: kind, Title: label, Text: text}
	if t.Kind == "generic_import" && t.Status == job.Completed {
		var p ImportRequest
		if unmarshalExactJSON(t.Params, &p) == nil && len(p.ImageURL) <= 2048 {
			event.ImageURL = p.ImageURL
		}
	}
	if err := s.collectNotificationEventExcept(ctx, event, "job:"+t.ID+":"+kind, group, window, progressAudience); err != nil {
		slog.Warn("notification lifecycle admission failed", "kind", kind)
	}
}

// NotifyStarted is called only after the TCP listener has successfully bound.
// New()/httptest fixtures do not fabricate network-readiness notifications.
func (s *Server) NotifyStarted() {
	n := s.notificationEvents
	if n == nil || !n.started.CompareAndSwap(false, true) {
		return
	}
	ctx, cancel := context.WithTimeout(s.ctx, 3*time.Second)
	defer cancel()
	if err := s.collectNotificationEvent(ctx, notify.Event{Type: "system_start", Title: "AniDan 已启动", Text: "版本 " + Version}, "startup:"+randomID(), "", false); err != nil {
		slog.Warn("startup notification admission failed")
	}
}
