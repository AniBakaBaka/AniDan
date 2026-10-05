// SPDX-License-Identifier: AGPL-3.0-only
// Translated from Misaka title_recognition.py at 01751526f6e4154bcc8f517481d02b68cb2684a9.
// Copyright and license acknowledgements are retained in THIRD_PARTY_NOTICES.md.
package recognition

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

const MaxContent = 1 << 20
const MaxText = 64 << 10
const MaxRules = 4096

type Warning struct {
	Line    int    `json:"line"`
	Message string `json:"message"`
}

func (w Warning) String() string { return fmt.Sprintf("line %d: %s", w.Line, w.Message) }

type Rule struct {
	Line                                                              int
	Raw, Kind, Stage, Source, Target, Before, After, Offset, Provider string
	Metadata                                                          map[string]any
	RangeStart                                                        int
	RangeEnd                                                          *int
}
type Rules struct{ Items []Rule }
type Input struct {
	Text            string
	Season, Episode *int
	Provider        string
}
type Trace struct {
	RuleIndex                                              int    `json:"ruleIndex"`
	Rule                                                   string `json:"rule"`
	Stage                                                  string `json:"stage"`
	Before                                                 string `json:"before"`
	After                                                  string `json:"after"`
	SeasonBefore, SeasonAfter, EpisodeBefore, EpisodeAfter *int
	Metadata                                               map[string]any `json:"metadata,omitempty"`
}
type Result struct {
	Text            string
	Season, Episode *int
	Metadata        map[string]any
	Changed         bool
	Trace           []Trace
	Warnings        []Warning
}

func Int(n int) *int { return &n }
func copyInt(p *int) *int {
	if p == nil {
		return nil
	}
	return Int(*p)
}
func eq(a, b *int) bool { return a == nil && b == nil || a != nil && b != nil && *a == *b }
func result(in Input) Result {
	return Result{Text: in.Text, Season: copyInt(in.Season), Episode: copyInt(in.Episode), Metadata: map[string]any{}, Trace: []Trace{}, Warnings: []Warning{}}
}
func (r Result) Input(provider string) Input { return Input{r.Text, r.Season, r.Episode, provider} }
func Parse(content string) (*Rules, []Warning) {
	out := &Rules{Items: []Rule{}}
	warnings := []Warning{}
	if len(content) > MaxContent {
		return out, []Warning{{0, "rule content exceeds 1 MiB"}}
	}
	for i, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if len(out.Items) >= MaxRules {
			warnings = append(warnings, Warning{i + 1, "more than 4096 rules"})
			break
		}
		r, e := parseRule(line)
		if e != nil {
			warnings = append(warnings, Warning{i + 1, e.Error()})
			continue
		}
		r.Line = i + 1
		r.Raw = line
		out.Items = append(out.Items, r)
	}
	return out, warnings
}
func parseRule(line string) (Rule, error) {
	r := Rule{Stage: "preprocess", Metadata: map[string]any{}, Provider: "all"}
	if strings.HasPrefix(line, "BLOCK:") {
		r.Kind = "block"
		r.Source = strings.TrimSpace(line[6:])
		if r.Source == "" {
			return r, fmt.Errorf("empty BLOCK word")
		}
		return r, nil
	}
	if a, b, ok := strings.Cut(line, " && "); ok {
		p, e := parseRule(a)
		if e != nil || p.Kind != "replace" {
			return r, fmt.Errorf("complex rule requires a literal replacement")
		}
		o, e := parseOffset(b)
		if e != nil {
			return r, e
		}
		p.Kind = "complex"
		p.Before = o.Before
		p.After = o.After
		p.Offset = o.Offset
		return p, nil
	}
	if a, b, ok := strings.Cut(line, " => "); ok {
		r.Source = strings.TrimSpace(a)
		r.Target = strings.TrimSpace(b)
		if r.Source == "" || r.Target == "" {
			return r, fmt.Errorf("empty replacement source or target")
		}
		r.Kind = "replace"
		if strings.HasPrefix(r.Target, "{<") && strings.HasSuffix(r.Target, ">}") {
			m := metadata(r.Target[2 : len(r.Target)-2])
			n, e := strconv.Atoi(m["search_season"])
			if e != nil {
				return r, fmt.Errorf("search_season must be an integer")
			}
			r.Kind = "search_season"
			r.Metadata["search_season"] = n
			return r, nil
		}
		if strings.HasPrefix(r.Target, "{[") && strings.HasSuffix(r.Target, "]}") {
			m := metadata(r.Target[2 : len(r.Target)-2])
			r.Stage = "postprocess"
			r.Kind = "metadata_replace"
			if m["source"] != "" {
				r.Provider = m["source"]
			}
			for k, v := range m {
				switch k {
				case "tmdbid", "doubanid", "search_season":
					n, e := strconv.Atoi(v)
					if e != nil {
						return r, fmt.Errorf("%s must be an integer", k)
					}
					r.Metadata[k] = n
				case "type", "s", "e", "season_offset", "title":
					r.Metadata[k] = v
				case "source", "ep_range", "ep_offset":
				default:
					return r, fmt.Errorf("unsupported metadata key %q", k)
				}
			}
			if v, ok := m["ep_range"]; ok {
				a, b, yes := strings.Cut(v, "-")
				lo, e := strconv.Atoi(strings.TrimSpace(a))
				if !yes || e != nil || lo < 0 {
					return r, fmt.Errorf("invalid episode range")
				}
				r.RangeStart = lo
				if strings.TrimSpace(b) != "*" {
					hi, e := strconv.Atoi(strings.TrimSpace(b))
					if e != nil || hi < lo {
						return r, fmt.Errorf("invalid episode range end")
					}
					r.RangeEnd = Int(hi)
				}
				r.Offset = m["ep_offset"]
				if e := validateOffset(r.Offset); e != nil {
					return r, e
				}
				r.Kind = "partial_offset"
			} else if v, ok := m["season_offset"]; ok {
				if _, _, _, e := parseSeasonOffset(v); e != nil {
					return r, e
				}
				r.Kind = "season_offset"
			}
			return r, nil
		}
		return r, nil
	}
	if strings.Contains(line, " <> ") && strings.Contains(line, " >> ") {
		return parseOffset(line)
	}
	return r, fmt.Errorf("unrecognized rule; use BLOCK:, =>, or <> and >> with spaces")
}
func metadata(s string) map[string]string {
	out := map[string]string{}
	for _, v := range strings.Split(s, ";") {
		if k, v, ok := strings.Cut(v, "="); ok {
			out[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	return out
}
func parseOffset(s string) (Rule, error) {
	r := Rule{Kind: "offset", Stage: "preprocess"}
	left, offset, ok := strings.Cut(s, " >> ")
	if !ok {
		return r, fmt.Errorf("missing >> offset")
	}
	before, after, ok := strings.Cut(left, " <> ")
	if !ok {
		return r, fmt.Errorf("missing <> locators")
	}
	r.Before = strings.TrimSpace(before)
	r.After = strings.TrimSpace(after)
	r.Offset = strings.TrimSpace(offset)
	if r.Before == "" && r.After == "" {
		return r, fmt.Errorf("both locators are empty")
	}
	return r, validateOffset(r.Offset)
}
func validateOffset(s string) error { _, e := Offset(1, s); return e }
func ExactMatch(text, pattern string) bool {
	if pattern == "" {
		return false
	}
	if text == pattern {
		return true
	}
	for start := 0; start < len(text); {
		idx := strings.Index(text[start:], pattern)
		if idx < 0 {
			return false
		}
		idx += start
		end := idx + len(pattern)
		before := idx == 0
		after := end == len(text)
		if !before {
			r, _ := utf8.DecodeLastRuneInString(text[:idx])
			before = boundary(r)
		}
		if !after {
			r, _ := utf8.DecodeRuneInString(text[end:])
			after = boundary(r)
		}
		if before && after {
			return true
		}
		start = idx + 1
	}
	return false
}
func boundary(r rune) bool {
	return unicode.IsSpace(r) || strings.ContainsRune("-_[]()（）【】", r)
}
func providerMatch(want, have string) bool {
	return want == "" || want == "all" || have == "" || want == have
}
func (rules *Rules) Preprocess(in Input) Result  { return rules.apply(in, "preprocess") }
func (rules *Rules) Postprocess(in Input) Result { return rules.apply(in, "postprocess") }
func (rules *Rules) Apply(in Input) Result {
	pre := rules.Preprocess(in)
	post := rules.Postprocess(pre.Input(in.Provider))
	post.Changed = pre.Changed || post.Changed
	post.Trace = append(pre.Trace, post.Trace...)
	post.Warnings = append(pre.Warnings, post.Warnings...)
	return post
}
func (rules *Rules) apply(in Input, stage string) Result {
	out := result(in)
	if len(in.Text) > MaxText {
		out.Warnings = append(out.Warnings, Warning{0, "text exceeds 64 KiB"})
		return out
	}
	if rules == nil {
		return out
	}
	for _, r := range rules.Items {
		if r.Stage != stage {
			continue
		}
		before := out.Text
		bs, be := copyInt(out.Season), copyInt(out.Episode)
		metaChanged := false
		switch r.Kind {
		case "block":
			out.Text = strings.TrimSpace(strings.ReplaceAll(out.Text, r.Source, ""))
		case "replace":
			if ExactMatch(out.Text, r.Source) {
				out.Text = boundedReplace(out.Text, r.Source, r.Target, r.Line, &out.Warnings)
			}
		case "complex":
			if strings.Contains(out.Text, r.Source) {
				out.Text = boundedReplace(out.Text, r.Source, r.Target, r.Line, &out.Warnings)
				out.Episode = applyLocator(out.Text, out.Episode, r, &out.Warnings)
			}
		case "offset":
			out.Episode = applyLocator(out.Text, out.Episode, r, &out.Warnings)
		case "search_season":
			if ExactMatch(out.Text, r.Source) {
				out.Season = Int(r.Metadata["search_season"].(int))
			}
		case "season_offset":
			if ExactMatch(out.Text, r.Source) && providerMatch(r.Provider, in.Provider) {
				if title, _ := r.Metadata["title"].(string); title != "" {
					out.Text = title
				}
				if out.Season != nil {
					v, e := SeasonOffset(*out.Season, r.Metadata["season_offset"].(string))
					if e != nil {
						out.Warnings = append(out.Warnings, Warning{r.Line, e.Error()})
					} else {
						out.Season = Int(v)
					}
				}
			}
		case "partial_offset":
			if ExactMatch(out.Text, r.Source) && providerMatch(r.Provider, in.Provider) && out.Episode != nil && *out.Episode >= r.RangeStart && (r.RangeEnd == nil || *out.Episode <= *r.RangeEnd) {
				v, e := Offset(*out.Episode, r.Offset)
				if e != nil {
					out.Warnings = append(out.Warnings, Warning{r.Line, e.Error()})
				} else {
					out.Episode = Int(v)
				}
			}
		case "metadata_replace":
			if ExactMatch(out.Text, r.Source) && providerMatch(r.Provider, in.Provider) {
				for k, v := range r.Metadata {
					out.Metadata[k] = v
				}
				metaChanged = true
			}
		}
		if len(out.Text) > MaxText {
			out.Text = before
			out.Warnings = append(out.Warnings, Warning{r.Line, "replacement exceeds 64 KiB"})
		}
		if out.Text != before || !eq(bs, out.Season) || !eq(be, out.Episode) || metaChanged {
			out.Changed = true
			tr := Trace{r.Line - 1, r.Raw, r.Stage, before, out.Text, bs, copyInt(out.Season), be, copyInt(out.Episode), nil}
			if metaChanged {
				tr.Metadata = map[string]any{}
				for k, v := range r.Metadata {
					tr.Metadata[k] = v
				}
			}
			out.Trace = append(out.Trace, tr)
		}
	}
	return out
}

var arabicNumbers = regexp.MustCompile(`[0-9]+`)
var chineseNumbers = regexp.MustCompile(`[零〇一二两三四五六七八九十百千]+`)

func applyLocator(text string, old *int, r Rule, warnings *[]Warning) *int {
	start := 0
	if r.Before != "" {
		i := strings.Index(text, r.Before)
		if i < 0 {
			return old
		}
		start = i + len(r.Before)
	}
	end := len(text)
	if r.After != "" {
		i := strings.Index(text[start:], r.After)
		if i < 0 {
			return old
		}
		end = start + i
	} else {
		return old
	}
	part := text[start:end]
	num := arabicNumbers.FindString(part)
	n, e := strconv.Atoi(num)
	if num == "" {
		n, e = ChineseNumber(chineseNumbers.FindString(part))
	}
	if e != nil {
		return old
	}
	v, e := Offset(n, r.Offset)
	if e != nil {
		*warnings = append(*warnings, Warning{r.Line, e.Error()})
		return old
	}
	return Int(v)
}
func ChineseNumber(s string) (int, error) {
	if n, e := strconv.Atoi(s); e == nil {
		return n, nil
	}
	if s == "" {
		return 0, fmt.Errorf("empty number")
	}
	digits := map[rune]int{'零': 0, '〇': 0, '一': 1, '二': 2, '两': 2, '三': 3, '四': 4, '五': 5, '六': 6, '七': 7, '八': 8, '九': 9}
	units := map[rune]int{'十': 10, '百': 100, '千': 1000}
	sum, n := 0, 0
	hasUnit := false
	for _, r := range s {
		if d, ok := digits[r]; ok {
			n = n*10 + d
		} else if u, ok := units[r]; ok {
			hasUnit = true
			if n == 0 {
				n = 1
			}
			sum += n * u
			n = 0
		} else {
			return 0, fmt.Errorf("invalid Chinese number")
		}
	}
	if !hasUnit {
		return n, nil
	}
	return sum + n, nil
}

var seasonRE = regexp.MustCompile(`^(\*|[0-9]+)\s*([+>\-])\s*([0-9]+)$`)

func parseSeasonOffset(s string) (string, string, int, error) {
	m := seasonRE.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return "", "", 0, fmt.Errorf("invalid season offset; expected 1>2, 1+2, 1-2, or *+2")
	}
	v, e := strconv.Atoi(m[3])
	if e != nil || v > 1000000 {
		return "", "", 0, fmt.Errorf("season offset out of range")
	}
	return m[1], m[2], v, nil
}
func SeasonOffset(season int, s string) (int, error) {
	from, op, n, e := parseSeasonOffset(s)
	if e != nil {
		return season, e
	}
	if from != "*" {
		v, _ := strconv.Atoi(from)
		if season != v {
			return season, nil
		}
	}
	switch op {
	case "+":
		season += n
	case "-":
		season -= n
	case ">":
		season = n
	}
	if season < 1 {
		season = 1
	}
	return season, nil
}
func (r *Rules) ReverseEpisode(text string, episode int, provider string) int {
	for _, v := range r.Items {
		if v.Kind != "partial_offset" || !ExactMatch(text, v.Source) || !providerMatch(v.Provider, provider) || strings.Contains(strings.ToUpper(v.Offset), "EP") {
			continue
		}
		n, e := strconv.Atoi(v.Offset)
		if e != nil {
			continue
		}
		raw := episode - n
		if raw < 1 {
			raw = 1
		}
		if raw >= v.RangeStart && (v.RangeEnd == nil || raw <= *v.RangeEnd) {
			return raw
		}
	}
	return episode
}

type Mapping struct {
	SearchTitle      string `json:"search_title"`
	RecognitionTitle string `json:"recognition_title"`
	Source           string `json:"rule_source_restriction"`
	SearchSeason     *int   `json:"search_season"`
}

func (r *Rules) SearchMapping(title string) *Mapping {
	for _, v := range r.Items {
		if v.Kind != "season_offset" && v.Kind != "metadata_replace" {
			continue
		}
		target, _ := v.Metadata["title"].(string)
		if !ExactMatch(title, target) {
			continue
		}
		m := &Mapping{SearchTitle: v.Source, RecognitionTitle: target, Source: v.Provider}
		if off, _ := v.Metadata["season_offset"].(string); off != "" {
			from, _, _, e := parseSeasonOffset(off)
			if e == nil && from != "*" {
				n, _ := strconv.Atoi(from)
				m.SearchSeason = Int(n)
			}
		}
		return m
	}
	return nil
}

func boundedReplace(text, source, target string, line int, warnings *[]Warning) string {
	if source == "" {
		return text
	}
	growth := len(target) - len(source)
	if growth > 0 && strings.Count(text, source) > (MaxText-len(text))/growth {
		*warnings = append(*warnings, Warning{line, "replacement exceeds 64 KiB"})
		return text
	}
	return strings.ReplaceAll(text, source, target)
}

// HintForResult explains an existing forward match; it does not rank candidates.
func (r *Rules) HintForResult(title, provider string) map[string]any {
	for _, v := range r.Items {
		if (v.Kind == "season_offset" || v.Kind == "metadata_replace") && ExactMatch(title, v.Source) && providerMatch(v.Provider, provider) {
			return map[string]any{"source": v.Source, "source_restriction": v.Provider, "recognition_title": v.Metadata["title"], "season_offset": v.Metadata["season_offset"]}
		}
	}
	return nil
}
