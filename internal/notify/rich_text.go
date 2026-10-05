// SPDX-License-Identifier: AGPL-3.0-only
package notify

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/netip"
	"net/url"
	"sort"
	"strings"
	"unicode/utf8"
)

// TextSpan uses [Start, End) rune offsets into Message.Text, excluding its title.
// Only bold, code and text_link are supported; ranges must not overlap or nest.
// A text_link may point only to a validated public HTTPS origin's /task route.
type TextSpan struct {
	Kind       string
	Start, End int
	URL        string
}

const (
	maxTextSpans             = 32 // Local resource policy, not a Telegram entity limit.
	maxTextLinkBytes         = 2048
	maxTextLinksBytes        = 8192
	maxTextEntitiesBytes     = 16 << 10
	maxTelegramTextWireBytes = 64 << 10
	telegramTextVersion      = "anidan.telegram.text.entities.v1"
)

type telegramTextEntity struct {
	Type   string `json:"type"`
	Offset int    `json:"offset"`
	Length int    `json:"length"`
	URL    string `json:"url,omitempty"`
}

type telegramText struct {
	Text     string               `json:"text"`
	Entities []telegramTextEntity `json:"entities,omitempty"`
}

func joinedMessageText(m Message) string {
	if m.Title != "" {
		return m.Title + "\n" + m.Text
	}
	return m.Text
}

func messageTextByteLimit(m Message) error {
	// Check each operand before adding or joining attacker-controlled strings.
	if len(m.Title) > 16384 || len(m.Text) > 16384 {
		return ErrLimit
	}
	separator := 0
	if m.Title != "" {
		separator = 1
	}
	if len(m.Title)+len(m.Text)+separator > 16384 {
		return ErrLimit
	}
	return nil
}

func snapshotTextSpans(m Message) Message {
	// Invalid oversized metadata is rejected without allocating a second copy.
	if len(m.TextSpans) <= maxTextSpans {
		m.TextSpans = append([]TextSpan(nil), m.TextSpans...)
	}
	return m
}

// TaskTextLink performs syntax checks only. It does not resolve DNS, contact the
// destination, prove ownership, or guarantee future public address resolution.
// Callers should pass only the explicitly configured application public origin.
func TaskTextLink(origin string) (string, error) {
	if len(origin) > maxTextLinkBytes || !utf8.ValidString(origin) {
		return "", ErrLimit
	}
	u, err := url.Parse(origin)
	if err != nil || origin != strings.TrimSpace(origin) || u.Scheme != "https" || u.User != nil || u.RawPath != "" || u.Opaque != "" || strings.ContainsAny(origin, "?#\\") || (u.Port() != "" && u.Port() != "443") {
		return "", errors.New("invalid task link origin")
	}
	if _, err = PublicDomain(origin); err != nil {
		return "", errors.New("invalid task link origin")
	}
	host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	if len(host) > 253 {
		return "", ErrLimit
	}
	if ip, err := netip.ParseAddr(host); err == nil {
		if !PublicIP(ip) {
			return "", errors.New("invalid task link host")
		}
	} else {
		// Reject malformed DNS names, local-use suffixes and alternate numeric IP
		// spellings. Reserved example/test DNS names remain useful inert fixtures.
		labels := strings.Split(host, ".")
		for _, label := range labels {
			if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
				return "", errors.New("invalid task link host")
			}
			for _, r := range label {
				if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-') {
					return "", errors.New("invalid task link host")
				}
			}
		}
		last := labels[len(labels)-1]
		hexNumeric := strings.HasPrefix(last, "0x") && strings.Trim(last[2:], "0123456789abcdef") == ""
		if !strings.ContainsAny(last, "abcdefghijklmnopqrstuvwxyz") || hexNumeric || last == "internal" || last == "lan" || last == "home" || last == "onion" {
			return "", errors.New("invalid task link host")
		}
	}
	authority := host
	if strings.Contains(host, ":") {
		authority = "[" + host + "]"
	}
	return (&url.URL{Scheme: "https", Host: authority, Path: "/task"}).String(), nil
}

func canonicalTaskTextLink(raw string) (string, error) {
	if len(raw) > maxTextLinkBytes || !utf8.ValidString(raw) {
		return "", ErrLimit
	}
	u, err := url.Parse(raw)
	if err != nil || u.Path != "/task" || u.RawPath != "" || u.User != nil || u.Opaque != "" || strings.ContainsAny(raw, "?#\\") {
		return "", errors.New("invalid task text link")
	}
	return TaskTextLink(u.Scheme + "://" + u.Host)
}

func renderTelegramText(m Message) (telegramText, error) {
	if len(m.Image) > 0 || m.PublicImageURL != "" {
		return telegramText{}, errors.New("text entities do not support image captions")
	}
	if err := messageTextByteLimit(m); err != nil {
		return telegramText{}, err
	}
	text := joinedMessageText(m)
	if !utf8.ValidString(text) || len(text) > 16384 || utf8.RuneCountInString(text) > 4096 || len(m.TextSpans) > maxTextSpans {
		return telegramText{}, ErrLimit
	}
	if text == "" {
		return telegramText{}, errors.New("notification text is required")
	}
	// The 4096-rune and 16 KiB limits above are local policies. Telegram's
	// character limit is separate from its documented UTF-16 entity offsets.
	units := func(r rune) int {
		if r > 0xffff {
			return 2
		}
		return 1
	}
	out := telegramText{Text: text}
	prefix := 0
	if m.Title != "" {
		for _, r := range m.Title {
			prefix += units(r)
		}
		out.Entities = append(out.Entities, telegramTextEntity{Type: "bold", Length: prefix})
		prefix++ // Joined newline is outside the bold title.
	}
	positions := make([]int, 1, utf8.RuneCountInString(m.Text)+1)
	for _, r := range m.Text {
		positions = append(positions, positions[len(positions)-1]+units(r))
	}
	spans := append([]TextSpan(nil), m.TextSpans...)
	sort.Slice(spans, func(i, j int) bool { return spans[i].Start < spans[j].Start })
	end, urlBytes := 0, 0
	for _, span := range spans {
		if span.Start < end || span.Start < 0 || span.End <= span.Start || span.End >= len(positions) {
			return telegramText{}, errors.New("invalid or overlapping notification text range")
		}
		entity := telegramTextEntity{Type: span.Kind, Offset: prefix + positions[span.Start], Length: positions[span.End] - positions[span.Start]}
		switch span.Kind {
		case "bold", "code":
			if span.URL != "" {
				return telegramText{}, errors.New("URL requires a text_link span")
			}
		case "text_link":
			var err error
			entity.URL, err = canonicalTaskTextLink(span.URL)
			if err != nil {
				return telegramText{}, err
			}
			urlBytes += len(span.URL)
			if urlBytes > maxTextLinksBytes {
				return telegramText{}, ErrLimit
			}
		default:
			return telegramText{}, errors.New("unsupported notification text entity")
		}
		out.Entities = append(out.Entities, entity)
		end = span.End
	}
	wire, err := json.Marshal(out.Entities)
	if err != nil || len(wire) > maxTextEntitiesBytes {
		return telegramText{}, ErrLimit
	}
	return out, nil
}

// TelegramTextFingerprint identifies the canonical rendering sent by ordinary
// Telegram text/progress requests, including the rendering version. It validates
// metadata and has no I/O. Recipients and receipt bindings remain separate.
func TelegramTextFingerprint(m Message) (string, error) {
	rendered, err := renderTelegramText(m)
	if err != nil {
		return "", err
	}
	wire, err := json.Marshal(struct {
		Version string `json:"version"`
		telegramText
	}{telegramTextVersion, rendered})
	if err != nil || len(wire) > maxTelegramTextWireBytes {
		return "", ErrLimit
	}
	hash := sha256.Sum256(wire)
	return hex.EncodeToString(hash[:]), nil
}

func telegramTextPayload(m Message, recipient, messageID string, markup map[string]any, noPreview bool) (map[string]any, error) {
	rendered, err := renderTelegramText(m)
	if err != nil {
		return nil, err
	}
	payload := map[string]any{"chat_id": recipient, "text": rendered.Text}
	if len(rendered.Entities) > 0 {
		payload["entities"] = rendered.Entities
		noPreview = true
	}
	if noPreview {
		payload["link_preview_options"] = map[string]bool{"is_disabled": true}
	}
	if messageID != "" {
		payload["message_id"] = json.Number(messageID)
	}
	if markup != nil {
		payload["reply_markup"] = markup
	}
	wire, err := json.Marshal(payload)
	if err != nil || len(wire) > maxTelegramTextWireBytes {
		return nil, ErrLimit
	}
	return payload, nil
}
