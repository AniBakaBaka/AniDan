// SPDX-License-Identifier: AGPL-3.0-only
package server

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/AniBakaBaka/AniDan/internal/store"
)

const localReviewConfirm = "BIND_CURRENT_LOCAL_ASSOCIATION"
const localReviewOperation = "bind-current-local-association-v1"
const localReviewLifetime = 5 * time.Minute

var errLocalReviewUncertain = errors.New("binding commit result is uncertain; refresh its status before another action")

type localReviewRows struct {
	Item, Anime, Metadata, Source, Episode store.Row
	binding                                *localImportBinding
}
type localReviewClaim struct {
	Version     int    `json:"version"`
	Operation   string `json:"operation"`
	Actor       string `json:"actor"`
	ItemID      int64  `json:"itemId"`
	Expires     int64  `json:"expires"`
	Fingerprint string `json:"fingerprint"`
}

var localReviewItemFields = []string{"id", "created_at", "updated_at", "file_path", "title", "media_type", "season", "episode", "year", "tmdb_id", "tvdb_id", "imdb_id", "poster_url", "is_imported"}
var localReviewAnimeFields = []string{"id", "created_at", "title", "type", "season", "year"}
var localReviewMetadataFields = []string{"id", "anime_id", "tmdb_id", "tvdb_id", "imdb_id"}
var localReviewSourceFields = []string{"id", "created_at", "anime_id", "provider_name", "media_id", "source_order"}
var localReviewEpisodeFields = []string{"id", "source_id", "episode_index", "provider_episode_id", "title", "danmaku_file_path", "comment_count", "fetched_at", "media_server_episode_id"}

func localReviewCoordinates(row store.Row) (int64, int64) {
	season, episode := int64(1), int64(1)
	if row["season"] != nil {
		season = number(row["season"])
	}
	if row["episode"] != nil {
		episode = number(row["episode"])
	}
	return season, episode
}
func (s *Server) localReviewKey() ([]byte, error) {
	s.localReviewOnce.Do(func() { _, s.localReviewKeyError = rand.Read(s.localReviewSecret[:]) })
	if s.localReviewKeyError != nil {
		return nil, errors.New("review signing is unavailable")
	}
	return s.localReviewSecret[:], nil
}
func (s *Server) localReviewSign(actor string, id int64, fingerprint string, expires time.Time) (string, error) {
	key, err := s.localReviewKey()
	if err != nil {
		return "", err
	}
	raw, err := json.Marshal(localReviewClaim{1, localReviewOperation, actor, id, expires.Unix(), fingerprint})
	if err != nil {
		return "", err
	}
	mac := hmac.New(sha256.New, key)
	mac.Write(raw)
	return base64.RawURLEncoding.EncodeToString(raw) + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), nil
}
func (s *Server) localReviewVerify(token, actor string, id int64) (localReviewClaim, error) {
	var claim localReviewClaim
	if len(token) == 0 || len(token) > 2048 {
		return claim, errLocalImportBindingReview
	}
	parts := strings.Split(token, ".")
	if len(parts) != 2 {
		return claim, errLocalImportBindingReview
	}
	raw, e := base64.RawURLEncoding.Strict().DecodeString(parts[0])
	if e != nil {
		return claim, errLocalImportBindingReview
	}
	signature, e := base64.RawURLEncoding.Strict().DecodeString(parts[1])
	if e != nil || len(signature) != sha256.Size {
		return claim, errLocalImportBindingReview
	}
	key, e := s.localReviewKey()
	if e != nil {
		return claim, e
	}
	mac := hmac.New(sha256.New, key)
	mac.Write(raw)
	if !hmac.Equal(signature, mac.Sum(nil)) {
		return claim, errLocalImportBindingReview
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if d.Decode(&claim) != nil {
		return claim, errLocalImportBindingReview
	}
	canonical, e := json.Marshal(claim)
	if e != nil || !bytes.Equal(canonical, raw) || claim.Version != 1 || claim.Operation != localReviewOperation || claim.Actor != actor || claim.ItemID != id || claim.Expires <= time.Now().Unix() || claim.Expires > time.Now().Add(localReviewLifetime).Unix()+1 || len(claim.Fingerprint) != 64 {
		return claim, errLocalImportBindingReview
	}
	if _, e = hex.DecodeString(claim.Fingerprint); e != nil {
		return claim, errLocalImportBindingReview
	}
	return claim, nil
}
func localReviewFingerprint(rows localReviewRows, files localReviewFiles) (string, error) {
	for _, row := range []store.Row{rows.Item, rows.Anime, rows.Metadata, rows.Source, rows.Episode} {
		for _, value := range row {
			if text, ok := value.(string); ok && !utf8.ValidString(text) {
				return "", errLocalImportBindingReview
			}
		}
	}
	for _, text := range []string{files.SourcePath, files.PoolPath, files.SHA256, files.SourceIdentity, files.PoolIdentity, files.SourceModified, files.PoolModified} {
		if !utf8.ValidString(text) {
			return "", errLocalImportBindingReview
		}
	}
	raw, e := json.Marshal(struct {
		Rows  localReviewRows
		Files localReviewFiles
	}{rows, files})
	if e != nil || len(raw) > 64<<10 {
		return "", errLocalImportBindingReview
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}
func (s *Server) localReviewActor(r *http.Request) (string, error) {
	u, _, err := s.authClaims(r)
	if err != nil || u == nil || number(u["id"]) <= 0 {
		return "", errors.New("authentication required")
	}
	for _, field := range []string{"username", "created_at"} {
		if text, ok := u[field].(string); ok && !utf8.ValidString(text) {
			return "", errors.New("authentication identity cannot be represented exactly")
		}
	}
	raw, err := json.Marshal([]any{"local-review-actor-v1", u["id"], u["username"], u["created_at"]})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

// The endpoint is bounded and cancelable while waiting for existing import/file
// locks. There is no separate worker, review registry, or durable preview state.
func (s *Server) localReviewLocks(ctx context.Context) (func(), error) {
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for !s.importMu.TryLock() {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ticker.C:
		}
	}
	for !s.fileMu.TryLock() {
		select {
		case <-ctx.Done():
			s.importMu.Unlock()
			return nil, ctx.Err()
		case <-ticker.C:
		}
	}
	release := func() { s.fileMu.Unlock(); s.importMu.Unlock() }
	if e := ctx.Err(); e != nil {
		release()
		return nil, e
	}
	if s.fileFault.Load() {
		release()
		return nil, errors.New("storage recovery is required")
	}
	return release, nil
}

// Serialize the final candidate predicate with existing library admissions.
// Take this only after importMu/fileMu and before any final SQL row locks.
func (s *Server) localReviewMutation(ctx context.Context) (func(), error) {
	mu := &s.authState().mutation
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for !mu.TryLock() {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ticker.C:
		}
	}
	if err := ctx.Err(); err != nil {
		mu.Unlock()
		return nil, err
	}
	return mu.Unlock, nil
}

func (s *Server) localReviewReadOne(ctx context.Context, tx *sql.Tx, table, key string, id int64, fields []string, lock bool) (store.Row, error) {
	query := "SELECT " + strings.Join(fields, ",") + " FROM " + s.Store.Quote(table) + " WHERE " + key + "=? LIMIT 2"
	if lock && s.Store.Dialect != "sqlite" {
		query += " FOR UPDATE"
	}
	rows, e := s.libRows(ctx, tx, table, query, id)
	if e != nil {
		return nil, e
	}
	if len(rows) != 1 {
		return nil, errLocalImportBindingReview
	}
	normalized, err := localBindingFields(table, rows[0], fields...)
	if err != nil {
		return nil, err
	}
	for _, value := range normalized {
		if text, ok := value.(string); ok && !utf8.ValidString(text) {
			return nil, errLocalImportBindingReview
		}
	}
	return normalized, nil
}

// Explicit review refuses recognized case/space aliases even on case-sensitive
// engines. The database's equality also detects its own collation aliases.
// This does not reinterpret arbitrary Unicode or noncanonical numeric suffixes.
func (s *Server) localReviewCheckNamespace(ctx context.Context, tx *sql.Tx, id int64) error {
	key := localImportBindingKey(id)
	query := "SELECT config_key FROM config WHERE config_key=? OR LOWER(TRIM(config_key))=LOWER(?) LIMIT 2"
	if s.Store.Dialect != "sqlite" {
		query += " FOR UPDATE"
	}
	rows, err := tx.QueryContext(ctx, s.Store.Rebind(query), key, key)
	if err != nil {
		return err
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		var actual string
		if err = rows.Scan(&actual); err != nil {
			return errLocalImportBindingReview
		}
		count++
		if actual != key || count > 1 {
			return errLocalImportBindingReview
		}
	}
	return rows.Err()
}

func (s *Server) localReviewCapture(ctx context.Context, tx *sql.Tx, id int64) (out localReviewRows, err error) {
	out.Item, err = s.localReviewReadOne(ctx, tx, "local_danmaku_items", "id", id, localReviewItemFields, true)
	if err != nil {
		return out, err
	}
	if !boolean(out.Item["is_imported"]) {
		return out, errLocalImportBindingReview
	}
	if err = s.localReviewCheckNamespace(ctx, tx, id); err != nil {
		return out, err
	}
	out.binding, err = s.localReadImportBinding(ctx, tx, id, true)
	if err != nil {
		return out, err
	}
	var animeID, sourceID, episodeID int64
	season, index := localReviewCoordinates(out.Item)
	if season < 0 || index < 0 {
		return out, errLocalImportBindingReview
	}
	marker := "local_" + strconv.FormatInt(id, 10) + "_" + strconv.FormatInt(index, 10)
	if out.binding != nil {
		animeID, sourceID, episodeID = out.binding.AnimeID, out.binding.SourceID, out.binding.EpisodeID
	} else {
		candidates, e := s.libRows(ctx, tx, "episode", "SELECT id,source_id,provider_episode_id,episode_index FROM episode WHERE provider_episode_id=? LIMIT 3", marker)
		if e != nil {
			return out, e
		}
		if len(candidates) != 1 || str(candidates[0]["provider_episode_id"]) != marker || number(candidates[0]["episode_index"]) != index {
			return out, errLocalImportBindingReview
		}
		episodeID, sourceID = number(candidates[0]["id"]), number(candidates[0]["source_id"])
		src, e := s.localReviewReadOne(ctx, tx, "anime_sources", "id", sourceID, localReviewSourceFields, false)
		if e != nil {
			return out, e
		}
		animeID = number(src["anime_id"])
	}
	// Preserve the established local writer's parent-first lock ordering.
	out.Anime, err = s.localReviewReadOne(ctx, tx, "anime", "id", animeID, localReviewAnimeFields, true)
	if err != nil {
		return out, err
	}
	out.Metadata, err = s.localReviewReadOne(ctx, tx, "anime_metadata", "anime_id", animeID, localReviewMetadataFields, true)
	if err != nil {
		return out, err
	}
	out.Source, err = s.localReviewReadOne(ctx, tx, "anime_sources", "id", sourceID, localReviewSourceFields, true)
	if err != nil {
		return out, err
	}
	out.Episode, err = s.localReviewReadOne(ctx, tx, "episode", "id", episodeID, localReviewEpisodeFields, true)
	if err != nil {
		return out, err
	}
	if animeID <= 0 || sourceID <= 0 || episodeID <= 0 || number(out.Source["anime_id"]) != animeID || number(out.Episode["source_id"]) != sourceID || number(out.Episode["episode_index"]) != index {
		return out, errLocalImportBindingReview
	}
	for _, r := range []store.Row{out.Item, out.Anime, out.Source} {
		if _, e := localBindingDate(str(r["created_at"])); e != nil {
			return out, errLocalImportBindingReview
		}
	}
	if out.binding != nil {
		if err = s.localCheckImportBindingRows(ctx, tx, out.binding); err != nil {
			return out, err
		}
		return out, nil
	}
	// The editable marker only locates candidates. Literal metadata, the actual
	// foreign-key chain, and independently checked file equality are all required.
	if str(out.Item["title"]) != str(out.Anime["title"]) || str(out.Item["media_type"]) != str(out.Anime["type"]) || number(out.Anime["season"]) != season || !reflect.DeepEqual(out.Item["year"], out.Anime["year"]) || str(out.Episode["provider_episode_id"]) != marker {
		return out, errLocalImportBindingReview
	}
	for _, key := range []string{"tmdb_id", "tvdb_id", "imdb_id"} {
		if !reflect.DeepEqual(localBindingKnownID(out.Item[key]), localBindingKnownID(out.Metadata[key])) {
			return out, errLocalImportBindingReview
		}
	}
	if str(out.Source["provider_name"]) == "" || str(out.Source["media_id"]) == "" || number(out.Source["source_order"]) < 0 {
		return out, errLocalImportBindingReview
	}
	return out, nil
}
func (s *Server) localReviewReadSnapshot(ctx context.Context, id int64) (localReviewRows, error) {
	release, e := s.localReviewMutation(ctx)
	if e != nil {
		return localReviewRows{}, e
	}
	defer release()
	tx, e := s.Store.DB.BeginTx(ctx, nil)
	if e != nil {
		return localReviewRows{}, e
	}
	defer tx.Rollback()
	rows, e := s.localReviewCapture(ctx, tx, id)
	if e != nil {
		return rows, e
	}
	return rows, tx.Commit()
}

// Preview must satisfy the same canonical record constraints as the eventual
// INSERT. This validates admissibility without writing any protected state.
func (s *Server) localReviewAdmissible(rows localReviewRows, files localReviewFiles) error {
	assignment, err := s.localImportSourceAssignment(files.SourcePath)
	if err != nil {
		return errLocalImportBindingReview
	}
	season, episode := localReviewCoordinates(rows.Item)
	opt := localImportOption{str(rows.Source["provider_name"]), str(rows.Source["media_id"])}
	input, err := localBindingInput(rows.Item, opt, assignment, season, episode, number(rows.Anime["id"]))
	if err != nil {
		return err
	}
	if input.Type != "movie" && input.Type != "tv_series" {
		return errLocalImportBindingReview
	}
	b := localImportBinding{Version: 1, Input: input, AnimeID: number(rows.Anime["id"]), SourceID: number(rows.Source["id"]), EpisodeID: number(rows.Episode["id"]),
		Anime:    localImportBindingAnime{str(rows.Anime["created_at"]), str(rows.Anime["title"]), str(rows.Anime["type"]), number(rows.Anime["season"]), localBindingInt(rows.Anime["year"])},
		Metadata: localImportBindingMeta{number(rows.Metadata["id"]), localBindingKnownID(rows.Metadata["tmdb_id"]), localBindingKnownID(rows.Metadata["tvdb_id"]), localBindingKnownID(rows.Metadata["imdb_id"])},
		Source:   localImportBindingSource{str(rows.Source["created_at"]), number(rows.Source["anime_id"]), opt.Provider, opt.MediaID, number(rows.Source["source_order"])},
		Episode:  localImportBindingEpisode{number(rows.Episode["source_id"]), number(rows.Episode["episode_index"]), localBindingString(rows.Episode["provider_episode_id"])},
	}
	raw, err := json.Marshal(b)
	if err != nil {
		return errLocalImportBindingReview
	}
	_, err = localDecodeImportBinding(string(raw), input.ItemID)
	return err
}

func localReviewOptions(rows localReviewRows) store.Row {
	return store.Row{"itemId": strconv.FormatInt(number(rows.Item["id"]), 10), "provider": str(rows.Source["provider_name"]), "mediaId": str(rows.Source["media_id"])}
}
func localReviewBound(rows localReviewRows) store.Row {
	return store.Row{"status": "bound", "itemId": strconv.FormatInt(number(rows.Item["id"]), 10), "identityValidated": true, "repeatOptions": localReviewOptions(rows)}
}
func (s *Server) localReviewBoundInput(ctx context.Context, rows localReviewRows) error {
	source, e := s.localAllowedPath(str(rows.Item["file_path"]), false)
	if e != nil {
		return errLocalImportBindingReview
	}
	f, e := s.localOpenAllowed(source)
	if e != nil {
		return errLocalImportBindingReview
	}
	info, e := f.Stat()
	f.Close()
	if e != nil || !info.Mode().IsRegular() || info.Size() > localReviewFileLimit {
		return errLocalImportBindingReview
	}
	assignment, e := s.localImportSourceAssignment(source)
	if e != nil {
		return errLocalImportBindingReview
	}
	season, index := localReviewCoordinates(rows.Item)
	if e = localCheckImportBindingInput(rows.binding, rows.Item, localImportOption{str(rows.Source["provider_name"]), str(rows.Source["media_id"])}, assignment, season, index); e != nil {
		return e
	}
	return ctx.Err()
}
func localReviewDecode(r *http.Request, v any) error {
	raw, e := io.ReadAll(io.LimitReader(r.Body, 4097))
	if e != nil || len(raw) > 4096 {
		return errors.New("invalid bounded review request")
	}
	if len(raw) == 0 {
		raw = []byte("{}")
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if e = d.Decode(v); e != nil {
		return errors.New("invalid review fields")
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return errors.New("review requires one JSON object")
	}
	return nil
}
func localReviewID(r *http.Request) (int64, error) {
	raw := r.PathValue("item_id")
	id, e := strconv.ParseInt(raw, 10, 64)
	if e != nil || id <= 0 || strconv.FormatInt(id, 10) != raw {
		return 0, errors.New("invalid exact item ID")
	}
	return id, nil
}
func localReviewHTTPError(w http.ResponseWriter, err error) {
	if errors.Is(err, errLocalReviewUncertain) {
		httpError(w, 500, errLocalReviewUncertain.Error())
		return
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		httpError(w, 408, "Review was canceled or timed out; refresh before another action")
		return
	}
	httpError(w, 409, "Current association cannot be verified or the reviewed data changed; refresh and review again. Existing data was retained")
}

func (s *Server) localBindingPreview(w http.ResponseWriter, r *http.Request) {
	actor, e := s.localReviewActor(r)
	if e != nil {
		httpError(w, 401, "Authenticated user token required")
		return
	}
	id, e := localReviewID(r)
	if e != nil {
		httpError(w, 422, e.Error())
		return
	}
	if e = localReviewDecode(r, &struct{}{}); e != nil {
		httpError(w, 422, e.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	release, e := s.localReviewLocks(ctx)
	if e != nil {
		localReviewHTTPError(w, e)
		return
	}
	defer release()
	rows, e := s.localReviewReadSnapshot(ctx, id)
	if e != nil {
		localReviewHTTPError(w, e)
		return
	}
	if rows.binding != nil {
		if e = s.localReviewBoundInput(ctx, rows); e != nil {
			localReviewHTTPError(w, e)
			return
		}
		writeJSON(w, 200, localReviewBound(rows))
		return
	}
	files, e := s.localReviewFilesLocked(ctx, rows.Item, rows.Episode)
	if e != nil {
		localReviewHTTPError(w, e)
		return
	}
	if e = s.localReviewAdmissible(rows, files); e != nil {
		localReviewHTTPError(w, e)
		return
	}
	fingerprint, e := localReviewFingerprint(rows, files)
	if e != nil {
		localReviewHTTPError(w, e)
		return
	}
	expires := time.Now().UTC().Add(localReviewLifetime)
	token, e := s.localReviewSign(actor, id, fingerprint, expires)
	if e != nil {
		localReviewHTTPError(w, e)
		return
	}
	season, index := localReviewCoordinates(rows.Item)
	writeJSON(w, 200, store.Row{"status": "reviewable", "itemId": strconv.FormatInt(id, 10), "reviewToken": token, "expiresAt": expires.Format(time.RFC3339), "repeatOptions": localReviewOptions(rows), "item": store.Row{"id": strconv.FormatInt(id, 10), "title": rows.Item["title"], "mediaType": rows.Item["media_type"], "year": rows.Item["year"], "season": season, "episode": index, "createdAt": rows.Item["created_at"], "sourcePath": files.SourcePath, "isImported": true, "tmdbId": rows.Item["tmdb_id"], "tvdbId": rows.Item["tvdb_id"], "imdbId": rows.Item["imdb_id"]}, "target": store.Row{"animeId": strconv.FormatInt(number(rows.Anime["id"]), 10), "title": rows.Anime["title"], "type": rows.Anime["type"], "year": rows.Anime["year"], "season": rows.Anime["season"], "metadataId": strconv.FormatInt(number(rows.Metadata["id"]), 10), "tmdbId": rows.Metadata["tmdb_id"], "tvdbId": rows.Metadata["tvdb_id"], "imdbId": rows.Metadata["imdb_id"]}, "source": store.Row{"sourceId": strconv.FormatInt(number(rows.Source["id"]), 10), "provider": rows.Source["provider_name"], "mediaId": rows.Source["media_id"], "sourceOrder": rows.Source["source_order"], "createdAt": rows.Source["created_at"]}, "episode": store.Row{"episodeId": strconv.FormatInt(number(rows.Episode["id"]), 10), "index": rows.Episode["episode_index"], "providerEpisodeId": rows.Episode["provider_episode_id"], "poolPath": files.PoolPath}, "files": store.Row{"sourceBytes": files.Bytes, "poolBytes": files.Bytes, "sourceSha256": files.SHA256, "poolSha256": files.SHA256, "commentCount": files.Count, "byteEqual": true}})
}
func (s *Server) localBindingConfirm(w http.ResponseWriter, r *http.Request) {
	actor, e := s.localReviewActor(r)
	if e != nil {
		httpError(w, 401, "Authenticated user token required")
		return
	}
	id, e := localReviewID(r)
	if e != nil {
		httpError(w, 422, e.Error())
		return
	}
	var in struct {
		ReviewToken string `json:"reviewToken"`
		Confirm     string `json:"confirm"`
	}
	if e = localReviewDecode(r, &in); e != nil || in.Confirm != localReviewConfirm || len(in.ReviewToken) > 2048 {
		httpError(w, 422, "Explicit bounded current-association confirmation required")
		return
	}
	claim, e := s.localReviewVerify(in.ReviewToken, actor, id)
	if e != nil {
		localReviewHTTPError(w, e)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	release, e := s.localReviewLocks(ctx)
	if e != nil {
		localReviewHTTPError(w, e)
		return
	}
	defer release()
	rows, e := s.localReviewReadSnapshot(ctx, id)
	if e != nil {
		localReviewHTTPError(w, e)
		return
	}
	if rows.binding != nil {
		localReviewHTTPError(w, errLocalImportBindingReview)
		return
	}
	files, e := s.localReviewFilesLocked(ctx, rows.Item, rows.Episode)
	if e != nil {
		localReviewHTTPError(w, e)
		return
	}
	if e = s.localReviewAdmissible(rows, files); e != nil {
		localReviewHTTPError(w, e)
		return
	}
	fingerprint, e := localReviewFingerprint(rows, files)
	if e != nil || fingerprint != claim.Fingerprint {
		localReviewHTTPError(w, errLocalImportBindingReview)
		return
	}
	assignment, e := s.localImportSourceAssignment(files.SourcePath)
	if e != nil {
		localReviewHTTPError(w, e)
		return
	}
	mutationRelease, e := s.localReviewMutation(ctx)
	if e != nil {
		localReviewHTTPError(w, e)
		return
	}
	defer mutationRelease()
	tx, e := s.Store.DB.BeginTx(ctx, nil)
	if e != nil {
		localReviewHTTPError(w, e)
		return
	}
	defer tx.Rollback()
	current, e := s.localReviewCapture(ctx, tx, id)
	if e != nil {
		localReviewHTTPError(w, e)
		return
	}
	if current.binding != nil {
		localReviewHTTPError(w, errLocalImportBindingReview)
		return
	}
	currentFingerprint, e := localReviewFingerprint(current, files)
	if e != nil || currentFingerprint != claim.Fingerprint {
		localReviewHTTPError(w, errLocalImportBindingReview)
		return
	}
	if _, e = s.localReviewVerify(in.ReviewToken, actor, id); e != nil {
		localReviewHTTPError(w, e)
		return
	}
	if e = s.localReviewFileVersionsLocked(ctx, current.Item, current.Episode, files); e != nil {
		localReviewHTTPError(w, e)
		return
	}
	season, index := localReviewCoordinates(current.Item)
	opt := localImportOption{str(current.Source["provider_name"]), str(current.Source["media_id"])}
	if e = s.localWriteImportBinding(ctx, tx, nil, current.Item, opt, assignment, season, index, number(current.Anime["id"]), number(current.Source["id"]), number(current.Episode["id"])); e != nil {
		localReviewHTTPError(w, e)
		return
	}
	if e = ctx.Err(); e != nil {
		localReviewHTTPError(w, e)
		return
	}
	if e = tx.Commit(); e != nil {
		localReviewHTTPError(w, errLocalReviewUncertain)
		return
	}
	writeJSON(w, 200, localReviewBound(current))
}
