// SPDX-License-Identifier: AGPL-3.0-only
package server

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/AniBakaBaka/AniDan/internal/store"
)

type libResolution struct {
	Provider string `json:"providerName"`
	Offset   int64  `json:"sourceOffset"`
	Episodes []struct {
		Index int64 `json:"episodeIndex"`
		Keep  bool  `json:"keepSource"`
	} `json:"episodeResolutions"`
}
type libReassociateRequest struct {
	Target      int64           `json:"targetAnimeId"`
	Resolutions []libResolution `json:"resolutions"`
}

func (s *Server) registerLibraryRelations(m *http.ServeMux) {
	m.HandleFunc("POST /api/ui/library/anime/{sourceAnimeId}/reassociate/check", s.libCheckReassociation)
	m.HandleFunc("POST /api/ui/library/anime/{sourceAnimeId}/reassociate", s.libReassociate)
	m.HandleFunc("POST /api/ui/library/anime/{sourceAnimeId}/reassociate/resolve", s.libReassociate)
	m.HandleFunc("GET /api/ui/library/source/{sourceId}/episodes-for-split", s.libSplitEpisodes)
	m.HandleFunc("POST /api/ui/library/anime/{animeId}/split-source", s.libSplitSource)
	m.HandleFunc("GET /api/ui/library/scan-duplicates", s.libDuplicates)
	m.HandleFunc("POST /api/ui/library/batch-merge", s.libBatchMerge)
}
func (s *Server) libSourcesQ(ctx context.Context, q libQueryer, id int64) ([]store.Row, error) {
	return s.libRows(ctx, q, "anime_sources", "SELECT * FROM anime_sources WHERE anime_id = ? ORDER BY source_order,id", id)
}
func (s *Server) libEpisodesQ(ctx context.Context, q libQueryer, id int64) ([]store.Row, error) {
	return s.libRows(ctx, q, "episode", "SELECT * FROM episode WHERE source_id = ? ORDER BY episode_index,id", id)
}
func (s *Server) libConflictReport(ctx context.Context, from, to int64) (store.Row, error) {
	if from == to {
		return nil, libErr(400, "源作品和目标作品不能相同")
	}
	for _, id := range []int64{from, to} {
		if a, e := s.Store.Get(ctx, "anime", id); e != nil || a == nil {
			return nil, sql.ErrNoRows
		}
	}
	src, e := s.libSourcesQ(ctx, s.Store.DB, from)
	if e != nil {
		return nil, e
	}
	dst, e := s.libSourcesQ(ctx, s.Store.DB, to)
	if e != nil {
		return nil, e
	}
	byProvider := map[string]store.Row{}
	for _, v := range dst {
		byProvider[authString(v["provider_name"])] = v
	}
	conflicts := []store.Row{}
	for _, v := range src {
		target := byProvider[authString(v["provider_name"])]
		if target == nil {
			continue
		}
		se, e := s.libEpisodesQ(ctx, s.Store.DB, authInt(v["id"]))
		if e != nil {
			return nil, e
		}
		te, e := s.libEpisodesQ(ctx, s.Store.DB, authInt(target["id"]))
		if e != nil {
			return nil, e
		}
		byIndex := map[int64]store.Row{}
		for _, ep := range te {
			byIndex[authInt(ep["episode_index"])] = ep
		}
		eps := []store.Row{}
		for _, ep := range se {
			other := byIndex[authInt(ep["episode_index"])]
			if other != nil {
				eps = append(eps, store.Row{"episodeIndex": ep["episode_index"], "sourceEpisodeId": ep["id"], "targetEpisodeId": other["id"], "sourceDanmakuCount": authInt(ep["comment_count"]), "targetDanmakuCount": authInt(other["comment_count"]), "sourceLastFetchTime": authDateWire(ep["fetched_at"]), "targetLastFetchTime": authDateWire(other["fetched_at"])})
			}
		}
		if len(eps) > 0 {
			conflicts = append(conflicts, store.Row{"providerName": v["provider_name"], "sourceSourceId": v["id"], "targetSourceId": target["id"], "conflictEpisodes": eps})
		}
	}
	return store.Row{"hasConflict": len(conflicts) > 0, "conflicts": conflicts}, nil
}
func (s *Server) libCheckReassociation(w http.ResponseWriter, r *http.Request) {
	if !s.libAuthorized(w, r) {
		return
	}
	from, e := idParam(r, "sourceAnimeId")
	var b libReassociateRequest
	if e != nil || readJSON(r, &b) != nil || b.Target <= 0 {
		httpError(w, 422, "Invalid targetAnimeId")
		return
	}
	v, e := s.libConflictReport(r.Context(), from, b.Target)
	if e != nil {
		libWriteError(w, e)
		return
	}
	writeJSON(w, 200, v)
}
func (s *Server) libReassociateTx(ctx context.Context, tx *sql.Tx, from, to int64, resolutions []libResolution, explicit bool) error {
	if from == to {
		return libErr(400, "源作品和目标作品不能相同")
	}
	for _, id := range []int64{from, to} {
		if _, e := s.libOne(ctx, tx, "anime", "id = ?", id); e != nil {
			return e
		}
	}
	sources, e := s.libSourcesQ(ctx, tx, from)
	if e != nil {
		return e
	}
	targets, e := s.libSourcesQ(ctx, tx, to)
	if e != nil {
		return e
	}
	byProvider := map[string]store.Row{}
	var order int64
	for _, v := range targets {
		byProvider[authString(v["provider_name"])] = v
		if n := authInt(v["source_order"]); n > order {
			order = n
		}
	}
	rm := map[string]libResolution{}
	for _, v := range resolutions {
		if _, ok := rm[v.Provider]; ok {
			return libErr(422, "Duplicate provider resolution")
		}
		rm[v.Provider] = v
	}
	for _, src := range sources {
		provider := authString(src["provider_name"])
		target := byProvider[provider]
		if target == nil {
			order++
			if e = s.libUpdate(ctx, tx, "anime_sources", src["id"], store.Row{"anime_id": to, "source_order": order}); e != nil {
				return e
			}
			src["anime_id"] = to
			src["source_order"] = order
			byProvider[provider] = src
			continue
		}
		resolution, hasResolution := rm[provider]
		if explicit && !hasResolution {
			return libErr(409, "A resolution is required for every conflicting provider")
		}
		keep := map[int64]bool{}
		for _, v := range resolution.Episodes {
			keep[v.Index] = v.Keep
		}
		episodes, e := s.libEpisodesQ(ctx, tx, authInt(src["id"]))
		if e != nil {
			return e
		}
		for _, ep := range episodes {
			index := authInt(ep["episode_index"]) + resolution.Offset
			if index < 1 {
				return libErr(422, "Adjusted episode index must be positive")
			}
			other, e := s.libOne(ctx, tx, "episode", "source_id = ? AND episode_index = ?", target["id"], index)
			if e != nil && !errors.Is(e, sql.ErrNoRows) {
				return e
			}
			if other != nil {
				if keep[authInt(ep["episode_index"])] {
					if _, e = tx.ExecContext(ctx, s.Store.Rebind("DELETE FROM episode WHERE id = ?"), other["id"]); e != nil {
						return e
					}
				} else {
					if _, e = tx.ExecContext(ctx, s.Store.Rebind("DELETE FROM episode WHERE id = ?"), ep["id"]); e != nil {
						return e
					}
					continue
				}
			}
			if e = s.libUpdate(ctx, tx, "episode", ep["id"], store.Row{"source_id": target["id"], "episode_index": index}); e != nil {
				return e
			}
		}
		if _, e = tx.ExecContext(ctx, s.Store.Rebind("UPDATE external_calendar_item SET local_source_id = ?, local_anime_id = ? WHERE local_source_id = ?"), target["id"], to, src["id"]); e != nil {
			return e
		}
		if _, e = tx.ExecContext(ctx, s.Store.Rebind("DELETE FROM anime_sources WHERE id = ?"), src["id"]); e != nil {
			return e
		}
	}
	// Keep source orders stable; changing them without rekeying every episode is unsafe.
	// XML paths remain valid and immutable, avoiding rename+SQL crash windows.
	_, e = tx.ExecContext(ctx, s.Store.Rebind("UPDATE external_calendar_item SET local_anime_id = ? WHERE local_anime_id = ?"), to, from)
	if e != nil {
		return e
	}
	_, e = tx.ExecContext(ctx, s.Store.Rebind("DELETE FROM anime WHERE id = ?"), from)
	return e
}
func (s *Server) libReassociate(w http.ResponseWriter, r *http.Request) {
	if !s.libAuthorized(w, r) {
		return
	}
	from, e := idParam(r, "sourceAnimeId")
	var b libReassociateRequest
	if e != nil || readJSON(r, &b) != nil || b.Target <= 0 {
		httpError(w, 422, "Invalid targetAnimeId")
		return
	}
	e = s.libTransaction(r.Context(), func(tx *sql.Tx) error {
		return s.libReassociateTx(r.Context(), tx, from, b.Target, b.Resolutions, strings.HasSuffix(r.URL.Path, "/resolve"))
	})
	if e != nil {
		libWriteError(w, e)
		return
	}
	w.WriteHeader(204)
}
func (s *Server) libSplitEpisodes(w http.ResponseWriter, r *http.Request) {
	if !s.libAuthorized(w, r) {
		return
	}
	id, e := idParam(r, "sourceId")
	if e != nil {
		httpError(w, 422, "Invalid source ID")
		return
	}
	src, e := s.Store.Get(r.Context(), "anime_sources", id)
	if e != nil || src == nil {
		httpError(w, 404, "Source not found")
		return
	}
	a, e := s.Store.Get(r.Context(), "anime", src["anime_id"])
	if e != nil {
		libWriteError(w, e)
		return
	}
	rows, e := s.libEpisodesQ(r.Context(), s.Store.DB, id)
	if e != nil {
		libWriteError(w, e)
		return
	}
	out := []store.Row{}
	for _, v := range rows {
		out = append(out, store.Row{"episodeId": v["id"], "title": v["title"], "episodeIndex": v["episode_index"], "commentCount": authInt(v["comment_count"])})
	}
	meta, e := s.authFind(r.Context(), "anime_metadata", store.Row{"anime_id": a["id"]})
	if e != nil && !errors.Is(e, sql.ErrNoRows) {
		libWriteError(w, e)
		return
	}
	info := store.Row{"sourceId": src["id"], "animeId": a["id"], "providerName": src["provider_name"], "mediaId": src["media_id"], "sourceOrder": src["source_order"], "year": a["year"], "title": a["title"], "type": a["type"], "season": a["season"], "imageUrl": a["image_url"], "tmdbId": meta["tmdb_id"], "bangumiId": meta["bangumi_id"], "airWeekday": meta["air_weekday"]}
	writeJSON(w, 200, store.Row{"episodes": out, "sourceInfo": info})
}
func libEpisodeID(anime, order, index int64) (int64, error) {
	if anime <= 0 || order <= 0 || index <= 0 {
		return 0, libErr(422, "Invalid episode identity components")
	}
	id, e := strconv.ParseInt(fmt.Sprintf("25%06d%02d%04d", anime, order, index), 10, 64)
	if e != nil {
		return 0, libErr(422, "Episode identity exceeds int64 range")
	}
	return id, nil
}
func (s *Server) libSplitSource(w http.ResponseWriter, r *http.Request) {
	if !s.libAuthorized(w, r) {
		return
	}
	anime, e := idParam(r, "animeId")
	var b struct {
		Source     int64     `json:"sourceId"`
		Episodes   []int64   `json:"episodeIds"`
		TargetType string    `json:"targetType"`
		New        store.Row `json:"newMediaInfo"`
		Existing   int64     `json:"existingMediaId"`
		Reindex    *bool     `json:"reindexEpisodes"`
	}
	if e != nil || readJSON(r, &b) != nil || len(b.Episodes) == 0 || len(b.Episodes) > 10000 {
		httpError(w, 422, "A source and episodeIds are required")
		return
	}
	reindex := b.Reindex == nil || *b.Reindex
	target := b.Existing
	moved := 0
	e = s.libTransaction(r.Context(), func(tx *sql.Tx) error {
		src, e := s.libOne(r.Context(), tx, "anime_sources", "id = ?", b.Source)
		if e != nil {
			return e
		}
		if authInt(src["anime_id"]) != anime {
			return libErr(400, "数据源不属于当前媒体")
		}
		if b.TargetType == "new" {
			if b.New == nil {
				return libErr(422, "newMediaInfo is required")
			}
			b.New["type"] = "tv_series"
			if e = libAnimeValidation(b.New, false, false); e != nil {
				return e
			}
			target, e = s.libCreateAnimeTx(r.Context(), tx, b.New, false)
			if e != nil {
				return e
			}
		} else if b.TargetType != "existing" {
			return libErr(422, "targetType must be new or existing")
		}
		if target == anime {
			return libErr(400, "Cannot split a source into the same anime")
		}
		if _, e = s.libOne(r.Context(), tx, "anime", "id = ?", target); e != nil {
			return e
		}
		dst, e := s.libOne(r.Context(), tx, "anime_sources", "anime_id = ? AND provider_name = ? AND media_id = ?", target, src["provider_name"], src["media_id"])
		if errors.Is(e, sql.ErrNoRows) {
			id, e := s.libAddSourceTx(r.Context(), tx, target, authString(src["provider_name"]), authString(src["media_id"]))
			if e != nil {
				return e
			}
			dst, e = s.libOne(r.Context(), tx, "anime_sources", "id = ?", id)
			if e != nil {
				return e
			}
		} else if e != nil {
			return e
		}
		all, e := s.libEpisodesQ(r.Context(), tx, b.Source)
		if e != nil {
			return e
		}
		selected := map[int64]bool{}
		for _, id := range b.Episodes {
			if selected[id] {
				return libErr(422, "Duplicate episode ID")
			}
			selected[id] = true
		}
		episodes := []store.Row{}
		for _, ep := range all {
			if selected[authInt(ep["id"])] {
				episodes = append(episodes, ep)
			}
		}
		if len(episodes) != len(selected) {
			return libErr(400, "Every selected episode must belong to the source")
		}
		var max int64
		if e = tx.QueryRowContext(r.Context(), s.Store.Rebind("SELECT COALESCE(MAX(episode_index),0) FROM episode WHERE source_id = ?"), dst["id"]).Scan(&max); e != nil {
			return e
		}
		for _, ep := range episodes {
			index := authInt(ep["episode_index"])
			if reindex {
				max++
				index = max
			}
			if _, e = s.libOne(r.Context(), tx, "episode", "source_id = ? AND episode_index = ?", dst["id"], index); e == nil {
				return libErr(409, "Target episode index already exists")
			} else if !errors.Is(e, sql.ErrNoRows) {
				return e
			}
			id, e := libEpisodeID(target, authInt(dst["source_order"]), index)
			if e != nil {
				return e
			}
			old := ep["id"]
			ep["id"] = id
			ep["source_id"] = dst["id"]
			ep["episode_index"] = index
			if _, e = tx.ExecContext(r.Context(), s.Store.Rebind("DELETE FROM episode WHERE id = ?"), old); e != nil {
				return e
			}
			if _, e = s.Store.InsertTx(r.Context(), tx, "episode", ep); e != nil {
				return e
			}
			moved++
		}
		return nil
	})
	if e != nil {
		libWriteError(w, e)
		return
	}
	writeJSON(w, 200, store.Row{"success": true, "targetMediaId": target, "movedEpisodeCount": moved, "message": fmt.Sprintf("成功拆分 %d 个分集", moved)})
}
func (s *Server) libBatchMerge(w http.ResponseWriter, r *http.Request) {
	if !s.libAuthorized(w, r) {
		return
	}
	var b struct {
		Operations []struct {
			Target  int64   `json:"targetAnimeId"`
			Sources []int64 `json:"sourceAnimeIds"`
		} `json:"operations"`
	}
	if readJSON(r, &b) != nil || len(b.Operations) > 1000 {
		httpError(w, 422, "Invalid operations")
		return
	}
	out := []store.Row{}
	success, failure := 0, 0
	for _, op := range b.Operations {
		for _, from := range op.Sources {
			e := s.libTransaction(r.Context(), func(tx *sql.Tx) error { return s.libReassociateTx(r.Context(), tx, from, op.Target, nil, false) })
			var msg any
			if e != nil {
				msg = e.Error()
				failure++
			} else {
				success++
			}
			out = append(out, store.Row{"targetAnimeId": op.Target, "success": e == nil, "error": msg})
		}
	}
	writeJSON(w, 200, store.Row{"results": out, "successCount": success, "failCount": failure})
}
func (s *Server) libDuplicates(w http.ResponseWriter, r *http.Request) {
	if !s.libAuthorized(w, r) {
		return
	}
	strict := r.URL.Query().Get("strict") != "false"
	rows, e := s.libRows(r.Context(), s.Store.DB, "anime_metadata", "SELECT * FROM anime_metadata WHERE tmdb_id IS NOT NULL AND tmdb_id <> ''")
	if e != nil {
		libWriteError(w, e)
		return
	}
	groups := map[string][]store.Row{}
	meta := map[string]store.Row{}
	for _, v := range rows {
		a, e := s.Store.Get(r.Context(), "anime", v["anime_id"])
		if e != nil || a == nil {
			continue
		}
		key := authString(v["tmdb_id"])
		if strict {
			key += "/" + authString(a["season"])
		}
		item, e := s.libAnimeInfo(r.Context(), a)
		if e != nil {
			libWriteError(w, e)
			return
		}
		for _, k := range []string{"type", "episodeCount", "createdAt", "groupId", "groupName", "sources"} {
			delete(item, k)
		}
		groups[key] = append(groups[key], item)
		var season any
		if strict {
			season = a["season"]
		}
		meta[key] = store.Row{"tmdbId": v["tmdb_id"], "season": season}
	}
	out := []store.Row{}
	total := 0
	for k, items := range groups {
		if len(items) < 2 {
			continue
		}
		v := meta[k]
		v["items"] = items
		out = append(out, v)
		total += len(items)
	}
	writeJSON(w, 200, store.Row{"groups": out, "totalGroups": len(out), "totalItems": total})
}
