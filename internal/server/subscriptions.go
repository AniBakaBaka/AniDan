// SPDX-License-Identifier: AGPL-3.0-only
// Compatible subscription persistence informed by Misaka (l429609201 et al.).
package server

import (
	"container/heap"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/AniBakaBaka/AniDan/internal/job"
	"github.com/AniBakaBaka/AniDan/internal/provider"
	"github.com/AniBakaBaka/AniDan/internal/store"
)

func (s *Server) registerSubscriptions(m *http.ServeMux) {
	m.HandleFunc("GET /api/ui/subscriptions/available-sources", s.operator(s.subscriptionSources))
	m.HandleFunc("GET /api/ui/subscriptions/discover", s.operator(s.subscriptionDiscover))
	m.HandleFunc("GET /api/ui/subscriptions/discover/offline", s.operator(s.subscriptionOffline))
	m.HandleFunc("GET /api/ui/subscriptions/explore", s.operator(s.subscriptionList))
	m.HandleFunc("GET /api/ui/subscriptions/targets", s.operator(s.subscriptionList))
	m.HandleFunc("POST /api/ui/subscriptions/targets", s.operator(s.subscriptionCreate))
	m.HandleFunc("PATCH /api/ui/subscriptions/targets/{target_id}", s.operator(s.subscriptionPatch))
	m.HandleFunc("DELETE /api/ui/subscriptions/targets/{target_id}", s.operator(s.subscriptionDelete))
	m.HandleFunc("POST /api/ui/subscriptions/targets/{target_id}/scan", s.operator(s.subscriptionScanHTTP))
	m.HandleFunc("GET /api/ui/subscriptions/items", s.operator(s.subscriptionList))
	m.HandleFunc("POST /api/ui/subscriptions/items/{item_id}/retry", s.operator(s.subscriptionItemStatus))
	m.HandleFunc("POST /api/ui/subscriptions/items/{item_id}/ignore", s.operator(s.subscriptionItemStatus))
	m.HandleFunc("POST /api/ui/subscriptions/resolve-url", s.operator(s.subscriptionResolve))
}
func subscriptionExtra(row store.Row) (map[string]any, error) {
	out := map[string]any{}
	if str(row["extra_data"]) == "" {
		return out, nil
	}
	d := json.NewDecoder(strings.NewReader(str(row["extra_data"])))
	d.UseNumber()
	if e := d.Decode(&out); e != nil || out == nil {
		return nil, errors.New("invalid subscription extra_data; original value preserved")
	}
	return out, nil
}
func subscriptionWire(row store.Row) map[string]any {
	out := camelRow(row)
	delete(out, "extraData")
	out["isSubscribed"] = boolean(row["is_subscribed"])
	out["subscriptionFailureCount"] = number(row["subscription_failure_count"])
	for _, k := range []string{"rating", "platformRating"} {
		if out[k] != nil {
			if n, e := strconv.ParseFloat(str(out[k]), 64); e == nil {
				out[k] = n
			}
		}
	}
	extra, e := subscriptionExtra(row)
	if e != nil {
		out["extraDataError"] = e.Error()
		return out
	}
	for k, v := range extra {
		if _, exists := out[k]; !exists {
			out[k] = v
		}
	}
	return out
}

// Iterate pages instead of loading every legacy JSON payload for filtering.
func (s *Server) subscriptionEach(ctx context.Context, filter store.Row, fn func(store.Row) error) error {
	for off := 0; ; off += 250 {
		rows, e := s.Store.List(ctx, "external_calendar_item", filter, 250, off)
		if e != nil {
			return e
		}
		for _, row := range rows {
			if e = fn(row); e != nil {
				return e
			}
		}
		if len(rows) < 250 {
			return nil
		}
		if e = ctx.Err(); e != nil {
			return e
		}
	}
}
func subscriptionPage(r *http.Request, defaultSize, maxSize int) (int, int, error) {
	page, size := 1, defaultSize
	for key, p := range map[string]*int{"page": &page, "pageSize": &size} {
		if raw := r.URL.Query().Get(key); raw != "" {
			n, e := strconv.Atoi(raw)
			if e != nil {
				return 0, 0, errors.New("invalid pagination")
			}
			*p = n
		}
	}
	if page < 1 || page > 1000000 || size < 1 || size > maxSize {
		return 0, 0, errors.New("pagination out of bounds")
	}
	return page, size, nil
}
func (s *Server) subscriptionList(w http.ResponseWriter, r *http.Request) {
	kind := "targets"
	if strings.HasSuffix(r.URL.Path, "/items") {
		kind = "items"
	}
	if strings.HasSuffix(r.URL.Path, "/explore") {
		kind = "explore"
	}
	maxSize, defaultSize := 200, 20
	if kind == "explore" {
		maxSize, defaultSize = 100, 30
	}
	page, size, e := subscriptionPage(r, defaultSize, maxSize)
	if e != nil {
		httpError(w, 422, e.Error())
		return
	}
	q := r.URL.Query()
	filter := store.Row{"is_subscribed": kind == "targets"}
	if q.Get("provider") != "" {
		filter["provider"] = q.Get("provider")
	}
	out := []map[string]any{}
	total := 0
	start := (page - 1) * size
	// Exploration needs global rating order, but retains only page-end highest rows.
	keep := start + size
	ranked := &subscriptionHeap{}
	if kind == "explore" && keep > 10000 {
		httpError(w, 422, "exploration page depth exceeds10,000 result window")
		return
	}
	e = s.subscriptionEach(r.Context(), filter, func(row store.Row) error {
		v := subscriptionWire(row)
		if v["extraDataError"] != nil {
			return errors.New("corrupt subscription payload; repair required")
		}
		if kind == "items" && str(v["parentExternalId"]) == "" {
			return nil
		}
		if kind == "explore" && (str(v["parentExternalId"]) != "" || str(v["exploreCategory"]) == "") {
			return nil
		}
		for _, pair := range [][2]string{{"type", "subscriptionType"}, {"parentExternalId", "parentExternalId"}, {"category", "exploreCategory"}} {
			if q.Get(pair[0]) != "" && str(v[pair[1]]) != q.Get(pair[0]) {
				return nil
			}
		}
		statusKey := "subscriptionStatus"
		if kind == "items" {
			statusKey = "itemStatus"
		}
		if q.Get("status") != "" && str(v[statusKey]) != q.Get("status") {
			return nil
		}
		if kw := strings.ToLower(q.Get("keyword")); kw != "" && !strings.Contains(strings.ToLower(str(v["animeTitle"])+str(v["titleZh"])+str(v["externalId"])), kw) {
			return nil
		}
		if kind == "explore" {
			if ranked.Len() < keep {
				heap.Push(ranked, v)
			} else if subscriptionBetter(v, (*ranked)[0]) {
				(*ranked)[0] = v
				heap.Fix(ranked, 0)
			}
		} else if total >= start && len(out) < size {
			out = append(out, v)
		}
		total++
		return nil
	})
	if e != nil {
		libWriteError(w, e)
		return
	}
	if kind == "explore" {
		out = []map[string]any(*ranked)
		sort.SliceStable(out, func(i, j int) bool { return subscriptionBetter(out[i], out[j]) })
		if start >= len(out) {
			out = []map[string]any{}
		} else {
			out = out[start:]
		}
	}
	writeJSON(w, 200, map[string]any{"total": total, "list": out})
}
func subscriptionRating(v map[string]any) float64 {
	if v["rating"] == nil {
		return -1
	}
	n, _ := strconv.ParseFloat(str(v["rating"]), 64)
	return n
}
func (s *Server) subscriptionCapable(name string) bool {
	for _, c := range s.Providers.Catalog() {
		if c.Name == name {
			return c.Episodes && c.Comments
		}
	}
	return false
}
func subscriptionTypeFor(name, id string) string {
	if name == "bilibili" {
		if strings.HasPrefix(id, "ss") || strings.HasPrefix(id, "ep") {
			return "bilibili_bangumi"
		}
		return "bilibili_video"
	}
	return name + "_series"
}
func (s *Server) subscriptionSources(w http.ResponseWriter, r *http.Request) {
	entries := []map[string]any{}
	for _, c := range s.Providers.Catalog() {
		row, e := s.Store.Get(r.Context(), "scrapers", c.Name)
		if e != nil && !errors.Is(e, sql.ErrNoRows) {
			libWriteError(w, e)
			return
		}
		if row != nil && !boolean(row["is_enabled"]) {
			continue
		}
		if !c.Episodes || !c.Comments {
			continue
		}
		types := []map[string]any{{"type": c.Name + "_series", "label": "系列分集", "description": "以媒体 ID 订阅系列及新增分集", "fields": []map[string]any{{"key": "mediaId", "label": "媒体 ID", "type": "text", "required": true}, {"key": "title", "label": "标题", "type": "text", "required": true}}}}
		if c.Name == "bilibili" {
			types = []map[string]any{{"type": "bilibili_bangumi", "label": "番剧", "fields": []map[string]any{{"key": "mediaId", "label": "ss/ep ID", "type": "text", "required": true}}}, {"type": "bilibili_video", "label": "视频分P", "fields": []map[string]any{{"key": "mediaId", "label": "BV/av ID", "type": "text", "required": true}}}}
		}
		if c.Name == "bilibili" {
			types = append(types, map[string]any{"type": "bilibili_up", "label": "UP 主", "description": "按公开UID/空间链接订阅视频"}, map[string]any{"type": "bilibili_favorites", "label": "收藏夹", "description": "按收藏夹ID/空间收藏链接订阅视频"}, map[string]any{"type": "bilibili_watchlist", "label": "用户追番", "description": "按UID读取追番列表；私有列表需对应账号配置"}, map[string]any{"type": "bilibili_drama_watchlist", "label": "用户追剧", "description": "按UID读取追剧列表；私有列表需对应账号配置"})
		}

		features := []string{}
		for _, v := range types {
			features = append(features, str(v["type"]))
		}
		entries = append(entries, map[string]any{"provider": c.Name, "displayName": c.Name, "sourceType": "danmaku", "available": true, "authRequired": false, "authStatus": "provider-config", "features": features, "subscriptionTypes": types, "handledDomains": []string{}, "reason": "Native series capability; live provider availability and account restrictions depend on source. Bilibili public UP/favorite discovery is available; private watchlists are not implied."})
	}
	writeJSON(w, 200, map[string]any{"danmakuSources": entries, "calendarSources": []any{}, "summary": map[string]any{"availableCount": len(entries), "unavailableCount": 0}})
}
func (s *Server) subscriptionDiscoverItems(ctx context.Context, name, query, typ string) ([]map[string]any, error) {
	if !s.subscriptionCapable(name) {
		return nil, libErr(400, "Source does not support native series subscriptions")
	}
	if name == "bilibili" {
		_, _, catalogURL := subscriptionCatalogURL(query)
		if subscriptionCatalogType(typ) || catalogURL {
			return s.subscriptionCatalogDiscover(ctx, query, typ)
		}
	}
	p, _ := s.Providers.Get(name)
	out := []map[string]any{}
	if strings.Contains(query, "://") {
		n, media, _, e := provider.ResolveURL(query)
		if e != nil || n != name || media == "" {
			return nil, libErr(400, "URL does not identify a supported series")
		}
		eps, e := p.Episodes(ctx, media)
		if e != nil {
			return nil, e
		}
		if len(eps) == 0 {
			return nil, libErr(404, "No episodes found")
		}
		out = append(out, map[string]any{"type": subscriptionTypeFor(name, media), "title": eps[0].Title, "cover": nil, "description": "原生系列订阅", "payload": map[string]any{"mediaId": media, "title": eps[0].Title, "url": query}})
	} else {
		items, e := p.Search(ctx, query)
		if e != nil {
			return nil, e
		}
		for _, v := range items {
			kind := subscriptionTypeFor(name, v.ID)
			if typ != "" && typ != kind && typ != name+"_series" {
				continue
			}
			out = append(out, map[string]any{"type": kind, "title": v.Title, "cover": v.ImageURL, "description": v.Type, "payload": map[string]any{"mediaId": v.ID, "title": v.Title, "imageUrl": v.ImageURL, "season": v.Season, "animeType": v.Type}})
		}
	}
	return out, nil
}
func (s *Server) subscriptionDiscover(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if q.Get("query") == "" || q.Get("provider") == "" {
		httpError(w, 422, "provider and query required")
		return
	}
	out, e := s.subscriptionDiscoverItems(r.Context(), q.Get("provider"), q.Get("query"), q.Get("type"))
	if e != nil {
		libWriteError(w, e)
		return
	}
	writeJSON(w, 200, map[string]any{"list": out})
}
func (s *Server) subscriptionResolve(w http.ResponseWriter, r *http.Request) {
	var in struct {
		URL string `json:"url"`
	}
	if e := readJSON(r, &in); e != nil {
		httpError(w, 422, e.Error())
		return
	}
	if _, _, ok := subscriptionCatalogURL(in.URL); ok {
		items, e := s.subscriptionCatalogDiscover(r.Context(), in.URL, "")
		if e != nil {
			libWriteError(w, e)
			return
		}
		writeJSON(w, 200, map[string]any{"provider": "bilibili", "list": items})
		return
	}
	name, _, _, e := provider.ResolveURL(in.URL)
	if e != nil {
		httpError(w, 400, "Unsupported source URL")
		return
	}
	items, e := s.subscriptionDiscoverItems(r.Context(), name, in.URL, "")
	if e != nil {
		libWriteError(w, e)
		return
	}
	writeJSON(w, 200, map[string]any{"provider": name, "list": items})
}
func (s *Server) subscriptionOffline(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	kw := strings.TrimSpace(q.Get("query"))
	if kw == "" {
		httpError(w, 422, "query required")
		return
	}
	out := []map[string]any{}
	for off := 0; ; off += 250 {
		rows, e := s.Store.List(r.Context(), "bangumi_data_index", nil, 250, off)
		if e != nil {
			libWriteError(w, e)
			return
		}
		for _, row := range rows {
			hay := strings.ToLower(str(row["title_main"]) + str(row["title_zh"]) + str(row["title_en"]) + str(row["titles_all"]))
			if !strings.Contains(hay, strings.ToLower(kw)) {
				continue
			}
			var sites any
			_ = json.Unmarshal([]byte(str(row["sites"])), &sites)
			title := str(row["title_zh"])
			if title == "" {
				title = str(row["title_main"])
			}
			out = append(out, map[string]any{"type": "bangumi_data_subject", "title": title, "cover": nil, "description": row["begin_date"], "payload": map[string]any{"id": row["id"], "bangumiId": row["bangumi_id"], "sites": sites, "title": title}})
			if len(out) >= 200 {
				break
			}
		}
		if len(rows) < 250 || len(out) >= 200 {
			break
		}
	}
	warnings := []string{}
	if name := q.Get("onlineProvider"); name != "" {
		items, e := s.subscriptionDiscoverItems(r.Context(), name, kw, "")
		if e != nil {
			warnings = append(warnings, e.Error())
		} else {
			out = append(out, items...)
		}
	}
	writeJSON(w, 200, map[string]any{"list": out, "warnings": warnings, "limit": 200})
}

// calUpsertTx preserves subscription intent and unknown extraData on refresh.
func (s *Server) calUpsertTx(ctx context.Context, tx *sql.Tx, name, external string, patch store.Row, extra map[string]any) (store.Row, error) {
	if name == "" || external == "" || len(name) > 50 || len(external) > 500 {
		return nil, libErr(400, "invalid external identity")
	}
	old, e := s.libOne(ctx, tx, "external_calendar_item", "provider=? AND external_id=?", name, external)
	if e != nil && !errors.Is(e, sql.ErrNoRows) {
		return nil, e
	}
	values := store.Row{}
	for k, v := range patch {
		values[k] = v
	}
	values["updated_at"] = s.now()
	if extra != nil {
		merged := map[string]any{}
		if old != nil {
			merged, e = subscriptionExtra(old)
			if e != nil {
				return nil, e
			}
		}
		for k, v := range extra {
			merged[k] = v
		}
		b, e := json.Marshal(merged)
		if e != nil {
			return nil, e
		}
		values["extra_data"] = string(b)
	}
	var id int64
	if old == nil {
		for _, c := range store.Schema["external_calendar_item"].Columns {
			if _, exists := values[c.Name]; !exists && c.HasDefault {
				values[c.Name] = c.Default
			}
		}
		values["provider"] = name
		values["external_id"] = external
		values["fetched_at"] = s.now()
		if values["anime_title"] == nil {
			values["anime_title"] = ""
		}
		id, e = s.Store.InsertTx(ctx, tx, "external_calendar_item", values)
	} else {
		id = number(old["id"])
		e = s.libUpdate(ctx, tx, "external_calendar_item", id, values)
	}
	if e != nil {
		return nil, e
	}
	return s.libOne(ctx, tx, "external_calendar_item", "id=?", id)
}
func (s *Server) calUpsert(ctx context.Context, name, external string, patch store.Row, extra map[string]any) (store.Row, error) {
	s.importMu.Lock()
	defer s.importMu.Unlock()
	tx, e := s.Store.DB.BeginTx(ctx, nil)
	if e != nil {
		return nil, e
	}
	defer tx.Rollback()
	row, e := s.calUpsertTx(ctx, tx, name, external, patch, extra)
	if e != nil {
		return nil, e
	}
	return row, tx.Commit()
}
func (s *Server) subscriptionCreate(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Provider string         `json:"provider"`
		Type     string         `json:"type"`
		Payload  map[string]any `json:"payload"`
		RunNow   bool           `json:"runNow"`
	}
	if e := readJSON(r, &in); e != nil {
		httpError(w, 422, e.Error())
		return
	}
	if !s.subscriptionCapable(in.Provider) {
		httpError(w, 400, "Source does not support native series subscriptions")
		return
	}
	if in.Provider == "bilibili" && subscriptionCatalogType(in.Type) {
		s.subscriptionCatalogCreate(w, r, in.Type, in.Payload, in.RunNow)
		return
	}
	id := str(in.Payload["mediaId"])
	if id == "" {
		id = str(in.Payload["externalId"])
	}
	if id == "" {
		id = str(in.Payload["seasonId"])
		if in.Provider == "bilibili" && id != "" {
			id = "ss" + strings.TrimPrefix(id, "ss")
		}
	}
	if id == "" {
		id = str(in.Payload["bvid"])
	}
	if id == "" || len(id) > 500 || strings.ContainsAny(id, "\x00\r\n") {
		httpError(w, 400, "mediaId required")
		return
	}
	if in.Type != in.Provider+"_series" && in.Type != subscriptionTypeFor(in.Provider, id) {
		httpError(w, 400, "This subscription type needs an unimplemented source capability; series only")
		return
	}
	p, _ := s.Providers.Get(in.Provider)
	eps, e := p.Episodes(r.Context(), id)
	if e != nil {
		httpError(w, 502, "Provider validation failed: "+e.Error())
		return
	}
	if len(eps) == 0 {
		httpError(w, 400, "Source returned no episodes")
		return
	}
	title := str(in.Payload["title"])
	if title == "" {
		title = eps[0].Title
	}
	extra := map[string]any{}
	for k, v := range in.Payload {
		extra[k] = v
	}
	extra["mediaId"] = id
	extra["subscriptionType"] = in.Type
	extra["enabled"] = true
	row, e := s.calUpsert(r.Context(), in.Provider, id, store.Row{"anime_title": title, "anime_type": "subscription", "is_subscribed": true, "subscription_status": "pending"}, extra)
	if e != nil {
		libWriteError(w, e)
		return
	}
	var task any
	if in.RunNow {
		id, e := s.Jobs.SubmitRegistered("subscription_target", map[string]any{"targetId": row["id"]})
		if e != nil {
			jobHTTPError(w, e)
			return
		}
		task = id
	}
	writeJSON(w, 201, map[string]any{"id": row["id"], "provider": in.Provider, "externalId": id, "type": in.Type, "title": title, "status": row["subscription_status"], "message": "订阅目标已创建", "taskId": task})
}
func (s *Server) subscriptionTarget(r *http.Request, param string) (store.Row, error) {
	id, e := strconv.ParseInt(r.PathValue(param), 10, 64)
	if e != nil || id < 1 {
		return nil, libErr(422, "invalid target ID")
	}
	return s.Store.Get(r.Context(), "external_calendar_item", id)
}
func (s *Server) subscriptionPatch(w http.ResponseWriter, r *http.Request) {
	row, e := s.subscriptionTarget(r, "target_id")
	if e != nil {
		libWriteError(w, e)
		return
	}
	var in struct {
		Enabled    *bool          `json:"enabled"`
		Status     *string        `json:"status"`
		ExtraPatch map[string]any `json:"extraPatch"`
	}
	if e = readJSON(r, &in); e != nil {
		httpError(w, 422, e.Error())
		return
	}
	if in.ExtraPatch == nil {
		in.ExtraPatch = map[string]any{}
	}
	if in.Enabled != nil {
		in.ExtraPatch["enabled"] = *in.Enabled
	}
	patch := store.Row{}
	if in.Status != nil {
		if !subscriptionStatusAllowed(*in.Status) {
			httpError(w, 422, "invalid subscription status")
			return
		}
		patch["subscription_status"] = *in.Status
	}
	_, e = s.calUpsert(r.Context(), str(row["provider"]), str(row["external_id"]), patch, in.ExtraPatch)
	if e != nil {
		libWriteError(w, e)
		return
	}
	writeJSON(w, 200, map[string]any{"success": true})
}
func subscriptionStatusAllowed(v string) bool {
	switch v {
	case "pending", "importing", "active", "failed", "completed", "ignored", "subscribed":
		return true
	}
	return false
}
func (s *Server) subscriptionDelete(w http.ResponseWriter, r *http.Request) {
	row, e := s.subscriptionTarget(r, "target_id")
	if e != nil {
		libWriteError(w, e)
		return
	}
	e = s.Store.Update(r.Context(), "external_calendar_item", row["id"], store.Row{"is_subscribed": false, "subscription_status": nil, "subscription_failure_count": 0, "subscription_last_attempt_at": nil, "updated_at": s.now()})
	if e != nil {
		libWriteError(w, e)
		return
	}
	writeJSON(w, 200, map[string]any{"success": true})
}
func (s *Server) subscriptionItemStatus(w http.ResponseWriter, r *http.Request) {
	row, e := s.subscriptionTarget(r, "item_id")
	if e != nil {
		libWriteError(w, e)
		return
	}
	extra, e := subscriptionExtra(row)
	if e != nil {
		libWriteError(w, e)
		return
	}
	if str(extra["parentExternalId"]) == "" {
		httpError(w, 400, "Not a subscription candidate")
		return
	}
	status := "waiting"
	if strings.HasSuffix(r.URL.Path, "/ignore") {
		status = "ignored"
	}
	_, e = s.calUpsert(r.Context(), str(row["provider"]), str(row["external_id"]), nil, map[string]any{"itemStatus": status, "lastError": nil})
	if e != nil {
		libWriteError(w, e)
		return
	}
	writeJSON(w, 200, map[string]any{"success": true})
}
func (s *Server) subscriptionScanHTTP(w http.ResponseWriter, r *http.Request) {
	row, e := s.subscriptionTarget(r, "target_id")
	if e != nil {
		libWriteError(w, e)
		return
	}
	n, e := s.scanSubscription(r.Context(), row, false, func(int, string) {})
	if e != nil {
		libWriteError(w, e)
		return
	}
	writeJSON(w, 200, map[string]any{"scanned": n, "message": fmt.Sprintf("扫描完成，写入 %d 个候选项", n)})
}
func (s *Server) scanSubscription(ctx context.Context, target store.Row, download bool, progress func(int, string)) (int, error) {
	extra, e := subscriptionExtra(target)
	if e != nil {
		return 0, e
	}
	if extra["enabled"] == false || !boolean(target["is_subscribed"]) {
		return 0, libErr(409, "Subscription is paused or cancelled")
	}
	name, id := str(target["provider"]), str(target["external_id"])
	if name == "anibt" && str(extra["subscriptionType"]) == "anibt_rss_feed" {
		return s.scanAniBTRSS(ctx, target, extra)
	}
	if name == "bilibili" && subscriptionCatalogType(str(extra["subscriptionType"])) {
		return s.scanSubscriptionCatalog(ctx, target, extra, download, progress)
	}
	if mid := str(extra["mediaId"]); mid != "" {
		id = mid
	}
	if !s.subscriptionCapable(name) {
		return 0, libErr(400, "Provider lacks native series subscription capability")
	}
	p, _ := s.Providers.Get(name)
	eps, e := p.Episodes(ctx, id)
	if e != nil {
		return 0, e
	}
	if len(eps) > 20000 {
		return 0, errors.New("source episode list exceeds20,000 safety limit")
	}
	// Snapshot scan results in one transaction; preserve ignored/completed items.
	s.importMu.Lock()
	tx, e := s.Store.DB.BeginTx(ctx, nil)
	if e != nil {
		s.importMu.Unlock()
		return 0, e
	}
	selected := []ImportEpisode{}
	n := 0
	for _, ep := range eps {
		if e = ctx.Err(); e != nil {
			break
		}
		external := id + ":" + ep.ID
		if len(external) > 500 {
			e = errors.New("candidate ID too long")
			break
		}
		old, err := s.libOne(ctx, tx, "external_calendar_item", "provider=? AND external_id=?", name, external)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			e = err
			break
		}
		status := "waiting"
		if old != nil {
			ex, err := subscriptionExtra(old)
			if err != nil {
				e = err
				break
			}
			if str(ex["parentExternalId"]) != "" && str(ex["parentExternalId"]) != str(target["external_id"]) {
				e = errors.New("candidate belongs to another target")
				break
			}
			if str(ex["itemStatus"]) != "" {
				status = str(ex["itemStatus"])
			}
		}
		_, e = s.calUpsertTx(ctx, tx, name, external, store.Row{"anime_title": ep.Title, "anime_type": "episode_candidate"}, map[string]any{"parentExternalId": str(target["external_id"]), "subscriptionType": str(extra["subscriptionType"]), "itemStatus": status, "episodeId": ep.ID, "episodeIndex": ep.Index, "mediaId": id})
		if e != nil {
			break
		}
		n++
		if status != "ignored" && status != "completed" && status != "downloaded" {
			selected = append(selected, ImportEpisode{ID: ep.ID, Title: ep.Title, Index: ep.Index, URL: ep.URL})
		}
	}
	if e == nil {
		_, e = s.calUpsertTx(ctx, tx, name, str(target["external_id"]), nil, map[string]any{"lastScanAt": s.now(), "nextScanAt": s.calNowTime().Add(15 * time.Minute).Format("2006-01-02T15:04:05"), "lastError": nil})
	}
	if e == nil {
		e = tx.Commit()
	} else {
		_ = tx.Rollback()
	}
	s.importMu.Unlock()
	if e != nil {
		return 0, e
	}
	if !download || len(selected) == 0 {
		return n, nil
	}
	req := ImportRequest{Provider: name, MediaID: id, Title: str(target["anime_title"]), Type: "tv_series", Season: 1, Episodes: selected}
	if typ := str(extra["animeType"]); typ == "movie" {
		req.Type = typ
	}
	if season := int(number(extra["season"])); season > 0 {
		req.Season = season
	}
	raw, _ := json.Marshal(req)
	result, e := s.runImport(ctx, raw, progress)
	if e != nil {
		return n, e
	}
	// Only mark completion after the import handler has verified its writes.
	var resultRow map[string]any
	b, _ := json.Marshal(result)
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.UseNumber()
	_ = dec.Decode(&resultRow)
	for _, ep := range selected {
		if _, e = s.calUpsert(ctx, name, id+":"+ep.ID, nil, map[string]any{"itemStatus": "completed", "lastError": nil}); e != nil {
			return n, e
		}
	}
	_, e = s.calUpsert(ctx, name, str(target["external_id"]), store.Row{"subscription_status": "active", "local_anime_id": resultRow["animeId"], "local_source_id": resultRow["sourceId"]}, nil)
	return n, e
}
func (s *Server) initSubscriptionJobs() error {
	if e := s.Jobs.Register("subscription_target", func(ctx context.Context, raw json.RawMessage, p func(int, string)) (any, error) {
		var v struct {
			TargetID int64 `json:"targetId"`
		}
		if e := unmarshalExactJSON(raw, &v); e != nil {
			return nil, e
		}
		row, e := s.Store.Get(ctx, "external_calendar_item", v.TargetID)
		if e != nil {
			return nil, e
		}
		n, e := s.scanSubscription(ctx, row, true, p)
		if e != nil {
			_, _ = s.calUpsert(ctx, str(row["provider"]), str(row["external_id"]), store.Row{"subscription_status": "failed", "subscription_failure_count": number(row["subscription_failure_count"]) + 1}, map[string]any{"lastError": e.Error()})
		}
		return map[string]any{"scanned": n}, e
	}); e != nil {
		return e
	}
	return s.Jobs.Register("subscriptionScan", func(ctx context.Context, raw json.RawMessage, p func(int, string)) (any, error) {
		ids := []int64{}
		kinds := map[int64]string{}
		e := s.subscriptionEach(ctx, store.Row{"is_subscribed": true}, func(row store.Row) error {
			ex, e := subscriptionExtra(row)
			if e != nil {
				return e
			}
			if ex["enabled"] == false {
				return nil
			}
			if date, err := s.authDate(ex["nextScanAt"]); err == nil && date.After(s.calNowTime()) {
				return nil
			}
			if len(ids) < 50 {
				kind := "subscription_target"
				if str(ex["subscriptionType"]) == "" || str(ex["subscriptionType"]) == "anibt_subject" {
					if str(row["subscription_status"]) == "active" {
						return nil
					}
					kind = "calendar_import"
				}
				id := number(row["id"])
				ids = append(ids, id)
				kinds[id] = kind
			}
			return nil
		})
		if e != nil {
			return nil, e
		}
		submitted := []string{}
		for _, id := range ids {
			if e = job.Checkpoint(ctx); e != nil {
				return nil, e
			}
			task, e := s.Jobs.SubmitWithOptions(kinds[id], map[string]any{"targetId": id}, nil, job.SubmitOptions{Title: "扫描订阅", UniqueKey: fmt.Sprintf("subscription:%d", id)})
			if e != nil {
				return map[string]any{"submitted": submitted}, e
			}
			submitted = append(submitted, task)
		}
		return map[string]any{"submitted": submitted}, nil
	})
}

func (s *Server) calNowTime() time.Time {
	loc, e := time.LoadLocation(s.Config.Timezone)
	if e != nil {
		return time.Now().UTC()
	}
	return time.Now().In(loc)
}

type subscriptionHeap []map[string]any

func (h subscriptionHeap) Len() int           { return len(h) }
func (h subscriptionHeap) Less(i, j int) bool { return subscriptionBetter(h[j], h[i]) }
func (h subscriptionHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *subscriptionHeap) Push(v any)        { *h = append(*h, v.(map[string]any)) }
func (h *subscriptionHeap) Pop() any {
	old := *h
	n := len(old)
	v := old[n-1]
	old[n-1] = nil
	*h = old[:n-1]
	return v
}
func subscriptionBetter(a, b map[string]any) bool {
	x, y := subscriptionRating(a), subscriptionRating(b)
	if x != y {
		return x > y
	}
	return number(a["id"]) < number(b["id"])
}
