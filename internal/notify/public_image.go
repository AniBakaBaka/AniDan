// SPDX-License-Identifier: AGPL-3.0-only
package notify

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

var publicThumbnailPath = regexp.MustCompile(`^/data/images/notification-public/[a-f0-9]{64}\.jpg$`)

// PrepareOperation validates and snapshots routing before a caller performs
// local rendering/publication. It performs no network request. Nested sends
// preserve this snapshot, including multi-part delivery.
func (s *Service) PrepareOperation(ctx context.Context, c Channel) (context.Context, error) {
	if err := Validate(c); err != nil {
		return ctx, err
	}
	if !c.Enabled {
		return ctx, errors.New("notification channel is disabled")
	}
	return s.withRoute(ctx, c)
}

// ValidatePublicImageURL permits only a deliberately published native thumbnail,
// never arbitrary source URLs, private addresses, credentials or login queries.
func ValidatePublicImageURL(raw string) error {
	if len(raw) > 2048 {
		return ErrLimit
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawPath != "" || !publicThumbnailPath.MatchString(u.Path) {
		return errors.New("invalid public notification thumbnail URL")
	}
	if _, err = PublicDomain("https://" + u.Host); err != nil {
		return errors.New("invalid public notification thumbnail origin")
	}
	return nil
}

// ValidateMessageShape preflights the whole message before a multi-part send can
// accept its first part. Pixel decoding remains bounded by Service.imageFormat.
func ValidateMessageShape(c Channel, m Message) error {
	if err := Validate(c); err != nil {
		return err
	}
	if !c.Enabled {
		return errors.New("notification channel is disabled")
	}
	if err := messageTextByteLimit(m); err != nil {
		return err
	}
	text := joinedMessageText(m)
	if !utf8.ValidString(text) || len(text) > 16384 {
		return ErrLimit
	}
	if len(m.Buttons) > 0 && c.Type != "telegram" {
		return fmt.Errorf("%w: inline buttons", ErrUnsupported)
	}
	markup, err := buttonMarkup(m.Buttons)
	if err != nil {
		return err
	}
	if len(m.Image) > 0 && m.PublicImageURL != "" {
		return errors.New("notification image bytes and public URL are mutually exclusive")
	}
	if m.PublicImageURL != "" {
		if !m.ImageShareable {
			return errors.New("private notification images cannot use public URLs")
		}
		if err := ValidatePublicImageURL(m.PublicImageURL); err != nil {
			return err
		}
	}
	hasImage := len(m.Image) > 0 || m.PublicImageURL != ""
	if hasImage && len(m.TextSpans) > 0 {
		return errors.New("text entities do not support image captions")
	}
	if !hasImage && len(m.TextSpans) > 0 {
		if _, err := renderTelegramText(m); err != nil {
			return err
		}
	}
	if hasImage && c.Type == "serverchan3" {
		return fmt.Errorf("%w: ServerChan3 images", ErrUnsupported)
	}
	recipient := m.Recipient
	if recipient == "" {
		key := "chat_id"
		if c.Type == "wechat" {
			key = "to_user"
		}
		recipient = Str(c.Config[key])
	}
	if recipient == "" || !validRecipient(recipient) {
		return errors.New("valid explicit notification recipient is required")
	}
	if c.Type == "wechat" {
		if len(text) > 2048 || len(m.Image) > 2<<20 {
			return ErrLimit
		}
	} else {
		maxRunes := 4096
		if hasImage {
			maxRunes = 1024
		}
		if len([]rune(text)) > maxRunes || len(m.Image) > MaxImageBytes {
			return ErrLimit
		}
		if c.Type == "serverchan3" {
			if _, err := strconv.ParseInt(recipient, 10, 64); err != nil {
				return errors.New("ServerChan3 chat_id must be numeric")
			}
		}
	}
	if c.Type == "telegram" && !hasImage {
		// Include buttons and recipient in the wire budget before publication or
		// the first part of a series. Progress IDs add only bounded decimal bytes.
		if _, err := telegramTextPayload(m, recipient, "", markup, false); err != nil {
			return err
		}
	}
	return nil
}

// SendSeries performs at most two preflighted deliveries. It never retries a
// failed or ambiguous part and identifies partial acceptance to the caller.
func (s *Service) SendSeries(ctx context.Context, c Channel, messages []Message) error {
	if len(messages) < 1 || len(messages) > 2 {
		return ErrLimit
	}
	snapshot := make([]Message, len(messages))
	for i, m := range messages {
		snapshot[i] = snapshotTextSpans(m)
	}
	messages = snapshot
	for _, m := range messages {
		if err := ValidateMessageShape(c, m); err != nil {
			return err
		}
		if len(m.Image) > 0 {
			limit := MaxImageBytes
			if c.Type == "wechat" {
				limit = 2 << 20
			}
			if _, err := s.imageFormat(ctx, m.Image, limit); err != nil {
				return err
			}
		}
	}
	// withRoute snapshots routing once; nested Send calls preserve it.
	var err error
	ctx, err = s.withRoute(ctx, c)
	if err != nil {
		return err
	}
	for i, m := range messages {
		if err = ctx.Err(); err == nil {
			err = s.Send(ctx, c, m)
		}
		if err != nil {
			if i > 0 {
				return fmt.Errorf("%w: first part accepted; following part failed: %w", ErrPartialDelivery, err)
			}
			return err
		}
	}
	return nil
}

func (s *Service) sendPublicImage(ctx context.Context, c Channel, m Message, text, recipient string, markup map[string]any) error {
	if err := ValidateMessageShape(c, m); err != nil {
		return err
	}
	switch c.Type {
	case "telegram":
		payload := map[string]any{"chat_id": recipient, "photo": m.PublicImageURL, "caption": text}
		if markup != nil {
			payload["reply_markup"] = markup
		}
		v, err := s.request(ctx, c, "POST", s.botURL(c, "sendPhoto"), payload)
		if err != nil {
			return err
		}
		return botOK(v)
	case "wechat":
		title := strings.TrimSpace(m.Title)
		if title == "" {
			title = "AniDan notification"
		}
		if len([]rune(title)) > 128 {
			title = string([]rune(title)[:128])
		}
		description := text
		if len([]rune(description)) > 512 {
			description = string([]rune(description)[:512])
		}
		article := map[string]string{"title": title, "description": description, "url": m.PublicImageURL, "picurl": m.PublicImageURL}
		return s.weSend(ctx, c, map[string]any{"touser": recipient, "msgtype": "news", "agentid": num(c.Config["agent_id"]), "news": map[string]any{"articles": []map[string]string{article}}})
	default:
		return fmt.Errorf("%w: public image URL", ErrUnsupported)
	}
}
