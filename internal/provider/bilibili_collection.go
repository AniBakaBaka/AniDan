// SPDX-License-Identifier: AGPL-3.0-only
package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/AniBakaBaka/AniDan/internal/danmaku"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// CollectionInfo is source-declared membership. Each video contributes its
// primary comment pool; canonical order is the API's sort_reverse=false order.
type CollectionInfo struct {
	SeasonID string `json:"seasonId"`
	Mid      string `json:"mid"`
	Title    string `json:"title"`
	Total    int    `json:"total"`
}

func (v CollectionInfo) MediaID() string { return "collection:" + v.SeasonID + ":" + v.Mid }
func collectionID(s string) bool {
	n, e := strconv.ParseInt(s, 10, 64)
	return e == nil && n > 0 && strconv.FormatInt(n, 10) == s
}
func parseCollectionID(s string) (CollectionInfo, error) {
	p := strings.Split(s, ":")
	if len(p) != 3 || p[0] != "collection" || !collectionID(p[1]) || !collectionID(p[2]) {
		return CollectionInfo{}, errors.New("invalid Bilibili collection identity")
	}
	return CollectionInfo{SeasonID: p[1], Mid: p[2]}, nil
}

type collectionVideo struct {
	AID   scalarString `json:"aid"`
	CID   scalarString `json:"cid"`
	BVID  string       `json:"bvid"`
	Title string       `json:"title"`
}
type collectionView struct {
	collectionVideo
	Season json.RawMessage `json:"ugc_season"`
}
type collectionSeason struct {
	ID       scalarString    `json:"id"`
	Mid      scalarString    `json:"mid"`
	Title    string          `json:"title"`
	Count    *int            `json:"ep_count"`
	Sections json.RawMessage `json:"sections"`
}

func collectionVideoValid(v collectionVideo, cid bool) bool {
	return collectionID(string(v.AID)) && (!cid || collectionID(string(v.CID))) && strings.HasPrefix(v.BVID, "BV") && len(v.BVID) <= 128 && alphaID.MatchString(v.BVID) && len(v.Title) <= 4096
}
func collectionJSONArray(raw []byte, limit int, visit func(json.RawMessage) error) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	tok, e := d.Token()
	if e != nil || tok != json.Delim('[') {
		return errors.New("invalid collection array")
	}
	count := 0
	for d.More() {
		if count >= limit {
			return danmaku.ErrLimit
		}
		count++
		var item json.RawMessage
		if e = d.Decode(&item); e != nil {
			return errors.New("invalid collection item")
		}
		if e = visit(item); e != nil {
			return e
		}
	}
	if _, e = d.Token(); e != nil {
		return errors.New("invalid collection array end")
	}
	var extra any
	if e = d.Decode(&extra); e != io.EOF {
		return errors.New("trailing collection array")
	}
	return nil
}
func collectionInfo(v collectionView) (*CollectionInfo, error) {
	if len(v.Season) == 0 || bytes.Equal(bytes.TrimSpace(v.Season), []byte("null")) {
		return nil, nil
	}
	var s collectionSeason
	if e := decodeJSON(v.Season, &s); e != nil {
		return nil, errors.New("invalid collection metadata")
	}
	if !collectionID(string(s.ID)) || !collectionID(string(s.Mid)) || strings.TrimSpace(s.Title) == "" || len(s.Title) > 4096 || s.Count == nil || *s.Count < 1 || *s.Count > 1000 {
		return nil, errors.New("collection identity/title/count missing or outside bounds")
	}
	return &CollectionInfo{SeasonID: string(s.ID), Mid: string(s.Mid), Title: s.Title, Total: *s.Count}, nil
}
func collectionSeed(v collectionView, expected CollectionInfo) (map[string]collectionVideo, error) {
	out := map[string]collectionVideo{}
	info, e := collectionInfo(v)
	if e != nil {
		return nil, e
	}
	if info == nil || info.SeasonID != expected.SeasonID || info.Mid != expected.Mid {
		return out, nil
	}
	if info.Total != expected.Total {
		return nil, errors.New("collection total changed during discovery")
	}
	var season collectionSeason
	if e = decodeJSON(v.Season, &season); e != nil {
		return nil, e
	}
	if len(season.Sections) == 0 || bytes.Equal(bytes.TrimSpace(season.Sections), []byte("null")) {
		return out, nil
	}
	ids := map[string]bool{}
	e = collectionJSONArray(season.Sections, 100, func(raw json.RawMessage) error {
		var section struct {
			Episodes json.RawMessage `json:"episodes"`
		}
		if e := decodeJSON(raw, &section); e != nil {
			return errors.New("invalid collection section")
		}
		return collectionJSONArray(section.Episodes, 1000-len(out), func(raw json.RawMessage) error {
			var entry struct {
				collectionVideo
				Arc collectionVideo `json:"arc"`
			}
			if e := decodeJSON(raw, &entry); e != nil {
				return errors.New("invalid collection video")
			}
			v := entry.collectionVideo
			if v.AID == "" {
				v.AID = entry.Arc.AID
			}
			if v.CID == "" {
				v.CID = entry.Arc.CID
			}
			if v.BVID == "" {
				v.BVID = entry.Arc.BVID
			}
			if v.Title == "" {
				v.Title = entry.Arc.Title
			}
			if (entry.Arc.AID != "" && entry.Arc.AID != v.AID) || (entry.Arc.CID != "" && entry.Arc.CID != v.CID) || (entry.Arc.BVID != "" && entry.Arc.BVID != v.BVID) {
				return errors.New("conflicting collection video identities")
			}
			if !collectionVideoValid(v, true) {
				return errors.New("collection item lacks bounded aid/cid/bvid")
			}
			if _, ok := out[v.BVID]; ok || ids[string(v.AID)] {
				return errors.New("duplicate collection video")
			}
			if len(out) >= expected.Total {
				return errors.New("collection sections exceed declared count")
			}
			out[v.BVID] = v
			ids[string(v.AID)] = true
			return nil
		})
	})
	return out, e
}
func (b *Bilibili) collectionGet(ctx context.Context, endpoint string, q url.Values, out any) error {
	h := b.HTTPProvider
	if h.MaxBytes <= 0 || h.MaxBytes > 4<<20 {
		h.MaxBytes = 4 << 20
	}
	h.Headers = h.Headers.Clone()
	if h.Headers == nil {
		h.Headers = make(http.Header)
	}
	if strings.Contains(endpoint, "/seasons_archives_list") {
		h.Headers.Set("Referer", "https://space.bilibili.com/")
	}
	return h.get(ctx, endpoint, q, out)
}
func (b *Bilibili) collectionView(ctx context.Context, id string) (collectionView, error) {
	q := url.Values{}
	switch {
	case strings.HasPrefix(id, "BV") && len(id) <= 128 && alphaID.MatchString(id):
		q.Set("bvid", id)
	case strings.HasPrefix(id, "av") && collectionID(id[2:]):
		q.Set("aid", id[2:])
	default:
		return collectionView{}, errors.New("UGC video URL required")
	}
	var body struct {
		Code *int            `json:"code"`
		Data *collectionView `json:"data"`
	}
	if e := b.collectionGet(ctx, "https://api.bilibili.com/x/web-interface/view", q, &body); e != nil {
		return collectionView{}, e
	}
	if body.Code == nil {
		return collectionView{}, errors.New("bilibili collection video response lacks code")
	}
	if *body.Code != 0 {
		return collectionView{}, fmt.Errorf("bilibili collection video code %d", *body.Code)
	}
	if body.Data == nil || !collectionVideoValid(body.Data.collectionVideo, true) {
		return collectionView{}, errors.New("collection member lacks video identity")
	}
	v := *body.Data
	if (q.Get("bvid") != "" && v.BVID != id) || (q.Get("aid") != "" && string(v.AID) != id[2:]) {
		return collectionView{}, errors.New("collection member URL identity mismatch")
	}
	return v, nil
}
func collectionURLID(raw string) (string, error) {
	name, id, _, e := ResolveURL(raw)
	if e != nil || name != "bilibili" {
		return "", errors.New("Bilibili member URL required")
	}
	if i := strings.Index(id, ":p"); i >= 0 {
		id = id[:i]
	}
	if !(strings.HasPrefix(id, "BV") || strings.HasPrefix(id, "av")) {
		return "", nil
	}
	return id, nil
}
func (b *Bilibili) CollectionMetadata(ctx context.Context, raw string) (*CollectionInfo, error) {
	id, e := collectionURLID(raw)
	if e != nil || id == "" {
		return nil, e
	}
	v, e := b.collectionView(ctx, id)
	if e != nil {
		return nil, e
	}
	info, e := collectionInfo(v)
	if e != nil {
		return nil, e
	}
	if info != nil && info.Total > b.maxSegments() {
		return nil, danmaku.ErrLimit
	}
	return info, nil
}

// CollectionMedia never accepts client-selected collection identities.
func (b *Bilibili) CollectionMedia(ctx context.Context, raw string) (CollectionInfo, []Episode, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	id, e := collectionURLID(raw)
	if e != nil {
		return CollectionInfo{}, nil, e
	}
	if id == "" {
		return CollectionInfo{}, nil, errors.New("UGC video URL required")
	}
	v, e := b.collectionView(ctx, id)
	if e != nil {
		return CollectionInfo{}, nil, e
	}
	info, e := collectionInfo(v)
	if e != nil {
		return CollectionInfo{}, nil, e
	}
	if info == nil {
		return CollectionInfo{}, nil, errors.New("video does not expose a UGC collection")
	}
	eps, e := b.collectionEpisodes(ctx, *info, &v)
	if e != nil {
		return CollectionInfo{}, nil, e
	}
	member := string(v.AID) + "," + string(v.CID)
	found := false
	for _, ep := range eps {
		if ep.ID == member {
			found = true
		}
	}
	if !found {
		return CollectionInfo{}, nil, errors.New("member URL is absent from its declared collection")
	}
	return *info, eps, nil
}
func (b *Bilibili) Collection(ctx context.Context, raw string) ([]Episode, error) {
	_, eps, e := b.CollectionMedia(ctx, raw)
	return eps, e
}

type collectionArchives []collectionVideo

func (v *collectionArchives) UnmarshalJSON(raw []byte) error {
	out := collectionArchives{}
	e := collectionJSONArray(raw, 30, func(raw json.RawMessage) error {
		var item collectionVideo
		if e := decodeJSON(raw, &item); e != nil {
			return errors.New("invalid collection archive")
		}
		if !collectionVideoValid(item, false) || strings.TrimSpace(item.Title) == "" {
			return errors.New("collection archive lacks identity/title")
		}
		out = append(out, item)
		return nil
	})
	*v = out
	return e
}
func (b *Bilibili) collectionArchives(ctx context.Context, info CollectionInfo) (CollectionInfo, []collectionVideo, error) {
	out := []collectionVideo{}
	seen := map[string]bool{}
	aids := map[string]bool{}
	for page := 1; page <= 34; page++ {
		if e := ctx.Err(); e != nil {
			return info, nil, e
		}
		var body struct {
			Code *int `json:"code"`
			Data *struct {
				Archives collectionArchives `json:"archives"`
				Meta     struct {
					Season scalarString `json:"season_id"`
					Mid    scalarString `json:"mid"`
					Title  string       `json:"title"`
					Total  *int         `json:"total"`
				} `json:"meta"`
				Page struct {
					Num   int  `json:"page_num"`
					Size  int  `json:"page_size"`
					Total *int `json:"total"`
				} `json:"page"`
			} `json:"data"`
		}
		q := url.Values{"season_id": {info.SeasonID}, "mid": {info.Mid}, "sort_reverse": {"false"}, "page_num": {strconv.Itoa(page)}, "page_size": {"30"}}
		if e := b.collectionGet(ctx, "https://api.bilibili.com/x/polymer/web-space/seasons_archives_list", q, &body); e != nil {
			return info, nil, e
		}
		if body.Code == nil {
			return info, nil, errors.New("bilibili collection list response lacks code")
		}
		if *body.Code != 0 {
			return info, nil, fmt.Errorf("bilibili collection list code %d", *body.Code)
		}
		d := body.Data
		if d == nil || string(d.Meta.Season) != info.SeasonID || string(d.Meta.Mid) != info.Mid || d.Meta.Total == nil || d.Page.Total == nil || *d.Meta.Total != *d.Page.Total || d.Page.Num != page || d.Page.Size != 30 {
			return info, nil, errors.New("collection pagination/identity mismatch")
		}
		total := *d.Meta.Total
		if total < 1 || total > b.maxSegments() || len(d.Meta.Title) > 4096 || strings.TrimSpace(d.Meta.Title) == "" {
			return info, nil, errors.New("collection title/count outside configured bounds")
		}
		if info.Total != 0 && total != info.Total {
			return info, nil, errors.New("collection total changed during pagination")
		}
		if page == 1 {
			info.Total = total
			info.Title = d.Meta.Title
		} else if d.Meta.Title != info.Title {
			return info, nil, errors.New("collection metadata changed during pagination")
		}
		if len(d.Archives) != min(30, total-len(out)) {
			return info, nil, errors.New("collection page is incomplete")
		}
		for _, v := range d.Archives {
			if seen[v.BVID] || aids[string(v.AID)] {
				return info, nil, errors.New("duplicate collection archive across pages")
			}
			seen[v.BVID] = true
			aids[string(v.AID)] = true
			out = append(out, v)
		}
		if len(out) == total {
			return info, out, nil
		}
	}
	return info, nil, danmaku.ErrLimit
}
func (b *Bilibili) collectionEpisodes(ctx context.Context, info CollectionInfo, seed *collectionView) ([]Episode, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	info, archives, e := b.collectionArchives(ctx, info)
	if e != nil {
		return nil, e
	}
	if seed == nil {
		first, e := b.collectionView(ctx, archives[0].BVID)
		if e != nil {
			return nil, e
		}
		seed = &first
	}
	seeds, e := collectionSeed(*seed, info)
	if e != nil {
		return nil, e
	}
	if v, ok := seeds[seed.BVID]; ok && (v.AID != seed.AID || v.CID != seed.CID) {
		return nil, errors.New("member primary pool conflicts with collection section")
	}
	seeds[seed.BVID] = seed.collectionVideo
	out := make([]Episode, len(archives))
	var next atomic.Int64
	var wg sync.WaitGroup
	var firstErr error
	var mu sync.Mutex
	workers := b.SegmentWorkers
	if workers < 1 {
		workers = 4
	}
	workers = min(workers, 4, len(archives))
	for range workers {
		wg.Go(func() {
			for {
				index := int(next.Add(1) - 1)
				if index >= len(archives) || ctx.Err() != nil {
					return
				}
				a := archives[index]
				v, ok := seeds[a.BVID]
				var err error
				if !ok {
					var got collectionView
					got, err = b.collectionView(ctx, a.BVID)
					v = got.collectionVideo
				}
				if err == nil && (v.AID != a.AID || v.BVID != a.BVID || !collectionVideoValid(v, true)) {
					err = errors.New("collection primary pool identity mismatch")
				}
				if err != nil {
					mu.Lock()
					if firstErr == nil {
						firstErr = err
						cancel()
					}
					mu.Unlock()
					return
				}
				out[index] = Episode{ID: string(v.AID) + "," + string(v.CID), Title: a.Title, URL: "https://www.bilibili.com/video/" + a.BVID + "?p=1", Index: index + 1}
			}
		})
	}
	wg.Wait()
	if firstErr != nil {
		return nil, firstErr
	}
	if e = ctx.Err(); e != nil {
		return nil, e
	}
	return out, nil
}
