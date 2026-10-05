// SPDX-License-Identifier: AGPL-3.0-only
package server

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"

	"github.com/AniBakaBaka/AniDan/internal/store"
)

// Content may change at the same assignment; a different assigned file is a
// review boundary even when its metadata happens to have the same title.
func (s *Server) localImportSourceAssignment(source string) (string, error) {
	base, err := filepath.Abs(s.DataDir)
	if err != nil {
		return "", err
	}
	base, err = filepath.EvalSymlinks(base)
	if err != nil {
		return "", err
	}
	if localContained(base, source) {
		rel, err := filepath.Rel(base, source)
		if err != nil {
			return "", err
		}
		return "data:" + filepath.ToSlash(rel), nil
	}
	return "rooted:" + filepath.Clean(source), nil
}

func (s *Server) localFillImportMetadata(ctx context.Context, tx *sql.Tx, animeID int64, row store.Row) error {
	metadata, err := s.libOne(ctx, tx, "anime_metadata", "anime_id=?", animeID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if metadata == nil {
		_, err = s.Store.InsertTx(ctx, tx, "anime_metadata", store.Row{"anime_id": animeID, "tmdb_id": row["tmdb_id"], "tvdb_id": row["tvdb_id"], "imdb_id": row["imdb_id"]})
		return err
	}
	changes := store.Row{}
	for _, key := range []string{"tmdb_id", "tvdb_id", "imdb_id"} {
		incoming, existing := str(row[key]), str(metadata[key])
		if incoming != "" && existing != "" && incoming != existing {
			return errLocalImportBindingReview
		}
		if incoming != "" && existing == "" {
			changes[key] = incoming
		}
	}
	if len(changes) == 0 {
		return nil
	}
	return s.libUpdate(ctx, tx, "anime_metadata", metadata["id"], changes)
}

func (s *Server) localCheckRepeatPoolSnapshot(ctx context.Context, tx *sql.Tx, before store.Row) error {
	if before == nil {
		return errLocalImportBindingReview
	}
	suffix := ""
	if s.Store.Dialect != "sqlite" {
		suffix = " FOR UPDATE"
	}
	rows, err := s.libRows(ctx, tx, "episode", "SELECT * FROM episode WHERE id=?"+suffix, before["id"])
	if err != nil {
		return err
	}
	if len(rows) != 1 {
		return errLocalImportBindingReview
	}
	for _, key := range []string{"source_id", "episode_index", "provider_episode_id", "danmaku_file_path", "comment_count", "fetched_at"} {
		if str(before[key]) != str(rows[0][key]) {
			return errors.New("local import pool changed during this operation; reload and review")
		}
	}
	return nil
}
