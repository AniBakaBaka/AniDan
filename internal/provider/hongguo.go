// SPDX-License-Identifier: AGPL-3.0-only
// Public web JSON protocol reference: huangxd-/danmu_api hongguo.js,
// commit fc1b7ff6add61d8af24c9bf978253273833f5afc (AGPL-3.0).
// No Android identity, signing constants, account token or private API is used.
package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/AniBakaBaka/AniDan/internal/danmaku"
)

// Find one embedded JSON value without evaluating page scripts. Structural
// depth is bounded and braces inside JSON strings do not terminate the value.
func embeddedProviderJSON(raw []byte, key string, open byte) ([]byte, error) {
	marker := []byte(`"` + key + `":`)
	start := bytes.Index(raw, marker)
	if start < 0 {
		return nil, fmt.Errorf("public page missing %s", key)
	}
	start += len(marker)
	for start < len(raw) && (raw[start] == ' ' || raw[start] == '\n' || raw[start] == '\r' || raw[start] == '\t') {
		start++
	}
	if start >= len(raw) || raw[start] != open {
		return nil, errors.New("unexpected embedded JSON shape")
	}
	quoted, escaped := false, false
	depth := 0
	for i := start; i < len(raw); i++ {
		c := raw[i]
		if quoted {
			if escaped {
				escaped = false
			} else if c == '\\' {
				escaped = true
			} else if c == '"' {
				quoted = false
			}
			continue
		}
		if c == '"' {
			quoted = true
			continue
		}
		if c == '{' || c == '[' {
			depth++
			if depth > 64 {
				return nil, danmaku.ErrLimit
			}
		}
		if c == '}' || c == ']' {
			depth--
			if depth == 0 {
				return raw[start : i+1], nil
			}
			if depth < 0 {
				return nil, errors.New("invalid embedded JSON")
			}
		}
	}
	return nil, errors.New("incomplete embedded JSON")
}

func (l *Legacy) hongguoSearch(ctx context.Context, keyword string) ([]Result, error) {
	if len(keyword) > 1024 || strings.TrimSpace(keyword) == "" {
		return nil, errors.New("invalid hongguo search query")
	}
	raw, e := l.request(ctx, http.MethodGet, "https://hongguoduanju.com/search/"+url.PathEscape(keyword), nil, nil, http.Header{"Accept": {"text/html"}})
	if e != nil {
		return nil, e
	}
	data, e := embeddedProviderJSON(raw, "searchList", '[')
	if e != nil {
		return nil, e
	}
	d := json.NewDecoder(bytes.NewReader(data))
	if _, e = d.Token(); e != nil {
		return nil, e
	}
	out := []Result{}
	seen := map[string]bool{}
	for d.More() {
		if len(out) >= 1000 {
			return nil, danmaku.ErrLimit
		}
		if e = ctx.Err(); e != nil {
			return nil, e
		}
		var item struct {
			Video struct {
				ID       scalarString    `json:"series_id"`
				Name     string          `json:"series_name"`
				Title    string          `json:"series_title"`
				Episodes int             `json:"episode_cnt"`
				Created  scalarString    `json:"create_time"`
				Cover    json.RawMessage `json:"series_cover"`
			} `json:"video_data"`
		}
		if e = d.Decode(&item); e != nil {
			return nil, e
		}
		v := item.Video
		id := string(v.ID)
		if !numericID.MatchString(id) || len(id) > 64 || v.Episodes <= 0 {
			continue
		}
		title := textFirst(v.Name, v.Title)
		if title == "" || len(title) > 4096 {
			return nil, errors.New("hongguo search missing valid title")
		}
		if seen[id] {
			continue
		}
		seen[id] = true
		var cover string
		if len(v.Cover) > 0 && v.Cover[0] == '"' {
			if e = json.Unmarshal(v.Cover, &cover); e != nil {
				return nil, e
			}
		}
		year := 0
		if stamp, err := strconv.ParseInt(string(v.Created), 10, 64); err == nil && stamp > 0 {
			year = time.Unix(stamp, 0).UTC().Year()
		}
		out = append(out, Result{ID: id, Title: title, Type: "tv_series", Year: year, ImageURL: cover})
	}
	if _, e = d.Token(); e != nil {
		return nil, e
	}
	return out, nil
}

func (l *Legacy) hongguoEpisodes(ctx context.Context, id string) ([]Episode, error) {
	if !numericID.MatchString(id) || len(id) > 64 {
		return nil, errors.New("hongguo requires numeric series ID")
	}
	raw, e := l.request(ctx, http.MethodGet, "https://hongguoduanju.com/detail", url.Values{"series_id": {id}}, nil, http.Header{"Accept": {"text/html"}})
	if e != nil {
		return nil, e
	}
	data, e := embeddedProviderJSON(raw, "seriesDetail", '{')
	if e != nil {
		return nil, e
	}
	d := json.NewDecoder(bytes.NewReader(data))
	if _, e = d.Token(); e != nil {
		return nil, e
	}
	out := []Episode{}
	seen := map[string]bool{}
	found := false
	count := -1
	matched := false
	for d.More() {
		key, e := d.Token()
		if e != nil {
			return nil, e
		}
		switch key {
		case "series_id":
			var got scalarString
			if e = d.Decode(&got); e != nil {
				return nil, e
			}
			if string(got) != id {
				return nil, errors.New("hongguo detail series ID mismatch")
			}
			matched = true
		case "episode_cnt":
			if e = d.Decode(&count); e != nil {
				return nil, e
			}
			if count < 0 || count > 10000 {
				return nil, danmaku.ErrLimit
			}
		case "vid_list":
			if found {
				return nil, errors.New("duplicate hongguo video list")
			}
			found = true
			token, e := d.Token()
			if e != nil || token != json.Delim('[') {
				return nil, errors.New("hongguo video list is not array")
			}
			for d.More() {
				if len(out) >= 10000 {
					return nil, danmaku.ErrLimit
				}
				if e = ctx.Err(); e != nil {
					return nil, e
				}
				var value json.RawMessage
				if e = d.Decode(&value); e != nil {
					return nil, e
				}
				if len(value) > 64<<10 {
					return nil, danmaku.ErrLimit
				}
				var vid scalarString
				title := ""
				if len(value) > 0 && value[0] == '{' {
					var v struct {
						ID           scalarString `json:"vid"`
						VideoID      scalarString `json:"video_id"`
						Title        string       `json:"title"`
						EpisodeTitle string       `json:"episode_title"`
					}
					if e = json.Unmarshal(value, &v); e != nil {
						return nil, e
					}
					vid = v.ID
					if vid == "" {
						vid = v.VideoID
					}
					title = textFirst(v.EpisodeTitle, v.Title)
				} else {
					if e = json.Unmarshal(value, &vid); e != nil {
						return nil, e
					}
				}
				if !numericID.MatchString(string(vid)) || len(vid) > 64 {
					return nil, errors.New("hongguo episode missing numeric video ID")
				}
				if seen[string(vid)] {
					return nil, errors.New("hongguo episode ID repeated")
				}
				seen[string(vid)] = true
				index := len(out) + 1
				if title == "" {
					title = fmt.Sprintf("第%d集", index)
				}
				out = append(out, Episode{ID: string(vid), Title: title, Index: index})
			}
			if _, e = d.Token(); e != nil {
				return nil, e
			}
		default:
			if e = xiguaDiscardJSON(d); e != nil {
				return nil, e
			}
		}
	}
	if _, e = d.Token(); e != nil {
		return nil, e
	}
	if !found || !matched {
		return nil, errors.New("hongguo detail lacks verified series/video list")
	}
	if count >= 0 && count != len(out) {
		return nil, errors.New("hongguo public episode list incomplete")
	}
	return out, nil
}
