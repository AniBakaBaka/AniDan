// SPDX-License-Identifier: AGPL-3.0-only
// Recognition wire contracts adapted from pinned Misaka ui/settings.py,
// recognition_check.py, debug.py, system.py and import_api.py.
package server

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"github.com/AniBakaBaka/AniDan/internal/provider"
	"github.com/AniBakaBaka/AniDan/internal/recognition"
	"github.com/AniBakaBaka/AniDan/internal/store"
	"net/http"
	"strconv"
	"strings"
	"time"
)

func (s *Server) registerRecognition(m *http.ServeMux) {
	m.HandleFunc("GET /api/ui/settings/title-recognition", s.operator(s.recognitionGet))
	m.HandleFunc("PUT /api/ui/settings/title-recognition", s.operator(s.recognitionPut))
	m.HandleFunc("POST /api/ui/settings/title-recognition/test", s.operator(s.recognitionTest))
	m.HandleFunc("GET /api/ui/settings/global-filter", s.operator(s.recognitionGlobalGet))
	m.HandleFunc("PUT /api/ui/settings/global-filter", s.operator(s.recognitionGlobalPut))
	m.HandleFunc("GET /api/ui/settings/global-filter/defaults", s.operator(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"cn": recognition.DefaultBlacklistCN, "eng": recognition.DefaultBlacklistEN})
	}))
	m.HandleFunc("POST /api/ui/settings/regex-test", s.operator(s.recognitionRegexTest))
	m.HandleFunc("GET /api/ui/settings/danmaku-blacklist/defaults", s.operator(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"patterns": recognition.DefaultDanmakuBlacklist})
	}))
	m.HandleFunc("GET /api/ui/settings/single-episode-filter", s.operator(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"content": s.setting(r.Context(), "singleEpisodeFilterRules", "")})
	}))
	m.HandleFunc("PUT /api/ui/settings/single-episode-filter", s.operator(s.recognitionSinglePut))
	m.HandleFunc("GET /api/ui/settings/global-episode-title-filter", s.operator(s.recognitionEpisodeGet))
	m.HandleFunc("PUT /api/ui/settings/global-episode-title-filter", s.operator(s.recognitionEpisodePut))
	m.HandleFunc("GET /api/ui/settings/global-episode-title-filter/defaults", s.operator(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"regex": recognition.DefaultEpisodeFilter, "engine": "RE2", "warnings": []string{"Legacy default uses Python lookaround; edit to a RE2-compatible pattern before enabling"}})
	}))
	m.HandleFunc("GET /api/ui/recognition-check/conflicts", s.operator(s.recognitionConflicts))
	m.HandleFunc("POST /api/ui/recognition-check/test", s.operator(s.recognitionCheck))
	m.HandleFunc("POST /api/ui/debug/match-trace", s.operator(s.recognitionMatchTrace))
	m.HandleFunc("POST /api/ui/tools/parse-filename", s.operator(s.recognitionFilename))
	m.HandleFunc("POST /api/ui/import/preview-offset", s.operator(s.recognitionPreview))
}
func (s *Server) recognitionContent(ctx context.Context) (string, error) {
	rows, e := s.Store.List(ctx, "title_recognition", nil, 1, 0)
	if e != nil {
		return "", e
	}
	if len(rows) > 0 {
		return str(rows[0]["content"]), nil
	}
	return recognition.DefaultRecognitionContent, nil
}
func (s *Server) loadRecognition(ctx context.Context) (*recognition.Rules, []recognition.Warning, error) {
	content, e := s.recognitionContent(ctx)
	if e != nil {
		return nil, nil, e
	}
	rules, warnings := recognition.Parse(content)
	return rules, warnings, nil
}
func recognitionWarnings(warnings []recognition.Warning) []string {
	out := []string{}
	for _, v := range warnings {
		out = append(out, v.String())
	}
	return out
}
func (s *Server) recognitionGet(w http.ResponseWriter, r *http.Request) {
	c, e := s.recognitionContent(r.Context())
	if e != nil {
		httpError(w, 500, e.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"content": c})
}
func (s *Server) saveRecognition(ctx context.Context, content string) error {
	return s.libTransaction(ctx, func(tx *sql.Tx) error {
		var id int64
		e := tx.QueryRowContext(ctx, "SELECT id FROM title_recognition ORDER BY id LIMIT 1").Scan(&id)
		if errors.Is(e, sql.ErrNoRows) {
			_, e = s.Store.InsertTx(ctx, tx, "title_recognition", store.Row{"content": content, "created_at": s.now(), "updated_at": s.now()})
			return e
		}
		if e != nil {
			return e
		}
		_, e = tx.ExecContext(ctx, s.Store.Rebind("UPDATE title_recognition SET content=?,updated_at=? WHERE id=?"), content, s.now(), id)
		return e
	})
}
func (s *Server) recognitionPut(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Content *string `json:"content"`
	}
	if e := readJSON(r, &in); e != nil || in.Content == nil {
		httpError(w, 422, "content is required")
		return
	}
	if len(*in.Content) > recognition.MaxContent {
		httpError(w, 413, "recognition content exceeds 1 MiB")
		return
	}
	_, warnings := recognition.Parse(*in.Content)
	if e := s.saveRecognition(r.Context(), *in.Content); e != nil {
		httpError(w, 500, e.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"success": true, "warnings": recognitionWarnings(warnings)})
}
func (s *Server) recognitionTest(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Title   *string `json:"title"`
		Season  *int    `json:"season"`
		Episode *int    `json:"episode"`
		Source  string  `json:"source"`
		Stage   string  `json:"stage"`
	}
	in.Season = recognition.Int(1)
	in.Episode = recognition.Int(1)
	in.Stage = "all"
	if e := readJSON(r, &in); e != nil || in.Title == nil || len(*in.Title) > recognition.MaxText {
		httpError(w, 422, "title required, maximum 64 KiB")
		return
	}
	rules, warnings, e := s.loadRecognition(r.Context())
	if e != nil {
		httpError(w, 500, e.Error())
		return
	}
	input := recognition.Input{Text: *in.Title, Season: in.Season, Episode: in.Episode, Provider: in.Source}
	var out recognition.Result
	switch in.Stage {
	case "preprocess":
		out = rules.Preprocess(input)
	case "postprocess":
		out = rules.Postprocess(input)
	case "all":
		out = rules.Apply(input)
	default:
		httpError(w, 422, "stage must be all, preprocess, or postprocess")
		return
	}
	matched := []string{}
	for _, v := range out.Trace {
		matched = append(matched, fmt.Sprintf("%s line %d: %s", v.Stage, v.RuleIndex+1, v.Rule))
	}
	writeJSON(w, 200, map[string]any{"originalTitle": in.Title, "processedTitle": out.Text, "originalSeason": in.Season, "processedSeason": out.Season, "originalEpisode": in.Episode, "processedEpisode": out.Episode, "matched": out.Changed, "matchedRules": matched, "metadata": out.Metadata, "warnings": recognitionWarnings(append(warnings, out.Warnings...))})
}
func (s *Server) recognitionGlobalGet(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{"cn": s.setting(r.Context(), "search_result_global_blacklist_cn", ""), "eng": s.setting(r.Context(), "search_result_global_blacklist_eng", "")})
}
func (s *Server) recognitionGlobalPut(w http.ResponseWriter, r *http.Request) {
	var in struct {
		CN *string `json:"cn"`
		EN *string `json:"eng"`
	}
	if e := readJSON(r, &in); e != nil || in.CN == nil || in.EN == nil {
		httpError(w, 422, "cn and eng strings required")
		return
	}
	if _, e := recognition.NewBlacklist(*in.CN, *in.EN); e != nil {
		httpError(w, 422, e.Error())
		return
	}
	if e := s.setSettingsAtomic(r.Context(), map[string]string{"search_result_global_blacklist_cn": *in.CN, "search_result_global_blacklist_eng": *in.EN}); e != nil {
		httpError(w, 500, e.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"message": "全局过滤规则已更新。"})
}
func (s *Server) recognitionRegexTest(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Text     string `json:"text"`
		Patterns []struct {
			Label   string `json:"label"`
			Pattern string `json:"pattern"`
		} `json:"patterns"`
	}
	if e := readJSON(r, &in); e != nil || len(in.Text) > recognition.MaxText || len(in.Patterns) > 256 {
		httpError(w, 422, "maximum 64 KiB text and 256 patterns")
		return
	}
	matches := []any{}
	invalids := []any{}
	for _, v := range in.Patterns {
		pattern := strings.TrimSpace(v.Pattern)
		if pattern == "" {
			continue
		}
		re, e := recognition.CompileRegex(pattern)
		if e != nil {
			invalids = append(invalids, map[string]any{"label": v.Label, "pattern": pattern, "error": e.Error()})
			continue
		}
		m, e := re.FindStringIndex(in.Text)
		if e != nil {
			invalids = append(invalids, map[string]any{"label": v.Label, "pattern": pattern, "error": e.Error()})
			continue
		}
		if m != nil {
			matches = append(matches, map[string]any{"label": v.Label, "pattern": pattern, "matchedText": in.Text[m[0]:m[1]]})
		}
	}
	writeJSON(w, 200, map[string]any{"matched": len(matches) > 0, "matches": matches, "invalids": invalids, "engine": "RE2 + bounded regexp2 compatibility"})
}
func (s *Server) recognitionSinglePut(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Content string `json:"content"`
	}
	if e := readJSON(r, &in); e != nil {
		httpError(w, 422, e.Error())
		return
	}
	if len(in.Content) > recognition.MaxContent {
		httpError(w, 413, "filter content exceeds 1 MiB")
		return
	}
	_, warnings := recognition.ParseEpisodeFilters(in.Content)
	if len(warnings) > 0 {
		writeJSON(w, 422, map[string]any{"detail": "invalid single-episode filters", "warnings": recognitionWarnings(warnings)})
		return
	}
	if e := s.setSetting(r.Context(), "singleEpisodeFilterRules", in.Content); e != nil {
		httpError(w, 500, e.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"message": "单剧分集过滤规则已更新。"})
}
func (s *Server) recognitionEpisodeGet(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{"enabled": s.setting(r.Context(), "globalEpisodeTitleFilterEnabled", "false") == "true", "regex": s.setting(r.Context(), "globalEpisodeTitleFilterRegex", "")})
}
func (s *Server) recognitionEpisodePut(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Enabled bool   `json:"enabled"`
		Regex   string `json:"regex"`
	}
	if e := readJSON(r, &in); e != nil {
		httpError(w, 422, e.Error())
		return
	}
	if in.Regex != "" {
		if _, e := recognition.CompileRegex(in.Regex); e != nil {
			httpError(w, 422, e.Error())
			return
		}
	}
	if e := s.setSettingsAtomic(r.Context(), map[string]string{"globalEpisodeTitleFilterEnabled": strconv.FormatBool(in.Enabled), "globalEpisodeTitleFilterRegex": in.Regex}); e != nil {
		httpError(w, 500, e.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"message": "兜底全局分集标题过滤配置已更新。"})
}
func (s *Server) recognitionConflicts(w http.ResponseWriter, r *http.Request) {
	content, e := s.recognitionContent(r.Context())
	if e != nil {
		httpError(w, 500, e.Error())
		return
	}
	writeJSON(w, 200, recognition.Conflicts(content))
}
func (s *Server) recognitionCheck(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Title *string `json:"title"`
	}
	if e := readJSON(r, &in); e != nil || in.Title == nil || len(*in.Title) > recognition.MaxText {
		httpError(w, 422, "title required, maximum 64 KiB")
		return
	}
	rules, warnings, e := s.loadRecognition(r.Context())
	if e != nil {
		httpError(w, 500, e.Error())
		return
	}
	out := rules.Apply(recognition.Input{Text: *in.Title})
	writeJSON(w, 200, map[string]any{"originalTitle": in.Title, "matchedRules": out.Trace, "transformedTitle": out.Text, "seasonOffset": nil, "warnings": recognitionWarnings(append(warnings, out.Warnings...))})
}
func (s *Server) recognitionFilename(w http.ResponseWriter, r *http.Request) {
	var in struct {
		FileName *string `json:"fileName"`
	}
	if e := readJSON(r, &in); e != nil || in.FileName == nil || len(*in.FileName) > recognition.MaxText {
		httpError(w, 422, "fileName required, maximum 64 KiB")
		return
	}
	p := recognition.ParseFilename(*in.FileName)
	if p == nil {
		writeJSON(w, 200, map[string]any{"success": false, "message": "无法识别该文件名", "result": nil})
		return
	}
	writeJSON(w, 200, map[string]any{"success": true, "result": p})
}
func (s *Server) recognitionPreview(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Title    *string `json:"animeTitle"`
		Episodes []int   `json:"episodeIndices"`
	}
	if e := readJSON(r, &in); e != nil || in.Title == nil || len(*in.Title) > recognition.MaxText || len(in.Episodes) > 10000 {
		httpError(w, 422, "animeTitle and up to 10000 episodeIndices required")
		return
	}
	rules, warnings, e := s.loadRecognition(r.Context())
	if e != nil {
		httpError(w, 500, e.Error())
		return
	}
	offsets := map[int]int{}
	for _, ep := range in.Episodes {
		out := rules.Postprocess(recognition.Input{Text: *in.Title, Episode: recognition.Int(ep)})
		if out.Episode != nil && *out.Episode != ep {
			offsets[ep] = *out.Episode
		}
		warnings = append(warnings, out.Warnings...)
	}
	writeJSON(w, 200, map[string]any{"offsetMap": offsets, "hasOffset": len(offsets) > 0, "warnings": recognitionWarnings(warnings)})
}

// filterSearchResults preserves input priority/order. Invalid migrated rules fail
// explicitly instead of silently suppressing or admitting search candidates.
func (s *Server) filterSearchResults(ctx context.Context, rows []provider.SearchResult) ([]provider.SearchResult, error) {
	b, e := recognition.NewBlacklist(s.setting(ctx, "search_result_global_blacklist_cn", ""), s.setting(ctx, "search_result_global_blacklist_eng", ""))
	if e != nil {
		return nil, e
	}
	out := make([]provider.SearchResult, 0, len(rows))
	for _, v := range rows {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		matched, err := b.Match(v.Title)
		if err != nil {
			return nil, err
		}
		if !matched {
			out = append(out, v)
		}
	}
	return out, nil
}
func (s *Server) filterEpisodes(ctx context.Context, episodes []provider.Episode, title, p, mediaID string, aliases []string) ([]provider.Episode, error) {
	filters, warnings := recognition.ParseEpisodeFilters(s.setting(ctx, "singleEpisodeFilterRules", ""))
	if len(warnings) > 0 {
		return nil, fmt.Errorf("single episode filter: %s", warnings[0].String())
	}
	global := s.setting(ctx, "globalEpisodeTitleFilterRegex", "")
	enabled := s.setting(ctx, "globalEpisodeTitleFilterEnabled", "false") == "true"
	out := append([]provider.Episode{}, episodes...)
	if enabled && global != "" {
		re, e := recognition.CompileRegex(global)
		if e != nil {
			return nil, fmt.Errorf("globalEpisodeTitleFilterRegex: %w", e)
		}
		kept := out[:0]
		for _, ep := range out {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			matched, err := re.MatchString(ep.Title)
			if err != nil {
				return nil, err
			}
			if !matched {
				kept = append(kept, ep)
			}
		}
		out = kept
	}
	for _, f := range filters {
		if !f.Applies(title, p, mediaID, aliases) {
			continue
		}
		kept := out[:0]
		for _, ep := range out {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			matched, err := f.Match(ep.Title)
			if err != nil {
				return nil, err
			}
			if !matched {
				kept = append(kept, ep)
			}
		}
		out = kept
	}
	return out, nil
}
func (s *Server) recognitionMatchTrace(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Title     *string `json:"title"`
		Year      *int    `json:"year"`
		Season    *int    `json:"season"`
		Episode   *int    `json:"episode"`
		MediaType string  `json:"media_type"`
	}
	if e := readJSON(r, &in); e != nil || in.Title == nil || len(*in.Title) > recognition.MaxText || strings.TrimSpace(*in.Title) == "" {
		httpError(w, 422, "nonempty title required, maximum 64 KiB")
		return
	}
	start := time.Now()
	steps := []any{}
	step := func(name string, t time.Time, input, output any, e error) {
		v := map[string]any{"name": name, "duration_ms": float64(time.Since(t).Microseconds()) / 1000, "success": e == nil, "input_data": input, "output_data": output}
		if e != nil {
			v["details"] = e.Error()
		}
		steps = append(steps, v)
	}
	t := time.Now()
	parsed := recognition.ParseSearchKeyword(*in.Title)
	if in.Season != nil {
		parsed.Season = in.Season
	}
	if in.Episode != nil {
		parsed.Episode = in.Episode
	}
	step("关键词解析", t, map[string]any{"keyword": in.Title}, parsed, nil)
	t = time.Now()
	plan, e := s.prepareSearch(r.Context(), *in.Title)
	count := 0
	if e != nil {
		step("识别词预处理", t, parsed, nil, e)
	} else {
		if in.Season != nil && plan.Mapping == nil {
			plan.Season = in.Season
		}
		if in.Episode != nil {
			plan.Episode = in.Episode
		}
		step("识别词预处理", t, parsed, map[string]any{"processed_title": plan.Title, "season": plan.Season, "episode": plan.Episode, "rules": plan.Trace, "warnings": plan.Warnings}, nil)
		step("识别词搜索映射", time.Now(), *in.Title, plan.Mapping, nil)
		step("名称转换", time.Now(), parsed.Title, map[string]any{"processed_title": plan.Title, "metadata": plan.Metadata, "enabled": s.setting(r.Context(), "nameConversionEnabled", "false") == "true", "warnings": plan.Warnings}, nil)
		t = time.Now()
		results, searchErr := s.searchRecognition(r.Context(), plan)
		resultRows := []any{}
		for _, v := range results {
			count++
			if len(resultRows) < 20 {
				resultRows = append(resultRows, plan.result(v))
			}
		}
		step("弹幕源搜索", t, map[string]any{"keywords": uniqueTitles(append([]string{plan.Title}, plan.Aliases...)...)}, map[string]any{"total_results": count, "results": resultRows}, searchErr)
		step("补充元数据别名搜索", time.Now(), plan.Title, map[string]any{"aliases": plan.Aliases, "warnings": plan.Warnings}, nil)
	}
	writeJSON(w, 200, map[string]any{"title": in.Title, "steps": steps, "total_duration_ms": float64(time.Since(start).Microseconds()) / 1000, "result_count": count})
}
