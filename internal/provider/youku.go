// SPDX-License-Identifier: AGPL-3.0-only
// Episode OpenAPI protocol from historical AGPL Misaka youku.py; the caller
// supplies its registered application ID. No historical partner key is bundled.
package provider

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/AniBakaBaka/AniDan/internal/danmaku"
)

func (l *Legacy) youkuEpisodes(ctx context.Context, id string) ([]Episode, error) {
	if l.YoukuClientID == "" {
		return nil, &UnsupportedError{l.Name(), "episodes", "registered youkuClientId must be configured"}
	}
	if !alphaID.MatchString(id) {
		return nil, errors.New("invalid Youku show ID")
	}
	const pageSize = 100
	seen := map[string]bool{}
	out := []Episode{}
	total := -1
	for page := 1; page <= l.maxSegments(); page++ {
		var v map[string]any
		q := url.Values{"client_id": {l.YoukuClientID}, "ext": {"show"}, "show_id": {id}, "page": {strconv.Itoa(page)}, "count": {strconv.Itoa(pageSize)}}
		if e := l.get(ctx, "https://openapi.youku.com/v2/shows/videos.json", q, &v); e != nil {
			return nil, e
		}
		if failure := object(v["error"]); failure != nil {
			return nil, fmt.Errorf("youku OpenAPI rejected episodes (code %d); verify registered client ID and API entitlement", integer(failure["code"]))
		}
		n, e := strconv.Atoi(str(v["total"]))
		if e != nil || n < 0 {
			return nil, errors.New("youku episode response missing valid total")
		}
		if n > 10000 || n > pageSize*l.maxSegments() {
			return nil, danmaku.ErrLimit
		}
		if total < 0 {
			total = n
		} else if n != total {
			return nil, errors.New("youku episode total changed during pagination; retry explicitly")
		}
		videos, ok := v["videos"].([]any)
		if !ok {
			return nil, errors.New("youku episode response missing videos")
		}
		want := min(pageSize, total-len(out))
		if len(videos) != want {
			return nil, errors.New("youku episode page incomplete or exceeds advertised total")
		}
		for _, item := range videos {
			m := object(item)
			vid := str(m["id"])
			if vid == "" || len(vid) > 256 || !alphaID.MatchString(vid) {
				return nil, errors.New("youku episode missing valid video ID")
			}
			if seen[vid] {
				return nil, errors.New("youku episode pagination repeated a video")
			}
			seen[vid] = true
			title := strings.TrimSpace(textFirst(m["displayName"], m["title"]))
			if title == "" {
				return nil, errors.New("youku episode missing title")
			}
			index := integer(m["seq"])
			if index < 1 {
				index = len(out) + 1
			}
			out = append(out, Episode{ID: vid, Title: title, Index: index, URL: "https://v.youku.com/v_show/id_" + url.PathEscape(vid) + ".html"})
		}
		if len(out) == total {
			return out, nil
		}
	}
	return nil, danmaku.ErrLimit
}
