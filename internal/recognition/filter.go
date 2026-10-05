// SPDX-License-Identifier: AGPL-3.0-only
// Adapted from Misaka episode_filter.py and scraper_manager.py, pinned upstream.
package recognition

import (
	"fmt"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

const DefaultBlacklistCN = `特典|预告|广告|菜单|花絮|特辑|速看|资讯|彩蛋|直拍|直播回顾|片头|片尾|幕后|映像|番外篇|纪录片|访谈|番外|短片|加更|走心|解忧|纯享|解读|揭秘|赏析`
const DefaultBlacklistEN = `NC|OP|ED|SP|OVA|OAD|CM|PV|MV|BDMenu|Menu|Bonus|Recap|Teaser|Trailer|Preview|CD|Disc|Scan|Sample|Logo|Info|EDPV|SongSpot|BDSpot`

// CompileRegex deliberately uses linear-time RE2. Backreferences, lookaround,
// and other Python-only constructs produce an explicit error, never a fallback.
func CompileRegex(pattern string) (*regexp.Regexp, error) {
	if len(pattern) > 16384 {
		return nil, fmt.Errorf("pattern exceeds 16 KiB")
	}
	r, e := regexp.Compile("(?i)" + pattern)
	if e != nil {
		return nil, fmt.Errorf("unsupported or invalid RE2 pattern: %w", e)
	}
	return r, nil
}

type Blacklist struct{ cn, en *regexp.Regexp }

func NewBlacklist(cn, en string) (*Blacklist, error) {
	b := &Blacklist{}
	var e error
	if cn != "" {
		b.cn, e = CompileRegex(cn)
		if e != nil {
			return nil, e
		}
	}
	if en != "" {
		b.en, e = CompileRegex("(?:" + en + ")(?:[0-9]{1,2})?(?:\\s|_ALL)?")
		if e != nil {
			return nil, e
		}
	}
	return b, nil
}
func word(r rune) bool { return unicode.IsLetter(r) || unicode.IsNumber(r) || r == '_' }
func (b *Blacklist) Match(title string) bool {
	if b == nil {
		return false
	}
	if b.cn != nil && b.cn.MatchString(title) {
		return true
	}
	if b.en == nil {
		return false
	}
	for _, loc := range b.en.FindAllStringIndex(title, -1) {
		lo, hi := loc[0], loc[1]
		left := lo == 0
		right := hi == len(title)
		first, _ := utf8.DecodeRuneInString(title[lo:])
		last, _ := utf8.DecodeLastRuneInString(title[:hi])
		if !left {
			p, _ := utf8.DecodeLastRuneInString(title[:lo])
			left = p == '[' || p == '【' || word(p) != word(first)
		}
		if !right {
			n, _ := utf8.DecodeRuneInString(title[hi:])
			right = n == ']' || n == '】' || word(n) != word(last)
		}
		if left && right {
			return true
		}
	}
	return false
}

type EpisodeFilter struct {
	Title, Pattern, Provider, MediaID string
	re                                *regexp.Regexp
	Line                              int
}

func ParseEpisodeFilters(content string) ([]EpisodeFilter, []Warning) {
	out := []EpisodeFilter{}
	warnings := []Warning{}
	if len(content) > MaxContent {
		return out, []Warning{{0, "filter exceeds 1 MiB"}}
	}
	for i, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if len(out) >= MaxRules {
			warnings = append(warnings, Warning{i + 1, "more than 4096 filters"})
			break
		}
		title, block, ok := strings.Cut(line, " => ")
		if !ok || !strings.HasPrefix(block, "{[") || !strings.HasSuffix(block, "]}") {
			warnings = append(warnings, Warning{i + 1, "expected Title => {[rules=regex;provider=optional;mediaId=optional]}"})
			continue
		}
		m := metadata(block[2 : len(block)-2])
		title = strings.TrimSpace(title)
		pattern := m["rules"]
		if title == "" || pattern == "" {
			warnings = append(warnings, Warning{i + 1, "title and rules are required"})
			continue
		}
		re, e := CompileRegex(pattern)
		if e != nil {
			warnings = append(warnings, Warning{i + 1, e.Error()})
			continue
		}
		out = append(out, EpisodeFilter{title, pattern, m["provider"], m["mediaId"], re, i + 1})
	}
	return out, warnings
}
func (f EpisodeFilter) Applies(title, provider, mediaID string, aliases []string) bool {
	if title == "" || f.Provider != "" && f.Provider != provider || f.MediaID != "" && f.MediaID != mediaID {
		return false
	}
	for _, t := range append([]string{title}, aliases...) {
		if strings.Contains(strings.ToLower(t), strings.ToLower(f.Title)) {
			return true
		}
	}
	return false
}
func (f EpisodeFilter) Match(episodeTitle string) bool {
	return f.re != nil && f.re.MatchString(episodeTitle)
}

type Conflict struct {
	RuleIndex    int    `json:"ruleIndex"`
	RuleContent  string `json:"ruleContent"`
	IssueType    string `json:"issueType"`
	Severity     string `json:"severity"`
	Detail       string `json:"detail"`
	RelatedRules []int  `json:"relatedRules"`
}

func Conflicts(content string) []Conflict {
	rules, warnings := Parse(content)
	out := []Conflict{}
	lines := strings.Split(content, "\n")
	for _, w := range warnings {
		line := ""
		if w.Line > 0 && w.Line <= len(lines) {
			line = lines[w.Line-1]
		}
		out = append(out, Conflict{w.Line - 1, line, "parse_error", "error", w.Message, []int{}})
	}
	seen := map[string]int{}
	for i, r := range rules.Items {
		if prior, ok := seen[r.Raw]; ok {
			out = append(out, Conflict{r.Line - 1, r.Raw, "duplicate", "warning", "identical earlier rule", []int{prior}})
		} else {
			seen[r.Raw] = r.Line - 1
		}
		if utf8.RuneCountInString(r.Source) == 1 {
			out = append(out, Conflict{r.Line - 1, r.Raw, "too_short", "warning", "one-character match may affect unrelated titles", []int{}})
		}
		for _, earlier := range rules.Items[:i] {
			if earlier.Kind == "block" && strings.Contains(r.Source, earlier.Source) {
				out = append(out, Conflict{r.Line - 1, r.Raw, "unreachable", "warning", "earlier BLOCK removes part of this rule's source before matching", []int{earlier.Line - 1}})
				break
			}
			if earlier.Source == r.Source && earlier.Stage == r.Stage && earlier.Kind == "replace" {
				out = append(out, Conflict{r.Line - 1, r.Raw, "overlap", "warning", "earlier replacement may change this source before it is tested", []int{earlier.Line - 1}})
				break
			}
		}
	}
	return out
}
