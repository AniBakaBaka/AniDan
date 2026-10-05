// SPDX-License-Identifier: AGPL-3.0-only
// Protocol compatibility with misaka_danmu_server notification adapters,
// l429609201 and contributors, AGPL-3.0. No upstream Python is executed.
package notify

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// ErrPartialDelivery means at least one part was accepted; blind retries can duplicate it.
var ErrPartialDelivery = errors.New("notification partially accepted")

// ErrDeliveryUncertain records an attempted send without claiming acceptance.
var ErrDeliveryUncertain = errors.New("notification delivery is unconfirmed; do not blindly retry")

var ErrUnsupported = errors.New("notification feature unsupported")
var ErrForbidden = errors.New("notification authorization failed")
var ErrLimit = errors.New("notification resource limit exceeded")

type Channel struct {
	ID           int64          `json:"id"`
	Name         string         `json:"name"`
	Type         string         `json:"channelType"`
	Enabled      bool           `json:"isEnabled"`
	UseProxy     bool           `json:"useProxy"`
	Config       map[string]any `json:"config"`
	EventsConfig map[string]any `json:"eventsConfig"`
}
type Button struct {
	Text string `json:"text"`
	Data string `json:"callback_data"`
}
type Message struct {
	Title string
	Text  string
	// TextSpans format disjoint rune ranges in Text for Telegram text messages.
	// They are runtime-only metadata and never interpret Title/Text as markup.
	TextSpans []TextSpan `json:"-"`
	Recipient string
	Buttons   [][]Button
	// Image contains PNG or JPEG bytes, never an URL. Send reports unsupported
	// transports and invalid/oversized images rather than dropping the image.
	Image []byte
	// PublicImageURL is an explicitly shareable server-prepared thumbnail, never a private image or login QR.
	PublicImageURL string `json:"imageUrl,omitempty"`
	// ImageShareable explicitly classifies public poster/collage bytes. False is mandatory for private images and login QR codes.
	ImageShareable bool `json:"-"`
	// ImageInteractive marks an explicit conversation image, separate from automatic event rendering.
	ImageInteractive bool `json:"-"`
}
type Command struct {
	Command     string `json:"command"`
	Description string `json:"description"`
}
type Result struct {
	Success bool           `json:"success"`
	Message string         `json:"message"`
	BotInfo map[string]any `json:"botInfo,omitempty"`
}
type Update struct {
	ID         string `json:"id"`
	Sender     string `json:"sender"`
	ChatID     string `json:"chatId"`
	Text       string `json:"text"`
	CallbackID string `json:"callbackId,omitempty"`
	Admin      bool   `json:"admin"`
}
type Event struct {
	ImageURL string `json:"imageUrl,omitempty"`
	Type     string `json:"type"`
	Title    string `json:"title"`
	Text     string `json:"text"`
}

type Field struct {
	Key         string              `json:"key"`
	Label       string              `json:"label"`
	Type        string              `json:"type"`
	Required    bool                `json:"required,omitempty"`
	Default     any                 `json:"default,omitempty"`
	Description string              `json:"description,omitempty"`
	Options     []map[string]string `json:"options,omitempty"`
}

func Str(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case json.Number:
		return x.String()
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case int:
		return strconv.Itoa(x)
	case int64:
		return strconv.FormatInt(x, 10)
	case bool:
		return strconv.FormatBool(x)
	}
	return ""
}
func Bool(v any) bool          { return v == true || Str(v) == "true" || Str(v) == "1" }
func num(v any) int64          { n, _ := strconv.ParseInt(Str(v), 10, 64); return n }
func obj(v any) map[string]any { m, _ := v.(map[string]any); return m }
func Schema(kind string) ([]Field, error) {
	f := []Field{}
	switch kind {
	case "telegram", "serverchan3":
		f = append(f, Field{Key: "bot_token", Label: "Bot Token", Type: "password", Required: true}, Field{Key: "chat_id", Label: "Chat ID", Type: "string"}, Field{Key: "mode", Label: "Interaction mode", Type: "select", Default: "webhook", Options: []map[string]string{{"value": "webhook", "label": "Webhook"}, {"value": "polling", "label": "Polling"}}, Description: "Polling and webhook are mutually exclusive; registration changes require an explicit operator action"}, Field{Key: "polling_timeout", Label: "Polling timeout", Type: "number", Default: 10, Description: "Long-poll wait in seconds, 1 to 30"}, Field{Key: "webhook_secret", Label: "Webhook secret", Type: "password"}, Field{Key: "webhook_base_url", Label: "External access URL", Type: "string", Description: "Stored for setup; registration must be performed explicitly"})
	case "wechat":
		f = append(f, Field{Key: "corp_id", Label: "Corporation ID", Type: "string", Required: true}, Field{Key: "corp_secret", Label: "Corporation secret", Type: "password", Required: true}, Field{Key: "agent_id", Label: "Agent ID", Type: "string", Required: true}, Field{Key: "to_user", Label: "Recipients", Type: "string", Required: true, Description: "Explicit user IDs separated by |; @all is accepted only when explicitly configured"}, Field{Key: "msg_token", Label: "Callback token", Type: "password"}, Field{Key: "encoding_aes_key", Label: "Callback AES key", Type: "password"}, Field{Key: "server_url", Label: "Server URL", Type: "string"})
	default:
		return nil, fmt.Errorf("unknown notification channel %q", kind)
	}
	switch kind {
	case "telegram":
		f = append(f, Field{Key: "telegram_api_proxy", Label: "API HTTPS relay", Type: "string", Description: "Explicit public HTTPS relay; overrides global forward proxy. Provider credentials and message content are sent to this relay. HTTP/private relay hosts are rejected"})
	case "serverchan3":
		f = append(f, Field{Key: "sc3_api_proxy", Label: "API HTTPS relay", Type: "string", Description: "Explicit public HTTPS relay; provider credentials, message content and configured webhook relay key are sent to it"})
	case "wechat":
		f = append(f, Field{Key: "wecom_proxy", Label: "API HTTPS relay", Type: "string", Description: "Public HTTPS relay; supports base, /out and /cgi-bin paths. Provider credentials and message content are sent to it"}, Field{Key: "wecom_proxy_relay_auth", Label: "Send relay authentication key", Type: "boolean", Default: false, Description: "Explicitly sends the runtime webhook API key as X-Relay-Key only to this configured HTTPS relay"})
	}
	f = append(f, Field{Key: "admin_ids", Label: "Admin user IDs", Type: "string", Description: "Comma-separated; required for mutating bot commands"}, Field{Key: "allowed_ids", Label: "Allowed user IDs", Type: "string", Description: "Comma-separated; empty means admins only; empty lists deny all interaction"}, Field{Key: "image_mode", Label: "Image mode", Type: "segmented", Default: "poster", Options: []map[string]string{{"value": "text", "label": "Text"}, {"value": "poster", "label": "Poster"}, {"value": "separate", "label": "Separate"}, {"value": "public_url", "label": "Public URL"}}, Description: "Public URL mode requires an explicitly shareable poster/collage; private images and login QR bytes must never be published"})
	return f, nil
}
func Types() []map[string]any {
	out := []map[string]any{}
	for _, x := range []struct{ k, n, en string }{{"telegram", "Telegram", "Telegram"}, {"serverchan3", "Server酱³", "ServerChan³"}, {"wechat", "企业微信", "WeCom"}} {
		schema, _ := Schema(x.k)
		out = append(out, map[string]any{"channelType": x.k, "displayName": x.n, "displayName_en": x.en, "displayName_tw": x.n, "configSchema": schema, "hideProxy": x.k != "telegram", "capabilities": Capabilities(x.k)})
	}
	return out
}

var tokenRE = regexp.MustCompile(`^[A-Za-z0-9_:\-]{3,512}$`)

func Validate(c Channel) error {
	if _, e := Schema(c.Type); e != nil {
		return e
	}
	if len(c.Name) > 500 {
		return errors.New("notification name too long")
	}
	if err := validateRouting(c); err != nil {
		return err
	}
	if Bool(c.Config["tunnel_enabled"]) {
		return fmt.Errorf("%w: VPS tunnels", ErrUnsupported)
	}
	if mode := Str(c.Config["image_mode"]); mode != "" && mode != "text" && mode != "poster" && mode != "separate" && mode != "public_url" {
		return fmt.Errorf("%w: image mode %s", ErrUnsupported, mode)
	}
	if len(c.Config) > 100 {
		return ErrLimit
	}
	for _, v := range c.Config {
		if len(Str(v)) > 16384 || strings.ContainsAny(Str(v), "\x00\r\n") {
			return errors.New("invalid notification configuration")
		}
	}
	if c.Type == "wechat" {
		for _, key := range []string{"corp_id", "corp_secret", "agent_id"} {
			if Str(c.Config[key]) == "" {
				return fmt.Errorf("%s is required", key)
			}
		}
		if num(c.Config["agent_id"]) <= 0 {
			return errors.New("agent_id must be positive")
		}
		key, token := Str(c.Config["encoding_aes_key"]), Str(c.Config["msg_token"])
		if (key == "") != (token == "") {
			return errors.New("callback token and AES key must be configured together")
		}
		if key != "" {
			if _, e := newCrypt(token, key, Str(c.Config["corp_id"])); e != nil {
				return e
			}
		}
	} else {
		if !tokenRE.MatchString(Str(c.Config["bot_token"])) {
			return errors.New("invalid bot_token")
		}
		if mode := Str(c.Config["mode"]); mode != "" && mode != "webhook" && mode != "polling" {
			return fmt.Errorf("%w: interaction mode", ErrUnsupported)
		}
		if value := Str(c.Config["polling_timeout"]); value != "" {
			timeout, err := strconv.ParseInt(value, 10, 64)
			if err != nil || timeout < 1 || timeout > 30 {
				return errors.New("polling_timeout must be between 1 and 30 seconds")
			}
		}
	}
	return nil
}

// Capabilities describes protocol support, not whether a channel is enabled,
// configured, currently polling, or has been registered with its provider.
func Capabilities(kind string) map[string]bool {
	bot := kind == "telegram" || kind == "serverchan3"
	return map[string]bool{
		"polling":             bot,
		"webhook":             bot || kind == "wechat",
		"imageBytes":          kind == "telegram" || kind == "wechat",
		"inlineButtons":       kind == "telegram",
		"commandRegistration": kind == "telegram" || kind == "wechat",
		"webhookRegistration": kind == "telegram",
		"textMessageEditing":  kind == "telegram",
	}
}
func Secrets() []string {
	return []string{"bot_token", "corp_secret", "msg_token", "encoding_aes_key", "webhook_secret", "__webhook_api_key", "__proxy_url", "telegram_api_proxy", "sc3_api_proxy", "wecom_proxy"}
}

// secretValuePresent deliberately does not use Str: disabled/imported channels
// may contain malformed objects, arrays, numbers or booleans in secret fields.
// Their content must never be reflected even when validation has not run.
func secretValuePresent(value any) bool {
	if value == nil {
		return false
	}
	if text, ok := value.(string); ok {
		return text != ""
	}
	return true
}
func Mask(config map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range config {
		out[k] = v
		lower := strings.ToLower(k)
		if (strings.Contains(lower, "secret") || strings.Contains(lower, "token") || strings.Contains(lower, "password") || strings.Contains(lower, "aes_key") || strings.Contains(lower, "api_key")) && secretValuePresent(v) {
			out[k] = "********"
		}
	}
	for _, k := range Secrets() {
		if secretValuePresent(out[k]) {
			out[k] = "********"
		}
	}
	return out
}
func MergeSecrets(old, next map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range next {
		out[k] = v
	}
	for _, k := range Secrets() {
		if Str(out[k]) == "********" {
			if v, ok := old[k]; ok {
				out[k] = v
			} else {
				delete(out, k)
			}
		}
	}
	return out
}
func contains(list, id string) bool {
	if id == "" {
		return false
	}
	for _, v := range strings.FieldsFunc(list, func(r rune) bool { return r == ',' || r == '|' }) {
		if strings.TrimSpace(v) == id {
			return true
		}
	}
	return false
}
func Authorize(c Channel, sender string) (bool, bool) {
	admin := contains(Str(c.Config["admin_ids"]), sender)
	return admin || contains(Str(c.Config["allowed_ids"]), sender), admin
}
