// SPDX-License-Identifier: AGPL-3.0-only
// Conversation/receive compatibility informed by Misaka 01751526f6e4154bcc8f517481d02b68cb2684a9.
package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/AniBakaBaka/AniDan/internal/job"
	"github.com/AniBakaBaka/AniDan/internal/notify"
)

const notificationSessionTTL = 15 * time.Minute
const notificationMaxSessions = 256

type notificationOwner struct {
	Channel      int64
	Sender, Chat string
}
type notificationSession struct {
	runtime      *notificationRuntime
	owner        notificationOwner
	editedImport *notificationEditedImport
	editedBytes  int64
	login        *notificationLogin
	gate         chan struct{}
	used         time.Time
	leases       int
	actions      map[string]notificationAction
	choices      []string
	state, input string
	data         notificationAction
	results      []notificationSearchResult
}
type notificationRate struct {
	at    time.Time
	count int
}
type notificationPoller struct {
	fingerprint     string
	cancel          context.CancelFunc
	done            chan struct{}
	status, message string
}
type notificationRuntime struct {
	closed            bool
	editedImportBytes int64
	wg                sync.WaitGroup
	mu                sync.Mutex
	sessions          map[notificationOwner]*notificationSession
	rates             map[notificationOwner]notificationRate
	pollers           map[int64]*notificationPoller
	now               func() time.Time
	service           *notify.Service
}

func newNotificationRuntime(service *notify.Service) *notificationRuntime {
	return &notificationRuntime{sessions: map[notificationOwner]*notificationSession{}, rates: map[notificationOwner]notificationRate{}, pollers: map[int64]*notificationPoller{}, now: time.Now, service: service}
}
func notificationKey(c notify.Channel, u notify.Update) notificationOwner {
	return notificationOwner{c.ID, u.Sender, u.ChatID}
}
func (n *notificationRuntime) session(ctx context.Context, key notificationOwner) (*notificationSession, func(), error) {
	n.mu.Lock()
	if n.closed {
		n.mu.Unlock()
		return nil, nil, context.Canceled
	}
	now := n.now()
	for k, v := range n.sessions {
		if v.leases == 0 {
			if v.editedImport != nil && !now.Before(v.editedImport.expires) {
				n.clearEditedImportLocked(v)
				v.actions = map[string]notificationAction{}
				v.choices, v.state = nil, ""
			}
			if now.Sub(v.used) >= notificationSessionTTL {
				n.clearEditedImportLocked(v)
				delete(n.sessions, k)
			}
		}
	}
	v := n.sessions[key]
	if v == nil {
		if len(n.sessions) >= notificationMaxSessions {
			n.mu.Unlock()
			return nil, nil, notify.ErrLimit
		}
		v = &notificationSession{runtime: n, owner: key, gate: make(chan struct{}, 1), actions: map[string]notificationAction{}, used: now}
		n.sessions[key] = v
	}
	v.leases++
	v.used = now
	n.mu.Unlock()
	release := func() {
		n.mu.Lock()
		v.leases--
		v.used = n.now()
		if v.leases == 0 && (n.closed || n.sessions[key] != v) {
			n.clearEditedImportLocked(v)
		}
		n.mu.Unlock()
	}
	select {
	case v.gate <- struct{}{}:
		return v, func() { <-v.gate; release() }, nil
	case <-ctx.Done():
		release()
		return nil, nil, ctx.Err()
	}
}
func (n *notificationRuntime) admit(key notificationOwner) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	now := n.now()
	for k, v := range n.rates {
		if now.Sub(v.at) >= time.Minute {
			delete(n.rates, k)
		}
	}
	v, ok := n.rates[key]
	if !ok {
		if len(n.rates) >= notificationMaxSessions {
			return notify.ErrLimit
		}
		v.at = now
	}
	if v.count >= 30 {
		return notify.ErrLimit
	}
	v.count++
	n.rates[key] = v
	return nil
}
func (n *notificationRuntime) forget(id int64) {
	n.mu.Lock()
	defer n.mu.Unlock()
	for k, v := range n.sessions {
		if k.Channel == id {
			delete(n.sessions, k)
			// An active lease still owns its snapshot until it returns. Keep its
			// reservation charged even after removing the public session entry.
			if v.leases == 0 {
				n.clearEditedImportLocked(v)
			}
		}
	}
	for k := range n.rates {
		if k.Channel == id {
			delete(n.rates, k)
		}
	}
	if p := n.pollers[id]; p != nil {
		p.cancel()
		delete(n.pollers, id)
	}
}

// Update IDs are scoped to a bot, not merely a database channel. Telegram's
// numeric bot identity survives token rotation; replacing a bot must not inherit
// the old bot's receipt/cursor namespace.
func notificationIdentity(c notify.Channel) string {
	identity := notify.Str(c.Config["bot_token"])
	if c.Type == "telegram" {
		identity = strings.SplitN(identity, ":", 2)[0]
	}
	if c.Type == "wechat" {
		identity = notify.Str(c.Config["corp_id"]) + ":" + notify.Str(c.Config["agent_id"])
	}
	sum := sha256.Sum256([]byte(c.Type + "\x00" + identity))
	return hex.EncodeToString(sum[:16])
}
func (s *Server) notificationQueue(ctx context.Context, c notify.Channel, event notify.Update) (string, bool, error) {
	allowed, admin := notify.Authorize(c, event.Sender)
	if !c.Enabled || !allowed {
		return "", false, notify.ErrForbidden
	}
	event.Admin = admin
	if event.ID == "" || len(event.ID) > 128 || len(event.Text) > 16384 || len(event.Sender) > 128 || len(event.ChatID) > 128 {
		return "", false, notify.ErrLimit
	}
	sum := sha256.Sum256([]byte(fmt.Sprintf("%d:%s:%s", c.ID, notificationIdentity(c), event.ID)))
	key := "notification_event_" + hex.EncodeToString(sum[:])
	// Check the persisted receipt before charging a retry against the command quota.
	if row, e := s.Store.Get(ctx, "cache_data", key); e == nil {
		if expires, parseErr := s.authDate(row["expires_at"]); parseErr == nil && time.Now().Before(expires) {
			raw := []byte(str(row["cache_value"]))
			var receipt map[string]json.RawMessage
			if len(raw) <= job.MaxResultBytes && json.Unmarshal(raw, &receipt) == nil && receipt != nil {
				id := ""
				value, hasID := receipt["taskId"]
				if !hasID || (json.Unmarshal(value, &id) == nil && len(id) <= 500) {
					return id, true, nil
				}
			}
		}
	}
	if e := s.notificationRuntime.admit(notificationKey(c, event)); e != nil {
		return "", false, e
	}
	return s.Jobs.SubmitWithReceipt(ctx, "notification_command", notificationParams{ChannelID: c.ID, Event: event}, job.SubmitOptions{}, key, time.Now().Add(7*24*time.Hour))
}
func (s *Server) notificationReceiveLoop(n *notificationRuntime) {
	defer n.wg.Done()
	// Let construction finish before admitting received work. No durable job worker is
	// occupied by a long poll, and server cancellation stops both reconciliation and polls.
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	for {
		select {
		case <-s.ctx.Done():
			n.mu.Lock()
			for _, p := range n.pollers {
				p.cancel()
			}
			n.mu.Unlock()
			return
		case <-timer.C:
			s.notificationReconcilePollers(n)
			timer.Reset(5 * time.Second)
		}
	}
}
func (s *Server) notificationReconcilePollers(n *notificationRuntime) {
	rows, e := s.Store.List(s.ctx, "notification_channels", nil, 1000, 0)
	if e != nil {
		return
	}
	wanted := map[int64]notify.Channel{}
	for _, r := range rows {
		c, e := notificationDecode(r)
		if e == nil && c.Enabled && notify.Str(c.Config["mode"]) == "polling" && (c.Type == "telegram" || c.Type == "serverchan3") {
			wanted[c.ID] = c
		}
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.closed {
		return
	}
	for id, p := range n.pollers {
		if _, ok := wanted[id]; !ok {
			p.cancel()
			delete(n.pollers, id)
		}
	}
	for id, c := range wanted {
		raw, _ := json.Marshal(c)
		sum := sha256.Sum256(raw)
		fingerprint := hex.EncodeToString(sum[:])
		old := n.pollers[id]
		if old != nil && old.fingerprint == fingerprint {
			continue
		}
		if old == nil && len(n.pollers) >= 16 {
			continue
		}
		ctx, cancel := context.WithCancel(s.ctx)
		p := &notificationPoller{fingerprint: fingerprint, cancel: cancel, done: make(chan struct{}), status: "starting"}
		if old != nil {
			old.cancel()
		}
		n.pollers[id] = p
		n.wg.Add(1)
		go func(c notify.Channel, p, old *notificationPoller) {
			defer n.wg.Done()
			defer close(p.done)
			if old != nil {
				select {
				case <-old.done:
				case <-ctx.Done():
					return
				}
			}
			s.notificationPoll(ctx, n, c, p)
		}(c, p, old)
	}
}
func (s *Server) notificationPoll(ctx context.Context, n *notificationRuntime, c notify.Channel, p *notificationPoller) {
	key := fmt.Sprintf("notification_offset_%d_%s", c.ID, notificationIdentity(c))
	offset := int64(0)
	row, e := s.Store.Get(ctx, "cache_data", key)
	if e == nil {
		offset, _ = strconv.ParseInt(str(row["cache_value"]), 10, 64)
	}
	setStatus := func(state, msg string) { n.mu.Lock(); p.status, p.message = state, msg; n.mu.Unlock() }
	save := func(next int64) error {
		if next <= offset {
			return nil
		}
		e := s.cachePut(ctx, key, "notification_offsets", next, 10*365*24*time.Hour)
		if e == nil {
			offset = next
		}
		return e
	}
	delay := time.Second
	for ctx.Err() == nil {
		setStatus("polling", "")
		updates, next, e := n.service.Poll(ctx, c, offset)
		if e == nil {
			for _, u := range updates {
				// Re-read current authorization before queueing; config revocation takes
				// effect immediately, even while an old HTTP long poll is completing.
				current, load := s.notificationChannel(ctx, c.ID)
				if load != nil || !current.Enabled || notify.Str(current.Config["mode"]) != "polling" {
					return
				}
				if _, _, e = s.notificationQueue(ctx, current, u); errors.Is(e, notify.ErrForbidden) {
					e = nil
				}
				if e != nil {
					break
				}
				id, parse := strconv.ParseInt(u.ID, 10, 64)
				if parse != nil {
					e = errors.New("invalid update ID")
					break
				}
				if e = save(id + 1); e != nil {
					break
				}
			}
			if e == nil {
				e = save(next)
			}
		}
		if ctx.Err() != nil {
			return
		}
		if e != nil {
			setStatus("backoff", notificationSafeError(e))
			delay = min(delay*2, 30*time.Second)
		} else {
			delay = time.Second
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}
func (s *Server) notificationReceiverStatus(w http.ResponseWriter, r *http.Request) {
	id, e := idParam(r, "channel_id")
	if e != nil {
		httpError(w, 400, "Invalid channel ID")
		return
	}
	c, e := s.notificationChannel(r.Context(), id)
	if e != nil {
		httpError(w, 404, "Channel not found")
		return
	}
	state, msg := "stopped", ""
	s.notificationRuntime.mu.Lock()
	if p := s.notificationRuntime.pollers[id]; p != nil {
		state, msg = p.status, p.message
	}
	s.notificationRuntime.mu.Unlock()
	if c.Enabled && notify.Str(c.Config["mode"]) == "polling" && state == "stopped" {
		msg = "Receiver pending reconciliation or the 16-channel polling limit was reached"
	}
	writeJSON(w, 200, map[string]any{"mode": notify.Str(c.Config["mode"]), "state": state, "message": msg, "capabilities": notify.Capabilities(c.Type)})
}
func (s *Server) notificationRegister(w http.ResponseWriter, r *http.Request) {
	id, e := idParam(r, "channel_id")
	if e != nil {
		httpError(w, 400, "Invalid channel ID")
		return
	}
	c, e := s.notificationChannel(r.Context(), id)
	if e != nil {
		httpError(w, 404, "Channel not found")
		return
	}
	var in struct {
		Action  string `json:"action"`
		Confirm string `json:"confirm"`
	}
	if readJSON(r, &in) != nil || in.Confirm != "REGISTER_NOTIFICATION_BOT" {
		httpError(w, 400, "Confirm REGISTER_NOTIFICATION_BOT and action menus, webhook, or delete_webhook")
		return
	}
	switch in.Action {
	case "menus":
		e = s.Notify.RegisterCommands(r.Context(), c, []notify.Command{{Command: "start", Description: "Open AniDan menu"}, {Command: "search", Description: "Search sources"}, {Command: "library", Description: "Browse library"}, {Command: "tasks", Description: "View tasks"}, {Command: "cancel", Description: "Cancel current conversation"}})
	case "delete_webhook":
		e = s.Notify.DeleteWebhook(r.Context(), c)
	case "webhook":
		if notify.Str(c.Config["mode"]) != "webhook" {
			httpError(w, 409, "Set receive mode to webhook before registration")
			return
		}
		base := notify.Str(c.Config["webhook_base_url"])
		normalized, err := notify.PublicDomain(base)
		u, parseErr := url.Parse(normalized)
		if err != nil || parseErr != nil || u.Scheme != "https" {
			httpError(w, 422, "A public HTTPS webhook_base_url is required")
			return
		}
		key := s.setting(r.Context(), "webhookApiKey", s.Config.WebhookAPIKey)
		if key == "" {
			httpError(w, 422, "A webhook API key is required")
			return
		}
		u.Path = fmt.Sprintf("/api/notification/channels/%d/webhook", id)
		u.RawQuery = url.Values{"api_key": {key}}.Encode()
		e = s.Notify.RegisterWebhook(r.Context(), c, u.String())
	default:
		httpError(w, 422, "Unknown registration action")
		return
	}
	if e != nil {
		httpError(w, 422, notificationSafeError(e))
		return
	}
	writeJSON(w, 200, map[string]any{"success": true, "action": in.Action})
}

// External/provider errors can include URLs or credentials. Conversations and
// durable tasks expose only reviewed categories, never arbitrary error strings.
func notificationSafeError(e error) string {
	if errors.Is(e, notify.ErrPartialDelivery) {
		return "Part of this notification was accepted; inspect the channel before retrying"
	}
	if e == nil {
		return ""
	}
	if errors.Is(e, notify.ErrForbidden) {
		return "Not authorized"
	}
	if errors.Is(e, notify.ErrLimit) {
		return "Notification resource or rate limit reached"
	}
	if errors.Is(e, notify.ErrUnsupported) {
		return "This notification service does not support that operation"
	}
	if errors.Is(e, context.Canceled) {
		return "Operation canceled"
	}
	if errors.Is(e, context.DeadlineExceeded) {
		return "Operation timed out"
	}
	return "Operation failed; check the authenticated AniDan UI for details"
}

// closeNotifications is called after server context cancellation and before
// closing Store. The closed flag prevents reconciliation from adding a goroutine
// while shutdown waits; removed/replaced pollers remain tracked by the waitgroup.
func (s *Server) closeNotifications() {
	n := s.notificationRuntime
	if n == nil {
		return
	}
	n.mu.Lock()
	n.closed = true
	for key, v := range n.sessions {
		delete(n.sessions, key)
		if v.leases == 0 {
			n.clearEditedImportLocked(v)
		}
	}
	for _, p := range n.pollers {
		p.cancel()
	}
	n.mu.Unlock()
	n.wg.Wait()
}
