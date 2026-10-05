// SPDX-License-Identifier: AGPL-3.0-only
// Protocol facts from historical AGPL-3.0 misaka_danmu_server/iqiyi.py.
package provider

import (
	"context"
	"errors"
	"fmt"
	"github.com/AniBakaBaka/AniDan/internal/danmaku"
	"net/http"
	"net/url"
	"strconv"
)

func (l *Legacy) iqiyiEpisodes(ctx context.Context, id string) ([]Episode, error) {
	var decoded map[string]any
	// The international decode API is the historical primary endpoint. The
	// similarly named mainland host returns 404 for this route.
	if e := l.get(ctx, "https://pcw-api.iq.com/api/decode/"+url.PathEscape(id), url.Values{"platformId": {"3"}, "modeCode": {"intl"}, "langCode": {"sg"}}, &decoded); e != nil {
		return nil, e
	}
	if code := str(decoded["code"]); code != "0" && code != "A00000" {
		return nil, fmt.Errorf("iqiyi decode code %s", code)
	}
	tvid := str(decoded["data"])
	if !numericID.MatchString(tvid) {
		return nil, errors.New("iqiyi decode did not return numeric tvid")
	}
	var v map[string]any
	if e := l.get(ctx, "https://pcw-api.iqiyi.com/video/video/baseinfo/"+tvid, nil, &v); e != nil {
		return nil, e
	}
	if str(v["code"]) != "A00000" {
		return nil, fmt.Errorf("iqiyi baseinfo code %s", str(v["code"]))
	}
	r := object(v["data"])
	if r == nil {
		return nil, errors.New("iqiyi missing baseinfo")
	}
	if str(r["channelName"]) == "电影" {
		return []Episode{{ID: tvid, Title: str(r["name"]), URL: str(r["playUrl"]), Index: 1}}, nil
	}
	aid := str(r["albumId"])
	if !numericID.MatchString(aid) {
		return nil, errors.New("iqiyi missing album ID")
	}
	out := []Episode{}
	seen := map[string]bool{}
	for page := 1; page <= l.maxSegments(); page++ {
		var v map[string]any
		if e := l.get(ctx, "https://pcw-api.iqiyi.com/albums/album/avlistinfo", url.Values{"aid": {aid}, "page": {strconv.Itoa(page)}, "size": {"200"}}, &v); e != nil {
			return nil, e
		}
		list, ok := at(v, "data", "epsodelist").([]any)
		if !ok {
			return nil, errors.New("iqiyi missing episode list")
		}
		added := 0
		for _, a := range list {
			m := object(a)
			id := str(m["tvId"])
			if !numericID.MatchString(id) {
				return nil, errors.New("iqiyi invalid episode tvid")
			}
			if seen[id] {
				continue
			}
			seen[id] = true
			added++
			out = append(out, Episode{ID: id, Title: str(m["name"]), URL: str(m["playUrl"]), Index: integer(m["order"])})
		}
		if len(list) < 200 {
			return out, nil
		}
		if added == 0 {
			return nil, errors.New("iqiyi episode pagination did not advance")
		}
	}
	return nil, danmaku.ErrLimit
}
func (l *Legacy) iqiyiComments(ctx context.Context, id string) ([]danmaku.Comment, error) {
	if !numericID.MatchString(id) || len(id) < 4 {
		return nil, errors.New("iqiyi comments require numeric tvid of at least four digits")
	}
	var v map[string]any
	if e := l.get(ctx, "https://pcw-api.iqiyi.com/video/video/baseinfo/"+id, nil, &v); e != nil {
		return nil, e
	}
	duration := integer(at(v, "data", "durationSec"))
	if str(v["code"]) != "A00000" || duration <= 0 {
		return nil, errors.New("iqiyi duration unavailable; refusing guessed download")
	}
	// Check before addition to avoid overflow on malformed upstream duration.
	if duration > l.maxSegments()*300 {
		return nil, danmaku.ErrLimit
	}
	count := (duration + 299) / 300
	totalBytes := int64(0)
	out := []danmaku.Comment{}
	workers := l.iqiyiFetchWorkers()
	for start := 1; start <= count; start += workers {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		n := min(workers, count-start+1)
		// Only compressed responses are staged. Ordered consumption has one XML
		// decoder and one canonical output, never one decoded pool per worker.
		compressed := make([][]byte, n)
		_, err := l.segments(ctx, n, func(work context.Context, index int) ([]danmaku.Comment, error) {
			seg := start + index
			raw, e := l.request(work, http.MethodGet, fmt.Sprintf("https://cmts.iqiyi.com/bullet/%s/%s/%s_300_%d.z", id[len(id)-4:len(id)-2], id[len(id)-2:], id, seg), nil, nil, nil)
			if e != nil {
				var he *HTTPError
				if errors.As(e, &he) && he.Status == 404 {
					return nil, nil
				}
				return nil, e
			}
			compressed[index] = raw
			return nil, nil
		})
		if err != nil {
			return nil, err
		}
		for i, raw := range compressed {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if raw == nil {
				continue
			} // A real404 is a gap, not end-of-stream.
			err = l.scanIQiyiSegment(ctx, raw, func(c danmaku.Comment) error {
				var e error
				out, e = l.appendBudget(out, &totalBytes, c)
				return e
			})
			compressed[i] = nil
			if err != nil {
				return nil, err
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return mergeCanonical(out)
}
