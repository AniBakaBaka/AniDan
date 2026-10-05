// SPDX-License-Identifier: AGPL-3.0-only
// Independently reads the public FillUnionInfo protocol used by Tencent's
// official video-detail bundle; no page JavaScript is evaluated.
package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"reflect"
	"strings"
)

type tencentURLCover struct {
	CID              string      `json:"cid"`
	Title            string      `json:"title"`
	Type             json.Number `json:"type"`
	PublishDate      string      `json:"publish_date"`
	VerticalImage    string      `json:"new_pic_vt"`
	HorizontalImage  string      `json:"new_pic_hz"`
	BigHorizontalPic string      `json:"big_horizontal_pic_url"`
	VideoIDs         []string    `json:"video_ids"`
}
type tencentURLVideo struct {
	VID       string   `json:"vid"`
	Covers    string   `json:"c_covers"`
	CoverList []string `json:"cover_list"`
}
type tencentURLUnion struct {
	CoverInfos map[string]json.RawMessage `json:"cover_infos"`
	VideoInfos map[string]json.RawMessage `json:"video_infos"`
	PlayerInfo struct {
		VideoInfos map[string]json.RawMessage `json:"video_infos"`
	} `json:"player_video_info_list"`
}

func (l *Legacy) tencentURLUnion(ctx context.Context, id string, video bool) (tencentURLUnion, error) {
	var zero tencentURLUnion
	if !alphaID.MatchString(id) || len(id) > 128 {
		return zero, errors.New("invalid tencent URL identity")
	}
	payload := map[string]any{"appid": "10001"}
	if video {
		payload["vids"], payload["scene"] = []string{id}, 1
	} else {
		payload["cids"] = []string{id}
	}
	body, _ := json.Marshal(payload)
	h := l.HTTPProvider
	if h.MaxBytes <= 0 || h.MaxBytes > 4<<20 {
		h.MaxBytes = 4 << 20
	}
	raw, err := h.request(ctx, http.MethodPost, "https://pbaccess.video.qq.com/trpc.universal_backend_service.union_extra_data.UnionExtraData/FillUnionInfo", url.Values{"video_appid": {"3000010"}, "vversion_platform": {"2"}, "vversion_name": {"8.2.95"}}, bytes.NewReader(body), http.Header{"Content-Type": {"application/json"}, "Origin": {"https://v.qq.com"}, "Referer": {"https://v.qq.com/"}})
	if err != nil {
		return zero, err
	}
	var response struct {
		Ret  *json.Number     `json:"ret"`
		Data *tencentURLUnion `json:"data"`
	}
	if err = decodeJSON(raw, &response); err != nil {
		return zero, err
	}
	if response.Ret == nil || *response.Ret != "0" || response.Data == nil {
		return zero, errors.New("tencent URL metadata unavailable")
	}
	return *response.Data, nil
}

func tencentURLVideoCovers(data tencentURLUnion, id string) ([]string, error) {
	var video tencentURLVideo
	found := false
	for _, rows := range []map[string]json.RawMessage{data.VideoInfos, data.PlayerInfo.VideoInfos} {
		if len(rows) == 0 {
			continue
		}
		raw, ok := rows[id]
		if !ok || len(rows) != 1 {
			return nil, errors.New("tencent video metadata identity mismatch")
		}
		var v tencentURLVideo
		if err := decodeJSON(raw, &v); err != nil {
			return nil, err
		}
		if v.VID != "" && v.VID != id {
			return nil, errors.New("tencent video metadata identity mismatch")
		}
		if found && !reflect.DeepEqual(video, v) {
			return nil, errors.New("tencent video metadata contains conflicting identities")
		}
		found, video = true, v
	}
	if !found || len(video.Covers) > 4096 || len(video.CoverList) > 32 {
		return nil, errors.New("tencent URL lacks bounded cover membership")
	}
	// Topic associations are not enough to establish series membership.
	ids := strings.SplitN(video.Covers, "+", 34)
	ids = append(ids, video.CoverList...)
	seen := map[string]bool{}
	out := []string{}
	for _, cid := range ids {
		if cid == "" {
			continue
		}
		if !alphaID.MatchString(cid) || len(cid) > 128 {
			return nil, errors.New("tencent video contains invalid cover identity")
		}
		if !seen[cid] {
			seen[cid] = true
			out = append(out, cid)
		}
		if len(out) > 32 {
			return nil, errors.New("tencent video has too many cover identities")
		}
	}
	if len(out) == 0 {
		return nil, errors.New("tencent video lacks cover identity")
	}
	return out, nil
}

func (l *Legacy) tencentResolveMedia(ctx context.Context, raw string) (Result, error) {
	name, cid, vid, err := ResolveURL(raw)
	if err != nil || name != "tencent" || l.Source != "tencent" || len(cid) > 128 || len(vid) > 128 {
		return Result{}, errors.New("invalid tencent media URL")
	}
	if vid != "" {
		data, e := l.tencentURLUnion(ctx, vid, true)
		if e != nil {
			return Result{}, e
		}
		covers, e := tencentURLVideoCovers(data, vid)
		if e != nil {
			return Result{}, e
		}
		if cid == "" {
			if len(covers) != 1 {
				return Result{}, errors.New("tencent video belongs to multiple media; use an explicit cover URL")
			}
			cid = covers[0]
		} else {
			found := false
			for _, c := range covers {
				if c == cid {
					found = true
					break
				}
			}
			if !found {
				return Result{}, errors.New("tencent URL video is not a member of the requested media")
			}
		}
	}
	data, err := l.tencentURLUnion(ctx, cid, false)
	if err != nil {
		return Result{}, err
	}
	body, ok := data.CoverInfos[cid]
	if !ok || len(data.CoverInfos) != 1 {
		return Result{}, errors.New("tencent cover metadata identity mismatch")
	}
	var cover tencentURLCover
	if err = decodeJSON(body, &cover); err != nil {
		return Result{}, err
	}
	if cover.CID != "" && cover.CID != cid {
		return Result{}, errors.New("tencent cover metadata identity mismatch")
	}
	title := strings.TrimSpace(cover.Title)
	if title == "" || len(title) > 4096 || len(cover.VideoIDs) > 10000 {
		return Result{}, errors.New("tencent cover lacks bounded media metadata")
	}
	if vid != "" && len(cover.VideoIDs) > 0 {
		found := false
		for _, id := range cover.VideoIDs {
			if id == vid {
				found = true
				break
			}
		}
		if !found {
			return Result{}, errors.New("tencent URL video is absent from the media inventory")
		}
	}
	typ := "other"
	switch cover.Type {
	case "1":
		typ = "movie"
	case "2", "3", "9", "10":
		typ = "tv_series"
	}
	return Result{ID: cid, Title: title, Type: typ, Year: yearFromDate(cover.PublishDate), ImageURL: sourceMediaImageURL(textFirst(cover.VerticalImage, cover.HorizontalImage, cover.BigHorizontalPic))}, nil
}
