// SPDX-License-Identifier: AGPL-3.0-only
package server

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/AniBakaBaka/AniDan/internal/integration"
	"github.com/AniBakaBaka/AniDan/internal/job"
	"github.com/AniBakaBaka/AniDan/internal/store"
)

const localPosterWarningLimit = 25

// These records deliberately contain neither provider errors nor source paths.
type localImportWarning struct {
	ItemID  int64  `json:"itemId"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

type localImportWarnings struct {
	records []localImportWarning
	count   int
}

func (w *localImportWarnings) add(v localImportWarning) {
	// Limit both stored records and the counter for arbitrarily large job input.
	if w.count < 1_000_000 {
		w.count++
	}
	if len(w.records) < localPosterWarningLimit {
		w.records = append(w.records, v)
	}
}
func (w *localImportWarnings) result(imported int, failedID int64) map[string]any {
	v := map[string]any{"imported": imported}
	if failedID != 0 {
		v["failedItemId"] = failedID
	}
	if w.count != 0 {
		v["warnings"] = w.records
		v["warningCount"] = w.count
		v["warningsTruncated"] = w.count > len(w.records)
	}
	return v
}

func localPosterWarning(id int64, code string) localImportWarning {
	messages := map[string]string{
		"tmdb_poster_identity_conflict": "TMDB poster skipped because the existing library identity conflicts with the local item.",
		"tmdb_poster_key_missing":       "TMDB poster skipped because an API key is not configured.",
		"tmdb_poster_route_unavailable": "TMDB poster skipped because its configured route is unavailable or unsupported.",
		"tmdb_poster_metadata_failed":   "TMDB poster skipped because metadata could not be retrieved.",
		"tmdb_poster_identity_mismatch": "TMDB poster skipped because the returned metadata identity did not match.",
		"tmdb_poster_missing":           "TMDB metadata did not include a poster.",
		"tmdb_poster_image_failed":      "TMDB poster skipped because the image could not be safely downloaded.",
	}
	return localImportWarning{ItemID: id, Code: code, Message: messages[code]}
}

// Keep the exact known identifier in requests and comparisons; never infer IDs
// from titles or turn malformed/negative identifiers into remote requests.
func localPositiveTMDBID(raw string) bool {
	if raw == "" || len(raw) > 19 {
		return false
	}
	for _, r := range raw {
		if r < '0' || r > '9' {
			return false
		}
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	return err == nil && n > 0
}

func localPosterDetailsMatch(details *integration.Metadata, id, typ string) bool {
	// TMDB's existing DTO omits Type for details; the explicit endpoint supplies
	// that type. A nonempty contradictory cached type is never accepted.
	return details != nil && details.ID == id && details.TMDBID == id && (details.Type == "" || details.Type == typ || (typ == "tv" && details.Type == "tv_series"))
}

type localImportTarget struct {
	ID             int64
	Title, Type    string
	Season, Year   sql.NullInt64
	TMDBID         sql.NullString
	TVDBID, IMDBID sql.NullString
	Poster, URL    sql.NullString
}

type localPosterQueryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

// New unbound items use an exact weak identity, including the presence of a
// year. Known identifiers can disambiguate compatible candidates, but cannot
// override a contradictory known identifier or select an arbitrary first row.
func (s *Server) localImportTarget(ctx context.Context, q localPosterQueryer, row store.Row, season int64, lock bool) (localImportTarget, error) {
	query := "SELECT id,title,type,season,year,local_image_path,image_url FROM anime WHERE title=? AND type=? AND season=? AND year"
	args := []any{row["title"], row["media_type"], season}
	if row["year"] == nil {
		query += " IS NULL"
	} else {
		query += "=?"
		args = append(args, row["year"])
	}
	query += " ORDER BY id LIMIT 65"
	suffix := ""
	if lock && s.Store.Dialect != "sqlite" {
		suffix = " FOR UPDATE"
	}
	rows, err := q.QueryContext(ctx, s.Store.Rebind(query+suffix), args...)
	if err != nil {
		return localImportTarget{}, err
	}
	candidates := []localImportTarget{}
	for rows.Next() {
		var t localImportTarget
		if err = rows.Scan(&t.ID, &t.Title, &t.Type, &t.Season, &t.Year, &t.Poster, &t.URL); err != nil {
			rows.Close()
			return localImportTarget{}, err
		}
		candidates = append(candidates, t)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return localImportTarget{}, err
	}
	if len(candidates) > 64 {
		return localImportTarget{}, errors.New("local library identity exceeds candidate limit; review required")
	}
	compatible, strong := []localImportTarget{}, []localImportTarget{}
	for _, t := range candidates {
		if err = ctx.Err(); err != nil {
			return localImportTarget{}, err
		}
		// Do not turn case/accent-insensitive SQL comparison into title guessing.
		if t.Title != str(row["title"]) || t.Type != str(row["media_type"]) {
			continue
		}
		if err = s.localImportTargetMetadata(ctx, q, &t, suffix); err != nil {
			return localImportTarget{}, err
		}
		if t.conflicts(row) {
			continue
		}
		compatible = append(compatible, t)
		matches := false
		for _, v := range [][2]string{{t.TMDBID.String, str(row["tmdb_id"])}, {t.TVDBID.String, str(row["tvdb_id"])}, {t.IMDBID.String, str(row["imdb_id"])}} {
			if v[0] != "" && v[0] == v[1] {
				matches = true
			}
		}
		if matches {
			strong = append(strong, t)
		}
	}
	if len(strong) > 0 {
		compatible = strong
	}
	if len(compatible) > 1 {
		return localImportTarget{}, errors.New("local library identity is ambiguous; review required")
	}
	if len(compatible) == 1 {
		return compatible[0], nil
	}
	return localImportTarget{}, nil
}
func (s *Server) localImportTargetMetadata(ctx context.Context, q localPosterQueryer, t *localImportTarget, suffix string) error {
	err := q.QueryRowContext(ctx, s.Store.Rebind("SELECT tmdb_id,tvdb_id,imdb_id FROM anime_metadata WHERE anime_id=?"+suffix), t.ID).Scan(&t.TMDBID, &t.TVDBID, &t.IMDBID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	return err
}
func (s *Server) localImportTargetByID(ctx context.Context, q localPosterQueryer, id int64, lock bool) (localImportTarget, error) {
	var t localImportTarget
	suffix := ""
	if lock && s.Store.Dialect != "sqlite" {
		suffix = " FOR UPDATE"
	}
	err := q.QueryRowContext(ctx, s.Store.Rebind("SELECT id,title,type,season,year,local_image_path,image_url FROM anime WHERE id=?"+suffix), id).Scan(&t.ID, &t.Title, &t.Type, &t.Season, &t.Year, &t.Poster, &t.URL)
	if err != nil {
		return t, err
	}
	err = s.localImportTargetMetadata(ctx, q, &t, suffix)
	return t, err
}
func (t localImportTarget) hasPoster() bool { return t.Poster.String != "" || t.URL.String != "" }
func (t localImportTarget) sameIdentity(other localImportTarget) bool {
	return t.ID == other.ID && t.Title == other.Title && t.Type == other.Type && t.Season == other.Season && t.Year == other.Year && t.TMDBID == other.TMDBID && t.TVDBID == other.TVDBID && t.IMDBID == other.IMDBID
}
func (t localImportTarget) conflicts(row store.Row) bool {
	if t.ID == 0 {
		return false
	}
	if (row["year"] == nil) != (!t.Year.Valid) || t.Year.Valid && t.Year.Int64 != number(row["year"]) {
		return true
	}
	for _, v := range [][2]string{{t.TMDBID.String, str(row["tmdb_id"])}, {t.TVDBID.String, str(row["tvdb_id"])}, {t.IMDBID.String, str(row["imdb_id"])}} {
		if v[0] != "" && v[1] != "" && v[0] != v[1] {
			return true
		}
	}
	return false
}

// Re-read and lock the selected record before any imported flag or linkage can
// be written. Compare semantic input identity; import status and update-time
// bookkeeping alone must not invalidate an otherwise unchanged selection.
func (s *Server) localRecheckImportRow(ctx context.Context, tx *sql.Tx, expected store.Row) error {
	query := "SELECT * FROM local_danmaku_items WHERE id=?"
	if s.Store.Dialect != "sqlite" {
		query += " FOR UPDATE"
	}
	rows, err := tx.QueryContext(ctx, s.Store.Rebind(query), expected["id"])
	if err != nil {
		return err
	}
	defer rows.Close()
	if !rows.Next() {
		if err = rows.Err(); err != nil {
			return err
		}
		return errors.New("local import item was deleted or changed")
	}
	current, err := store.ScanRow(rows, store.Schema["local_danmaku_items"])
	if err != nil {
		return err
	}
	for _, key := range []string{"id", "created_at", "file_path", "title", "media_type", "season", "episode", "year", "tmdb_id", "tvdb_id", "imdb_id", "poster_url"} {
		value, exists := expected[key]
		if key == "created_at" && !exists {
			continue // Preserve direct callers which do not provide bookkeeping.
		}
		column, _ := store.Schema["local_danmaku_items"].Column(key)
		normalized, err := store.NormalizeValue(column, value)
		if err != nil || normalized != current[key] {
			return errors.New("local import item was deleted or changed")
		}
	}
	return nil
}

type localPendingPoster struct {
	data []byte
	ext  string
}

func (p *localPendingPoster) release() {
	if p != nil {
		p.data = nil
		<-libPosterSlots
	}
}

// This optional path only runs after XML validation, with an empty local poster
// and an already-known positive TMDB ID. The caller owns the slot until the
// downloaded bytes are committed or discarded, bounding concurrent image memory.
func (s *Server) localFetchTMDBPoster(ctx context.Context, row store.Row, target localImportTarget, attempted *bool, warn func(localImportWarning)) (*localPendingPoster, error) {
	if err := job.Checkpoint(ctx); err != nil {
		return nil, err
	}
	id := str(row["tmdb_id"])
	if str(row["poster_url"]) != "" || target.hasPoster() || !localPositiveTMDBID(id) {
		return nil, nil
	}
	report := func(code string) (*localPendingPoster, error) {
		if err := job.Checkpoint(ctx); err != nil {
			return nil, err
		}
		if warn != nil {
			warn(localPosterWarning(number(row["id"]), code))
		}
		return nil, nil
	}
	if target.conflicts(row) {
		return report("tmdb_poster_identity_conflict")
	}
	metadata := s.Metadata
	if metadata == nil || metadata.Settings == nil || strings.TrimSpace(metadata.Settings(ctx, "tmdbApiKey", "")) == "" {
		return report("tmdb_poster_key_missing")
	}
	// A timeout belonging to optional enrichment is a warning; cancellation of
	// the actual import context remains terminal at every boundary below.
	fetchCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	fetchCtx, err := metadata.WithRoutingContext(fetchCtx, "tmdb")
	if err != nil {
		return report("tmdb_poster_route_unavailable")
	}
	route, err := metadata.RoutingConfig(fetchCtx, "tmdb")
	if err != nil {
		return report("tmdb_poster_route_unavailable")
	}
	base := s.localTMDBPosterImageBase(fetchCtx)
	typ := "tv"
	if str(row["media_type"]) == "movie" {
		typ = "movie"
	}
	if err = job.Checkpoint(ctx); err != nil {
		return nil, err
	}
	*attempted = true
	details, err := metadata.Details(fetchCtx, "tmdb", id, typ, integration.Credential{})
	if err != nil {
		return report("tmdb_poster_metadata_failed")
	}
	if err = job.Checkpoint(ctx); err != nil {
		return nil, err
	}
	if !localPosterDetailsMatch(details, id, typ) {
		return report("tmdb_poster_identity_mismatch")
	}
	if details.ImageURL == "" {
		return report("tmdb_poster_missing")
	}
	client, err := s.localTMDBPosterClient(fetchCtx, route, base, details.ImageURL)
	if err != nil {
		return report("tmdb_poster_route_unavailable")
	}
	defer client.CloseIdleConnections()
	select {
	case libPosterSlots <- struct{}{}:
	case <-fetchCtx.Done():
		return report("tmdb_poster_image_failed")
	}
	keep := false
	defer func() {
		if !keep {
			<-libPosterSlots
		}
	}()
	if err = job.Checkpoint(ctx); err != nil {
		return nil, err
	}
	data, ext, err := libFetchPoster(fetchCtx, details.ImageURL, client)
	if err != nil {
		if errors.Is(err, errLocalTMDBPosterRouteUnsupported) {
			return report("tmdb_poster_route_unavailable")
		}
		return report("tmdb_poster_image_failed")
	}
	if err = job.Checkpoint(ctx); err != nil {
		return nil, err
	}
	keep = true
	return &localPendingPoster{data: data, ext: ext}, nil
}
