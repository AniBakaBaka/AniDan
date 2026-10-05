// SPDX-License-Identifier: AGPL-3.0-only
package server

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/url"
	"regexp"
	"strings"
	"unicode/utf8"
)

const (
	webhookLogMaxBytes      = 8 << 10
	webhookLogMaxDepth      = 8
	webhookLogMaxNodes      = 256
	webhookLogMaxCollection = 64
	webhookLogMaxString     = 512
	webhookLogRedacted      = "[REDACTED]"
)

type webhookLogCapture struct {
	Payload        string
	RedactedFields int
	Truncated      bool
}
type webhookLogSanitizer struct {
	nodes, redacted int
	truncated       bool
}

var webhookLogURL = regexp.MustCompile(`[A-Za-z][A-Za-z0-9+.-]*://[^\s<>"']+`)
var webhookLogCredentialText = regexp.MustCompile(`(?i)(?:\b(?:bearer|basic)\s+\S+|\b[A-Za-z0-9_-]*(?:password|passwd|secret|token|api[_-]?key|authorization|cookie|credential)[A-Za-z0-9_-]*[\\"']*\s*[:=])`)

func webhookLogSensitiveKey(key string) bool {
	if len(key) > 128 {
		return true
	}
	var b strings.Builder
	for _, ch := range strings.ToLower(key) {
		if ch >= 'a' && ch <= 'z' || ch >= '0' && ch <= '9' {
			b.WriteRune(ch)
		}
	}
	normalized := b.String()
	if normalized == "key" || normalized == "pwd" || normalized == "auth" || normalized == "headers" || normalized == "query" || normalized == "querystring" || normalized == "sig" || normalized == "sas" || normalized == "assertion" {
		return true
	}
	for _, part := range []string{"password", "passwd", "secret", "token", "apikey", "authorization", "credential", "cookie", "session", "accesskey", "privatekey", "signingkey", "encryptionkey", "decryptionkey", "appkey", "signature", "authentication", "auth", "header", "query"} {
		if strings.Contains(normalized, part) {
			return true
		}
	}
	return false
}
func webhookLogPrefix(value string, limit int) (string, bool) {
	if len(value) <= limit {
		return value, false
	}
	value = value[:limit]
	for !utf8.ValidString(value) && len(value) > 0 {
		value = value[:len(value)-1]
	}
	return value, true
}
func (s *webhookLogSanitizer) text(value string) string {
	value, truncated := webhookLogPrefix(value, webhookLogMaxString)
	s.truncated = s.truncated || truncated
	// A truncated URL could hide its @ delimiter beyond the retained prefix,
	// making credential userinfo look like an ordinary host/port. Omit it whole.
	if truncated && strings.Contains(value, "://") {
		s.redacted++
		return "[REDACTED TRUNCATED URL]"
	}
	if webhookLogCredentialText.MatchString(value) {
		s.redacted++
		return webhookLogRedacted
	}
	value = webhookLogURL.ReplaceAllStringFunc(value, func(raw string) string {
		parsed, err := url.Parse(raw)
		if err != nil {
			s.redacted++
			return webhookLogRedacted
		}
		changed := false
		if parsed.User != nil {
			parsed.User = nil
			changed = true
		}
		if parsed.RawQuery != "" {
			query, err := url.ParseQuery(parsed.RawQuery)
			if err != nil {
				parsed.RawQuery = "redacted"
				changed = true
			} else {
				for key := range query {
					if webhookLogSensitiveKey(key) {
						query.Set(key, webhookLogRedacted)
						changed = true
					}
				}
				if changed {
					parsed.RawQuery = query.Encode()
				}
			}
		}
		if parsed.Fragment != "" {
			parsed.Fragment = ""
			changed = true
		}
		if changed {
			s.redacted++
		}
		return parsed.String()
	})
	if truncated {
		value += "[TRUNCATED]"
	}
	return value
}
func (s *webhookLogSanitizer) walk(value any, depth int) any {
	if depth > webhookLogMaxDepth || s.nodes >= webhookLogMaxNodes {
		s.truncated = true
		return "[TRUNCATED]"
	}
	s.nodes++
	switch v := value.(type) {
	case map[string]any:
		out := map[string]any{}
		seen := 0
		for key, child := range v {
			if seen >= webhookLogMaxCollection || s.nodes >= webhookLogMaxNodes {
				s.truncated = true
				out["_truncated"] = true
				break
			}
			seen++
			displayKey := key
			if len(key) > 128 {
				displayKey = "[LONG_KEY]"
				s.truncated = true
			} else {
				displayKey = s.text(key)
			}
			if webhookLogSensitiveKey(key) {
				out[displayKey] = webhookLogRedacted
				s.redacted++
				s.nodes++
				continue
			}
			out[displayKey] = s.walk(child, depth+1)
		}
		return out
	case []any:
		length := min(len(v), webhookLogMaxCollection)
		out := make([]any, 0, length+1)
		for i := 0; i < length; i++ {
			if s.nodes >= webhookLogMaxNodes {
				s.truncated = true
				break
			}
			out = append(out, s.walk(v[i], depth+1))
		}
		if len(out) < len(v) {
			s.truncated = true
			out = append(out, "[TRUNCATED]")
		}
		return out
	case string:
		return s.text(v)
	case bool, nil, float64, json.Number:
		return v
	default:
		s.truncated = true
		return "[UNSUPPORTED VALUE]"
	}
}
func sanitizeWebhookLog(payload map[string]any) webhookLogCapture {
	sanitizer := &webhookLogSanitizer{}
	safe := sanitizer.walk(payload, 0)
	encoded, err := json.Marshal(safe)
	if err != nil {
		encoded = []byte(`{"_truncated":true,"_reason":"payload could not be represented safely"}`)
		sanitizer.truncated = true
	}
	if len(encoded) > webhookLogMaxBytes {
		sanitizer.truncated = true
		// The preview is derived only from the already-sanitized representation.
		// Bound the *final encoded bytes*, including quoting/escaping overhead.
		preview, _ := webhookLogPrefix(string(encoded), (webhookLogMaxBytes-256)/2)
		for {
			encoded, _ = json.Marshal(map[string]any{"_truncated": true, "_reason": "sanitized payload exceeded byte limit", "preview": preview})
			if len(encoded) <= webhookLogMaxBytes {
				break
			}
			preview, _ = webhookLogPrefix(preview, len(preview)/2)
		}
	}
	return webhookLogCapture{string(encoded), sanitizer.redacted, sanitizer.truncated}
}
func writeWebhookPayloadLog(ctx context.Context, logger *slog.Logger, source string, payload map[string]any) {
	capture := sanitizeWebhookLog(payload)
	// No request object, headers, URL/query parameters, raw body, or output path is
	// supplied to the logger. The existing application's logging destination owns it.
	logger.InfoContext(ctx, "Webhook payload captured (sanitized)", "source", source, "payload", capture.Payload, "payloadFormat", "sanitized-json", "payloadBytes", len(capture.Payload), "redactedFields", capture.RedactedFields, "truncated", capture.Truncated)
}
