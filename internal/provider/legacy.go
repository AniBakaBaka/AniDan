// SPDX-License-Identifier: AGPL-3.0-only
// Protocol compatibility based on l429609201/misaka_danmu_server, AGPL-3.0,
// historical source commit 300ad904b3bed5a490c18855ca2903bdff5e1792.
package provider

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"github.com/AniBakaBaka/AniDan/internal/danmaku"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

type Legacy struct {
	HTTPProvider
	Source        string
	YoukuClientID string `json:"-"`
}

func NewLegacy(name string, c *http.Client) *Legacy {
	h := newHTTP(c)
	h.sourceName = strings.ToLower(name)
	return &Legacy{HTTPProvider: h, Source: strings.ToLower(name)}
}
func (l *Legacy) Name() string { return l.Source }
func (l *Legacy) Capability() Capability {
	c := Capability{Name: l.Name(), Status: "not implemented", Blocker: "current provider distributed only as CPython 3.12 binary; no auditable protocol implementation"}
	switch l.Source {
	case "ezdmw":
		c.Blocker = "authoritative domain and readable request/response contract not established"
	case "girigirilove":
		c.Blocker = "public search/chapter rules exist; native adapter, comment-pool mapping and permitted CAPTCHA-free access unverified"
	case "mddcloud":
		c.Blocker = "readable signing/payload reference exists; native adapter and identity setup unverified; public website returned HTTP 403"
	case "hongguo":
		c.Search = true
		c.Episodes = true
		c.Status = "partial; public web discovery live verified 2026-09-30"
		c.Blocker = "comment API identity/bootstrap and acceptance unverified; no signed Android requests are made"
	case "migu":
		c.Search = true
		c.Episodes = true
		c.Comments = true
		c.Status = "public protocol; search/episodes live, nonempty comment acceptance pending"
		c.Blocker = "first live comment pool was empty; second query lacked expected search data; regional/title coverage unverified"
	case "xigua":
		c.Search = true
		c.Episodes = true
		c.Comments = true
		c.Status = "public mobile protocol; fixture tested, live HTTP 500"
		c.Blocker = "search/episode HTML and unsigned comment API depend on public regional availability"
	case "hanjutv", "gamer", "mgtv":
		c.Search = true
		c.Episodes = true
		c.Comments = true
		c.Status = "historical protocol; fixture-tested, live acceptance pending"
		c.Blocker = "current compiled-provider parity not established"
	case "tencent":
		c.Search = true
		c.Episodes = true
		c.Comments = true
		c.Status = "partial"
		c.Blocker = "tab-based episodes only; generic fallback and source-specific heuristics not ported"
	case "sohu":
		c.Search = true
		c.Episodes = true
		c.Comments = true
		c.Status = "public player duration-window coverage; bounded fixture tested"
		c.Blocker = "archival all-stored-comment completeness unverified; category and regional coverage unverified"
	case "renren":
		c.Comments = true
		c.Status = "partial"
		c.Blocker = "plaintext static comments only; signed/encrypted search and episodes not ported"
	case "le":
		c.Search = true
		c.Episodes = true
		c.Comments = true
		c.Status = "public search and episode protocol; bounded fixture tested"
		c.Blocker = "episodic series pagination only; source-specific variety discovery unverified; comments require verifiable page duration"
	case "iqiyi":
		c.Search = true
		c.Episodes = true
		c.Comments = true
		c.Status = "partial"
		c.Blocker = "legacy decode/album episodes and zlib XML comments; new signed Brotli/protobuf and variety-month fallbacks not ported"
	case "youku":
		c.Search = true
		c.Episodes = true
		c.Status = "partial; registered client ID required for episodes"
		c.Blocker = "OpenAPI episodes require a registered youkuClientId; MTop comment signing/token flow is not live verified"
	}
	return c
}
func (l *Legacy) unsupported(op string) error {
	return &UnsupportedError{l.Source, op, l.Capability().Blocker}
}
func (l *Legacy) Search(ctx context.Context, keyword string) ([]Result, error) {
	switch l.Source {
	case "le":
		return l.leSearch(ctx, keyword)
	case "hongguo":
		return l.hongguoSearch(ctx, keyword)
	case "migu":
		return l.miguSearch(ctx, keyword)
	case "xigua":
		return l.xiguaSearch(ctx, keyword)
	case "tencent":
		return l.tencentSearch(ctx, keyword)
	case "hanjutv":
		return l.hanjuSearch(ctx, keyword)
	case "gamer":
		return l.gamerSearch(ctx, keyword)
	case "mgtv":
		return l.mgtvSearch(ctx, keyword)
	case "sohu":
		return l.sohuSearch(ctx, keyword)
	case "iqiyi":
		return l.iqiyiSearch(ctx, keyword)
	case "youku":
		return l.youkuSearch(ctx, keyword)
	default:
		return nil, l.unsupported("search")
	}
}
func (l *Legacy) Episodes(ctx context.Context, id string) ([]Episode, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	if e := validID(id); e != nil {
		return nil, e
	}
	switch l.Source {
	case "le":
		return l.leEpisodes(ctx, id)
	case "hongguo":
		return l.hongguoEpisodes(ctx, id)
	case "migu":
		return l.miguEpisodes(ctx, id)
	case "xigua":
		return l.xiguaEpisodes(ctx, id)
	case "tencent":
		return l.tencentEpisodes(ctx, id)
	case "iqiyi":
		return l.iqiyiEpisodes(ctx, id)
	case "hanjutv":
		return l.hanjuEpisodes(ctx, id)
	case "gamer":
		return l.gamerEpisodes(ctx, id)
	case "mgtv":
		return l.mgtvEpisodes(ctx, id)
	case "sohu":
		return l.sohuEpisodes(ctx, id)
	case "youku":
		return l.youkuEpisodes(ctx, id)
	default:
		return nil, l.unsupported("episodes")
	}
}
func (l *Legacy) Comments(ctx context.Context, id string) ([]danmaku.Comment, error) {
	return l.coalesce(ctx, l.Source+":"+id, func(work context.Context) ([]danmaku.Comment, error) { return l.comments(work, id) })
}
func (l *Legacy) comments(ctx context.Context, id string) ([]danmaku.Comment, error) {
	if e := validID(id); e != nil {
		return nil, e
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	switch l.Source {
	case "sohu":
		return l.sohuComments(ctx, id)
	case "migu":
		return l.miguComments(ctx, id)
	case "xigua":
		return l.xiguaComments(ctx, id)
	case "iqiyi":
		return l.iqiyiComments(ctx, id)
	case "hanjutv":
		return l.hanjuComments(ctx, id)
	case "gamer":
		return l.gamerComments(ctx, id)
	case "mgtv":
		return l.mgtvComments(ctx, id)
	case "tencent":
		return l.tencentComments(ctx, id)
	case "renren":
		return l.renrenComments(ctx, id)
	case "le":
		return l.leComments(ctx, id)
	default:
		return nil, l.unsupported("comments")
	}
}
func (l *Legacy) hanjuSearch(ctx context.Context, key string) ([]Result, error) {
	var v map[string]any
	e := l.get(ctx, "https://hxqapi.hiyun.tv/wapi/search/aggregate/search", url.Values{"keyword": {key}, "scope": {"101"}, "page": {"1"}}, &v)
	if e != nil {
		return nil, e
	}
	if v["rescode"] != nil && integer(v["rescode"]) != 0 {
		return nil, fmt.Errorf("hanjutv search rejected request (rescode %d)", integer(v["rescode"]))
	}
	data := at(v, "seriesData", "seriesList")
	if data == nil {
		return nil, errors.New("hanjutv response missing seriesData.seriesList")
	}
	out := []Result{}
	for _, a := range array(data) {
		m := object(a)
		if str(m["sid"]) == "" {
			continue
		}
		typ := "tv_series"
		if integer(m["category"]) == 3 {
			typ = "movie"
		}
		out = append(out, Result{ID: str(m["sid"]), Title: str(m["name"]), ImageURL: str(at(m, "image", "thumb")), Type: typ, Season: 1})
	}
	return out, nil
}
func (l *Legacy) hanjuEpisodes(ctx context.Context, id string) ([]Episode, error) {
	var v map[string]any
	e := l.get(ctx, "https://hxqapi.hiyun.tv/wapi/series/series/detail", url.Values{"sid": {id}}, &v)
	if e != nil {
		return nil, e
	}
	if _, ok := v["episodes"]; !ok {
		return nil, errors.New("hanjutv response missing episodes")
	}
	out := []Episode{}
	for i, a := range array(v["episodes"]) {
		m := object(a)
		pid := str(m["pid"])
		if pid == "" {
			continue
		}
		index := integer(m["serialNo"])
		if index == 0 {
			index = i + 1
		}
		title := str(m["title"])
		if title == "" {
			title = fmt.Sprintf("第%d集", index)
		}
		out = append(out, Episode{ID: pid, Title: title, Index: index, URL: "https://hanjutv.com/play/" + id + "/" + pid})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Index < out[j].Index })
	return out, nil
}
func (l *Legacy) hanjuComments(ctx context.Context, id string) ([]danmaku.Comment, error) {
	axis := int64(0)
	const last = int64(100000000)
	totalBytes := int64(0)
	out := []danmaku.Comment{}
	for page := 0; page < l.maxSegments(); page++ {
		var v map[string]any
		e := l.get(ctx, "https://hxqapi.zmdcq.com/api/danmu/playItem/list", url.Values{"pid": {id}, "fromAxis": {strconv.FormatInt(axis, 10)}, "toAxis": {strconv.FormatInt(last, 10)}}, &v)
		if e != nil {
			return nil, e
		}
		if _, ok := v["danmus"]; !ok {
			return nil, errors.New("hanjutv response missing danmus")
		}
		for _, a := range array(v["danmus"]) {
			m := object(a)
			mode := integer(m["tp"])
			if mode == 0 {
				mode = 1
			}
			color := 0xffffff
			if m["sc"] != nil {
				color = integer(m["sc"])
			}
			c, e := comment(m["did"], number(m["t"])/1000, mode, color, str(m["con"]), l.Source)
			if e != nil {
				return nil, e
			}
			out, e = l.appendBudget(out, &totalBytes, c)
			if e != nil {
				return nil, e
			}
		}
		next := int64(integer(v["nextAxis"]))
		if v["nextAxis"] == nil || next >= last {
			return mergeCanonical(out)
		}
		if next <= axis {
			return nil, errors.New("hanjutv cursor did not advance")
		}
		axis = next
	}
	return nil, fmt.Errorf("%w: hanjutv pages", danmaku.ErrLimit)
}
func (l *Legacy) mgtvSearch(ctx context.Context, key string) ([]Result, error) {
	var v map[string]any
	e := l.get(ctx, "https://mobileso.bz.mgtv.com/msite/search/v2", url.Values{"q": {key}, "pc": {"30"}, "pn": {"1"}, "sort": {"-99"}, "ty": {"0"}, "du": {"0"}, "pt": {"0"}, "corr": {"1"}, "abroad": {"0"}, "_support": {"10000000000000000"}}, &v)
	if e != nil {
		return nil, e
	}
	if at(v, "data", "contents") == nil {
		return nil, errors.New("mgtv response missing contents")
	}
	out := []Result{}
	for _, a := range array(at(v, "data", "contents")) {
		m := object(a)
		if str(m["type"]) != "media" {
			continue
		}
		for _, b := range array(m["data"]) {
			r := object(b)
			if str(r["source"]) != "imgo" {
				continue
			}
			parts := strings.Split(str(r["url"]), "/")
			id := ""
			for i, p := range parts {
				if p == "b" && i+1 < len(parts) {
					id = parts[i+1]
					break
				}
			}
			if id == "" {
				continue
			}
			out = append(out, Result{ID: id, Title: tags.ReplaceAllString(str(r["title"]), ""), ImageURL: str(r["img"]), Type: "tv_series", Season: 1})
		}
	}
	return out, nil
}
func (l *Legacy) mgtvEpisodes(ctx context.Context, id string) ([]Episode, error) {
	months := []string{""}
	seen := map[string]bool{}
	out := []Episode{}
	for page := 0; page < len(months); page++ {
		if page >= l.maxSegments() {
			return nil, danmaku.ErrLimit
		}
		var v map[string]any
		e := l.get(ctx, "https://pcweb.api.mgtv.com/variety/showlist", url.Values{"allowedRC": {"1"}, "collection_id": {id}, "month": {months[page]}, "page": {"1"}, "_support": {"10000000"}}, &v)
		if e != nil {
			return nil, e
		}
		r := object(v["data"])
		if r == nil {
			return nil, errors.New("mgtv response missing data")
		}
		if page == 0 {
			for _, a := range array(r["tab_m"]) {
				month := str(object(a)["m"])
				if month != "" {
					months = append(months, month)
				}
			}
		}
		for _, a := range array(r["list"]) {
			m := object(a)
			vid := str(m["video_id"])
			if vid == "" || seen[vid] || str(m["src_clip_id"]) != id {
				continue
			}
			seen[vid] = true
			out = append(out, Episode{ID: id + "," + vid, Title: strings.TrimSpace(str(m["t2"]) + " " + str(m["t1"])), Index: len(out) + 1, URL: "https://www.mgtv.com/b/" + id + "/" + vid + ".html"})
		}
	}
	return out, nil
}
func (l *Legacy) mgtvComments(ctx context.Context, id string) ([]danmaku.Comment, error) {
	p := strings.Split(id, ",")
	if len(p) != 2 {
		return nil, errors.New("mgtv episode ID must be collection,video")
	}
	totalBytes := int64(0)
	out := []danmaku.Comment{}
	cursor := 0
	for page := 0; page < l.maxSegments(); page++ {
		var v map[string]any
		e := l.get(ctx, "https://galaxy.bz.mgtv.com/cdn/opbarrage", url.Values{"vid": {p[1]}, "pid": {""}, "cid": {p[0]}, "ticket": {""}, "time": {strconv.Itoa(cursor)}, "allowedRC": {"1"}}, &v)
		if e != nil {
			return nil, e
		}
		r := object(v["data"])
		if r == nil {
			return nil, errors.New("mgtv response missing data")
		}
		for _, a := range array(r["items"]) {
			m := object(a)
			mode := 1
			switch integer(m["type"]) {
			case 1:
				mode = 5
			case 2:
				mode = 4
			}
			color := 0xffffff
			if rgb := object(at(m, "v2_color", "color_left")); rgb != nil {
				r, g, b := integer(rgb["r"]), integer(rgb["g"]), integer(rgb["b"])
				if r < 0 || r > 255 || g < 0 || g > 255 || b < 0 || b > 255 {
					return nil, errors.New("invalid mgtv RGB")
				}
				color = r<<16 | g<<8 | b
			}
			c, e := comment(m["id"], number(m["time"])/1000, mode, color, str(m["content"]), l.Source)
			if e != nil {
				return nil, e
			}
			out, e = l.appendBudget(out, &totalBytes, c)
			if e != nil {
				return nil, e
			}
		}
		next := integer(r["next"])
		if next == 0 {
			return mergeCanonical(out)
		}
		if next <= cursor {
			return nil, errors.New("mgtv cursor did not advance")
		}
		cursor = next
	}
	return nil, danmaku.ErrLimit
}
func (l *Legacy) tencentComments(ctx context.Context, id string) ([]danmaku.Comment, error) {
	var v map[string]any
	if e := l.get(ctx, "https://dm.video.qq.com/barrage/base/"+url.PathEscape(id), nil, &v); e != nil {
		return nil, e
	}
	index := object(v["segment_index"])
	if index == nil {
		return nil, errors.New("tencent response missing segment_index")
	}
	if len(index) > l.maxSegments() {
		return nil, danmaku.ErrLimit
	}
	keys := make([]string, 0, len(index))
	for k := range index {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return integer(keys[i]) < integer(keys[j]) })
	out, e := l.segments(ctx, len(keys), func(ctx context.Context, i int) ([]danmaku.Comment, error) {
		key := keys[i]
		name := str(object(index[key])["segment_name"])
		// Tencent returns a relative path such as t/v1/0/30000, not a
		// single opaque ID. Validate each component, keeping its separators.
		if !validTencentSegment(name) {
			return nil, errors.New("invalid tencent segment name")
		}
		raw, e := l.request(ctx, http.MethodGet, "https://dm.video.qq.com/barrage/segment/"+url.PathEscape(id)+"/"+name, nil, nil, nil)
		if e != nil {
			return nil, e
		}
		return decodeTencentSegment(raw)
	})
	if e != nil {
		return nil, e
	}
	return mergeCanonical(out)
}
func (l *Legacy) renrenComments(ctx context.Context, id string) ([]danmaku.Comment, error) {
	var v any
	if e := l.get(ctx, "https://static-dm.rrmj.plus/v1/produce/danmu/EPISODE/"+url.PathEscape(id), nil, &v); e != nil {
		return nil, fmt.Errorf("renren plaintext static comments: %w", e)
	}
	list := array(v)
	if list == nil {
		list = array(object(v)["data"])
	}
	if list == nil {
		return nil, errors.New("renren comments require plaintext JSON array; encrypted response unsupported")
	}
	totalBytes := int64(0)
	out := []danmaku.Comment{}
	for _, a := range list {
		m := object(a)
		c, e := danmaku.Normalize(danmaku.Comment{P: str(m["p"]), M: str(m["d"])}, "[renren]")
		if e != nil {
			return nil, e
		}
		out, e = l.appendBudget(out, &totalBytes, c)
		if e != nil {
			return nil, e
		}
	}
	return mergeCanonical(out)
}
func decodeJSONP(raw []byte, v any) error {
	raw = bytes.TrimSpace(raw)
	if len(raw) > 0 && (raw[0] == '{' || raw[0] == '[') {
		return decodeJSON(raw, v)
	}
	start := bytes.IndexByte(raw, '(')
	end := bytes.LastIndexByte(raw, ')')
	if start < 1 || end < start {
		return errors.New("invalid JSONP")
	}
	for _, c := range raw[:start] {
		if !(c == '_' || c == '.' || c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9') {
			return errors.New("invalid JSONP callback")
		}
	}
	if tail := strings.TrimSpace(string(raw[end+1:])); tail != "" && tail != ";" {
		return errors.New("invalid JSONP suffix")
	}
	return decodeJSON(raw[start+1:end], v)
}
func (l *Legacy) sohuSearch(ctx context.Context, key string) ([]Result, error) {
	var v map[string]any
	if e := l.get(ctx, "https://m.so.tv.sohu.com/search/pc/keyword", url.Values{"key": {key}, "type": {"1"}, "page": {"1"}, "page_size": {"20"}, "poster": {"4"}, "tuple": {"6"}, "ssl": {"1"}}, &v); e != nil {
		return nil, e
	}
	if integer(at(v, "data", "is_empty")) == 1 {
		return []Result{}, nil
	}
	if at(v, "data", "items") == nil {
		return nil, errors.New("sohu response missing data.items")
	}
	out := []Result{}
	for _, a := range array(at(v, "data", "items")) {
		m := object(a)
		if integer(m["data_type"]) != 257 || str(m["aid"]) == "" {
			continue
		}
		title := strings.ReplaceAll(strings.ReplaceAll(str(m["album_name"]), "<<<", ""), ">>>", "")
		out = append(out, Result{ID: str(m["aid"]), Title: title, Type: "tv_series", Year: integer(m["year"]), Season: 1, ImageURL: str(m["ver_big_pic"])})
	}
	return out, nil
}
func (l *Legacy) iqiyiSearch(ctx context.Context, key string) ([]Result, error) {
	var v map[string]any
	if e := l.get(ctx, "https://search.video.iqiyi.com/o", url.Values{"if": {"html5"}, "key": {key}, "pageNum": {"1"}, "pageSize": {"20"}}, &v); e != nil {
		return nil, e
	}
	if at(v, "data", "docinfos") == nil {
		return nil, errors.New("iqiyi response missing docinfos")
	}
	out := []Result{}
	for _, a := range array(at(v, "data", "docinfos")) {
		m := object(a)
		r := object(m["albumDocInfo"])
		if r == nil {
			r = object(m["album_doc_info"])
		}
		if site := str(r["siteId"]); site != "" && site != "iqiyi" {
			continue
		}
		if typ := r["videoDocType"]; typ != nil && integer(typ) != 1 {
			continue
		}
		link := textFirst(r["albumLink"], r["album_link"])
		if vids := array(r["videoinfos"]); len(vids) > 0 {
			if first := str(object(vids[0])["itemLink"]); first != "" {
				link = first
			}
		}
		u, e := url.Parse(link)
		if e != nil || !(u.Hostname() == "www.iqiyi.com" || u.Hostname() == "m.iqiyi.com") {
			continue
		}
		base := pathBase(u.Path)
		if !strings.HasPrefix(base, "v_") || !strings.HasSuffix(base, ".html") {
			continue
		}
		id := strings.TrimSuffix(strings.TrimPrefix(base, "v_"), ".html")
		if id == "" {
			continue
		}
		out = append(out, Result{ID: id, Title: tags.ReplaceAllString(textFirst(r["albumTitle"], r["album_title"]), ""), Type: "tv_series", ImageURL: textFirst(r["albumImg"], r["album_img"]), Year: yearFromDate(str(r["releaseDate"])), Season: 1})
	}
	return out, nil
}
func pathBase(s string) string {
	p := strings.Split(strings.TrimSuffix(s, "/"), "/")
	return p[len(p)-1]
}
func (l *Legacy) youkuSearch(ctx context.Context, key string) ([]Result, error) {
	var v map[string]any
	if e := l.get(ctx, "https://search.youku.com/api/search", url.Values{"keyword": {key}, "site": {"1"}, "categories": {"0"}, "ftype": {"0"}, "ob": {"0"}, "pg": {"1"}, "userAgent": {l.Headers.Get("User-Agent")}}, &v); e != nil {
		return nil, e
	}
	if _, ok := v["pageComponentList"]; !ok {
		return nil, errors.New("youku response missing pageComponentList")
	}
	out := []Result{}
	for _, a := range array(v["pageComponentList"]) {
		r := object(object(a)["commonData"])
		id := str(r["showId"])
		if id == "" || (integer(r["isYouku"]) != 1 && integer(r["hasYouku"]) != 1) {
			continue
		}
		out = append(out, Result{ID: id, Title: str(at(r, "titleDTO", "displayName")), Type: "tv_series", ImageURL: str(at(r, "posterDTO", "vThumbUrl")), Season: 1})
	}
	return out, nil
}
