// SPDX-License-Identifier: AGPL-3.0-only
package danmaku

import (
	"errors"
	"fmt"
	"hash/fnv"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Options applies transformations to a fresh slice without mutating stored data.
// Convert must be an explicitly supplied converter (e.g. a full OpenCC binding).
// No incomplete character table is silently substituted for phrase conversion.
type Options struct {
	Offset      float64
	Start, End  *float64
	Blacklist   []*regexp.Regexp
	BlockModes  map[int]bool
	ModeMap     map[int]int
	Deduplicate bool
	MaxComments int
	ColorMode   string
	Palette     []int
	StripLikes  bool
	LikesStyle  string
	Convert     func(string) (string, error)
}

func Transform(in []Comment, o Options) ([]Comment, error) {
	if len(in) > DefaultParseOptions().MaxComments {
		return nil, ErrLimit
	}
	if !finite(o.Offset) || (o.Start != nil && !finite(*o.Start)) || (o.End != nil && !finite(*o.End)) {
		return nil, errors.New("non-finite time")
	}
	if o.Start != nil && o.End != nil && *o.Start > *o.End {
		return nil, errors.New("start exceeds end")
	}
	if o.MaxComments < 0 {
		return nil, errors.New("negative sample size")
	}
	switch o.ColorMode {
	case "", "off", "all_white", "all_random", "white_to_random", "highlight_only":
	default:
		return nil, fmt.Errorf("unsupported color mode %q", o.ColorMode)
	}
	palette := o.Palette
	if len(palette) == 0 {
		palette = []int{16777215, 16744319, 16752762, 16774799, 9498256, 8388564, 8900346, 14204888, 16758465}
	}
	for _, p := range palette {
		if p < 0 || p > 0xffffff {
			return nil, errors.New("palette RGB out of range")
		}
	}
	out := make([]Comment, 0, len(in))
	seen := make(map[string]struct{})
	for _, raw := range in {
		c, e := Normalize(raw, "[xml]")
		if e != nil {
			return nil, e
		}
		c.T += o.Offset
		if !finite(c.T) {
			return nil, errors.New("time overflow")
		}
		if c.T < 0 || (o.Start != nil && c.T < *o.Start) || (o.End != nil && c.T >= *o.End) {
			continue
		}
		blocked := false
		for _, re := range o.Blacklist {
			if re != nil && re.MatchString(c.M) {
				blocked = true
				break
			}
		}
		if blocked {
			continue
		}
		parts := strings.Split(c.P, ",")
		m, _ := strconv.Atoi(parts[1])
		if o.BlockModes[m] {
			continue
		}
		if dest, ok := o.ModeMap[m]; ok {
			if dest < 1 || dest > 9 {
				return nil, errors.New("mode out of range")
			}
			parts[1] = strconv.Itoa(dest)
		}
		if o.StripLikes || o.LikesStyle == "off" {
			c.M = StripLikes(c.M)
		} else if o.LikesStyle != "" {
			c.M = RestyleLikes(c.M, o.LikesStyle)
		}
		if o.Convert != nil {
			c.M, e = o.Convert(c.M)
			if e != nil {
				return nil, e
			}
		}
		col, _ := strconv.Atoi(parts[3])
		replace := o.ColorMode == "all_random" || (o.ColorMode == "white_to_random" && col == 0xffffff) || (o.ColorMode == "highlight_only" && HasLikes(c.M))
		if o.ColorMode == "all_white" {
			col = 0xffffff
		} else if replace {
			h := fnv.New64a()
			h.Write([]byte(c.P))
			h.Write([]byte(c.M))
			col = palette[int(h.Sum64()%uint64(len(palette)))]
		}
		parts[0] = formatTime(c.T)
		parts[3] = strconv.Itoa(col)
		c.P = strings.Join(parts, ",")
		if o.Deduplicate {
			k := c.P + "\x00" + c.M
			if _, ok := seen[k]; ok {
				continue
			}
			seen[k] = struct{}{}
		}
		out = append(out, c)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].T < out[j].T })
	if o.MaxComments > 0 {
		out = Sample(out, o.MaxComments)
	}
	return out, nil
}
func Offset(in []Comment, seconds float64) ([]Comment, error) {
	return Transform(in, Options{Offset: seconds})
}

// Split uses a half-open boundary: at belongs to the second part, rebased to 0.
func Split(in []Comment, at float64) ([]Comment, []Comment, error) {
	if at < 0 || !finite(at) {
		return nil, nil, errors.New("invalid split time")
	}
	a, e := Transform(in, Options{End: &at})
	if e != nil {
		return nil, nil, e
	}
	b, e := Transform(in, Options{Start: &at})
	if e != nil {
		return nil, nil, e
	}
	b, e = Offset(b, -at)
	return a, b, e
}
func Merge(groups ...[]Comment) ([]Comment, error) {
	n := 0
	for _, g := range groups {
		if len(g) > DefaultParseOptions().MaxComments-n {
			return nil, ErrLimit
		}
		n += len(g)
	}
	out := make([]Comment, 0, n)
	for _, g := range groups {
		out = append(out, g...)
	}
	return Transform(out, Options{Deduplicate: true})
}

// Sample deterministically samples a time-ordered copy; unlike legacy random
// sampling repeated player requests produce stable output.
func Sample(in []Comment, n int) []Comment {
	if n <= 0 {
		return []Comment{}
	}
	out := append([]Comment(nil), in...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].T < out[j].T })
	if n >= len(out) {
		return out
	}
	if n == 1 {
		return out[:1]
	}
	sample := make([]Comment, n)
	for i := range sample {
		sample[i] = out[int(math.Round(float64(i)*float64(len(out)-1)/float64(n-1)))]
	}
	return sample
}
func Player(in []Comment) ([]Comment, error) {
	out := make([]Comment, 0, len(in))
	for i, c := range in {
		n, e := Normalize(c, "[xml]")
		if e != nil {
			return nil, e
		}
		p := strings.Split(n.P, ",")
		n.P = strings.Join(append([]string{p[0], p[1], p[3]}, p[4:]...), ",")
		n.CID = int64(i)
		out = append(out, n)
	}
	return out, nil
}

var likesRE = regexp.MustCompile(`\s+(?:[🤍🩵🩷❤♡🔥]\x{FE0F}?\s+\d+(?:\.\d+)?[wk]?|\[[👍🔥]\d+(?:\.\d+)?[wk]?\]|\((?:点赞|热门)\d+(?:\.\d+)?[wk]?\)|\+\d+(?:\.\d+)?[wk]?)$`)
var legacyLikesRE = regexp.MustCompile(`\s+([🤍🔥])\s+(\d+(?:\.\d+)?[wk]?)$`)

func StripLikes(s string) string { return likesRE.ReplaceAllString(s, "") }
func HasLikes(s string) bool     { return likesRE.MatchString(s) }
func Likes(s string, n, threshold int, style string) string {
	if n < 5 || style == "off" {
		return s
	}
	v := strconv.Itoa(n)
	if n >= 10000 {
		v = fmt.Sprintf("%.1fw", float64(n)/10000)
	} else if n >= 1000 {
		v = fmt.Sprintf("%.1fk", float64(n)/1000)
	}
	if threshold <= 0 {
		threshold = 1000
	}
	return s + likeSuffix(v, n >= threshold, style)
}
func likeSuffix(n string, hot bool, style string) string {
	normal := map[string]string{"heart_white": "🤍", "heart_blue": "🩵", "heart_pink": "🩷", "heart_red": "❤️", "heart_outline": "♡"}
	sym := normal[style]
	if sym == "" {
		sym = "🤍"
	}
	if hot {
		sym = "🔥"
	}
	switch style {
	case "like_bracket":
		if !hot {
			sym = "👍"
		}
		return " [" + sym + n + "]"
	case "text":
		word := "点赞"
		if hot {
			word = "热门"
		}
		return " (" + word + n + ")"
	case "num_only":
		return " +" + n
	default:
		return " " + sym + " " + n
	}
}
func RestyleLikes(s, style string) string {
	if style == "off" {
		return StripLikes(s)
	}
	idx := legacyLikesRE.FindStringSubmatchIndex(s)
	if idx == nil {
		return s
	}
	return s[:idx[0]] + likeSuffix(s[idx[4]:idx[5]], s[idx[2]:idx[3]] == "🔥", style)
}

func PlayerComments(in []Comment) ([]Comment, error) { return Player(in) }
