// SPDX-License-Identifier: AGPL-3.0-only
// Menus adapted from the behavior of Misaka notification/menus, pinned in ARCHITECTURE.md.
package server

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/AniBakaBaka/AniDan/internal/job"
	"github.com/AniBakaBaka/AniDan/internal/notify"
	"github.com/AniBakaBaka/AniDan/internal/provider"
	"github.com/AniBakaBaka/AniDan/internal/store"
)

type notificationSearchResult = provider.SearchResult
type notificationAction struct {
	Kind, ID, Text string
	Page           int
	Index          int
	Revision       uint64
	Value          bool
	Confirm        bool
	Import         *ImportRequest
	Schedule       *job.ScheduledTask
	Auto           *controlAutoParams
}
type notificationOption struct {
	Label  string
	Action notificationAction
}

func (s *Server) notificationCanImport(name string) bool {
	for _, c := range s.Providers.Catalog() {
		if strings.EqualFold(c.Name, name) {
			return c.Episodes && c.Comments
		}
	}
	return false
}
func (s *Server) notificationCanRefresh(ctx context.Context, episode store.Row) bool {
	source, e := s.Store.Get(ctx, "anime_sources", episode["source_id"])
	if e != nil {
		return false
	}
	for _, c := range s.Providers.Catalog() {
		if c.Name == str(source["provider_name"]) {
			return c.Comments
		}
	}
	return false
}
func notificationChoice(label, kind, id string) notificationOption {
	return notificationOption{label, notificationAction{Kind: kind, ID: id}}
}
func notificationClip(s string, n int) string {
	r := []rune(strings.ToValidUTF8(s, "�"))
	if len(r) > n {
		return string(r[:n]) + "…"
	}
	return string(r)
}

// Byte-limited WeCom text must still end on a UTF-8 boundary.
func notificationClipBytes(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	end := maxBytes - 3
	for end > 0 && !utf8.RuneStart(s[end]) {
		end--
	}
	return s[:end] + "…"
}
func notificationReset(v *notificationSession) {
	notificationClearEditedImport(v)
	notificationClearLogin(v)
	v.actions = map[string]notificationAction{}
	v.choices = nil
	v.state = ""
	v.input = ""
	v.data = notificationAction{}
	v.results = nil
}
func (s *Server) notificationRender(c notify.Channel, u notify.Update, v *notificationSession, text string, options ...notificationOption) notify.Message {
	v.actions = map[string]notificationAction{}
	v.choices = nil
	m := notify.Message{Text: notificationClip(text, 1100), Recipient: u.ChatID}
	if c.Type == "wechat" {
		m.Text = notificationClipBytes(m.Text, 900)
	}
	for i, opt := range options[:min(len(options), 10)] {
		key := "n:" + randomID()
		v.actions[key] = opt.Action
		v.choices = append(v.choices, key)
		label := notificationClipBytes(notificationClip(opt.Label, 55), 120)
		if c.Type == "wechat" {
			label = notificationClipBytes(label, 72)
		}
		if c.Type == "telegram" {
			m.Buttons = append(m.Buttons, []notify.Button{{Text: label, Data: key}})
		} else {
			m.Text += fmt.Sprintf("\n%d. %s", i+1, label)
		}
	}
	if c.Type != "telegram" && len(options) > 0 {
		m.Text += "\nReply with a number; /cancel ends this menu."
	}
	return m
}
func (s *Server) notificationHome(c notify.Channel, u notify.Update, v *notificationSession) notify.Message {
	notificationReset(v)
	options := []notificationOption{notificationChoice("Search sources", "search_input", ""), notificationChoice("Library / refresh", "library", ""), notificationChoice("Tasks", "tasks", ""), notificationChoice("Server status", "status", "")}
	if u.Admin {
		options = append(options, notificationChoice("Bilibili QR login", "login", ""), notificationChoice("Automatic import", "auto", ""), notificationChoice("API tokens", "tokens", ""), notificationChoice("Scheduled tasks", "schedules", ""), notificationChoice("Expired cache cleanup", "cache_confirm", ""))
	}
	return s.notificationRender(c, u, v, "AniDan\n/search [provider] <keyword> or /sh <keyword>\n/library [keyword], /tasks, /status\n/url <supported media URL>\n/cancel ends the current conversation\nAdmins can confirm imports, refreshes, task and token changes in these menus.", options...)
}
func (s *Server) notificationConversation(ctx context.Context, c notify.Channel, u notify.Update) (notify.Message, error) {
	if !utf8.ValidString(u.Text) || len(u.Text) > 4096 {
		return notify.Message{}, notify.ErrLimit
	}
	v, release, e := s.notificationRuntime.session(ctx, notificationKey(c, u))
	if e != nil {
		return notify.Message{}, e
	}
	defer release()
	text := strings.TrimSpace(u.Text)
	if strings.HasPrefix(text, "n:") || u.CallbackID != "" {
		action, ok := v.actions[text]
		if !ok {
			return s.notificationRender(c, u, v, "This menu expired or was already used. Open /start again."), nil
		}
		v.actions = map[string]notificationAction{}
		v.choices = nil // atomically consume the entire rendered generation
		return s.notificationAction(ctx, c, u, v, action)
	}
	if !strings.HasPrefix(text, "/") {
		if n, e := strconv.Atoi(text); e == nil && v.state == "" && n >= 1 && n <= len(v.choices) {
			action := v.actions[v.choices[n-1]]
			v.actions = map[string]notificationAction{}
			v.choices = nil
			return s.notificationAction(ctx, c, u, v, action)
		}
		if v.state != "" {
			return s.notificationInput(ctx, c, u, v, text)
		}
		return s.notificationRender(c, u, v, "Open /start, or choose a number from the latest menu."), nil
	}
	args := strings.Fields(text)
	command := strings.ToLower(strings.Split(strings.TrimPrefix(args[0], "/"), "@")[0])
	rest := strings.TrimSpace(strings.TrimPrefix(text, args[0]))
	notificationReset(v)
	switch command {
	case "start", "help":
		return s.notificationHome(c, u, v), nil
	case "cancel":
		if rest == "" {
			return s.notificationRender(c, u, v, "Current conversation canceled."), nil
		}
		if !u.Admin {
			return notify.Message{}, notify.ErrForbidden
		}
		return s.notificationConfirm(c, u, v, "Cancel task "+notificationClip(rest, 100)+"?", notificationAction{Kind: "task_cancel", ID: rest}), nil
	case "status":
		return s.notificationAction(ctx, c, u, v, notificationAction{Kind: "status"})
	case "library":
		v.input = rest
		return s.notificationAction(ctx, c, u, v, notificationAction{Kind: "library"})
	case "refresh":
		if rest == "" {
			return s.notificationAction(ctx, c, u, v, notificationAction{Kind: "library"})
		}
		return s.notificationAction(ctx, c, u, v, notificationAction{Kind: "episode", ID: rest})
	case "search", "sh":
		if rest == "" {
			return s.notificationAction(ctx, c, u, v, notificationAction{Kind: "search_input"})
		}
		return s.notificationSearch(ctx, c, u, v, rest)
	case "tasks":
		return s.notificationAction(ctx, c, u, v, notificationAction{Kind: "tasks"})
	case "task":
		return s.notificationAction(ctx, c, u, v, notificationAction{Kind: "task", ID: rest})
	case "login":
		return s.notificationLogin(ctx, c, u, v, notificationAction{Kind: "login"})
	case "auto":
		return s.notificationAuto(ctx, c, u, v, notificationAction{Kind: "auto"})
	case "tokens":
		return s.notificationAction(ctx, c, u, v, notificationAction{Kind: "tokens"})
	case "schedules":
		return s.notificationAction(ctx, c, u, v, notificationAction{Kind: "schedules"})
	case "cache":
		return s.notificationAction(ctx, c, u, v, notificationAction{Kind: "cache_confirm"})
	case "url":
		if rest == "" {
			v.state = "url"
			return s.notificationRender(c, u, v, "Send a supported media URL, or /cancel."), nil
		}
		return s.notificationResolve(ctx, c, u, v, rest)
	case "import":
		if !u.Admin {
			return notify.Message{}, notify.ErrForbidden
		}
		parts := strings.Fields(rest)
		if len(parts) < 3 {
			return s.notificationRender(c, u, v, "Use /import <provider> <mediaId> <title>."), nil
		}
		if !s.notificationCanImport(parts[0]) {
			return s.notificationRender(c, u, v, "Unknown or unavailable provider."), nil
		}
		in := &ImportRequest{Provider: parts[0], MediaID: parts[1], Title: strings.Join(parts[2:], " "), Type: "tv_series", Season: 1}
		return s.notificationConfirm(c, u, v, "Import "+notificationClip(in.Title, 200)+" from "+in.Provider+"?", notificationAction{Kind: "import", Import: in}), nil
	default:
		return s.notificationRender(c, u, v, "Unsupported command. Use /help. Use /login for supported Bilibili login; token disclosure and unsupported provider actions are not available."), nil
	}
}
func (s *Server) notificationConfirm(c notify.Channel, u notify.Update, v *notificationSession, text string, a notificationAction) notify.Message {
	a.Confirm = true
	return s.notificationRender(c, u, v, text, notificationOption{"Confirm", a}, notificationChoice("Cancel", "home", ""))
}
func (s *Server) notificationInput(ctx context.Context, c notify.Channel, u notify.Update, v *notificationSession, text string) (notify.Message, error) {
	if text == "" {
		return s.notificationRender(c, u, v, "An input value is required; /cancel ends this conversation."), nil
	}
	state := v.state
	v.state = ""
	switch state {
	case "edit_import_season":
		return s.notificationEditedImportSeason(ctx, c, u, v, text)
	case "auto_term", "auto_season", "auto_episodes":
		return s.notificationAutoInput(c, u, v, state, text)
	case "search":
		return s.notificationSearch(ctx, c, u, v, text)
	case "library":
		v.input = text
		return s.notificationLibrary(ctx, c, u, v, 0)
	case "url":
		return s.notificationResolve(ctx, c, u, v, text)
	case "token_name":
		if !u.Admin {
			return notify.Message{}, notify.ErrForbidden
		}
		if len([]rune(text)) > 50 {
			v.state = state
			return s.notificationRender(c, u, v, "Token name must be 1–50 characters."), nil
		}
		v.input = text
		options := []notificationOption{}
		for _, period := range []string{"30d", "90d", "180d", "permanent"} {
			options = append(options, notificationChoice(period, "token_validity", period))
		}
		return s.notificationRender(c, u, v, "Choose validity for token "+notificationClip(text, 50)+". New tokens have a 500 call/day limit. The secret is available only in the authenticated UI.", options...), nil
	case "schedule_name":
		if !u.Admin {
			return notify.Message{}, notify.ErrForbidden
		}
		if len(text) > 200 {
			v.state = state
			return s.notificationRender(c, u, v, "Schedule name is limited to 200 bytes."), nil
		}
		v.data.Schedule.Name = text
		v.state = "schedule_cron"
		return s.notificationRender(c, u, v, "Send a five-field cron expression in "+s.Config.Timezone+", for example: 0 3 * * *"), nil
	case "schedule_cron", "schedule_cron_edit":
		if !u.Admin {
			return notify.Message{}, notify.ErrForbidden
		}
		if len(text) > 100 {
			v.state = state
			return s.notificationRender(c, u, v, "Cron expression is too long."), nil
		}
		v.data.Schedule.CronExpression = text
		if state == "schedule_cron_edit" {
			return s.notificationConfirm(c, u, v, "Change schedule "+notificationClip(v.data.Schedule.Name, 100)+" cron to "+text+" ("+s.Config.Timezone+")?", notificationAction{Kind: "schedule_save", Schedule: v.data.Schedule, Text: v.data.Text}), nil
		}
		return s.notificationConfirm(c, u, v, "Create enabled schedule "+notificationClip(v.data.Schedule.Name, 100)+"\nJob: "+v.data.Schedule.Kind+"\nCron: "+text+" ("+s.Config.Timezone+")\nConfiguration: {}", notificationAction{Kind: "schedule_create", Schedule: v.data.Schedule}), nil
	default:
		return s.notificationRender(c, u, v, "Conversation expired. Open /start."), nil
	}
}
func (s *Server) notificationAction(ctx context.Context, c notify.Channel, u notify.Update, v *notificationSession, a notificationAction) (notify.Message, error) {
	home := notificationChoice("Home", "home", "")
	if strings.HasPrefix(a.Kind, "edit_import_") {
		return s.notificationEditedImportAction(ctx, c, u, v, a)
	}
	if v.editedImport != nil {
		notificationClearEditedImport(v)
	}
	if strings.HasPrefix(a.Kind, "token") || strings.HasPrefix(a.Kind, "schedule") || a.Kind == "cache_confirm" || a.Kind == "cache_clear" || a.Kind == "import" || a.Kind == "refresh" || strings.HasPrefix(a.Kind, "task_") {
		if !u.Admin {
			return notify.Message{}, notify.ErrForbidden
		}
	}
	switch a.Kind {
	case "login", "login_generate", "login_poll", "login_save", "login_cancel":
		return s.notificationLogin(ctx, c, u, v, a)
	case "auto", "auto_search_type", "auto_media_type", "auto_submit":
		return s.notificationAuto(ctx, c, u, v, a)
	case "source_actions", "source_refresh", "source_incremental", "source_delete", "episode_delete":
		return s.notificationLibraryAction(ctx, c, u, v, a)
	case "home":
		return s.notificationHome(c, u, v), nil
	case "status":
		text, e := s.notificationCommand(ctx, notify.Update{Text: "/status", Admin: u.Admin})
		return s.notificationRender(c, u, v, text, notificationChoice("Refresh", "status", ""), home), e
	case "search_input":
		v.state = "search"
		return s.notificationRender(c, u, v, "Send a search keyword, optionally prefixed by a provider name. Example: bilibili My Show. /cancel ends the search."), nil
	case "search_page":
		return s.notificationSearchPage(c, u, v, a.Page), nil
	case "search_result":
		i, e := strconv.Atoi(a.ID)
		if e != nil || i < 0 || i >= len(v.results) {
			return notify.Message{}, notify.ErrForbidden
		}
		r := v.results[i]
		opts := []notificationOption{}
		if u.Admin && s.notificationCanImport(r.Provider) {
			in := &ImportRequest{Provider: r.Provider, MediaID: r.ID, Title: r.Title, Type: r.Type, Season: max(r.Season, 1), ImageURL: r.ImageURL}
			if in.Type == "" {
				in.Type = "tv_series"
			}
			if r.Year > 0 {
				year := r.Year
				in.Year = &year
			}
			opts = append(opts, notificationOption{"Import this result", notificationAction{Kind: "import_confirm", Import: in}},
				notificationOption{"Edit import", notificationAction{Kind: "edit_import_start", Index: i, Page: a.Page}})
		}
		if r.ImageURL != "" && c.Type != "serverchan3" {
			opts = append(opts, notificationChoice("Show poster", "poster", r.ImageURL))
		}
		opts = append(opts, notificationOption{"Back to results", notificationAction{Kind: "search_page", Page: a.Page}}, home)
		return s.notificationRender(c, u, v, fmt.Sprintf("%s\nProvider: %s\nMedia ID: %s\nType: %s, season %d", notificationClip(r.Title, 300), r.Provider, notificationClip(r.ID, 100), r.Type, max(r.Season, 1)), opts...), nil
	case "import_confirm":
		if !u.Admin || a.Import == nil {
			return notify.Message{}, notify.ErrForbidden
		}
		return s.notificationConfirm(c, u, v, fmt.Sprintf("Import %s\nProvider: %s\nType: %s\nSeason: %d\nThis queues a real library import.", notificationClip(a.Import.Title, 200), a.Import.Provider, a.Import.Type, a.Import.Season), notificationAction{Kind: "import", Import: a.Import}), nil
	case "import":
		if !a.Confirm || a.Import == nil {
			return notify.Message{}, notify.ErrForbidden
		}
		if !s.notificationCanImport(a.Import.Provider) {
			return notify.Message{}, notify.ErrUnsupported
		}
		id, e := s.Jobs.SubmitRegistered("generic_import", *a.Import)
		if e != nil {
			return notify.Message{}, e
		}
		return s.notificationRender(c, u, v, "Import queued: "+id, notificationChoice("View task", "task", id), home), nil
	case "library":
		return s.notificationLibrary(ctx, c, u, v, a.Page)
	case "library_input":
		v.state = "library"
		return s.notificationRender(c, u, v, "Send a library title keyword, or /cancel."), nil
	case "anime":
		return s.notificationAnime(ctx, c, u, v, a.ID, a.Page)
	case "source":
		return s.notificationSource(ctx, c, u, v, a.ID, a.Page)
	case "episode":
		row, e := s.Store.Get(ctx, "episode", a.ID)
		if e != nil {
			return notify.Message{}, e
		}
		opts := []notificationOption{notificationChoice("Back to source", "source", str(row["source_id"])), home}
		if u.Admin {
			opts = append([]notificationOption{notificationChoice("Delete record; retain XML", "episode_delete", str(row["id"]))}, opts...)
			if s.notificationCanRefresh(ctx, row) {
				opts = append([]notificationOption{notificationChoice("Refresh comments", "refresh_confirm", str(row["id"]))}, opts...)
			}
		}
		return s.notificationRender(c, u, v, fmt.Sprintf("Episode %s: %s\nComments: %s", str(row["episode_index"]), notificationClip(str(row["title"]), 300), str(row["comment_count"])), opts...), nil
	case "refresh_confirm":
		if !u.Admin {
			return notify.Message{}, notify.ErrForbidden
		}
		row, e := s.Store.Get(ctx, "episode", a.ID)
		if e != nil {
			return notify.Message{}, e
		}
		if !s.notificationCanRefresh(ctx, row) {
			return notify.Message{}, notify.ErrUnsupported
		}
		return s.notificationConfirm(c, u, v, "Refresh comments for "+notificationClip(str(row["title"]), 200)+"?", notificationAction{Kind: "refresh", ID: a.ID}), nil
	case "refresh":
		if !a.Confirm {
			return notify.Message{}, notify.ErrForbidden
		}
		id, e := strconv.ParseInt(a.ID, 10, 64)
		if e != nil || id <= 0 {
			return notify.Message{}, notify.ErrForbidden
		}
		row, e := s.Store.Get(ctx, "episode", id)
		if e != nil {
			return notify.Message{}, e
		}
		if !s.notificationCanRefresh(ctx, row) {
			return notify.Message{}, notify.ErrUnsupported
		}
		task, e := s.Jobs.SubmitRegistered("fetch_comments", map[string]any{"episodeId": id})
		if e != nil {
			return notify.Message{}, e
		}
		return s.notificationRender(c, u, v, "Refresh queued: "+task, notificationChoice("View task", "task", task), home), nil
	case "tasks":
		return s.notificationTasks(c, u, v, a.Page), nil
	case "task":
		task, ok := s.Jobs.Get(a.ID)
		if !ok {
			return s.notificationRender(c, u, v, "Task not found.", home), nil
		}
		opts := []notificationOption{notificationChoice("Refresh task", "task", task.ID), notificationChoice("Task list", "tasks", "")}
		if u.Admin {
			for _, op := range []string{"cancel", "pause", "resume", "retry"} {
				opts = append(opts, notificationChoice(op+" task", "task_"+op+"_confirm", task.ID))
			}
		}
		return s.notificationRender(c, u, v, fmt.Sprintf("Task %s\n%s\n%s %d%%", task.ID, notificationClip(task.Title, 200), task.Status, task.Progress), opts...), nil
	case "task_cancel_confirm", "task_pause_confirm", "task_resume_confirm", "task_retry_confirm":
		action := strings.TrimSuffix(a.Kind, "_confirm")
		return s.notificationConfirm(c, u, v, strings.TrimPrefix(action, "task_")+" task "+a.ID+"?", notificationAction{Kind: action, ID: a.ID}), nil
	case "task_cancel", "task_pause", "task_resume", "task_retry":
		if !a.Confirm {
			return notify.Message{}, notify.ErrForbidden
		}
		var e error
		id := a.ID
		switch a.Kind {
		case "task_cancel":
			e = s.Jobs.Cancel(id)
		case "task_pause":
			e = s.Jobs.Pause(id)
		case "task_resume":
			e = s.Jobs.Resume(id)
		case "task_retry":
			id, e = s.Jobs.Retry(id)
		}
		if e != nil {
			return notify.Message{}, e
		}
		return s.notificationRender(c, u, v, "Task action accepted: "+id, notificationChoice("View task", "task", id), home), nil
	case "tokens", "token", "token_add", "token_validity", "token_create", "token_toggle", "token_delete", "token_reset":
		return s.notificationTokens(ctx, c, u, v, a)
	case "schedules", "schedule", "schedule_toggle", "schedule_delete", "schedule_run", "schedule_add", "schedule_kind", "schedule_create", "schedule_edit", "schedule_save":
		return s.notificationSchedules(ctx, c, u, v, a)
	case "cache_confirm":
		return s.notificationConfirm(c, u, v, "Delete expired database cache entries? Active bot replay receipts and polling offsets are preserved until their own expiry.", notificationAction{Kind: "cache_clear"}), nil
	case "cache_clear":
		if !a.Confirm {
			return notify.Message{}, notify.ErrForbidden
		}
		id, e := s.Jobs.SubmitRegistered("cacheCleanup", map[string]any{})
		if e != nil {
			return notify.Message{}, e
		}
		return s.notificationRender(c, u, v, "Expired cache cleanup queued: "+id, notificationChoice("View task", "task", id), home), nil
	case "poster":
		return s.notificationPoster(ctx, c, u, v, a.ID)
	case "collage":
		return s.notificationCollage(ctx, c, u, v, a.Page)
	default:
		return s.notificationRender(c, u, v, "Unsupported or expired menu action.", home), nil
	}
}
func (s *Server) notificationSearch(ctx context.Context, c notify.Channel, u notify.Update, v *notificationSession, text string) (notify.Message, error) {
	if len(text) > 500 {
		return s.notificationRender(c, u, v, "Search is limited to 500 bytes."), nil
	}
	args := strings.Fields(text)
	names := []string{}
	if len(args) > 1 {
		if _, ok := s.Providers.Get(args[0]); ok {
			names = []string{args[0]}
			text = strings.Join(args[1:], " ")
		}
	}
	results, e := s.cachedProviderSearch(ctx, text, names...)
	v.results = append([]notificationSearchResult(nil), results[:min(len(results), 100)]...)
	v.input = text
	// Keep memory bounded even if an external provider returns oversized strings.
	for i := range v.results {
		r := &v.results[i]
		r.Title = notificationClip(r.Title, 300)
		if len(r.ID) > 1024 || len(r.ImageURL) > 2048 {
			r.ID = ""
			r.ImageURL = ""
		}
	}
	if len(v.results) == 0 && e != nil {
		return notify.Message{}, e
	}
	m := s.notificationSearchPage(c, u, v, 0)
	if e != nil {
		m.Text += "\nSome providers were unavailable."
	}
	if len(results) > 100 {
		m.Text += "\nShowing the first 100 results."
	}
	return m, nil
}
func (s *Server) notificationSearchPage(c notify.Channel, u notify.Update, v *notificationSession, page int) notify.Message {
	page = max(0, min(page, max((len(v.results)-1)/5, 0)))
	start := page * 5
	end := min(start+5, len(v.results))
	lines := []string{fmt.Sprintf("Search: %s (%d results)", notificationClip(v.input, 100), len(v.results))}
	opts := []notificationOption{}
	for i := start; i < end; i++ {
		r := v.results[i]
		lines = append(lines, fmt.Sprintf("%d. [%s] %s", i+1, r.Provider, notificationClip(r.Title, 80)))
		if r.ID != "" {
			opts = append(opts, notificationOption{fmt.Sprintf("%d. %s", i+1, notificationClip(r.Title, 40)), notificationAction{Kind: "search_result", ID: strconv.Itoa(i), Page: page}})
		}
	}
	if len(v.results) == 0 {
		lines = append(lines, "No matching results")
	}
	if page > 0 {
		opts = append(opts, notificationOption{"Previous", notificationAction{Kind: "search_page", Page: page - 1}})
	}
	if end < len(v.results) {
		opts = append(opts, notificationOption{"Next", notificationAction{Kind: "search_page", Page: page + 1}})
	}
	if c.Type != "serverchan3" && end > start {
		opts = append(opts, notificationOption{"Show poster collage", notificationAction{Kind: "collage", Page: page}})
	}
	opts = append(opts, notificationChoice("New search", "search_input", ""), notificationChoice("Home", "home", ""))
	return s.notificationRender(c, u, v, strings.Join(lines, "\n"), opts...)
}
func (s *Server) notificationResolve(ctx context.Context, c notify.Channel, u notify.Update, v *notificationSession, text string) (notify.Message, error) {
	if len(text) > 2048 {
		return notify.Message{}, notify.ErrLimit
	}
	r, e := s.Providers.ResolveMedia(ctx, text)
	if e != nil {
		return notify.Message{}, e
	}
	v.results = []notificationSearchResult{r}
	v.input = "URL result"
	return s.notificationAction(ctx, c, u, v, notificationAction{Kind: "search_result", ID: "0"})
}
func notificationPageOptions(options []notificationOption, kind, id string, page, total int) []notificationOption {
	if page > 0 {
		options = append(options, notificationOption{"Previous", notificationAction{Kind: kind, ID: id, Page: page - 1}})
	}
	if (page+1)*5 < total {
		options = append(options, notificationOption{"Next", notificationAction{Kind: kind, ID: id, Page: page + 1}})
	}
	return options
}
func (s *Server) notificationLibrary(ctx context.Context, c notify.Channel, u notify.Update, v *notificationSession, page int) (notify.Message, error) {
	var rows []store.Row
	var e error
	if v.input != "" {
		rows, e = s.localSearch(ctx, v.input)
	} else {
		rows, e = s.Store.List(ctx, "anime", nil, 500, 0)
	}
	if e != nil {
		return notify.Message{}, e
	}
	page = max(0, min(page, max((len(rows)-1)/5, 0)))
	opts := []notificationOption{}
	for _, r := range rows[page*5 : min(page*5+5, len(rows))] {
		opts = append(opts, notificationChoice(str(r["id"])+": "+notificationClip(str(r["title"]), 40), "anime", str(r["id"])))
	}
	opts = notificationPageOptions(opts, "library", "", page, len(rows))
	opts = append(opts, notificationChoice("Filter library", "library_input", ""), notificationChoice("Home", "home", ""))
	return s.notificationRender(c, u, v, fmt.Sprintf("Library: %d entries (up to 500 shown)\nFilter: %s", len(rows), notificationClip(v.input, 100)), opts...), nil
}
func (s *Server) notificationAnime(ctx context.Context, c notify.Channel, u notify.Update, v *notificationSession, id string, page int) (notify.Message, error) {
	row, e := s.Store.Get(ctx, "anime", id)
	if e != nil {
		return notify.Message{}, e
	}
	sources, e := s.Store.List(ctx, "anime_sources", store.Row{"anime_id": id}, 500, 0)
	if e != nil {
		return notify.Message{}, e
	}
	page = max(0, min(page, max((len(sources)-1)/5, 0)))
	opts := []notificationOption{}
	for _, r := range sources[page*5 : min(page*5+5, len(sources))] {
		opts = append(opts, notificationChoice(str(r["provider_name"])+" / "+str(r["media_id"]), "source", str(r["id"])))
	}
	opts = notificationPageOptions(opts, "anime", id, page, len(sources))
	if str(row["image_url"]) != "" && c.Type != "serverchan3" {
		opts = append(opts, notificationChoice("Show poster", "poster", str(row["image_url"])))
	}
	opts = append(opts, notificationChoice("Back to library", "library", ""))
	return s.notificationRender(c, u, v, notificationClip(str(row["title"]), 300)+fmt.Sprintf("\nSeason: %s\nSources: %d", str(row["season"]), len(sources)), opts...), nil
}
func (s *Server) notificationSource(ctx context.Context, c notify.Channel, u notify.Update, v *notificationSession, id string, page int) (notify.Message, error) {
	row, e := s.Store.Get(ctx, "anime_sources", id)
	if e != nil {
		return notify.Message{}, e
	}
	total, e := s.Store.Count(ctx, "episode", store.Row{"source_id": id})
	if e != nil {
		return notify.Message{}, e
	}
	page = max(0, min(page, max((int(total)-1)/5, 0)))
	eps, e := s.Store.List(ctx, "episode", store.Row{"source_id": id}, 5, page*5)
	if e != nil {
		return notify.Message{}, e
	}
	opts := []notificationOption{}
	for _, r := range eps {
		opts = append(opts, notificationChoice(str(r["episode_index"])+". "+notificationClip(str(r["title"]), 40), "episode", str(r["id"])))
	}
	opts = notificationPageOptions(opts, "source", id, page, int(total))
	if u.Admin {
		opts = append(opts, notificationChoice("Manage source", "source_actions", id))
	}
	opts = append(opts, notificationChoice("Back to title", "anime", str(row["anime_id"])))
	return s.notificationRender(c, u, v, fmt.Sprintf("Source: %s\nEpisodes: %d", str(row["provider_name"]), total), opts...), nil
}
func (s *Server) notificationTasks(c notify.Channel, u notify.Update, v *notificationSession, page int) notify.Message {
	tasks := s.Jobs.List()
	page = max(0, min(page, max((len(tasks)-1)/5, 0)))
	opts := []notificationOption{}
	for _, t := range tasks[page*5 : min(page*5+5, len(tasks))] {
		opts = append(opts, notificationChoice(fmt.Sprintf("%s %d%% %s", t.Status, t.Progress, notificationClip(t.Title, 30)), "task", t.ID))
	}
	opts = notificationPageOptions(opts, "tasks", "", page, len(tasks))
	if u.Admin {
		opts = append(opts, notificationChoice("Scheduled tasks", "schedules", ""))
	}
	opts = append(opts, notificationChoice("Refresh", "tasks", ""), notificationChoice("Home", "home", ""))
	return s.notificationRender(c, u, v, fmt.Sprintf("Tasks: %d", len(tasks)), opts...)
}
