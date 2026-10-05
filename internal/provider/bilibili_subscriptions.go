// SPDX-License-Identifier: AGPL-3.0-only
// Read-only public API contracts: api.bilibili.com x/space/wbi/arc/search and
// x/v3/fav/resource/list. No account mutations or borrowed credentials.
package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/AniBakaBaka/AniDan/internal/danmaku"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type subscriptionVideoWire struct {
	BVID       string       `json:"bvid"`
	SeasonID   scalarString `json:"season_id"`
	SeasonType int          `json:"season_type"`
	Title      string       `json:"title"`
	Author     string       `json:"author"`
	Pic        string       `json:"pic"`
	Cover      string       `json:"cover"`
	Type       int          `json:"type"`
	Attr       int          `json:"attr"`
}
type subscriptionVideoPage []subscriptionVideoWire

func (p *subscriptionVideoPage) UnmarshalJSON(raw []byte) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	tok, e := d.Token()
	if e != nil || tok != json.Delim('[') {
		return errors.New("invalid subscription video page")
	}
	out := subscriptionVideoPage{}
	for d.More() {
		if len(out) >= 30 {
			return danmaku.ErrLimit
		}
		var v subscriptionVideoWire
		if e = d.Decode(&v); e != nil {
			return e
		}
		if len(v.Title) > 16384 || len(v.BVID) > 128 || len(v.Cover) > 8192 || len(v.Pic) > 8192 || len(v.Author) > 2048 {
			return danmaku.ErrLimit
		}
		out = append(out, v)
	}
	if _, e = d.Token(); e != nil {
		return e
	}
	*p = out
	return nil
}

type SubscriptionVideos struct {
	Title    string
	Cover    string
	Items    []Result
	Complete bool
	Skipped  int
}

func (b *Bilibili) ListSubscriptionVideos(ctx context.Context, kind, id string, all bool) (SubscriptionVideos, error) {
	out := SubscriptionVideos{Items: []Result{}}
	if !numericID.MatchString(id) || len(id) > 20 {
		return out, errors.New("numeric UP/favorite ID required")
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	if kind == "bilibili_watchlist" || kind == "bilibili_drama_watchlist" {
		return b.listWatchlist(ctx, kind, id, all)
	}
	key := ""
	var e error
	switch kind {
	case "bilibili_up":
		key, e = b.wbi(ctx)
		if e != nil {
			return out, e
		}
		out.Title = "UP 主 " + id
	case "bilibili_favorites":
		out.Title = "收藏夹 " + id
	default:
		return out, errors.New("unsupported subscription listing type")
	}
	seen := map[string]bool{}
	scanned := 0
	expected := -1
	for page := 1; page <= b.maxSegments(); page++ {
		var wire struct {
			Code int `json:"code"`
			Data *struct {
				List *struct {
					Videos subscriptionVideoPage `json:"vlist"`
				} `json:"list"`
				Page *struct {
					Count int `json:"count"`
				} `json:"page"`
				Info *struct {
					Title string `json:"title"`
					Cover string `json:"cover"`
					Count int    `json:"media_count"`
				} `json:"info"`
				HasMore *bool                  `json:"has_more"`
				Medias  *subscriptionVideoPage `json:"medias"`
			} `json:"data"`
		}
		q := url.Values{"pn": {strconv.Itoa(page)}}
		endpoint := "https://api.bilibili.com/x/v3/fav/resource/list"
		if kind == "bilibili_up" {
			endpoint = "https://api.bilibili.com/x/space/wbi/arc/search"
			q.Set("mid", id)
			q.Set("ps", "30")
			q.Set("order", "pubdate")
			signWBI(q, key, time.Now().Unix())
		} else {
			q.Set("media_id", id)
			q.Set("ps", "20")
			q.Set("order", "mtime")
			q.Set("platform", "web")
		}
		h := b.HTTPProvider
		if h.MaxBytes <= 0 || h.MaxBytes > 4<<20 {
			h.MaxBytes = 4 << 20
		}
		raw, e := h.request(ctx, "GET", endpoint, q, nil, nil)
		if e != nil {
			return out, e
		}
		// A page cannot legitimately contain an unbounded metadata array.
		if len(raw) > 4<<20 {
			return out, danmaku.ErrLimit
		}
		if e = json.Unmarshal(raw, &wire); e != nil {
			return out, e
		}
		if wire.Code != 0 {
			return out, fmt.Errorf("bilibili subscription API code %d; authentication or platform access may be required", wire.Code)
		}
		if wire.Data == nil {
			return out, errors.New("bilibili listing missing data")
		}
		videos := []Result{}
		count := 0
		more := false
		if kind == "bilibili_up" {
			if wire.Data.List == nil || wire.Data.Page == nil {
				return out, errors.New("bilibili UP response missing list/count")
			}
			if expected >= 0 && expected != wire.Data.Page.Count {
				return out, errors.New("UP uploads changed during pagination; retry scan")
			}
			expected = wire.Data.Page.Count
			if expected < 0 || len(wire.Data.List.Videos) > 30 {
				return out, errors.New("invalid UP pagination")
			}
			count = len(wire.Data.List.Videos)
			more = scanned+count < expected
			for _, v := range wire.Data.List.Videos {
				if page == 1 && out.Title == "UP 主 "+id && v.Author != "" {
					out.Title = v.Author
				}
				videos = append(videos, Result{ID: v.BVID, Title: v.Title, Type: "other", ImageURL: v.Pic, Season: 1})
			}
		} else {
			if wire.Data.Info == nil || wire.Data.HasMore == nil {
				return out, errors.New("bilibili favorite response missing info/has_more")
			}
			if wire.Data.Info.Title != "" {
				out.Title = wire.Data.Info.Title
			}
			out.Cover = wire.Data.Info.Cover
			if expected >= 0 && expected != wire.Data.Info.Count {
				return out, errors.New("favorite count changed during pagination; retry scan")
			}
			expected = wire.Data.Info.Count
			if expected < 0 {
				return out, errors.New("invalid favorite count")
			}
			more = *wire.Data.HasMore
			if wire.Data.Medias == nil {
				if more || wire.Data.Info.Count > 0 {
					return out, errors.New("favorite list missing media entries")
				}
			} else {
				count = len(*wire.Data.Medias)
				if count > 20 {
					return out, errors.New("favorite page exceeds requested page size")
				}
				for _, v := range *wire.Data.Medias {
					if v.Type != 2 || v.Attr != 0 {
						out.Skipped++
						continue
					}
					videos = append(videos, Result{ID: v.BVID, Title: v.Title, Type: "other", ImageURL: v.Cover, Season: 1})
				}
			}
		}
		for _, v := range videos {
			if !strings.HasPrefix(v.ID, "BV") || !alphaID.MatchString(v.ID) || v.Title == "" {
				return out, errors.New("listing contains an invalid video identity/title")
			}
			if seen[v.ID] {
				return out, errors.New("bilibili subscription pagination repeated a video")
			}
			seen[v.ID] = true
			out.Items = append(out.Items, v)
		}
		scanned += count
		if len(out.Items) > 10000 {
			return out, danmaku.ErrLimit
		}
		if !more {
			if expected >= 0 && scanned != expected {
				return out, errors.New("bilibili listing total mismatch")
			}
			out.Complete = true
			return out, nil
		}
		if count == 0 {
			return out, errors.New("bilibili pagination did not advance")
		}
		if !all {
			return out, nil
		}
	}
	return out, danmaku.ErrLimit
}

func (b *Bilibili) listWatchlist(ctx context.Context, kind, id string, all bool) (SubscriptionVideos, error) {
	out := SubscriptionVideos{Items: []Result{}, Title: "追番列表 " + id}
	typ := "1"
	if kind == "bilibili_drama_watchlist" {
		typ = "2"
		out.Title = "追剧列表 " + id
	}
	seen := map[string]bool{}
	expected := -1
	for page := 1; page <= b.maxSegments(); page++ {
		var wire struct {
			Code int `json:"code"`
			Data *struct {
				List  *subscriptionVideoPage `json:"list"`
				Total *int                   `json:"total"`
			} `json:"data"`
		}
		h := b.HTTPProvider
		if h.MaxBytes <= 0 || h.MaxBytes > 4<<20 {
			h.MaxBytes = 4 << 20
		}
		raw, e := h.request(ctx, "GET", "https://api.bilibili.com/x/space/bangumi/follow/list", url.Values{"vmid": {id}, "type": {typ}, "pn": {strconv.Itoa(page)}, "ps": {"30"}}, nil, nil)
		if e != nil {
			return out, e
		}
		if len(raw) > 4<<20 {
			return out, danmaku.ErrLimit
		}
		if e = json.Unmarshal(raw, &wire); e != nil {
			return out, e
		}
		if wire.Code != 0 {
			return out, fmt.Errorf("bilibili watchlist code %d; private lists require the owner's configured account", wire.Code)
		}
		if wire.Data == nil || wire.Data.Total == nil || wire.Data.List == nil || *wire.Data.Total < 0 {
			return out, errors.New("watchlist missing list/total")
		}
		if expected >= 0 && expected != *wire.Data.Total {
			return out, errors.New("watchlist changed during pagination; retry scan")
		}
		expected = *wire.Data.Total
		for _, v := range *wire.Data.List {
			sid := string(v.SeasonID)
			if !numericID.MatchString(sid) || seen[sid] || v.Title == "" {
				return out, errors.New("invalid or repeated watchlist season")
			}
			seen[sid] = true
			t := "tv_series"
			if v.SeasonType == 2 {
				t = "movie"
			}
			out.Items = append(out.Items, Result{ID: "ss" + sid, Title: v.Title, ImageURL: v.Cover, Type: t, Season: 1})
		}
		if len(out.Items) > 10000 {
			return out, danmaku.ErrLimit
		}
		if len(out.Items) == expected {
			out.Complete = true
			return out, nil
		}
		if len(out.Items) > expected || len(*wire.Data.List) == 0 {
			return out, errors.New("watchlist pagination did not match total")
		}
		if !all {
			return out, nil
		}
	}
	return out, danmaku.ErrLimit
}
