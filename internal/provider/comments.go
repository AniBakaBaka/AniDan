// SPDX-License-Identifier: AGPL-3.0-only
package provider

import (
	"bytes"
	"encoding/json"
	"errors"
	"github.com/AniBakaBaka/AniDan/internal/danmaku"
	"sort"
	"strconv"
)

// Provider constructors already validated/canonicalized these records. Avoid
// formatting/parsing them again just to deduplicate and sort a downloaded pool.
func mergeCanonical(in []danmaku.Comment) ([]danmaku.Comment, error) {
	if len(in) > danmaku.DefaultParseOptions().MaxComments {
		return nil, danmaku.ErrLimit
	}
	seen := make(map[string]struct{}, len(in))
	out := in[:0]
	for _, c := range in {
		if c.T < 0 {
			continue
		}
		key := c.P + "\x00" + c.M
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, c)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].T < out[j].T })
	return out, nil
}

type scalarString string

func (s *scalarString) UnmarshalJSON(b []byte) error {
	if len(b) > 4096 {
		return danmaku.ErrLimit
	}
	if len(b) > 0 && b[0] == '"' {
		var v string
		if e := json.Unmarshal(b, &v); e != nil {
			return e
		}
		*s = scalarString(v)
		return nil
	}
	var n json.Number
	if e := json.Unmarshal(b, &n); e != nil {
		return e
	}
	*s = scalarString(n.String())
	return nil
}

type tencentStyle struct {
	Color    string   `json:"color"`
	Position int      `json:"position"`
	Gradient []string `json:"gradient_colors"`
}

func (s *tencentStyle) UnmarshalJSON(b []byte) error {
	if bytes.Equal(b, []byte(`""`)) || bytes.Equal(b, []byte("null")) {
		return nil
	}
	if len(b) > 0 && b[0] == '"' {
		var inner string
		if e := json.Unmarshal(b, &inner); e != nil {
			return e
		}
		b = []byte(inner)
	}
	type plain tencentStyle
	return json.Unmarshal(b, (*plain)(s))
}

type tencentWireComment struct {
	ID      scalarString `json:"id"`
	Time    scalarString `json:"time_offset"`
	Content *string      `json:"content"`
	Style   tencentStyle `json:"content_style"`
}

func decodeTencentSegment(raw []byte) ([]danmaku.Comment, error) {
	var v struct {
		Comments *[]tencentWireComment `json:"barrage_list"`
	}
	if e := decodeJSON(raw, &v); e != nil {
		return nil, e
	}
	if v.Comments == nil {
		return nil, errors.New("tencent response missing barrage_list")
	}
	if len(*v.Comments) > 500000 {
		return nil, danmaku.ErrLimit
	}
	out := make([]danmaku.Comment, 0, len(*v.Comments))
	for _, m := range *v.Comments {
		if m.Content == nil || m.Time == "" {
			continue
		}
		mode := 1
		switch m.Style.Position {
		case 2:
			mode = 5
		case 3:
			mode = 4
		}
		color := 0xffffff
		hex := m.Style.Color
		if len(m.Style.Gradient) > 0 {
			hex = m.Style.Gradient[0]
		}
		if hex != "" {
			var e error
			if hex[0] != '#' {
				hex = "#" + hex
			}
			color, e = danmaku.ParseColor(hex)
			if e != nil {
				return nil, e
			}
		}
		seconds, e := strconv.ParseFloat(string(m.Time), 64)
		if e != nil {
			return nil, errors.New("invalid tencent comment time")
		}
		cid, _ := strconv.ParseInt(string(m.ID), 10, 64)
		c, e := danmaku.NewComment(cid, seconds/1000, mode, 25, color, *m.Content, "[tencent]")
		if e != nil {
			return nil, e
		}
		out = append(out, c)
	}
	return out, nil
}
