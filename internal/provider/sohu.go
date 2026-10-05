// SPDX-License-Identifier: AGPL-3.0-only
// Keyless list and public player window contracts independently inspected in
// Sohu's linked public scripts on 2026-10-02. Remote JavaScript is not executed.
package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/AniBakaBaka/AniDan/internal/danmaku"
	"golang.org/x/text/encoding/simplifiedchinese"
)

const sohuWindowSeconds = 300

// The current public list controller declares this page size. An independent
// request verified it with exact count/order and Content-Type charset=GBK.
const sohuListPageSize = 100
const sohuMaxVideos = 10000

type sohuVideo struct {
	ID        scalarString `json:"vid"`
	Order     scalarString `json:"order"`
	Duration  scalarString `json:"playLength"`
	Name      string       `json:"name"`
	VideoName string       `json:"video_name"`
	PageURL   string       `json:"pageUrl"`
	HTML5URL  string       `json:"url_html5"`
}

type sohuListPage struct {
	Album       scalarString   `json:"playlistid"`
	CurrentPage scalarString   `json:"currentPage"`
	PageSize    scalarString   `json:"pageSize"`
	Size        scalarString   `json:"size"`
	TotalSet    scalarString   `json:"totalSet"`
	Videos      sohuPageVideos `json:"videos"`
}

// Decode list entries one at a time. A raw-byte response cap alone does not
// bound the number of tiny JSON objects a whole-slice unmarshal allocates.
type sohuPageVideos []sohuVideo

func (videos *sohuPageVideos) UnmarshalJSON(raw []byte) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	start, err := d.Token()
	if err != nil || start != json.Delim('[') {
		return errors.New("sohu list videos must be an array")
	}
	out := sohuPageVideos{}
	for d.More() {
		if len(out) >= sohuListPageSize {
			return danmaku.ErrLimit
		}
		var video sohuVideo
		if err := d.Decode(&video); err != nil {
			return err
		}
		out = append(out, video)
	}
	if _, err := d.Token(); err != nil {
		return err
	}
	*videos = out
	return nil
}

func sohuID(id string) bool {
	return len(id) > 0 && len(id) <= 64 && id[0] != '0' && numericID.MatchString(id)
}

func sohuIDs(id string) (string, string, error) {
	p := strings.Split(id, ":")
	if len(p) != 2 || !sohuID(p[0]) || !sohuID(p[1]) {
		return "", "", errors.New("sohu episode ID must be numeric vid:aid")
	}
	return p[0], p[1], nil
}

func sohuListUTF8(raw []byte) ([]byte, error) {
	decoded, err := simplifiedchinese.GBK.NewDecoder().Bytes(raw)
	if err != nil || !utf8.Valid(decoded) || bytes.ContainsRune(decoded, utf8.RuneError) {
		return nil, errors.New("sohu list contains invalid GBK text")
	}
	return decoded, nil
}

func (l *Legacy) sohuVideos(ctx context.Context, album string) ([]sohuVideo, error) {
	if !sohuID(album) {
		return nil, errors.New("sohu requires numeric album ID")
	}
	total := -1
	totalSet := scalarString("")
	out := []sohuVideo{}
	seen := map[scalarString]bool{}
	retained := int64(0)
	budget := l.MaxDownloadBytes
	if budget <= 0 {
		budget = 64 << 20
	}
	for page := 1; page <= l.maxSegments(); page++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		raw, err := l.request(ctx, http.MethodGet, "https://pl.hd.sohu.com/videolist", url.Values{
			"callback": {"anidan_sohu"}, "playlistid": {album},
			"pagesize": {strconv.Itoa(sohuListPageSize)}, "pagenum": {strconv.Itoa(page)},
		}, nil, nil)
		if err != nil {
			return nil, err
		}
		// This endpoint's observed wire contract is GBK, including JSONP
		// titles. Decode that contract explicitly, never infer an encoding
		// from title bytes or accept lossy replacement of malformed bytes.
		raw, err = sohuListUTF8(raw)
		if err != nil {
			return nil, err
		}
		var v sohuListPage
		if err := decodeJSONP(raw, &v); err != nil {
			return nil, err
		}
		if string(v.Album) != album || string(v.CurrentPage) != strconv.Itoa(page) || string(v.PageSize) != strconv.Itoa(sohuListPageSize) || v.Videos == nil {
			return nil, errors.New("sohu list identity or pagination metadata missing or mismatched")
		}
		count, err := strconv.Atoi(string(v.Size))
		if err != nil || count < 0 || strconv.Itoa(count) != string(v.Size) {
			return nil, errors.New("sohu list invalid declared size")
		}
		if count > sohuMaxVideos || count > l.maxSegments()*sohuListPageSize {
			return nil, fmt.Errorf("%w: sohu metadata pages", danmaku.ErrLimit)
		}
		if total < 0 {
			total, totalSet = count, v.TotalSet
		} else if total != count || totalSet != v.TotalSet {
			return nil, errors.New("sohu list declared counts changed during pagination")
		}
		if v.TotalSet != "" && string(v.TotalSet) != strconv.Itoa(total) {
			return nil, errors.New("sohu list totalSet differs from declared size")
		}
		if len(v.Videos) != min(sohuListPageSize, total-len(out)) {
			return nil, errors.New("sohu list page incomplete or excessive")
		}
		for _, item := range v.Videos {
			index, err := strconv.Atoi(string(item.Order))
			if !sohuID(string(item.ID)) || seen[item.ID] || err != nil || index != len(out)+1 || strconv.Itoa(index) != string(item.Order) {
				return nil, errors.New("sohu list video identity or order invalid or repeated")
			}
			title := textFirst(item.Name, item.VideoName)
			if strings.TrimSpace(title) == "" || len(title) > 4096 || strings.ContainsRune(title, utf8.RuneError) {
				return nil, errors.New("sohu list title missing or invalid")
			}
			if len(item.PageURL) > 4096 || len(item.HTML5URL) > 4096 {
				return nil, danmaku.ErrLimit
			}
			added := int64(len(item.ID)+len(item.Order)+len(item.Duration)+len(item.Name)+len(item.VideoName)+len(item.PageURL)+len(item.HTML5URL)) + 128
			if added > budget-retained {
				return nil, danmaku.ErrLimit
			}
			retained += added
			seen[item.ID] = true
			out = append(out, item)
		}
		if len(out) == total {
			return out, nil
		}
	}
	return nil, fmt.Errorf("%w: sohu metadata pages", danmaku.ErrLimit)
}

func (l *Legacy) sohuEpisodes(ctx context.Context, id string) ([]Episode, error) {
	videos, err := l.sohuVideos(ctx, id)
	if err != nil {
		return nil, err
	}
	out := make([]Episode, 0, len(videos))
	for i, item := range videos {
		out = append(out, Episode{ID: string(item.ID) + ":" + id, Title: textFirst(item.Name, item.VideoName), Index: i + 1, URL: textFirst(item.PageURL, item.HTML5URL)})
	}
	return out, nil
}

// sohuComments covers all public-player windows for a verified duration. The
// player fixes page=1: undocumented response counters/flags are not interpreted
// as archival completeness, pagination cursors, or reasons to stop on gaps.
func (l *Legacy) sohuComments(ctx context.Context, id string) ([]danmaku.Comment, error) {
	vid, aid, err := sohuIDs(id)
	if err != nil {
		return nil, err
	}
	videos, err := l.sohuVideos(ctx, aid)
	if err != nil {
		return nil, err
	}
	duration := scalarString("")
	found := false
	for _, v := range videos {
		if string(v.ID) == vid {
			duration, found = v.Duration, true
		}
	}
	if !found {
		return nil, errors.New("sohu requested video missing from verified album list")
	}
	seconds, err := strconv.ParseFloat(string(duration), 64)
	if err != nil || math.IsNaN(seconds) || math.IsInf(seconds, 0) || seconds <= 0 {
		return nil, errors.New("sohu requested video has no valid source-declared duration")
	}
	// Compare as floating point before converting to int or rounding, so even
	// huge finite source values cannot overflow the window count or end time.
	if seconds > float64(l.maxSegments()*sohuWindowSeconds) {
		return nil, fmt.Errorf("%w: sohu duration windows", danmaku.ErrLimit)
	}
	count := max(1, int(math.Ceil(seconds/sohuWindowSeconds)))
	out, err := l.segments(ctx, count, func(ctx context.Context, i int) ([]danmaku.Comment, error) {
		return l.sohuSegment(ctx, vid, aid, i*sohuWindowSeconds, (i+1)*sohuWindowSeconds)
	})
	if err != nil {
		return nil, err
	}
	return mergeCanonical(out)
}

// SohuWindow preserves the explicit caller-defined interval, in sequential
// 60-second requests (including a shorter final request). Responses must now
// declare success and echo that exact identity and interval. No duration lookup
// or empty-window termination changes these caller-specified boundaries.
func (l *Legacy) SohuWindow(ctx context.Context, id string, start, end int) ([]danmaku.Comment, error) {
	if start < 0 || end <= start {
		return nil, errors.New("invalid sohu time window")
	}
	span := end - start // Both are nonnegative and ordered; subtraction is safe.
	if 1+(span-1)/60 > l.maxSegments() {
		return nil, fmt.Errorf("%w: sohu explicit windows", danmaku.ErrLimit)
	}
	vid, aid, err := sohuIDs(id)
	if err != nil {
		return nil, err
	}
	out := []danmaku.Comment{}
	retained := int64(0)
	for begin := start; begin < end; {
		finish := begin + min(60, end-begin) // Never adds beyond the valid end.
		part, err := l.sohuSegment(ctx, vid, aid, begin, finish)
		if err != nil {
			return nil, err
		}
		out, err = l.appendBudget(out, &retained, part...)
		if err != nil {
			return nil, err
		}
		begin = finish
	}
	return mergeCanonical(out)
}

func (l *Legacy) sohuSegment(ctx context.Context, vid, aid string, begin, end int) ([]danmaku.Comment, error) {
	raw, err := l.request(ctx, http.MethodGet, "https://api.danmu.tv.sohu.com/dmh5/dmListAll", url.Values{
		"act": {"dmlist_v2"}, "dct": {"1"}, "request_from": {"h5_js"},
		"vid": {vid}, "aid": {aid}, "pct": {"2"}, "from": {"PlayerType.SOHU_VRS"},
		"page": {"1"}, "o": {"4"}, "time_begin": {strconv.Itoa(begin)}, "time_end": {strconv.Itoa(end)},
	}, nil, nil)
	if err != nil {
		return nil, err
	}
	if !utf8.Valid(raw) {
		return nil, errors.New("sohu comment response is not UTF-8")
	}
	var v struct {
		Status scalarString `json:"status"`
		Info   *struct {
			Video    scalarString    `json:"vid"`
			PCT      scalarString    `json:"pct"`
			Begin    scalarString    `json:"begin"`
			End      scalarString    `json:"end"`
			Defaults sohuColor       `json:"dfopt"`
			Comments json.RawMessage `json:"comments"`
		} `json:"info"`
	}
	if err := decodeJSON(raw, &v); err != nil {
		return nil, err
	}
	if v.Status != "1" {
		return nil, errors.New("sohu comment response did not declare success")
	}
	info := v.Info
	if info == nil || string(info.Video) != vid || info.PCT != "2" || string(info.Begin) != strconv.Itoa(begin) || string(info.End) != strconv.Itoa(end) || info.Comments == nil {
		return nil, errors.New("sohu comment identity, interval or comments array missing or mismatched")
	}
	// Retain only raw bounded JSON plus one wire record at a time. Canonical
	// count/byte checks run before retaining each record, and segments also
	// applies the same budgets across concurrently completed windows.
	d := json.NewDecoder(bytes.NewReader(info.Comments))
	start, err := d.Token()
	if err != nil || start != json.Delim('[') {
		return nil, errors.New("sohu comments must be an array")
	}
	limit := l.MaxComments
	if limit < 1 {
		limit = 500000
	}
	out := []danmaku.Comment{}
	retained := int64(0)
	for d.More() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if len(out) >= limit {
			return nil, danmaku.ErrLimit
		}
		var item struct {
			ID      scalarString `json:"i"`
			Seconds scalarString `json:"v"`
			Content *string      `json:"c"`
			Style   sohuStyle    `json:"t"`
		}
		if err := d.Decode(&item); err != nil {
			return nil, err
		}
		cid, err := strconv.ParseInt(string(item.ID), 10, 64)
		if err != nil || cid <= 0 || strconv.FormatInt(cid, 10) != string(item.ID) {
			return nil, errors.New("sohu comment has invalid identity")
		}
		seconds, err := strconv.ParseFloat(string(item.Seconds), 64)
		// Adjacent responses may share their boundary record. Accept the end
		// inclusively, then canonical deduplication removes exact repeats.
		if err != nil || math.IsNaN(seconds) || math.IsInf(seconds, 0) || seconds < float64(begin) || seconds > float64(end) || item.Content == nil {
			return nil, errors.New("sohu comment has invalid or out-of-window time or missing text")
		}
		colorText := item.Style.Color
		if item.Style.Default {
			colorText = info.Defaults.Color
		}
		color, err := danmaku.ParseColor(string(colorText))
		if err != nil {
			return nil, errors.New("sohu comment has missing or invalid color")
		}
		// The source does not establish a complete canonical movement/size map.
		// Preserve the existing neutral scrolling mode and font size.
		c, err := danmaku.NewComment(cid, seconds, 1, 25, color, *item.Content, "[sohu]")
		if err != nil {
			return nil, err
		}
		out, err = l.appendBudget(out, &retained, c)
		if err != nil {
			return nil, err
		}
	}
	if _, err := d.Token(); err != nil {
		return nil, err
	}
	return out, nil
}

type sohuColor struct {
	Color scalarString `json:"c"`
}

type sohuStyle struct {
	Color   scalarString
	Default bool
}

func (style *sohuStyle) UnmarshalJSON(raw []byte) error {
	if len(raw) > 0 && raw[0] == '"' {
		var name string
		if err := decodeJSON(raw, &name); err != nil || name != "df" {
			return errors.New("sohu comment has unsupported style")
		}
		*style = sohuStyle{Default: true}
		return nil
	}
	if len(raw) == 0 || raw[0] != '{' {
		return errors.New("sohu comment has missing or unsupported style")
	}
	var color sohuColor
	if err := decodeJSON(raw, &color); err != nil {
		return err
	}
	if color.Color == "" {
		return errors.New("sohu comment style missing color")
	}
	*style = sohuStyle{Color: color.Color}
	return nil
}
