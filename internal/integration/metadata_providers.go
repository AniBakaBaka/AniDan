// SPDX-License-Identifier: AGPL-3.0-or-later
package integration

import (
	"context"
	"html"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"
)

func bgmHeaders(cred Credential, personal string) map[string]string {
	token := cred.AccessToken
	if token == "" {
		token = personal
	}
	h := map[string]string{}
	if token != "" {
		h["Authorization"] = "Bearer " + token
	}
	return h
}
func cleanMovie(s string) string {
	for _, v := range []string{"剧场版", "劇場版", "电影版", "電影版"} {
		s = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(s), v))
	}
	return s
}
func hasCJK(s string) bool {
	for _, r := range s {
		if unicode.In(r, unicode.Han) {
			return true
		}
	}
	return false
}
func (c *Client) bgmItem(ctx context.Context, o map[string]any, details bool) Metadata {
	id := str(o["id"])
	m := Metadata{ID: id, BangumiID: id, Title: cleanMovie(first(o["name_cn"], o["name"])), Type: "tv_series", NameJp: cleanMovie(str(o["name"])), Year: year(o["date"]), AliasesCn: unique([]string{cleanMovie(str(o["name_cn"]))})}
	images := object(o["images"])
	m.ImageURL = first(images["large"], images["common"], images["medium"], images["small"], images["grid"])
	m.ImageURL = strings.Replace(m.ImageURL, "https://lain.bgm.tv", c.base(ctx, "bangumiImageBaseUrl", "https://lain.bgm.tv"), 1)
	if details {
		if integer(o["eps"]) == 1 || cleanMovie(first(o["name_cn"], o["name"])) != first(o["name_cn"], o["name"]) {
			m.Type = "movie"
		}
		parts := []string{}
		if date := str(o["date"]); date != "" {
			if t, e := time.Parse("2006-01-02", date); e == nil {
				parts = append(parts, t.Format("2006年01月02日"))
			} else {
				parts = append(parts, date)
			}
		}
		staff := map[string]string{}
		for _, x := range array(o["infobox"]) {
			b := object(x)
			k := str(b["key"])
			v := b["value"]
			values := []string{}
			if s, ok := v.(string); ok {
				values = strings.Split(s, "/")
			} else {
				for _, a := range array(v) {
					values = append(values, str(object(a)["v"]))
				}
			}
			if k == "英文名" {
				m.NameEn = cleanMovie(str(v))
			}
			if k == "罗马字" {
				m.NameRomaji = cleanMovie(str(v))
			}
			if k == "别名" {
				for _, a := range values {
					if hasCJK(a) {
						m.AliasesCn = append(m.AliasesCn, cleanMovie(a))
					}
				}
			}
			staff[k] = strings.Join(unique(values), "、")
		}
		for _, k := range []string{"导演", "原作", "脚本", "人物设定", "系列构成", "总作画监督"} {
			if staff[k] != "" && len(parts) < 5 {
				parts = append(parts, staff[k])
			}
		}
		m.Details = strings.Join(parts, " / ")
		m.AliasesCn = unique(m.AliasesCn)
	}
	return m
}
func (c *Client) bangumiSearch(ctx context.Context, q string, cred Credential) ([]Metadata, error) {
	v, e := c.json(ctx, "bangumi", "POST", c.base(ctx, "bangumiApiBaseUrl", "https://api.bgm.tv")+"/v0/search/subjects", nil, bgmHeaders(cred, c.setting(ctx, "bangumiToken", "")), map[string]any{"keyword": q, "filter": map[string]any{"type": []int{2}}})
	if e != nil {
		return nil, e
	}
	rows := array(object(v)["data"])
	out := []Metadata{}
	var name, cn string
	for i, x := range rows {
		o := object(x)
		if i == 0 {
			name = cleanMovie(str(o["name"]))
			cn = cleanMovie(str(o["name_cn"]))
		}
		if i > 0 && !((name != "" && strings.Contains(cleanMovie(str(o["name"])), name)) || (cn != "" && strings.Contains(cleanMovie(str(o["name_cn"])), cn))) {
			continue
		}
		out = append(out, c.bgmItem(ctx, o, false))
	}
	return out, nil
}
func (c *Client) bangumiDetails(ctx context.Context, id string, cred Credential) (*Metadata, error) {
	v, e := c.json(ctx, "bangumi", "GET", c.base(ctx, "bangumiApiBaseUrl", "https://api.bgm.tv")+"/v0/subjects/"+url.PathEscape(id), nil, bgmHeaders(cred, c.setting(ctx, "bangumiToken", "")), nil)
	if e != nil {
		return nil, e
	}
	m := c.bgmItem(ctx, object(v), true)
	return &m, nil
}

func (c *Client) tmdbRequest(ctx context.Context, path string, q url.Values) (any, error) {
	key := c.setting(ctx, "tmdbApiKey", "")
	if key == "" {
		return nil, &Error{"tmdb", 412, "API key not configured"}
	}
	base := c.base(ctx, "tmdbApiBaseUrl", "https://api.themoviedb.org/3")
	if !strings.HasSuffix(base, "/3") {
		base += "/3"
	}
	if q == nil {
		q = url.Values{}
	}
	q.Set("api_key", key)
	q.Set("language", "zh-CN")
	return c.json(ctx, "tmdb", "GET", base+path, q, nil, nil)
}
func (c *Client) tmdbImage(ctx context.Context, p any) string {
	path := str(p)
	if path == "" {
		return ""
	}
	base := c.base(ctx, "tmdbImageBaseUrl", "https://image.tmdb.org/t/p/w500")
	if !strings.Contains(base, "/t/p/") {
		base += "/t/p/w500"
	}
	return base + path
}
func (c *Client) tmdbSearch(ctx context.Context, q, typ string) ([]Metadata, error) {
	if typ != "tv" && typ != "movie" && typ != "multi" {
		return nil, &Error{"tmdb", 400, "mediaType must be tv, movie or multi"}
	}
	v, e := c.tmdbRequest(ctx, "/search/"+typ, url.Values{"query": {q}})
	if e != nil {
		return nil, e
	}
	out := []Metadata{}
	for _, x := range array(object(v)["results"]) {
		o := object(x)
		t := typ
		if t == "multi" {
			t = str(o["media_type"])
		}
		if t != "tv" && t != "movie" {
			continue
		}
		title, date := o["title"], o["release_date"]
		if t == "tv" {
			title, date = o["name"], o["first_air_date"]
		}
		id := str(o["id"])
		out = append(out, Metadata{ID: id, TMDBID: id, Title: str(title), Type: t, Year: year(date), ImageURL: c.tmdbImage(ctx, o["poster_path"]), Details: first(date, "未知年份") + " / " + first(o["original_language"], "N/A")})
	}
	return out, nil
}
func (c *Client) tmdbDetails(ctx context.Context, id, typ string) (*Metadata, error) {
	if typ != "tv" && typ != "movie" {
		return nil, &Error{"tmdb", 400, "mediaType must be tv or movie"}
	}
	v, e := c.tmdbRequest(ctx, "/"+typ+"/"+url.PathEscape(id), url.Values{"append_to_response": {"alternative_titles,translations,external_ids"}})
	if e != nil {
		return nil, e
	}
	o := object(v)
	m := Metadata{ID: str(o["id"]), TMDBID: str(o["id"]), Title: first(o["name"], o["title"]), ImageURL: c.tmdbImage(ctx, o["poster_path"]), Details: str(o["overview"]), IMDBID: str(nested(o, "external_ids", "imdb_id")), TVDBID: str(nested(o, "external_ids", "tvdb_id")), Year: year(first(o["first_air_date"], o["release_date"]))}
	for _, x := range array(nested(o, "translations", "translations")) {
		a := object(x)
		t := first(nested(a, "data", "name"), nested(a, "data", "title"))
		switch str(a["iso_639_1"]) {
		case "en":
			if m.NameEn == "" {
				m.NameEn = t
			}
		case "ja":
			if m.NameJp == "" {
				m.NameJp = t
			}
			m.AliasesJp = append(m.AliasesJp, t)
		case "zh":
			m.AliasesCn = append(m.AliasesCn, t)
		}
	}
	alts := array(nested(o, "alternative_titles", "results"))
	if alts == nil {
		alts = array(nested(o, "alternative_titles", "titles"))
	}
	for _, x := range alts {
		a := object(x)
		t := str(a["title"])
		switch str(a["iso_3166_1"]) {
		case "CN", "HK", "TW", "SG":
			m.AliasesCn = append(m.AliasesCn, t)
		case "JP":
			if str(a["type"]) == "Romaji" && !hasCJK(t) {
				if m.NameRomaji == "" {
					m.NameRomaji = t
				}
			} else {
				m.AliasesJp = append(m.AliasesJp, t)
				if m.NameJp == "" {
					m.NameJp = t
				}
			}
		case "US", "GB":
			if m.NameEn == "" {
				m.NameEn = t
			}
		}
	}
	m.AliasesCn = unique(m.AliasesCn)
	m.AliasesJp = unique(m.AliasesJp)
	sort.Strings(m.AliasesCn)
	sort.Strings(m.AliasesJp)
	if typ == "tv" {
		for _, x := range array(o["seasons"]) {
			a := object(x)
			m.Seasons = append(m.Seasons, Season{AirDate: ptr(str(a["air_date"])), EpisodeCount: integer(a["episode_count"]), ID: int64(integer(a["id"])), Name: str(a["name"]), SeasonNumber: integer(a["season_number"]), PosterPath: ptr(str(a["poster_path"])), Aliases: []string{}})
		}
	}
	return &m, nil
}
func (c *Client) TMDBAction(ctx context.Context, action string, payload map[string]any) (any, error) {
	switch action {
	case "get_episode_groups":
		id := first(payload["tmdbId"], payload["tmdb_id"])
		if e := safeID(id); e != nil {
			return nil, e
		}
		return c.tmdbRequest(ctx, "/tv/"+id+"/episode_groups", nil)
	case "get_all_episodes", "update_mappings":
		id := first(payload["egid"], payload["groupId"], payload["episodeGroupId"], payload["group_id"])
		if e := safeID(id); e != nil {
			return nil, e
		}
		return c.tmdbRequest(ctx, "/tv/episode_group/"+id, nil)
	}
	return nil, &Error{"tmdb", 400, "unknown action"}
}

func (c *Client) tvdbToken(ctx context.Context) (string, error) {
	key := c.setting(ctx, "tvdbApiKey", "")
	c.tvdbMu.Lock()
	defer c.tvdbMu.Unlock()
	if c.tvdb.key == key && time.Until(c.tvdb.expiry) > 7*24*time.Hour {
		return c.tvdb.value, nil
	}
	cached := c.setting(ctx, "tvdbJwtToken", "")
	expiry := c.setting(ctx, "tvdbTokenExpiresAt", "")
	if cached != "" && expiry != "" {
		var expires time.Time
		for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04:05.999999999", "2006-01-02 15:04:05.999999999"} {
			if t, e := time.Parse(layout, expiry); e == nil {
				expires = t
				break
			}
		}
		if time.Until(expires) > 7*24*time.Hour {
			c.tvdb = tokenCache{cached, key, expires}
			return cached, nil
		}
	}
	if key == "" {
		return "", &Error{"tvdb", 412, "API key not configured"}
	}
	v, e := c.json(ctx, "tvdb", "POST", c.base(ctx, "tvdbApiBaseUrl", "https://api4.thetvdb.com/v4")+"/login", nil, nil, map[string]any{"apikey": key})
	if e != nil {
		return "", e
	}
	token := str(nested(v, "data", "token"))
	if token == "" {
		return "", &Error{"tvdb", 502, "login response omitted token"}
	}
	expires := time.Now().Add(29 * 24 * time.Hour)
	if c.SetSettings != nil {
		if e = c.SetSettings(ctx, map[string]string{"tvdbJwtToken": token, "tvdbTokenExpiresAt": expires.UTC().Format(time.RFC3339Nano)}); e != nil {
			return "", &Error{"tvdb", 500, "could not persist refreshed token"}
		}
	}
	c.tvdb = tokenCache{token, key, expires}
	return token, nil
}
func (c *Client) tvdbRequest(ctx context.Context, path string, q url.Values) (any, error) {
	token, e := c.tvdbToken(ctx)
	if e != nil {
		return nil, e
	}
	return c.json(ctx, "tvdb", "GET", c.base(ctx, "tvdbApiBaseUrl", "https://api4.thetvdb.com/v4")+path, q, map[string]string{"Authorization": "Bearer " + token}, nil)
}
func (c *Client) tvdbSearch(ctx context.Context, q, typ string) ([]Metadata, error) {
	params := url.Values{"query": {q}}
	if typ != "" {
		params.Set("type", typ)
	}
	v, e := c.tvdbRequest(ctx, "/search", params)
	if e != nil {
		return nil, e
	}
	out := []Metadata{}
	for _, x := range array(object(v)["data"]) {
		o := object(x)
		t := str(o["type"])
		if t == "series" {
			t = "tv_series"
		}
		id := str(o["tvdb_id"])
		out = append(out, Metadata{ID: id, TVDBID: id, Title: str(o["name"]), Type: t, ImageURL: str(o["image_url"]), Details: "Year: " + str(o["year"])})
	}
	return out, nil
}
func (c *Client) tvdbDetails(ctx context.Context, id, typ string) (*Metadata, error) {
	entities := []string{"movies", "series"}
	if typ == "movie" {
		entities = []string{"movies"}
	}
	if typ == "series" || typ == "tv_series" || typ == "tv" {
		entities = []string{"series"}
	}
	var last error
	for _, entity := range entities {
		v, e := c.tvdbRequest(ctx, "/"+entity+"/"+url.PathEscape(id)+"/extended", nil)
		if e != nil {
			last = e
			if Status(e) == 404 {
				continue
			}
			return nil, e
		}
		o := object(object(v)["data"])
		if len(o) == 0 {
			continue
		}
		t := "tv_series"
		if entity == "movies" {
			t = "movie"
		}
		m := Metadata{ID: str(o["id"]), TVDBID: str(o["id"]), Title: str(o["name"]), ImageURL: str(o["image"]), Details: str(o["overview"]), Year: year(o["year"]), Type: t}
		for _, x := range array(o["remoteIds"]) {
			a := object(x)
			if str(a["sourceName"]) == "IMDB" {
				m.IMDBID = str(a["id"])
			}
		}
		return &m, nil
	}
	if last == nil {
		last = &Error{"tvdb", 404, "item not found"}
	}
	return nil, last
}

func imdbType(t string) string {
	switch t {
	case "movie", "tvMovie", "feature", "video":
		return "movie"
	case "tvSeries", "tvMiniSeries", "tvSpecial":
		return "tv"
	}
	return ""
}
func (c *Client) imdbItem(o map[string]any) Metadata {
	id := str(o["id"])
	return Metadata{ID: id, IMDBID: id, Title: first(o["primaryTitle"], o["originalTitle"]), Type: imdbType(str(o["type"])), Year: year(o["startYear"]), ImageURL: str(nested(o, "primaryImage", "url")), Details: str(o["plot"])}
}
func (c *Client) imdbSearch(ctx context.Context, q, typ string) ([]Metadata, error) {
	api := c.setting(ctx, "imdbUseApi", "true") == "true"
	var e error
	var out []Metadata
	for i := 0; i < 2; i++ {
		out, e = c.imdbSearchMode(ctx, q, typ, api)
		if e == nil || i == 1 || c.setting(ctx, "imdbEnableFallback", "true") != "true" {
			return out, e
		}
		api = !api
	}
	return out, e
}
func (c *Client) imdbSearchMode(ctx context.Context, q, typ string, api bool) ([]Metadata, error) {
	out := []Metadata{}
	if api {
		v, e := c.json(ctx, "imdb", "GET", c.base(ctx, "imdbApiBaseUrl", "https://api.imdbapi.dev")+"/search/titles", url.Values{"query": {q}, "limit": {"20"}}, nil, nil)
		if e != nil {
			return nil, e
		}
		for _, x := range array(object(v)["titles"]) {
			m := c.imdbItem(object(x))
			if typ == "" || m.Type == typ {
				out = append(out, m)
			}
		}
		return out, nil
	}
	v, e := c.json(ctx, "imdb", "GET", c.base(ctx, "imdbSuggestionBaseUrl", "https://v3.sg.media-imdb.com")+"/suggestion/titles/x/"+url.PathEscape(strings.ToLower(q))+".json", nil, nil, nil)
	if e != nil {
		return nil, e
	}
	for _, x := range array(object(v)["d"]) {
		o := object(x)
		if imdbType(str(o["q"])) == "" {
			continue
		}
		out = append(out, Metadata{ID: str(o["id"]), IMDBID: str(o["id"]), Title: str(o["l"]), ImageURL: str(nested(o, "i", "imageUrl")), Details: "年份: " + str(o["y"]) + " / 演员: " + str(o["s"])})
	}
	return out, nil
}
func (c *Client) imdbDetails(ctx context.Context, id, typ string) (*Metadata, error) {
	if !regexp.MustCompile(`^tt\d+$`).MatchString(id) {
		return nil, &Error{"imdb", 400, "invalid IMDb ID"}
	}
	api := c.setting(ctx, "imdbUseApi", "true") == "true"
	var e error
	var m *Metadata
	for i := 0; i < 2; i++ {
		m, e = c.imdbDetailsMode(ctx, id, api)
		if e == nil || i == 1 || c.setting(ctx, "imdbEnableFallback", "true") != "true" {
			return m, e
		}
		api = !api
	}
	return m, e
}
func (c *Client) imdbDetailsMode(ctx context.Context, id string, api bool) (*Metadata, error) {
	if api {
		base := c.base(ctx, "imdbApiBaseUrl", "https://api.imdbapi.dev")
		v, e := c.json(ctx, "imdb", "GET", base+"/titles/"+id, nil, nil, nil)
		if e != nil {
			return nil, e
		}
		o := object(v)
		m := c.imdbItem(o)
		m.NameEn = first(o["primaryTitle"], o["originalTitle"])
		a, e := c.json(ctx, "imdb", "GET", base+"/titles/"+id+"/akas", nil, nil, nil)
		if e != nil {
			return nil, e
		}
		for _, x := range array(object(a)["akas"]) {
			m.AliasesCn = append(m.AliasesCn, str(object(x)["text"]))
		}
		if str(o["originalTitle"]) != str(o["primaryTitle"]) {
			m.AliasesCn = append(m.AliasesCn, str(o["originalTitle"]))
		}
		m.AliasesCn = unique(m.AliasesCn)
		return &m, nil
	}
	b, e := c.request(ctx, "imdb", "GET", c.base(ctx, "imdbWebBaseUrl", "https://www.imdb.com")+"/title/"+id+"/", nil, nil, nil, 16<<20)
	if e != nil {
		return nil, e
	}
	next := regexp.MustCompile(`(?is)<script[^>]*id=["']__NEXT_DATA__["'][^>]*>(.*?)</script>`).FindSubmatch(b)
	if len(next) > 1 {
		var data any
		if decode(next[1], &data) == nil {
			o := object(nested(data, "props", "pageProps", "mainColumnData"))
			title := str(nested(o, "titleText", "text"))
			if title != "" {
				m := Metadata{ID: id, IMDBID: id, Title: title, NameEn: title, Year: year(nested(o, "releaseYear", "year"))}
				for _, x := range array(nested(o, "akas", "edges")) {
					m.AliasesCn = append(m.AliasesCn, str(nested(x, "node", "text")))
				}
				original := str(nested(o, "originalTitleText", "text"))
				if original != title {
					m.AliasesCn = append(m.AliasesCn, original)
				}
				m.AliasesCn = unique(m.AliasesCn)
				return &m, nil
			}
		}
	}
	re := regexp.MustCompile(`(?is)<script[^>]*type=["']application/ld\+json["'][^>]*>(.*?)</script>`)
	match := re.FindSubmatch(b)
	if len(match) < 2 {
		return nil, &Error{"imdb", 502, "title metadata not found in HTML"}
	}
	var raw any
	if decode(match[1], &raw) != nil {
		return nil, &Error{"imdb", 502, "invalid title metadata"}
	}
	o := object(raw)
	m := Metadata{ID: id, IMDBID: id, Title: html.UnescapeString(str(o["name"])), NameEn: html.UnescapeString(str(o["name"])), Year: year(o["datePublished"]), ImageURL: str(o["image"]), Details: html.UnescapeString(str(o["description"])), AliasesCn: unique([]string{str(o["alternateName"])})}
	return &m, nil
}

func (c *Client) doubanHeaders(ctx context.Context) map[string]string {
	h := map[string]string{"Referer": "https://movie.douban.com/"}
	if cookie := c.setting(ctx, "doubanCookie", ""); cookie != "" {
		h["Cookie"] = cookie
	}
	return h
}
func (c *Client) doubanSearch(ctx context.Context, q string) ([]Metadata, error) {
	out := []Metadata{}
	seen := map[string]bool{}
	var last error
	for _, t := range []string{"movie", "tv"} {
		v, e := c.json(ctx, "douban", "GET", c.base(ctx, "doubanApiBaseUrl", "https://movie.douban.com")+"/j/search_subjects", url.Values{"type": {t}, "tag": {q}, "page_limit": {"20"}, "page_start": {"0"}}, c.doubanHeaders(ctx), nil)
		if e != nil {
			last = e
			continue
		}
		for _, x := range array(object(v)["subjects"]) {
			o := object(x)
			id := str(o["id"])
			if seen[id] {
				continue
			}
			seen[id] = true
			typ := t
			if typ == "tv" {
				typ = "tv_series"
			}
			f := false
			out = append(out, Metadata{ID: id, DoubanID: id, Title: str(o["title"]), Type: typ, ImageURL: str(o["cover"]), Details: "评分: " + str(o["rate"]), SupportsEpisodeURLs: &f})
		}
	}
	if len(out) == 0 && last != nil {
		return nil, last
	}
	return out, nil
}
func capture(pattern, s string) string {
	m := regexp.MustCompile(pattern).FindStringSubmatch(s)
	if len(m) > 1 {
		return html.UnescapeString(strings.TrimSpace(m[1]))
	}
	return ""
}
func (c *Client) doubanDetails(ctx context.Context, id string) (*Metadata, error) {
	b, e := c.request(ctx, "douban", "GET", c.base(ctx, "doubanApiBaseUrl", "https://movie.douban.com")+"/subject/"+url.PathEscape(id)+"/", nil, c.doubanHeaders(ctx), nil, 16<<20)
	if e != nil {
		return nil, e
	}
	s := string(b)
	title := capture(`(?is)<span[^>]*property=["']v:itemreviewed["'][^>]*>(.*?)</span>`, s)
	if title == "" {
		return nil, &Error{"douban", 502, "subject metadata not found in HTML"}
	}
	aliases := strings.Split(capture(`(?is)<span[^>]*class=["']pl["'][^>]*>又名:</span>(.*?)<br\s*/?>`, s), "/")
	f := false
	m := Metadata{ID: id, DoubanID: id, Title: title, IMDBID: capture(`https://www\.imdb\.com/title/(tt\d+)`, s), Year: year(capture(`<span class="year">\((\d{4})\)</span>`, s)), AliasesCn: unique(append([]string{title}, aliases...)), SupportsEpisodeURLs: &f}
	return &m, nil
}

func (c *Client) traktHeaders(ctx context.Context, cred Credential, public bool) (map[string]string, error) {
	id := first(cred.ClientID, c.setting(ctx, "traktClientId", ""))
	if id == "" {
		return nil, &Error{"trakt", 412, "OAuth client ID not configured"}
	}
	h := map[string]string{"trakt-api-version": "2", "trakt-api-key": id}
	if !public && cred.AccessToken != "" {
		h["Authorization"] = "Bearer " + cred.AccessToken
	}
	return h, nil
}
func traktItem(o map[string]any) Metadata {
	ids := object(o["ids"])
	return Metadata{ID: str(ids["trakt"]), Title: str(o["title"]), Type: "tv_series", Year: year(o["year"]), Details: str(o["overview"]), TMDBID: str(ids["tmdb"]), IMDBID: str(ids["imdb"]), TVDBID: str(ids["tvdb"]), Provider: "trakt"}
}
func (c *Client) traktSearch(ctx context.Context, q string, cred Credential) ([]Metadata, error) {
	h, e := c.traktHeaders(ctx, cred, true)
	if e != nil {
		return nil, e
	}
	v, e := c.json(ctx, "trakt", "GET", c.base(ctx, "traktApiBaseUrl", "https://api.trakt.tv")+"/search/show", url.Values{"query": {q}, "extended": {"full"}}, h, nil)
	if e != nil {
		return nil, e
	}
	out := []Metadata{}
	for i, x := range array(v) {
		if i >= 20 {
			break
		}
		out = append(out, traktItem(object(object(x)["show"])))
	}
	return out, nil
}
func (c *Client) traktDetails(ctx context.Context, id string, cred Credential) (*Metadata, error) {
	h, e := c.traktHeaders(ctx, cred, false)
	if e != nil {
		return nil, e
	}
	v, e := c.json(ctx, "trakt", "GET", c.base(ctx, "traktApiBaseUrl", "https://api.trakt.tv")+"/shows/"+url.PathEscape(id), url.Values{"extended": {"full"}}, h, nil)
	if e != nil {
		return nil, e
	}
	m := traktItem(object(v))
	return &m, nil
}

func (c *Client) anibtItem(ctx context.Context, o map[string]any) Metadata {
	titles := object(o["title"])
	id := first(o["bgmId"], o["_id"], o["animeId"])
	typ := "other"
	switch strings.ToUpper(first(o["format"], o["kind"])) {
	case "MOVIE":
		typ = "movie"
	case "TV", "ONA", "OVA":
		typ = "tv_series"
	}
	m := Metadata{ID: id, Provider: "anibt", Title: first(titles["chinese"], titles["primary"], o["nameCn"], titles["native"], titles["japanese"], o["name"], "AniBT "+id), Type: typ, BangumiID: str(o["bgmId"]), NameEn: str(titles["english"]), NameJp: first(titles["japanese"], titles["native"], o["name"]), NameRomaji: first(titles["romaji"], o["titleRomaji"]), Year: year(first(o["seasonYear"], o["date"])), Details: str(o["description"]), Extra: map[string]any{}}
	m.AliasesCn = unique([]string{str(titles["chinese"]), str(titles["chineseTraditional"]), str(o["nameCn"])})
	m.AliasesJp = unique([]string{str(titles["japanese"]), str(titles["native"])})
	m.AliasesCn = remove(m.AliasesCn, m.Title)
	m.AliasesJp = remove(m.AliasesJp, m.NameJp)
	for _, k := range []string{"animeId", "format", "status", "season", "genres", "officialSite", "episodes", "airingAt", "weekday", "scheduleStatus", "rssReleaseCount", "hasRelease"} {
		m.Extra[k] = o[k]
	}
	m.Extra["animeId"] = first(o["animeId"], o["_id"])
	image := first(nested(o, "coverImage", "extraLarge"), nested(o, "coverImage", "large"), o["cover"], o["image"])
	width := -1
	for _, x := range array(nested(o, "coverImageWebp", "variants")) {
		a := object(x)
		if w := integer(a["width"]); w > width {
			width = w
			image = str(a["url"])
		}
	}
	if image != "" {
		base := first(c.setting(ctx, "anibtImageBaseUrl", ""), c.base(ctx, "anibtApiBaseUrl", "https://anibt.net"))
		u, e := url.Parse(base + "/")
		if e == nil {
			v, e := url.Parse(image)
			if e == nil {
				m.ImageURL = u.ResolveReference(v).String()
			}
		}
	}
	return m
}
func remove(list []string, s string) []string {
	out := []string{}
	for _, v := range list {
		if v != s {
			out = append(out, v)
		}
	}
	return out
}
func (c *Client) anibtSearch(ctx context.Context, q, mediaType string) ([]Metadata, error) {
	r := []rune(q)
	if len(r) > 120 {
		q = string(r[:120])
	}
	v, e := c.json(ctx, "anibt", "GET", c.base(ctx, "anibtApiBaseUrl", "https://anibt.net")+"/api/seasons/anime", url.Values{"query": {q}}, nil, nil)
	if e != nil {
		return nil, e
	}
	out := []Metadata{}
	for _, group := range array(nested(v, "data", "byWeekday")) {
		for _, x := range array(object(group)["animes"]) {
			if len(out) >= 50 {
				break
			}
			out = append(out, c.anibtItem(ctx, object(x)))
		}
	}
	for i := 0; i < len(out) && i < 3; i++ {
		detail, e := c.anibtDetails(ctx, first(out[i].BangumiID, out[i].ID))
		if e == nil && detail != nil {
			out[i].AliasesCn = unique(append(out[i].AliasesCn, detail.AliasesCn...))
			out[i].AliasesJp = unique(append(out[i].AliasesJp, detail.AliasesJp...))
			if out[i].NameEn == "" {
				out[i].NameEn = detail.NameEn
			}
			if out[i].NameJp == "" {
				out[i].NameJp = detail.NameJp
			}
			if out[i].NameRomaji == "" {
				out[i].NameRomaji = detail.NameRomaji
			}
			if out[i].Year == 0 {
				out[i].Year = detail.Year
			}
			if out[i].ImageURL == "" {
				out[i].ImageURL = detail.ImageURL
			}
			if len(detail.Details) > len(out[i].Details) {
				out[i].Details = detail.Details
			}
			if out[i].Type == "other" && detail.Type != "" {
				out[i].Type = detail.Type
			}
			for k, v := range detail.Extra {
				out[i].Extra[k] = v
			}
		}
	}
	if mediaType != "" {
		filtered := []Metadata{}
		for _, m := range out {
			if m.Type == mediaType {
				filtered = append(filtered, m)
			}
		}
		out = filtered
	}
	return out, nil
}
func (c *Client) anibtDetails(ctx context.Context, id string) (*Metadata, error) {
	id = strings.TrimPrefix(id, "bgm:")
	if !regexp.MustCompile(`^\d+$`).MatchString(id) {
		return nil, &Error{"anibt", 404, "Bangumi ID required"}
	}
	v, e := c.json(ctx, "anibt", "GET", c.base(ctx, "anibtApiBaseUrl", "https://anibt.net")+"/api/anime/lookup", url.Values{"source": {"bgm"}, "id": {id}}, nil, nil)
	if e != nil {
		return nil, e
	}
	o := object(nested(v, "data", "anime"))
	if len(o) == 0 {
		return nil, &Error{"anibt", 404, "item not found"}
	}
	m := c.anibtItem(ctx, o)
	return &m, nil
}
func (c *Client) AniBTSeason(ctx context.Context, params url.Values) (any, error) {
	return c.json(ctx, "anibt", "GET", c.base(ctx, "anibtApiBaseUrl", "https://anibt.net")+"/api/seasons/anime", params, nil, nil)
}

func (c *Client) so360Search(ctx context.Context, q string) ([]Metadata, error) {
	b, e := c.request(ctx, "360", "GET", c.base(ctx, "so360ApiBaseUrl", "https://api.so.360kan.com")+"/index", url.Values{"force_v": {"1"}, "kw": {q}, "from": {""}, "pageno": {"1"}, "v_ap": {"1"}, "tab": {"all"}, "cb": {"__jp0"}}, map[string]string{"Referer": "https://so.360kan.com/?kw=" + url.QueryEscape(q)}, nil, 16<<20)
	if e != nil {
		return nil, e
	}
	s := strings.TrimSpace(string(b))
	if strings.HasPrefix(s, "__jp0(") {
		end := strings.LastIndex(s, ")")
		if end < 6 {
			return nil, &Error{"360", 502, "invalid JSONP"}
		}
		s = s[6:end]
	}
	var v any
	if decode([]byte(s), &v) != nil {
		return nil, &Error{"360", 502, "invalid search JSON"}
	}
	rows := array(nested(v, "data", "longData", "rows"))
	if rows == nil {
		rows = array(v)
	}
	out := []Metadata{}
	for _, x := range rows {
		o := object(x)
		title := str(o["titleTxt"])
		if !strings.Contains(strings.ToLower(title), strings.ToLower(q)) {
			continue
		}
		skip := false
		for _, k := range []string{"花絮", "独家专访", "幕后", "专访", "无障碍", "路演"} {
			if strings.Contains(title, k) {
				skip = true
			}
		}
		if skip {
			continue
		}
		t := "other"
		cat := str(o["cat_name"])
		if strings.Contains(cat, "电影") {
			t = "movie"
		} else if strings.Contains(cat, "电视") || strings.Contains(cat, "动漫") {
			t = "tv_series"
		}
		f := false
		out = append(out, Metadata{ID: first(o["en_id"], o["id"]), Provider: "360", Title: title, Type: t, Year: year(o["year"]), ImageURL: str(o["cover"]), AliasesCn: stringsOf(o["alias"]), Extra: map[string]any{"item_data": o}, SupportsEpisodeURLs: &f})
	}
	for _, item := range out {
		c.cacheSource("360:"+item.ID, object(item.Extra["item_data"]))
	}
	c.probe360(ctx, out)
	return out, nil
}
func (c *Client) so360Details(ctx context.Context, id string) (*Metadata, error) {
	var last error
	for _, p := range []string{"dianshiju", "dongman", "dianying"} {
		b, e := c.request(ctx, "360", "GET", c.base(ctx, "so360WebBaseUrl", "https://www.360kan.com")+"/"+p+"/"+url.PathEscape(id)+".html", nil, nil, nil, 16<<20)
		if e != nil {
			last = e
			if Status(e) == 404 {
				continue
			}
			return nil, e
		}
		match := regexp.MustCompile(`(?s)window\.g_initialData\s*=\s*(\{.*?\});`).FindSubmatch(b)
		if len(match) < 2 {
			continue
		}
		var v any
		if decode(match[1], &v) != nil {
			continue
		}
		o := object(nested(v, "coverInfo", "coverInfo"))
		if len(o) == 0 {
			continue
		}
		typ := "tv_series"
		if p == "dianying" {
			typ = "movie"
		}
		f := true
		m := Metadata{ID: id, Provider: "360", Title: str(o["title"]), Type: typ, ImageURL: str(o["cover"]), Details: first(o["description"], o["description_txt"]), Year: year(o["year"]), AliasesCn: unique([]string{str(o["sub_title"])}), SupportsEpisodeURLs: &f}
		return &m, nil
	}
	if last == nil {
		last = &Error{"360", 404, "item not found"}
	}
	return nil, last
}

func (c *Client) AniBTDiscoverSeason(ctx context.Context, params url.Values) (any, error) {
	v, e := c.AniBTSeason(ctx, params)
	if e != nil {
		return nil, e
	}
	data := object(object(v)["data"])
	items := []map[string]any{}
	for _, group := range array(data["byWeekday"]) {
		for _, x := range array(object(group)["animes"]) {
			m := c.anibtItem(ctx, object(x))
			items = append(items, map[string]any{"provider": "anibt", "type": "anibt_season_anime", "title": m.Title, "cover": nullable(m.ImageURL), "description": m.Details, "payload": map[string]any{"bangumiId": nullable(m.BangumiID), "animeId": m.Extra["animeId"], "season": data["requestedSeason"], "hasRelease": m.Extra["hasRelease"]}})
		}
	}
	return map[string]any{"season": data["requestedSeason"], "items": items}, nil
}
