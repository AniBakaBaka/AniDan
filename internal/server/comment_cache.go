// SPDX-License-Identifier: AGPL-3.0-only
package server

import (
	"context"
	"fmt"
	"github.com/AniBakaBaka/AniDan/internal/danmaku"
	"io"
	"os"
	"strings"
)

func (s *Server) commentCacheKey(path string, info os.FileInfo) string {
	return commentFileIdentity(info) + "\x00" + s.normalizeStoredPath(path) + "\x00" + fmt.Sprintf("%d:%d", info.Size(), info.ModTime().UnixNano())
}

// Call after a successful publication/removal, while holding fileMu whenever the
// operation also changes a live SQL pointer. Identity prefixes catch cached path
// aliases and hardlinks; the normalized path also evicts prior inode versions.
// Output caching is content-addressed and needs no global invalidation.
func (s *Server) invalidateCommentFile(path string, before, after os.FileInfo) {
	if s.parsedCache == nil {
		return
	}
	pathPart := "\x00" + s.normalizeStoredPath(path) + "\x00"
	ids := []string{}
	for _, info := range []os.FileInfo{before, after} {
		if id := commentFileIdentity(info); id != "" {
			ids = append(ids, id+"\x00")
		}
	}
	s.parsedCache.Invalidate(func(key string) bool {
		if strings.Contains(key, pathPart) {
			return true
		}
		for _, prefix := range ids {
			if strings.HasPrefix(key, prefix) {
				return true
			}
		}
		return false
	})
}

// parseCommentFile expects the caller to hold fileMu.RLock for live files. The
// cache loader runs synchronously inside that read snapshot, before a writer can
// publish and invalidate. Reader is explicit so its cancellation/bounds remain
// testable without replacing the production parser or adding global test hooks.
func (s *Server) parseCommentFile(ctx context.Context, path string, info os.FileInfo, reader io.Reader) ([]danmaku.Comment, error) {
	key := s.commentCacheKey(path, info)
	load := func() ([]danmaku.Comment, int64, error) {
		comments, e := danmaku.Parse(contextReader{ctx, reader})
		var size int64
		for _, c := range comments {
			size += int64(64 + len(c.P) + len(c.M))
		}
		return comments, size, e
	}
	if s.parsedCache == nil {
		v, _, e := load()
		return v, e
	}
	return s.parsedCache.Do(ctx, key, load)
}
