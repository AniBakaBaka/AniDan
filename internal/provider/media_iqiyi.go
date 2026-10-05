// SPDX-License-Identifier: AGPL-3.0-only
// Uses the same public decode/baseinfo protocol as the historical AGPL Misaka
// iQiyi adapter; albumName is verified from the current public baseinfo response.
package provider

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"strings"
)

type iqiyiURLVideo struct {
	TVID          json.Number `json:"tvId"`
	VideoID       json.Number `json:"videoId"`
	AlbumID       json.Number `json:"albumId"`
	ChannelID     json.Number `json:"channelId"`
	ChannelName   string      `json:"channelName"`
	Name          string      `json:"name"`
	AlbumName     string      `json:"albumName"`
	PlayURL       string      `json:"playUrl"`
	ImageURL      string      `json:"imageUrl"`
	AlbumImageURL string      `json:"albumImageUrl"`
	Order         *int        `json:"order"`
}

// Preserve the legacy media-link identity: Episodes accepts this public link ID,
// while Comments accepts the separately verified numeric tvId. Album IDs must
// not replace media-link IDs merely because they identify the same series.
func (l *Legacy) iqiyiURLVideo(ctx context.Context, raw string) (string, iqiyiURLVideo, error) {
	var zero iqiyiURLVideo
	name, link, ep, err := ResolveURL(raw)
	if err != nil || name != "iqiyi" || l.Source != "iqiyi" || ep != "" || len(link) > 128 || !alphaID.MatchString(link) {
		return "", zero, errors.New("invalid iqiyi media URL")
	}
	h := l.HTTPProvider
	if h.MaxBytes <= 0 || h.MaxBytes > 4<<20 {
		h.MaxBytes = 4 << 20
	}
	var decoded struct {
		Code string      `json:"code"`
		Data json.Number `json:"data"`
	}
	if err = h.get(ctx, "https://pcw-api.iq.com/api/decode/"+url.PathEscape(link), url.Values{"platformId": {"3"}, "modeCode": {"intl"}, "langCode": {"sg"}}, &decoded); err != nil {
		return "", zero, err
	}
	tvid := decoded.Data.String()
	if (decoded.Code != "0" && decoded.Code != "A00000") || !numericID.MatchString(tvid) || len(tvid) > 32 || strings.Trim(tvid, "0") == "" {
		return "", zero, errors.New("iqiyi URL decode lacks a valid video identity")
	}
	var response struct {
		Code string         `json:"code"`
		Data *iqiyiURLVideo `json:"data"`
	}
	if err = h.get(ctx, "https://pcw-api.iqiyi.com/video/video/baseinfo/"+tvid, nil, &response); err != nil {
		return "", zero, err
	}
	if response.Code != "A00000" || response.Data == nil {
		return "", zero, errors.New("iqiyi URL metadata unavailable")
	}
	v := *response.Data
	actual := v.TVID.String()
	if actual == "" {
		actual = v.VideoID.String()
	}
	if actual != tvid || v.TVID != "" && v.VideoID != "" && v.TVID != v.VideoID {
		return "", zero, errors.New("iqiyi URL video identity mismatch")
	}
	p, resolved, _, err := ResolveURL(v.PlayURL)
	if err != nil || p != "iqiyi" || resolved != link {
		return "", zero, errors.New("iqiyi metadata play URL does not match requested video")
	}
	return link, v, nil
}

func (l *Legacy) iqiyiResolveMedia(ctx context.Context, raw string) (Result, error) {
	link, v, err := l.iqiyiURLVideo(ctx, raw)
	if err != nil {
		return Result{}, err
	}
	typ := "other"
	if v.ChannelID == "1" || v.ChannelName == "电影" {
		typ = "movie"
	} else if v.ChannelID == "2" || v.ChannelID == "4" || v.ChannelID == "6" || v.ChannelName == "电视剧" || v.ChannelName == "动漫" || v.ChannelName == "综艺" {
		typ = "tv_series"
	}
	title, cover := strings.TrimSpace(v.AlbumName), v.AlbumImageURL
	if typ == "movie" {
		title, cover = strings.TrimSpace(v.Name), v.ImageURL
	} else if !numericID.MatchString(v.AlbumID.String()) || len(v.AlbumID) > 32 || strings.Trim(v.AlbumID.String(), "0") == "" {
		return Result{}, errors.New("iqiyi series metadata lacks album identity")
	}
	if title == "" || len(title) > 4096 {
		return Result{}, errors.New("iqiyi metadata lacks a bounded media title")
	}
	return Result{ID: link, Title: title, Type: typ, ImageURL: sourceMediaImageURL(cover)}, nil
}
