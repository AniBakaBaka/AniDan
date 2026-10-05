// SPDX-License-Identifier: AGPL-3.0-only
// Package safelog provides deliberately lossy, bounded diagnostic response logs.
// It never receives request URLs, headers, credentials, or raw error messages.
package safelog

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
)

const (
	MaxInputBytes  = 64 << 10
	MaxOutputBytes = 8 << 10
	maxDepth       = 8
	maxNodes       = 256
	maxCollection  = 64
)

// Capture is a representation, never a promise of literal raw response bytes.
type Capture struct {
	Payload        string
	Format         string
	OmittedReason  string
	RedactedFields int
	Truncated      bool
}

var errBudget = errors.New("response representation budget exceeded")

// Only fixed, common protocol field names can reach logs. Unknown names may
// themselves contain secrets. All string values are omitted, even in public-
// looking fields: an upstream can echo a credential under an innocent name.
func visibleKey(key string) bool {
	switch key {
	case "data", "result", "results", "items", "list", "comments", "episodes", "seasons", "subjects", "code", "status", "count", "total", "total_count", "total_results", "total_pages", "page", "pages", "limit", "offset", "id", "title", "name", "error", "errors", "message", "url", "success", "type", "size", "index", "duration", "has_more", "hasMore":
		return true
	}
	return false
}

type parser struct {
	dec             *json.Decoder
	nodes, redacted int
}

func (p *parser) value(depth int) (any, error) {
	if depth > maxDepth || p.nodes >= maxNodes {
		return nil, errBudget
	}
	p.nodes++
	token, err := p.dec.Token()
	if err != nil {
		return nil, err
	}
	switch v := token.(type) {
	case json.Delim:
		switch v {
		case '{':
			out := map[string]any{}
			omitted, seen := 0, 0
			for p.dec.More() {
				if seen >= maxCollection || p.nodes >= maxNodes {
					return nil, errBudget
				}
				seen++
				p.nodes++ // Object keys consume the same bounded node budget.
				key, err := p.dec.Token()
				if err != nil {
					return nil, err
				}
				name, ok := key.(string)
				if !ok {
					return nil, errors.New("invalid object key")
				}
				child, err := p.value(depth + 1)
				if err != nil {
					return nil, err
				}
				if visibleKey(name) {
					out[name] = child
				} else {
					omitted++
					p.redacted++
				}
			}
			if _, err := p.dec.Token(); err != nil {
				return nil, err
			}
			if omitted > 0 {
				out["_omittedFields"] = omitted
			}
			return out, nil
		case '[':
			out := []any{}
			for p.dec.More() {
				if len(out) >= maxCollection {
					return nil, errBudget
				}
				child, err := p.value(depth + 1)
				if err != nil {
					return nil, err
				}
				out = append(out, child)
			}
			if _, err := p.dec.Token(); err != nil {
				return nil, err
			}
			return out, nil
		}
	case string:
		p.redacted++
		return "[STRING OMITTED]", nil
	case json.Number:
		if len(v) > 32 {
			p.redacted++
			return "[NUMBER OMITTED]", nil
		}
		return v, nil
	case bool, nil:
		return v, nil
	}
	return nil, errors.New("invalid JSON token")
}

func omitted(reason string, truncated bool) Capture {
	return Capture{Format: "omitted", OmittedReason: reason, Truncated: truncated}
}

// Sanitize does not inspect incomplete bodies. Invalid JSON and non-JSON formats
// (including HTML, XML, protobuf, JSONP and plain text) are omitted entirely.
func Sanitize(body []byte, complete bool) Capture {
	if !complete {
		return omitted("incomplete-or-unread-body", true)
	}
	if len(body) > MaxInputBytes {
		return omitted("input-byte-limit", true)
	}
	if len(body) == 0 {
		return omitted("empty-body", false)
	}
	p := parser{dec: json.NewDecoder(bytes.NewReader(body))}
	p.dec.UseNumber()
	value, err := p.value(0)
	if errors.Is(err, errBudget) {
		return omitted("json-complexity-limit", true)
	}
	if err != nil {
		return omitted("non-json-or-invalid-json", false)
	}
	if _, err = p.dec.Token(); err != io.EOF {
		return omitted("non-json-or-invalid-json", false)
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return omitted("unrepresentable-json", false)
	}
	if len(encoded) > MaxOutputBytes {
		return omitted("output-byte-limit", true)
	}
	return Capture{Payload: string(encoded), Format: "sanitized-json-structure", RedactedFields: p.redacted}
}

// Response writes to the application's configured slog destination (app.log in
// the normal executable). Call only after the caller's cheap opt-in check.
// category, provider and outcome must be fixed implementation identifiers.
func Response(ctx context.Context, category, provider string, status int, outcome string, body []byte, complete bool) {
	logger := slog.Default()
	if !logger.Enabled(ctx, slog.LevelInfo) {
		return
	}
	// Keep this boundary safe even if a future caller supplies a configured
	// provider name or a raw error in place of these fixed identifiers.
	switch category {
	case "source", "metadata":
	default:
		category = "other"
	}
	switch provider {
	case "bilibili", "dandanplay", "gamer", "hanjutv", "sohu", "le", "mgtv", "iqiyi", "tencent", "youku", "renren", "ezdmw", "girigirilove", "hongguo", "mddcloud", "migu", "xigua", "bangumi", "tmdb", "tvdb", "imdb", "douban", "trakt", "anibt", "360":
	default:
		provider = "other"
	}
	switch outcome {
	case "network-error", "http-error", "response-limit", "read-error", "received":
	default:
		outcome = "other"
	}
	capture := Sanitize(body, complete)
	logger.InfoContext(ctx, "Upstream response captured (sanitized or omitted)",
		"category", category, "provider", provider, "status", status, "outcome", outcome,
		"payload", capture.Payload, "payloadFormat", capture.Format, "payloadBytes", len(capture.Payload),
		"observedBytes", len(body), "redactedFields", capture.RedactedFields,
		"truncated", capture.Truncated, "omittedReason", capture.OmittedReason)
}
