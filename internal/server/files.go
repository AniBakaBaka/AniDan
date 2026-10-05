// SPDX-License-Identifier: AGPL-3.0-only
package server

import (
	"context"
	"errors"
	"github.com/AniBakaBaka/AniDan/internal/danmaku"
	"github.com/AniBakaBaka/AniDan/internal/job"
	"github.com/AniBakaBaka/AniDan/internal/store"
	"path/filepath"
	"strings"
)

// Set only by a source-bound speculative fetch, never from request JSON.
type predownloadPublicationKey struct{}

// saveComments uses immutable content-addressed files: SQL atomically switches the
// pointer only after fsync. A crash can leave an unreferenced object but cannot
// truncate the previous XML or point the database to partially written content.
// Writable legacy/custom stable paths use a checksum-verified rollback journal;
// read-only external files are never overwritten and cleanup is explicit.
func (s *Server) saveComments(ctx context.Context, ep store.Row, comments []danmaku.Comment, preserveIfSmaller bool) error {
	if ep == nil {
		return errors.New("episode required")
	}
	if e := job.Checkpoint(ctx); e != nil {
		return e
	}
	src, e := s.Store.Get(ctx, "anime_sources", ep["source_id"])
	if e != nil || src == nil {
		return errors.New("source not found")
	}
	return s.saveCommentsSnapshot(ctx, ep, src, comments, preserveIfSmaller)
}

// saveCommentsSnapshot binds a download to the source identity observed before
// network work. The transaction validates both rows before publishing the file.
func (s *Server) saveCommentsSnapshot(ctx context.Context, ep, src store.Row, comments []danmaku.Comment, preserveIfSmaller bool) error {
	_, err := s.saveCommentsSnapshotOutcome(ctx, ep, src, comments, preserveIfSmaller)
	return err
}

func (s *Server) saveCommentsSnapshotOutcome(ctx context.Context, ep, src store.Row, comments []danmaku.Comment, preserveIfSmaller bool) (commentPublicationOutcome, error) {
	out := commentPublicationOutcome{FetchedCount: len(comments)}
	if ep == nil || src == nil {
		return out, errors.New("episode and source snapshots required")
	}
	pool, err := s.inspectCommentPool(ctx, ep)
	if err != nil {
		return out, err
	}
	ctx = context.WithValue(ctx, commentPoolSnapshotKey{}, pool)
	if !pool.missing && str(ep["danmaku_file_path"]) != "" {
		current := s.normalizeStoredPath(str(ep["danmaku_file_path"]))
		if immutableObjectName.MatchString(filepath.Base(current)) {
			ctx = context.WithValue(ctx, commentObjectCurrentPathKey{}, commentObjectCurrentPath{path: current})
		}
	}
	if preserveIfSmaller && pool.missing && len(comments) == 0 {
		return s.commitCommentObjectOutcome(ctx, ep, src, "", 0, true)
	}
	if preserveIfSmaller && !pool.missing && int64(len(comments)) < pool.count {
		// The locked recheck must still confirm the inspected bytes and rows.
		// A known retained response must not accumulate private staged copies.
		return s.commitCommentObjectOutcome(ctx, ep, src, "", len(comments), true)
	}
	// Set before entry: cancellation or a write failure need not establish how
	// much staging completed, so errors conservatively retain that uncertainty.
	out.StagingStarted = true
	path, e := s.writeCommentObject(ctx, number(src["anime_id"]), number(ep["id"]), comments)
	if e != nil {
		return out, e
	}
	return s.commitCommentObjectOutcome(ctx, ep, src, path, len(comments), preserveIfSmaller)
}

// normalizeStoredPath recognizes real paths before legacy aliases. This prevents
// /data/anidan/file being remapped to /data/anidan/anidan/file when the operator
// chooses a data directory nested below a legacy-looking prefix.
func (s *Server) normalizeStoredPath(path string) string {
	path = strings.ReplaceAll(path, "\\", "/")
	roots := append([]string{s.DataDir}, s.Config.ReadRoots...)
	roots = append(roots, s.Config.WriteRoots...)
	absolute, e := filepath.Abs(path)
	if e == nil {
		for _, root := range roots {
			base, e := filepath.Abs(root)
			if e != nil {
				continue
			}
			rel, e := filepath.Rel(base, absolute)
			if e == nil && filepath.IsLocal(rel) {
				return filepath.Clean(absolute)
			}
		}
	}
	for _, prefix := range []string{"/app/config/", "config/", "/data/"} {
		if strings.HasPrefix(path, prefix) {
			return filepath.Join(s.DataDir, strings.TrimPrefix(path, prefix))
		}
	}
	if !filepath.IsAbs(path) {
		return filepath.Join(s.DataDir, path)
	}
	return filepath.Clean(path)
}
