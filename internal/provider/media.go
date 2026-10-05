// SPDX-License-Identifier: AGPL-3.0-only
package provider

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// ResolveMedia retrieves source-declared media metadata. URL parsing or an
// episode title is never substituted for a missing media title. It returns the
// canonical media ID; ResolveEpisode handles an exact ep/p URL selection.
func (r *Registry) ResolveMedia(ctx context.Context, raw string) (SearchResult, error) {
	name, _, _, e := ResolveURL(raw)
	if e != nil {
		return SearchResult{}, e
	}
	p, ok := r.Get(name)
	if !ok {
		return SearchResult{}, errors.New("provider not registered")
	}
	m, ok := p.(interface {
		ResolveMedia(context.Context, string) (Result, error)
	})
	if !ok {
		return SearchResult{}, &UnsupportedError{name, "media metadata", "native URL metadata retrieval is not implemented for this provider"}
	}
	result, e := m.ResolveMedia(ctx, raw)
	if e != nil {
		return SearchResult{}, e
	}
	return SearchResult{Result: result, Provider: name}, nil
}

func (l *Legacy) ResolveMedia(ctx context.Context, raw string) (Result, error) {
	if len(raw) > 16384 {
		return Result{}, errors.New("provider URL exceeds metadata limit")
	}
	name, _, _, err := ResolveURL(raw)
	if err != nil || name != l.Name() {
		return Result{}, errors.New("URL provider mismatch")
	}
	switch name {
	case "tencent":
		return l.tencentResolveMedia(ctx, raw)
	case "iqiyi":
		return l.iqiyiResolveMedia(ctx, raw)
	case "le":
		return l.leResolveMedia(ctx, raw)
	default:
		return Result{}, &UnsupportedError{name, "media metadata", "native URL metadata retrieval is not implemented for this provider"}
	}
}

// Optional source image metadata is not permission to fetch it. Server-side
// image retrieval retains its separate public-address and content checks.
func sourceMediaImageURL(raw string) string {
	if strings.HasPrefix(raw, "//") {
		raw = "https:" + raw
	}
	if len(raw) > 4096 {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" || u.User != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return ""
	}
	return raw
}

func (b *Bilibili) ResolveMedia(ctx context.Context, raw string) (Result, error) {
	name, id, _, e := ResolveURL(raw)
	if e != nil {
		return Result{}, e
	}
	if name != b.Name() {
		return Result{}, errors.New("URL provider mismatch")
	}
	if i := strings.Index(id, ":p"); i >= 0 {
		id = id[:i]
	}
	var v map[string]any
	if strings.HasPrefix(id, "ss") || strings.HasPrefix(id, "ep") {
		key := "season_id"
		if strings.HasPrefix(id, "ep") {
			key = "ep_id"
		}
		if e = b.get(ctx, "https://api.bilibili.com/pgc/view/web/season", url.Values{key: {id[2:]}}, &v); e != nil {
			return Result{}, e
		}
		if integer(v["code"]) != 0 {
			return Result{}, fmt.Errorf("bilibili media code %s", str(v["code"]))
		}
		m := object(v["result"])
		sid := str(m["season_id"])
		title := strings.TrimSpace(str(m["title"]))
		if !numericID.MatchString(sid) || title == "" {
			return Result{}, errors.New("bilibili season lacks media ID/title")
		}
		typ := "tv_series"
		if integer(m["type"]) == 2 || integer(m["season_type"]) == 2 {
			typ = "movie"
		}
		return Result{ID: "ss" + sid, Title: title, ImageURL: str(m["cover"]), Type: typ, Year: yearFromDate(str(at(m, "publish", "pub_time")))}, nil
	}
	q := url.Values{}
	if strings.HasPrefix(id, "BV") {
		q.Set("bvid", id)
	} else if strings.HasPrefix(id, "av") {
		q.Set("aid", id[2:])
	} else {
		return Result{}, errors.New("unsupported bilibili media ID")
	}
	if e = b.get(ctx, "https://api.bilibili.com/x/web-interface/view", q, &v); e != nil {
		return Result{}, e
	}
	if integer(v["code"]) != 0 {
		return Result{}, fmt.Errorf("bilibili media code %s", str(v["code"]))
	}
	m := object(v["data"])
	title := strings.TrimSpace(str(m["title"]))
	canonical := str(m["bvid"])
	if !strings.HasPrefix(canonical, "BV") || !alphaID.MatchString(canonical) {
		canonical = ""
		if aid := str(m["aid"]); numericID.MatchString(aid) {
			canonical = "av" + aid
		}
	}
	if canonical == "" || title == "" {
		return Result{}, errors.New("bilibili video lacks media ID/title")
	}
	return Result{ID: canonical, Title: title, ImageURL: str(m["pic"]), Type: "other"}, nil
}
