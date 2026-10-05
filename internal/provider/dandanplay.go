// SPDX-License-Identifier: AGPL-3.0-only
package provider

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"github.com/AniBakaBaka/AniDan/internal/danmaku"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// Dandanplay follows the official signing contract at doc.dandanplay.com/open/.
// AppSecret is supplied by deployment config; it is never hard-coded or logged.
type Dandanplay struct {
	HTTPProvider
	AppID     string
	AppSecret string `json:"-"`
	Now       func() time.Time
}

func NewDandanplay(c *http.Client, appID, secret string) *Dandanplay {
	h := newHTTP(c)
	h.sourceName = "dandanplay"
	return &Dandanplay{HTTPProvider: h, AppID: appID, AppSecret: secret, Now: time.Now}
}
func (d *Dandanplay) Name() string { return "dandanplay" }
func (d *Dandanplay) Capability() Capability {
	status := "fixture-tested; live acceptance pending"
	blocker := ""
	if d.AppID == "" || d.AppSecret == "" {
		status = "configuration required"
		blocker = "registered Dandanplay AppID/AppSecret required"
	}
	return Capability{Name: d.Name(), Search: true, Episodes: true, Comments: true, Status: status, Blocker: blocker}
}
func (d *Dandanplay) api(ctx context.Context, path string, q url.Values, v any) error {
	if d.AppID == "" || d.AppSecret == "" {
		return &UnsupportedError{d.Name(), "request", "registered AppID and AppSecret must be configured"}
	}
	now := time.Now()
	if d.Now != nil {
		now = d.Now()
	}
	ts := strconv.FormatInt(now.Unix(), 10)
	sum := sha256.Sum256([]byte(d.AppID + ts + path + d.AppSecret))
	h := http.Header{"X-AppId": {d.AppID}, "X-Timestamp": {ts}, "X-Signature": {base64.StdEncoding.EncodeToString(sum[:])}}
	b, e := d.request(ctx, http.MethodGet, "https://api.dandanplay.net"+path, q, nil, h)
	if e != nil {
		return e
	}
	return decodeJSON(b, v)
}
func dandanOK(v map[string]any) error {
	if success, ok := v["success"].(bool); ok && !success {
		return fmt.Errorf("dandanplay error %s: %s", str(v["errorCode"]), str(v["errorMessage"]))
	}
	return nil
}
func (d *Dandanplay) Search(ctx context.Context, keyword string) ([]Result, error) {
	var v map[string]any
	if e := d.api(ctx, "/api/v2/search/anime", url.Values{"keyword": {keyword}}, &v); e != nil {
		return nil, e
	}
	if e := dandanOK(v); e != nil {
		return nil, e
	}
	if _, ok := v["animes"]; !ok {
		return nil, errors.New("dandanplay response missing animes")
	}
	out := []Result{}
	for _, a := range array(v["animes"]) {
		m := object(a)
		typ := "tv_series"
		if str(m["type"]) == "movie" || str(m["typeDescription"]) == "剧场版" {
			typ = "movie"
		}
		out = append(out, Result{ID: str(m["animeId"]), Title: str(m["animeTitle"]), Type: typ, ImageURL: str(m["imageUrl"]), Year: yearFromDate(str(m["startDate"])), Season: 1})
	}
	return out, nil
}
func (d *Dandanplay) Episodes(ctx context.Context, id string) ([]Episode, error) {
	if _, e := strconv.ParseInt(id, 10, 64); e != nil {
		return nil, errors.New("invalid Dandanplay anime ID")
	}
	var v map[string]any
	if e := d.api(ctx, "/api/v2/bangumi/"+id, nil, &v); e != nil {
		return nil, e
	}
	if e := dandanOK(v); e != nil {
		return nil, e
	}
	m := object(v["bangumi"])
	if m == nil {
		return nil, errors.New("dandanplay response missing bangumi")
	}
	out := []Episode{}
	for i, a := range array(m["episodes"]) {
		ep := object(a)
		out = append(out, Episode{ID: str(ep["episodeId"]), Title: str(ep["episodeTitle"]), Index: i + 1, URL: str(ep["url"])})
	}
	return out, nil
}
func (d *Dandanplay) Comments(ctx context.Context, id string) ([]danmaku.Comment, error) {
	return d.coalesce(ctx, "dandanplay:"+id, func(work context.Context) ([]danmaku.Comment, error) { return d.comments(work, id) })
}
func (d *Dandanplay) comments(ctx context.Context, id string) ([]danmaku.Comment, error) {
	if _, e := strconv.ParseInt(id, 10, 64); e != nil {
		return nil, errors.New("invalid Dandanplay episode ID")
	}
	var v map[string]any
	if e := d.api(ctx, "/api/v2/comment/"+id, url.Values{"withRelated": {"true"}, "chConvert": {"0"}}, &v); e != nil {
		return nil, e
	}
	if e := dandanOK(v); e != nil {
		return nil, e
	}
	if _, ok := v["comments"]; !ok {
		return nil, errors.New("dandanplay response missing comments")
	}
	totalBytes := int64(0)
	out := []danmaku.Comment{}
	for _, a := range array(v["comments"]) {
		m := object(a)
		cid, _ := strconv.ParseInt(str(m["cid"]), 10, 64)
		c, e := danmaku.FromPlayer(danmaku.Comment{CID: cid, P: str(m["p"]), M: str(m["m"])}, "[dandanplay]")
		if e != nil {
			return nil, e
		}
		out, e = d.appendBudget(out, &totalBytes, c)
		if e != nil {
			return nil, e
		}
	}
	return out, nil
}

func yearFromDate(s string) int {
	if len(s) >= 4 {
		n, _ := strconv.Atoi(s[:4])
		if n >= 1800 && n <= 2200 {
			return n
		}
	}
	return 0
}
