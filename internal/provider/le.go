// SPDX-License-Identifier: AGPL-3.0-only
// Search metadata protocol: l429609201/misaka_danmu_server, historical commit
// 300ad904b3bed5a490c18855ca2903bdff5e1792 (AGPL-3.0). Episode pagination is
// independently verified against Le's public detail-index.js on 2026-09-30.
// Source JavaScript is never executed.
package provider

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/AniBakaBaka/AniDan/internal/danmaku"
	"golang.org/x/net/html"
)

// Only the flat string-valued data attributes used by the public search page
// are supported. This deliberately is not an evaluator or general JS parser.
// Duplicate keys, executable expressions, nested values and trailing input fail.
func leStringObject(raw string) (map[string]string, error) {
	if len(raw) > 512<<10 {
		return nil, danmaku.ErrLimit
	}
	if !utf8.ValidString(raw) {
		return nil, errors.New("le metadata is not UTF-8")
	}
	s := strings.TrimSpace(raw)
	if len(s) < 2 || s[0] != '{' {
		return nil, errors.New("le invalid metadata object")
	}
	i := 1
	space := func() {
		for i < len(s) && strings.ContainsRune(" \t\r\n", rune(s[i])) {
			i++
		}
	}
	quoted := func() (string, error) {
		if i >= len(s) || (s[i] != '\'' && s[i] != '"') {
			return "", errors.New("le metadata value must be quoted")
		}
		q := s[i]
		i++
		var b strings.Builder
		for i < len(s) {
			c := s[i]
			i++
			if c == q {
				return b.String(), nil
			}
			if c < 0x20 {
				return "", errors.New("le metadata contains control character")
			}
			if c == '\\' {
				if i >= len(s) {
					break
				}
				c = s[i]
				i++
				switch c {
				case '\\', '/', '\'', '"':
					b.WriteByte(c)
				case 'n':
					b.WriteByte('\n')
				case 'r':
					b.WriteByte('\r')
				case 't':
					b.WriteByte('\t')
				case 'b':
					b.WriteByte('\b')
				case 'f':
					b.WriteByte('\f')
				case 'u':
					if i+4 > len(s) {
						return "", errors.New("le incomplete unicode escape")
					}
					n, err := strconv.ParseUint(s[i:i+4], 16, 16)
					if err != nil {
						return "", errors.New("le invalid unicode escape")
					}
					i += 4
					r := rune(n)
					if utf16.IsSurrogate(r) {
						if r > 0xdbff || i+6 > len(s) || s[i:i+2] != `\u` {
							return "", errors.New("le incomplete unicode surrogate")
						}
						low, err := strconv.ParseUint(s[i+2:i+6], 16, 16)
						if err != nil || low < 0xdc00 || low > 0xdfff {
							return "", errors.New("le invalid unicode surrogate")
						}
						r = utf16.DecodeRune(r, rune(low))
						i += 6
					}
					b.WriteRune(r)
				default:
					return "", errors.New("le unsupported metadata escape")
				}
			} else {
				b.WriteByte(c)
			}
		}
		return "", errors.New("le incomplete quoted metadata")
	}
	out := map[string]string{}
	for {
		space()
		if i < len(s) && s[i] == '}' {
			i++
			space()
			if i != len(s) {
				return nil, errors.New("le trailing metadata expression")
			}
			return out, nil
		}
		if len(out) >= 64 {
			return nil, danmaku.ErrLimit
		}
		key := ""
		if i < len(s) && (s[i] == '\'' || s[i] == '"') {
			var err error
			key, err = quoted()
			if err != nil {
				return nil, err
			}
		} else {
			start := i
			for i < len(s) && ((s[i] >= 'a' && s[i] <= 'z') || (s[i] >= 'A' && s[i] <= 'Z') || (i > start && s[i] >= '0' && s[i] <= '9') || s[i] == '_') {
				i++
			}
			key = s[start:i]
		}
		if key == "" || len(key) > 64 {
			return nil, errors.New("le invalid metadata key")
		}
		if _, ok := out[key]; ok {
			return nil, errors.New("le duplicate metadata key")
		}
		space()
		if i >= len(s) || s[i] != ':' {
			return nil, errors.New("le metadata missing colon")
		}
		i++
		space()
		value, err := quoted()
		if err != nil {
			return nil, err
		}
		out[key] = value
		space()
		if i >= len(s) {
			return nil, errors.New("le incomplete metadata object")
		}
		if s[i] == '}' {
			continue
		}
		if s[i] != ',' {
			return nil, errors.New("le invalid metadata separator")
		}
		i++
	}
}

// Bound both input and DOM structure before any recursive selector helpers run.
func leHTML(raw []byte) (*html.Node, error) {
	if len(raw) > 4<<20 {
		return nil, danmaku.ErrLimit
	}
	if !utf8.Valid(raw) {
		return nil, errors.New("le page is not UTF-8")
	}
	doc, err := html.Parse(bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	type entry struct {
		n     *html.Node
		depth int
	}
	stack := []entry{{doc, 0}}
	count := 0
	for len(stack) > 0 {
		x := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		count++
		if count > 100000 || x.depth > 128 {
			return nil, danmaku.ErrLimit
		}
		for c := x.n.FirstChild; c != nil; c = c.NextSibling {
			stack = append(stack, entry{c, x.depth + 1})
		}
	}
	return doc, nil
}

func (l *Legacy) leSearch(ctx context.Context, keyword string) ([]Result, error) {
	if strings.TrimSpace(keyword) == "" || len(keyword) > 1024 {
		return nil, errors.New("invalid le search query")
	}
	raw, err := l.request(ctx, http.MethodGet, "https://so.le.com/s", url.Values{"wd": {keyword}, "from": {"pc"}, "ref": {"click"}, "click_area": {"search_button"}, "query": {keyword}, "is_default_query": {"0"}, "module": {"search_rst_page"}}, nil, http.Header{"Accept": {"text/html"}, "Referer": {"https://so.le.com/"}})
	if err != nil {
		return nil, err
	}
	doc, err := leHTML(raw)
	if err != nil {
		return nil, err
	}
	items := nodes(doc, func(n *html.Node) bool { return n.Data == "div" && class(n, "So-detail") })
	if len(items) == 0 {
		return nil, errors.New("le search layout missing; empty result not established")
	}
	if len(items) > 1000 {
		return nil, danmaku.ErrLimit
	}
	out := []Result{}
	seen := map[string]bool{}
	for _, n := range items {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		info, err := leStringObject(attr(n, "data-info"))
		if err != nil {
			return nil, err
		}
		typ := map[string]string{"tv": "tv_series", "movie": "movie", "cartoon": "anime", "comic": "anime", "playlet": "tv_series"}[info["type"]]
		if typ == "" {
			continue
		} // Subjects, clips and unrelated result kinds lack this episode contract.
		id := info["pid"]
		if !numericID.MatchString(id) || len(id) > 64 {
			return nil, errors.New("le search invalid media ID")
		}
		if seen[id] {
			continue
		}
		seen[id] = true
		heading := findNode(n, func(n *html.Node) bool { return n.Data == "h1" })
		link := findNode(heading, func(n *html.Node) bool { return n.Data == "a" })
		if link == nil {
			link = findNode(n, func(n *html.Node) bool { return n.Data == "a" && class(n, "j-baidu-a") })
		}
		title := strings.TrimSpace(attr(link, "title"))
		if title == "" {
			title = nodeText(link)
		}
		if title == "" || len(title) > 4096 {
			return nil, errors.New("le search missing valid title")
		}
		img := findNode(n, func(n *html.Node) bool { return n.Data == "img" })
		cover := textFirst(attr(img, "data-src"), attr(img, "src"))
		if strings.HasPrefix(cover, "//") {
			cover = "https:" + cover
		}
		year := 0
		if y := yearRE.FindString(info["keyWord"]); y != "" {
			year, _ = strconv.Atoi(y)
		}
		out = append(out, Result{ID: id, Title: title, Type: typ, ImageURL: cover, Year: year})
	}
	return out, nil
}

func (l *Legacy) leEpisodes(ctx context.Context, id string) ([]Episode, error) {
	if !numericID.MatchString(id) || len(id) > 64 {
		return nil, errors.New("le requires numeric media ID")
	}
	const size = 50
	total := -1
	out := []Episode{}
	seen := map[string]bool{}
	for page := 1; page <= l.maxSegments(); page++ {
		var v struct {
			Code scalarString `json:"code"`
			Data struct {
				Total    *int         `json:"total"`
				Page     scalarString `json:"current_page"`
				PageSize scalarString `json:"page_size"`
				List     []struct {
					ID      scalarString `json:"vid"`
					MediaID scalarString `json:"pid"`
					Index   scalarString `json:"episode"`
					Title   string       `json:"title"`
				} `json:"list"`
			} `json:"data"`
		}
		err := l.get(ctx, "https://d-api-m.le.com/detail/episode", url.Values{"pid": {id}, "platform": {"pc"}, "page": {strconv.Itoa(page)}, "pagesize": {strconv.Itoa(size)}, "type": {"1"}}, &v)
		if err != nil {
			return nil, err
		}
		if v.Code != "200" {
			return nil, errors.New("le episode API rejected request")
		}
		d := v.Data
		if d.Total == nil || d.List == nil || string(d.Page) != strconv.Itoa(page) || d.PageSize != "50" {
			return nil, errors.New("le episode pagination metadata missing or mismatched")
		}
		if *d.Total < 0 || *d.Total > 10000 {
			return nil, danmaku.ErrLimit
		}
		if total < 0 {
			total = *d.Total
			if (total+size-1)/size > l.maxSegments() {
				return nil, danmaku.ErrLimit
			}
		} else if *d.Total != total {
			return nil, errors.New("le episode total changed during pagination")
		}
		if len(d.List) != min(size, total-len(out)) {
			return nil, errors.New("le episode page incomplete")
		}
		for _, item := range d.List {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			vid := string(item.ID)
			index, err := strconv.Atoi(string(item.Index))
			if !numericID.MatchString(vid) || len(vid) > 64 || string(item.MediaID) != id || err != nil || index != len(out)+1 || seen[vid] {
				return nil, errors.New("le episode ID, media ID or sequence mismatch")
			}
			if item.Title == "" || len(item.Title) > 4096 {
				return nil, errors.New("le episode missing valid title")
			}
			seen[vid] = true
			out = append(out, Episode{ID: vid, Title: item.Title, Index: index, URL: fmt.Sprintf("https://www.le.com/ptv/vplay/%s.html", vid)})
		}
		if len(out) == total {
			return out, nil
		}
	}
	return nil, danmaku.ErrLimit
}
