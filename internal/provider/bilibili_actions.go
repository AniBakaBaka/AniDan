// SPDX-License-Identifier: AGPL-3.0-only
// Login protocol adapted from pinned readable Misaka Bilibili actions at 300ad904.
package provider

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"sort"
	"strings"
	"time"

	"golang.org/x/net/publicsuffix"
)

type BilibiliLoginSession struct {
	b   *Bilibili
	key string
}

func (b *Bilibili) LoginInfo(ctx context.Context) (map[string]any, error) {
	var v map[string]any
	if e := b.get(ctx, "https://api.bilibili.com/x/web-interface/nav", nil, &v); e != nil {
		return nil, e
	}
	data := object(v["data"])
	if data == nil {
		return nil, errors.New("bilibili login status omitted data")
	}
	if data["isLogin"] != true {
		return map[string]any{"isLogin": false}, nil
	}
	return map[string]any{"isLogin": true, "uname": data["uname"], "face": data["face"], "level": at(data, "level_info", "current_level"), "vipStatus": at(data, "vip", "status"), "vipType": at(data, "vip", "type"), "vipDueDate": at(data, "vip", "due_date")}, nil
}
func (b *Bilibili) NewLoginSession(ctx context.Context) (*BilibiliLoginSession, map[string]any, error) {
	jar, e := cookiejar.New(&cookiejar.Options{PublicSuffixList: publicsuffix.List})
	if e != nil {
		return nil, nil, e
	}
	h := b.HTTPProvider
	client := *h.Client
	client.Jar = jar
	// Login challenge is isolated from the active source account and other admins.
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	h.Client = &client
	h.Headers = h.Headers.Clone()
	h.Headers.Del("Cookie")
	if h.MaxBytes <= 0 || h.MaxBytes > 1<<20 {
		h.MaxBytes = 1 << 20
	}
	session := &BilibiliLoginSession{b: &Bilibili{h}}
	key, e := session.b.wbi(ctx)
	if e != nil {
		return nil, nil, e
	}
	q := url.Values{}
	signWBI(q, key, time.Now().Unix())
	var v map[string]any
	if e = session.b.get(ctx, "https://passport.bilibili.com/x/passport-login/web/qrcode/generate", q, &v); e != nil {
		return nil, nil, e
	}
	if integer(v["code"]) != 0 {
		return nil, nil, errors.New("bilibili declined QR generation")
	}
	data := object(v["data"])
	session.key = str(data["qrcode_key"])
	raw := str(data["url"])
	u, e := url.Parse(raw)
	if e != nil || u.Scheme != "https" || u.Host != "passport.bilibili.com" || u.User != nil || !strings.HasPrefix(u.Path, "/h5-app/passport/login/") || len(raw) > 4096 || validID(session.key) != nil {
		return nil, nil, errors.New("bilibili returned invalid login challenge")
	}
	return session, map[string]any{"qrcodeKey": session.key, "url": raw}, nil
}
func (s *BilibiliLoginSession) Poll(ctx context.Context) (map[string]any, string, error) {
	key, e := s.b.wbi(ctx)
	if e != nil {
		return nil, "", e
	}
	q := url.Values{"qrcode_key": {s.key}}
	signWBI(q, key, time.Now().Unix())
	var v map[string]any
	if e = s.b.get(ctx, "https://passport.bilibili.com/x/passport-login/web/qrcode/poll", q, &v); e != nil {
		return nil, "", e
	}
	if integer(v["code"]) != 0 {
		return nil, "", errors.New("bilibili declined login polling")
	}
	data := object(v["data"])
	if _, ok := data["code"]; !ok {
		return nil, "", errors.New("bilibili login status omitted code")
	}
	n, ok := data["code"].(json.Number)
	if !ok {
		return nil, "", errors.New("invalid login status code")
	}
	value, e := n.Int64()
	if e != nil || value < -2147483648 || value > 2147483647 {
		return nil, "", errors.New("invalid login status code")
	}
	code := int(value)
	// The upstream poll body may contain refresh tokens or a credential-bearing
	// redirect. Return only the fields consumed by the login UI.
	message := map[int]string{0: "Login confirmed", 86101: "Waiting for scan", 86090: "Waiting for mobile confirmation", 86038: "QR code expired"}[code]
	if message == "" {
		message = "Bilibili returned a login status"
	}
	out := map[string]any{"code": code, "message": message}
	if code != 0 {
		return out, "", nil
	}
	origin := "https://passport.bilibili.com/"
	if base := s.b.BaseURLs["passport.bilibili.com"]; base != "" {
		origin = base
	}
	u, e := url.Parse(origin)
	if e != nil {
		return nil, "", e
	}
	cookies := s.b.Client.Jar.Cookies(u)
	allowed := map[string]bool{"SESSDATA": true, "bili_jct": true, "DedeUserID": true, "DedeUserID__ckMd5": true, "sid": true, "buvid3": true, "buvid4": true, "buvid_fp": true}
	values := []string{}
	hasSession := false
	for _, c := range cookies {
		if !allowed[c.Name] {
			continue
		}
		if strings.ContainsAny(c.Value, "\r\n\x00;") || len(c.Value) > 8192 {
			return nil, "", errors.New("invalid login cookie")
		}
		values = append(values, c.Name+"="+c.Value)
		if c.Name == "SESSDATA" && c.Value != "" {
			hasSession = true
		}
	}
	if !hasSession {
		return nil, "", errors.New("login was confirmed but no session cookie was supplied")
	}
	sort.Strings(values)
	cookie := strings.Join(values, "; ")
	if len(cookie) > 32768 {
		return nil, "", errors.New("login cookie exceeds limit")
	}
	return out, cookie, nil
}
