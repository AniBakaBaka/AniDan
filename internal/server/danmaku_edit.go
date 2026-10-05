// SPDX-License-Identifier: AGPL-3.0-only
package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/AniBakaBaka/AniDan/internal/danmaku"
	"github.com/AniBakaBaka/AniDan/internal/store"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

func (s *Server) registerDanmakuEdit(m *http.ServeMux) {
	m.HandleFunc("GET /api/ui/danmaku/detail/{episodeId}", s.operator(s.danmakuDetail))
	m.HandleFunc("GET /api/ui/danmaku/comments/{episodeId}", s.operator(s.danmakuPage))
	m.HandleFunc("POST /api/ui/danmaku/offset", s.operator(s.danmakuOffset))
	m.HandleFunc("POST /api/ui/danmaku/split", s.operator(s.danmakuSplit))
	m.HandleFunc("POST /api/ui/danmaku/merge", s.operator(s.danmakuMerge))
	m.HandleFunc("GET /api/control/danmaku/{episodeId}", s.operator(s.controlComments))
	m.HandleFunc("POST /api/control/danmaku/{episodeId}", s.operator(s.controlOverwrite))
	if e := s.Jobs.Register("overwrite_comments", s.runOverwrite); e != nil {
		panic(e)
	}
}

type previewComment struct {
	Time    float64 `json:"time"`
	Content string  `json:"content"`
	Source  string  `json:"source"`
}

func preview(c danmaku.Comment) previewComment {
	source := ""
	if p := strings.Split(c.P, ","); len(p) > 4 {
		source = strings.Trim(p[4], "[]")
	}
	return previewComment{c.T, c.M, source}
}
func (s *Server) episodeComments(r *http.Request) (store.Row, []danmaku.Comment, error) {
	id, e := idParam(r, "episodeId")
	if e != nil {
		return nil, nil, e
	}
	ep, e := s.Store.Get(r.Context(), "episode", id)
	if e != nil {
		return nil, nil, e
	}
	comments, e := s.readComments(r.Context(), ep)
	return ep, comments, e
}
func (s *Server) danmakuDetail(w http.ResponseWriter, r *http.Request) {
	ep, comments, e := s.episodeComments(r)
	if e != nil || len(comments) == 0 {
		httpError(w, 404, "分集不存在或没有弹幕")
		return
	}
	start, end := comments[0].T, comments[0].T
	sources := map[string]int{}
	dist := map[int]int{}
	previews := []previewComment{}
	for i, c := range comments {
		p := preview(c)
		sources[p.Source]++
		dist[int(c.T/60)]++
		if c.T < start {
			start = c.T
		}
		if c.T > end {
			end = c.T
		}
		if i < 100 {
			previews = append(previews, p)
		}
	}
	sv := []map[string]any{}
	for k, n := range sources {
		sv = append(sv, map[string]any{"name": k, "count": n})
	}
	sort.Slice(sv, func(i, j int) bool { return str(sv[i]["name"]) < str(sv[j]["name"]) })
	dv := []map[string]any{}
	for minute, n := range dist {
		dv = append(dv, map[string]any{"minute": minute, "count": n})
	}
	sort.Slice(dv, func(i, j int) bool { return number(dv[i]["minute"]) < number(dv[j]["minute"]) })
	writeJSON(w, 200, map[string]any{"episodeId": ep["id"], "totalCount": len(comments), "timeRange": map[string]any{"start": start, "end": end}, "sources": sv, "distribution": dv, "comments": previews})
}
func (s *Server) danmakuPage(w http.ResponseWriter, r *http.Request) {
	_, comments, e := s.episodeComments(r)
	if e != nil {
		httpError(w, 404, e.Error())
		return
	}
	page, size := pageParams(r)
	if r.URL.Query().Get("pageSize") == "" {
		size = 100
	}
	from, to := -math.MaxFloat64, math.MaxFloat64
	if raw := r.URL.Query().Get("startTime"); raw != "" {
		from, e = strconv.ParseFloat(raw, 64)
		if e != nil || math.IsNaN(from) || math.IsInf(from, 0) {
			httpError(w, 400, "invalid startTime")
			return
		}
	}
	if raw := r.URL.Query().Get("endTime"); raw != "" {
		to, e = strconv.ParseFloat(raw, 64)
		if e != nil || math.IsNaN(to) || math.IsInf(to, 0) {
			httpError(w, 400, "invalid endTime")
			return
		}
	}
	total := 0
	out := []previewComment{}
	for _, c := range comments {
		if c.T < from || c.T > to {
			continue
		}
		if total >= (page-1)*size && len(out) < size {
			out = append(out, preview(c))
		}
		total++
	}
	writeJSON(w, 200, map[string]any{"total": total, "comments": out, "page": page, "pageSize": size})
}

type editObject struct {
	Row   store.Row
	Path  string
	Count int
	New   bool
}

func (s *Server) stageEdit(ctx context.Context, row store.Row, comments []danmaku.Comment, isNew bool) (editObject, error) {
	src, e := s.Store.Get(ctx, "anime_sources", row["source_id"])
	if e != nil {
		return editObject{}, e
	}
	path, e := s.writeCommentObject(ctx, number(src["anime_id"]), number(row["id"]), comments)
	return editObject{row, path, len(comments), isNew}, e
}
func (s *Server) commitEdits(ctx context.Context, plans []editObject, deletes []int64, inputs []editSnapshot) error {
	s.fileMu.Lock()
	defer s.fileMu.Unlock()
	if s.fileFault.Load() {
		return errors.New("storage recovery required before further writes")
	}
	return s.libTransaction(ctx, func(tx *sql.Tx) error {
		if err := s.validateEditSnapshots(ctx, tx, inputs, plans, deletes); err != nil {
			return err
		}
		for _, id := range deletes {
			if _, e := tx.ExecContext(ctx, s.Store.Rebind("DELETE FROM episode WHERE id=?"), id); e != nil {
				return e
			}
		}
		for _, p := range plans {
			if p.New {
				row := store.Row{}
				for k, v := range p.Row {
					row[k] = v
				}
				row["danmaku_file_path"] = p.Path
				row["comment_count"] = p.Count
				row["fetched_at"] = s.now()
				if _, e := s.Store.InsertTx(ctx, tx, "episode", row); e != nil {
					return e
				}
			} else {
				res, e := tx.ExecContext(ctx, s.Store.Rebind("UPDATE episode SET danmaku_file_path=?,comment_count=?,fetched_at=? WHERE id=?"), p.Path, p.Count, s.now(), p.Row["id"])
				if e != nil {
					return e
				}
				n, e := res.RowsAffected()
				if e != nil {
					return e
				}
				// MySQL reports changed rather than matched rows by default. A
				// same-second no-op may report zero; the locked snapshot above
				// already proved that this exact episode exists in this transaction.
				if n != 1 && !(s.Store.Dialect == "mysql" && n == 0) {
					return errors.New("episode changed during edit")
				}
			}
		}
		return nil
	})
}
func (s *Server) danmakuOffset(w http.ResponseWriter, r *http.Request) {
	var in struct {
		EpisodeIDs []int64 `json:"episodeIds"`
		Offset     float64 `json:"offsetSeconds"`
	}
	if e := readJSON(r, &in); e != nil {
		httpError(w, 400, e.Error())
		return
	}
	if len(in.EpisodeIDs) == 0 || len(in.EpisodeIDs) > 1000 || math.IsNaN(in.Offset) || math.IsInf(in.Offset, 0) {
		httpError(w, 400, "invalid episodeIds or offsetSeconds")
		return
	}
	plans := []editObject{}
	inputs := []editSnapshot{}
	total := 0
	seen := map[int64]bool{}
	for _, id := range in.EpisodeIDs {
		if seen[id] {
			continue
		}
		seen[id] = true
		input, comments, e := s.readEditSnapshot(r.Context(), id)
		if e != nil {
			editInputError(w, e)
			return
		}
		ep := input.Episode
		inputs = append(inputs, input)
		out, e := offsetForEdit(comments, in.Offset, true)
		if e != nil {
			httpError(w, 400, e.Error())
			return
		}
		plan, e := s.stageEdit(r.Context(), ep, out, false)
		if e != nil {
			httpError(w, 500, e.Error())
			return
		}
		plans = append(plans, plan)
		total += len(out)
	}
	if e := s.commitEdits(r.Context(), plans, nil, inputs); e != nil {
		httpError(w, 409, e.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"success": true, "modifiedCount": len(plans), "totalComments": total})
}
func (s *Server) danmakuSplit(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Source int64 `json:"sourceEpisodeId"`
		Splits []struct {
			Index int     `json:"episodeIndex"`
			Start float64 `json:"startTime"`
			End   float64 `json:"endTime"`
			Title *string `json:"title"`
		} `json:"splits"`
		Delete *bool `json:"deleteSource"`
		Reset  *bool `json:"resetTime"`
	}
	if e := readJSON(r, &in); e != nil {
		httpError(w, 400, e.Error())
		return
	}
	if len(in.Splits) == 0 || len(in.Splits) > 100 {
		httpError(w, 400, "splits must contain1..100 ranges")
		return
	}
	input, comments, e := s.readEditSnapshot(r.Context(), in.Source)
	if e != nil {
		editInputError(w, e)
		return
	}
	ep, src := input.Episode, input.Source
	deleting := in.Delete == nil || *in.Delete
	reset := in.Reset == nil || *in.Reset
	plans := []editObject{}
	out := []map[string]any{}
	indexes := map[int]bool{}
	for _, part := range in.Splits {
		if part.Index < 0 || indexes[part.Index] || part.Start < 0 || part.End <= part.Start {
			httpError(w, 400, "invalid or duplicate split range/index")
			return
		}
		indexes[part.Index] = true
		id, e := episodeIdentifier(number(src["anime_id"]), number(src["source_order"]), int64(part.Index))
		if e != nil {
			httpError(w, 400, e.Error())
			return
		}
		if id == in.Source && !deleting {
			httpError(w, 409, "split would replace retained source")
			return
		}
		options := danmaku.Options{Start: &part.Start, End: &part.End}
		filtered, e := danmaku.Transform(comments, options)
		if e != nil {
			httpError(w, 400, e.Error())
			return
		}
		if reset {
			filtered, e = danmaku.Offset(filtered, -part.Start)
			if e != nil {
				httpError(w, 400, e.Error())
				return
			}
		}
		title := fmt.Sprintf("第%d集", part.Index)
		if part.Title != nil {
			title = *part.Title
		}
		row := store.Row{"id": id, "source_id": ep["source_id"], "title": title, "episode_index": part.Index, "provider_episode_id": ep["provider_episode_id"], "source_url": ep["source_url"]}
		plan, e := s.stageEdit(r.Context(), row, filtered, true)
		if e != nil {
			httpError(w, 500, e.Error())
			return
		}
		plans = append(plans, plan)
		out = append(out, map[string]any{"episodeId": id, "episodeIndex": part.Index, "commentCount": len(filtered)})
	}
	deletes := []int64{}
	if deleting {
		deletes = append(deletes, in.Source)
	}
	if e = s.commitEdits(r.Context(), plans, deletes, []editSnapshot{input}); e != nil {
		writeJSON(w, 200, map[string]any{"success": false, "error": e.Error(), "newEpisodes": []any{}})
		return
	}
	writeJSON(w, 200, map[string]any{"success": true, "error": nil, "newEpisodes": out})
}
func (s *Server) danmakuMerge(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Sources []struct {
			ID     int64   `json:"episodeId"`
			Offset float64 `json:"offsetSeconds"`
		} `json:"sourceEpisodes"`
		Index  int    `json:"targetEpisodeIndex"`
		Title  string `json:"targetTitle"`
		Delete *bool  `json:"deleteSources"`
		Dedup  bool   `json:"deduplicate"`
	}
	if e := readJSON(r, &in); e != nil {
		httpError(w, 400, e.Error())
		return
	}
	if len(in.Sources) == 0 || len(in.Sources) > 100 || in.Index < 0 {
		httpError(w, 400, "invalid sourceEpisodes or index")
		return
	}
	var first store.Row
	inputs := []editSnapshot{}
	combined := []danmaku.Comment{}
	deletes := []int64{}
	seen := map[int64]bool{}
	for _, v := range in.Sources {
		if seen[v.ID] {
			httpError(w, 400, "duplicate source episode")
			return
		}
		seen[v.ID] = true
		input, comments, e := s.readEditSnapshot(r.Context(), v.ID)
		if e != nil {
			editInputError(w, e)
			return
		}
		ep := input.Episode
		inputs = append(inputs, input)
		if first == nil {
			first = ep
		}
		comments, e = offsetForEdit(comments, v.Offset, false)
		if e != nil {
			httpError(w, 400, e.Error())
			return
		}
		if len(combined)+len(comments) > 1000000 {
			httpError(w, 413, "merged comment limit exceeded")
			return
		}
		combined = append(combined, comments...)
		if in.Delete == nil || *in.Delete {
			deletes = append(deletes, v.ID)
		}
	}
	out := combined
	if in.Dedup {
		seen := map[string]bool{}
		out = []danmaku.Comment{}
		for _, c := range combined {
			key := fmt.Sprintf("%.1f_%s", c.T, c.M)
			if !seen[key] {
				seen[key] = true
				out = append(out, c)
			}
		}
	}
	var e error
	sort.SliceStable(out, func(i, j int) bool { return out[i].T < out[j].T })
	src, e := s.Store.Get(r.Context(), "anime_sources", first["source_id"])
	if e != nil {
		httpError(w, 404, e.Error())
		return
	}
	id, e := episodeIdentifier(number(src["anime_id"]), number(src["source_order"]), int64(in.Index))
	if e != nil {
		httpError(w, 400, e.Error())
		return
	}
	row := store.Row{"id": id, "source_id": first["source_id"], "title": in.Title, "episode_index": in.Index}
	plan, e := s.stageEdit(r.Context(), row, out, true)
	if e != nil {
		httpError(w, 500, e.Error())
		return
	}
	if e = s.commitEdits(r.Context(), []editObject{plan}, deletes, inputs); e != nil {
		writeJSON(w, 200, map[string]any{"success": false, "error": e.Error(), "newEpisodeId": nil, "commentCount": 0})
		return
	}
	writeJSON(w, 200, map[string]any{"success": true, "error": nil, "newEpisodeId": id, "commentCount": len(out)})
}
func (s *Server) controlComments(w http.ResponseWriter, r *http.Request) {
	_, comments, e := s.episodeComments(r)
	if e != nil {
		httpError(w, 404, e.Error())
		return
	}
	out := []PlayerComment{}
	for _, c := range comments {
		out = append(out, PlayerComment{CID: c.CID, P: c.P, M: c.M})
	}
	writeJSON(w, 200, map[string]any{"count": len(out), "comments": out, "status": nil, "taskId": nil, "episodeId": nil, "progress": nil, "description": nil})
}

type overwriteParams struct {
	ID         int64             `json:"episodeId"`
	Comments   []danmaku.Comment `json:"comments,omitempty"`
	PayloadRef string            `json:"payloadRef,omitempty"`
}

func (s *Server) controlOverwrite(w http.ResponseWriter, r *http.Request) {
	id, e := idParam(r, "episodeId")
	if e != nil {
		httpError(w, 400, e.Error())
		return
	}
	var in overwriteParams
	if e = readJSON(r, &in); e != nil {
		httpError(w, 400, e.Error())
		return
	}
	in.ID = id
	in.PayloadRef = ""
	if _, e = s.Store.Get(r.Context(), "episode", id); e != nil {
		httpError(w, 404, e.Error())
		return
	}
	for i, c := range in.Comments {
		normalized, e := danmaku.Normalize(c, "[custom]")
		if e != nil {
			httpError(w, 400, e.Error())
			return
		}
		in.Comments[i] = normalized
	}
	encoded, e := json.Marshal(in.Comments)
	if e != nil {
		httpError(w, 400, e.Error())
		return
	}
	if len(encoded) > 256<<10 {
		in.PayloadRef, e = s.spoolImportPayload(r.Context(), encoded)
		if e != nil {
			httpError(w, 400, e.Error())
			return
		}
		in.Comments = nil
	}
	task, e := s.Jobs.SubmitRegistered("overwrite_comments", in)
	if e != nil {
		httpError(w, 503, e.Error())
		return
	}
	writeJSON(w, 202, map[string]any{"status": "success", "message": "弹幕覆盖任务已提交", "taskId": task})
}
func (s *Server) runOverwrite(ctx context.Context, raw json.RawMessage, progress func(int, string)) (any, error) {
	var in overwriteParams
	if e := unmarshalExactJSON(raw, &in); e != nil {
		return nil, e
	}
	if in.PayloadRef != "" {
		b, e := s.readImportPayload(in.PayloadRef)
		if e != nil {
			return nil, e
		}
		if e = unmarshalExactJSON(b, &in.Comments); e != nil {
			return nil, e
		}
	}
	ep, e := s.Store.Get(ctx, "episode", in.ID)
	if e != nil {
		return nil, e
	}
	progress(10, "Writing immutable replacement")
	if e = s.saveComments(ctx, ep, in.Comments, false); e != nil {
		return nil, e
	}
	progress(100, "Comments replaced")
	return map[string]any{"episodeId": in.ID, "count": len(in.Comments)}, nil
}

func offsetForEdit(in []danmaku.Comment, offset float64, clamp bool) ([]danmaku.Comment, error) {
	if math.IsNaN(offset) || math.IsInf(offset, 0) {
		return nil, errors.New("non-finite offset")
	}
	out := make([]danmaku.Comment, len(in))
	for i, c := range in {
		c.T += offset
		if math.IsNaN(c.T) || math.IsInf(c.T, 0) {
			return nil, errors.New("time overflow")
		}
		if clamp && c.T < 0 {
			c.T = 0
		}
		parts := strings.Split(c.P, ",")
		parts[0] = fmt.Sprintf("%.3f", c.T)
		c.P = strings.Join(parts, ",")
		out[i] = c
	}
	return out, nil
}
