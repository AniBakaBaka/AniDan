// SPDX-License-Identifier: AGPL-3.0-or-later
package integration

import (
	"context"
	"encoding/json"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type sourceCacheItem struct {
	Data    []byte
	Expires time.Time
}
type EpisodeURL struct {
	Index    int    `json:"episodeIndex"`
	URL      string `json:"url"`
	Provider string `json:"provider"`
}

var so360Platforms = map[string]string{"qq": "tencent", "qiyi": "iqiyi", "youku": "youku", "bilibili1": "bilibili", "imgo": "mgtv", "migu": "migu", "sohu": "sohu", "leshi": "le", "xigua": "xigua"}

func (c *Client) cacheSource(id string, item map[string]any) {
	b, e := json.Marshal(item)
	if e != nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.sourceItems == nil {
		c.sourceItems = map[string]sourceCacheItem{}
	}
	for k, v := range c.sourceItems {
		if time.Now().After(v.Expires) {
			delete(c.sourceItems, k)
		}
	}
	if len(c.sourceItems) >= 1024 {
		for k := range c.sourceItems {
			delete(c.sourceItems, k)
			break
		}
	}
	c.sourceItems[id] = sourceCacheItem{b, time.Now().Add(3 * time.Hour)}
}
func (c *Client) cachedSource(id string) map[string]any {
	c.mu.Lock()
	v, ok := c.sourceItems[id]
	c.mu.Unlock()
	if !ok || time.Now().After(v.Expires) {
		return nil
	}
	var m map[string]any
	_ = decode(v.Data, &m)
	return m
}
func decodeJSONP(b []byte, callback string) (any, error) {
	s := strings.TrimSpace(string(b))
	if strings.HasPrefix(s, callback+"(") {
		end := strings.LastIndex(s, ")")
		if end < len(callback)+1 {
			return nil, &Error{"360", 502, "invalid JSONP"}
		}
		s = s[len(callback)+1 : end]
	}
	var v any
	if decode([]byte(s), &v) != nil {
		return nil, &Error{"360", 502, "invalid JSONP response"}
	}
	return v, nil
}
func providerURL(raw, fallback string) (string, string) {
	u, e := url.Parse(raw)
	if e != nil || u.Host == "" || u.User != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return "", ""
	}
	host := strings.ToLower(u.Hostname())
	if host == "www.hunantv.com" || host == "hunantv.com" {
		u.Host = "www.mgtv.com"
		host = "www.mgtv.com"
	}
	for domain, p := range map[string]string{"bilibili.com": "bilibili", "qq.com": "tencent", "iqiyi.com": "iqiyi", "youku.com": "youku", "mgtv.com": "mgtv", "migu.cn": "migu", "sohu.com": "sohu", "le.com": "le", "ixigua.com": "xigua"} {
		if host == domain || strings.HasSuffix(host, "."+domain) {
			return u.String(), p
		}
	}
	return u.String(), fallback
}
func episodeURLs(rows []any, p string) []EpisodeURL {
	out := []EpisodeURL{}
	for i, x := range rows {
		raw := str(x)
		if o, ok := x.(map[string]any); ok {
			raw = str(o["url"])
		}
		raw, actual := providerURL(raw, p)
		if raw != "" {
			out = append(out, EpisodeURL{i + 1, raw, actual})
		}
	}
	return out
}
func (c *Client) EpisodeURLs(ctx context.Context, metadataProvider, id, targetProvider string, item map[string]any) ([]EpisodeURL, error) {
	if metadataProvider != "360" {
		return nil, &Error{metadataProvider, 400, "episode URL supplementation is not available for this provider"}
	}
	if e := safeID(id); e != nil {
		return nil, e
	}
	if len(item) == 0 {
		item = c.cachedSource("360:" + id)
	}
	if len(item) == 0 {
		detail, e := c.so360Details(ctx, id)
		if e != nil {
			return nil, e
		}
		if detail != nil {
			if _, e = c.so360Search(ctx, detail.Title); e != nil {
				return nil, e
			}
			item = c.cachedSource("360:" + id)
		}
	}
	if len(item) == 0 {
		return nil, &Error{"360", 404, "search result context unavailable after refresh"}
	}
	site := ""
	for s, p := range so360Platforms {
		if p == targetProvider {
			site = s
			break
		}
	}
	playlinks := object(item["playlinks"])
	if targetProvider != "" && site == "" {
		return nil, &Error{"360", 400, "unsupported target source"}
	}
	if site == "" {
		keys := []string{}
		for k := range playlinks {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		if len(keys) > 0 {
			site = keys[0]
		}
	}
	if site == "" {
		return []EpisodeURL{}, nil
	}
	p := so360Platforms[site]
	series := array(item["seriesPlaylinks"])
	if len(series) > 0 && str(item["seriesSite"]) == site {
		return episodeURLs(series, p), nil
	}
	cat := str(item["cat_id"])
	if cat == "1" || ((len(series) <= 1) && cat != "2" && cat != "3" && cat != "4") {
		v := playlinks[site]
		switch x := v.(type) {
		case string:
			return episodeURLs([]any{x}, p), nil
		case map[string]any:
			return episodeURLs([]any{x}, p), nil
		case []any:
			if len(x) > 0 {
				return episodeURLs(x[:1], p), nil
			}
		}
		return []EpisodeURL{}, nil
	}
	base := c.base(ctx, "so360ApiBaseUrl", "https://api.so.360kan.com")
	ent := first(item["en_id"], item["id"], id)
	if cat == "3" || strings.Contains(str(item["cat_name"]), "综艺") {
		years := array(nested(item, "playlinks_year", site))
		if len(years) == 0 {
			years = array(item["years"])
		}
		if len(years) == 0 {
			years = []any{""}
		}
		if len(years) > 100 {
			return nil, &Error{"360", 502, "too many variety years"}
		}
		all := []any{}
		for _, yr := range years {
			for offset := 0; offset < 10000; offset += 8 {
				cb := "__jp" + strconv.Itoa(7+offset/8)
				b, e := c.request(ctx, "360", "GET", base+"/episodeszongyi", url.Values{"site": {site}, "y": {str(yr)}, "entid": {first(item["id"], id)}, "offset": {strconv.Itoa(offset)}, "count": {"8"}, "v_ap": {"1"}, "cb": {cb}}, nil, nil, 16<<20)
				if e != nil {
					return nil, e
				}
				v, e := decodeJSONP(b, cb)
				if e != nil {
					return nil, e
				}
				rows := array(nested(v, "data", "list"))
				all = append(all, rows...)
				if len(all) > 10000 {
					return nil, &Error{"360", 502, "too many episodes"}
				}
				if len(rows) < 8 {
					break
				}
				if offset+8 >= 10000 {
					return nil, &Error{"360", 502, "episode pagination limit reached"}
				}
			}
		}
		return episodeURLs(all, p), nil
	}
	query, _ := json.Marshal([]map[string]string{{"cat_id": cat, "ent_id": ent, "site": site}})
	b, e := c.request(ctx, "360", "GET", base+"/episodesv2", url.Values{"v_ap": {"1"}, "s": {string(query)}, "cb": {"__jp8"}}, nil, nil, 16<<20)
	if e != nil {
		return nil, e
	}
	v, e := decodeJSONP(b, "__jp8")
	if e != nil {
		return nil, e
	}
	rows := array(object(v)["data"])
	if len(rows) == 0 {
		return []EpisodeURL{}, nil
	}
	return episodeURLs(array(nested(rows[0], "seriesHTML", "seriesPlaylinks")), p), nil
}
func (c *Client) probe360(ctx context.Context, out []Metadata) {
	sem := make(chan struct{}, 4)
	var wg sync.WaitGroup
	for i := range out {
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			wg.Wait()
			return
		}
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()
			item := object(out[i].Extra["item_data"])
			counts := map[string]int{}
			for _, site := range []string{"qq", "qiyi", "youku", "bilibili1", "imgo"} {
				if _, ok := object(item["playlinks"])[site]; !ok {
					continue
				}
				episodes, e := c.EpisodeURLs(ctx, "360", out[i].ID, so360Platforms[site], item)
				if e == nil && len(episodes) > 0 {
					counts[episodes[0].Provider] = len(episodes)
				}
			}
			names := []string{}
			for p := range counts {
				names = append(names, p)
			}
			sort.Strings(names)
			if len(names) > 0 {
				out[i].Extra["supported_providers"] = names
				out[i].Extra["provider_episode_counts"] = counts
			}
			supported := len(names) > 0
			out[i].SupportsEpisodeURLs = &supported
		}(i)
	}
	wg.Wait()
}
