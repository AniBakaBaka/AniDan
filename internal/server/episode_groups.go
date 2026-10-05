// SPDX-License-Identifier: AGPL-3.0-or-later
package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"unicode"

	"github.com/AniBakaBaka/AniDan/internal/integration"
	"github.com/AniBakaBaka/AniDan/internal/store"
)

var episodeGroupMu sync.Mutex

func (s *Server) registerEpisodeGroups(m *http.ServeMux) {
	m.HandleFunc("GET /api/control/episode-groups", s.operator(s.episodeGroupsList))
	m.HandleFunc("GET /api/control/episode-groups/{groupId}", s.operator(s.episodeGroupGet))
	m.HandleFunc("POST /api/control/episode-groups", s.operator(s.episodeGroupCreate))
	m.HandleFunc("PUT /api/control/episode-groups/{groupId}", s.operator(s.episodeGroupUpdate))
	m.HandleFunc("PUT /api/control/episode-groups/{groupId}/associate", s.operator(s.episodeGroupAssociate))
	m.HandleFunc("DELETE /api/control/episode-groups/{groupId}/associate/{animeId}", s.operator(s.episodeGroupDisassociate))
	m.HandleFunc("DELETE /api/control/episode-groups/{groupId}", s.operator(s.episodeGroupDelete))
	m.HandleFunc("POST /api/ui/local-episode-group/fetch", s.operator(s.localEpisodeGroupFetch))
	m.HandleFunc("POST /api/ui/local-episode-group/apply", s.operator(s.localEpisodeGroupApply))
	m.HandleFunc("GET /api/ui/local-episode-group/detail", s.operator(s.episodeGroupGet))
}
func groupError(status int, detail string) error {
	return &integration.Error{Provider: "episode-group", Status: status, Kind: detail}
}
func validateGroupID(id string) error {
	if id == "" || len([]rune(id)) > 500 || strings.ContainsAny(id, "/\\") {
		return groupError(422, "invalid group ID")
	}
	for _, r := range id {
		if unicode.IsControl(r) {
			return groupError(422, "invalid group ID")
		}
	}
	return nil
}
func (s *Server) episodeGroupRows(ctx context.Context, id string) ([]store.Row, error) {
	if e := validateGroupID(id); e != nil {
		return nil, e
	}
	rows, e := s.Store.List(ctx, "tmdb_episode_mapping", store.Row{"tmdb_episode_group_id": id}, 100000, 0)
	if e != nil {
		return nil, e
	}
	sort.SliceStable(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		if number(a["custom_season_number"]) != number(b["custom_season_number"]) {
			return number(a["custom_season_number"]) < number(b["custom_season_number"])
		}
		if number(a["custom_episode_number"]) != number(b["custom_episode_number"]) {
			return number(a["custom_episode_number"]) < number(b["custom_episode_number"])
		}
		return number(a["id"]) < number(b["id"])
	})
	return rows, nil
}
func (s *Server) episodeGroupDetail(ctx context.Context, id string) (map[string]any, error) {
	rows, e := s.episodeGroupRows(ctx, id)
	if e != nil {
		return nil, e
	}
	if len(rows) == 0 {
		return nil, groupError(404, "episode group not found")
	}
	groups := []map[string]any{}
	indexes := map[int64]int{}
	for _, row := range rows {
		season := number(row["custom_season_number"])
		idx, ok := indexes[season]
		if !ok {
			idx = len(groups)
			indexes[season] = idx
			name := fmt.Sprintf("第 %d 组", season)
			if season == 0 {
				name = "特别篇"
			}
			groups = append(groups, map[string]any{"name": name, "order": season, "episodes": []map[string]any{}})
		}
		eps := groups[idx]["episodes"].([]map[string]any)
		eps = append(eps, map[string]any{"seasonNumber": number(row["tmdb_season_number"]), "episodeNumber": number(row["tmdb_episode_number"]), "order": number(row["custom_episode_number"]) - 1, "name": str(row["episode_name"])})
		groups[idx]["episodes"] = eps
	}
	name := id
	if strings.HasPrefix(id, "local-") {
		name = "本地剧集组"
	}
	return map[string]any{"id": id, "tmdbTvId": number(rows[0]["tmdb_tv_id"]), "name": name, "description": "", "groups": groups}, nil
}
func (s *Server) episodeGroupsList(w http.ResponseWriter, r *http.Request) {
	q := s.Store.Quote
	ph := s.Store.Placeholder
	query := "SELECT " + q("tmdb_episode_group_id") + "," + q("tmdb_tv_id") + ",COUNT(*),COUNT(DISTINCT " + q("custom_season_number") + ") FROM " + q("tmdb_episode_mapping")
	args := []any{}
	if v := r.URL.Query().Get("tmdbTvId"); v != "" {
		id, e := strconv.ParseInt(v, 10, 64)
		if e != nil {
			httpError(w, 422, "Invalid tmdbTvId")
			return
		}
		query += " WHERE " + q("tmdb_tv_id") + "=" + ph(1)
		args = append(args, id)
	}
	query += " GROUP BY " + q("tmdb_episode_group_id") + "," + q("tmdb_tv_id") + " ORDER BY " + q("tmdb_tv_id") + "," + q("tmdb_episode_group_id")
	rows, e := s.Store.DB.QueryContext(r.Context(), query, args...)
	if e != nil {
		metadataError(w, e)
		return
	}
	out := []map[string]any{}
	for rows.Next() {
		var id string
		var tv, episodes, groups int64
		if e = rows.Scan(&id, &tv, &episodes, &groups); e != nil {
			rows.Close()
			metadataError(w, e)
			return
		}
		out = append(out, map[string]any{"groupId": id, "tmdbTvId": tv, "episodeCount": episodes, "groupCount": groups, "isLocal": strings.HasPrefix(id, "local-"), "associatedAnimeIds": []int64{}})
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		metadataError(w, e)
		return
	}
	assoc, e := s.Store.DB.QueryContext(r.Context(), "SELECT "+q("tmdb_episode_group_id")+","+q("anime_id")+" FROM "+q("anime_metadata")+" WHERE "+q("tmdb_episode_group_id")+" IS NOT NULL ORDER BY "+q("anime_id"))
	if e != nil {
		metadataError(w, e)
		return
	}
	links := map[string][]int64{}
	for assoc.Next() {
		var group string
		var id int64
		if e = assoc.Scan(&group, &id); e != nil {
			assoc.Close()
			metadataError(w, e)
			return
		}
		links[group] = append(links[group], id)
	}
	e = assoc.Err()
	assoc.Close()
	if e != nil {
		metadataError(w, e)
		return
	}
	for _, item := range out {
		if ids := links[str(item["groupId"])]; ids != nil {
			item["associatedAnimeIds"] = ids
		}
	}
	writeJSON(w, 200, out)
}
func (s *Server) episodeGroupGet(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("groupId")
	if id == "" {
		id = r.URL.Query().Get("groupId")
	}
	v, e := s.episodeGroupDetail(r.Context(), id)
	if e != nil {
		metadataError(w, e)
		return
	}
	writeJSON(w, 200, v)
}
func groupPayload(r *http.Request) (map[string]any, int64, error) {
	in := map[string]any{}
	if e := readJSON(r, &in); e != nil {
		return nil, 0, groupError(422, "invalid group payload")
	}
	raw, ok := in["tmdbTvId"]
	if !ok {
		return nil, 0, groupError(422, "tmdbTvId is required")
	}
	tv, e := strconv.ParseInt(str(raw), 10, 64)
	if e != nil || tv <= 0 || tv > 2147483647 {
		return nil, 0, groupError(422, "invalid tmdbTvId")
	}
	return in, tv, nil
}
func (s *Server) episodeGroupCreate(w http.ResponseWriter, r *http.Request) {
	in, tv, e := groupPayload(r)
	if e != nil {
		metadataError(w, e)
		return
	}
	id := str(in["groupId"])
	if id != "" && strings.HasPrefix(id, "local-") {
		httpError(w, 400, "groupId 不能以 'local-' 开头，本地剧集组请勿传入 groupId")
		return
	}
	if id == "" {
		id = fmt.Sprintf("local-%d", tv)
	}
	group, e := integration.DecodeEpisodeGroup(in, id, false)
	if e != nil {
		metadataError(w, e)
		return
	}
	var anime *int64
	if v, ok := in["animeId"]; ok && v != nil {
		n, e := strconv.ParseInt(str(v), 10, 64)
		if e != nil || n <= 0 {
			httpError(w, 422, "Invalid animeId")
			return
		}
		anime = &n
	}
	if e = s.episodeGroupSave(r.Context(), tv, group, "create", anime); e != nil {
		metadataError(w, e)
		return
	}
	writeJSON(w, 201, map[string]any{"message": fmt.Sprintf("剧集组 %s 创建成功", id), "groupId": id, "episodeCount": group.EpisodeCount})
}
func (s *Server) episodeGroupUpdate(w http.ResponseWriter, r *http.Request) {
	in, tv, e := groupPayload(r)
	if e != nil {
		metadataError(w, e)
		return
	}
	group, e := integration.DecodeEpisodeGroup(in, r.PathValue("groupId"), false)
	if e != nil {
		metadataError(w, e)
		return
	}
	if e = s.episodeGroupSave(r.Context(), tv, group, "update", nil); e != nil {
		metadataError(w, e)
		return
	}
	writeJSON(w, 200, map[string]any{"message": fmt.Sprintf("剧集组 %s 更新成功", group.ID), "episodeCount": group.EpisodeCount})
}
func (s *Server) episodeGroupSave(ctx context.Context, tv int64, group *integration.EpisodeGroup, mode string, anime *int64) error {
	if tv <= 0 || tv > 2147483647 {
		return groupError(422, "invalid TMDB TV ID")
	}
	if group == nil {
		return groupError(422, "group is required")
	}
	if e := validateGroupID(group.ID); e != nil {
		return e
	}
	episodeGroupMu.Lock()
	defer episodeGroupMu.Unlock()
	tx, e := s.Store.DB.BeginTx(ctx, nil)
	if e != nil {
		return e
	}
	defer tx.Rollback()
	q, ph := s.Store.Quote, s.Store.Placeholder
	var count int64
	if e = tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+q("tmdb_episode_mapping")+" WHERE "+q("tmdb_episode_group_id")+"="+ph(1), group.ID).Scan(&count); e != nil {
		return e
	}
	if mode == "create" && count > 0 {
		return groupError(409, "episode group already exists")
	}
	if mode == "update" && count == 0 {
		return groupError(404, "episode group not found")
	}
	if anime != nil {
		if e = s.episodeGroupCheckAnimeTx(ctx, tx, *anime); e != nil {
			return e
		}
	}
	if _, e = tx.ExecContext(ctx, "DELETE FROM "+q("tmdb_episode_mapping")+" WHERE "+q("tmdb_episode_group_id")+"="+ph(1), group.ID); e != nil {
		return e
	}
	groups := append([]integration.EpisodeGroupSection(nil), group.Groups...)
	sort.SliceStable(groups, func(i, j int) bool { return groups[i].Order < groups[j].Order })
	for _, section := range groups {
		for index, ep := range section.Episodes {
			_, e = s.Store.InsertTx(ctx, tx, "tmdb_episode_mapping", store.Row{"tmdb_tv_id": tv, "tmdb_episode_group_id": group.ID, "tmdb_episode_id": ep.ID, "tmdb_season_number": ep.SeasonNumber, "tmdb_episode_number": ep.EpisodeNumber, "custom_season_number": section.Order, "custom_episode_number": index + 1, "absolute_episode_number": ep.EpisodeNumber, "episode_name": ep.Name})
			if e != nil {
				return e
			}
		}
	}
	if anime != nil {
		if e = s.episodeGroupLinkTx(ctx, tx, *anime, group.ID); e != nil {
			return e
		}
	}
	return tx.Commit()
}
func (s *Server) episodeGroupCheckAnimeTx(ctx context.Context, tx *sql.Tx, id int64) error {
	var n int
	if e := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+s.Store.Quote("anime")+" WHERE "+s.Store.Quote("id")+"="+s.Store.Placeholder(1), id).Scan(&n); e != nil {
		return e
	}
	if n == 0 {
		return groupError(404, "anime not found")
	}
	return nil
}
func (s *Server) episodeGroupLinkTx(ctx context.Context, tx *sql.Tx, id int64, groupID string) error {
	q, ph := s.Store.Quote, s.Store.Placeholder
	var n int
	if e := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+q("anime_metadata")+" WHERE "+q("anime_id")+"="+ph(1), id).Scan(&n); e != nil {
		return e
	}
	if n == 0 {
		_, e := s.Store.InsertTx(ctx, tx, "anime_metadata", store.Row{"anime_id": id, "tmdb_episode_group_id": groupID})
		return e
	}
	_, e := tx.ExecContext(ctx, "UPDATE "+q("anime_metadata")+" SET "+q("tmdb_episode_group_id")+"="+ph(1)+" WHERE "+q("anime_id")+"="+ph(2), groupID, id)
	return e
}
func (s *Server) episodeGroupAssociate(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("groupId")
	if e := validateGroupID(id); e != nil {
		metadataError(w, e)
		return
	}
	var in struct {
		AnimeID int64 `json:"animeId"`
	}
	if readJSON(r, &in) != nil || in.AnimeID <= 0 {
		httpError(w, 422, "animeId is required")
		return
	}
	episodeGroupMu.Lock()
	defer episodeGroupMu.Unlock()
	tx, e := s.Store.DB.BeginTx(r.Context(), nil)
	if e != nil {
		metadataError(w, e)
		return
	}
	defer tx.Rollback()
	var n int
	e = tx.QueryRowContext(r.Context(), "SELECT COUNT(*) FROM "+s.Store.Quote("tmdb_episode_mapping")+" WHERE "+s.Store.Quote("tmdb_episode_group_id")+"="+s.Store.Placeholder(1), id).Scan(&n)
	if e == nil && n == 0 {
		e = groupError(404, "episode group not found")
	}
	if e == nil {
		e = s.episodeGroupCheckAnimeTx(r.Context(), tx, in.AnimeID)
	}
	if e == nil {
		e = s.episodeGroupLinkTx(r.Context(), tx, in.AnimeID, id)
	}
	if e == nil {
		e = tx.Commit()
	}
	if e != nil {
		metadataError(w, e)
		return
	}
	writeJSON(w, 200, map[string]any{"message": fmt.Sprintf("条目 %d 已关联到剧集组 %s", in.AnimeID, id)})
}
func (s *Server) episodeGroupDisassociate(w http.ResponseWriter, r *http.Request) {
	id, e := strconv.ParseInt(r.PathValue("animeId"), 10, 64)
	if e != nil || id <= 0 {
		httpError(w, 422, "Invalid animeId")
		return
	}
	episodeGroupMu.Lock()
	defer episodeGroupMu.Unlock()
	tx, e := s.Store.DB.BeginTx(r.Context(), nil)
	if e != nil {
		metadataError(w, e)
		return
	}
	defer tx.Rollback()
	if e = s.episodeGroupCheckAnimeTx(r.Context(), tx, id); e != nil {
		metadataError(w, e)
		return
	}
	if e = s.episodeGroupLinkTx(r.Context(), tx, id, ""); e != nil {
		metadataError(w, e)
		return
	}
	if e = tx.Commit(); e != nil {
		metadataError(w, e)
		return
	}
	writeJSON(w, 200, map[string]any{"message": fmt.Sprintf("条目 %d 已解除与剧集组 %s 的关联", id, r.PathValue("groupId"))})
}
func (s *Server) episodeGroupDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("groupId")
	if e := validateGroupID(id); e != nil {
		metadataError(w, e)
		return
	}
	episodeGroupMu.Lock()
	defer episodeGroupMu.Unlock()
	res, e := s.Store.DB.ExecContext(r.Context(), "DELETE FROM "+s.Store.Quote("tmdb_episode_mapping")+" WHERE "+s.Store.Quote("tmdb_episode_group_id")+"="+s.Store.Placeholder(1), id)
	if e != nil {
		metadataError(w, e)
		return
	}
	n, e := res.RowsAffected()
	if e != nil {
		metadataError(w, e)
		return
	}
	if n == 0 {
		httpError(w, 404, "episode group not found")
		return
	}
	writeJSON(w, 200, map[string]any{"message": fmt.Sprintf("剧集组 %s 已删除，共移除 %d 条映射记录", id, n)})
}
func (s *Server) localEpisodeGroupFetch(w http.ResponseWriter, r *http.Request) {
	var in struct {
		URL string `json:"url"`
	}
	if readJSON(r, &in) != nil || strings.TrimSpace(in.URL) == "" {
		httpError(w, 422, "url is required")
		return
	}
	source := strings.TrimSpace(in.URL)
	var data map[string]any
	var e error
	if strings.HasPrefix(source, "http://") || strings.HasPrefix(source, "https://") {
		data, e = s.Metadata.FetchEpisodeGroupURL(r.Context(), source)
	} else {
		if !strings.EqualFold(filepath.Ext(source), ".json") {
			httpError(w, 400, "仅支持 .json 文件")
			return
		}
		var f *os.File
		f, e = s.localOpenAllowed(source)
		if e != nil {
			if errors.Is(e, os.ErrNotExist) {
				httpError(w, 404, "File not found")
			} else {
				httpError(w, 403, "File is outside readable roots or inaccessible")
			}
			return
		}
		defer f.Close()
		var b []byte
		b, e = io.ReadAll(io.LimitReader(f, (4<<20)+1))
		if e == nil && len(b) > 4<<20 {
			e = groupError(400, "group document exceeds 4 MiB")
		}
		if e == nil {
			d := json.NewDecoder(strings.NewReader(string(b)))
			d.UseNumber()
			e = d.Decode(&data)
			if e == nil {
				var tail any
				if d.Decode(&tail) != io.EOF {
					e = groupError(422, "trailing JSON")
				}
			}
		}
	}
	if e != nil {
		metadataError(w, e)
		return
	}
	if _, ok := data["groups"].([]any); !ok {
		httpError(w, 422, "JSON格式不正确，缺少 groups 字段")
		return
	}
	writeJSON(w, 200, data)
}
func (s *Server) localEpisodeGroupApply(w http.ResponseWriter, r *http.Request) {
	var in struct {
		TMDBID int64          `json:"tmdbId"`
		Data   map[string]any `json:"localEpisodeGroup"`
	}
	if readJSON(r, &in) != nil || in.TMDBID <= 0 {
		httpError(w, 422, "tmdbId and localEpisodeGroup are required")
		return
	}
	id := fmt.Sprintf("local-%d", in.TMDBID)
	group, e := integration.DecodeEpisodeGroup(in.Data, id, true)
	if e != nil {
		metadataError(w, e)
		return
	}
	if e = s.episodeGroupSave(r.Context(), in.TMDBID, group, "upsert", nil); e != nil {
		metadataError(w, e)
		return
	}
	writeJSON(w, 200, map[string]any{"message": "本地剧集组映射更新成功", "groupId": id, "episodeCount": group.EpisodeCount})
}

// EpisodeEquivalence preserves custom-first lookup semantics and returns the
// original absolute episode number for callers performing player/import mapping.
func (s *Server) EpisodeEquivalence(ctx context.Context, groupID string, season, episode int64) (map[string]any, error) {
	rows, e := s.episodeGroupRows(ctx, groupID)
	if e != nil {
		return nil, e
	}
	for _, direction := range []string{"custom_to_tmdb", "tmdb_to_custom"} {
		for _, row := range rows {
			hit := number(row["custom_season_number"]) == season && number(row["custom_episode_number"]) == episode
			if direction == "tmdb_to_custom" {
				hit = number(row["tmdb_season_number"]) == season && number(row["tmdb_episode_number"]) == episode
			}
			if !hit {
				continue
			}
			count := 0
			for _, r := range rows {
				if number(r["custom_season_number"]) == number(row["custom_season_number"]) {
					count++
				}
			}
			return map[string]any{"custom_season": number(row["custom_season_number"]), "custom_episode": number(row["custom_episode_number"]), "tmdb_season": number(row["tmdb_season_number"]), "tmdb_episode": number(row["tmdb_episode_number"]), "absolute_episode": number(row["absolute_episode_number"]), "season_total_episodes": count, "episode_name": str(row["episode_name"]), "match_direction": direction}, nil
		}
	}
	return nil, nil
}

func (s *Server) EpisodeEquivalenceBatch(ctx context.Context, groupID string, season int64, episodes []int64) (map[int64]int64, error) {
	if len(episodes) > 100000 {
		return nil, groupError(422, "too many requested episodes")
	}
	wanted := map[int64]bool{}
	for _, n := range episodes {
		wanted[n] = true
	}
	out := map[int64]int64{}
	if len(wanted) == 0 {
		return out, nil
	}
	rows, e := s.episodeGroupRows(ctx, groupID)
	if e != nil {
		return nil, e
	}
	for _, row := range rows {
		n := number(row["custom_episode_number"])
		if number(row["custom_season_number"]) == season && wanted[n] {
			out[n] = number(row["absolute_episode_number"])
		}
	}
	return out, nil
}
func (s *Server) EpisodeGroupIDForAnime(ctx context.Context, id int64) (string, error) {
	rows, e := s.Store.List(ctx, "anime_metadata", store.Row{"anime_id": id}, 1, 0)
	if e != nil {
		return "", e
	}
	if len(rows) == 0 {
		return "", nil
	}
	return str(rows[0]["tmdb_episode_group_id"]), nil
}
