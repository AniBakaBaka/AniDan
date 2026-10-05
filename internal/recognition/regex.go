// SPDX-License-Identifier: AGPL-3.0-only
package recognition

import (
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/dlclark/regexp2"
)

const regexTimeout = 50 * time.Millisecond

// Regex keeps native RE2 behavior for existing patterns. The bounded fallback
// supports legacy lookaround without dropping or weakening exclusion rules.
type Regex struct {
	linear *regexp.Regexp
	legacy *regexp2.Regexp
}

func CompileRegex(pattern string) (*Regex, error) { return CompileRegexCase(pattern, true) }

func CompileRegexCase(pattern string, ignoreCase bool) (*Regex, error) {
	if len(pattern) > 16384 {
		return nil, errors.New("pattern exceeds 16 KiB")
	}
	if ignoreCase {
		pattern = "(?i)" + pattern
	}
	if re, err := regexp.Compile(pattern); err == nil {
		return &Regex{linear: re}, nil
	}
	re, err := regexp2.Compile(pattern, regexp2.RE2)
	if err != nil {
		return nil, fmt.Errorf("invalid or unsupported compatibility regex: %w", err)
	}
	re.MatchTimeout = regexTimeout
	return &Regex{legacy: re}, nil
}

func (r *Regex) MatchString(text string) (bool, error) {
	if len(text) > MaxText {
		return false, errors.New("regex input exceeds 64 KiB")
	}
	if r.linear != nil {
		return r.linear.MatchString(text), nil
	}
	matched, err := r.legacy.MatchString(text)
	if err != nil {
		return false, errors.New("兼容正则匹配超时或失败，请简化过滤规则后重试")
	}
	return matched, nil
}

func (r *Regex) FindStringIndex(text string) ([]int, error) {
	matches, err := r.FindAllStringIndex(text, 1)
	if err != nil || len(matches) == 0 {
		return nil, err
	}
	return matches[0], nil
}

func (r *Regex) FindAllStringIndex(text string, n int) ([][]int, error) {
	if len(text) > MaxText {
		return nil, errors.New("regex input exceeds 64 KiB")
	}
	if r.linear != nil {
		return r.linear.FindAllStringIndex(text, n), nil
	}
	if n == 0 {
		return nil, nil
	}
	// regexp2 returns rune offsets; callers slice UTF-8 strings using bytes.
	offsets := make([]int, 0, len(text)+1)
	for offset := range text {
		offsets = append(offsets, offset)
	}
	offsets = append(offsets, len(text))
	deadline := time.Now().Add(regexTimeout)
	match, err := r.legacy.FindStringMatch(text)
	var out [][]int
	previousEnd := -1
	for match != nil && err == nil {
		// Match Go's rule for ignoring empty matches adjacent to a prior match.
		if match.Length != 0 || match.Index != previousEnd {
			out = append(out, []int{offsets[match.Index], offsets[match.Index+match.Length]})
			if n > 0 && len(out) >= n {
				return out, nil
			}
		}
		previousEnd = match.Index + match.Length
		if time.Now().After(deadline) {
			return nil, errors.New("兼容正则匹配超时，请简化过滤规则后重试")
		}
		match, err = r.legacy.FindNextMatch(match)
	}
	if err != nil {
		return nil, errors.New("兼容正则匹配超时或失败，请简化过滤规则后重试")
	}
	return out, nil
}
