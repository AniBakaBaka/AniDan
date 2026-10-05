// SPDX-License-Identifier: AGPL-3.0-only
// Historical protocol compatibility with AGPL-3.0 misaka Tencent adapter.
package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Misaka's ordinary-series order is numeric episode titles first, with a
// stable order for everything else. RPC tab order is not episode order.
var tencentEpisodeNumber = regexp.MustCompile(`^(?:第)?([0-9]+)(?:集|话)?$`)

func tencentEpisodeSortIndex(title string) int {
	m := tencentEpisodeNumber.FindStringSubmatch(strings.TrimSpace(title))
	if len(m) == 2 {
		if n, err := strconv.Atoi(m[1]); err == nil && n > 0 {
			return n
		}
	}
	return int(^uint(0) >> 1)
}

// Historical Tencent default; an explicitly empty database setting disables it.
const TencentEpisodeBlacklistDefault = `^(.*?)(vlog|reaction|纯享|加更|抢先|预告|花絮|特辑|彩蛋|专访|幕后|直播|未播|衍生|番外|会员|片花|精华|看点|速看|解读|影评|解说|吐槽|盘点|拍摄花絮|制作花絮|幕后花絮|未播花絮|独家花絮|花絮特辑|先导预告|终极预告|正式预告|官方预告|彩蛋片段|删减片段|未播片段|番外彩蛋|精彩片段|精彩看点|精彩回顾|精彩集锦|看点解析|看点预告|NG镜头|NG花絮|番外篇|番外特辑|制作特辑|拍摄特辑|幕后特辑|导演特辑|演员特辑|片尾曲|插曲|主题曲|背景音乐|OST|音乐MV|歌曲MV|前季回顾|剧情回顾|往期回顾|内容总结|剧情盘点|精选合集|剪辑合集|混剪视频|独家专访|演员访谈|导演访谈|主创访谈|媒体采访|发布会采访|抢先看|抢先版|试看版|短剧|精编|会员版|Plus|独家版|特别版|短片|合唱)(.*?)$`

func validTencentSegment(name string) bool {
	if len(name) == 0 || len(name) > 256 {
		return false
	}
	parts := strings.Split(name, "/")
	if len(parts) > 8 {
		return false
	}
	for _, p := range parts {
		if p == "" || p == "." || p == ".." {
			return false
		}
		for _, c := range p {
			if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_' || c == '-') {
				return false
			}
		}
	}
	return true
}

func (l *Legacy) tencentSearch(ctx context.Context, key string) ([]Result, error) {
	payload := map[string]any{"query": key, "version": "", "filterValue": "firstTabid=150", "retry": 0, "pagenum": 0, "pagesize": 20, "queryFrom": 4, "isneedQc": true, "adRequestInfo": "", "sdkRequestInfo": "", "sceneId": 21, "platform": "23"}
	body, _ := json.Marshal(payload)
	raw, e := l.request(ctx, http.MethodPost, "https://pbaccess.video.qq.com/trpc.videosearch.mobile_search.HttpMobileRecall/MbSearchHttp", nil, bytes.NewReader(body), http.Header{"Content-Type": {"application/json"}, "Origin": {"https://v.qq.com"}, "Referer": {"https://v.qq.com/"}})
	if e != nil {
		return nil, e
	}
	var v map[string]any
	if e = decodeJSON(raw, &v); e != nil {
		return nil, e
	}
	items, ok := at(v, "data", "normalList", "itemList").([]any)
	if !ok {
		return nil, errors.New("tencent search missing normalList.itemList")
	}
	out := []Result{}
	for _, a := range items {
		m := object(a)
		r := object(m["videoInfo"])
		id := str(at(m, "doc", "id"))
		if id == "" || r == nil || str(r["subTitle"]) == "全网搜" || integer(r["playFlag"]) == 2 {
			continue
		}
		sites := append(array(r["playSites"]), array(r["episodeSites"])...)
		own := len(sites) == 0
		for _, s := range sites {
			if str(object(s)["enName"]) == "qq" {
				own = true
			}
		}
		if !own {
			continue
		}
		typ := "tv_series"
		if str(r["typeName"]) == "电影" {
			typ = "movie"
		}
		out = append(out, Result{ID: id, Title: tags.ReplaceAllString(str(r["title"]), ""), Type: typ, ImageURL: str(r["imgUrl"]), Year: integer(r["year"]), Season: 1})
	}
	return out, nil
}
func (l *Legacy) tencentPage(ctx context.Context, id, pc string) (map[string]any, error) {
	payload := map[string]any{"has_cache": 1, "page_params": map[string]string{"req_from": "web_vsite", "page_id": "vsite_episode_list", "page_type": "detail_operation", "id_type": "1", "page_size": "", "cid": id, "vid": "", "lid": "", "page_num": "", "page_context": pc, "detail_page_type": "1"}}
	body, _ := json.Marshal(payload)
	raw, e := l.request(ctx, http.MethodPost, "https://pbaccess.video.qq.com/trpc.universal_backend_service.page_server_rpc.PageServer/GetPageData?video_appid=3000010&vversion_name=8.2.96&vversion_platform=2", nil, bytes.NewReader(body), http.Header{"Content-Type": {"application/json"}, "Origin": {"https://v.qq.com"}, "Referer": {"https://v.qq.com/x/cover/" + id + ".html"}})
	if e != nil {
		return nil, e
	}
	var v map[string]any
	if e = decodeJSON(raw, &v); e != nil {
		return nil, e
	}
	if v["ret"] == nil || integer(v["ret"]) != 0 {
		return nil, errors.New("tencent episode RPC rejected request")
	}
	if at(v, "data", "module_list_datas") == nil {
		return nil, errors.New("tencent episode response missing modules")
	}
	return v, nil
}
func tencentModules(v map[string]any) []map[string]any {
	out := []map[string]any{}
	for _, a := range array(at(v, "data", "module_list_datas")) {
		for _, b := range array(object(a)["module_datas"]) {
			out = append(out, object(b))
		}
	}
	return out
}
func (l *Legacy) tencentEpisodes(ctx context.Context, id string) ([]Episode, error) {
	if !alphaID.MatchString(id) {
		return nil, errors.New("invalid tencent cover ID")
	}
	v, e := l.tencentPage(ctx, id, "cid="+id+"&detail_page_type=1&req_from=web_vsite&req_from_second_type=&req_type=0")
	if e != nil {
		return nil, e
	}
	tabs := []map[string]any{}
	for _, m := range tencentModules(v) {
		s := str(at(m, "module_params", "tabs"))
		if s != "" {
			if e = decodeJSON([]byte(s), &tabs); e != nil {
				return nil, e
			}
			break
		}
	}
	if len(tabs) == 0 {
		return nil, errors.New("tencent episode tabs unavailable; generic fallback not implemented")
	}
	if len(tabs) > l.maxSegments() {
		return nil, errors.New("too many tencent episode tabs")
	}
	out := []Episode{}
	seen := map[string]bool{}
	for _, tab := range tabs {
		pc := str(tab["page_context"])
		if pc == "" || len(pc) > 16384 {
			return nil, errors.New("invalid tencent episode tab context")
		}
		v, e = l.tencentPage(ctx, id, pc)
		if e != nil {
			return nil, e
		}
		for _, m := range tencentModules(v) {
			for _, a := range array(at(m, "item_data_lists", "item_datas")) {
				r := object(object(a)["item_params"])
				vid := str(r["vid"])
				if vid == "" || seen[vid] || str(r["is_trailer"]) == "1" {
					continue
				}
				seen[vid] = true
				out = append(out, Episode{ID: vid, Title: strings.TrimSpace(textFirst(r["union_title"], r["title"])), Index: len(out) + 1, URL: "https://v.qq.com/x/cover/" + id + "/" + vid + ".html"})
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		return tencentEpisodeSortIndex(out[i].Title) < tencentEpisodeSortIndex(out[j].Title)
	})
	for i := range out {
		out[i].Index = i + 1
	}
	return out, nil
}
