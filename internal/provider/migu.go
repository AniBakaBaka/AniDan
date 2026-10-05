// SPDX-License-Identifier: AGPL-3.0-only
// Protocol/response-codec compatibility from huangxd-/danmu_api, AGPL-3.0,
// commit fc1b7ff6add61d8af24c9bf978253273833f5afc, migu.js and migu-util.js.
// No JavaScript is executed and no borrowed session SID is sent.
package provider

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/AniBakaBaka/AniDan/internal/danmaku"
)

func (l *Legacy) miguSearch(ctx context.Context, keyword string) ([]Result, error) {
	// The official public search bundle calls getRandom() for each SID. Generate
	// a fresh local request identifier rather than adopting the reference's SID.
	var nonce [64]byte
	if _, e := rand.Read(nonce[:]); e != nil {
		return nil, e
	}
	const alphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZ"
	for i := range nonce {
		nonce[i] = alphabet[int(nonce[i])%len(alphabet)]
	}
	payload := map[string]any{"appVersion": "6.1.1.00", "ct": 101, "isCorrectWord": 1, "k": keyword, "mediaSource": 9000000, "pageIdx": 1, "pageSize": 20, "copyrightTerminal": 3, "searchScene": 2, "uiVersion": "A3.26.0", "sid": string(nonce[:])}
	body, _ := json.Marshal(payload)
	raw, e := l.request(ctx, http.MethodPost, "https://jadeite.migu.cn/search/v3/open-search", nil, bytes.NewReader(body), http.Header{"Content-Type": {"application/json"}, "Origin": {"https://www.miguvideo.com"}, "Referer": {"https://www.miguvideo.com/"}, "appId": {"miguvideo"}, "terminalId": {"www"}})
	if e != nil {
		return nil, e
	}
	var v map[string]any
	if e = decodeJSON(raw, &v); e != nil {
		return nil, e
	}
	if e = miguResponseStatus(v); e != nil {
		return nil, e
	}
	items, ok := at(v, "body", "contentInfoList").([]any)
	if !ok {
		return nil, errors.New("migu search missing body.contentInfoList")
	}
	if len(items) > 1000 {
		return nil, danmaku.ErrLimit
	}
	out := []Result{}
	for _, item := range items {
		m := object(at(object(item), "shortMediaAsset"))
		isLong, _ := m["isLong"].(bool)
		if !isLong && integer(m["isLong"]) != 1 {
			continue
		}
		id := str(m["pID"])
		if eps := array(at(m, "extraData", "episodes")); len(eps) > 0 {
			id = str(eps[0])
		}
		if !numericID.MatchString(id) || len(id) > 64 || str(m["name"]) == "" {
			continue
		}
		typ := "other"
		switch str(m["contDisplayName"]) {
		case "电影":
			typ = "movie"
		case "电视剧", "动漫", "综艺":
			typ = "tv_series"
		}
		out = append(out, Result{ID: id, Title: str(m["name"]), Type: typ, Year: integer(m["year"]), ImageURL: str(at(m, "h5pics", "highResolutionV"))})
	}
	return out, nil
}

func (l *Legacy) miguDetail(ctx context.Context, id string) (map[string]any, error) {
	if !numericID.MatchString(id) || len(id) > 64 {
		return nil, errors.New("migu requires numeric content ID")
	}
	var v map[string]any
	if e := l.get(ctx, "https://v3-sc.miguvideo.com/program/v4/cont/content-info/"+id+"/1", nil, &v); e != nil {
		return nil, e
	}
	if e := miguResponseStatus(v); e != nil {
		return nil, e
	}
	m := object(at(v, "body", "data"))
	if m == nil {
		return nil, errors.New("migu content response missing body.data")
	}
	return m, nil
}

func (l *Legacy) miguEpisodes(ctx context.Context, id string) ([]Episode, error) {
	v, e := l.miguDetail(ctx, id)
	if e != nil {
		return nil, e
	}
	items, ok := v["datas"].([]any)
	if !ok {
		pid := str(at(v, "playing", "pID"))
		if pid == "" {
			return nil, errors.New("migu content has neither episode list nor playing ID")
		}
		items = []any{map[string]any{"pID": pid, "name": v["name"]}}
	}
	if len(items) > 10000 {
		return nil, danmaku.ErrLimit
	}
	out := make([]Episode, 0, len(items))
	seen := map[string]bool{}
	for i, item := range items {
		m := object(item)
		pid, title := str(m["pID"]), str(m["name"])
		if !numericID.MatchString(pid) || len(pid) > 64 || title == "" {
			return nil, errors.New("migu episode missing numeric ID/title")
		}
		if seen[pid] {
			return nil, errors.New("migu episode list repeats content ID")
		}
		seen[pid] = true
		// The official homepage's route table maps content IDs to /p/detail/.
		out = append(out, Episode{ID: pid, Title: title, Index: i + 1, URL: "https://www.miguvideo.com/p/detail/" + pid})
	}
	return out, nil
}

func miguDuration(v any) (float64, error) {
	s := str(v)
	parts := strings.Split(s, ":")
	if len(parts) > 3 || len(parts) == 0 {
		return 0, errors.New("invalid migu duration")
	}
	total := 0.0
	for i, p := range parts {
		n, e := strconv.ParseFloat(p, 64)
		if e != nil || n < 0 || math.IsNaN(n) || math.IsInf(n, 0) || (i > 0 && n >= 60) {
			return 0, errors.New("invalid migu duration")
		}
		total = total*60 + n
	}
	if total <= 0 || total > 86400 {
		return 0, errors.New("migu duration unavailable or exceeds one day")
	}
	return total, nil
}

func (l *Legacy) miguComments(ctx context.Context, id string) ([]danmaku.Comment, error) {
	v, e := l.miguDetail(ctx, id)
	if e != nil {
		return nil, e
	}
	duration, e := miguDuration(at(v, "playing", "duration"))
	if e != nil {
		return nil, e
	}
	parent := str(v["epsID"])
	if parent == "" {
		parent = id
	}
	if !numericID.MatchString(parent) || len(parent) > 64 {
		return nil, errors.New("migu invalid episode collection ID")
	}
	n := int(math.Ceil(duration / 30))
	comments, e := l.segments(ctx, n, func(ctx context.Context, i int) ([]danmaku.Comment, error) {
		start, end := i*30, min((i+1)*30, int(math.Ceil(duration)))
		endpoint := fmt.Sprintf("https://webapi.miguvideo.com/gateway/live_barrage/videox/barrage/v2/list/%s/%s/%d/%d/020", parent, id, start, end)
		raw, e := l.request(ctx, http.MethodGet, endpoint, nil, nil, http.Header{"appCode": {"miguvideo_default_h5"}})
		if e != nil {
			return nil, e
		}
		decoded, e := decodeMiguPayload(raw, l.MaxBytes)
		if e != nil {
			return nil, e
		}
		return parseMiguCommentsBounded(ctx, decoded, l.MaxComments, l.MaxDownloadBytes, duration)
	})
	if e != nil {
		return nil, e
	}
	return mergeCanonical(comments)
}

// The inspected public client uses a constant AES response codec, not an account
// credential. Strict framing/padding/UTF-8 checks reject malformed payloads.
func decodeMiguPayload(raw []byte, limit int64) ([]byte, error) {
	if limit <= 0 {
		limit = 32 << 20
	}
	if int64(len(raw)) > limit {
		return nil, danmaku.ErrLimit
	}
	raw = bytes.TrimSpace(raw)
	if len(raw) > 0 && raw[0] == '{' {
		return raw, nil
	}
	if len(raw) > 0 && raw[0] == '"' {
		var s string
		if e := json.Unmarshal(raw, &s); e != nil {
			return nil, e
		}
		raw = []byte(s)
	}
	compact := strings.Join(strings.Fields(string(raw)), "")
	data, e := base64.StdEncoding.Strict().DecodeString(compact)
	if e != nil || len(data) == 0 || len(data)%aes.BlockSize != 0 {
		return nil, errors.New("invalid migu encrypted response framing")
	}
	key, _ := base64.StdEncoding.DecodeString("vwwLu7e6ug4HAQMAug8CsA8HD7oHDwuxAg4HAQG6DLA=")
	sub := [16]byte{3, 5, 7, 0, 15, 10, 13, 1, 11, 14, 4, 6, 9, 12, 8, 2}
	for i, v := range key {
		key[i] = (sub[v>>4] << 4) | sub[v&15]
	}
	cipher, e := aes.NewCipher(key)
	if e != nil {
		return nil, e
	}
	for i := 0; i < len(data); i += aes.BlockSize {
		cipher.Decrypt(data[i:i+aes.BlockSize], data[i:i+aes.BlockSize])
	}
	pad := int(data[len(data)-1])
	if pad < 1 || pad > aes.BlockSize || pad > len(data) {
		return nil, errors.New("invalid migu response padding")
	}
	for _, v := range data[len(data)-pad:] {
		if int(v) != pad {
			return nil, errors.New("invalid migu response padding")
		}
	}
	data = data[:len(data)-pad]
	if !utf8.Valid(data) {
		return nil, errors.New("invalid migu response UTF-8")
	}
	return data, nil
}

func parseMiguComments(raw []byte) ([]danmaku.Comment, error) {
	return parseMiguCommentsBounded(context.Background(), raw, 500000, 64<<20, math.MaxFloat64)
}

// Stream each record instead of first allocating a full []map wire tree. This
// enforces configured comment/byte budgets before large arrays can amplify RAM.
func parseMiguCommentsBounded(ctx context.Context, raw []byte, limit int, budget int64, duration float64) ([]danmaku.Comment, error) {
	if limit <= 0 {
		limit = 500000
	}
	if budget <= 0 {
		budget = 64 << 20
	}
	if !utf8.Valid(raw) {
		return nil, errors.New("invalid migu JSON UTF-8")
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	out := []danmaku.Comment{}
	used := int64(0)
	bodyFound, resultFound := false, false
	var objectFields func(bool) error
	objectFields = func(body bool) error {
		token, e := d.Token()
		if e != nil || token != json.Delim('{') {
			return errors.New("invalid migu JSON object")
		}
		for d.More() {
			key, e := d.Token()
			if e != nil {
				return e
			}
			switch {
			case key == "body" && !body:
				if bodyFound {
					return errors.New("duplicate migu body")
				}
				bodyFound = true
				if e = objectFields(true); e != nil {
					return e
				}
			case key == "result" && body:
				if resultFound {
					return errors.New("duplicate migu result")
				}
				resultFound = true
				token, e = d.Token()
				if e != nil || token != json.Delim('[') {
					return errors.New("migu result is not array")
				}
				for d.More() {
					if len(out) >= limit {
						return danmaku.ErrLimit
					}
					if e = ctx.Err(); e != nil {
						return e
					}
					var v struct {
						ID    scalarString `json:"cid"`
						Time  scalarString `json:"playtime"`
						Color string       `json:"textcolor"`
						Text  *string      `json:"msg"`
					}
					if e = d.Decode(&v); e != nil {
						return e
					}
					if v.Text == nil {
						return errors.New("migu comment missing text")
					}
					seconds, e := strconv.ParseFloat(string(v.Time), 64)
					if e != nil || seconds < 0 || seconds > duration {
						return errors.New("migu comment time outside verified duration")
					}
					color := 0xffffff
					if v.Color != "" {
						value := v.Color
						if !strings.HasPrefix(value, "#") {
							value = "#" + value
						}
						color, e = danmaku.ParseColor(value)
						if e != nil {
							return e
						}
					}
					cid, err := strconv.ParseInt(string(v.ID), 10, 64)
					if err != nil {
						cid = 0
					}
					c, e := danmaku.NewComment(cid, seconds, 1, 25, color, *v.Text, "[migu]")
					if e != nil {
						return e
					}
					used += int64(len(c.P) + len(c.M) + 64)
					if used > budget {
						return danmaku.ErrLimit
					}
					out = append(out, c)
				}
				if token, e = d.Token(); e != nil || token != json.Delim(']') {
					return errors.New("incomplete migu result array")
				}
			case key == "header" && !body:
				var h struct {
					Code       *scalarString `json:"code"`
					ResultCode *scalarString `json:"resultCode"`
				}
				if e = d.Decode(&h); e != nil {
					return e
				}
				status := map[string]any{}
				if h.Code != nil {
					status["code"] = string(*h.Code)
				}
				if h.ResultCode != nil {
					status["resultCode"] = string(*h.ResultCode)
				}
				if e = miguResponseStatus(status); e != nil {
					return e
				}
			case (key == "code" || key == "resultCode") && !body:
				var code scalarString
				if e = d.Decode(&code); e != nil {
					return e
				}
				if e = miguResponseStatus(map[string]any{"code": string(code)}); e != nil {
					return e
				}
			default:
				if e = xiguaDiscardJSON(d); e != nil {
					return e
				}
			}
		}
		if token, e = d.Token(); e != nil || token != json.Delim('}') {
			return errors.New("incomplete migu JSON object")
		}
		return nil
	}
	if e := objectFields(false); e != nil {
		return nil, e
	}
	if !bodyFound || !resultFound {
		return nil, errors.New("migu response missing body.result")
	}
	if _, e := d.Token(); e != io.EOF {
		return nil, errors.New("trailing migu comment JSON")
	}
	return out, nil
}

func miguResponseStatus(v map[string]any) error {
	for _, m := range []map[string]any{v, object(v["header"])} {
		for _, key := range []string{"code", "resultCode"} {
			value, ok := m[key]
			if !ok || value == nil {
				continue
			}
			s := str(value)
			if strings.EqualFold(s, "success") {
				continue
			}
			n, e := strconv.Atoi(s)
			if e != nil || (n != 0 && n != 200) {
				return errors.New("migu upstream reported a non-success status")
			}
		}
	}
	return nil
}
