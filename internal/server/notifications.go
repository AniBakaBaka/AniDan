// SPDX-License-Identifier: AGPL-3.0-only
package server

import (
	"context"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/AniBakaBaka/AniDan/internal/job"
	"github.com/AniBakaBaka/AniDan/internal/notify"
	"github.com/AniBakaBaka/AniDan/internal/store"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type notificationParams struct {
	ChannelID int64         `json:"channelId"`
	Event     notify.Update `json:"event"`
}

func (s *Server) registerNotifications(m *http.ServeMux) {
	s.initNotificationRouting()
	s.notificationRuntime = newNotificationRuntime(s.Notify)
	s.initNotificationEvents(m)
	s.initNotificationProgress(m)
	m.HandleFunc("POST /api/ui/notification/channels/{channel_id}/register", s.operator(s.notificationRegister))
	m.HandleFunc("GET /api/ui/notification/channels/{channel_id}/receiver-status", s.operator(s.notificationReceiverStatus))
	m.HandleFunc("GET /api/ui/notification/channel-types", s.operator(s.notificationTypes))
	m.HandleFunc("GET /api/ui/notification/schema/{channel_type}", s.operator(s.notificationSchema))
	m.HandleFunc("GET /api/ui/notification/channels", s.operator(s.notificationList))
	m.HandleFunc("POST /api/ui/notification/channels", s.operator(s.notificationCreate))
	m.HandleFunc("PUT /api/ui/notification/channels/{channel_id}", s.operator(s.notificationUpdate))
	m.HandleFunc("DELETE /api/ui/notification/channels/{channel_id}", s.operator(s.notificationDelete))
	m.HandleFunc("POST /api/ui/notification/channels/{channel_id}/test", s.operator(s.notificationTest))
	m.HandleFunc("GET /api/ui/notification/public-domain/validate", s.operator(s.notificationPublicDomain))
	m.HandleFunc("GET /api/notification/channels/{channel_id}/webhook", s.notificationWebhook)
	m.HandleFunc("POST /api/notification/channels/{channel_id}/webhook", s.notificationWebhook)
	if s.Jobs != nil {
		if e := s.Jobs.Register("notification_fallback_search", s.notificationFallbackJob); e != nil {
			panic(e)
		}
		if e := s.Jobs.Register("notification_delivery", s.notificationDeliveryJob); e != nil {
			panic(e)
		}
		if e := s.Jobs.Register("notification_command", s.notificationCommandJob); e != nil {
			panic(e)
		}
	}
	s.notificationRuntime.wg.Add(1)
	go s.notificationReceiveLoop(s.notificationRuntime)
}
func (s *Server) notificationTypes(w http.ResponseWriter, r *http.Request) {
	out := notify.Types()
	for _, kind := range out {
		kind["automaticEvents"] = append([]string(nil), notificationAutomaticEvents...)
		if kind["channelType"] == "telegram" {
			kind["automaticEvents"] = append(kind["automaticEvents"].([]string), "task_progress")
		}
	}
	writeJSON(w, 200, out)
}

func notificationDecode(row store.Row) (notify.Channel, error) {
	c := notify.Channel{ID: number(row["id"]), Name: str(row["name"]), Type: str(row["channel_type"]), Enabled: boolean(row["is_enabled"]), UseProxy: boolean(row["use_proxy"]), Config: map[string]any{}, EventsConfig: map[string]any{}}
	for _, v := range []struct {
		raw string
		dst *map[string]any
	}{{str(row["config"]), &c.Config}, {str(row["events_config"]), &c.EventsConfig}} {
		if v.raw != "" {
			d := json.NewDecoder(strings.NewReader(v.raw))
			d.UseNumber()
			if e := d.Decode(v.dst); e != nil {
				return c, errors.New("invalid stored notification JSON")
			}
		}
	}
	return c, nil
}
func notificationPublic(row store.Row) (map[string]any, error) {
	c, e := notificationDecode(row)
	if e != nil {
		return nil, e
	}
	return map[string]any{"id": c.ID, "name": c.Name, "channelType": c.Type, "isEnabled": c.Enabled, "useProxy": c.UseProxy, "config": notify.Mask(c.Config), "eventsConfig": c.EventsConfig, "createdAt": row["created_at"], "updatedAt": row["updated_at"]}, nil
}
func (s *Server) notificationChannel(ctx context.Context, id int64) (notify.Channel, error) {
	row, e := s.Store.Get(ctx, "notification_channels", id)
	if e != nil {
		return notify.Channel{}, e
	}
	return notificationDecode(row)
}
func (s *Server) notificationSchema(w http.ResponseWriter, r *http.Request) {
	fields, e := notify.Schema(r.PathValue("channel_type"))
	if e != nil {
		httpError(w, 404, e.Error())
		return
	}
	writeJSON(w, 200, fields)
}
func (s *Server) notificationList(w http.ResponseWriter, r *http.Request) {
	rows, e := s.Store.List(r.Context(), "notification_channels", nil, 1000, 0)
	if e != nil {
		httpError(w, 500, "Cannot read notification channels")
		return
	}
	out := []map[string]any{}
	for _, row := range rows {
		v, e := notificationPublic(row)
		if e != nil {
			httpError(w, 500, e.Error())
			return
		}
		out = append(out, v)
	}
	writeJSON(w, 200, out)
}

type notificationInput struct {
	Name         *string        `json:"name"`
	Type         *string        `json:"channelType"`
	Enabled      *bool          `json:"isEnabled"`
	UseProxy     *bool          `json:"useProxy"`
	Config       map[string]any `json:"config"`
	EventsConfig map[string]any `json:"eventsConfig"`
}

func (s *Server) notificationCreate(w http.ResponseWriter, r *http.Request) {
	s.notificationSave(w, r, true)
}
func (s *Server) notificationUpdate(w http.ResponseWriter, r *http.Request) {
	s.notificationSave(w, r, false)
}
func (s *Server) notificationSave(w http.ResponseWriter, r *http.Request, create bool) {
	var in notificationInput
	if e := readJSON(r, &in); e != nil {
		httpError(w, 400, "Invalid notification channel JSON")
		return
	}
	c := notify.Channel{Enabled: true, Config: map[string]any{}, EventsConfig: map[string]any{}}
	var e error
	if !create {
		c.ID, e = idParam(r, "channel_id")
		if e == nil {
			c, e = s.notificationChannel(r.Context(), c.ID)
		}
		if e != nil {
			httpError(w, 404, "Notification channel not found")
			return
		}
	}
	if in.Name != nil {
		c.Name = *in.Name
	}
	if in.Type != nil {
		c.Type = *in.Type
	}
	if in.Enabled != nil {
		c.Enabled = *in.Enabled
	}
	if in.UseProxy != nil {
		c.UseProxy = *in.UseProxy
	}
	if in.Config != nil {
		c.Config = notify.MergeSecrets(c.Config, in.Config)
	}
	if in.EventsConfig != nil {
		c.EventsConfig = in.EventsConfig
	}
	if strings.TrimSpace(c.Name) == "" || len(c.Name) > 500 {
		httpError(w, 422, "name is required and limited to500 bytes")
		return
	}
	if _, e = notify.Schema(c.Type); e != nil {
		httpError(w, 422, e.Error())
		return
	}
	if c.Enabled {
		if e = notify.Validate(c); e != nil {
			httpError(w, 422, e.Error())
			return
		}
	}
	cfg, e := json.Marshal(c.Config)
	if e != nil || len(cfg) > 65536 {
		httpError(w, 422, "Notification config exceeds size limit")
		return
	}
	events, e := json.Marshal(c.EventsConfig)
	if e != nil || len(events) > 65536 {
		httpError(w, 422, "Notification events config exceeds size limit")
		return
	}
	row := store.Row{"name": c.Name, "channel_type": c.Type, "is_enabled": c.Enabled, "use_proxy": c.UseProxy, "config": string(cfg), "events_config": string(events), "updated_at": s.now()}
	status := 200
	if create {
		count, e := s.Store.Count(r.Context(), "notification_channels", nil)
		if e != nil {
			httpError(w, 500, "Cannot count channels")
			return
		}
		if count >= 1000 {
			httpError(w, 422, "Notification channel limit reached")
			return
		}
		row["created_at"] = s.now()
		c.ID, e = s.Store.Insert(r.Context(), "notification_channels", row)
		status = 201
	} else {
		e = s.Store.Update(r.Context(), "notification_channels", c.ID, row)
	}
	if e != nil {
		httpError(w, 500, "Cannot save notification channel")
		return
	}
	stored, e := s.Store.Get(r.Context(), "notification_channels", c.ID)
	if e != nil {
		httpError(w, 500, "Cannot reload saved notification channel")
		return
	}
	out, e := notificationPublic(stored)
	if e != nil {
		httpError(w, 500, e.Error())
		return
	}
	s.notificationRuntime.forget(c.ID)
	writeJSON(w, status, out)
}
func (s *Server) notificationDelete(w http.ResponseWriter, r *http.Request) {
	id, e := idParam(r, "channel_id")
	if e != nil {
		httpError(w, 400, e.Error())
		return
	}
	c, e := s.notificationChannel(r.Context(), id)
	if e != nil {
		httpError(w, 404, "Notification channel not found")
		return
	}
	if e = s.Store.Delete(r.Context(), "notification_channels", id); e != nil {
		httpError(w, 500, "Cannot delete channel")
		return
	}
	s.Notify.Forget(c)
	s.notificationRuntime.forget(c.ID)
	w.WriteHeader(204)
}
func (s *Server) notificationTest(w http.ResponseWriter, r *http.Request) {
	id, e := idParam(r, "channel_id")
	if e != nil {
		httpError(w, 400, e.Error())
		return
	}
	c, e := s.notificationChannel(r.Context(), id)
	if e != nil {
		httpError(w, 404, "Notification channel not found")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	writeJSON(w, 200, s.Notify.Test(ctx, c))
}
func (s *Server) notificationPublicDomain(w http.ResponseWriter, r *http.Request) {
	domain := notificationPublicOrigin(s.setting(r.Context(), "custom_api_domain", ""), r.URL.Query().Get("fallback_base_url"), r.URL.Query().Get("fallback_server_url"))
	if domain == "" {
		httpError(w, 400, "Public notification images require a valid HTTPS origin without credentials, query or private literal address")
		return
	}
	probe, _ := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII=")
	name := "notification_probe_" + randomID() + ".png"
	dir := filepath.Join(s.DataDir, "image")
	if e := os.MkdirAll(dir, 0700); e != nil {
		httpError(w, 500, "Cannot create probe directory")
		return
	}
	path := filepath.Join(dir, name)
	if e := os.WriteFile(path, probe, 0600); e != nil {
		httpError(w, 500, "Cannot create probe image")
		return
	}
	defer os.Remove(path)
	out, e := notify.ProbePublicDomain(r.Context(), domain, "/data/images/"+name, probe)
	if e != nil {
		httpError(w, 400, e.Error())
		return
	}
	writeJSON(w, 200, out)
}
func (s *Server) notificationWebhook(w http.ResponseWriter, r *http.Request) {
	want := s.setting(r.Context(), "webhookApiKey", s.Config.WebhookAPIKey)
	got := r.URL.Query().Get("api_key")
	if want == "" || len(want) != len(got) || subtle.ConstantTimeCompare([]byte(want), []byte(got)) != 1 {
		httpError(w, 401, "Invalid webhook API key")
		return
	}
	id, e := idParam(r, "channel_id")
	if e != nil {
		httpError(w, 400, e.Error())
		return
	}
	c, e := s.notificationChannel(r.Context(), id)
	if e != nil || !c.Enabled {
		httpError(w, 404, "Channel not found or disabled")
		return
	}
	if r.Method == "GET" && r.URL.Query().Get("echostr") == "" {
		writeJSON(w, 200, map[string]any{"ok": true, "detail": "Webhook route and API key verified"})
		return
	}
	body, e := io.ReadAll(io.LimitReader(r.Body, (1<<20)+1))
	if e != nil || len(body) > 1<<20 {
		httpError(w, 413, "Webhook body too large")
		return
	}
	event, echo, e := s.Notify.ParseWebhook(c, r.Method, r.URL.Query(), r.Header, body)
	if e != nil {
		status := 400
		if errors.Is(e, notify.ErrForbidden) {
			status = 403
		}
		if errors.Is(e, notify.ErrUnsupported) {
			status = 422
		}
		httpError(w, status, e.Error())
		return
	}
	if r.Method == "GET" {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprint(w, echo)
		return
	}
	if event == nil {
		httpError(w, 422, "No actionable webhook event")
		return
	}
	task, duplicate, e := s.notificationQueue(r.Context(), c, *event)
	if e != nil {
		status := 503
		if errors.Is(e, notify.ErrLimit) {
			status = 429
		}
		if errors.Is(e, notify.ErrForbidden) {
			status = 403
		}
		httpError(w, status, notificationSafeError(e))
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "duplicate": duplicate, "taskId": task})
}
func (s *Server) notificationCommandJob(ctx context.Context, raw json.RawMessage, progress func(int, string)) (any, error) {
	var p notificationParams
	if e := unmarshalExactJSON(raw, &p); e != nil {
		return nil, e
	}
	c, e := s.notificationChannel(ctx, p.ChannelID)
	if e != nil {
		return nil, e
	}
	allowed, admin := notify.Authorize(c, p.Event.Sender)
	if !c.Enabled || !allowed {
		return nil, notify.ErrForbidden
	}
	p.Event.Admin = admin
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	progress(10, "Processing notification command")
	if p.Event.CallbackID != "" {
		if ack := s.Notify.AnswerCallback(ctx, c, p.Event.CallbackID, "Processing"); ack != nil {
			return nil, ack
		}
	}
	message, commandErr := s.notificationConversation(ctx, c, p.Event)
	if commandErr != nil {
		message = notify.Message{Text: notificationSafeError(commandErr), Recipient: p.Event.ChatID}
	}
	if sendErr := s.notificationSend(ctx, c, message); sendErr != nil {
		return nil, errors.New(notificationSafeError(sendErr))
	}
	progress(100, "Reply accepted by notification service")
	if commandErr != nil {
		return nil, errors.New(notificationSafeError(commandErr))
	}
	return map[string]any{"replied": true}, nil
}

func (s *Server) notificationCommand(ctx context.Context, event notify.Update) (string, error) {
	text := strings.TrimSpace(event.Text)
	args := strings.Fields(text)
	if len(args) == 0 {
		return "", errors.New("empty command")
	}
	command := strings.TrimPrefix(strings.ToLower(strings.Split(args[0], "@")[0]), "/")
	if strings.HasPrefix(command, "menu:") {
		command = strings.TrimPrefix(command, "menu:")
	}
	switch command {
	case "start", "help":
		return "AniDan commands:\n/status\n/library [keyword]\n/tasks\n/task <id>\n/search <provider> <keyword>\nAdmins: /refresh <episodeId>, /cancel <taskId>, /import <provider> <mediaId> <title>\nUse /start for interactive menus; /cancel ends the current conversation.", nil
	case "status":
		anime, e := s.Store.Count(ctx, "anime", nil)
		if e != nil {
			return "", e
		}
		eps, e := s.Store.Count(ctx, "episode", nil)
		if e != nil {
			return "", e
		}
		return fmt.Sprintf("AniDan %s\nAnime: %d\nEpisodes: %d\nTasks: %d", Version, anime, eps, len(s.Jobs.List())), nil
	case "library":
		var rows []store.Row
		var e error
		if len(args) > 1 {
			rows, e = s.localSearch(ctx, strings.Join(args[1:], " "))
		} else {
			rows, e = s.Store.List(ctx, "anime", nil, 20, 0)
		}
		if e != nil {
			return "", e
		}
		lines := []string{"Library"}
		for _, row := range rows[:min(len(rows), 20)] {
			lines = append(lines, fmt.Sprintf("%s: %s", str(row["id"]), str(row["title"])))
		}
		if len(lines) == 1 {
			lines = append(lines, "No matching entries")
		}
		return strings.Join(lines, "\n"), nil
	case "tasks":
		lines := []string{"Tasks"}
		tasks := s.Jobs.List()
		for _, task := range tasks[:min(len(tasks), 15)] {
			lines = append(lines, fmt.Sprintf("%s %s %d%%", task.ID, task.Status, task.Progress))
		}
		return strings.Join(lines, "\n"), nil
	case "task":
		if len(args) != 2 {
			return "", errors.New("use /task <id>")
		}
		task, ok := s.Jobs.Get(args[1])
		if !ok {
			return "", errors.New("task not found")
		}
		return fmt.Sprintf("%s\n%s %d%%", task.ID, task.Status, task.Progress), nil
	case "search":
		if len(args) < 3 {
			return "", errors.New("use /search <provider> <keyword>")
		}
		out, e := s.Providers.Search(ctx, strings.Join(args[2:], " "), args[1])
		lines := []string{"Search results"}
		for _, v := range out[:min(len(out), 10)] {
			lines = append(lines, fmt.Sprintf("%s %s: %s", v.Provider, v.ID, v.Title))
		}
		if e != nil {
			lines = append(lines, "Some results unavailable: "+e.Error())
		}
		if len(out) == 0 && e == nil {
			lines = append(lines, "No matching results")
		}
		return strings.Join(lines, "\n"), nil
	case "refresh":
		if !event.Admin {
			return "", notify.ErrForbidden
		}
		if len(args) != 2 {
			return "", errors.New("use /refresh <episodeId>")
		}
		id, e := strconv.ParseInt(args[1], 10, 64)
		if e != nil || id < 1 {
			return "", errors.New("invalid episode ID")
		}
		if _, e = s.Store.Get(ctx, "episode", id); e != nil {
			return "", errors.New("episode not found")
		}
		task, e := s.Jobs.SubmitRegistered("fetch_comments", map[string]any{"episodeId": id})
		return "Refresh queued: " + task, e
	case "cancel":
		if !event.Admin {
			return "", notify.ErrForbidden
		}
		if len(args) != 2 {
			return "", errors.New("use /cancel <taskId>")
		}
		e := s.Jobs.Cancel(args[1])
		return "Cancellation requested: " + args[1], e
	case "import":
		if !event.Admin {
			return "", notify.ErrForbidden
		}
		if len(args) < 4 {
			return "", errors.New("use /import <provider> <mediaId> <title>")
		}
		if _, ok := s.Providers.Get(args[1]); !ok {
			return "", errors.New("unknown provider")
		}
		task, e := s.Jobs.SubmitRegistered("generic_import", ImportRequest{Provider: args[1], MediaID: args[2], Title: strings.Join(args[3:], " "), Type: "tv_series", Season: 1})
		return "Import queued: " + task, e
	default:
		return "", errors.New("unsupported command; use /help")
	}
}

// NotifyEvent is the explicit server-side event hook. Only channels that opted
// into this event (or '*') are eligible. It performs no implicit subscription.
func (s *Server) NotifyEvent(ctx context.Context, event, title, text string) error {
	return s.notificationDeliverEvent(ctx, notify.Event{Type: event, Title: title, Text: text}, libPosterClient())
}

func (s *Server) notificationDeliverEvent(ctx context.Context, event notify.Event, client libHTTPDoer) error {
	if event.Type == "" || len(event.Title)+len(event.Text) > 16000 || len(event.ImageURL) > 2048 {
		return errors.New("invalid notification event")
	}
	rows, e := s.notificationEnabledRows(ctx)
	if e != nil {
		return e
	}
	errs := []error{}
	var preparedImage []byte
	imageAttempted := false
	for _, row := range rows {
		c, e := notificationDecode(row)
		if e != nil {
			errs = append(errs, e)
			continue
		}
		if !notify.Bool(c.EventsConfig[event.Type]) && !notify.Bool(c.EventsConfig["*"]) {
			continue
		}
		m := notify.Message{Title: event.Title, Text: event.Text}
		if event.ImageURL != "" && notificationImageMode(c) != "text" && c.Type != "serverchan3" {
			if !imageAttempted {
				imageAttempted = true
				preparedImage, _ = s.notificationPreparePoster(ctx, event.ImageURL, client)
				if ctx.Err() != nil {
					return ctx.Err()
				}
			}
			m.Image = preparedImage
			m.ImageShareable = len(preparedImage) > 0
		}
		if e = s.notificationSend(ctx, c, m); e != nil {
			errs = append(errs, fmt.Errorf("notification channel %d: %w", c.ID, e))
		}
	}
	return errors.Join(errs...)
}

// emitNotification queues opted-in delivery on the existing bounded durable job
// manager. The delivery job never emits its own notification, avoiding recursion.
func (s *Server) emitNotification(ctx context.Context, event notify.Event) error {
	return s.collectNotificationEvent(ctx, event, "", "", false)
}

func (s *Server) notificationDeliveryJob(ctx context.Context, raw json.RawMessage, progress func(int, string)) (any, error) {
	var envelope struct {
		Event *notify.Event `json:"event"`
	}
	if err := unmarshalExactJSON(raw, &envelope); err != nil {
		return nil, err
	}
	if envelope.Event != nil {
		var batch notify.DeliveryBatch
		if err := unmarshalExactJSON(raw, &batch); err != nil {
			return nil, err
		}
		ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		defer cancel()
		if err := s.deliverNotificationBatch(ctx, batch); err != nil {
			return nil, err
		}
		return map[string]any{"processed": true, "event": batch.Event.Type}, nil
	}
	return notificationLegacyReview()
}

func notificationLegacyReview() (any, error) {
	return job.DiagnosticResult{"reviewRequired": true, "reason": "Legacy notice has no verifiable audience and attempt identity", "action": "Review the retained original task and intended recipient. Create a fresh explicit notice only if still needed; ordinary import/refresh jobs are unaffected."}, errors.New("Legacy notification requires manual review; automatic delivery is disabled")
}
func notificationTaskNeedsReview(task job.Task) bool {
	if task.Kind == "notification_delivery" {
		var p struct {
			Event *notify.Event `json:"event"`
			ID    string        `json:"id"`
		}
		return unmarshalExactJSON(task.Params, &p) != nil || p.Event == nil || p.ID == ""
	}
	if task.Kind == "notification_fallback_search" {
		var p notificationFallbackParams
		return unmarshalExactJSON(task.Params, &p) != nil || p.NoticeID == ""
	}
	return false
}
