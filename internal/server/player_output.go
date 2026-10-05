// SPDX-License-Identifier: AGPL-3.0-only
package server

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"github.com/AniBakaBaka/AniDan/internal/danmaku"
	"github.com/AniBakaBaka/AniDan/internal/store"
	"regexp"
	"strconv"
	"strings"
)

type PlayerComment struct {
	CID int64  `json:"cid"`
	P   string `json:"p"`
	M   string `json:"m"`
}

func (s *Server) playerOutput(ctx context.Context, comments []danmaku.Comment, convert string) ([]PlayerComment, error) {
	options := danmaku.Options{ModeMap: map[int]int{}}
	if strings.EqualFold(s.setting(ctx, "danmakuBlacklistEnabled", "false"), "true") {
		for _, pattern := range strings.Split(s.setting(ctx, "danmakuBlacklistPatterns", ""), "\n") {
			pattern = strings.TrimSpace(pattern)
			if pattern == "" || strings.HasPrefix(pattern, "#") {
				continue
			}
			re, e := regexp.Compile("(?i)" + pattern)
			if e != nil {
				return nil, fmt.Errorf("blacklist pattern is not RE2-compatible: %w", e)
			}
			options.Blacklist = append(options.Blacklist, re)
		}
	}
	options.StripLikes = !strings.EqualFold(s.setting(ctx, "danmakuLikesOutputEnabled", "true"), "true")
	options.LikesStyle = s.setting(ctx, "danmakuLikesStyle", "heart_white")
	options.ColorMode = s.setting(ctx, "danmakuRandomColorMode", "off")
	palette := s.setting(ctx, "danmakuRandomColorPalette", "")
	if palette != "" {
		var vals []any
		if json.Unmarshal([]byte(palette), &vals) != nil {
			for _, part := range strings.Split(palette, ",") {
				vals = append(vals, strings.TrimSpace(part))
			}
		}
		for _, raw := range vals {
			n, e := danmaku.ParseColor(str(raw))
			if e != nil {
				n = 16777215
			}
			options.Palette = append(options.Palette, n)
		}
	}

	mapping := map[string]int{"scroll": 1, "top": 5, "bottom": 4}
	for key, mode := range map[string]int{"danmakuTopConvertTo": 5, "danmakuBottomConvertTo": 4} {
		if v := s.setting(ctx, key, "none"); v != "none" {
			if target, ok := mapping[v]; ok {
				options.ModeMap[mode] = target
			} else {
				return nil, fmt.Errorf("invalid %s value", key)
			}
		}
	}
	playerMode, _ := strconv.Atoi(convert)
	serverMode, _ := strconv.Atoi(s.setting(ctx, "danmakuChConvert", "0"))
	mode := serverMode
	if s.setting(ctx, "danmakuChConvertPriority", "player") == "player" && playerMode != 0 {
		mode = playerMode
	}
	if mode != 0 {
		options.Convert = func(text string) (string, error) { return danmaku.Convert(text, mode) }
	}
	settings := map[string]any{"offset": options.Offset, "colorMode": options.ColorMode, "palette": options.Palette, "stripLikes": options.StripLikes, "likesStyle": options.LikesStyle, "modeMap": options.ModeMap, "convert": mode}
	patterns := []string{}
	for _, v := range options.Blacklist {
		patterns = append(patterns, v.String())
	}
	settings["blacklist"] = patterns
	encoded, _ := json.Marshal(settings)
	hash := sha256.New()
	hash.Write(encoded)
	var length [8]byte
	for _, c := range comments {
		binary.LittleEndian.PutUint64(length[:], uint64(len(c.P)))
		hash.Write(length[:])
		hash.Write([]byte(c.P))
		binary.LittleEndian.PutUint64(length[:], uint64(len(c.M)))
		hash.Write(length[:])
		hash.Write([]byte(c.M))
	}
	key := hex.EncodeToString(hash.Sum(nil))
	load := func() ([]PlayerComment, int64, error) {
		transformed, e := danmaku.Transform(comments, options)
		if e != nil {
			return nil, 0, e
		}
		out, e := danmaku.Player(transformed)
		if e != nil {
			return nil, 0, e
		}
		result := make([]PlayerComment, len(out))
		var size int64
		for i, c := range out {
			result[i] = PlayerComment{CID: int64(i), P: c.P, M: c.M}
			size += int64(48 + len(c.P) + len(c.M))
		}
		return result, size, nil
	}
	if s.outputCache == nil {
		v, _, e := load()
		return v, e
	}
	return s.outputCache.Do(ctx, key, load)
}
func (s *Server) mergedComments(ctx context.Context, ep store.Row, first []danmaku.Comment) ([]danmaku.Comment, error) {
	limit, _ := strconv.Atoi(s.setting(ctx, "danmakuOutputLimitPerSource", "-1"))

	if !strings.EqualFold(s.setting(ctx, "danmakuMergeOutputEnabled", "false"), "true") {
		if limit > 0 {
			first = danmaku.Sample(first, limit)
		}
		return first, nil
	}
	src, e := s.Store.Get(ctx, "anime_sources", ep["source_id"])
	if e != nil {
		return nil, e
	}
	sources, e := s.Store.List(ctx, "anime_sources", store.Row{"anime_id": src["anime_id"]}, 1000, 0)
	if e != nil {
		return nil, e
	}
	batches := [][]danmaku.Comment{first}
	for _, other := range sources {
		if number(other["id"]) == number(src["id"]) {
			continue
		}
		eps, e := s.Store.List(ctx, "episode", store.Row{"source_id": other["id"], "episode_index": ep["episode_index"]}, 1, 0)
		if e != nil {
			return nil, e
		}
		if len(eps) == 0 || str(eps[0]["danmaku_file_path"]) == "" {
			continue
		}
		comments, e := s.readComments(ctx, eps[0])
		if e != nil {
			return nil, e
		}

		batches = append(batches, comments)
	}
	merged, e := danmaku.Merge(batches...)
	if e != nil {
		return nil, e
	}
	if len(merged) < len(first) {
		merged = first
	}
	if limit > 0 {
		merged = danmaku.Sample(merged, limit)
	}
	return merged, nil
}
