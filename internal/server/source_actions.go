// SPDX-License-Identifier: AGPL-3.0-only
package server

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/AniBakaBaka/AniDan/internal/provider"
)

type sourceActionState struct {
	mu         sync.Mutex
	challenges map[int64]*biliChallenge
}
type biliChallenge struct {
	session           *provider.BilibiliLoginSession
	key               string
	expires, nextPoll time.Time
	busy              bool
	revision          uint64
}

func (s *Server) initSourceActions() {
	s.sourceActionsOnce.Do(func() { s.sourceActions = &sourceActionState{challenges: map[int64]*biliChallenge{}} })
}
func (s *Server) registerSourceActions(m *http.ServeMux) {
	m.HandleFunc("POST /api/ui/scrapers/{providerName}/actions/{actionName}", s.operator(s.sourceAction))
}
func (s *Server) sourceAction(w http.ResponseWriter, r *http.Request) {
	user, e := s.requireUser(r)
	if e != nil {
		httpError(w, 401, "Authentication required")
		return
	}
	uid := number(user["id"])
	if r.PathValue("providerName") != "bilibili" {
		httpError(w, 501, "This native provider does not expose custom actions")
		return
	}
	p, ok := s.Providers.Get("bilibili")
	if !ok {
		httpError(w, 404, "Bilibili is unavailable")
		return
	}
	b, ok := p.(*provider.Bilibili)
	if !ok {
		httpError(w, 501, "This Bilibili adapter does not support login actions")
		return
	}
	s.initSourceActions()
	state := s.sourceActions
	w.Header().Set("Cache-Control", "no-store")
	switch r.PathValue("actionName") {
	case "get_login_info":
		result, e := b.LoginInfo(r.Context())
		if e != nil {
			httpError(w, 502, "Bilibili login status request failed")
			return
		}
		writeJSON(w, 200, result)
	case "generate_qrcode":
		state.mu.Lock()
		now := time.Now()
		for id, ch := range state.challenges {
			if now.After(ch.expires) {
				delete(state.challenges, id)
			}
		}
		if len(state.challenges) >= 256 && state.challenges[uid] == nil {
			state.mu.Unlock()
			httpError(w, 429, "Too many active login challenges")
			return
		}
		ch := &biliChallenge{expires: now.Add(3 * time.Minute), busy: true, revision: s.Providers.Revision()}
		state.challenges[uid] = ch
		state.mu.Unlock()
		session, result, e := b.NewLoginSession(r.Context())
		state.mu.Lock()
		defer state.mu.Unlock()
		if state.challenges[uid] != ch {
			httpError(w, 409, "Login challenge was replaced or canceled")
			return
		}
		if e != nil {
			delete(state.challenges, uid)
			httpError(w, 502, "Bilibili QR generation failed")
			return
		}
		ch.session = session
		ch.key = str(result["qrcodeKey"])
		ch.busy = false
		writeJSON(w, 200, result)
	case "poll_login":
		var input struct {
			Key string `json:"qrcodeKey"`
		}
		if readJSON(r, &input) != nil || input.Key == "" {
			httpError(w, 422, "qrcodeKey is required")
			return
		}
		state.mu.Lock()
		ch := state.challenges[uid]
		if ch == nil || ch.key != input.Key || time.Now().After(ch.expires) {
			state.mu.Unlock()
			httpError(w, 404, "Login challenge expired or does not belong to this user")
			return
		}
		if ch.busy || time.Now().Before(ch.nextPoll) {
			state.mu.Unlock()
			w.Header().Set("Retry-After", "1")
			httpError(w, 429, "Wait before polling again")
			return
		}
		ch.busy = true
		ch.nextPoll = time.Now().Add(900 * time.Millisecond)
		state.mu.Unlock()
		result, cookie, e := ch.session.Poll(r.Context())
		s.providerConfigMu.Lock()
		defer s.providerConfigMu.Unlock()
		state.mu.Lock()
		defer state.mu.Unlock()
		if state.challenges[uid] != ch {
			httpError(w, 409, "Login challenge was replaced or canceled")
			return
		}
		if time.Now().After(ch.expires) || ch.revision != s.Providers.Revision() {
			delete(state.challenges, uid)
			httpError(w, 409, "Login challenge expired or provider configuration changed")
			return
		}
		ch.busy = false
		if e != nil {
			httpError(w, 502, "Bilibili login polling failed")
			return
		}
		if cookie != "" {
			if e = s.saveBiliCookie(r.Context(), cookie); e != nil {
				httpError(w, 503, "Login confirmed, but saving the source session failed; retry before expiry")
				return
			}
			delete(state.challenges, uid)
		} else if number(result["code"]) == 86038 {
			delete(state.challenges, uid)
		}
		writeJSON(w, 200, result)
	case "cancel_login":
		var input struct {
			Key string `json:"qrcodeKey"`
		}
		if r.ContentLength != 0 && readJSON(r, &input) != nil {
			httpError(w, 422, "Invalid login cancellation")
			return
		}
		state.mu.Lock()
		ch := state.challenges[uid]
		if ch != nil && (input.Key == "" || input.Key == ch.key) {
			delete(state.challenges, uid)
		}
		state.mu.Unlock()
		writeJSON(w, 200, map[string]any{"canceled": true})
	case "logout":
		s.providerConfigMu.Lock()
		defer s.providerConfigMu.Unlock()
		state.mu.Lock()
		defer state.mu.Unlock()
		if e = s.saveBiliCookie(r.Context(), ""); e != nil {
			httpError(w, 503, "Source session could not be removed")
			return
		}
		clear(state.challenges)
		writeJSON(w, 200, map[string]any{"message": "注销成功"})
	default:
		httpError(w, 404, "Unknown Bilibili action")
	}
}

// Caller holds providerConfigMu. Detached validation precedes SQL persistence;
// runtime publication happens only after commit, just like explicit source config.
func (s *Server) saveBiliCookie(ctx context.Context, cookie string) error {
	if len(cookie) > 32768 || strings.ContainsAny(cookie, "\r\n\x00") {
		return errors.New("invalid cookie")
	}
	values := map[string]string{"bilibiliCookie": cookie}
	if e := s.Providers.ValidateConfiguration("bilibili", values); e != nil {
		return e
	}
	tx, e := s.Store.DB.BeginTx(ctx, nil)
	if e != nil {
		return e
	}
	defer tx.Rollback()
	if e = s.metadataConfigTx(ctx, tx, "bilibiliCookie", cookie); e != nil {
		return e
	}
	if e = tx.Commit(); e != nil {
		return e
	}
	return s.Providers.Configure("bilibili", values)
}
