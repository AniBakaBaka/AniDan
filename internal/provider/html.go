// SPDX-License-Identifier: AGPL-3.0-only
// Historical protocol compatibility; see LICENSES/provenance/PROVIDERS.md.
package provider

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"github.com/AniBakaBaka/AniDan/internal/danmaku"
	"golang.org/x/net/html"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

func attr(n *html.Node, key string) string {
	if n == nil {
		return ""
	}
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val
		}
	}
	return ""
}
func class(n *html.Node, name string) bool {
	return strings.Contains(" "+attr(n, "class")+" ", " "+name+" ")
}
func findNode(n *html.Node, pred func(*html.Node) bool) *html.Node {
	if n == nil {
		return nil
	}
	if pred(n) {
		return n
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if v := findNode(c, pred); v != nil {
			return v
		}
	}
	return nil
}
func nodeText(n *html.Node) string {
	if n == nil {
		return ""
	}
	var out strings.Builder
	var visit func(*html.Node)
	visit = func(v *html.Node) {
		if v.Type == html.TextNode {
			out.WriteString(v.Data)
		}
		for c := v.FirstChild; c != nil; c = c.NextSibling {
			visit(c)
		}
	}
	visit(n)
	return strings.TrimSpace(out.String())
}
func nodes(n *html.Node, pred func(*html.Node) bool) []*html.Node {
	out := []*html.Node{}
	var visit func(*html.Node)
	visit = func(v *html.Node) {
		if pred(v) {
			out = append(out, v)
		}
		for c := v.FirstChild; c != nil; c = c.NextSibling {
			visit(c)
		}
	}
	if n != nil {
		visit(n)
	}
	return out
}

var yearRE = regexp.MustCompile(`(?:19|20)\d{2}`)

func (l *Legacy) gamerSearch(ctx context.Context, key string) ([]Result, error) {
	raw, e := l.request(ctx, http.MethodGet, "https://ani.gamer.com.tw/search.php", url.Values{"keyword": {key}}, nil, http.Header{"Referer": {"https://ani.gamer.com.tw/"}})
	if e != nil {
		return nil, e
	}
	doc, e := html.Parse(bytes.NewReader(raw))
	if e != nil {
		return nil, e
	}
	container := findNode(doc, func(n *html.Node) bool { return class(n, "animate-theme-list") })
	if container == nil {
		return nil, errors.New("gamer search layout missing, possibly login/challenge page")
	}
	out := []Result{}
	for _, n := range nodes(container, func(n *html.Node) bool {
		return n.Type == html.ElementNode && n.Data == "a" && class(n, "theme-list-main")
	}) {
		u, e := url.Parse(attr(n, "href"))
		if e != nil || !strings.HasSuffix(u.Path, "animeRef.php") {
			continue
		}
		id := u.Query().Get("sn")
		if id == "" {
			continue
		}
		title := nodeText(findNode(n, func(n *html.Node) bool { return class(n, "theme-name") }))
		if title == "" {
			continue
		}
		year, _ := strconv.Atoi(yearRE.FindString(nodeText(findNode(n, func(n *html.Node) bool { return class(n, "theme-time") }))))
		img := findNode(n, func(n *html.Node) bool { return class(n, "theme-img") })
		image := textFirst(attr(img, "data-src"), attr(img, "src"))
		out = append(out, Result{ID: id, Title: title, Type: "tv_series", Year: year, ImageURL: image, Season: 1})
	}
	return out, nil
}

var gamerSN = regexp.MustCompile(`animefun\.videoSn\s*=\s*(\d+)\s*;`)
var gamerTitle = regexp.MustCompile(`animefun\.title\s*=\s*'([^']+)'\s*;`)

func (l *Legacy) gamerEpisodes(ctx context.Context, id string) ([]Episode, error) {
	raw, e := l.request(ctx, http.MethodGet, "https://ani.gamer.com.tw/animeRef.php", url.Values{"sn": {id}}, nil, nil)
	if e != nil {
		return nil, e
	}
	doc, e := html.Parse(bytes.NewReader(raw))
	if e != nil {
		return nil, e
	}
	section := findNode(doc, func(n *html.Node) bool { return n.Data == "section" && class(n, "season") })
	out := []Episode{}
	for _, n := range nodes(section, func(n *html.Node) bool { return n.Data == "a" }) {
		u, e := url.Parse(attr(n, "href"))
		if e != nil || !strings.HasSuffix(u.Path, "animeVideo.php") {
			continue
		}
		sn := u.Query().Get("sn")
		if sn == "" {
			continue
		}
		out = append(out, Episode{ID: sn, Title: nodeText(n), Index: len(out) + 1, URL: "https://ani.gamer.com.tw/animeVideo.php?sn=" + url.QueryEscape(sn)})
	}
	if section != nil {
		return out, nil
	}
	sn, title := gamerSN.FindSubmatch(raw), gamerTitle.FindSubmatch(raw)
	if len(sn) < 2 || len(title) < 2 {
		return nil, errors.New("gamer episode layout missing, possibly login/challenge page")
	}
	return []Episode{{ID: string(sn[1]), Title: string(title[1]), Index: 1, URL: "https://ani.gamer.com.tw/animeVideo.php?sn=" + string(sn[1])}}, nil
}
func (l *Legacy) gamerComments(ctx context.Context, id string) ([]danmaku.Comment, error) {
	raw, e := l.request(ctx, http.MethodPost, "https://ani.gamer.com.tw/ajax/danmuGet.php", nil, strings.NewReader(url.Values{"sn": {id}}.Encode()), http.Header{"Content-Type": {"application/x-www-form-urlencoded"}, "Referer": {"https://ani.gamer.com.tw/"}})
	if e != nil {
		return nil, e
	}
	var list []map[string]any
	if e = decodeJSON(raw, &list); e != nil {
		return nil, e
	}
	if list == nil {
		return nil, errors.New("gamer response is not an array")
	}
	totalBytes := int64(0)
	out := []danmaku.Comment{}
	seen := map[string]bool{}
	for _, m := range list {
		cid := str(m["sn"])
		if cid != "" && seen[cid] {
			continue
		}
		seen[cid] = true
		mode := 1
		switch integer(m["position"]) {
		case 1:
			mode = 5
		case 2:
			mode = 4
		}
		col := 0xffffff
		if s := str(m["color"]); s != "" {
			col, e = danmaku.ParseColor("#" + strings.TrimPrefix(s, "#"))
			if e != nil {
				return nil, e
			}
		}
		c, e := comment(m["sn"], number(m["time"])/10, mode, col, str(m["text"]), l.Source)
		if e != nil {
			return nil, e
		}
		out, e = l.appendBudget(out, &totalBytes, c)
		if e != nil {
			return nil, e
		}
	}
	return mergeCanonical(out)
}

var leDuration = regexp.MustCompile(`duration['"]?\s*:\s*['"]?((?:\d+:)?\d+:\d+)`)

func (l *Legacy) leComments(ctx context.Context, id string) ([]danmaku.Comment, error) {
	raw, e := l.request(ctx, http.MethodGet, "https://www.le.com/ptv/vplay/"+url.PathEscape(id)+".html", nil, nil, nil)
	if e != nil {
		return nil, e
	}
	match := leDuration.FindSubmatch(raw)
	if len(match) != 2 {
		return nil, errors.New("le video duration not found; refusing guessed/truncated download")
	}
	duration, e := durationSeconds(string(match[1]))
	if e != nil {
		return nil, e
	}
	count := (duration + 299) / 300
	if count > l.maxSegments() {
		return nil, danmaku.ErrLimit
	}
	totalBytes := int64(0)
	out := []danmaku.Comment{}
	seen := map[string]bool{}
	// Preserve first raw-ID wins before validation, including nonnumeric IDs
	// and malformed later duplicates. Fetch bounded windows in parallel, then
	// consume their original segment order. At most four response lists are
	// retained; the existing unique-comment and normalized-byte budgets below
	// remain unchanged. The shared scheduler cancels and joins failed windows.
	workers := l.SegmentWorkers
	if workers < 1 {
		workers = 4
	}
	workers = min(workers, 4)
	for first := 0; first < count; first += workers {
		parts := make([][]any, min(workers, count-first))
		_, e = l.segments(ctx, len(parts), func(ctx context.Context, index int) ([]danmaku.Comment, error) {
			start := (first + index) * 300
			raw, err := l.request(ctx, http.MethodGet, "https://hd-my.le.com/danmu/list", url.Values{"vid": {id}, "start": {strconv.Itoa(start)}, "end": {strconv.Itoa(min(start+300, duration))}, "callback": {"vjs_1"}}, nil, nil)
			if err != nil {
				return nil, err
			}
			var v map[string]any
			if err = decodeJSONP(raw, &v); err != nil {
				return nil, err
			}
			if integer(v["code"]) != 200 {
				return nil, fmt.Errorf("le comment code %s", str(v["code"]))
			}
			if at(v, "data", "list") == nil {
				return nil, errors.New("le response missing data.list")
			}
			parts[index] = array(at(v, "data", "list"))
			return nil, nil
		})
		if e != nil {
			return nil, e
		}
		for _, records := range parts {
			for _, a := range records {
				m := object(a)
				cid := textFirst(m["id"], m["_id"])
				if cid != "" && seen[cid] {
					continue
				}
				seen[cid] = true
				mode := 1
				switch integer(m["position"]) {
				case 3:
					mode = 4
				case 1:
					mode = 5
				}
				col := 0xffffff
				if s := str(m["color"]); s != "" {
					col, e = danmaku.ParseColor("#" + strings.TrimPrefix(s, "#"))
					if e != nil {
						return nil, e
					}
				}
				c, e := comment(cid, number(m["start"]), mode, col, str(m["txt"]), "le")
				if e != nil {
					return nil, e
				}
				out, e = l.appendBudget(out, &totalBytes, c)
				if e != nil {
					return nil, e
				}
			}
		}
	}
	return mergeCanonical(out)
}
func durationSeconds(s string) (int, error) {
	parts := strings.Split(s, ":")
	if len(parts) < 2 || len(parts) > 3 {
		return 0, errors.New("invalid duration")
	}
	total := 0
	for i, p := range parts {
		n, e := strconv.Atoi(p)
		if e != nil || n < 0 || n > 86400 || (i > 0 && n > 59) {
			return 0, errors.New("invalid duration")
		}
		total = total*60 + n
	}
	if total <= 0 || total > 86400 {
		return 0, errors.New("duration outside 24 hour limit")
	}
	return total, nil
}
