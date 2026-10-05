// SPDX-License-Identifier: AGPL-3.0-only
package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/AniBakaBaka/AniDan/internal/job"
)

const notificationFallbackProgressRawLimit = 64 << 10
const notificationFallbackProgressTextLimit = 1800
const notificationFallbackProgressJSONDepth = 32
const notificationFallbackProgressJSONTokens = 8192

// notificationFallbackProgressSummary is an optional, plain-text addition to
// the coordinator's operation/status/progress/task-ID message. Decode only the
// selected presentation fields: task descriptions, errors, cache keys, tokens,
// match records, provider result IDs, image URLs and buttons never enter it.
func notificationFallbackProgressSummary(e job.LifecycleEvent) string {
	label := ""
	switch e.Task.Kind {
	case "player_search":
		label = "后备搜索"
	case "player_match":
		label = "后备匹配"
	default:
		return ""
	}
	if e.Cancelled {
		return label + "已取消。"
	}
	if e.Task.Status == job.Failed {
		return label + "失败，结果不可用。"
	}
	query := notificationFallbackProgressQuery(e.Task)
	if e.Phase == job.LifecycleStarted || e.Phase == job.LifecycleProgress {
		return "查询：" + query
	}
	unavailable := label + "结果暂不可用。"
	if e.Phase != job.LifecycleFinished || e.Task.Status != job.Completed {
		return unavailable
	}

	var summary string
	if e.Task.Kind == "player_search" {
		// This is the native runPlayerSearch envelope, not the raw search/cache
		// response. Pointer fields distinguish missing/null data from zero results.
		var result struct {
			Count  *int64 `json:"count"`
			Notice *struct {
				Success *bool  `json:"success"`
				Total   *int64 `json:"total"`
				Results []struct {
					Provider string `json:"provider"`
					Title    string `json:"title"`
				} `json:"results"`
			} `json:"notification"`
		}
		if !notificationFallbackProgressDecode(e.Task.Result, &result) || result.Count == nil || *result.Count < 0 || result.Notice == nil || result.Notice.Success == nil {
			return unavailable
		}
		if !*result.Notice.Success {
			return label + "失败，结果不可用。"
		}
		if result.Notice.Total == nil || *result.Notice.Total != *result.Count || int64(len(result.Notice.Results)) > *result.Count {
			return unavailable
		}
		count := fmt.Sprint(*result.Count)
		if *result.Count > 100000 {
			count = "100000+"
		}
		summary = "查询：" + query + "\n搜索结果：" + count
		shown := 0
		for _, row := range result.Notice.Results {
			title := notificationFallbackProgressTitle(row.Title, 240)
			if title == "" {
				continue
			}
			provider := notificationFallbackProgressTitle(row.Provider, 48)
			if provider == "" {
				provider = "来源"
			}
			shown++
			summary += fmt.Sprintf("\n%d. [%s] %s", shown, provider, title)
			if shown == 5 {
				break
			}
		}
	} else {
		var result struct {
			Matched   *bool           `json:"isMatched"`
			Success   json.RawMessage `json:"success"`
			ErrorCode json.RawMessage `json:"errorCode"`
		}
		if !notificationFallbackProgressDecode(e.Task.Result, &result) {
			return unavailable
		}
		// isMatched is the outcome. Optional native protocol fields may veto it,
		// but false/missing/malformed data must never become a successful match.
		if len(result.Success) != 0 {
			var success *bool
			if json.Unmarshal(result.Success, &success) != nil || success == nil {
				return unavailable
			}
			if !*success {
				return label + "失败，结果不可用。"
			}
		}
		if len(result.ErrorCode) != 0 {
			var code *int64
			if json.Unmarshal(result.ErrorCode, &code) != nil || code == nil {
				return unavailable
			}
			if *code != 0 {
				return label + "失败，结果不可用。"
			}
		}
		if result.Matched == nil {
			return unavailable
		}
		outcome := "未匹配"
		if *result.Matched {
			outcome = "已匹配"
		}
		summary = "查询：" + query + "\n匹配结果：" + outcome
	}
	return notificationClipBytes(summary, notificationFallbackProgressTextLimit)
}

func notificationFallbackProgressDecode(raw json.RawMessage, target any) bool {
	if len(raw) == 0 || len(raw) > notificationFallbackProgressRawLimit || !utf8.Valid(raw) {
		return false
	}
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || raw[0] != '{' {
		return false
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	// The validation pass inspects structure only. Opaque provider identifiers
	// and other numeric literals must not be evaluated or rounded to float64.
	decoder.UseNumber()
	check := notificationFallbackProgressJSONCheck{decoder: decoder, remaining: notificationFallbackProgressJSONTokens}
	if !check.value(0) {
		return false
	}
	if _, err := decoder.Token(); err != io.EOF {
		return false
	}
	return json.Unmarshal(raw, target) == nil
}

type notificationFallbackProgressJSONCheck struct {
	decoder   *json.Decoder
	remaining int
}

func (c *notificationFallbackProgressJSONCheck) token() (json.Token, bool) {
	if c.remaining == 0 {
		return nil, false
	}
	c.remaining--
	token, err := c.decoder.Token()
	return token, err == nil
}

func (c *notificationFallbackProgressJSONCheck) value(depth int) bool {
	token, ok := c.token()
	if !ok {
		return false
	}
	delim, container := token.(json.Delim)
	if !container {
		return true
	}
	if depth >= notificationFallbackProgressJSONDepth {
		return false
	}
	switch delim {
	case '{':
		seen := map[string]bool{}
		for c.decoder.More() {
			keyToken, ok := c.token()
			key, isString := keyToken.(string)
			if !ok || !isString {
				return false
			}
			// encoding/json accepts case aliases for struct fields. Treat every
			// Unicode simple-fold equivalent key as one key, including escaped
			// spellings, so an alias cannot overwrite an earlier false outcome.
			folded := strings.Map(func(r rune) rune {
				minimum := r
				for next := unicode.SimpleFold(r); next != r; next = unicode.SimpleFold(next) {
					minimum = min(minimum, next)
				}
				return minimum
			}, key)
			if seen[folded] {
				return false
			}
			seen[folded] = true
			if !c.value(depth + 1) {
				return false
			}
		}
		close, ok := c.token()
		return ok && close == json.Delim('}')
	case '[':
		for c.decoder.More() {
			if !c.value(depth + 1) {
				return false
			}
		}
		close, ok := c.token()
		return ok && close == json.Delim(']')
	default:
		return false
	}
}

func notificationFallbackProgressQuery(t job.Task) string {
	var params struct {
		Keyword string `json:"keyword"`
		Parsed  struct {
			Title string `json:"title"`
		} `json:"parsed"`
	}
	if !notificationFallbackProgressDecode(t.Params, &params) {
		return notificationKeyword("")
	}
	value := params.Keyword
	if t.Kind == "player_match" {
		value = params.Parsed.Title
	}
	return notificationKeyword(notificationFallbackProgressTitle(value, 240))
}

func notificationFallbackProgressTitle(value string, maxBytes int) string {
	// The shared helper strips controls and rejects absolute paths/schemed URLs.
	// This stricter presentation boundary also rejects relative paths, filenames,
	// bare hosts, markup, URI/credential separators and invisible format controls.
	// Reject the whole field before clipping; an unsafe suffix must not disappear
	// beyond the display limit and leave a misleading apparently safe prefix.
	if value == "" || !utf8.ValidString(value) {
		return ""
	}
	runes := []rune(value)
	for i, r := range runes {
		if unicode.IsControl(r) || unicode.In(r, unicode.Cf, unicode.Zl, unicode.Zp) || r == utf8.RuneError {
			return ""
		}
		if !unicode.IsLetter(r) && !unicode.IsNumber(r) && !unicode.IsMark(r) && !unicode.IsSpace(r) && !strings.ContainsRune(" -_.,!?、，。！？：；;()（）[]【】'’\"“”–—·・…+&#~", r) {
			return ""
		}
		if (r == '.' || r == '。' || r == '．') && i+1 < len(runes) && (unicode.IsLetter(runes[i+1]) || unicode.IsNumber(runes[i+1])) {
			return ""
		}
	}
	value = strings.Join(strings.Fields(value), " ")
	if strings.HasPrefix(value, ".") || strings.HasPrefix(value, "~") {
		return ""
	}
	return notificationClipBytes(notificationMediaTitle(value), maxBytes)
}
