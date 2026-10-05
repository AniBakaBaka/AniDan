// SPDX-License-Identifier: AGPL-3.0-or-later
package integration

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// Bangumi-data is CC BY 4.0, independently of AniDan's source code license.
const DatasetAttribution = "bangumi-data contributors — https://github.com/bangumi-data/bangumi-data — CC BY 4.0 (https://creativecommons.org/licenses/by/4.0/)"

type Dataset struct {
	Items    []map[string]any
	SiteMeta map[string]any
	SHA256   string
	Source   string
}

func ParseDataset(b []byte) (Dataset, error) {
	var v any
	if decode(b, &v) != nil {
		return Dataset{}, &Error{"bangumi-data", 502, "invalid JSON"}
	}
	rows := array(v)
	meta := map[string]any{}
	if o, ok := v.(map[string]any); ok {
		rows = array(o["items"])
		meta = object(o["siteMeta"])
	}
	if len(rows) == 0 || len(rows) > 500000 {
		return Dataset{}, &Error{"bangumi-data", 502, "dataset has no items or exceeds item limit"}
	}
	items := make([]map[string]any, 0, len(rows))
	for _, x := range rows {
		o := object(x)
		if str(o["title"]) != "" {
			items = append(items, o)
		}
	}
	if len(items) == 0 {
		return Dataset{}, &Error{"bangumi-data", 502, "dataset has no valid titles"}
	}
	hash := sha256.Sum256(b)
	return Dataset{Items: items, SiteMeta: meta, SHA256: hex.EncodeToString(hash[:])}, nil
}
func (c *Client) FetchDataset(ctx context.Context) (Dataset, error) {
	urls := strings.Split(c.setting(ctx, "bangumiDataUrl", "https://unpkg.com/bangumi-data@0.3/dist/data.json,https://cdn.jsdelivr.net/npm/bangumi-data@0.3/dist/data.json"), ",")
	var last error
	for _, raw := range urls {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		b, e := c.request(ctx, "bangumi-data", "GET", raw, nil, nil, nil, 64<<20)
		if e != nil {
			last = e
			continue
		}
		d, e := ParseDataset(b)
		if e != nil {
			last = e
			continue
		}
		u, _ := url.Parse(raw)
		u.RawQuery = ""
		u.Fragment = ""
		d.Source = u.String()
		return d, nil
	}
	if last == nil {
		last = &Error{"bangumi-data", 412, "no dataset URLs configured"}
	}
	return Dataset{}, last
}
func LocalDate(s string, loc *time.Location) string {
	if loc == nil {
		loc = time.UTC
	}
	t, e := time.Parse(time.RFC3339Nano, s)
	if e != nil {
		return s
	}
	return t.In(loc).Format("2006-01-02 15:04:05")
}
func LocalBroadcast(s string, loc *time.Location) string {
	return regexp.MustCompile(`\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:?\d{2})`).ReplaceAllStringFunc(s, func(v string) string { return LocalDate(v, loc) })
}
func DatasetRow(item map[string]any, id int64, loc *time.Location) map[string]any {
	tt := object(item["titleTranslate"])
	main := str(item["title"])
	all := []string{main}
	for _, lang := range []string{"zh-Hans", "zh-Hant", "en", "ja"} {
		all = append(all, stringsOf(tt[lang])...)
	}
	zh := append(stringsOf(tt["zh-Hans"]), stringsOf(tt["zh-Hant"])...)
	en := stringsOf(tt["en"])
	zhTitle, enTitle := "", ""
	if len(zh) > 0 {
		zhTitle = zh[0]
	}
	if len(en) > 0 {
		enTitle = en[0]
	}
	bgm := ""
	for _, x := range array(item["sites"]) {
		o := object(x)
		if str(o["site"]) == "bangumi" {
			bgm = str(o["id"])
			break
		}
	}
	var beginYear any
	if y := year(item["begin"]); y != 0 {
		beginYear = y
	}
	return map[string]any{"id": id, "bangumi_id": nullable(bgm), "title_main": clip(main, 500), "titles_all": strings.Join(unique(all), "\n"), "title_zh": nullable(clip(zhTitle, 500)), "title_en": nullable(clip(enTitle, 500)), "type": nullable(str(item["type"])), "begin_year": beginYear, "lang": nullable(clip(str(item["lang"]), 16)), "official_site": nullable(clip(str(item["officialSite"]), 500)), "begin_date": nullable(LocalDate(str(item["begin"]), loc)), "end_date": nullable(LocalDate(str(item["end"]), loc)), "broadcast": nullable(LocalBroadcast(str(item["broadcast"]), loc)), "comment": nullable(str(item["comment"])), "sites": item["sites"], "updated_at": time.Now().In(loc).Format("2006-01-02 15:04:05.000000")}
}
func clip(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		return string(r[:n])
	}
	return s
}
func PlatformURLs(sites []any, meta map[string]any) []map[string]any {
	out := []map[string]any{}
	seen := map[string]bool{}
	for _, x := range sites {
		o := object(x)
		site := str(o["site"])
		if seen[site] {
			continue
		}
		seen[site] = true
		m := object(meta[site])
		raw := strings.ReplaceAll(strings.ReplaceAll(str(m["urlTemplate"]), "{{id}}", "{id}"), "{id}", str(o["id"]))
		var link any
		if u, e := url.Parse(raw); e == nil && u.Host != "" && (u.Scheme == "https" || u.Scheme == "http") && u.User == nil {
			link = u.String()
		}
		out = append(out, map[string]any{"site": site, "id": o["id"], "title": m["title"], "type": m["type"], "url": link})
	}
	return out
}
