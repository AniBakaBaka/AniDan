// SPDX-License-Identifier: AGPL-3.0-only
package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/AniBakaBaka/AniDan/internal/notify"
)

const (
	notificationEditedImportTTL            = 5 * time.Minute
	notificationEditedImportMaxBytes int64 = 4 << 20
	// Reserve before listing, including a second bounded validation snapshot,
	// selected row structs, mutable selection, action generation and metadata.
	// The snapshot budget already conservatively includes row backing storage.
	notificationEditedImportReservation int64 = 2*notificationImportMaxBytes + 16<<10
)

// This is accessed under the existing conversation gate. Its reservation is
// guarded by runtime.mu and survives removal from the sessions map while a
// lease is active. The source snapshot is immutable; only these bounded fields
// can be edited. Callback payloads contain only opaque action keys.
type notificationEditedImport struct {
	owner                     notificationOwner
	id, channelFingerprint    string
	snapshot                  *notificationImportSnapshot
	selected                  []bool
	mediaType                 string
	season, page, resultsPage int
	expires                   time.Time
	revision                  uint64
	reviewed                  bool
}

// No goroutine is left waiting on a mutex after the requesting command ends.
func notificationEditedImportLock(ctx context.Context, mu *sync.Mutex) error {
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for !mu.TryLock() {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
	if err := ctx.Err(); err != nil {
		mu.Unlock()
		return err
	}
	return nil
}

func (n *notificationRuntime) clearEditedImportLocked(v *notificationSession) {
	n.editedImportBytes -= v.editedBytes
	v.editedBytes = 0
	v.editedImport = nil
}

func notificationClearEditedImport(v *notificationSession) {
	if v.runtime == nil {
		v.editedImport = nil
		v.editedBytes = 0
		return
	}
	v.runtime.mu.Lock()
	v.runtime.clearEditedImportLocked(v)
	v.runtime.mu.Unlock()
}

func notificationEditedImportFingerprint(c notify.Channel) (string, error) {
	// Empty decoded maps and absent caller maps have identical semantics.
	config, events := c.Config, c.EventsConfig
	if config == nil {
		config = map[string]any{}
	}
	if events == nil {
		events = map[string]any{}
	}
	raw, err := json.Marshal(struct {
		ID                int64
		Type              string
		Enabled, UseProxy bool
		Config, Events    map[string]any
	}{c.ID, c.Type, c.Enabled, c.UseProxy, config, events})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

// Ordinary imports permit existing authorized group-chat administration. QR
// login's additional private-chat requirement is intentionally not reused.
func (s *Server) notificationEditedImportChannel(ctx context.Context, c notify.Channel, u notify.Update) (notify.Channel, string, error) {
	row, err := s.Store.Get(ctx, "notification_channels", c.ID)
	if err != nil {
		return notify.Channel{}, "", notify.ErrForbidden
	}
	// Unlike a streaming decoder, validation rejects trailing garbage or a
	// second JSON value in persisted configuration before checking roles.
	for _, field := range []string{"config", "events_config"} {
		raw := str(row[field])
		if len(raw) > 64<<10 || !utf8.ValidString(raw) || (strings.TrimSpace(raw) != "" && !json.Valid([]byte(raw))) {
			return notify.Channel{}, "", notify.ErrForbidden
		}
	}
	current, err := notificationDecode(row)
	if err != nil || !current.Enabled || current.Type != c.Type || u.Sender == "" || u.ChatID == "" {
		return notify.Channel{}, "", notify.ErrForbidden
	}
	allowed, admin := notify.Authorize(current, u.Sender)
	if !allowed || !admin {
		return notify.Channel{}, "", notify.ErrForbidden
	}
	switch current.Type {
	case "telegram", "serverchan3", "wechat":
	default:
		return notify.Channel{}, "", notify.ErrUnsupported
	}
	fingerprint, err := notificationEditedImportFingerprint(current)
	original, oldErr := notificationEditedImportFingerprint(c)
	if err != nil || oldErr != nil || original != fingerprint {
		return notify.Channel{}, "", notify.ErrForbidden
	}
	return current, fingerprint, nil
}

func (s *Server) notificationEditedImportCurrent(c notify.Channel, u notify.Update, v *notificationSession, d *notificationEditedImport) bool {
	n := s.notificationRuntime
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.editedImportCurrentLocked(c, u, v, d)
}

func (n *notificationRuntime) editedImportCurrentLocked(c notify.Channel, u notify.Update, v *notificationSession, d *notificationEditedImport) bool {
	return !n.closed && n.sessions[notificationKey(c, u)] == v && d != nil && v.editedImport == d &&
		d.owner == notificationKey(c, u) && n.now().Before(d.expires)
}

func (s *Server) notificationEditedImportEnded(c notify.Channel, u notify.Update, v *notificationSession, text string) notify.Message {
	notificationClearEditedImport(v)
	v.state = ""
	return s.notificationRender(c, u, v, text,
		notificationChoice("Back to results", "search_page", ""), notificationChoice("Home", "home", ""))
}

func notificationEditedImportChoice(d *notificationEditedImport, label, kind string) notificationOption {
	return notificationOption{label, notificationAction{Kind: "edit_import_" + kind, ID: d.id, Revision: d.revision}}
}

func notificationEditedImportCount(d *notificationEditedImport) int {
	count := 0
	for _, chosen := range d.selected {
		if chosen {
			count++
		}
	}
	return count
}

func (s *Server) notificationEditedImportPage(c notify.Channel, u notify.Update, v *notificationSession, d *notificationEditedImport, page int) notify.Message {
	v.state = ""
	d.reviewed = false
	d.page = max(0, min(page, max((len(d.selected)-1)/5, 0)))
	start, end := d.page*5, min(d.page*5+5, len(d.selected))
	opts := make([]notificationOption, 0, 10)
	for i := start; i < end; i++ {
		ep, mark := d.snapshot.Episodes[i], "[ ]"
		if d.selected[i] {
			mark = "[x]"
		}
		opt := notificationEditedImportChoice(d, fmt.Sprintf("%s %d. %s", mark, ep.Index, notificationClip(ep.Title, 36)), "toggle")
		opt.Action.Index = i
		opts = append(opts, opt)
	}
	if d.page > 0 {
		opt := notificationEditedImportChoice(d, "Previous", "page")
		opt.Action.Page = d.page - 1
		opts = append(opts, opt)
	}
	if end < len(d.selected) {
		opt := notificationEditedImportChoice(d, "Next", "page")
		opt.Action.Page = d.page + 1
		opts = append(opts, opt)
	}
	opts = append(opts, notificationEditedImportChoice(d, "Settings", "settings"), notificationEditedImportChoice(d, "Review import", "review"), notificationEditedImportChoice(d, "Back to results", "back"))
	return s.notificationRender(c, u, v, fmt.Sprintf("Edit import: %s\nSelected %d/%d; page %d/%d\nType: %s; season: %d\nToggle episodes below. Original source order and indices are retained. This draft expires five minutes after opening; /cancel discards it.", notificationClip(d.snapshot.Request.Title, 150), notificationEditedImportCount(d), len(d.selected), d.page+1, (len(d.selected)+4)/5, d.mediaType, d.season), opts...)
}

func (s *Server) notificationEditedImportSettings(c notify.Channel, u notify.Update, v *notificationSession, d *notificationEditedImport) notify.Message {
	v.state = ""
	d.reviewed = false
	return s.notificationRender(c, u, v, fmt.Sprintf("Import settings\nSelected %d/%d\nType: %s; season: %d\nOnly selection, media type and season can be changed.", notificationEditedImportCount(d), len(d.selected), d.mediaType, d.season),
		notificationEditedImportChoice(d, "Select all", "all"), notificationEditedImportChoice(d, "Select none", "none"),
		notificationEditedImportChoice(d, "Type: movie", "movie"), notificationEditedImportChoice(d, "Type: tv_series", "tv_series"),
		notificationEditedImportChoice(d, "Edit season", "season"), notificationEditedImportChoice(d, "Back to episodes", "episodes"),
		notificationEditedImportChoice(d, "Review import", "review"), notificationEditedImportChoice(d, "Back to results", "back"))
}

func (s *Server) notificationEditedImportAction(ctx context.Context, c notify.Channel, u notify.Update, v *notificationSession, a notificationAction) (notify.Message, error) {
	current, fingerprint, err := s.notificationEditedImportChannel(ctx, c, u)
	if err != nil {
		notificationClearEditedImport(v)
		return notify.Message{}, err
	}
	c = current
	if a.Kind == "edit_import_start" {
		notificationClearEditedImport(v)
		if a.Index < 0 || a.Index >= len(v.results) || !s.notificationCanImport(v.results[a.Index].Provider) {
			return notify.Message{}, notify.ErrForbidden
		}
		n := s.notificationRuntime
		n.mu.Lock()
		if n.closed || n.sessions[notificationKey(c, u)] != v {
			n.mu.Unlock()
			return s.notificationEditedImportEnded(c, u, v, "This conversation was canceled or replaced. Open a new search."), nil
		}
		if n.editedImportBytes > notificationEditedImportMaxBytes-notificationEditedImportReservation {
			n.mu.Unlock()
			return s.notificationEditedImportEnded(c, u, v, "Import editor memory is busy. Close another draft or try again after it expires. No import was queued."), nil
		}
		d := &notificationEditedImport{owner: notificationKey(c, u), id: randomID(), channelFingerprint: fingerprint, resultsPage: a.Page, expires: n.now().Add(notificationEditedImportTTL), revision: 1}
		v.editedImport, v.editedBytes = d, notificationEditedImportReservation
		n.editedImportBytes += v.editedBytes
		n.mu.Unlock()
		snapshot, buildErr := s.buildNotificationImportSnapshot(ctx, v.results[a.Index])
		if _, after, authErr := s.notificationEditedImportChannel(ctx, c, u); authErr != nil || after != fingerprint {
			notificationClearEditedImport(v)
			return notify.Message{}, notify.ErrForbidden
		}
		if !s.notificationEditedImportCurrent(c, u, v, d) || ctx.Err() != nil {
			return s.notificationEditedImportEnded(c, u, v, "This edited import expired, was canceled, or was replaced. No import was queued."), nil
		}
		if buildErr != nil {
			return s.notificationEditedImportEnded(c, u, v, "The source preview is unavailable, invalid, empty or too large. The editor supports at most 1000 source episodes and 256 KiB of preview data; no episodes were truncated and no import was queued."), nil
		}
		d.snapshot, d.mediaType, d.season = snapshot, snapshot.Request.Type, snapshot.Request.Season
		d.selected = make([]bool, len(snapshot.Episodes))
		for i := range d.selected {
			d.selected[i] = true
		}
		return s.notificationEditedImportPage(c, u, v, d, 0), nil
	}
	d := v.editedImport
	if d == nil || d.snapshot == nil || a.ID != d.id || a.Revision != d.revision || fingerprint != d.channelFingerprint || !s.notificationEditedImportCurrent(c, u, v, d) {
		return s.notificationEditedImportEnded(c, u, v, "This edited import expired, changed, or was canceled. Open a new edit from the results."), nil
	}
	v.state = ""
	switch a.Kind {
	case "edit_import_back":
		page := d.resultsPage
		notificationClearEditedImport(v)
		return s.notificationSearchPage(c, u, v, page), nil
	case "edit_import_page":
		return s.notificationEditedImportPage(c, u, v, d, a.Page), nil
	case "edit_import_episodes":
		return s.notificationEditedImportPage(c, u, v, d, d.page), nil
	case "edit_import_toggle":
		if a.Index < 0 || a.Index >= len(d.selected) {
			return notify.Message{}, notify.ErrForbidden
		}
		d.selected[a.Index] = !d.selected[a.Index]
		d.revision++
		return s.notificationEditedImportPage(c, u, v, d, d.page), nil
	case "edit_import_settings":
		return s.notificationEditedImportSettings(c, u, v, d), nil
	case "edit_import_all", "edit_import_none":
		for i := range d.selected {
			d.selected[i] = a.Kind == "edit_import_all"
		}
		d.revision++
		return s.notificationEditedImportSettings(c, u, v, d), nil
	case "edit_import_movie", "edit_import_tv_series":
		d.mediaType = strings.TrimPrefix(a.Kind, "edit_import_")
		d.revision++
		return s.notificationEditedImportSettings(c, u, v, d), nil
	case "edit_import_season":
		d.reviewed = false
		v.state = "edit_import_season"
		opts := []notificationOption{}
		if c.Type == "telegram" {
			opts = append(opts, notificationEditedImportChoice(d, "Back to episodes", "episodes"), notificationEditedImportChoice(d, "Back to results", "back"))
		}
		// Numbered transport choices would be ambiguous with season 1 or 2.
		return s.notificationRender(c, u, v, "Send a season number from 0 to 9999. Zero explicitly means season 0. /cancel discards this draft.", opts...), nil
	case "edit_import_review":
		if notificationEditedImportCount(d) == 0 {
			m := s.notificationEditedImportSettings(c, u, v, d)
			m.Text = "Select at least one episode before review.\n" + m.Text
			return m, nil
		}
		d.reviewed = true
		confirm := notificationEditedImportChoice(d, "Confirm edited import", "confirm")
		confirm.Action.Confirm = true
		return s.notificationRender(c, u, v, fmt.Sprintf("Review edited import\n%s\nProvider: %s\nSelected: %d/%d episodes in original source order and indices\nType: %s; season: %d\nOrdinary configured title/type/season recognition and metadata enrichment still apply. Confirm queues the selected episodes after a fresh source check. A queue acknowledgement is not completion; upstream state may change after admission.", notificationClipBytes(d.snapshot.Request.Title, 160), notificationClipBytes(d.snapshot.Request.Provider, 80), notificationEditedImportCount(d), len(d.selected), d.mediaType, d.season), confirm, notificationEditedImportChoice(d, "Back to episodes", "episodes"), notificationEditedImportChoice(d, "Back to results", "back")), nil
	case "edit_import_confirm":
		if !a.Confirm || !d.reviewed {
			return notify.Message{}, notify.ErrForbidden
		}
		request, validateErr := s.validateNotificationImportSnapshot(ctx, d.snapshot, d.selected, d.mediaType, d.season)
		if _, after, authErr := s.notificationEditedImportChannel(ctx, c, u); authErr != nil || after != d.channelFingerprint {
			notificationClearEditedImport(v)
			return notify.Message{}, notify.ErrForbidden
		}
		if !s.notificationEditedImportCurrent(c, u, v, d) || ctx.Err() != nil {
			return s.notificationEditedImportEnded(c, u, v, "This edited import expired, was canceled, or was replaced. No import was queued."), nil
		}
		if validateErr != nil {
			return s.notificationEditedImportEnded(c, u, v, "The source preview or configuration changed or could not be verified. No import was queued. Start a new edit from the results."), nil
		}
		// Final authorization precedes a short claim under runtime.mu. A
		// forgotten session cannot start admission; after the claim an already
		// confirmed admission may finish. Keep its lease/reservation charged,
		// but never block unrelated sessions on database or queue operations.
		n := s.notificationRuntime
		admissionCtx, cancel := context.WithTimeout(ctx, d.expires.Sub(n.now()))
		defer cancel()
		if lockErr := notificationEditedImportLock(admissionCtx, &s.providerConfigMu); lockErr != nil {
			return s.notificationEditedImportEnded(c, u, v, "This import request ended before queue admission. No import was queued."), nil
		}
		_, after, authErr := s.notificationEditedImportChannel(admissionCtx, c, u)
		_, sourceErr := s.notificationImportProvider(admissionCtx, request.Provider, d.snapshot.ProviderIdentity)
		if lockErr := notificationEditedImportLock(admissionCtx, &n.mu); lockErr != nil {
			s.providerConfigMu.Unlock()
			return s.notificationEditedImportEnded(c, u, v, "This import request ended before queue admission. No import was queued."), nil
		}
		valid := authErr == nil && after == d.channelFingerprint && sourceErr == nil && admissionCtx.Err() == nil && n.editedImportCurrentLocked(c, u, v, d) && d.revision == a.Revision && d.reviewed
		if !valid {
			n.clearEditedImportLocked(v)
			n.mu.Unlock()
			s.providerConfigMu.Unlock()
			return s.notificationRender(c, u, v, "Authorization, source configuration or this draft changed. No import was queued. Open a new search."), nil
		}
		d.reviewed = false
		d.revision++
		n.mu.Unlock()
		id, submitErr := s.Jobs.SubmitRegisteredContext(admissionCtx, "generic_import", request)
		s.providerConfigMu.Unlock()
		// Queue persistence errors may have an uncertain result. Never retain
		// a reusable confirmation or automatically retry this submission.
		notificationClearEditedImport(v)
		if submitErr != nil {
			return s.notificationRender(c, u, v, "The queue result could not be confirmed. This draft was cleared. Inspect /tasks before starting another import."), nil
		}
		return s.notificationRender(c, u, v, "Edited import queued: "+id+"\nQueue acknowledgement is not completion. Check the task for the final result.", notificationChoice("View task", "task", id), notificationChoice("Home", "home", "")), nil
	default:
		return notify.Message{}, notify.ErrUnsupported
	}
}

func (s *Server) notificationEditedImportSeason(ctx context.Context, c notify.Channel, u notify.Update, v *notificationSession, text string) (notify.Message, error) {
	current, fingerprint, err := s.notificationEditedImportChannel(ctx, c, u)
	if err != nil {
		notificationClearEditedImport(v)
		return notify.Message{}, err
	}
	c = current
	d := v.editedImport
	if d == nil || d.snapshot == nil || fingerprint != d.channelFingerprint || !s.notificationEditedImportCurrent(c, u, v, d) {
		return s.notificationEditedImportEnded(c, u, v, "This edited import expired or changed. Open a new edit from the results."), nil
	}
	season, err := strconv.Atoi(text)
	if err != nil || season < 0 || season > 9999 {
		v.state = "edit_import_season"
		return s.notificationRender(c, u, v, "Season must be a whole number from 0 to 9999. Send another value, or /cancel."), nil
	}
	d.season = season
	d.revision++
	return s.notificationEditedImportSettings(c, u, v, d), nil
}
