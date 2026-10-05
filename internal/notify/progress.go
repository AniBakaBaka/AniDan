// SPDX-License-Identifier: AGPL-3.0-only
package notify

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// ProgressReceipt identifies one accepted Telegram text message. Persist all
// fields together, privately: the receipt is not a public status representation.
// IDs are canonical decimal strings, never floating-point JSON values. Binding
// prevents an altered receipt, channel, token or recipient from targeting another
// message. Receipts are obtained only from SendProgress's validated response.
type ProgressReceipt struct {
	MessageID string `json:"messageId"`
	ChatID    string `json:"chatId"`
	Recipient string `json:"recipient"`
	Binding   string `json:"binding"`
}

// SendProgress sends exactly one text message with explicit entities through the bounded
// notification transport. It never retries or invents a receipt after an
// uncertain delivery. The caller must not replace an uncertain send with a new
// send: the provider may already have accepted the first request.
func (s *Service) SendProgress(ctx context.Context, c Channel, m Message) (ProgressReceipt, error) {
	m = snapshotTextSpans(m)
	_, recipient, err := progressMessage(c, m)
	if err != nil {
		return ProgressReceipt{}, err
	}
	payload, err := telegramTextPayload(m, recipient, "", nil, true)
	if err != nil {
		return ProgressReceipt{}, err
	}
	ctx, err = s.PrepareOperation(ctx, c)
	if err != nil {
		return ProgressReceipt{}, err
	}
	v, err := s.request(ctx, c, "POST", s.botURL(c, "sendMessage"), payload)
	if err != nil {
		return ProgressReceipt{}, fmt.Errorf("%w: %w", ErrDeliveryUncertain, err)
	}
	if err = progressOK(v); err != nil {
		return ProgressReceipt{}, err
	}
	messageID, chatID, err := progressResponseIDs(v, recipient)
	if err != nil {
		return ProgressReceipt{}, fmt.Errorf("%w: %w", ErrDeliveryUncertain, err)
	}
	receipt := ProgressReceipt{MessageID: messageID, ChatID: chatID, Recipient: recipient}
	receipt.Binding = progressBinding(c, receipt)
	return receipt, nil
}

// EditProgress updates only the message in an authenticated receipt, using its
// resolved numeric chat ID. It never sends a replacement message, uploads an
// image or deletes a message. A changed configured recipient must start a new
// explicitly admitted lifecycle; it cannot retarget a previous receipt.
func (s *Service) EditProgress(ctx context.Context, c Channel, receipt ProgressReceipt, m Message) error {
	m = snapshotTextSpans(m)
	_, recipient, err := progressMessage(c, m)
	if err != nil {
		return err
	}
	if !validProgressReceipt(c, receipt, recipient) {
		return fmt.Errorf("%w: invalid progress receipt binding", ErrForbidden)
	}
	// message_id remains an exact JSON integer, including synthetic >2^53 IDs.
	payload, err := telegramTextPayload(m, receipt.ChatID, receipt.MessageID, nil, true)
	if err != nil {
		return err
	}
	ctx, err = s.PrepareOperation(ctx, c)
	if err != nil {
		return err
	}
	v, err := s.request(ctx, c, "POST", s.botURL(c, "editMessageText"), payload)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrDeliveryUncertain, err)
	}
	if err = progressOK(v); err != nil {
		return err
	}
	messageID, chatID, err := progressResponseIDs(v, recipient)
	if err != nil || messageID != receipt.MessageID || chatID != receipt.ChatID {
		return fmt.Errorf("%w: invalid edited progress message response", ErrDeliveryUncertain)
	}
	return nil
}

func progressMessage(c Channel, m Message) (text, recipient string, err error) {
	if c.Type != "telegram" {
		return "", "", fmt.Errorf("%w: task progress requires Telegram message editing", ErrUnsupported)
	}
	if len(m.Image) != 0 || m.PublicImageURL != "" || len(m.Buttons) != 0 || m.ImageShareable || m.ImageInteractive {
		return "", "", fmt.Errorf("%w: progress messages must be text-only", ErrUnsupported)
	}
	if err = ValidateMessageShape(c, m); err != nil {
		return "", "", err
	}
	recipient, err = progressRecipient(c.Config["chat_id"])
	if err != nil {
		return "", "", err
	}
	if m.Recipient != "" {
		requested, recipientErr := progressRecipient(m.Recipient)
		if recipientErr != nil || requested != recipient {
			return "", "", fmt.Errorf("%w: progress recipient differs from configured channel", ErrForbidden)
		}
	}
	text = joinedMessageText(m)
	if text == "" {
		return "", "", errors.New("progress message text is required")
	}
	return text, recipient, nil
}

func progressRecipient(value any) (string, error) {
	// Do not use Str on floating-point values: large stored IDs might already
	// have lost information before reaching this boundary.
	var raw string
	switch value := value.(type) {
	case string:
		raw = value
	case json.Number:
		raw = value.String()
	case int:
		raw = strconv.Itoa(value)
	case int64:
		raw = strconv.FormatInt(value, 10)
	default:
		return "", errors.New("progress chat_id must be an exact integer or username string")
	}
	if strings.HasPrefix(raw, "@") {
		if len(raw) < 2 || len(raw) > 33 {
			return "", errors.New("invalid progress chat username")
		}
		for _, ch := range raw[1:] {
			if !(ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9' || ch == '_') {
				return "", errors.New("invalid progress chat username")
			}
		}
		return strings.ToLower(raw), nil
	}
	if !canonicalProgressID(raw, false) {
		return "", errors.New("progress chat_id must be a nonzero canonical integer or username")
	}
	return raw, nil
}

func canonicalProgressID(raw string, positive bool) bool {
	if len(raw) == 0 || len(raw) > 20 {
		return false
	}
	id, err := strconv.ParseInt(raw, 10, 64)
	return err == nil && id != 0 && (!positive || id > 0) && strconv.FormatInt(id, 10) == raw
}

func progressOK(v map[string]any) error {
	ok, exists := v["ok"].(bool)
	if !exists {
		return fmt.Errorf("%w: invalid progress API response", ErrDeliveryUncertain)
	}
	if !ok {
		// Provider descriptions are not reflected: they can contain credentials,
		// recipients or message content. HTTP errors remain redacted by request.
		return botOK(v)
	}
	return nil
}

func progressResponseIDs(v map[string]any, recipient string) (string, string, error) {
	result := obj(v["result"])
	messageID, messageOK := result["message_id"].(json.Number)
	chat := obj(result["chat"])
	chatID, chatOK := chat["id"].(json.Number)
	if !messageOK || !chatOK || !canonicalProgressID(messageID.String(), true) || !canonicalProgressID(chatID.String(), false) {
		return "", "", errors.New("invalid progress message identifiers")
	}
	if strings.HasPrefix(recipient, "@") {
		username, ok := chat["username"].(string)
		resolved, err := progressRecipient("@" + username)
		if !ok || err != nil || resolved != recipient {
			return "", "", errors.New("progress response recipient binding mismatch")
		}
	} else if chatID.String() != recipient {
		return "", "", errors.New("progress response recipient binding mismatch")
	}
	return messageID.String(), chatID.String(), nil
}

func progressBinding(c Channel, receipt ProgressReceipt) string {
	mac := hmac.New(sha256.New, []byte(Str(c.Config["bot_token"])))
	_, _ = mac.Write([]byte(strings.Join([]string{"anidan.telegram.progress.v1", routeOwner(c), receipt.Recipient, receipt.ChatID, receipt.MessageID}, "\x00")))
	return hex.EncodeToString(mac.Sum(nil))
}

func validProgressReceipt(c Channel, receipt ProgressReceipt, recipient string) bool {
	if !canonicalProgressID(receipt.MessageID, true) || !canonicalProgressID(receipt.ChatID, false) || receipt.Recipient != recipient || len(receipt.Binding) != 64 {
		return false
	}
	if !strings.HasPrefix(recipient, "@") && receipt.ChatID != recipient {
		return false
	}
	return hmac.Equal([]byte(receipt.Binding), []byte(progressBinding(c, receipt)))
}
