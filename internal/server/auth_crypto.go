// SPDX-License-Identifier: AGPL-3.0-only
package server

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/AniBakaBaka/AniDan/internal/store"
	"golang.org/x/crypto/bcrypt"
	"golang.org/x/crypto/pbkdf2"
)

// Fernet preserves upstream's encrypted otp_secret representation. HMAC is
// checked before decrypting. The fixed salt is a legacy storage-format contract.
func authOTPKey(master string) []byte {
	return pbkdf2.Key([]byte(master), []byte("misaka-otp-secret-encryption-v1"), 100000, 32, sha256.New)
}
func authEncryptOTP(secret, master string) (string, error) {
	if master == "" {
		return "", errors.New("OTP encryption key unavailable")
	}
	key := authOTPKey(master)
	plain := []byte(secret)
	padding := aes.BlockSize - len(plain)%aes.BlockSize
	for i := 0; i < padding; i++ {
		plain = append(plain, byte(padding))
	}
	out := make([]byte, 25+len(plain))
	out[0] = 0x80
	binary.BigEndian.PutUint64(out[1:9], uint64(time.Now().Unix()))
	if _, e := rand.Read(out[9:25]); e != nil {
		return "", e
	}
	block, e := aes.NewCipher(key[16:])
	if e != nil {
		return "", e
	}
	cipher.NewCBCEncrypter(block, out[9:25]).CryptBlocks(out[25:], plain)
	mac := hmac.New(sha256.New, key[:16])
	mac.Write(out)
	out = append(out, mac.Sum(nil)...)
	return base64.URLEncoding.EncodeToString(out), nil
}
func authDecryptOTP(value, master string) (string, error) {
	if master == "" {
		return "", errors.New("OTP encryption key unavailable")
	}
	raw, e := base64.URLEncoding.DecodeString(value)
	if e != nil {
		raw, e = base64.RawURLEncoding.DecodeString(value)
	}
	if e != nil || len(raw) < 73 || raw[0] != 0x80 || (len(raw)-57)%16 != 0 {
		return "", errors.New("invalid encrypted OTP secret")
	}
	key := authOTPKey(master)
	mac := hmac.New(sha256.New, key[:16])
	mac.Write(raw[:len(raw)-32])
	if !hmac.Equal(mac.Sum(nil), raw[len(raw)-32:]) {
		return "", errors.New("OTP secret authentication failed")
	}
	block, e := aes.NewCipher(key[16:])
	if e != nil {
		return "", e
	}
	plain := make([]byte, len(raw)-57)
	cipher.NewCBCDecrypter(block, raw[9:25]).CryptBlocks(plain, raw[25:len(raw)-32])
	n := int(plain[len(plain)-1])
	if n < 1 || n > 16 || n > len(plain) {
		return "", errors.New("invalid OTP padding")
	}
	for _, b := range plain[len(plain)-n:] {
		if int(b) != n {
			return "", errors.New("invalid OTP padding")
		}
	}
	return string(plain[:len(plain)-n]), nil
}
func authTOTP(secret string, t time.Time) string {
	key, e := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(strings.TrimRight(strings.ToUpper(secret), "="))
	if e != nil || len(key) == 0 {
		return ""
	}
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], uint64(t.Unix()/30))
	m := hmac.New(sha1.New, key)
	m.Write(b[:])
	sum := m.Sum(nil)
	off := sum[len(sum)-1] & 15
	n := binary.BigEndian.Uint32(sum[off:off+4]) & 0x7fffffff
	return fmt.Sprintf("%06d", n%1000000)
}
func authCheckTOTP(secret, code string, t time.Time) bool {
	if len(code) != 6 {
		return false
	}
	for _, c := range code {
		if c < '0' || c > '9' {
			return false
		}
	}
	valid := 0
	for i := -1; i <= 1; i++ {
		want := authTOTP(secret, t.Add(time.Duration(i)*30*time.Second))
		valid |= subtle.ConstantTimeCompare([]byte(want), []byte(code))
	}
	return valid == 1
}
func (s *Server) authVerifyOTP(u store.Row, code string) bool {
	if !authBool(u["is_otp"]) {
		return false
	}
	stored := authString(u["otp_secret"])
	secret, e := authDecryptOTP(stored, s.Config.JWTSecret)
	if e != nil && authPlainOTP(stored) {
		secret = stored
		e = nil
	}
	return e == nil && authCheckTOTP(secret, code, time.Now())
}
func (s *Server) registerMFA(m *http.ServeMux) {
	p := "/api/ui/auth/mfa"
	m.HandleFunc("GET "+p+"/status", s.authMFAStatus)
	m.HandleFunc("POST "+p+"/totp/setup", s.authTOTPSetup)
	m.HandleFunc("POST "+p+"/totp/verify-setup", s.authTOTPEnable)
	m.HandleFunc("POST "+p+"/totp/disable", s.authTOTPDisable)
	m.HandleFunc("POST "+p+"/verify", s.authMFAVerify)
	s.registerPasskey(m)
}
func (s *Server) authMFAStatus(w http.ResponseWriter, r *http.Request) {
	u, ok := s.authRequired(w, r)
	if !ok {
		return
	}
	keys, e := s.Store.List(r.Context(), "user_passkeys", store.Row{"user_id": u["id"]}, 10000, 0)
	if e != nil {
		httpError(w, 500, "Unable to read passkeys")
		return
	}
	out := []store.Row{}
	for _, v := range keys {
		out = append(out, store.Row{"id": v["id"], "deviceName": v["device_name"], "createdAt": authDateWire(v["created_at"]), "lastUsedAt": authDateWire(v["last_used_at"])})
	}
	writeJSON(w, 200, store.Row{"totpEnabled": authBool(u["is_otp"]), "passkeyCount": len(out), "passkeys": out})
}
func (s *Server) authTOTPSetup(w http.ResponseWriter, r *http.Request) {
	u, ok := s.authRequired(w, r)
	if !ok {
		return
	}
	if authBool(u["is_otp"]) {
		httpError(w, 400, "TOTP 已启用，请先关闭后再重新设置")
		return
	}
	if s.Config.JWTSecret == "" {
		httpError(w, 503, "OTP encryption key unavailable")
		return
	}
	b := make([]byte, 20)
	if _, e := rand.Read(b); e != nil {
		httpError(w, 500, "Unable to generate secret")
		return
	}
	secret := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b)
	name := authString(u["username"])
	issuer := "Misaka弹幕库"
	v := url.Values{"secret": {secret}, "issuer": {issuer}}
	uri := "otpauth://totp/" + url.PathEscape(issuer+":"+name) + "?" + v.Encode()
	s.authPut("totp_"+name, secret)
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, 200, store.Row{"secret": secret, "uri": uri})
}
func (s *Server) authTOTPEnable(w http.ResponseWriter, r *http.Request) {
	if !s.authRateCheck(w, r) {
		return
	}
	u, ok := s.authRequired(w, r)
	if !ok {
		return
	}
	var b struct {
		Code string `json:"code"`
	}
	if readJSON(r, &b) != nil || len(b.Code) != 6 {
		httpError(w, 422, "Invalid OTP code")
		return
	}
	key := "totp_" + authString(u["username"])
	value, ok := s.authPeek(key)
	if !ok {
		httpError(w, 400, "TOTP 设置已过期，请重新生成")
		return
	}
	secret, ok := value.(string)
	if !ok || !authCheckTOTP(secret, b.Code, time.Now()) {
		s.authFailure(r)
		httpError(w, 400, "验证码错误，请重试")
		return
	}
	encrypted, e := authEncryptOTP(secret, s.Config.JWTSecret)
	if e != nil {
		httpError(w, 500, "Unable to encrypt OTP secret")
		return
	}
	if e = s.Store.Update(r.Context(), "users", u["id"], store.Row{"is_otp": true, "otp_secret": encrypted}); e != nil {
		httpError(w, 500, "Unable to enable TOTP")
		return
	}
	s.authTake(key)
	s.authAudit(r, "totp_enable", "TOTP enabled", true)
	writeJSON(w, 200, store.Row{"message": "TOTP 两步验证已启用"})
}
func (s *Server) authTOTPDisable(w http.ResponseWriter, r *http.Request) {
	u, ok := s.authRequired(w, r)
	if !ok {
		return
	}
	var b struct {
		Password string `json:"password"`
	}
	if readJSON(r, &b) != nil {
		httpError(w, 422, "Invalid request")
		return
	}
	if !authBool(u["is_otp"]) {
		httpError(w, 400, "TOTP 未启用")
		return
	}
	if bcrypt.CompareHashAndPassword([]byte(authString(u["hashed_password"])), []byte(b.Password)) != nil {
		httpError(w, 400, "密码错误")
		return
	}
	tx, e := s.Store.DB.BeginTx(r.Context(), nil)
	if e != nil {
		httpError(w, 500, "Unable to begin transaction")
		return
	}
	defer tx.Rollback()
	result, e := tx.ExecContext(r.Context(), s.Store.Rebind("DELETE FROM user_passkeys WHERE user_id = ?"), u["id"])
	if e != nil {
		httpError(w, 500, "Unable to remove passkeys")
		return
	}
	if _, e = tx.ExecContext(r.Context(), s.Store.Rebind("UPDATE users SET is_otp = ?, otp_secret = NULL WHERE id = ?"), false, u["id"]); e != nil {
		httpError(w, 500, "Unable to disable TOTP")
		return
	}
	if e = tx.Commit(); e != nil {
		httpError(w, 500, "Unable to commit MFA update")
		return
	}
	s.authAudit(r, "totp_disable", "TOTP disabled and passkeys removed", true)
	n, _ := result.RowsAffected()
	writeJSON(w, 200, store.Row{"message": fmt.Sprintf("TOTP 两步验证已关闭，同时已清除 %d 个 PassKey", n)})
}
func (s *Server) authMFAVerify(w http.ResponseWriter, r *http.Request) {
	if !s.authRateCheck(w, r) {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	if r.ParseForm() != nil {
		httpError(w, 422, "Invalid form")
		return
	}
	value, ok := s.authTake("mfa_" + r.Form.Get("mfa_token"))
	ticket, typed := value.(authMFATicket)
	if !ok || !typed {
		httpError(w, 401, "MFA 令牌无效或已过期")
		return
	}
	u, e := s.authUser(r.Context(), ticket.Username)
	if e != nil || authInt(u["id"]) != ticket.UserID {
		httpError(w, 401, "用户不存在")
		return
	}
	if code := r.Form.Get("otp_code"); code != "" {
		if !s.authVerifyOTP(u, code) {
			s.authFailure(r)
			httpError(w, 401, "验证码错误")
			return
		}
		if e = s.authUpgradeOTP(r.Context(), u); e != nil {
			httpError(w, 500, "Unable to secure migrated OTP secret")
			return
		}
		s.authIssueResponse(w, r, u)
		return
	}
	if credential := r.Form.Get("passkey_credential"); credential != "" {
		user, e := s.authVerifyAssertion(r, "passkey_auth_"+ticket.Username, credential, ticket.UserID)
		if e != nil {
			s.authFailure(r)
			httpError(w, 400, "PassKey 验证失败")
			return
		}
		s.authIssueResponse(w, r, user)
		return
	}
	httpError(w, 400, "请提供 TOTP 验证码或 PassKey 凭证")
}

func authPlainOTP(value string) bool {
	if len(value) < 16 || len(value) > 128 || value != strings.ToUpper(value) {
		return false
	}
	for _, c := range value {
		if !(c >= 'A' && c <= 'Z' || c >= '2' && c <= '7') {
			return false
		}
	}
	b, e := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(value)
	return e == nil && len(b) >= 10
}
func (s *Server) authUpgradeOTP(ctx context.Context, u store.Row) error {
	stored := authString(u["otp_secret"])
	if !authPlainOTP(stored) {
		return nil
	}
	encrypted, e := authEncryptOTP(stored, s.Config.JWTSecret)
	if e != nil {
		return e
	}
	return s.Store.Update(ctx, "users", u["id"], store.Row{"otp_secret": encrypted})
}
