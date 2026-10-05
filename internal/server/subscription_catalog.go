// SPDX-License-Identifier: AGPL-3.0-only
package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"github.com/AniBakaBaka/AniDan/internal/job"
	"github.com/AniBakaBaka/AniDan/internal/provider"
	"github.com/AniBakaBaka/AniDan/internal/store"
	"net/http"
	"net/url"
	"regexp"
	"strings"
)

var subscriptionNumeric = regexp.MustCompile(`^[0-9]{1,20}$`)

type subscriptionVideoLister interface {
	ListSubscriptionVideos(context.Context, string, string, bool) (provider.SubscriptionVideos, error)
}

func subscriptionCatalogType(kind string) bool {
	return kind == "bilibili_up" || kind == "bilibili_favorites" || kind == "bilibili_watchlist" || kind == "bilibili_drama_watchlist"
}
func subscriptionCatalogURL(raw string) (kind, id string, ok bool) {
	u, e := url.Parse(raw)
	if e != nil || u.User != nil || u.Port() != "" || u.Fragment != "" || (u.Scheme != "https" && u.Scheme != "http") || strings.ToLower(u.Hostname()) != "space.bilibili.com" {
		return
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) == 0 || !subscriptionNumeric.MatchString(parts[0]) {
		return
	}
	if len(parts) == 1 || (len(parts) == 2 && parts[1] == "upload") {
		return "bilibili_up", parts[0], true
	}
	if len(parts) == 2 && parts[1] == "favlist" && subscriptionNumeric.MatchString(u.Query().Get("fid")) {
		return "bilibili_favorites", u.Query().Get("fid"), true
	}
	return
}
func (s *Server) subscriptionCatalogList(ctx context.Context, kind, id string, all bool) (provider.SubscriptionVideos, error) {
	p, ok := s.Providers.Get("bilibili")
	if !ok {
		return provider.SubscriptionVideos{}, errors.New("Bilibili provider unavailable")
	}
	l, ok := p.(subscriptionVideoLister)
	if !ok {
		return provider.SubscriptionVideos{}, errors.New("Bilibili catalog discovery unavailable")
	}
	return l.ListSubscriptionVideos(ctx, kind, id, all)
}
func (s *Server) subscriptionCatalogDiscover(ctx context.Context, query, kind string) ([]map[string]any, error) {
	id := strings.TrimSpace(query)
	if strings.Contains(query, "://") {
		k, i, ok := subscriptionCatalogURL(query)
		if !ok {
			return nil, libErr(400, "Unsupported UP/favorite URL")
		}
		if kind != "" && kind != k {
			return nil, libErr(400, "Subscription type does not match URL")
		}
		kind, id = k, i
	}
	if !subscriptionCatalogType(kind) || !subscriptionNumeric.MatchString(id) {
		return nil, libErr(422, "Provide an UP UID or favorite folder ID")
	}
	listing, e := s.subscriptionCatalogList(ctx, kind, id, false)
	if e != nil {
		return nil, e
	}
	payload := map[string]any{"listingId": id, "title": listing.Title}
	if kind != "bilibili_favorites" {
		payload["uid"] = id
	} else {
		payload["fid"] = id
	}
	return []map[string]any{{"type": kind, "title": listing.Title, "cover": listing.Cover, "description": "订阅新增视频；私有内容需要原账号配置，拒绝访问时不会绕过", "payload": payload}}, nil
}
func (s *Server) subscriptionCatalogCreate(w http.ResponseWriter, r *http.Request, kind string, payload map[string]any, runNow bool) {
	id := str(payload["listingId"])
	if id == "" {
		if kind != "bilibili_favorites" {
			id = str(payload["uid"])
			if id == "" {
				id = str(payload["mid"])
			}
		} else {
			id = str(payload["fid"])
			if id == "" {
				id = str(payload["media_id"])
			}
		}
	}
	if !subscriptionNumeric.MatchString(id) {
		httpError(w, 422, "Numeric uid/fid required")
		return
	}
	listing, e := s.subscriptionCatalogList(r.Context(), kind, id, false)
	if e != nil {
		httpError(w, 502, e.Error())
		return
	}
	title := str(payload["title"])
	if title == "" {
		title = listing.Title
	}
	external := kind + ":" + id
	row, e := s.calUpsert(r.Context(), "bilibili", external, store.Row{"anime_title": title, "anime_type": "subscription", "is_subscribed": true, "subscription_status": "pending"}, map[string]any{"subscriptionType": kind, "listingId": id, "enabled": true})
	if e != nil {
		libWriteError(w, e)
		return
	}
	var task any
	if runNow {
		task, e = s.Jobs.SubmitRegistered("subscription_target", map[string]any{"targetId": row["id"]})
		if e != nil {
			jobHTTPError(w, e)
			return
		}
	}
	writeJSON(w, 201, map[string]any{"id": row["id"], "provider": "bilibili", "externalId": external, "type": kind, "title": title, "status": row["subscription_status"], "taskId": task, "message": "订阅目标已创建"})
}
func (s *Server) scanSubscriptionCatalog(ctx context.Context, target store.Row, extra map[string]any, download bool, progress func(int, string)) (int, error) {
	kind, id := str(extra["subscriptionType"]), str(extra["listingId"])
	listing, e := s.subscriptionCatalogList(ctx, kind, id, true)
	if e != nil {
		return 0, e
	}
	if !listing.Complete {
		return 0, errors.New("incomplete subscription listing")
	}
	external := str(target["external_id"])
	selected := []provider.Result{}
	if e = job.Checkpoint(ctx); e != nil {
		return 0, e
	}
	s.importMu.Lock()
	tx, e := s.Store.DB.BeginTx(ctx, nil)
	if e != nil {
		s.importMu.Unlock()
		return 0, e
	}
	e = func() error {
		current, e := s.libOne(ctx, tx, "external_calendar_item", "id=?", target["id"])
		if e != nil {
			return e
		}
		now, e := subscriptionExtra(current)
		if e != nil {
			return e
		}
		if !boolean(current["is_subscribed"]) || now["enabled"] == false {
			return libErr(409, "Subscription paused or cancelled")
		}
		for _, v := range listing.Items {
			if e = ctx.Err(); e != nil {
				return e
			}
			candidate := external + ":media:" + v.ID
			old, err := s.libOne(ctx, tx, "external_calendar_item", "provider=? AND external_id=?", "bilibili", candidate)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			status := "waiting"
			if old != nil {
				prev, err := subscriptionExtra(old)
				if err != nil {
					return err
				}
				if str(prev["parentExternalId"]) != "" && str(prev["parentExternalId"]) != external {
					return errors.New("subscription candidate ownership mismatch")
				}
				if value := str(prev["itemStatus"]); value != "" {
					status = value
				}
			}
			_, e = s.calUpsertTx(ctx, tx, "bilibili", candidate, store.Row{"anime_title": v.Title, "anime_type": "media_candidate"}, map[string]any{"parentExternalId": external, "subscriptionType": kind, "itemStatus": status, "mediaId": v.ID, "imageUrl": v.ImageURL, "animeType": v.Type})
			if e != nil {
				return e
			}
			if status != "ignored" && status != "completed" && status != "downloaded" {
				selected = append(selected, v)
			}
		}
		_, e = s.calUpsertTx(ctx, tx, "bilibili", external, nil, map[string]any{"lastScanAt": s.now(), "lastError": nil, "skippedUnavailableItems": listing.Skipped})
		return e
	}()
	if e == nil {
		e = tx.Commit()
	} else {
		_ = tx.Rollback()
	}
	s.importMu.Unlock()
	if e != nil {
		return 0, e
	}
	if !download {
		return len(listing.Items), nil
	}
	for i, v := range selected {
		if e = job.Checkpoint(ctx); e != nil {
			return len(listing.Items), e
		}
		current, err := s.Store.Get(ctx, "external_calendar_item", target["id"])
		if err != nil {
			return len(listing.Items), err
		}
		now, err := subscriptionExtra(current)
		if err != nil {
			return len(listing.Items), err
		}
		if !boolean(current["is_subscribed"]) || now["enabled"] == false {
			return len(listing.Items), libErr(409, "Subscription paused or cancelled")
		}
		candidate, err := s.authFind(ctx, "external_calendar_item", store.Row{"provider": "bilibili", "external_id": external + ":media:" + v.ID})
		if err != nil {
			return len(listing.Items), err
		}
		state, err := subscriptionExtra(candidate)
		if err != nil {
			return len(listing.Items), err
		}
		if str(state["parentExternalId"]) != external {
			return len(listing.Items), errors.New("subscription candidate ownership changed")
		}
		switch str(state["itemStatus"]) {
		case "ignored", "completed", "downloaded":
			progress((i+1)*100/len(selected), "跳过已忽略或已完成候选")
			continue
		}
		req := ImportRequest{Provider: "bilibili", MediaID: v.ID, Title: v.Title, Type: v.Type, ImageURL: v.ImageURL, Season: v.Season}
		raw, _ := json.Marshal(req)
		_, e = s.runImport(ctx, raw, func(n int, msg string) { progress((i*100+n)/len(selected), msg) })
		if e != nil {
			return len(listing.Items), e
		}
		if e = s.completeSubscriptionCatalogItem(ctx, external+":media:"+v.ID, external); e != nil {
			return len(listing.Items), e
		}
	}
	_, e = s.calUpsert(ctx, "bilibili", external, store.Row{"subscription_status": "active"}, nil)
	return len(listing.Items), e
}

// User ignore decisions made while a request was already in flight still win
// over the worker's completion bookkeeping; downloaded data is not deleted.
func (s *Server) completeSubscriptionCatalogItem(ctx context.Context, id, parent string) error {
	s.importMu.Lock()
	defer s.importMu.Unlock()
	tx, e := s.Store.DB.BeginTx(ctx, nil)
	if e != nil {
		return e
	}
	defer tx.Rollback()
	row, e := s.libOne(ctx, tx, "external_calendar_item", "provider=? AND external_id=?", "bilibili", id)
	if e != nil {
		return e
	}
	extra, e := subscriptionExtra(row)
	if e != nil {
		return e
	}
	if str(extra["parentExternalId"]) != parent {
		return errors.New("subscription candidate ownership changed")
	}
	if str(extra["itemStatus"]) != "ignored" {
		if _, e = s.calUpsertTx(ctx, tx, "bilibili", id, nil, map[string]any{"itemStatus": "completed", "lastError": nil}); e != nil {
			return e
		}
	}
	return tx.Commit()
}
