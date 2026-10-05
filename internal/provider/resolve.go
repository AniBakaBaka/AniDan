// SPDX-License-Identifier: AGPL-3.0-only
package provider

import (
	"errors"
	"net/url"
	"regexp"
	"strings"
)

var biliMedia = regexp.MustCompile(`^(?:ss[0-9]+|ep[0-9]+|av[0-9]+|BV[A-Za-z0-9]+)$`)
var numericID = regexp.MustCompile(`^[0-9]+$`)
var alphaID = regexp.MustCompile(`^[A-Za-z0-9_=\-]+$`)

// ResolveURL is parsing only, never a fetch. Exact trusted hosts and path shapes
// prevent arbitrary URL imports and SSRF through user-supplied locations.
// Bilibili multipart p=N is retained as a :pN media suffix for Episodes.
func ResolveURL(raw string) (providerName, mediaID, episodeID string, err error) {
	u, e := url.Parse(raw)
	if e != nil || u.User != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Port() != "" || u.Fragment != "" {
		return "", "", "", errors.New("invalid provider URL")
	}
	host := strings.ToLower(u.Hostname())
	p := strings.Split(strings.Trim(u.Path, "/"), "/")
	bad := func() (string, string, string, error) {
		return "", "", "", errors.New("unsupported provider host or URL path")
	}
	switch host {
	case "hongguoduanju.com":
		id := u.Query().Get("series_id")
		if u.Path == "/detail" && numericID.MatchString(id) && len(id) <= 64 {
			return "hongguo", id, "", nil
		}
	case "www.miguvideo.com":
		if len(p) == 3 && p[0] == "p" && p[1] == "detail" && numericID.MatchString(p[2]) && len(p[2]) <= 64 {
			return "migu", p[2], p[2], nil
		}
	case "m.ixigua.com", "www.ixigua.com", "ixigua.com":
		if len(p) == 2 && p[0] == "video" && numericID.MatchString(p[1]) && len(p[1]) <= 24 {
			return "xigua", p[1], p[1], nil
		}
	case "www.bilibili.com", "bilibili.com", "m.bilibili.com":
		id := ""
		if len(p) == 2 && p[0] == "video" {
			id = p[1]
		} else if len(p) == 3 && p[0] == "bangumi" && p[1] == "play" {
			id = p[2]
		}
		if !biliMedia.MatchString(id) {
			return bad()
		}
		if page := u.Query().Get("p"); page != "" {
			if !numericID.MatchString(page) || len(page) > 5 {
				return bad()
			}
			id += ":p" + page
		}
		return "bilibili", id, "", nil
	case "ani.gamer.com.tw":
		sn := u.Query().Get("sn")
		if !numericID.MatchString(sn) {
			return bad()
		}
		if u.Path == "/animeVideo.php" {
			return "gamer", "", sn, nil
		}
		if u.Path == "/animeRef.php" {
			return "gamer", sn, "", nil
		}
	case "hanjutv.com", "www.hanjutv.com", "hanju.com", "www.hanju.com":
		if len(p) == 2 && (p[0] == "series" || p[0] == "play") && alphaID.MatchString(p[1]) {
			return "hanjutv", p[1], "", nil
		}
		if len(p) == 3 && p[0] == "play" && alphaID.MatchString(p[1]) && alphaID.MatchString(p[2]) {
			return "hanjutv", p[1], p[2], nil
		}
	case "www.mgtv.com", "mgtv.com":
		if len(p) >= 2 && p[0] == "b" && numericID.MatchString(p[1]) {
			if len(p) == 2 {
				return "mgtv", p[1], "", nil
			}
			vid := strings.TrimSuffix(p[2], ".html")
			if len(p) == 3 && numericID.MatchString(vid) {
				return "mgtv", p[1], p[1] + "," + vid, nil
			}
		}
	case "v.qq.com", "m.v.qq.com":
		if len(p) == 4 && p[0] == "x" && p[1] == "cover" {
			vid := strings.TrimSuffix(p[3], ".html")
			if alphaID.MatchString(p[2]) && alphaID.MatchString(vid) {
				return "tencent", p[2], vid, nil
			}
		}
		if len(p) == 3 && p[0] == "x" && (p[1] == "cover" || p[1] == "page") {
			id := strings.TrimSuffix(p[2], ".html")
			if alphaID.MatchString(id) {
				if p[1] == "page" {
					return "tencent", "", id, nil
				}
				return "tencent", id, "", nil
			}
		}
	case "v.youku.com":
		if len(p) == 2 && p[0] == "v_show" && strings.HasPrefix(p[1], "id_") {
			id := strings.TrimSuffix(strings.TrimPrefix(p[1], "id_"), ".html")
			if alphaID.MatchString(id) {
				return "youku", "", id, nil
			}
		}
	case "www.iqiyi.com", "m.iqiyi.com", "iqiyi.com":
		if len(p) == 1 && strings.HasPrefix(p[0], "v_") && strings.HasSuffix(p[0], ".html") {
			id := strings.TrimSuffix(strings.TrimPrefix(p[0], "v_"), ".html")
			if alphaID.MatchString(id) {
				return "iqiyi", id, "", nil
			}
		}
	case "www.le.com", "le.com":
		if len(p) == 3 && p[0] == "ptv" && p[1] == "vplay" {
			id := strings.TrimSuffix(p[2], ".html")
			if numericID.MatchString(id) {
				return "le", "", id, nil
			}
		}
		if len(p) == 2 && (p[0] == "tv" || p[0] == "comic" || p[0] == "movie" || p[0] == "playlet") {
			id := strings.TrimSuffix(p[1], ".html")
			if numericID.MatchString(id) {
				return "le", id, "", nil
			}
		}
	case "tv.sohu.com", "m.tv.sohu.com":
		if vid, aid := u.Query().Get("vid"), u.Query().Get("aid"); numericID.MatchString(vid) && numericID.MatchString(aid) {
			return "sohu", aid, vid + ":" + aid, nil
		}
		if len(p) == 2 && p[0] == "item" {
			id := strings.TrimSuffix(p[1], ".html")
			if alphaID.MatchString(id) {
				return "sohu", id, "", nil
			}
		}
	case "rrsp.com.cn", "www.rrsp.com.cn":
		if len(p) == 3 && p[0] == "v" && alphaID.MatchString(p[1]) && alphaID.MatchString(p[2]) {
			return "renren", p[1], p[2], nil
		}
		if len(p) == 3 && p[0] == "pc" && p[1] == "drama" && alphaID.MatchString(p[2]) {
			return "renren", p[2], "", nil
		}
	}
	return bad()
}

// ProviderForURL performs exact known-domain matching without any network I/O.
// A match identifies the provider only; it does not promise path resolution or
// supported capabilities. Compiled-only providers without audited domains do not
// match speculatively.
func ProviderForURL(raw string) (string, bool) {
	u, e := url.Parse(raw)
	if e != nil || u.User != nil || u.Port() != "" || (u.Scheme != "http" && u.Scheme != "https") {
		return "", false
	}
	domains := map[string]string{"hongguoduanju.com": "hongguo", "www.miguvideo.com": "migu", "m.ixigua.com": "xigua", "www.ixigua.com": "xigua", "ixigua.com": "xigua", "bilibili.com": "bilibili", "www.bilibili.com": "bilibili", "m.bilibili.com": "bilibili", "b23.tv": "bilibili", "api.dandanplay.net": "dandanplay", "www.dandanplay.com": "dandanplay", "ani.gamer.com.tw": "gamer", "hanjutv.com": "hanjutv", "www.hanjutv.com": "hanjutv", "hanju.com": "hanjutv", "www.hanju.com": "hanjutv", "www.mgtv.com": "mgtv", "mgtv.com": "mgtv", "v.qq.com": "tencent", "m.v.qq.com": "tencent", "v.youku.com": "youku", "www.iqiyi.com": "iqiyi", "m.iqiyi.com": "iqiyi", "iqiyi.com": "iqiyi", "www.le.com": "le", "le.com": "le", "tv.sohu.com": "sohu", "m.tv.sohu.com": "sohu", "rrsp.com.cn": "renren", "www.rrsp.com.cn": "renren"}
	p, ok := domains[strings.ToLower(u.Hostname())]
	return p, ok
}
