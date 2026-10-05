// SPDX-License-Identifier: AGPL-3.0-only
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"mime/multipart"
	"net/textproto"
	"net/url"
	"unicode/utf8"
)

const MaxImageBytes = 5 << 20
const MaxImagePixels = 4 << 20

func buttonMarkup(buttons [][]Button) (map[string]any, error) {
	if len(buttons) == 0 {
		return nil, nil
	}
	if len(buttons) > 10 {
		return nil, ErrLimit
	}
	for _, row := range buttons {
		if len(row) == 0 || len(row) > 8 {
			return nil, ErrLimit
		}
		for _, b := range row {
			if b.Data == "" || b.Text == "" || len(b.Data) > 64 || len(b.Text) > 128 || !utf8.ValidString(b.Text) || !utf8.ValidString(b.Data) {
				return nil, ErrLimit
			}
		}
	}
	// Callback keys are opaque server-issued values. Never rewrite or store them.
	return map[string]any{"inline_keyboard": buttons}, nil
}

func (s *Service) imageFormat(ctx context.Context, data []byte, limit int) (string, error) {
	if len(data) == 0 || len(data) > limit {
		return "", ErrLimit
	}
	select {
	case s.imageGate <- struct{}{}:
		defer func() { <-s.imageGate }()
	case <-ctx.Done():
		return "", ctx.Err()
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	cfg, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || (format != "png" && format != "jpeg") {
		return "", errors.New("notification image must be valid PNG or JPEG bytes")
	}
	if cfg.Width <= 0 || cfg.Height <= 0 || cfg.Width > 4096 || cfg.Height > 4096 || int64(cfg.Width)*int64(cfg.Height) > MaxImagePixels {
		return "", ErrLimit
	}
	// Decode only after checking dimensions, and with independent bounded slots.
	// This rejects truncated/corrupt input without risking unbounded pixel memory.
	if _, actual, err := image.Decode(bytes.NewReader(data)); err != nil || actual != format {
		return "", errors.New("notification image must be valid PNG or JPEG bytes")
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return format, nil
}

func (s *Service) imageRequest(ctx context.Context, c Channel, endpoint, field, format string, data []byte, fields map[string]string) (map[string]any, error) {
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	for key, value := range fields {
		if err := w.WriteField(key, value); err != nil {
			return nil, errors.New("invalid notification multipart field")
		}
	}
	header := textproto.MIMEHeader{}
	header.Set("Content-Disposition", fmt.Sprintf(`form-data; name="%s"; filename="image.%s"`, field, format))
	header.Set("Content-Type", "image/"+format)
	part, err := w.CreatePart(header)
	if err != nil {
		return nil, errors.New("invalid notification multipart image")
	}
	if _, err = part.Write(data); err != nil {
		return nil, errors.New("invalid notification multipart image")
	}
	if err = w.Close(); err != nil {
		return nil, errors.New("invalid notification multipart image")
	}
	if body.Len() > MaxImageBytes+(64<<10) {
		return nil, ErrLimit
	}
	return s.requestBody(ctx, c, "POST", endpoint, body.Bytes(), w.FormDataContentType(), s.Client, s.gate)
}

func (s *Service) sendTelegramImage(ctx context.Context, c Channel, recipient, caption string, markup map[string]any, data []byte) error {
	format, err := s.imageFormat(ctx, data, MaxImageBytes)
	if err != nil {
		return err
	}
	fields := map[string]string{"chat_id": recipient, "caption": caption}
	if markup != nil {
		b, err := json.Marshal(markup)
		if err != nil {
			return errors.New("invalid notification image buttons")
		}
		fields["reply_markup"] = string(b)
	}
	v, err := s.imageRequest(ctx, c, s.botURL(c, "sendPhoto"), "photo", format, data, fields)
	if err != nil {
		return err
	}
	return botOK(v)
}

func (s *Service) sendWeImage(ctx context.Context, c Channel, recipient string, data []byte) error {
	format, err := s.imageFormat(ctx, data, 2<<20)
	if err != nil {
		return err
	}
	token, err := s.weToken(ctx, c, false)
	if err != nil {
		return err
	}
	mediaID := ""
	for attempt := 0; attempt < 2; attempt++ {
		endpoint := "https://qyapi.weixin.qq.com/cgi-bin/media/upload?" + url.Values{"access_token": {token}, "type": {"image"}}.Encode()
		v, err := s.imageRequest(ctx, c, endpoint, "media", format, data, nil)
		if err != nil {
			return err
		}
		if attempt == 0 && num(v["errcode"]) == 42001 {
			token, err = s.weToken(ctx, c, true)
			if err != nil {
				return err
			}
			continue
		}
		// The successful media/upload response omits errcode in WeCom's API.
		if v["errcode"] != nil && !zeroCode(v["errcode"]) {
			return fmt.Errorf("WeCom image upload rejected (code %s)", responseCode(v["errcode"]))
		}
		mediaID = Str(v["media_id"])
		if mediaID == "" || len(mediaID) > 512 {
			return errors.New("WeCom image upload returned invalid media ID")
		}
		break
	}
	if mediaID == "" {
		return errors.New("WeCom image upload failed")
	}
	return s.weSend(ctx, c, map[string]any{"touser": recipient, "msgtype": "image", "agentid": num(c.Config["agent_id"]), "image": map[string]string{"media_id": mediaID}})
}

func (s *Service) weSend(ctx context.Context, c Channel, payload map[string]any) error {
	token, err := s.weToken(ctx, c, false)
	if err != nil {
		return err
	}
	for attempt := 0; attempt < 2; attempt++ {
		v, err := s.request(ctx, c, "POST", "https://qyapi.weixin.qq.com/cgi-bin/message/send?"+url.Values{"access_token": {token}}.Encode(), payload)
		if err != nil {
			return err
		}
		if v["errcode"] != nil && zeroCode(v["errcode"]) {
			if Str(v["invaliduser"]) != "" || Str(v["invalidparty"]) != "" || Str(v["invalidtag"]) != "" {
				return errors.New("WeCom rejected one or more recipients")
			}
			return nil
		}
		if attempt == 0 && num(v["errcode"]) == 42001 {
			token, err = s.weToken(ctx, c, true)
			if err != nil {
				return err
			}
			continue
		}
		return fmt.Errorf("WeCom message rejected (code %s)", responseCode(v["errcode"]))
	}
	return errors.New("WeCom message failed")
}
