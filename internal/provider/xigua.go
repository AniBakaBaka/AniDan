// SPDX-License-Identifier: AGPL-3.0-only
// Native protocol adaptation from huangxd-/danmu_api, AGPL-3.0,
// commit fc1b7ff6add61d8af24c9bf978253273833f5afc, danmu_api/sources/xigua.js.
package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/AniBakaBaka/AniDan/internal/danmaku"
	"golang.org/x/net/html"
)

var xiguaHeaders = http.Header{"User-Agent": {"Mozilla/5.0 (iPhone; CPU iPhone OS 15_0 like Mac OS X) AppleWebKit/603.1.30 (KHTML, like Gecko) Version/17.5 Mobile/15A5370a Safari/602.1"}}
var xiguaEpisodesKey = regexp.MustCompile(`"episodes_list"\s*:\s*`)
var xiguaDurationKey = regexp.MustCompile(`"duration"\s*:\s*`)
var xiguaJSONNumber = regexp.MustCompile(`^(-?(?:0|[1-9][0-9]*)(?:\.[0-9]+)?(?:[eE][+-]?[0-9]+)?)(?:[\s,}\]])`)

func xiguaID(id string) error {
	if len(id) > 24 || !numericID.MatchString(id) {
		return errors.New("xigua requires a numeric video ID")
	}
	return nil
}
func (l *Legacy) xiguaPage(ctx context.Context, id string) ([]byte, error) {
	if e := xiguaID(id); e != nil {
		return nil, e
	}
	return l.request(ctx, http.MethodGet, "https://m.ixigua.com/video/"+id, nil, nil, xiguaHeaders)
}
func (l *Legacy) xiguaSearch(ctx context.Context, keyword string) ([]Result, error) {
	if strings.TrimSpace(keyword) == "" || len(keyword) > 2048 {
		return nil, errors.New("invalid xigua keyword")
	}
	raw, e := l.request(ctx, http.MethodGet, "https://m.ixigua.com/s/"+url.PathEscape(keyword), nil, nil, xiguaHeaders)
	if e != nil {
		return nil, e
	}
	doc, e := html.Parse(bytes.NewReader(raw))
	if e != nil {
		return nil, e
	}
	section := findNode(doc, func(n *html.Node) bool {
		return class(n, "search-section") && strings.TrimSpace(nodeText(findNode(n, func(v *html.Node) bool { return class(v, "search-section-title") }))) == "相关视频"
	})
	if section == nil {
		if bytes.Contains(raw, []byte("暂无相关视频")) {
			return []Result{}, nil
		}
		return nil, errors.New("xigua search lacks the verified related-video section")
	}
	cards := nodes(section, func(n *html.Node) bool { return class(n, "s-long-video") })
	if len(cards) > 1000 {
		return nil, danmaku.ErrLimit
	}
	out := []Result{}
	seen := map[string]bool{}
	for _, card := range cards {
		link := findNode(card, func(n *html.Node) bool { return n.Data == "a" && strings.HasPrefix(attr(n, "href"), "/video/") })
		id := strings.TrimPrefix(attr(link, "href"), "/video/")
		if xiguaID(id) != nil {
			continue
		}
		heading := findNode(card, func(n *html.Node) bool { return class(n, "s-long-video-info-title") })
		titled := findNode(heading, func(n *html.Node) bool { return attr(n, "title") != "" })
		title := strings.TrimSpace(attr(titled, "title"))
		if title == "" {
			title = nodeText(heading)
		}
		if title == "" || seen[id] {
			continue
		}
		seen[id] = true
		image := attr(findNode(card, func(n *html.Node) bool { return n.Data == "img" }), "src")
		if strings.HasPrefix(image, "//") {
			image = "https:" + image
		}
		typ, year := "other", 0
		for _, p := range nodes(card, func(n *html.Node) bool { return n.Data == "p" }) {
			parts := strings.Split(nodeText(p), "/")
			if len(parts) == 3 {
				y, err := strconv.Atoi(parts[2])
				if err == nil && y >= 1900 && y <= 2200 {
					year = y
					switch parts[0] {
					case "电影":
						typ = "movie"
					case "电视剧", "动漫", "动画":
						typ = "tv_series"
					}
				}
			}
		}
		out = append(out, Result{ID: id, Title: title, Type: typ, Year: year, ImageURL: image})
	}
	if len(cards) > 0 && len(out) == 0 {
		return nil, errors.New("xigua search cards contain no valid media identities")
	}
	return out, nil
}
func (l *Legacy) xiguaEpisodes(ctx context.Context, id string) ([]Episode, error) {
	raw, e := l.xiguaPage(ctx, id)
	if e != nil {
		return nil, e
	}
	matches := xiguaEpisodesKey.FindAllIndex(raw, 2)
	if len(matches) != 1 {
		return nil, errors.New("xigua page lacks one unambiguous episodes_list")
	}
	d := json.NewDecoder(bytes.NewReader(raw[matches[0][1]:]))
	d.UseNumber()
	token, e := d.Token()
	if e != nil || token != json.Delim('[') {
		return nil, errors.New("invalid xigua episode array")
	}
	out := []Episode{}
	seen := map[string]bool{}
	indices := map[int]bool{}
	for d.More() {
		if len(out) >= 10000 {
			return nil, danmaku.ErrLimit
		}
		if e = ctx.Err(); e != nil {
			return nil, e
		}
		var v struct {
			ID    scalarString `json:"gid"`
			Index int          `json:"seq_num"`
			Title string       `json:"title"`
		}
		if e = d.Decode(&v); e != nil {
			return nil, e
		}
		key := string(v.ID)
		if xiguaID(key) != nil || v.Index < 1 || seen[key] || indices[v.Index] {
			return nil, errors.New("invalid or duplicate xigua episode identity/index")
		}
		seen[key] = true
		indices[v.Index] = true
		title := v.Title
		if len(title) > 16384 {
			return nil, danmaku.ErrLimit
		}
		if title == "" {
			title = fmt.Sprintf("第 %d 集", v.Index)
		}
		out = append(out, Episode{ID: key, Index: v.Index, Title: title, URL: "https://m.ixigua.com/video/" + key})
	}
	if token, e = d.Token(); e != nil || token != json.Delim(']') {
		return nil, errors.New("incomplete xigua episode array")
	}
	if len(out) == 0 {
		return nil, errors.New("xigua episodes_list empty")
	}

	return out, nil
}
func xiguaDuration(raw []byte) (float64, error) {
	duration := float64(0)
	for {
		loc := xiguaDurationKey.FindIndex(raw)
		if loc == nil {
			break
		}
		raw = raw[loc[1]:]
		value := xiguaJSONNumber.FindSubmatch(raw)
		if value == nil {
			return 0, errors.New("invalid xigua duration value")
		}
		v, e := strconv.ParseFloat(string(value[1]), 64)
		if e != nil || math.IsNaN(v) || math.IsInf(v, 0) || v <= 0 {
			return 0, errors.New("invalid xigua duration")
		}
		if duration != 0 && v != duration {
			return 0, errors.New("ambiguous xigua page durations")
		}
		duration = v
		raw = raw[len(value[0]):]
	}
	if duration <= 0 {
		return 0, errors.New("xigua page has no verified duration")
	}
	return duration, nil
}

func (l *Legacy) xiguaComments(ctx context.Context, id string) ([]danmaku.Comment, error) {
	raw, e := l.xiguaPage(ctx, id)
	if e != nil {
		return nil, e
	}
	seconds, e := xiguaDuration(raw)
	if e != nil {
		return nil, e
	}
	if seconds > float64(l.maxSegments())*300 {
		return nil, danmaku.ErrLimit
	}
	duration := int64(math.Ceil(seconds * 1000))
	n := int((duration + 299999) / 300000)
	comments, e := l.segments(ctx, n, func(ctx context.Context, i int) ([]danmaku.Comment, error) {
		start, end := int64(i)*300000, min(int64(i+1)*300000, duration)
		q := url.Values{"item_id": {id}, "start_time": {strconv.FormatInt(start, 10)}, "end_time": {strconv.FormatInt(end, 10)}, "format": {"json"}}
		body, e := l.request(ctx, http.MethodGet, "https://ib.snssdk.com/vapp/danmaku/list/v1/", q, nil, xiguaHeaders)
		if e != nil {
			return nil, e
		}
		limit := l.MaxComments
		if limit <= 0 {
			limit = 500000
		}
		budget := l.MaxDownloadBytes
		if budget <= 0 {
			budget = 64 << 20
		}
		return decodeXiguaSegment(ctx, body, limit, budget, duration)
	})
	if e != nil {
		return nil, e
	}
	return mergeCanonical(comments)
}

// Decode one record at a time so a small-object JSON array cannot allocate an
// unbounded wire slice before the configured comment and byte caps are checked.
func decodeXiguaSegment(ctx context.Context, raw []byte, limit int, budget, duration int64) ([]danmaku.Comment, error) {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	tok, e := d.Token()
	if e != nil || tok != json.Delim('{') {
		return nil, errors.New("invalid xigua comment object")
	}
	out := []danmaku.Comment{}
	found := false
	used := int64(0)
	for d.More() {
		key, e := d.Token()
		if e != nil {
			return nil, e
		}
		switch key {
		case "data":
			if found {
				return nil, errors.New("duplicate xigua comment data")
			}
			found = true
			tok, e = d.Token()
			if e != nil || tok != json.Delim('[') {
				return nil, errors.New("xigua data must be an array")
			}
			for d.More() {
				if len(out) >= limit {
					return nil, danmaku.ErrLimit
				}
				if e = ctx.Err(); e != nil {
					return nil, e
				}
				var v struct {
					ID     scalarString `json:"danmaku_id"`
					Offset scalarString `json:"offset_time"`
					Text   *string      `json:"text"`
				}
				if e = d.Decode(&v); e != nil {
					return nil, e
				}
				if v.Text == nil {
					return nil, errors.New("xigua comment lacks text")
				}
				offset, e := strconv.ParseFloat(string(v.Offset), 64)
				if e != nil || offset < 0 || offset > float64(duration) {
					return nil, errors.New("invalid xigua comment offset")
				}
				cid, idError := strconv.ParseInt(string(v.ID), 10, 64)
				if idError != nil {
					cid = 0
				}
				c, e := danmaku.NewComment(cid, offset/1000, 1, 25, 0xffffff, *v.Text, "[xigua]")
				if e != nil {
					return nil, e
				}
				used += int64(len(c.M) + len(c.P) + 64)
				if used > budget {
					return nil, danmaku.ErrLimit
				}
				out = append(out, c)
			}
			tok, e = d.Token()
			if e != nil || tok != json.Delim(']') {
				return nil, errors.New("incomplete xigua comment array")
			}
		case "status_code", "code":
			var code int
			if e = d.Decode(&code); e != nil {
				return nil, e
			}
			if code != 0 {
				return nil, fmt.Errorf("xigua comment error %d", code)
			}
		default:
			if e = xiguaDiscardJSON(d); e != nil {
				return nil, e
			}
		}
	}
	tok, e = d.Token()
	if e != nil || tok != json.Delim('}') {
		return nil, errors.New("incomplete xigua comment object")
	}
	if !found {
		return nil, errors.New("xigua response missing comment data")
	}
	if _, e = d.Token(); e != io.EOF {
		return nil, errors.New("trailing xigua comment JSON")
	}
	return out, nil
}
func xiguaDiscardJSON(d *json.Decoder) error {
	token, e := d.Token()
	if e != nil {
		return e
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	if delim != '{' && delim != '[' {
		return errors.New("invalid JSON delimiter")
	}
	depth := 1
	for depth > 0 {
		token, e = d.Token()
		if e != nil {
			return e
		}
		if v, ok := token.(json.Delim); ok {
			switch v {
			case '[', '{':
				depth++
			case ']', '}':
				depth--
			}
			if depth > 64 {
				return danmaku.ErrLimit
			}
		}
	}
	return nil
}
