// SPDX-License-Identifier: AGPL-3.0-only
package server

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/AniBakaBaka/AniDan/internal/job"
	"github.com/AniBakaBaka/AniDan/internal/notify"
	"github.com/AniBakaBaka/AniDan/internal/store"
)

func notificationFingerprint(v any) string {
	b, _ := json.Marshal(v)
	return fmt.Sprintf("%x", sha256.Sum256(b))
}
func (s *Server) notificationTokens(ctx context.Context, c notify.Channel, u notify.Update, v *notificationSession, a notificationAction) (notify.Message, error) {
	if !u.Admin {
		return notify.Message{}, notify.ErrForbidden
	}
	back := notificationChoice("Token list", "tokens", "")
	switch a.Kind {
	case "tokens":
		rows, e := s.Store.List(ctx, "api_tokens", nil, 500, 0)
		if e != nil {
			return notify.Message{}, e
		}
		page := max(0, min(a.Page, max((len(rows)-1)/5, 0)))
		opts := []notificationOption{}
		for _, r := range rows[page*5 : min(page*5+5, len(rows))] {
			state := "disabled"
			if boolean(r["is_enabled"]) {
				state = "enabled"
			}
			opts = append(opts, notificationChoice(notificationClip(str(r["name"]), 35)+" ("+state+")", "token", str(r["id"])))
		}
		opts = notificationPageOptions(opts, "tokens", "", page, len(rows))
		opts = append(opts, notificationChoice("Create token", "token_add", ""), notificationChoice("Home", "home", ""))
		return s.notificationRender(c, u, v, fmt.Sprintf("API tokens: %d (up to 500 shown)\nSecrets are never sent to chat.", len(rows)), opts...), nil
	case "token_add":
		v.state = "token_name"
		return s.notificationRender(c, u, v, "Send the new token name (1–50 characters), or /cancel."), nil
	case "token_validity":
		if v.input == "" {
			return notify.Message{}, notify.ErrForbidden
		}
		if a.ID != "30d" && a.ID != "90d" && a.ID != "180d" && a.ID != "permanent" {
			return notify.Message{}, notify.ErrForbidden
		}
		return s.notificationConfirm(c, u, v, "Create API token "+notificationClip(v.input, 50)+"?\nValidity: "+a.ID+"\nLimit: 500 calls/day\nThis grants player API access. Retrieve its secret only in the authenticated UI.", notificationAction{Kind: "token_create", ID: a.ID, Text: v.input}), nil
	case "token_create":
		if !a.Confirm || a.Text == "" || len([]rune(a.Text)) > 50 {
			return notify.Message{}, notify.ErrForbidden
		}
		var expires any
		days := map[string]int{"30d": 30, "90d": 90, "180d": 180}[a.ID]
		if days > 0 {
			expires = time.Now().In(s.authLocation()).AddDate(0, 0, days).Format("2006-01-02T15:04:05")
		} else if a.ID != "permanent" {
			return notify.Message{}, notify.ErrForbidden
		}
		id, e := s.Store.Insert(ctx, "api_tokens", store.Row{"name": a.Text, "token": randomID(), "is_enabled": true, "created_at": s.now(), "expires_at": expires, "daily_call_limit": 500, "daily_call_count": 0})
		if e != nil {
			return notify.Message{}, e
		}
		v.input = ""
		return s.notificationRender(c, u, v, fmt.Sprintf("Token %d created. Retrieve its secret in the authenticated AniDan UI.", id), back), nil
	case "token":
		row, e := s.Store.Get(ctx, "api_tokens", a.ID)
		if e != nil {
			return notify.Message{}, e
		}
		state := "disabled"
		toggle := "Enable"
		if boolean(row["is_enabled"]) {
			state = "enabled"
			toggle = "Disable"
		}
		return s.notificationRender(c, u, v, fmt.Sprintf("Token: %s\nState: %s\nExpires: %s\nCalls today: %s / %s", notificationClip(str(row["name"]), 50), state, str(row["expires_at"]), str(row["daily_call_count"]), str(row["daily_call_limit"])), notificationChoice(toggle, "token_toggle", a.ID), notificationChoice("Reset daily counter", "token_reset", a.ID), notificationChoice("Delete token", "token_delete", a.ID), back), nil
	case "token_toggle", "token_reset", "token_delete":
		row, e := s.Store.Get(ctx, "api_tokens", a.ID)
		if e != nil {
			return notify.Message{}, e
		}
		if !a.Confirm {
			a.Text = notificationFingerprint(store.Row{"name": row["name"], "token": row["token"], "is_enabled": row["is_enabled"], "expires_at": row["expires_at"], "daily_call_limit": row["daily_call_limit"]})
			a.Value = !boolean(row["is_enabled"])
			action := "Delete token permanently"
			if a.Kind == "token_reset" {
				action = "Reset daily call counter"
			}
			if a.Kind == "token_toggle" {
				action = "Disable token"
				if a.Value {
					action = "Enable token"
				}
			}
			return s.notificationConfirm(c, u, v, action+" for "+notificationClip(str(row["name"]), 50)+"?", a), nil
		}
		current := notificationFingerprint(store.Row{"name": row["name"], "token": row["token"], "is_enabled": row["is_enabled"], "expires_at": row["expires_at"], "daily_call_limit": row["daily_call_limit"]})
		if current != a.Text {
			return s.notificationRender(c, u, v, "Token changed since confirmation. Review it again.", back), nil
		}
		switch a.Kind {
		case "token_delete":
			e = s.Store.Delete(ctx, "api_tokens", a.ID)
		case "token_toggle":
			e = s.Store.Update(ctx, "api_tokens", a.ID, store.Row{"is_enabled": a.Value})
		case "token_reset":
			e = s.Store.Update(ctx, "api_tokens", a.ID, store.Row{"daily_call_count": 0})
		}
		if e != nil {
			return notify.Message{}, e
		}
		return s.notificationRender(c, u, v, "Token change completed.", back), nil
	}
	return notify.Message{}, notify.ErrUnsupported
}
func notificationScheduleFingerprint(t job.ScheduledTask) string {
	return notificationFingerprint(struct {
		ID, Name, Kind, Cron string
		Enabled              bool
		Config               string
	}{t.ID, t.Name, t.Kind, t.CronExpression, t.IsEnabled, string(t.TaskConfig)})
}
func (s *Server) notificationSchedules(ctx context.Context, c notify.Channel, u notify.Update, v *notificationSession, a notificationAction) (notify.Message, error) {
	if !u.Admin {
		return notify.Message{}, notify.ErrForbidden
	}
	back := notificationChoice("Schedule list", "schedules", "")
	switch a.Kind {
	case "schedules":
		rows, e := s.Jobs.Schedules()
		if e != nil {
			return notify.Message{}, e
		}
		page := max(0, min(a.Page, max((len(rows)-1)/5, 0)))
		opts := []notificationOption{}
		for _, r := range rows[page*5 : min(page*5+5, len(rows))] {
			state := "disabled"
			if r.IsEnabled {
				state = "enabled"
			}
			opts = append(opts, notificationChoice(notificationClip(r.Name, 35)+" ("+state+")", "schedule", r.ID))
		}
		opts = notificationPageOptions(opts, "schedules", "", page, len(rows))
		opts = append(opts, notificationChoice("Add maintenance schedule", "schedule_add", ""), notificationChoice("Home", "home", ""))
		return s.notificationRender(c, u, v, fmt.Sprintf("Scheduled tasks: %d\nTimezone: %s", len(rows), s.Config.Timezone), opts...), nil
	case "schedule":
		r, e := s.Jobs.Schedule(a.ID)
		if e != nil {
			return notify.Message{}, e
		}
		opts := []notificationOption{notificationChoice("Run now", "schedule_run", r.ID), notificationChoice("Toggle enabled", "schedule_toggle", r.ID)}
		if !r.IsSystemTask {
			opts = append(opts, notificationChoice("Edit cron expression", "schedule_edit", r.ID), notificationChoice("Delete schedule", "schedule_delete", r.ID))
		}
		opts = append(opts, back)
		return s.notificationRender(c, u, v, fmt.Sprintf("Schedule: %s\nJob: %s\nCron: %s (%s)\nEnabled: %t\nSystem task: %t", notificationClip(r.Name, 200), r.Kind, r.CronExpression, s.Config.Timezone, r.IsEnabled, r.IsSystemTask), opts...), nil
	case "schedule_add":
		return s.notificationRender(c, u, v, "Choose a supported maintenance job. Creation uses its default configuration {}.", notificationChoice("Expired cache cleanup", "schedule_kind", "cacheCleanup"), notificationChoice("Database maintenance", "schedule_kind", "databaseMaintenance"), notificationChoice("Full backup", "schedule_kind", "databaseBackup"), back), nil
	case "schedule_kind":
		if a.ID != "cacheCleanup" && a.ID != "databaseMaintenance" && a.ID != "databaseBackup" {
			return notify.Message{}, notify.ErrUnsupported
		}
		v.data = notificationAction{Schedule: &job.ScheduledTask{Kind: a.ID, IsEnabled: true, TaskConfig: json.RawMessage(`{}`)}}
		v.state = "schedule_name"
		return s.notificationRender(c, u, v, "Send a name for this "+a.ID+" schedule, or /cancel."), nil
	case "schedule_edit":
		r, e := s.Jobs.Schedule(a.ID)
		if e != nil {
			return notify.Message{}, e
		}
		if r.IsSystemTask {
			return notify.Message{}, notify.ErrForbidden
		}
		v.data = notificationAction{Schedule: &r, Text: notificationScheduleFingerprint(r)}
		v.state = "schedule_cron_edit"
		return s.notificationRender(c, u, v, "Send a new five-field cron expression in "+s.Config.Timezone+". Current: "+r.CronExpression), nil
	case "schedule_create", "schedule_save":
		if !a.Confirm || a.Schedule == nil {
			return notify.Message{}, notify.ErrForbidden
		}
		if a.Kind == "schedule_save" {
			r, e := s.Jobs.Schedule(a.Schedule.ID)
			if e != nil {
				return notify.Message{}, e
			}
			if r.IsSystemTask || notificationScheduleFingerprint(r) != a.Text {
				return s.notificationRender(c, u, v, "Schedule changed since confirmation. Review it again.", back), nil
			}
		}
		result, e := s.Jobs.SaveSchedule(*a.Schedule)
		if e != nil {
			return notify.Message{}, e
		}
		return s.notificationRender(c, u, v, "Schedule saved: "+notificationClip(result.Name, 200), notificationChoice("View schedule", "schedule", result.ID), back), nil
	case "schedule_toggle", "schedule_delete", "schedule_run":
		r, e := s.Jobs.Schedule(a.ID)
		if e != nil {
			return notify.Message{}, e
		}
		if a.Kind == "schedule_delete" && r.IsSystemTask {
			return notify.Message{}, notify.ErrForbidden
		}
		if !a.Confirm {
			a.Text = notificationScheduleFingerprint(r)
			a.Value = !r.IsEnabled
			description := strings.TrimPrefix(a.Kind, "schedule_")
			if a.Kind == "schedule_toggle" {
				description = "disable"
				if a.Value {
					description = "enable"
				}
			}
			return s.notificationConfirm(c, u, v, description+" schedule "+notificationClip(r.Name, 200)+"?\nJob: "+r.Kind+"\nCron: "+r.CronExpression, a), nil
		}
		if notificationScheduleFingerprint(r) != a.Text {
			return s.notificationRender(c, u, v, "Schedule changed since confirmation. Review it again.", back), nil
		}
		switch a.Kind {
		case "schedule_delete":
			e = s.Jobs.DeleteSchedule(r.ID)
		case "schedule_toggle":
			r.IsEnabled = a.Value
			_, e = s.Jobs.SaveSchedule(r)
		case "schedule_run":
			var id string
			id, e = s.Jobs.RunSchedule(r.ID)
			if e == nil {
				return s.notificationRender(c, u, v, "Scheduled run queued: "+id, notificationChoice("View task", "task", id), back), nil
			}
		}
		if e != nil {
			return notify.Message{}, e
		}
		return s.notificationRender(c, u, v, "Schedule change completed.", back), nil
	}
	return notify.Message{}, notify.ErrUnsupported
}
