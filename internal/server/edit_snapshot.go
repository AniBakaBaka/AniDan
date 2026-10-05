// SPDX-License-Identifier: AGPL-3.0-only
package server

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"sort"

	"github.com/AniBakaBaka/AniDan/internal/danmaku"
	"github.com/AniBakaBaka/AniDan/internal/store"
)

// editSnapshot binds derived XML to the exact input bytes and episode/source
// rows. Path, mtime and fetched_at alone are insufficient: legacy stable-path
// refreshes can preserve a path and complete in the same wall-clock second.
// Unreferenced staged objects are harmless; no live pointer changes on conflict.
type editSnapshot struct {
	Episode store.Row
	Source  store.Row
	SHA256  string
}

func (s *Server) readEditSnapshot(ctx context.Context, id int64) (editSnapshot, []danmaku.Comment, error) {
	s.fileMu.RLock()
	defer s.fileMu.RUnlock()
	if s.fileFault.Load() {
		return editSnapshot{}, nil, errors.New("storage recovery required")
	}
	ep, err := s.Store.Get(ctx, "episode", id)
	if err != nil {
		return editSnapshot{}, nil, err
	}
	src, err := s.Store.Get(ctx, "anime_sources", ep["source_id"])
	if err != nil {
		return editSnapshot{}, nil, err
	}
	file, err := s.resolveDanmaku(str(ep["danmaku_file_path"]))
	if err != nil {
		return editSnapshot{}, nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return editSnapshot{}, nil, err
	}
	if !info.Mode().IsRegular() {
		return editSnapshot{}, nil, errors.New("edit input must be a regular XML file")
	}
	hash := sha256.New()
	// Parse reads through EOF with its normal byte/comment/depth bounds. Using
	// that exact stream avoids a separate stat/hash race and bypasses a stale
	// parsed cache when an external writer preserved all file metadata.
	comments, err := danmaku.Parse(io.TeeReader(contextReader{ctx, file}, hash))
	if err != nil {
		return editSnapshot{}, nil, err
	}
	return editSnapshot{Episode: ep, Source: src, SHA256: hex.EncodeToString(hash.Sum(nil))}, comments, nil
}

// Caller holds fileMu exclusively and uses libTransaction's mutation lock.
// Database row locks also prevent an independent SQL connection from changing
// the selected rows between validation and commit on MySQL/PostgreSQL. SQLite's
// read/write transaction fails rather than upgrading a changed read snapshot.
func (s *Server) validateEditSnapshots(ctx context.Context, tx *sql.Tx, inputs []editSnapshot, plans []editObject, deletes []int64) error {
	if len(inputs) == 0 {
		return errors.New("edit input snapshot required")
	}
	ordered := append([]editSnapshot(nil), inputs...)
	sort.Slice(ordered, func(i, j int) bool { return number(ordered[i].Episode["id"]) < number(ordered[j].Episode["id"]) })
	seen := make(map[int64]bool, len(ordered))
	for _, input := range ordered {
		id := number(input.Episode["id"])
		if id <= 0 || seen[id] || len(input.SHA256) != 64 {
			return errors.New("invalid or duplicate edit input snapshot")
		}
		seen[id] = true
		for _, item := range []struct {
			table string
			row   store.Row
		}{{"episode", input.Episode}, {"anime_sources", input.Source}} {
			query := "SELECT * FROM " + s.Store.Quote(item.table) + " WHERE id=?"
			if s.Store.Dialect != "sqlite" {
				query += " FOR UPDATE"
			}
			rows, err := s.libRows(ctx, tx, item.table, query, item.row["id"])
			if err != nil {
				return err
			}
			if len(rows) != 1 || !reflect.DeepEqual(rows[0], item.row) {
				return fmt.Errorf("episode %d changed during edit; reload and retry", id)
			}
		}
		hash, err := s.hashDanmakuFile(ctx, str(input.Episode["danmaku_file_path"]))
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("episode %d input is no longer readable; reload and retry", id)
		}
		if hash != input.SHA256 {
			return fmt.Errorf("episode %d comments changed during edit; reload and retry", id)
		}
	}
	for _, id := range deletes {
		if !seen[id] {
			return errors.New("delete requires an edit input snapshot")
		}
	}
	for _, plan := range plans {
		if !plan.New && !seen[number(plan.Row["id"])] {
			return errors.New("update requires an edit input snapshot")
		}
	}
	return nil
}

func editInputError(w http.ResponseWriter, err error) {
	status := http.StatusBadRequest
	if errors.Is(err, sql.ErrNoRows) {
		status = http.StatusNotFound
	}
	httpError(w, status, err.Error())
}
