// SPDX-License-Identifier: AGPL-3.0-or-later
package integration

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"time"
)

// Calendar returns the pinned upstream's normalized calendar item fields. No
// arbitrary text/URL from a provider is executed; credential-bearing redirects
// remain governed by Client's transport. Enrichment is explicit and bounded.
func (c *Client) Calendar(ctx context.Context, provider string, start time.Time, days int, cred Credential) ([]map[string]any, error) {
	if days < 1 || days > 31 {
		return nil, &Error{provider, 400, "calendar days must be 1..31"}
	}
	out := []map[string]any{}
	switch provider {
	case "bangumi":
		v, e := c.json(ctx, provider, "GET", c.base(ctx, "bangumiApiBaseUrl", "https://api.bgm.tv")+"/calendar", nil, nil, nil)
		if e != nil {
			return nil, e
		}
		for _, x := range array(v) {
			g := object(x)
			day := integer(nested(g, "weekday", "id"))
			if day < 1 || day > 7 {
				continue
			}
			for _, y := range array(g["items"]) {
				o := object(y)
				var yr any
				if n := year(o["air_date"]); n > 0 {
					yr = n
				}
				out = append(out, map[string]any{"animeTitle": first(o["name_cn"], o["name"]), "airWeekday": day, "origin": "bangumi", "isLocal": false, "bangumiId": str(o["id"]), "traktId": nil, "imageUrl": nullable(first(nested(o, "images", "common"), nested(o, "images", "medium"))), "rating": nested(o, "rating", "score"), "rank": o["rank"], "year": yr, "airDate": nullable(str(o["air_date"])), "latestEpisodeIndex": nil, "episodeCount": nil})
			}
		}
		return out, nil
	case "trakt":
		h, e := c.traktHeaders(ctx, cred, true)
		if e != nil {
			return nil, e
		}
		v, e := c.json(ctx, provider, "GET", c.base(ctx, "traktApiBaseUrl", "https://api.trakt.tv")+fmt.Sprintf("/calendars/all/shows/%s/%d", start.Format("2006-01-02"), days), nil, h, nil)
		if e != nil {
			return nil, e
		}
		seen := map[string]bool{}
		for _, x := range array(v) {
			entry := object(x)
			show := object(entry["show"])
			ids := object(show["ids"])
			id := str(ids["trakt"])
			if seen[id] {
				continue
			}
			date := str(entry["first_aired"])
			air, e := time.Parse(time.RFC3339Nano, date)
			if e != nil {
				continue
			}
			seen[id] = true
			day := int(air.Weekday())
			if day == 0 {
				day = 7
			}
			ep := object(entry["episode"])
			out = append(out, map[string]any{"animeTitle": str(show["title"]), "airWeekday": day, "origin": "trakt", "isLocal": false, "bangumiId": nil, "traktId": id, "imageUrl": nil, "traktImdbId": ids["imdb"], "traktTmdbId": ids["tmdb"], "year": show["year"], "season": ep["season"], "airDate": air.Format("2006-01-02"), "latestEpisodeIndex": ep["number"], "episodeCount": nil})
		}
		return out, nil
	default:
		return nil, &Error{provider, 400, "provider does not expose a public calendar"}
	}
}

// CalendarProgress supplies the slow progress-enrichment phase for calendar jobs.
// Bangumi counts aired normal episodes, with a bounded page count. Trakt exposes
// aired_episodes as its total-known episode count; it does not imply completion.
func (c *Client) CalendarProgress(ctx context.Context, provider, id string, now time.Time, cred Credential) (aired, total int, err error) {
	if e := safeID(id); e != nil {
		return 0, 0, e
	}
	switch provider {
	case "bangumi":
		h := bgmHeaders(cred, c.setting(ctx, "bangumiToken", ""))
		base := c.base(ctx, "bangumiApiBaseUrl", "https://api.bgm.tv")
		v, e := c.json(ctx, provider, "GET", base+"/v0/subjects/"+url.PathEscape(id), nil, h, nil)
		if e != nil {
			return 0, 0, e
		}
		total = integer(first(object(v)["total_episodes"], object(v)["eps"]))
		for offset := 0; offset < 10000; offset += 100 {
			v, e := c.json(ctx, provider, "GET", base+"/v0/episodes", url.Values{"subject_id": {id}, "type": {"0"}, "limit": {"100"}, "offset": {strconv.Itoa(offset)}}, h, nil)
			if e != nil {
				return aired, total, e
			}
			rows := array(object(v)["data"])
			for _, x := range rows {
				date := str(object(x)["airdate"])
				if len(date) >= 10 && date[:10] <= now.Format("2006-01-02") {
					aired++
				}
			}
			if len(rows) < 100 || offset+100 >= integer(object(v)["total"]) {
				return aired, total, nil
			}
		}
		return aired, total, &Error{provider, 502, "episode pagination limit reached"}
	case "trakt":
		h, e := c.traktHeaders(ctx, cred, true)
		if e != nil {
			return 0, 0, e
		}
		v, e := c.json(ctx, provider, "GET", c.base(ctx, "traktApiBaseUrl", "https://api.trakt.tv")+"/shows/"+url.PathEscape(id), url.Values{"extended": {"full"}}, h, nil)
		if e != nil {
			return 0, 0, e
		}
		total = integer(object(v)["aired_episodes"])
		return total, total, nil
	}
	return 0, 0, &Error{provider, 400, "calendar progress unsupported"}
}
