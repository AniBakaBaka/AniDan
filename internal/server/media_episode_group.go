// SPDX-License-Identifier: AGPL-3.0-only
package server

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/AniBakaBaka/AniDan/internal/store"
)

const mediaEpisodeGroupRowLimit = 100000

// This acquisition-only snapshot does not change the public scalar helper's
// custom-first/inverse behavior or the batch helper's stored absolute numbers.
// A lookup here is exclusively custom -> official/provider, with no inverse.
type mediaEpisodeGroupSnapshot struct {
	groupID          string
	customSeason     int
	fingerprint      string
	animeID          int64
	tvID             int64
	tmdbID           string
	targets          map[int]mediaEpisodeGroupTarget
	rawSeasons       map[int]int
	animeIDs         []int64
	metadataIDs      []int64
	metadataAnimeIDs []int64
}

type mediaEpisodeGroupTarget struct {
	officialSeason  int
	officialEpisode int
	providerEpisode int
}

func mediaEpisodeGroupIdentity(detail string) error {
	return fmt.Errorf("%w: episode group %s", errMediaAcquisitionIdentity, detail)
}

func (g *mediaEpisodeGroupSnapshot) lookup(customEpisode int) (mediaEpisodeGroupTarget, bool, error) {
	if g == nil {
		return mediaEpisodeGroupTarget{}, false, nil
	}
	target, ok := g.targets[customEpisode]
	if !ok {
		return mediaEpisodeGroupTarget{}, false, nil
	}
	// A raw number cannot distinguish official seasons. Acquisition currently
	// has no independently verified numbering convention for mixed seasons.
	if g.rawSeasons[target.providerEpisode] < 0 {
		return mediaEpisodeGroupTarget{}, false, mediaEpisodeGroupIdentity("provider number spans multiple official seasons")
	}
	return target, true, nil
}

// mediaEpisodeGroupRows deliberately does not use the existing CRUD List limit:
// one extra row proves that a snapshot is complete rather than truncated. All
// locking reads use the caller's transaction and lock current rows on SQL backends.
func (s *Server) mediaEpisodeGroupRows(ctx context.Context, q libQueryer, table, where string, args ...any) ([]store.Row, error) {
	return s.mediaEpisodeGroupRowsMode(ctx, q, true, table, where, args...)
}

func (s *Server) mediaEpisodeGroupRowsMode(ctx context.Context, q libQueryer, lock bool, table, where string, args ...any) ([]store.Row, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var columns []string
	switch table {
	case "anime_metadata":
		columns = []string{"id", "anime_id", "tmdb_id", "tmdb_episode_group_id"}
	case "tmdb_episode_mapping":
		columns = []string{"id", "tmdb_tv_id", "tmdb_episode_group_id", "tmdb_episode_id", "tmdb_season_number", "tmdb_episode_number", "custom_season_number", "custom_episode_number", "absolute_episode_number"}
	case "anime":
		columns = []string{"id", "type"}
	default:
		return nil, mediaEpisodeGroupIdentity("snapshot table is invalid")
	}
	for i := range columns {
		columns[i] = s.Store.Quote(columns[i])
	}
	query := "SELECT " + strings.Join(columns, ",") + " FROM " + s.Store.Quote(table) + " WHERE " + where + " ORDER BY " + s.Store.Quote("id") + " LIMIT 100001"
	if _, transactional := q.(*sql.Tx); lock && transactional && s.Store.Dialect != "sqlite" {
		query += " FOR UPDATE"
	}
	rows, err := q.QueryContext(ctx, s.Store.Rebind(query), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []store.Row{}
	for rows.Next() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if len(out) == mediaEpisodeGroupRowLimit {
			return nil, mediaEpisodeGroupIdentity("snapshot exceeds 100000 rows")
		}
		row, err := store.ScanRow(rows, store.Schema[table])
		if err != nil {
			if canceled := ctx.Err(); canceled != nil {
				return nil, canceled
			}
			return nil, fmt.Errorf("%w: episode group snapshot row is invalid: %v", errMediaAcquisitionIdentity, err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, ctx.Err()
}

func (s *Server) loadMediaEpisodeGroup(ctx context.Context, anime store.Row, tmdbID string, customSeason int) (*mediaEpisodeGroupSnapshot, error) {
	return s.loadMediaEpisodeGroupQ(ctx, s.Store.DB, anime, tmdbID, customSeason, nil, nil)
}

func (s *Server) loadMediaEpisodeGroupQ(ctx context.Context, q libQueryer, anime store.Row, tmdbID string, customSeason int, expected *mediaEpisodeGroupSnapshot, lockedMappings []store.Row) (*mediaEpisodeGroupSnapshot, error) {
	return s.loadMediaEpisodeGroupLockedQ(ctx, q, anime, tmdbID, customSeason, expected, lockedMappings, nil, nil)
}

// Capture reads do not lock association rows. Revalidation supplies every anime
// row already locked in ID order; only metadata may be locked during this pass.
func (s *Server) loadMediaEpisodeGroupLockedQ(ctx context.Context, q libQueryer, anime store.Row, tmdbID string, customSeason int, expected *mediaEpisodeGroupSnapshot, lockedMappings []store.Row, lockedAnime map[int64]store.Row, lockedMetadata map[int64]bool) (*mediaEpisodeGroupSnapshot, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if customSeason < 0 {
		return nil, mediaEpisodeGroupIdentity("custom season is invalid")
	}
	g := &mediaEpisodeGroupSnapshot{customSeason: customSeason, tmdbID: tmdbID, targets: map[int]mediaEpisodeGroupTarget{}, rawSeasons: map[int]int{}}
	readRows := func(table, where string, args ...any) ([]store.Row, error) {
		rows, err := s.mediaEpisodeGroupRowsMode(ctx, q, lockedAnime != nil && table == "anime_metadata", table, where, args...)
		if err != nil {
			return nil, err
		}
		if table == "anime_metadata" {
			for _, row := range rows {
				id, animeID := number(row["id"]), number(row["anime_id"])
				if id <= 0 || animeID <= 0 {
					return nil, mediaEpisodeGroupIdentity("association row identity is invalid")
				}
				if lockedAnime != nil && (!lockedMetadata[id] || lockedAnime[animeID] == nil) {
					return nil, mediaEpisodeGroupIdentity("associated TV identities changed during locking; retry required")
				}
				g.metadataIDs = append(g.metadataIDs, id)
				g.metadataAnimeIDs = append(g.metadataAnimeIDs, animeID)
			}
		}
		return rows, nil
	}
	var targetAnime, targetMetadata store.Row
	if anime != nil {
		id, err := strconv.ParseInt(str(anime["id"]), 10, 64)
		if err != nil || id <= 0 {
			return nil, mediaEpisodeGroupIdentity("target anime identity is invalid")
		}
		g.animeID = id
		var current []store.Row
		if lockedAnime != nil {
			if row := lockedAnime[id]; row != nil {
				current = []store.Row{row}
			}
		} else {
			current, err = readRows("anime", "id=?", id)
			if err != nil {
				return nil, err
			}
		}
		if len(current) != 1 || str(current[0]["type"]) != "tv_series" {
			return nil, mediaEpisodeGroupIdentity("target anime is missing or is not an exact TV identity")
		}
		targetAnime = current[0]
		metadata, err := readRows("anime_metadata", "anime_id=?", id)
		if err != nil {
			return nil, err
		}
		if len(metadata) > 1 {
			return nil, mediaEpisodeGroupIdentity("target association is ambiguous")
		}
		if len(metadata) == 1 {
			targetMetadata = metadata[0]
			g.groupID = str(targetMetadata["tmdb_episode_group_id"])
		}
	}
	knownTMDB := tmdbID
	if knownTMDB == "" {
		knownTMDB = str(targetMetadata["tmdb_id"])
	}
	var fallbackAssociations []store.Row
	// TMDB movie and television numbers have different namespaces. Restrict
	// fallback to existing TV associations, then inspect their stored types
	// exactly below because SQL collation may broaden the type comparison.
	tvAssociation := " EXISTS (SELECT 1 FROM " + s.Store.Quote("anime") + " WHERE " + s.Store.Quote("anime") + "." + s.Store.Quote("id") + "=" + s.Store.Quote("anime_metadata") + "." + s.Store.Quote("anime_id") + " AND " + s.Store.Quote("type") + "='tv_series')"
	if g.groupID == "" {
		if knownTMDB == "" {
			return nil, nil
		}
		rows, err := readRows("anime_metadata", "tmdb_id=? AND tmdb_episode_group_id IS NOT NULL AND"+tvAssociation, knownTMDB)
		if err != nil {
			return nil, err
		}
		for _, row := range rows {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if str(row["tmdb_id"]) != knownTMDB {
				return nil, mediaEpisodeGroupIdentity("TMDB association is not an exact match")
			}
			id := str(row["tmdb_episode_group_id"])
			if id == "" {
				continue
			}
			if g.groupID != "" && g.groupID != id {
				return nil, mediaEpisodeGroupIdentity("has multiple associated groups for the known TMDB identity")
			}
			g.groupID = id
			fallbackAssociations = append(fallbackAssociations, row)
		}
		if g.groupID == "" {
			return nil, nil
		}
	}
	if err := validateGroupID(g.groupID); err != nil {
		return nil, mediaEpisodeGroupIdentity("association has an invalid group ID")
	}
	if expected != nil && g.groupID != expected.groupID {
		return nil, mediaEpisodeGroupIdentity("association changed; committed pools require review")
	}
	rows := lockedMappings
	if expected == nil {
		var err error
		rows, err = readRows("tmdb_episode_mapping", "tmdb_episode_group_id=?", g.groupID)
		if err != nil {
			return nil, err
		}
	}
	if len(rows) == 0 {
		return nil, mediaEpisodeGroupIdentity("association has no mapping rows")
	}
	tvID := number(rows[0]["tmdb_tv_id"])
	if tvID <= 0 {
		return nil, mediaEpisodeGroupIdentity("has an invalid TMDB TV identity")
	}
	g.tvID = tvID
	tvText := strconv.FormatInt(tvID, 10)
	for _, known := range []string{tmdbID, str(targetMetadata["tmdb_id"])} {
		if known != "" && known != tvText {
			return nil, mediaEpisodeGroupIdentity("conflicts with a known TMDB identity")
		}
	}
	customCoordinates, officialCoordinates := map[[2]int]bool{}, map[[2]int]bool{}
	for _, row := range rows {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if str(row["tmdb_episode_group_id"]) != g.groupID {
			return nil, mediaEpisodeGroupIdentity("mapping group ID is not an exact match")
		}
		if number(row["tmdb_tv_id"]) != tvID {
			return nil, mediaEpisodeGroupIdentity("mapping rows have conflicting TMDB TV identities")
		}
		if number(row["id"]) <= 0 || number(row["tmdb_episode_id"]) <= 0 {
			return nil, mediaEpisodeGroupIdentity("mapping row identity is invalid")
		}
		custom := [2]int{int(number(row["custom_season_number"])), int(number(row["custom_episode_number"]))}
		official := [2]int{int(number(row["tmdb_season_number"])), int(number(row["tmdb_episode_number"]))}
		providerEpisode := int(number(row["absolute_episode_number"]))
		if custom[0] < 0 || custom[1] < 0 || official[0] < 0 || official[1] < 0 || providerEpisode < 0 || providerEpisode != official[1] {
			return nil, mediaEpisodeGroupIdentity("mapping coordinates are invalid or inconsistent")
		}
		if customCoordinates[custom] || officialCoordinates[official] {
			return nil, mediaEpisodeGroupIdentity("mapping coordinates are ambiguous")
		}
		customCoordinates[custom], officialCoordinates[official] = true, true
		if prior, exists := g.rawSeasons[providerEpisode]; exists && prior != official[0] {
			g.rawSeasons[providerEpisode] = -1
		} else {
			g.rawSeasons[providerEpisode] = official[0]
		}
		if custom[0] == customSeason {
			g.targets[custom[1]] = mediaEpisodeGroupTarget{officialSeason: official[0], officialEpisode: official[1], providerEpisode: providerEpisode}
		}
	}
	associations, err := readRows("anime_metadata", "tmdb_episode_group_id=? AND"+tvAssociation, g.groupID)
	if err != nil {
		return nil, err
	}
	for _, row := range associations {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if str(row["tmdb_episode_group_id"]) != g.groupID {
			return nil, mediaEpisodeGroupIdentity("association group ID is not an exact match")
		}
		if known := str(row["tmdb_id"]); known != "" && known != tvText {
			return nil, mediaEpisodeGroupIdentity("associated anime has a conflicting known TMDB identity")
		}
	}
	var associatedAnime []store.Row
	if lockedAnime != nil {
		seen := map[int64]bool{}
		for _, row := range associations {
			id := number(row["anime_id"])
			if lockedAnime[id] == nil {
				return nil, mediaEpisodeGroupIdentity("associated TV identities changed during locking; retry required")
			}
			if !seen[id] {
				associatedAnime = append(associatedAnime, lockedAnime[id])
				seen[id] = true
			}
		}
		sort.Slice(associatedAnime, func(i, j int) bool { return number(associatedAnime[i]["id"]) < number(associatedAnime[j]["id"]) })
	} else {
		associatedAnime, err = readRows("anime", s.Store.Quote("type")+"='tv_series' AND id IN (SELECT anime_id FROM "+s.Store.Quote("anime_metadata")+" WHERE tmdb_episode_group_id=?)", g.groupID)
		if err != nil {
			return nil, err
		}
	}
	if len(associatedAnime) != len(associations) {
		return nil, mediaEpisodeGroupIdentity("associated TV identities changed during capture")
	}
	for _, row := range associatedAnime {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if str(row["type"]) != "tv_series" {
			return nil, mediaEpisodeGroupIdentity("associated anime type is not an exact TV identity")
		}
	}
	if g.animeID > 0 {
		g.animeIDs = append(g.animeIDs, g.animeID)
	}
	for _, row := range associatedAnime {
		id := number(row["id"])
		if id <= 0 {
			return nil, mediaEpisodeGroupIdentity("associated anime identity is invalid")
		}
		if id != g.animeID {
			g.animeIDs = append(g.animeIDs, id)
		}
	}
	sort.Slice(g.animeIDs, func(i, j int) bool { return g.animeIDs[i] < g.animeIDs[j] })
	// Encode each bounded row directly into the digest. This avoids duplicating
	// a large group as a single JSON byte slice. JSON preserves exact int64 IDs;
	// named sections/counts delimit the sorted evidence without concatenation
	// ambiguities, and cancellation is checked between rows.
	hash := sha256.New()
	encoder := json.NewEncoder(hash)
	if err := encoder.Encode(struct {
		GroupID        string
		CustomSeason   int
		AnimeID        int64
		TMDBID         string
		TargetAnime    store.Row
		TargetMetadata store.Row
	}{g.groupID, g.customSeason, g.animeID, g.tmdbID, targetAnime, targetMetadata}); err != nil {
		return nil, err
	}
	for _, section := range []struct {
		name string
		rows []store.Row
	}{{"fallback-associations", fallbackAssociations}, {"group-associations", associations}, {"associated-anime", associatedAnime}, {"mappings", rows}} {
		if err := encoder.Encode(struct {
			Name  string
			Count int
		}{section.name, len(section.rows)}); err != nil {
			return nil, err
		}
		for _, row := range section.rows {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if err := encoder.Encode(row); err != nil {
				return nil, err
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	g.fingerprint = fmt.Sprintf("%x", hash.Sum(nil))
	return g, nil
}

func (s *Server) validateMediaEpisodeGroup(ctx context.Context, q libQueryer, expected *mediaEpisodeGroupSnapshot) error {
	return s.validateMediaEpisodeGroupForTarget(ctx, q, expected, 0)
}

func (s *Server) validateMediaEpisodeGroupForTarget(ctx context.Context, q libQueryer, expected *mediaEpisodeGroupSnapshot, targetAnimeID int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if expected == nil {
		return nil
	}
	if targetAnimeID < 0 {
		return mediaEpisodeGroupIdentity("target anime identity is invalid")
	}
	var anime store.Row
	if expected.animeID > 0 {
		anime = store.Row{"id": expected.animeID}
	}
	// Group CRUD replaces mappings first. Anime edits lock anime before their
	// metadata, so all three phases must use mappings -> anime -> metadata.
	mappings, err := s.mediaEpisodeGroupRows(ctx, q, "tmdb_episode_mapping", "tmdb_episode_group_id=?", expected.groupID)
	if err != nil {
		return err
	}
	current, err := s.loadMediaEpisodeGroupQ(ctx, q, anime, expected.tmdbID, expected.customSeason, expected, mappings)
	if err != nil {
		return err
	}
	if current == nil || current.fingerprint != expected.fingerprint {
		return mediaEpisodeGroupIdentity("mapping or association changed; committed pools require review")
	}
	// The discovery pass also records skipped empty fallback rows: these rows
	// participate in the SQL predicate and must not introduce a later anime lock.
	metadataIDs := map[int64]bool{}
	for _, id := range current.metadataIDs {
		metadataIDs[id] = true
	}
	if targetAnimeID > 0 {
		metadata, err := s.mediaEpisodeGroupRowsMode(ctx, q, false, "anime_metadata", "anime_id=?", targetAnimeID)
		if err != nil {
			return err
		}
		for _, row := range metadata {
			metadataIDs[number(row["id"])] = true
		}
	}
	ids := map[int64]bool{}
	for _, captured := range [][]int64{expected.animeIDs, current.animeIDs, current.metadataAnimeIDs} {
		for _, id := range captured {
			ids[id] = true
		}
	}
	if targetAnimeID > 0 {
		ids[targetAnimeID] = true
	}
	if len(ids) > 2*mediaEpisodeGroupRowLimit+2 || len(metadataIDs) > 2*mediaEpisodeGroupRowLimit+2 {
		return mediaEpisodeGroupIdentity("anime lock set exceeds snapshot bounds")
	}
	ordered := make([]int64, 0, len(ids))
	for id := range ids {
		ordered = append(ordered, id)
	}
	sort.Slice(ordered, func(i, j int) bool { return ordered[i] < ordered[j] })
	lockedAnime := make(map[int64]store.Row, len(ordered))
	// Stay below backend parameter limits without changing the global ID order.
	const chunkSize = 500
	for start := 0; start < len(ordered); start += chunkSize {
		end := min(start+chunkSize, len(ordered))
		args := make([]any, end-start)
		for i, id := range ordered[start:end] {
			args[i] = id
		}
		where := "id IN (" + strings.TrimSuffix(strings.Repeat("?,", len(args)), ",") + ")"
		rows, err := s.mediaEpisodeGroupRows(ctx, q, "anime", where, args...)
		if err != nil {
			return err
		}
		if len(rows) != len(args) {
			return mediaEpisodeGroupIdentity("associated or target anime disappeared during locking")
		}
		for _, row := range rows {
			if number(row["id"]) == targetAnimeID && str(row["type"]) != "tv_series" {
				return mediaEpisodeGroupIdentity("target anime is not an exact TV identity")
			}
			lockedAnime[number(row["id"])] = row
		}
	}
	// Lock the discovered metadata IDs in the same global order. Each discovery
	// query still has its own 100000-row sentinel, so their union may be larger.
	ordered = ordered[:0]
	for id := range metadataIDs {
		ordered = append(ordered, id)
	}
	sort.Slice(ordered, func(i, j int) bool { return ordered[i] < ordered[j] })
	for start := 0; start < len(ordered); start += chunkSize {
		end := min(start+chunkSize, len(ordered))
		args := make([]any, end-start)
		for i, id := range ordered[start:end] {
			args[i] = id
		}
		where := "id IN (" + strings.TrimSuffix(strings.Repeat("?,", len(args)), ",") + ")"
		rows, err := s.mediaEpisodeGroupRows(ctx, q, "anime_metadata", where, args...)
		if err != nil {
			return err
		}
		if len(rows) != len(args) {
			return mediaEpisodeGroupIdentity("association disappeared during locking; retry required")
		}
		for _, row := range rows {
			if lockedAnime[number(row["anime_id"])] == nil {
				return mediaEpisodeGroupIdentity("associated TV identities changed during locking; retry required")
			}
		}
	}
	// Reread association predicates with current locking reads, including under
	// repeatable read. A changed set is refused; the anime lock set is never
	// expanded after metadata locking begins.
	current, err = s.loadMediaEpisodeGroupLockedQ(ctx, q, anime, expected.tmdbID, expected.customSeason, expected, mappings, lockedAnime, metadataIDs)
	if err != nil {
		return err
	}
	if current == nil || current.fingerprint != expected.fingerprint {
		return mediaEpisodeGroupIdentity("mapping or association changed; committed pools require review")
	}
	return nil
}
