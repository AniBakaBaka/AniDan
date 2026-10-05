// SPDX-License-Identifier: AGPL-3.0-only
package server

import (
	"context"
	"net/url"
	"strings"
	"time"

	"github.com/AniBakaBaka/AniDan/internal/notify"
	"github.com/AniBakaBaka/AniDan/internal/provider"
	qrcode "github.com/skip2/go-qrcode"
)

// This state is private, memory-only, and accessed under its conversation gate.
// Cookies and refresh tokens never enter actions, durable jobs, messages, errors,
// or logs. The short-lived challenge is shared only as a QR or validated SC3 URL;
// callbacks carry an unrelated random ID rather than the upstream poll key.
type notificationLogin struct {
	owner             notificationOwner
	identity, id      string
	session           *provider.BilibiliLoginSession
	expires, nextPoll time.Time
	revision          uint64
	cookie            string
}

func notificationClearLogin(v *notificationSession) {
	if v.login != nil {
		v.login.cookie = ""
		v.login.session = nil
	}
	v.login = nil
}

// Check persisted authorization rather than trusting the role captured when a
// command was queued. Bot replacement also invalidates the original identity.
func (s *Server) notificationLoginChannel(ctx context.Context, c notify.Channel, u notify.Update) (notify.Channel, error) {
	current, err := s.notificationChannel(ctx, c.ID)
	if err != nil || !current.Enabled || current.Type != c.Type || notificationIdentity(current) != notificationIdentity(c) {
		return notify.Channel{}, notify.ErrForbidden
	}
	allowed, admin := notify.Authorize(current, u.Sender)
	if !allowed || !admin || u.Sender == "" || u.ChatID == "" {
		return notify.Channel{}, notify.ErrForbidden
	}
	switch current.Type {
	case "telegram":
		if u.ChatID != u.Sender {
			return notify.Channel{}, notify.ErrForbidden
		}
	case "wechat", "serverchan3": // authenticated adapters address one user
	default:
		return notify.Channel{}, notify.ErrUnsupported
	}
	return current, nil
}

func (s *Server) notificationLoginCurrent(c notify.Channel, u notify.Update, v *notificationSession) bool {
	if s.notificationRuntime == nil {
		return false
	}
	s.notificationRuntime.mu.Lock()
	defer s.notificationRuntime.mu.Unlock()
	return !s.notificationRuntime.closed && s.notificationRuntime.sessions[notificationKey(c, u)] == v
}

func notificationLoginURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && len(raw) <= 4096 && !strings.ContainsAny(raw, "\r\n\x00") &&
		u.Scheme == "https" && u.Host == "passport.bilibili.com" && u.User == nil && u.Fragment == "" &&
		strings.HasPrefix(u.Path, "/h5-app/passport/login/")
}

func (s *Server) notificationLoginEnded(c notify.Channel, u notify.Update, v *notificationSession, text string) notify.Message {
	notificationClearLogin(v)
	return s.notificationRender(c, u, v, text, notificationChoice("New Bilibili login", "login", ""), notificationChoice("Home", "home", ""))
}

func (s *Server) notificationLoginSavePrompt(c notify.Channel, u notify.Update, v *notificationSession) notify.Message {
	return s.notificationRender(c, u, v,
		"Bilibili confirmed the scan. Save this account as AniDan's shared Bilibili source account? This persistently replaces the current source login for every server user. Only confirm if you scanned your intended account. The login secret will never be sent in chat.",
		notificationOption{"Save source account", notificationAction{Kind: "login_save", ID: v.login.id, Confirm: true}},
		notificationChoice("Cancel login", "login_cancel", v.login.id))
}

func (s *Server) notificationLogin(ctx context.Context, c notify.Channel, u notify.Update, v *notificationSession, a notificationAction) (notify.Message, error) {
	current, err := s.notificationLoginChannel(ctx, c, u)
	if err != nil {
		notificationClearLogin(v)
		return notify.Message{}, err
	}
	c = current
	if !s.notificationLoginCurrent(c, u, v) {
		return s.notificationLoginEnded(c, u, v, "This login conversation was canceled or replaced. Open a new login."), nil
	}
	if a.Kind == "login" {
		notificationClearLogin(v)
		return s.notificationRender(c, u, v,
			"Bilibili source login\nGenerate a private, three-minute login challenge and scan it with the Bilibili app. After mobile confirmation, you must separately confirm here before AniDan saves and replaces the shared source account.",
			notificationChoice("Generate login QR", "login_generate", ""), notificationChoice("Cancel login", "login_cancel", "")), nil
	}
	if a.Kind == "login_cancel" {
		return s.notificationLoginEnded(c, u, v, "Bilibili login canceled. No source account was saved."), nil
	}
	if a.Kind == "login_generate" {
		notificationClearLogin(v)
		// Capture the provider snapshot and revision together with other source
		// configuration writes, without holding a global lock over network I/O.
		s.providerConfigMu.Lock()
		revision := s.Providers.Revision()
		p, ok := s.Providers.Get("bilibili")
		s.providerConfigMu.Unlock()
		b, supported := p.(*provider.Bilibili)
		if !ok || !supported {
			return s.notificationLoginEnded(c, u, v, "Bilibili QR login is unavailable for this source."), nil
		}
		ch := &notificationLogin{owner: notificationKey(c, u), identity: notificationIdentity(c), id: randomID(), expires: time.Now().Add(3 * time.Minute), revision: revision}
		v.login = ch
		session, result, err := b.NewLoginSession(ctx)
		if err != nil {
			return s.notificationLoginEnded(c, u, v, "Bilibili could not generate a login challenge. Try a new login."), nil
		}
		if _, err = s.notificationLoginChannel(ctx, c, u); err != nil {
			notificationClearLogin(v)
			return notify.Message{}, err
		}
		if !s.notificationLoginCurrent(c, u, v) || !time.Now().Before(ch.expires) || ch.revision != s.Providers.Revision() {
			return s.notificationLoginEnded(c, u, v, "This login expired, was canceled, or the source configuration changed. Generate a new challenge."), nil
		}
		raw := str(result["url"])
		if !notificationLoginURL(raw) {
			return s.notificationLoginEnded(c, u, v, "Bilibili returned an invalid login challenge. No login was saved."), nil
		}
		ch.session = session
		m := s.notificationRender(c, u, v, "Scan this QR with the Bilibili app and confirm on your phone, then check the status here. It expires in three minutes. A separate confirmation here is required before saving the source account.",
			notificationChoice("Check login status", "login_poll", ch.id), notificationChoice("Cancel login", "login_cancel", ch.id))
		if c.Type == "serverchan3" {
			// SC3 cannot upload images. Preserve the complete validated URL rather
			// than letting the menu's text truncation corrupt the challenge link.
			m.Text = "ServerChan3 cannot upload a QR image. Open this Bilibili login link on another screen and use the Bilibili app to complete login. It expires in three minutes. Saving requires a separate confirmation here.\n" + raw + "\n1. Check login status\n2. Cancel login\nReply with a number; /cancel ends this menu."
			if len([]rune(m.Text)) > 4096 {
				return s.notificationLoginEnded(c, u, v, "The Bilibili login link is too long for ServerChan3. Use the authenticated web UI to log in."), nil
			}
			return m, nil
		}
		m.Image, err = qrcode.Encode(raw, qrcode.Medium, 256)
		if err != nil {
			return s.notificationLoginEnded(c, u, v, "The Bilibili challenge could not be encoded as a QR image. Generate a new login."), nil
		}
		return m, nil
	}

	ch := v.login
	if ch == nil || ch.session == nil || ch.owner != notificationKey(c, u) || ch.identity != notificationIdentity(c) || a.ID != ch.id || !time.Now().Before(ch.expires) || ch.revision != s.Providers.Revision() {
		return s.notificationLoginEnded(c, u, v, "This login expired, was canceled, or the source configuration changed. Generate a new challenge."), nil
	}
	switch a.Kind {
	case "login_poll":
		if ch.cookie != "" {
			return s.notificationLoginSavePrompt(c, u, v), nil
		}
		if time.Now().Before(ch.nextPoll) {
			return s.notificationRender(c, u, v, "Wait at least one second between Bilibili status checks.", notificationChoice("Check login status", "login_poll", ch.id), notificationChoice("Cancel login", "login_cancel", ch.id)), nil
		}
		ch.nextPoll = time.Now().Add(time.Second)
		result, cookie, err := ch.session.Poll(ctx)
		if _, authErr := s.notificationLoginChannel(ctx, c, u); authErr != nil {
			notificationClearLogin(v)
			return notify.Message{}, authErr
		}
		if !s.notificationLoginCurrent(c, u, v) || !time.Now().Before(ch.expires) || ch.revision != s.Providers.Revision() {
			return s.notificationLoginEnded(c, u, v, "This login expired, was canceled, or the source configuration changed. No source account was saved."), nil
		}
		if err != nil {
			return s.notificationRender(c, u, v, "Bilibili login status could not be checked. Retry before the challenge expires.", notificationChoice("Check login status", "login_poll", ch.id), notificationChoice("Cancel login", "login_cancel", ch.id)), nil
		}
		if cookie != "" {
			ch.cookie = cookie
			return s.notificationLoginSavePrompt(c, u, v), nil
		}
		if number(result["code"]) == 86038 {
			return s.notificationLoginEnded(c, u, v, "Bilibili reports that this QR code expired. Generate a new login."), nil
		}
		text := "Bilibili login is still pending. Scan the QR and confirm on your phone."
		if number(result["code"]) == 86090 {
			text = "QR scanned. Confirm the login in the Bilibili app, then check again."
		}
		return s.notificationRender(c, u, v, text, notificationChoice("Check login status", "login_poll", ch.id), notificationChoice("Cancel login", "login_cancel", ch.id)), nil
	case "login_save":
		if ch.cookie == "" {
			return s.notificationRender(c, u, v, "Complete the scan and mobile confirmation before saving.", notificationChoice("Check login status", "login_poll", ch.id), notificationChoice("Cancel login", "login_cancel", ch.id)), nil
		}
		if !a.Confirm {
			return s.notificationLoginSavePrompt(c, u, v), nil
		}
		s.providerConfigMu.Lock()
		defer s.providerConfigMu.Unlock()
		// Keep runtime invalidation serialized with the final authorization check
		// and save. A forgotten session cannot resume and persist an old cookie.
		s.notificationRuntime.mu.Lock()
		defer s.notificationRuntime.mu.Unlock()
		if _, err = s.notificationLoginChannel(ctx, c, u); err != nil {
			notificationClearLogin(v)
			return notify.Message{}, err
		}
		if s.notificationRuntime.closed || s.notificationRuntime.sessions[ch.owner] != v || !time.Now().Before(ch.expires) || ch.revision != s.Providers.Revision() {
			return s.notificationLoginEnded(c, u, v, "This login expired, was canceled, or the source configuration changed. No source account was saved."), nil
		}
		if err = s.saveBiliCookie(ctx, ch.cookie); err != nil {
			m := s.notificationLoginSavePrompt(c, u, v)
			m.Text = "The source account could not be saved. You may confirm again before the challenge expires.\n" + m.Text
			return m, nil
		}
		return s.notificationLoginEnded(c, u, v, "Bilibili source account saved. The shared source now uses the account you confirmed."), nil
	default:
		return notify.Message{}, notify.ErrUnsupported
	}
}
