// SPDX-License-Identifier: AGPL-3.0-only
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

type tokenEntry struct {
	value   string
	expires time.Time
	owner   string
}
type Service struct {
	Client   *http.Client
	BaseURLs map[string]string
	Now      func() time.Time
	// Optional caller policy is applied after endpoint routing, before any HTTP.
	// Set once before concurrent use; failures are redacted. Useful for fixtures
	// that must refuse every non-loopback destination.
	RequestGuard func(*http.Request) error
	// Routing is configured once before concurrent use; its results are snapshotted per operation.
	Routing     func(context.Context, Channel) (RoutingConfig, error)
	routes      map[string]routeTransport
	closed      bool
	lookupIP    relayLookup
	dialContext func(context.Context, string, string) (net.Conn, error)
	gate        chan struct{}
	pollGate    chan struct{}
	imageGate   chan struct{}
	mu          sync.Mutex
	tokens      map[string]tokenEntry
}

func NewService(client *http.Client) *Service {
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	cp := *client
	cp.Jar = nil
	if cp.Transport == nil {
		cp.Transport = boundedTransport()
	} else if tr, ok := cp.Transport.(*http.Transport); ok {
		cp.Transport = constrainTransport(tr)
	}
	if cp.Timeout <= 0 || cp.Timeout > 15*time.Second {
		cp.Timeout = 15 * time.Second
	}
	cp.CheckRedirect = func(*http.Request, []*http.Request) error { return errors.New("notification redirects are forbidden") }
	return &Service{Client: &cp, BaseURLs: map[string]string{}, Now: time.Now, gate: make(chan struct{}, 4), pollGate: make(chan struct{}, 4), imageGate: make(chan struct{}, 2), tokens: map[string]tokenEntry{}, routes: map[string]routeTransport{}}
}
func (s *Service) request(ctx context.Context, c Channel, method, raw string, payload any) (map[string]any, error) {
	var body []byte
	if payload != nil {
		var err error
		body, err = json.Marshal(payload)
		if err != nil {
			return nil, errors.New("invalid notification payload")
		}
		if len(body) > 1<<20 {
			return nil, ErrLimit
		}
	}
	return s.requestBody(ctx, c, method, raw, body, "application/json", s.Client, s.gate)
}

// requestBody shares bounded response parsing and redacted errors across JSON,
// multipart uploads and polling. Polling has its own semaphore/client timeout so
// long polls cannot occupy the slots needed to deliver replies.
func (s *Service) requestBody(ctx context.Context, c Channel, method, raw string, body []byte, contentType string, client *http.Client, gate chan struct{}) (map[string]any, error) {
	ctx, cancel := context.WithTimeout(ctx, client.Timeout)
	defer cancel()
	select {
	case gate <- struct{}{}:
		defer func() { <-gate }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	ctx, e := s.withRoute(ctx, c)
	if e != nil {
		return nil, e
	}
	route := ctx.Value(routeContextKey{}).(*outboundRoute)
	client, e = s.routeClient(route, client)
	if e != nil {
		return nil, e
	}
	u, e := url.Parse(raw)
	if e != nil || u.Scheme != "https" || u.User != nil || u.Fragment != "" || (u.Host != "api.telegram.org" && u.Host != "bot-go.apijia.cn" && u.Host != "qyapi.weixin.qq.com") {
		return nil, errors.New("invalid notification endpoint")
	}
	u, headers := routedEndpoint(route, u, c)
	if b, ok := s.BaseURLs[u.Host]; ok && route.relay == nil && route.forward == nil {
		base, e := url.Parse(b)
		if e != nil || base.Host == "" || base.User != nil || (base.Scheme != "http" && base.Scheme != "https") {
			return nil, errors.New("invalid notification test endpoint")
		}
		u.Scheme = base.Scheme
		u.Host = base.Host
		u.Path = strings.TrimRight(base.Path, "/") + u.Path
	}
	requestCtx := context.WithValue(ctx, requestContextKey{}, ctx)
	r, e := http.NewRequestWithContext(requestCtx, method, u.String(), bytes.NewReader(body))
	if e != nil {
		return nil, errors.New("invalid notification request")
	}
	r.Header = headers
	r.Header.Set("Content-Type", contentType)
	if s.RequestGuard != nil {
		if err := s.RequestGuard(r); err != nil {
			return nil, errors.New("notification request blocked by caller policy")
		}
	}
	resp, e := client.Do(r)
	if e != nil {
		return nil, transportFailure(ctx, e, "notification network request failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("notification service HTTP %d", resp.StatusCode)
	}
	b, e := io.ReadAll(io.LimitReader(resp.Body, (1<<20)+1))
	if e != nil {
		return nil, transportFailure(ctx, e, "notification response read failed")
	}
	if len(b) > 1<<20 {
		return nil, ErrLimit
	}
	var v map[string]any
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	if e = d.Decode(&v); e != nil || v == nil {
		return nil, errors.New("notification service returned invalid JSON")
	}
	var trailing any
	if d.Decode(&trailing) != io.EOF {
		return nil, errors.New("notification service returned trailing JSON")
	}
	return v, nil
}
func (s *Service) botURL(c Channel, method string) string {
	host := "api.telegram.org"
	if c.Type == "serverchan3" {
		host = "bot-go.apijia.cn"
	}
	return "https://" + host + "/bot" + Str(c.Config["bot_token"]) + "/" + method
}
func botOK(v map[string]any) error {
	ok, exists := v["ok"].(bool)
	if !exists || !ok {
		return fmt.Errorf("bot API rejected request (code %s)", responseCode(v["error_code"]))
	}
	return nil
}
func (s *Service) weToken(ctx context.Context, c Channel, force bool) (string, error) {
	ctx, err := s.withRoute(ctx, c)
	if err != nil {
		return "", err
	}
	route := ctx.Value(routeContextKey{}).(*outboundRoute)
	key := digest(route.fingerprint, Str(c.Config["corp_id"]), Str(c.Config["corp_secret"]))
	s.mu.Lock()
	for oldKey, entry := range s.tokens {
		if entry.owner == routeOwner(c) && oldKey != key {
			delete(s.tokens, oldKey)
		}
	}
	if tok, ok := s.tokens[key]; ok && !force && s.Now().Before(tok.expires) {
		s.mu.Unlock()
		return tok.value, nil
	}
	s.mu.Unlock()
	q := url.Values{"corpid": {Str(c.Config["corp_id"])}, "corpsecret": {Str(c.Config["corp_secret"])}}
	v, e := s.request(ctx, c, "GET", "https://qyapi.weixin.qq.com/cgi-bin/gettoken?"+q.Encode(), nil)
	if e != nil {
		return "", e
	}
	if v["errcode"] == nil || !zeroCode(v["errcode"]) || Str(v["access_token"]) == "" {
		return "", fmt.Errorf("WeCom token request rejected (code %s)", responseCode(v["errcode"]))
	}
	token := Str(v["access_token"])
	expiry := num(v["expires_in"])
	if expiry <= 0 || expiry > 86400 {
		expiry = 7200
	}
	s.mu.Lock()
	if len(s.tokens) >= 256 {
		s.tokens = map[string]tokenEntry{}
	}
	if !s.closed {
		s.tokens[key] = tokenEntry{token, s.Now().Add(time.Duration(max(expiry-300, 1)) * time.Second), routeOwner(c)}
	}
	s.mu.Unlock()
	return token, nil
}
func (s *Service) Send(ctx context.Context, c Channel, m Message) error {
	m = snapshotTextSpans(m)
	if err := ValidateMessageShape(c, m); err != nil {
		return err
	}
	var routeErr error
	ctx, routeErr = s.withRoute(ctx, c)
	if routeErr != nil {
		return routeErr
	}
	if e := Validate(c); e != nil {
		return e
	}
	if !c.Enabled {
		return errors.New("notification channel is disabled")
	}
	if len(m.Image) > 0 && m.PublicImageURL != "" {
		return errors.New("notification image bytes and public URL are mutually exclusive")
	}
	text := joinedMessageText(m)
	if !utf8.ValidString(text) || len(text) > 16384 {
		return ErrLimit
	}
	if len(m.Buttons) > 0 && c.Type != "telegram" {
		return fmt.Errorf("%w: inline buttons; caller must provide numbered text choices", ErrUnsupported)
	}
	markup, err := buttonMarkup(m.Buttons)
	if err != nil {
		return err
	}
	if len(m.Image) > 0 && c.Type == "serverchan3" {
		return fmt.Errorf("%w: ServerChan3 image upload", ErrUnsupported)
	}
	recipient := m.Recipient
	if c.Type == "wechat" {
		if recipient == "" {
			recipient = Str(c.Config["to_user"])
		}
		if recipient == "" {
			return errors.New("explicit WeCom recipient is required")
		}
		if !validRecipient(recipient) {
			return ErrLimit
		}
		if len(text) > 2048 {
			return ErrLimit
		}
		if m.PublicImageURL != "" {
			return s.sendPublicImage(ctx, c, m, text, recipient, markup)
		}
		if len(m.Image) > 0 {
			if err := s.sendWeImage(ctx, c, recipient, m.Image); err != nil {
				return err
			}
			if text == "" {
				return nil
			}
		}
		if err := s.weSend(ctx, c, map[string]any{"touser": recipient, "msgtype": "text", "agentid": num(c.Config["agent_id"]), "text": map[string]string{"content": text}}); err != nil {
			if len(m.Image) > 0 {
				return fmt.Errorf("%w: WeCom image accepted, accompanying text failed: %w", ErrPartialDelivery, err)
			}
			return err
		}
		return nil
	}
	if recipient == "" {
		recipient = Str(c.Config["chat_id"])
	}
	if recipient == "" {
		return errors.New("chat_id is required")
	}
	if !validRecipient(recipient) {
		return ErrLimit
	}
	if m.PublicImageURL != "" {
		return s.sendPublicImage(ctx, c, m, text, recipient, markup)
	}
	if len(m.Image) > 0 {
		if len([]rune(text)) > 1024 {
			return ErrLimit
		}
		return s.sendTelegramImage(ctx, c, recipient, text, markup, m.Image)
	}
	if len([]rune(text)) > 4096 {
		return ErrLimit
	}
	payload := map[string]any{"chat_id": recipient, "text": text}
	if c.Type == "serverchan3" {
		id, err := strconv.ParseInt(recipient, 10, 64)
		if err != nil {
			return errors.New("ServerChan3 chat_id must be numeric")
		}
		payload["chat_id"] = id
	}
	if markup != nil {
		payload["reply_markup"] = markup
	}
	if c.Type == "telegram" {
		payload, err = telegramTextPayload(m, recipient, "", markup, false)
		if err != nil {
			return err
		}
	}
	v, e := s.request(ctx, c, "POST", s.botURL(c, "sendMessage"), payload)
	if e != nil {
		return e
	}
	return botOK(v)
}
func (s *Service) Test(ctx context.Context, c Channel) Result {
	var routeErr error
	ctx, routeErr = s.withRoute(ctx, c)
	if routeErr != nil {
		return Result{Message: routeErr.Error()}
	}
	if e := Validate(c); e != nil {
		return Result{Message: e.Error()}
	}
	c.Enabled = true
	if c.Type == "wechat" {
		e := s.Send(ctx, c, Message{Title: "AniDan notification test", Text: "The WeCom channel is connected."})
		if e != nil {
			return Result{Message: e.Error()}
		}
		return Result{Success: true, Message: "Connection verified and test message accepted"}
	}
	v, e := s.request(ctx, c, "GET", s.botURL(c, "getMe"), nil)
	if e == nil {
		e = botOK(v)
	}
	if e != nil {
		return Result{Message: e.Error()}
	}
	result := Result{Success: true, Message: "Bot connection verified", BotInfo: safeBotInfo(obj(v["result"]))}
	if Str(c.Config["chat_id"]) != "" {
		if e = s.Send(ctx, c, Message{Title: "AniDan notification test", Text: "The notification channel is connected."}); e != nil {
			return Result{Message: e.Error()}
		}
		result.Message = "Connection verified and test message accepted"
	}
	return result
}
func (s *Service) AnswerCallback(ctx context.Context, c Channel, id, text string) error {
	if id == "" || c.Type != "telegram" {
		return nil
	}
	if e := Validate(c); e != nil {
		return e
	}
	if len(id) > 256 || len([]rune(text)) > 200 || !utf8.ValidString(text) {
		return ErrLimit
	}
	v, e := s.request(ctx, c, "POST", s.botURL(c, "answerCallbackQuery"), map[string]string{"callback_query_id": id, "text": text})
	if e != nil {
		return e
	}
	return botOK(v)
}
func (s *Service) Forget(c Channel) {
	s.mu.Lock()
	defer s.mu.Unlock()
	owner := routeOwner(c)
	for key, entry := range s.tokens {
		if entry.owner == owner {
			delete(s.tokens, key)
		}
	}
	if entry, ok := s.routes[owner]; ok {
		entry.transport.CloseIdleConnections()
		delete(s.routes, owner)
	}
}

func zeroCode(v any) bool { return Str(v) == "0" }
func safeBotInfo(in map[string]any) map[string]any {
	out := map[string]any{}
	for _, k := range []string{"id", "is_bot", "first_name", "username", "can_join_groups", "can_read_all_group_messages", "supports_inline_queries"} {
		if v, ok := in[k]; ok {
			out[k] = v
		}
	}
	return out
}

// Provider descriptions and nonnumeric "codes" can echo secrets and are never
// exposed through errors/status endpoints.
func responseCode(v any) string {
	code, err := strconv.ParseInt(Str(v), 10, 32)
	if err != nil {
		return "unknown"
	}
	return strconv.FormatInt(code, 10)
}

func validRecipient(recipient string) bool {
	return len(recipient) <= 16384 && utf8.ValidString(recipient) && !strings.ContainsAny(recipient, "\x00\r\n")
}
