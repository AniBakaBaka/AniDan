// SPDX-License-Identifier: AGPL-3.0-only
// Legacy HTTP contracts adapted from l429609201/misaka_danmu_server.
package server

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/AniBakaBaka/AniDan/internal/store"
	"github.com/golang-jwt/jwt/v5"
	"golang.org/x/crypto/bcrypt"
)

type authPending struct {
	value   any
	created time.Time
}
type authFailure struct {
	count int
	first time.Time
}
type authState struct {
	mu       sync.Mutex
	pending  map[string]authPending
	failures map[string]authFailure
	mutation sync.Mutex
	slots    chan struct{}
}
type authMFATicket struct {
	UserID   int64
	Username string
}

func (s *Server) authState() *authState {
	s.authOnce.Do(func() {
		s.auth = &authState{pending: map[string]authPending{}, failures: map[string]authFailure{}, slots: make(chan struct{}, 4)}
	})
	return s.auth
}
func authRandom() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}
func (s *Server) authPut(key string, value any) {
	a := s.authState()
	a.mu.Lock()
	defer a.mu.Unlock()
	now := time.Now()
	for k, v := range a.pending {
		if now.Sub(v.created) > 5*time.Minute {
			delete(a.pending, k)
		}
	}
	if len(a.pending) >= 10000 {
		var oldest string
		var t time.Time
		for k, v := range a.pending {
			if oldest == "" || v.created.Before(t) {
				oldest = k
				t = v.created
			}
		}
		delete(a.pending, oldest)
	}
	a.pending[key] = authPending{value, now}
}
func (s *Server) authTake(key string) (any, bool) {
	a := s.authState()
	a.mu.Lock()
	defer a.mu.Unlock()
	v, ok := a.pending[key]
	delete(a.pending, key)
	return v.value, ok && time.Since(v.created) < 5*time.Minute
}
func authString(v any) string {
	if v == nil {
		return ""
	}
	switch x := v.(type) {
	case string:
		return x
	case []byte:
		return string(x)
	default:
		return fmt.Sprint(v)
	}
}
func authInt(v any) int64 {
	switch x := v.(type) {
	case int64:
		return x
	case int:
		return int64(x)
	case float64:
		return int64(x)
	case bool:
		if x {
			return 1
		}
		return 0
	default:
		n, _ := strconv.ParseInt(authString(v), 10, 64)
		return n
	}
}
func authBool(v any) bool { x := strings.ToLower(authString(v)); return x == "true" || x == "1" }
func (s *Server) authLocation() *time.Location {
	l, e := time.LoadLocation(s.Config.Timezone)
	if e != nil {
		return time.UTC
	}
	return l
}
func (s *Server) authNow() string {
	return time.Now().In(s.authLocation()).Format("2006-01-02 15:04:05.000000")
}
func (s *Server) authDate(v any) (time.Time, error) {
	if t, ok := v.(time.Time); ok {
		return t, nil
	}
	for _, f := range []string{time.RFC3339Nano, "2006-01-02 15:04:05.999999999", "2006-01-02T15:04:05.999999999", "2006-01-02 15:04:05"} {
		t, e := time.ParseInLocation(f, authString(v), s.authLocation())
		if e == nil {
			return t, nil
		}
	}
	return time.Time{}, errors.New("invalid timestamp")
}
func (s *Server) authConfig(ctx context.Context, key, fallback string) string {
	r, e := s.Store.Get(ctx, "config", key)
	if e == nil && r != nil && r["config_value"] != nil {
		return authString(r["config_value"])
	}
	return fallback
}
func (s *Server) authFind(ctx context.Context, table string, filter store.Row) (store.Row, error) {
	rows, e := s.Store.List(ctx, table, filter, 1, 0)
	if e != nil {
		return nil, e
	}
	if len(rows) == 0 {
		return nil, sql.ErrNoRows
	}
	return rows[0], nil
}
func (s *Server) authUser(ctx context.Context, name string) (store.Row, error) {
	return s.authFind(ctx, "users", store.Row{"username": name})
}
func (s *Server) authKey(ctx context.Context) string {
	return s.authConfig(ctx, "jwtSecretKey", s.Config.JWTSecret)
}
func (s *Server) authAlgorithm() string {
	if s.Config.JWTAlgorithm == "" {
		return "HS256"
	}
	return s.Config.JWTAlgorithm
}
func authAddr(v string) netip.Addr {
	h, _, e := net.SplitHostPort(v)
	if e == nil {
		v = h
	}
	a, _ := netip.ParseAddr(strings.TrimSpace(v))
	return a.Unmap()
}
func authInNetworks(ip netip.Addr, list string) bool {
	if !ip.IsValid() {
		return false
	}
	for _, v := range strings.Split(list, ",") {
		v = strings.TrimSpace(v)
		if p, e := netip.ParsePrefix(v); e == nil && p.Contains(ip) {
			return true
		}
		if a, e := netip.ParseAddr(v); e == nil && a.Unmap() == ip {
			return true
		}
	}
	return false
}
func (s *Server) authClientIP(r *http.Request) string {
	peer := authAddr(r.RemoteAddr)
	trusted := s.authConfig(r.Context(), "trustedProxies", "")
	if !authInNetworks(peer, trusted) {
		return peer.String()
	}
	chain := strings.Split(r.Header.Get("X-Forwarded-For"), ",")
	for i := len(chain) - 1; i >= 0; i-- {
		a := authAddr(chain[i])
		if !a.IsValid() {
			return peer.String()
		}
		if !authInNetworks(a, trusted) {
			return a.String()
		}
	}
	if r.Header.Get("X-Forwarded-For") != "" && len(chain) > 0 {
		return authAddr(chain[0]).String()
	}
	if a := authAddr(r.Header.Get("X-Real-IP")); a.IsValid() {
		return a.String()
	}
	return peer.String()
}
func (s *Server) authWhitelisted(r *http.Request) bool {
	return authInNetworks(authAddr(s.authClientIP(r)), s.authConfig(r.Context(), "ipWhitelist", ""))
}
func (s *Server) authClaims(r *http.Request) (store.Row, string, error) {
	v := strings.Fields(r.Header.Get("Authorization"))
	if len(v) != 2 || !strings.EqualFold(v[0], "Bearer") {
		return nil, "", errors.New("authentication required")
	}
	alg := s.authAlgorithm()
	if alg != "HS256" && alg != "HS384" && alg != "HS512" {
		return nil, "", errors.New("unsupported JWT algorithm")
	}
	key := s.authKey(r.Context())
	if key == "" {
		return nil, "", errors.New("JWT signing key unavailable")
	}
	token, e := jwt.Parse(v[1], func(t *jwt.Token) (any, error) { return []byte(key), nil }, jwt.WithValidMethods([]string{alg}), jwt.WithExpirationRequired())
	if e != nil || !token.Valid {
		return nil, "", errors.New("invalid credentials")
	}
	claims, ok := token.Claims.(jwt.MapClaims)
	if !ok {
		return nil, "", errors.New("invalid credentials")
	}
	if s.Config.SessionAudience != "" {
		audience, err := claims.GetAudience()
		if err != nil || len(audience) != 1 || audience[0] != s.Config.SessionAudience {
			return nil, "", errors.New("invalid credentials")
		}
	}
	name, _ := claims.GetSubject()
	if name == "" {
		return nil, "", errors.New("invalid credentials")
	}
	user, e := s.authUser(r.Context(), name)
	if e != nil {
		return nil, "", errors.New("invalid credentials")
	}
	jti, _ := claims["jti"].(string)
	if jti != "" {
		session, e := s.authFind(r.Context(), "user_sessions", store.Row{"jti": jti})
		if e != nil || authBool(session["is_revoked"]) || authInt(session["user_id"]) != authInt(user["id"]) {
			return nil, "", errors.New("session revoked")
		}
		if session["expires_at"] != nil {
			exp, e := s.authDate(session["expires_at"])
			if e != nil || !exp.After(time.Now()) {
				return nil, "", errors.New("session expired")
			}
		}
	}
	return user, jti, nil
}
func (s *Server) requireUser(r *http.Request) (store.Row, error) {
	u, _, e := s.authClaims(r)
	if e == nil {
		return u, nil
	}
	if s.authWhitelisted(r) {
		return s.authUser(r.Context(), "admin")
	}
	return nil, errors.New("Could not validate credentials")
}
func (s *Server) requireControl(r *http.Request) error {
	want := s.authConfig(r.Context(), "externalApiKey", s.Config.ExternalAPIKey)
	got := r.URL.Query().Get("api_key")
	if got == "" {
		got = r.Header.Get("X-API-KEY")
	}
	if want == "" || len(want) != len(got) || subtle.ConstantTimeCompare([]byte(want), []byte(got)) != 1 {
		return errors.New("Invalid API key")
	}
	return nil
}
func (s *Server) authRequired(w http.ResponseWriter, r *http.Request) (store.Row, bool) {
	u, e := s.requireUser(r)
	if e != nil {
		w.Header().Set("WWW-Authenticate", "Bearer")
		httpError(w, 401, e.Error())
		return nil, false
	}
	return u, true
}
func (s *Server) authRateCheck(w http.ResponseWriter, r *http.Request) bool {
	ip := s.authClientIP(r)
	max, parseErr := strconv.Atoi(s.authConfig(r.Context(), "loginMaxFailCount", "3"))
	if parseErr != nil {
		max = 3
	}
	mins, _ := strconv.Atoi(s.authConfig(r.Context(), "loginLockoutMinutes", "60"))
	if mins <= 0 {
		mins = 60
	}
	if mins > 10080 {
		mins = 10080
	}
	a := s.authState()
	a.mu.Lock()
	defer a.mu.Unlock()
	v, ok := a.failures[ip]
	if ok && time.Since(v.first) >= time.Duration(mins)*time.Minute {
		delete(a.failures, ip)
		ok = false
	}
	if ok && max > 0 && v.count >= max {
		remaining := int(time.Until(v.first.Add(time.Duration(mins)*time.Minute)).Seconds()) + 1
		w.Header().Set("Retry-After", strconv.Itoa(remaining))
		httpError(w, 429, "登录失败次数过多，请稍后重试")
		return false
	}
	return true
}
func (s *Server) authFailure(r *http.Request) {
	ip := s.authClientIP(r)
	a := s.authState()
	a.mu.Lock()
	defer func() { a.mu.Unlock(); s.authAudit(r, "login_failed", "Authentication verification failed", false) }()
	now := time.Now()
	for k, v := range a.failures {
		if now.Sub(v.first) > 24*time.Hour {
			delete(a.failures, k)
		}
	}
	if len(a.failures) >= 10000 {
		for k := range a.failures {
			delete(a.failures, k)
			break
		}
	}
	v := a.failures[ip]
	if v.first.IsZero() {
		v.first = now
	}
	v.count++
	a.failures[ip] = v
}
func (s *Server) authIssue(r *http.Request, u store.Row) (store.Row, error) {
	key := s.authKey(r.Context())
	if key == "" {
		return nil, errors.New("JWT signing key unavailable")
	}
	alg := s.authAlgorithm()
	if alg != "HS256" && alg != "HS384" && alg != "HS512" {
		return nil, errors.New("unsupported JWT algorithm")
	}
	mins, _ := strconv.Atoi(s.authConfig(r.Context(), "jwtExpireMinutes", "4320"))
	if mins <= 0 {
		mins = 4320
	}
	if mins > 43200 {
		mins = 43200
	}
	now := time.Now()
	jti := authRandom()
	claims := jwt.MapClaims{"sub": authString(u["username"]), "jti": jti, "iat": now.Unix(), "exp": now.Add(time.Duration(mins) * time.Minute).Unix()}
	if s.Config.SessionAudience != "" {
		claims["aud"] = s.Config.SessionAudience
	}
	token, e := jwt.NewWithClaims(jwt.GetSigningMethod(alg), claims).SignedString([]byte(key))
	if e != nil {
		return nil, e
	}
	ua := r.UserAgent()
	if utf8.RuneCountInString(ua) > 500 {
		ua = string([]rune(ua)[:500])
	}
	_, e = s.Store.Insert(r.Context(), "user_sessions", store.Row{"user_id": u["id"], "jti": jti, "ip_address": s.authClientIP(r), "user_agent": ua, "created_at": s.authNow(), "last_used_at": s.authNow(), "expires_at": now.Add(time.Duration(mins) * time.Minute).In(s.authLocation()).Format("2006-01-02 15:04:05.000000"), "is_revoked": false})
	if e != nil {
		return nil, e
	}
	h := sha256.Sum256([]byte(token))
	e = s.Store.Update(r.Context(), "users", u["id"], store.Row{"token": fmt.Sprintf("%x", h[:16]), "token_update": s.authNow()})
	if e != nil {
		return nil, e
	}
	clientIP := s.authClientIP(r)
	a := s.authState()
	a.mu.Lock()
	delete(a.failures, clientIP)
	a.mu.Unlock()
	return store.Row{"accessToken": token, "tokenType": "bearer", "expiresIn": mins}, nil
}
func (s *Server) authIssueResponse(w http.ResponseWriter, r *http.Request, u store.Row) {
	v, e := s.authIssue(r, u)
	if e != nil {
		httpError(w, 500, "Unable to create session")
		return
	}
	s.authAudit(r, "login_success", "User authenticated", true)
	writeJSON(w, 200, v)
}
func (s *Server) registerAuth(m *http.ServeMux) {
	p := "/api/ui/auth"
	m.HandleFunc("POST "+p+"/token", s.authLogin)
	m.HandleFunc("POST "+p+"/auto-login", s.authAutoLogin)
	m.HandleFunc("GET "+p+"/users/me", func(w http.ResponseWriter, r *http.Request) {
		u, ok := s.authRequired(w, r)
		if ok {
			writeJSON(w, 200, store.Row{"id": u["id"], "username": u["username"]})
		}
	})
	m.HandleFunc("POST "+p+"/logout", s.authLogout)
	m.HandleFunc("PUT "+p+"/users/me/password", s.authPassword)
	m.HandleFunc("GET "+p+"/sessions", s.authSessions)
	m.HandleFunc("DELETE "+p+"/sessions/{session_id}", s.authRevokeSession)
	m.HandleFunc("DELETE "+p+"/sessions/others/all", s.authRevokeOthers)
	m.HandleFunc("GET "+p+"/login-lockout", s.authLockouts)
	m.HandleFunc("DELETE "+p+"/login-lockout", s.authClearLockout)
	s.registerMFA(m)
}
func (s *Server) authLogin(w http.ResponseWriter, r *http.Request) {
	a := s.authState()
	select {
	case a.slots <- struct{}{}:
		defer func() { <-a.slots }()
	default:
		w.Header().Set("Retry-After", "1")
		httpError(w, 429, "Authentication is busy; retry shortly")
		return
	}
	if !s.authRateCheck(w, r) {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	if e := r.ParseForm(); e != nil {
		httpError(w, 422, "Invalid login form")
		return
	}
	name, pass := r.Form.Get("username"), r.Form.Get("password")
	if name == "" || pass == "" {
		httpError(w, 422, "username and password are required")
		return
	}
	u, e := s.authUser(r.Context(), name)
	hash := "$2a$10$N9qo8uLOickgx2ZMRZoMyeIjZAgcfl7p92ldGxad68LJZdL17lhWy"
	if e == nil {
		hash = authString(u["hashed_password"])
	}
	if bcrypt.CompareHashAndPassword([]byte(hash), []byte(pass)) != nil || e != nil {
		s.authFailure(r)
		w.Header().Set("WWW-Authenticate", "Bearer")
		httpError(w, 401, "Incorrect username or password")
		return
	}
	keys, e := s.Store.Count(r.Context(), "user_passkeys", store.Row{"user_id": u["id"]})
	if e != nil {
		httpError(w, 500, "Unable to inspect MFA state")
		return
	}
	hasTOTP := authBool(u["is_otp"])
	if hasTOTP || keys > 0 {
		if otp := r.Form.Get("otp_password"); otp != "" && hasTOTP {
			if !s.authVerifyOTP(u, otp) {
				s.authFailure(r)
				httpError(w, 401, "Invalid OTP code")
				return
			}
			if e = s.authUpgradeOTP(r.Context(), u); e != nil {
				httpError(w, 500, "Unable to secure migrated OTP secret")
				return
			}
		} else {
			types := []string{}
			if hasTOTP {
				types = append(types, "totp")
			}
			if keys > 0 {
				types = append(types, "passkey")
			}
			t := authRandom()
			s.authPut("mfa_"+t, authMFATicket{authInt(u["id"]), name})
			writeJSON(w, 403, store.Row{"detail": "MFA required", "mfaRequired": true, "mfaTypes": types, "mfaToken": t})
			return
		}
	}
	s.authIssueResponse(w, r, u)
}
func (s *Server) authAutoLogin(w http.ResponseWriter, r *http.Request) {
	if !s.authWhitelisted(r) {
		httpError(w, 401, "Not in IP whitelist")
		return
	}
	u, e := s.authUser(r.Context(), "admin")
	if e != nil {
		httpError(w, 500, "Admin user not found")
		return
	}
	s.authIssueResponse(w, r, u)
}
func (s *Server) authLogout(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.authRequired(w, r); !ok {
		return
	}
	_, jti, e := s.authClaims(r)
	if e == nil && jti != "" {
		session, e := s.authFind(r.Context(), "user_sessions", store.Row{"jti": jti})
		if e == nil {
			if e = s.Store.Update(r.Context(), "user_sessions", session["id"], store.Row{"is_revoked": true}); e != nil {
				httpError(w, 500, "Unable to revoke session")
				return
			}
		}
	}
	s.authAudit(r, "logout", "Current session revoked", true)
	w.WriteHeader(204)
}
func (s *Server) authPassword(w http.ResponseWriter, r *http.Request) {
	u, ok := s.authRequired(w, r)
	if !ok {
		return
	}
	var b struct {
		Old string `json:"oldPassword"`
		New string `json:"newPassword"`
	}
	if readJSON(r, &b) != nil || utf8.RuneCountInString(b.New) < 8 || len([]byte(b.New)) > 72 {
		httpError(w, 422, "New password must contain at least 8 characters and at most 72 UTF-8 bytes")
		return
	}
	if bcrypt.CompareHashAndPassword([]byte(authString(u["hashed_password"])), []byte(b.Old)) != nil {
		httpError(w, 400, "Incorrect old password")
		return
	}
	hash, e := bcrypt.GenerateFromPassword([]byte(b.New), bcrypt.DefaultCost)
	if e != nil {
		httpError(w, 500, "Unable to hash password")
		return
	}
	if e = s.Store.Update(r.Context(), "users", u["id"], store.Row{"hashed_password": string(hash)}); e != nil {
		httpError(w, 500, "Unable to change password")
		return
	}
	s.authAudit(r, "password_change", "Password changed", true)
	w.WriteHeader(204)
}
func authSessionPublic(v store.Row) store.Row {
	return store.Row{"id": v["id"], "userId": v["user_id"], "jti": v["jti"], "ipAddress": v["ip_address"], "userAgent": v["user_agent"], "createdAt": authDateWire(v["created_at"]), "lastUsedAt": authDateWire(v["last_used_at"]), "expiresAt": authDateWire(v["expires_at"]), "isRevoked": authBool(v["is_revoked"])}
}
func (s *Server) authSessions(w http.ResponseWriter, r *http.Request) {
	u, ok := s.authRequired(w, r)
	if !ok {
		return
	}
	_, jti, _ := s.authClaims(r)
	rows, e := s.Store.List(r.Context(), "user_sessions", store.Row{"user_id": u["id"]}, 10000, 0)
	if e != nil {
		httpError(w, 500, "Unable to list sessions")
		return
	}
	sort.SliceStable(rows, func(i, j int) bool {
		a, ea := s.authDate(rows[i]["created_at"])
		b, eb := s.authDate(rows[j]["created_at"])
		if ea == nil && eb == nil && !a.Equal(b) {
			return a.After(b)
		}
		return authInt(rows[i]["id"]) > authInt(rows[j]["id"])
	})
	out := []store.Row{}
	for _, v := range rows {
		o := authSessionPublic(v)
		o["isCurrent"] = authString(v["jti"]) == jti
		o["isWhitelist"] = strings.HasPrefix(authString(v["jti"]), "whitelist_")
		out = append(out, o)
	}
	writeJSON(w, 200, store.Row{"sessions": out, "currentJti": jti})
}
func (s *Server) authRevokeSession(w http.ResponseWriter, r *http.Request) {
	u, ok := s.authRequired(w, r)
	if !ok {
		return
	}
	id, e := idParam(r, "session_id")
	if e != nil {
		httpError(w, 422, "Invalid session ID")
		return
	}
	row, e := s.Store.Get(r.Context(), "user_sessions", id)
	if e != nil || row == nil || authInt(row["user_id"]) != authInt(u["id"]) {
		httpError(w, 404, "Session not found")
		return
	}
	_, jti, _ := s.authClaims(r)
	if authString(row["jti"]) == jti {
		httpError(w, 400, "Cannot revoke current session")
		return
	}
	if e = s.Store.Update(r.Context(), "user_sessions", id, store.Row{"is_revoked": true}); e != nil {
		httpError(w, 500, "Unable to revoke session")
		return
	}
	s.authAudit(r, "session_revoke", "Session revoked", true)
	w.WriteHeader(204)
}
func (s *Server) authRevokeOthers(w http.ResponseWriter, r *http.Request) {
	u, ok := s.authRequired(w, r)
	if !ok {
		return
	}
	_, jti, _ := s.authClaims(r)
	rows, e := s.Store.List(r.Context(), "user_sessions", store.Row{"user_id": u["id"]}, 10000, 0)
	if e != nil {
		httpError(w, 500, "Unable to list sessions")
		return
	}
	n := 0
	for _, v := range rows {
		if authString(v["jti"]) != jti && !authBool(v["is_revoked"]) {
			if e = s.Store.Update(r.Context(), "user_sessions", v["id"], store.Row{"is_revoked": true}); e != nil {
				httpError(w, 500, "Unable to revoke sessions")
				return
			}
			n++
		}
	}
	writeJSON(w, 200, store.Row{"revokedCount": n})
}
func (s *Server) authLockouts(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.authRequired(w, r); !ok {
		return
	}
	a := s.authState()
	a.mu.Lock()
	defer a.mu.Unlock()
	out := []store.Row{}
	for ip, v := range a.failures {
		out = append(out, store.Row{"ip": ip, "failCount": v.count, "firstFailTime": v.first.Unix(), "elapsedSeconds": int(time.Since(v.first).Seconds())})
	}
	writeJSON(w, 200, store.Row{"lockedIps": out, "total": len(out)})
}
func (s *Server) authClearLockout(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.authRequired(w, r); !ok {
		return
	}
	a := s.authState()
	a.mu.Lock()
	defer a.mu.Unlock()
	n := 0
	if ip := r.URL.Query().Get("ip"); ip != "" {
		if _, ok := a.failures[ip]; ok {
			delete(a.failures, ip)
			n = 1
		}
	} else {
		n = len(a.failures)
		clear(a.failures)
	}
	writeJSON(w, 200, store.Row{"message": "已清除登录锁定", "cleared": n})
}

func (s *Server) authAudit(r *http.Request, event, detail string, success bool) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if e := s.recordAuditEvent(ctx, event, s.authClientIP(r), r.UserAgent(), detail, success); e != nil {
		slog.Warn("Could not persist security audit event", "event", event)
	}
}

func (s *Server) authPeek(key string) (any, bool) {
	a := s.authState()
	a.mu.Lock()
	defer a.mu.Unlock()
	v, ok := a.pending[key]
	if !ok || time.Since(v.created) >= 5*time.Minute {
		delete(a.pending, key)
		return nil, false
	}
	return v.value, true
}
func authDateWire(value any) any {
	if value == nil {
		return nil
	}
	if t, ok := value.(time.Time); ok {
		return t.Format("2006-01-02T15:04:05.999999")
	}
	return strings.Replace(authString(value), " ", "T", 1)
}
