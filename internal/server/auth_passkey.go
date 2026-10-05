// SPDX-License-Identifier: AGPL-3.0-only
package server

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/AniBakaBaka/AniDan/internal/store"
	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"
)

type authWebUser struct {
	row         store.Row
	credentials []webauthn.Credential
}

func (u authWebUser) WebAuthnID() []byte                         { return []byte(strconv.FormatInt(authInt(u.row["id"]), 10)) }
func (u authWebUser) WebAuthnName() string                       { return authString(u.row["username"]) }
func (u authWebUser) WebAuthnDisplayName() string                { return u.WebAuthnName() }
func (u authWebUser) WebAuthnCredentials() []webauthn.Credential { return u.credentials }

type authWebChallenge struct {
	Session      webauthn.SessionData
	Origin, RPID string
}

func authDecodeBase64(v string) ([]byte, error) {
	b, e := base64.RawURLEncoding.DecodeString(v)
	if e != nil {
		return base64.URLEncoding.DecodeString(v)
	}
	return b, e
}
func (s *Server) authWebUser(ctx context.Context, row store.Row) (authWebUser, error) {
	u := authWebUser{row: row}
	rows, e := s.Store.List(ctx, "user_passkeys", store.Row{"user_id": row["id"]}, 10000, 0)
	if e != nil {
		return u, e
	}
	for _, r := range rows {
		id, e := authDecodeBase64(authString(r["credential_id"]))
		if e != nil {
			return u, e
		}
		pub, e := authDecodeBase64(authString(r["public_key"]))
		if e != nil {
			return u, e
		}
		c := webauthn.Credential{ID: id, PublicKey: pub, Authenticator: webauthn.Authenticator{SignCount: uint32(authInt(r["sign_count"]))}}
		for _, t := range strings.Split(authString(r["transports"]), ",") {
			if t != "" {
				c.Transport = append(c.Transport, protocol.AuthenticatorTransport(t))
			}
		}
		u.credentials = append(u.credentials, c)
	}
	return u, nil
}

// Never use the request's Origin as the expected origin. A fixed public URL is
// mandatory for non-loopback deployments, including TLS-terminating proxies.
func (s *Server) authWebConfig(r *http.Request) (*webauthn.WebAuthn, string, error) {
	origin := strings.TrimRight(s.Config.PublicURL, "/")
	if origin == "" {
		scheme := "http"
		if r.TLS != nil {
			scheme = "https"
		}
		u, e := url.Parse(scheme + "://" + r.Host)
		if e != nil {
			return nil, "", errors.New("invalid host")
		}
		host := u.Hostname()
		if host != "localhost" && !authAddr(host).IsLoopback() {
			return nil, "", errors.New("ANIDAN_PUBLIC_URL is required for passkeys")
		}
		origin = u.Scheme + "://" + u.Host
	}
	u, e := url.Parse(origin)
	if e != nil || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return nil, "", errors.New("invalid public origin")
	}
	if u.Scheme != "https" && !(u.Scheme == "http" && (u.Hostname() == "localhost" || authAddr(u.Hostname()).IsLoopback())) {
		return nil, "", errors.New("passkeys require HTTPS or localhost")
	}
	if v := r.Header.Get("Origin"); v != "" && strings.TrimRight(v, "/") != origin {
		return nil, "", errors.New("origin is not allowed")
	}
	wa, e := webauthn.New(&webauthn.Config{RPID: u.Hostname(), RPDisplayName: "Misaka弹幕库", RPOrigins: []string{origin}, AuthenticatorSelection: protocol.AuthenticatorSelection{ResidentKey: protocol.ResidentKeyRequirementPreferred, UserVerification: protocol.VerificationPreferred}, Timeouts: webauthn.TimeoutsConfig{Login: webauthn.TimeoutConfig{Enforce: true, Timeout: 5 * time.Minute}, Registration: webauthn.TimeoutConfig{Enforce: true, Timeout: 5 * time.Minute}}})
	return wa, origin, e
}
func (s *Server) registerPasskey(m *http.ServeMux) {
	p := "/api/ui/auth/mfa/passkey"
	m.HandleFunc("POST "+p+"/register/options", s.authPasskeyRegisterOptions)
	m.HandleFunc("POST "+p+"/register/verify", s.authPasskeyRegisterVerify)
	m.HandleFunc("POST "+p+"/authenticate/options", s.authPasskeyOptions)
	m.HandleFunc("POST "+p+"/authenticate/verify", s.authPasskeyVerify)
	m.HandleFunc("POST "+p+"/login/options", s.authPasskeyLoginOptions)
	m.HandleFunc("POST "+p+"/login/verify", s.authPasskeyLoginVerify)
	m.HandleFunc("PUT "+p+"/{passkey_id}/rename", s.authPasskeyRename)
	m.HandleFunc("DELETE "+p+"/{passkey_id}", s.authPasskeyDelete)
}
func (s *Server) authPasskeyRegisterOptions(w http.ResponseWriter, r *http.Request) {
	row, ok := s.authRequired(w, r)
	if !ok {
		return
	}
	if !authBool(row["is_otp"]) {
		httpError(w, 400, "请先启用 TOTP 两步验证后再注册 PassKey")
		return
	}
	wa, origin, e := s.authWebConfig(r)
	if e != nil {
		httpError(w, 400, e.Error())
		return
	}
	u, e := s.authWebUser(r.Context(), row)
	if e != nil {
		httpError(w, 500, "Unable to read credentials")
		return
	}
	exclude := []protocol.CredentialDescriptor{}
	for _, c := range u.credentials {
		exclude = append(exclude, c.Descriptor())
	}
	opts, session, e := wa.BeginRegistration(u, webauthn.WithExclusions(exclude), webauthn.WithRegistrationOrigin(origin))
	if e != nil {
		httpError(w, 500, "Unable to create registration challenge")
		return
	}
	s.authPut("passkey_reg_"+u.WebAuthnName(), authWebChallenge{*session, origin, wa.Config.RPID})
	data, e := json.Marshal(opts.Response)
	if e != nil {
		httpError(w, 500, "Unable to encode options")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, 200, store.Row{"options": string(data)})
}
func (s *Server) authPasskeyRegisterVerify(w http.ResponseWriter, r *http.Request) {
	row, ok := s.authRequired(w, r)
	if !ok {
		return
	}
	if !authBool(row["is_otp"]) {
		httpError(w, 400, "TOTP must remain enabled")
		return
	}
	var b struct {
		Credential string `json:"credential"`
		Name       string `json:"deviceName"`
	}
	if readJSON(r, &b) != nil || len(b.Credential) > 1<<20 || len(b.Name) > 500 {
		httpError(w, 422, "Invalid credential")
		return
	}
	v, ok := s.authTake("passkey_reg_" + authString(row["username"]))
	ch, typed := v.(authWebChallenge)
	if !ok || !typed {
		httpError(w, 400, "注册已过期，请重新开始")
		return
	}
	wa, origin, e := s.authWebConfig(r)
	if e != nil || origin != ch.Origin || wa.Config.RPID != ch.RPID {
		httpError(w, 400, "Origin mismatch")
		return
	}
	u, e := s.authWebUser(r.Context(), row)
	if e != nil {
		httpError(w, 500, "Unable to read user")
		return
	}
	parsed, e := protocol.ParseCredentialCreationResponseBytes([]byte(b.Credential))
	if e != nil {
		httpError(w, 400, "Invalid credential response")
		return
	}
	cred, e := wa.CreateCredential(u, ch.Session, parsed)
	if e != nil {
		httpError(w, 400, "PassKey 注册验证失败")
		return
	}
	if b.Name == "" {
		b.Name = "未命名设备"
	}
	transports := []string{}
	for _, t := range cred.Transport {
		transports = append(transports, string(t))
	}
	id, e := s.Store.Insert(r.Context(), "user_passkeys", store.Row{"user_id": row["id"], "credential_id": base64.RawURLEncoding.EncodeToString(cred.ID), "public_key": base64.RawURLEncoding.EncodeToString(cred.PublicKey), "sign_count": int64(cred.Authenticator.SignCount), "device_name": b.Name, "transports": strings.Join(transports, ","), "created_at": s.authNow()})
	if e != nil {
		httpError(w, 409, "PassKey already registered or could not be saved")
		return
	}
	s.authAudit(r, "passkey_register", "PassKey registered", true)
	writeJSON(w, 200, store.Row{"message": "PassKey 注册成功", "id": id})
}
func (s *Server) authBeginAssertion(w http.ResponseWriter, r *http.Request, name, key, sessionID string) {
	wa, origin, e := s.authWebConfig(r)
	if e != nil {
		httpError(w, 400, e.Error())
		return
	}
	var opts *protocol.CredentialAssertion
	var session *webauthn.SessionData
	if name != "" {
		row, e := s.authUser(r.Context(), name)
		if e == nil {
			u, e := s.authWebUser(r.Context(), row)
			if e == nil && len(u.credentials) > 0 {
				opts, session, e = wa.BeginLogin(u, webauthn.WithLoginOrigin(origin))
				if e != nil {
					httpError(w, 400, "Unable to create assertion options")
					return
				}
			}
		}
	}
	if opts == nil {
		opts, session, e = wa.BeginDiscoverableLogin(webauthn.WithLoginOrigin(origin))
		if e != nil {
			httpError(w, 400, "Unable to create assertion options")
			return
		}
	}
	s.authPut(key, authWebChallenge{*session, origin, wa.Config.RPID})
	data, _ := json.Marshal(opts.Response)
	out := store.Row{"options": string(data)}
	if sessionID != "" {
		out["sessionId"] = sessionID
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, 200, out)
}
func (s *Server) authPasskeyOptions(w http.ResponseWriter, r *http.Request) {
	if !s.authRateCheck(w, r) {
		return
	}
	name := r.URL.Query().Get("username")
	s.authBeginAssertion(w, r, name, "passkey_auth_"+name, "")
}
func (s *Server) authPasskeyLoginOptions(w http.ResponseWriter, r *http.Request) {
	if !s.authRateCheck(w, r) {
		return
	}
	id := authRandom()
	s.authBeginAssertion(w, r, "", "passkey_login_"+id, id)
}
func (s *Server) authVerifyAssertion(r *http.Request, key, credential string, expectedUser int64) (store.Row, error) {
	value, ok := s.authTake(key)
	ch, typed := value.(authWebChallenge)
	if !ok || !typed {
		return nil, errors.New("expired challenge")
	}
	if !ch.Session.Expires.IsZero() && time.Now().After(ch.Session.Expires) {
		return nil, errors.New("expired WebAuthn session")
	}
	wa, origin, e := s.authWebConfig(r)
	if e != nil || origin != ch.Origin || wa.Config.RPID != ch.RPID {
		return nil, errors.New("origin mismatch")
	}
	if len(credential) > 1<<20 {
		return nil, errors.New("credential too large")
	}
	parsed, e := protocol.ParseCredentialRequestResponseBytes([]byte(credential))
	if e != nil {
		return nil, e
	}
	encoded := base64.RawURLEncoding.EncodeToString(parsed.RawID)
	row, e := s.authFind(r.Context(), "user_passkeys", store.Row{"credential_id": encoded})
	if e != nil {
		return nil, errors.New("unknown credential")
	}
	userID := authInt(row["user_id"])
	if expectedUser != 0 && userID != expectedUser {
		return nil, errors.New("credential belongs to a different user")
	}
	if len(ch.Session.UserID) > 0 && !bytes.Equal(ch.Session.UserID, []byte(strconv.FormatInt(userID, 10))) {
		return nil, errors.New("session user mismatch")
	}
	if h := parsed.Response.UserHandle; len(h) > 0 && !bytes.Equal(h, []byte(strconv.FormatInt(userID, 10))) {
		return nil, errors.New("user handle mismatch")
	}
	if len(ch.Session.AllowedCredentialIDs) > 0 {
		allowed := false
		for _, id := range ch.Session.AllowedCredentialIDs {
			if bytes.Equal(id, parsed.RawID) {
				allowed = true
			}
		}
		if !allowed {
			return nil, errors.New("credential not allowed")
		}
	}
	pub, e := authDecodeBase64(authString(row["public_key"]))
	if e != nil {
		return nil, e
	}
	if e = parsed.Verify(ch.Session.Challenge, ch.RPID, "", []string{ch.Origin}, nil, nil, protocol.TopOriginExplicitVerificationMode, false, ch.Session.UserVerification == protocol.VerificationRequired, true, pub, protocol.SignaturePolicy{}); e != nil {
		return nil, e
	}
	flags := parsed.Response.AuthenticatorData.Flags
	if flags.HasBackupState() && !flags.HasBackupEligible() {
		return nil, errors.New("invalid backup flags")
	}
	old := authInt(row["sign_count"])
	next := int64(parsed.Response.AuthenticatorData.Counter)
	if (next != 0 || old != 0) && next <= old {
		return nil, errors.New("signature counter did not increase")
	}
	result, e := s.Store.DB.ExecContext(r.Context(), s.Store.Rebind("UPDATE user_passkeys SET sign_count = ?, last_used_at = ? WHERE id = ? AND sign_count = ?"), next, s.authNow(), row["id"], old)
	if e != nil {
		return nil, e
	}
	n, e := result.RowsAffected()
	if e != nil || n != 1 {
		return nil, errors.New("concurrent credential use; retry")
	}
	return s.Store.Get(r.Context(), "users", userID)
}
func (s *Server) authPasskeyVerify(w http.ResponseWriter, r *http.Request) {
	if !s.authRateCheck(w, r) {
		return
	}
	var b struct {
		Credential string `json:"credential"`
	}
	if readJSON(r, &b) != nil {
		httpError(w, 422, "Invalid credential")
		return
	}
	name := r.URL.Query().Get("username")
	var expected int64
	if name != "" {
		u, e := s.authUser(r.Context(), name)
		if e != nil {
			httpError(w, 400, "Unknown user")
			return
		}
		expected = authInt(u["id"])
	}
	u, e := s.authVerifyAssertion(r, "passkey_auth_"+name, b.Credential, expected)
	if e != nil {
		s.authFailure(r)
		httpError(w, 400, "PassKey 认证验证失败")
		return
	}
	writeJSON(w, 200, store.Row{"verified": true, "userId": u["id"]})
}
func (s *Server) authPasskeyLoginVerify(w http.ResponseWriter, r *http.Request) {
	if !s.authRateCheck(w, r) {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	if r.ParseForm() != nil {
		httpError(w, 422, "Invalid login form")
		return
	}
	u, e := s.authVerifyAssertion(r, "passkey_login_"+r.Form.Get("session_id"), r.Form.Get("credential"), 0)
	if e != nil {
		s.authFailure(r)
		httpError(w, 400, "PassKey 登录验证失败")
		return
	}
	s.authIssueResponse(w, r, u)
}
func (s *Server) authPasskeyOwned(w http.ResponseWriter, r *http.Request) (store.Row, bool) {
	u, ok := s.authRequired(w, r)
	if !ok {
		return nil, false
	}
	id, e := idParam(r, "passkey_id")
	if e != nil {
		httpError(w, 422, "Invalid ID")
		return nil, false
	}
	row, e := s.Store.Get(r.Context(), "user_passkeys", id)
	if e != nil || row == nil || authInt(row["user_id"]) != authInt(u["id"]) {
		httpError(w, 404, "PassKey not found")
		return nil, false
	}
	return row, true
}
func (s *Server) authPasskeyRename(w http.ResponseWriter, r *http.Request) {
	row, ok := s.authPasskeyOwned(w, r)
	if !ok {
		return
	}
	var b struct {
		Name string `json:"deviceName"`
	}
	if readJSON(r, &b) != nil || len(b.Name) > 500 {
		httpError(w, 422, "Invalid name")
		return
	}
	if e := s.Store.Update(r.Context(), "user_passkeys", row["id"], store.Row{"device_name": b.Name}); e != nil {
		httpError(w, 500, "Unable to rename PassKey")
		return
	}
	writeJSON(w, 200, store.Row{"message": "重命名成功"})
}
func (s *Server) authPasskeyDelete(w http.ResponseWriter, r *http.Request) {
	row, ok := s.authPasskeyOwned(w, r)
	if !ok {
		return
	}
	if e := s.Store.Delete(r.Context(), "user_passkeys", row["id"]); e != nil {
		httpError(w, 500, "Unable to delete PassKey")
		return
	}
	s.authAudit(r, "passkey_delete", "PassKey deleted", true)
	writeJSON(w, 200, store.Row{"message": "PassKey 已删除"})
}
