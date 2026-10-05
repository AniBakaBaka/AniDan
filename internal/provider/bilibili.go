// SPDX-License-Identifier: AGPL-3.0-only
// Protocol details adapted from misaka_danmu_server/src/scrapers/bilibili.py,
// l429609201 and contributors, historical commit 300ad904 (AGPL-3.0).
package provider

import (
	"context"
	"crypto/md5"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"github.com/AniBakaBaka/AniDan/internal/danmaku"
	"html"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

type Bilibili struct{ HTTPProvider }

func NewBilibili(c *http.Client) *Bilibili {
	h := newHTTP(c)
	h.sourceName = "bilibili"
	h.Headers.Set("Referer", "https://www.bilibili.com/")
	return &Bilibili{h}
}
func (b *Bilibili) Name() string { return "bilibili" }
func (b *Bilibili) Capability() Capability {
	return Capability{Name: b.Name(), Search: true, Episodes: true, Comments: true, Status: "live sample verified 2026-09-30", Blocker: "one public title verified; WBI search can require authorized cookies; primary comment pool only"}
}

var tags = regexp.MustCompile(`<[^>]*>`)
var wbiPermutation = []int{46, 47, 18, 2, 53, 8, 23, 32, 15, 50, 10, 31, 58, 3, 45, 35, 27, 43, 5, 49, 33, 9, 42, 19, 29, 28, 14, 39, 12, 38, 41, 13, 37, 48, 7, 16, 24, 55, 40, 61, 26, 17, 0, 1, 60, 51, 30, 4, 22, 25, 54, 21, 56, 59, 6, 63, 57, 62, 11, 36, 20, 34, 44, 52}

func (b *Bilibili) wbi(ctx context.Context) (string, error) {
	var v map[string]any
	if e := b.get(ctx, "https://api.bilibili.com/x/web-interface/nav", nil, &v); e != nil {
		return "", e
	}
	key := ""
	for _, n := range []string{"img_url", "sub_url"} {
		raw := str(at(v, "data", "wbi_img", n))
		u, e := url.Parse(raw)
		if e != nil {
			return "", e
		}
		base := path.Base(u.Path)
		key += strings.TrimSuffix(base, path.Ext(base))
	}
	if len(key) < 64 {
		return "", errors.New("bilibili did not provide WBI keys")
	}
	var out strings.Builder
	for _, i := range wbiPermutation[:32] {
		out.WriteByte(key[i])
	}
	return out.String(), nil
}
func signWBI(q url.Values, key string, ts int64) {
	q.Set("wts", strconv.FormatInt(ts, 10))
	for k, vs := range q {
		for i, v := range vs {
			vs[i] = strings.Map(func(r rune) rune {
				if strings.ContainsRune("!'()*", r) {
					return -1
				}
				return r
			}, v)
		}
		q[k] = vs
	}
	sum := md5.Sum([]byte(q.Encode() + key))
	q.Set("w_rid", hex.EncodeToString(sum[:]))
}
func (b *Bilibili) Search(ctx context.Context, keyword string) ([]Result, error) {
	key, e := b.wbi(ctx)
	if e != nil {
		return nil, e
	}
	out := []Result{}
	seen := map[string]bool{}
	for _, kind := range []string{"media_bangumi", "media_ft"} {
		q := url.Values{"keyword": {keyword}, "search_type": {kind}, "page": {"1"}}
		signWBI(q, key, time.Now().Unix())
		var v map[string]any
		if e = b.get(ctx, "https://api.bilibili.com/x/web-interface/wbi/search/type", q, &v); e != nil {
			return out, e
		}
		if integer(v["code"]) != 0 {
			return out, fmt.Errorf("bilibili search code %s", str(v["code"]))
		}
		if object(v["data"]) == nil {
			return out, errors.New("bilibili search response missing data")
		}
		for _, a := range array(at(v, "data", "result")) {
			m := object(a)
			id := ""
			if s := str(m["season_id"]); s != "" && s != "0" {
				id = "ss" + s
			} else if s = str(m["bvid"]); s != "" {
				id = s
			}
			if id == "" || seen[id] {
				continue
			}
			seen[id] = true
			typ := "tv_series"
			if str(m["season_type_name"]) == "电影" {
				typ = "movie"
			}
			img := str(m["cover"])
			if strings.HasPrefix(img, "//") {
				img = "https:" + img
			}
			year := 0
			if n := integer(m["pubtime"]); n > 0 {
				year = time.Unix(int64(n), 0).UTC().Year()
			}
			out = append(out, Result{ID: id, Title: html.UnescapeString(tags.ReplaceAllString(str(m["title"]), "")), Type: typ, ImageURL: img, Year: year, Season: 1})
		}
	}
	return out, nil
}
func (b *Bilibili) Episodes(ctx context.Context, id string) ([]Episode, error) {
	if strings.HasPrefix(id, "collection:") {
		info, err := parseCollectionID(id)
		if err != nil {
			return nil, err
		}
		return b.collectionEpisodes(ctx, info, nil)
	}

	if e := validID(id); e != nil {
		return nil, e
	}
	pageFilter := 0
	if cut := strings.LastIndex(id, ":p"); cut > 0 {
		var err error
		pageFilter, err = strconv.Atoi(id[cut+2:])
		if err != nil || pageFilter < 1 {
			return nil, errors.New("invalid bilibili page")
		}
		id = id[:cut]
	}
	var v map[string]any
	out := []Episode{}
	if strings.HasPrefix(id, "ss") || strings.HasPrefix(id, "ep") {
		param := "season_id"
		if strings.HasPrefix(id, "ep") {
			param = "ep_id"
		}
		if _, e := strconv.ParseInt(id[2:], 10, 64); e != nil {
			return nil, errors.New("invalid bilibili season/episode ID")
		}
		if e := b.get(ctx, "https://api.bilibili.com/pgc/view/web/season", url.Values{param: {id[2:]}}, &v); e != nil {
			return nil, e
		}
		if integer(v["code"]) != 0 {
			return nil, fmt.Errorf("bilibili season code %s", str(v["code"]))
		}
		r := object(v["result"])
		if r == nil {
			return nil, errors.New("bilibili response missing result")
		}
		eps := array(at(r, "main_section", "episodes"))
		if len(eps) == 0 {
			eps = array(r["episodes"])
		}
		for i, a := range eps {
			m := object(a)
			if strings.HasPrefix(id, "ep") && str(m["id"]) != id[2:] {
				continue
			}
			aid, cid := str(m["aid"]), str(m["cid"])
			if aid == "" || cid == "" {
				return nil, errors.New("bilibili episode missing aid/cid")
			}
			out = append(out, Episode{ID: aid + "," + cid, Title: textFirst(m["show_title"], m["long_title"], m["title"]), URL: "https://www.bilibili.com/bangumi/play/ep" + str(m["id"]), Index: i + 1})
		}
		return out, nil
	}
	if strings.HasPrefix(id, "bvBV") {
		id = id[2:]
	}
	q := url.Values{}
	if strings.HasPrefix(id, "BV") {
		q.Set("bvid", id)
	} else if strings.HasPrefix(id, "av") {
		q.Set("aid", id[2:])
	} else {
		return nil, errors.New("bilibili media ID must start ss, ep, BV, or av")
	}
	if e := b.get(ctx, "https://api.bilibili.com/x/web-interface/view", q, &v); e != nil {
		return nil, e
	}
	if integer(v["code"]) != 0 {
		return nil, fmt.Errorf("bilibili video code %s", str(v["code"]))
	}
	r := object(v["data"])
	if r == nil {
		return nil, errors.New("bilibili response missing data")
	}
	for i, a := range array(r["pages"]) {
		m := object(a)
		if pageFilter > 0 && integer(m["page"]) != pageFilter {
			continue
		}
		if str(m["cid"]) == "" {
			return nil, errors.New("bilibili page missing cid")
		}
		out = append(out, Episode{ID: str(r["aid"]) + "," + str(m["cid"]), Title: str(m["part"]), URL: "https://www.bilibili.com/video/" + textFirst(r["bvid"], id) + "?p=" + str(m["page"]), Index: i + 1})
	}
	return out, nil
}
func (b *Bilibili) Comments(ctx context.Context, id string) ([]danmaku.Comment, error) {
	return b.coalesce(ctx, "bilibili:"+id, func(work context.Context) ([]danmaku.Comment, error) { return b.comments(work, id) })
}
func (b *Bilibili) comments(ctx context.Context, id string) ([]danmaku.Comment, error) {
	parts := strings.Split(id, ",")
	if len(parts) != 2 {
		return nil, errors.New("bilibili episode ID must be aid,cid")
	}
	for _, v := range parts {
		n, e := strconv.ParseInt(v, 10, 64)
		if e != nil || n <= 0 {
			return nil, errors.New("invalid bilibili aid/cid")
		}
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	q := url.Values{"type": {"1"}, "pid": {parts[0]}, "oid": {parts[1]}}
	raw, e := b.request(ctx, http.MethodGet, "https://api.bilibili.com/x/v2/dm/web/view", q, nil, nil)
	if e != nil {
		return nil, e
	}
	total := 0
	e = wireFields(raw, func(num int, w int, n uint64, p []byte) error {
		if num == 4 && w == 2 {
			return wireFields(p, func(f int, w int, n uint64, p []byte) error {
				if f == 2 && w == 0 {
					if n > uint64(b.maxSegments()) {
						return danmaku.ErrLimit
					}
					total = int(n)
				}
				return nil
			})
		}
		return nil
	})
	if e != nil {
		return nil, e
	}
	if total == 0 {
		return nil, errors.New("bilibili response lacks segment count; refusing incomplete result")
	}
	comments, e := b.segments(ctx, total, func(ctx context.Context, index int) ([]danmaku.Comment, error) {
		sq := url.Values{"type": {"1"}, "pid": {parts[0]}, "oid": {parts[1]}, "segment_index": {strconv.Itoa(index + 1)}}
		raw, e := b.request(ctx, http.MethodGet, "https://api.bilibili.com/x/v2/dm/web/seg.so", sq, nil, nil)
		if e != nil {
			return nil, e
		}
		return parseBiliSegment(raw)
	})
	if e != nil {
		return nil, e
	}
	out := make([]danmaku.Comment, 0, len(comments))
	seen := map[int64]bool{}
	for _, c := range comments {
		if c.T < 0 {
			continue
		}
		if c.CID != 0 && seen[c.CID] {
			continue
		}
		seen[c.CID] = true
		out = append(out, c)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].T < out[j].T })
	return out, nil
}
func parseBiliSegment(raw []byte) ([]danmaku.Comment, error) {
	out := []danmaku.Comment{}
	e := wireFields(raw, func(field, wire int, n uint64, p []byte) error {
		if field != 1 || wire != 2 {
			return nil
		}
		var id int64
		progress := 0
		mode := 1
		font := 25
		color := 0xffffff
		content := ""
		err := wireFields(p, func(f, w int, n uint64, p []byte) error {
			switch {
			case f == 1 && w == 0:
				id = int64(n)
			case f == 2 && w == 0:
				progress = int(n)
			case f == 3 && w == 0:
				mode = int(n)
			case f == 4 && w == 0:
				font = int(n)
			case f == 5 && w == 0:
				color = int(n)
			case f == 7 && w == 2:
				content = string(p)
			}
			return nil
		})
		if err != nil {
			return err
		}
		c, err := danmaku.NewComment(id, float64(progress)/1000, mode, font, color, content, "[bilibili]")
		if err != nil {
			return err
		}
		if len(out) >= 500000 {
			return danmaku.ErrLimit
		}
		out = append(out, c)
		return nil
	})
	return out, e
}
func wireFields(b []byte, fn func(int, int, uint64, []byte) error) error {
	for len(b) > 0 {
		tag, n := binary.Uvarint(b)
		if n <= 0 {
			return errors.New("invalid protobuf tag")
		}
		b = b[n:]
		field, wire := int(tag>>3), int(tag&7)
		if field <= 0 {
			return errors.New("invalid protobuf field")
		}
		var v uint64
		var p []byte
		switch wire {
		case 0:
			v, n = binary.Uvarint(b)
			if n <= 0 {
				return errors.New("invalid protobuf integer")
			}
			b = b[n:]
		case 1:
			if len(b) < 8 {
				return errors.New("truncated protobuf fixed64")
			}
			p = b[:8]
			b = b[8:]
		case 2:
			size, k := binary.Uvarint(b)
			if k <= 0 || size > uint64(len(b)-max(k, 0)) {
				return errors.New("truncated protobuf bytes")
			}
			b = b[k:]
			p = b[:int(size)]
			b = b[int(size):]
		case 5:
			if len(b) < 4 {
				return errors.New("truncated protobuf fixed32")
			}
			p = b[:4]
			b = b[4:]
		default:
			return errors.New("unsupported protobuf wire type")
		}
		if e := fn(field, wire, v, p); e != nil {
			return e
		}
	}
	return nil
}
