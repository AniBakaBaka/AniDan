// SPDX-License-Identifier: AGPL-3.0-only
package notify

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha1"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

type weCrypt struct {
	key         []byte
	token, corp string
}

func newCrypt(token, key, corp string) (*weCrypt, error) {
	b, e := base64.StdEncoding.DecodeString(key + "=")
	if e != nil || len(key) != 43 || len(b) != 32 || token == "" || corp == "" {
		return nil, errors.New("invalid WeCom callback cryptography configuration")
	}
	return &weCrypt{b, token, corp}, nil
}
func signed(parts ...string) string {
	sort.Strings(parts)
	sum := sha1.Sum([]byte(strings.Join(parts, "")))
	return hex.EncodeToString(sum[:])
}
func secureEqual(a, b string) bool {
	return len(a) == len(b) && subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}
func (w *weCrypt) decrypt(encoded string) (string, error) {
	raw, e := base64.StdEncoding.DecodeString(encoded)
	if e != nil || len(raw) < 32 || len(raw)%aes.BlockSize != 0 {
		return "", ErrForbidden
	}
	block, e := aes.NewCipher(w.key)
	if e != nil {
		return "", ErrForbidden
	}
	plain := make([]byte, len(raw))
	cipher.NewCBCDecrypter(block, w.key[:16]).CryptBlocks(plain, raw)
	pad := int(plain[len(plain)-1])
	if pad < 1 || pad > 32 || pad > len(plain) {
		return "", ErrForbidden
	}
	for _, v := range plain[len(plain)-pad:] {
		if int(v) != pad {
			return "", ErrForbidden
		}
	}
	plain = plain[:len(plain)-pad]
	if len(plain) < 20 {
		return "", ErrForbidden
	}
	size := uint64(binary.BigEndian.Uint32(plain[16:20]))
	if size > uint64(len(plain)-20) {
		return "", ErrForbidden
	}
	message := plain[20 : 20+int(size)]
	corp := plain[20+int(size):]
	if !secureEqual(string(corp), w.corp) || !utf8.Valid(message) {
		return "", ErrForbidden
	}
	return string(message), nil
}
func strictXML(body []byte, v any) error {
	d := xml.NewDecoder(bytes.NewReader(body))
	depth := 0
	for {
		tok, e := d.Token()
		if e == io.EOF {
			break
		}
		if e != nil {
			return errors.New("invalid callback XML")
		}
		switch tok.(type) {
		case xml.Directive:
			return ErrForbidden
		case xml.StartElement:
			depth++
			if depth > 32 {
				return ErrLimit
			}
		case xml.EndElement:
			depth--
		}
	}
	if e := xml.Unmarshal(body, v); e != nil {
		return errors.New("invalid callback XML")
	}
	return nil
}
func (s *Service) ParseWebhook(c Channel, method string, q url.Values, h http.Header, body []byte) (*Update, string, error) {
	if len(body) > 1<<20 {
		return nil, "", ErrLimit
	}
	if !c.Enabled {
		return nil, "", ErrForbidden
	}
	if c.Type == "wechat" {
		crypt, e := newCrypt(Str(c.Config["msg_token"]), Str(c.Config["encoding_aes_key"]), Str(c.Config["corp_id"]))
		if e != nil {
			return nil, "", e
		}
		ts, e := strconv.ParseInt(q.Get("timestamp"), 10, 64)
		if e != nil || s.Now().Sub(time.Unix(ts, 0)) > 5*time.Minute || time.Unix(ts, 0).Sub(s.Now()) > 5*time.Minute || q.Get("nonce") == "" {
			return nil, "", ErrForbidden
		}
		encrypted := q.Get("echostr")
		if method != "GET" {
			var envelope struct {
				XMLName xml.Name `xml:"xml"`
				Encrypt string   `xml:"Encrypt"`
			}
			if e = strictXML(body, &envelope); e != nil {
				return nil, "", e
			}
			encrypted = envelope.Encrypt
		}
		if encrypted == "" || !secureEqual(signed(crypt.token, q.Get("timestamp"), q.Get("nonce"), encrypted), q.Get("msg_signature")) {
			return nil, "", ErrForbidden
		}
		text, e := crypt.decrypt(encrypted)
		if e != nil {
			return nil, "", e
		}
		if method == "GET" {
			return nil, text, nil
		}
		var m struct {
			XMLName xml.Name `xml:"xml"`
			To      string   `xml:"ToUserName"`
			From    string   `xml:"FromUserName"`
			Type    string   `xml:"MsgType"`
			Content string   `xml:"Content"`
			ID      string   `xml:"MsgId"`
			Created string   `xml:"CreateTime"`
			Event   string   `xml:"Event"`
			Key     string   `xml:"EventKey"`
		}
		if e = strictXML([]byte(text), &m); e != nil {
			return nil, "", e
		}
		if m.To != "" && m.To != crypt.corp {
			return nil, "", ErrForbidden
		}
		command := m.Content
		if m.Type == "event" && strings.EqualFold(m.Event, "click") {
			command = "/" + strings.TrimPrefix(m.Key, "/")
		}
		if m.Type != "text" && !(m.Type == "event" && strings.EqualFold(m.Event, "click")) {
			return nil, "", ErrUnsupported
		}
		allowed, admin := Authorize(c, m.From)
		if !allowed {
			return nil, "", ErrForbidden
		}
		id := m.ID
		if id == "" {
			sum := sha1.Sum([]byte(text))
			id = hex.EncodeToString(sum[:])
		}
		return &Update{ID: id, Sender: m.From, ChatID: m.From, Text: command, Admin: admin}, "", nil
	}
	if c.Type != "telegram" && c.Type != "serverchan3" {
		return nil, "", ErrUnsupported
	}
	if mode := Str(c.Config["mode"]); mode != "" && mode != "webhook" {
		return nil, "", ErrUnsupported
	}
	if method != "POST" {
		return nil, "", ErrUnsupported
	}
	secret := Str(c.Config["webhook_secret"])
	header := "X-Telegram-Bot-Api-Secret-Token"
	if c.Type == "serverchan3" {
		header = "X-Sc3Bot-Webhook-Secret"
	}
	if secret != "" && !secureEqual(secret, h.Get(header)) {
		return nil, "", ErrForbidden
	}
	var v map[string]any
	d := json.NewDecoder(bytes.NewReader(body))
	d.UseNumber()
	if e := d.Decode(&v); e != nil {
		return nil, "", errors.New("invalid bot callback JSON")
	}
	var more any
	if e := d.Decode(&more); e != io.EOF {
		return nil, "", errors.New("trailing bot callback JSON")
	}
	u, err := parseBotUpdate(c, v)
	return u, "", err
}

// parseBotUpdate is deliberately private. Webhooks must pass their own method,
// mode and secret checks; only a successful provider getUpdates response may
// bypass those checks for polling. Both sources still require the same allowlist.
func parseBotUpdate(c Channel, v map[string]any) (*Update, error) {
	id := Str(v["update_id"])
	if value, err := strconv.ParseInt(id, 10, 64); err != nil || value < 0 {
		return nil, errors.New("valid callback update_id required")
	}
	m := obj(v["message"])
	cb := obj(v["callback_query"])
	text, sender, chat, callback := "", "", "", ""
	if cb != nil {
		m = obj(cb["message"])
		text = Str(cb["data"])
		sender = Str(obj(cb["from"])["id"])
		callback = Str(cb["id"])
	} else if m != nil {
		text = Str(m["text"])
		sender = Str(obj(m["from"])["id"])
	}
	chat = Str(obj(m["chat"])["id"])
	if c.Type == "serverchan3" {
		// SC3 documents a flat message.chat_id and also emits Telegram-shaped
		// chat/from objects. The SC3 UID is the chat ID in every representation.
		if flat := Str(m["chat_id"]); flat != "" {
			chat = flat
		}
		if chat == "" {
			chat = Str(obj(m["from"])["id"])
		}
		sender = chat
	}
	if text == "" || chat == "" || sender == "" || len(text) > 16384 || len([]rune(text)) > 4096 || len(chat) > 128 || len(sender) > 128 || len(callback) > 256 || !utf8.ValidString(text) {
		return nil, ErrUnsupported
	}
	allowed, admin := Authorize(c, sender)
	if !allowed {
		return nil, ErrForbidden
	}
	return &Update{ID: id, Sender: sender, ChatID: chat, Text: text, CallbackID: callback, Admin: admin}, nil
}
