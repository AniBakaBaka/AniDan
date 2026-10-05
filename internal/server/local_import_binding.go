// SPDX-License-Identifier: AGPL-3.0-only
package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"reflect"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/AniBakaBaka/AniDan/internal/store"
)

const localImportBindingLimit = 8 << 10

var errLocalImportBindingReview = errors.New("local import ownership requires review; existing library data was retained")

// Ownership is durable protocol state in the protected config namespace. It is
// written with the first successful import or an explicit, evidence-checked
// current-association review. It is never inferred automatically from editable
// source names/provider episode IDs or reset by an ordinary repeat import.
type localImportBinding struct {
	Version   int                       `json:"version"`
	Input     localImportBindingInput   `json:"input"`
	AnimeID   int64                     `json:"animeId"`
	Anime     localImportBindingAnime   `json:"anime"`
	Metadata  localImportBindingMeta    `json:"metadata"`
	SourceID  int64                     `json:"sourceId"`
	Source    localImportBindingSource  `json:"source"`
	EpisodeID int64                     `json:"episodeId"`
	Episode   localImportBindingEpisode `json:"episode"`
	raw       string
}

type localImportBindingInput struct {
	ItemID           int64   `json:"itemId"`
	CreatedAt        string  `json:"createdAt"`
	Title            string  `json:"title"`
	Type             string  `json:"type"`
	Year             *int64  `json:"year"`
	Season           int64   `json:"season"`
	Episode          int64   `json:"episode"`
	TMDBID           *string `json:"tmdbId"`
	TVDBID           *string `json:"tvdbId"`
	IMDBID           *string `json:"imdbId"`
	SourceAssignment string  `json:"sourceAssignment"`
	Provider         string  `json:"provider"`
	MediaID          string  `json:"mediaId"`
}

type localImportBindingAnime struct {
	CreatedAt string `json:"createdAt"`
	Title     string `json:"title"`
	Type      string `json:"type"`
	Season    int64  `json:"season"`
	Year      *int64 `json:"year"`
}

type localImportBindingMeta struct {
	ID     int64   `json:"id"`
	TMDBID *string `json:"tmdbId"`
	TVDBID *string `json:"tvdbId"`
	IMDBID *string `json:"imdbId"`
}

type localImportBindingSource struct {
	CreatedAt string `json:"createdAt"`
	AnimeID   int64  `json:"animeId"`
	Provider  string `json:"provider"`
	MediaID   string `json:"mediaId"`
	Order     int64  `json:"order"`
}

type localImportBindingEpisode struct {
	SourceID   int64   `json:"sourceId"`
	Index      int64   `json:"index"`
	ProviderID *string `json:"providerId"`
}

func localImportBindingKey(itemID int64) string {
	return "anidan.local_import.binding." + strconv.FormatInt(itemID, 10)
}

// Databases represent the same naive timestamp with a space or T separator.
// Normalize that representation without introducing a timezone conversion.
func localBindingDate(v string) (string, error) {
	for _, layout := range []string{"2006-01-02T15:04:05.999999999", "2006-01-02 15:04:05.999999999", "2006-01-02"} {
		if parsed, err := time.Parse(layout, v); err == nil {
			return parsed.Format("2006-01-02T15:04:05.999999999"), nil
		}
	}
	return "", errLocalImportBindingReview
}

func localBindingFields(table string, row store.Row, fields ...string) (store.Row, error) {
	out := store.Row{}
	for _, key := range fields {
		column, _ := store.Schema[table].Column(key)
		if _, floating := row[key].(float64); floating && (column.Kind == "integer" || column.Kind == "bigint") {
			return nil, errLocalImportBindingReview
		}
		v, err := store.NormalizeValue(column, row[key])
		if err != nil {
			return nil, errLocalImportBindingReview
		}
		if column.Kind == "datetime" && v != nil {
			v, err = localBindingDate(v.(string))
			if err != nil {
				return nil, err
			}
		}
		out[key] = v
	}
	return out, nil
}

func localBindingString(v any) *string {
	if v == nil {
		return nil
	}
	s := v.(string) // Callers use only schema-normalized rows.
	return &s
}

func localBindingKnownID(v any) *string {
	if v == nil || v == "" {
		return nil // SQL NULL and empty IDs both mean no known identifier.
	}
	return localBindingString(v)
}

func localBindingInt(v any) *int64 {
	if v == nil {
		return nil
	}
	n := v.(int64)
	return &n
}

func localBindingInput(row store.Row, opt localImportOption, sourceAssignment string, season, episode, animeID int64) (localImportBindingInput, error) {
	r, err := localBindingFields("local_danmaku_items", row, "id", "created_at", "title", "media_type", "year", "tmdb_id", "tvdb_id", "imdb_id")
	if err != nil {
		return localImportBindingInput{}, err
	}
	id := r["id"].(int64)
	if id <= 0 || animeID <= 0 || season < 0 || episode < 0 || sourceAssignment == "" || len(sourceAssignment) > 2048 || !utf8.ValidString(sourceAssignment) || strings.ContainsRune(sourceAssignment, 0) {
		return localImportBindingInput{}, errLocalImportBindingReview
	}
	if opt.Provider == "" {
		opt.Provider = "custom"
	}
	if opt.MediaID == "" {
		opt.MediaID = "custom_" + strconv.FormatInt(animeID, 10) + "_" + strconv.FormatInt(id, 10)
	}
	if len([]rune(opt.Provider)) > 500 || len([]rune(opt.MediaID)) > 255 || !utf8.ValidString(opt.Provider) || !utf8.ValidString(opt.MediaID) {
		return localImportBindingInput{}, errLocalImportBindingReview
	}
	return localImportBindingInput{ItemID: id, CreatedAt: r["created_at"].(string), Title: r["title"].(string), Type: r["media_type"].(string), Year: localBindingInt(r["year"]), Season: season, Episode: episode, TMDBID: localBindingKnownID(r["tmdb_id"]), TVDBID: localBindingKnownID(r["tvdb_id"]), IMDBID: localBindingKnownID(r["imdb_id"]), SourceAssignment: sourceAssignment, Provider: opt.Provider, MediaID: opt.MediaID}, nil
}

func localDecodeImportBinding(raw string, itemID int64) (*localImportBinding, error) {
	if len(raw) == 0 || len(raw) > localImportBindingLimit || !utf8.ValidString(raw) {
		return nil, errLocalImportBindingReview
	}
	var b localImportBinding
	d := json.NewDecoder(strings.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(&b); err != nil {
		return nil, errLocalImportBindingReview
	}
	// Only this writer's canonical encoding is accepted. Besides bounding the
	// format, this rejects missing/null required fields, duplicates, aliases,
	// trailing data and noncanonical integer spellings without using float64.
	canonical, err := json.Marshal(&b)
	if err != nil || string(canonical) != raw || b.Version != 1 || itemID <= 0 || b.Input.ItemID != itemID || b.AnimeID <= 0 || b.Metadata.ID <= 0 || b.SourceID <= 0 || b.EpisodeID <= 0 || b.Source.AnimeID != b.AnimeID || b.Episode.SourceID != b.SourceID || b.Source.Order < 0 || b.Anime.Season < 0 || b.Episode.Index != b.Input.Episode || b.Source.Provider != b.Input.Provider || b.Source.MediaID != b.Input.MediaID {
		return nil, errLocalImportBindingReview
	}
	i := b.Input
	r := store.Row{"id": i.ItemID, "created_at": i.CreatedAt, "title": i.Title, "media_type": i.Type, "year": localBindingIntValue(i.Year), "tmdb_id": localBindingStringValue(i.TMDBID), "tvdb_id": localBindingStringValue(i.TVDBID), "imdb_id": localBindingStringValue(i.IMDBID)}
	normal, err := localBindingInput(r, localImportOption{i.Provider, i.MediaID}, i.SourceAssignment, i.Season, i.Episode, b.AnimeID)
	if err != nil || !reflect.DeepEqual(normal, i) {
		return nil, errLocalImportBindingReview
	}
	for _, date := range []string{b.Anime.CreatedAt, b.Source.CreatedAt} {
		if normal, err := localBindingDate(date); err != nil || normal != date {
			return nil, errLocalImportBindingReview
		}
	}
	// Validate the remaining captured fields against their actual SQL schema.
	// This keeps even a well-formed/canonical but semantically invalid record
	// on the review path before any target or source can be selected from it.
	for _, part := range []struct {
		table string
		row   store.Row
	}{
		{"anime", store.Row{"title": b.Anime.Title, "type": b.Anime.Type, "season": b.Anime.Season, "year": localBindingIntValue(b.Anime.Year)}},
		{"anime_metadata", store.Row{"tmdb_id": localBindingStringValue(b.Metadata.TMDBID), "tvdb_id": localBindingStringValue(b.Metadata.TVDBID), "imdb_id": localBindingStringValue(b.Metadata.IMDBID)}},
		{"episode", store.Row{"provider_episode_id": localBindingStringValue(b.Episode.ProviderID)}},
	} {
		if _, err := store.ValidateRow(part.table, part.row, false); err != nil {
			return nil, errLocalImportBindingReview
		}
	}
	for _, id := range []*string{b.Metadata.TMDBID, b.Metadata.TVDBID, b.Metadata.IMDBID} {
		if id != nil && *id == "" {
			return nil, errLocalImportBindingReview
		}
	}
	b.raw = raw
	return &b, nil
}

func localBindingIntValue(v *int64) any {
	if v == nil {
		return nil
	}
	return *v
}

func localBindingStringValue(v *string) any {
	if v == nil {
		return nil
	}
	return *v
}

func (s *Server) localReadImportBinding(ctx context.Context, q libQueryer, itemID int64, lock bool) (*localImportBinding, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if itemID <= 0 {
		return nil, errLocalImportBindingReview
	}
	query := "SELECT config_key,SUBSTR(config_value,1,8193) FROM config WHERE config_key=? LIMIT 2"
	if lock && s.Store.Dialect != "sqlite" {
		query += " FOR UPDATE"
	}
	key := localImportBindingKey(itemID)
	rows, err := q.QueryContext(ctx, s.Store.Rebind(query), key)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	if !rows.Next() {
		return nil, rows.Err()
	}
	var actual, raw string
	if err = rows.Scan(&actual, &raw); err != nil {
		if cancelled := ctx.Err(); cancelled != nil {
			return nil, cancelled
		}
		return nil, errLocalImportBindingReview
	}
	if actual != key || rows.Next() {
		return nil, errLocalImportBindingReview
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	return localDecodeImportBinding(raw, itemID)
}

func localCheckImportBindingInput(b *localImportBinding, row store.Row, opt localImportOption, sourceAssignment string, season, episode int64) error {
	if b == nil {
		return errLocalImportBindingReview
	}
	input, err := localBindingInput(row, opt, sourceAssignment, season, episode, b.AnimeID)
	if err != nil || !reflect.DeepEqual(input, b.Input) {
		return errLocalImportBindingReview
	}
	return nil
}

func (s *Server) localBindingRow(ctx context.Context, tx *sql.Tx, table, where string, id int64, fields ...string) (store.Row, error) {
	query := "SELECT " + strings.Join(fields, ",") + " FROM " + table + " WHERE " + where + "=?"
	if s.Store.Dialect != "sqlite" {
		query += " FOR UPDATE"
	}
	rows, err := s.libRows(ctx, tx, table, query, id)
	if err != nil {
		return nil, err
	}
	if len(rows) != 1 {
		return nil, errLocalImportBindingReview
	}
	return localBindingFields(table, rows[0], fields...)
}

// Keep the existing transaction lock order: anime, metadata, source, episode.
// No metadata manager, filesystem or network callback occurs while SQL is held.
func (s *Server) localCaptureImportBindingRows(ctx context.Context, tx *sql.Tx, b *localImportBinding) error {
	a, err := s.localBindingRow(ctx, tx, "anime", "id", b.AnimeID, "created_at", "title", "type", "season", "year")
	if err != nil {
		return err
	}
	m, err := s.localBindingRow(ctx, tx, "anime_metadata", "anime_id", b.AnimeID, "id", "tmdb_id", "tvdb_id", "imdb_id")
	if err != nil {
		return err
	}
	src, err := s.localBindingRow(ctx, tx, "anime_sources", "id", b.SourceID, "created_at", "anime_id", "provider_name", "media_id", "source_order")
	if err != nil {
		return err
	}
	ep, err := s.localBindingRow(ctx, tx, "episode", "id", b.EpisodeID, "source_id", "episode_index", "provider_episode_id")
	if err != nil {
		return err
	}
	b.Anime = localImportBindingAnime{a["created_at"].(string), a["title"].(string), a["type"].(string), a["season"].(int64), localBindingInt(a["year"])}
	b.Metadata = localImportBindingMeta{m["id"].(int64), localBindingKnownID(m["tmdb_id"]), localBindingKnownID(m["tvdb_id"]), localBindingKnownID(m["imdb_id"])}
	b.Source = localImportBindingSource{src["created_at"].(string), src["anime_id"].(int64), src["provider_name"].(string), src["media_id"].(string), src["source_order"].(int64)}
	b.Episode = localImportBindingEpisode{ep["source_id"].(int64), ep["episode_index"].(int64), localBindingString(ep["provider_episode_id"])}
	return nil
}

// Compare only against the original immutable capture. Every originally known
// ID must still match, and at least one such ID must anchor any newly known ID.
// Later additions never become anchors or rewrite this original ownership.
func localBindingMetadataCompatible(captured, current localImportBindingMeta) bool {
	if captured.ID != current.ID {
		return false
	}
	anchored, added := false, false
	for _, pair := range [][2]*string{{captured.TMDBID, current.TMDBID}, {captured.TVDBID, current.TVDBID}, {captured.IMDBID, current.IMDBID}} {
		if pair[0] != nil {
			if pair[1] == nil || *pair[0] != *pair[1] {
				return false
			}
			anchored = true
		} else if pair[1] != nil {
			added = true
		}
	}
	return !added || anchored
}

func (s *Server) localCheckImportBindingRows(ctx context.Context, tx *sql.Tx, b *localImportBinding) error {
	if b == nil {
		return errLocalImportBindingReview
	}
	protected, err := s.localReadImportBinding(ctx, tx, b.Input.ItemID, true)
	if err != nil {
		return err
	}
	if protected == nil || b.raw == "" || protected.raw != b.raw || !reflect.DeepEqual(protected, b) {
		return errLocalImportBindingReview
	}
	current := *b
	if err := s.localCaptureImportBindingRows(ctx, tx, &current); err != nil {
		return err
	}
	if !localBindingMetadataCompatible(b.Metadata, current.Metadata) {
		return errLocalImportBindingReview
	}
	// Only metadata additions proven compatible above are excluded from the
	// remaining exact comparison; the protected record itself stays untouched.
	current.Metadata = b.Metadata
	if !reflect.DeepEqual(current, *b) {
		return errLocalImportBindingReview
	}
	return nil
}

func (s *Server) localWriteImportBinding(ctx context.Context, tx *sql.Tx, prior *localImportBinding, row store.Row, opt localImportOption, sourceAssignment string, season, episode, animeID, sourceID, episodeID int64) error {
	input, err := localBindingInput(row, opt, sourceAssignment, season, episode, animeID)
	if err != nil {
		return err
	}
	current, err := s.localReadImportBinding(ctx, tx, input.ItemID, true)
	if err != nil {
		return err
	}
	if prior != nil {
		if current == nil || prior.raw == "" || current.raw != prior.raw || !reflect.DeepEqual(current, prior) || animeID != prior.AnimeID || sourceID != prior.SourceID || episodeID != prior.EpisodeID || !reflect.DeepEqual(input, prior.Input) {
			return errLocalImportBindingReview
		}
		return s.localCheckImportBindingRows(ctx, tx, prior)
	}
	if current != nil {
		return errLocalImportBindingReview
	}
	b := localImportBinding{Version: 1, Input: input, AnimeID: animeID, SourceID: sourceID, EpisodeID: episodeID}
	if err = s.localCaptureImportBindingRows(ctx, tx, &b); err != nil {
		return err
	}
	raw, err := json.Marshal(&b)
	if err != nil {
		return errLocalImportBindingReview
	}
	if _, err = localDecodeImportBinding(string(raw), input.ItemID); err != nil {
		return err
	}
	// INSERT alone is intentional. A competing binding or a collation alias
	// must abort this transaction, never be adopted by an upsert.
	_, err = tx.ExecContext(ctx, s.Store.Rebind("INSERT INTO config(config_key,config_value,description) VALUES(?,?,?)"), localImportBindingKey(input.ItemID), string(raw), "Protected local import ownership")
	if err != nil {
		if cancelled := ctx.Err(); cancelled != nil {
			return cancelled
		}
		return errLocalImportBindingReview
	}
	return nil
}
