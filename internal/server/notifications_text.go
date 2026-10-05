// SPDX-License-Identifier: AGPL-3.0-only
package server

import (
	"context"
	"strings"
	"unicode/utf8"

	"github.com/AniBakaBaka/AniDan/internal/notify"
)

// notificationTextBuilder styles only ranges appended by the server. Literal
// source values are never parsed as markup or scanned for links.
type notificationTextBuilder struct {
	text  strings.Builder
	runes int
	spans []notify.TextSpan
}

func (b *notificationTextBuilder) append(text, kind, target string) {
	start := b.runes
	b.text.WriteString(text)
	b.runes += utf8.RuneCountInString(text)
	if kind != "" && start < b.runes {
		b.spans = append(b.spans, notify.TextSpan{Kind: kind, Start: start, End: b.runes, URL: target})
	}
}

// Add navigation only to a selected Telegram text part, after aggregation and
// clipping have produced its final body. The configured application origin is
// the sole authority; transport endpoints and source image URLs are unrelated.
// Optional navigation never makes an otherwise valid notice too large to send.
func (s *Server) notificationText(ctx context.Context, c notify.Channel, m notify.Message) notify.Message {
	if c.Type != "telegram" || m.Title == "" || len(m.Image) != 0 || m.PublicImageURL != "" {
		return m
	}
	// Do not copy an already-invalid body or metadata while considering an
	// optional addition. The eventual Send still reports the original error.
	if notify.ValidateMessageShape(c, m) != nil {
		return m
	}
	target, err := notify.TaskTextLink(s.setting(ctx, "custom_api_domain", ""))
	if err != nil {
		return m
	}
	b := notificationTextBuilder{spans: append([]notify.TextSpan(nil), m.TextSpans...)}
	b.append(m.Text, "", "")
	if m.Text != "" {
		b.append("\n", "", "")
	}
	b.append("任务列表（需登录）：", "bold", "")
	b.append(target, "text_link", target)
	candidate := m
	candidate.Text = b.text.String()
	candidate.TextSpans = b.spans
	if notify.ValidateMessageShape(c, candidate) != nil {
		return m
	}
	return candidate
}
