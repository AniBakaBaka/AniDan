// SPDX-License-Identifier: AGPL-3.0-only
package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/AniBakaBaka/AniDan/internal/danmaku"
	"github.com/AniBakaBaka/AniDan/internal/store"
)

var errStoredPoolUnreadable = errors.New("stored comments are unreadable; original bytes preserved; restore a verified copy before retrying")
var errStoredPoolChanged = errors.New("stored comments changed during publication; reload and retry")

type commentPoolSnapshotKey struct{}

type commentPoolState struct {
	missing bool
	count   int64
	info    os.FileInfo
	digest  [32]byte
}

type commentPoolHandle struct {
	root     *os.Root
	relative string
	file     *os.File
	info     os.FileInfo
}

func (h *commentPoolHandle) close() { h.file.Close(); h.root.Close() }

func poolUnavailable(reason string) error {
	return fmt.Errorf("%w: %s", errStoredPoolUnreadable, reason)
}

// Only an absent component reached through ordinary existing directories proves
// absence. A dangling/changed symlink, missing configured root, permission or
// non-directory error must not authorize a replacement.
func genuinePoolAbsence(root *os.Root, relative string) bool {
	prefix := ""
	parts := strings.Split(filepath.Clean(relative), string(filepath.Separator))
	for i, part := range parts {
		prefix = filepath.Join(prefix, part)
		info, err := root.Lstat(prefix)
		if err != nil {
			return errors.Is(err, os.ErrNotExist)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return false
		}
		if i < len(parts)-1 && !info.IsDir() {
			return false
		}
	}
	return false
}

func (s *Server) openCommentPool(path string) (*commentPoolHandle, bool, error) {
	if path == "" {
		return nil, true, nil
	}
	path = s.normalizeStoredPath(path)
	if err := s.guardWebMigrationPath(path); err != nil {
		return nil, false, poolUnavailable("stored path refers to private migration state")
	}
	roots := append([]string{s.DataDir}, s.Config.ReadRoots...)
	roots = append(roots, s.Config.WriteRoots...)
	for _, base := range roots {
		absolute, err := filepath.Abs(base)
		if err != nil {
			continue
		}
		rel, err := filepath.Rel(absolute, path)
		if err != nil || !filepath.IsLocal(rel) {
			continue
		}
		root, err := os.OpenRoot(absolute)
		if err != nil {
			return nil, false, poolUnavailable("cannot open configured read root")
		}
		before, err := root.Stat(rel)
		if err != nil {
			missing := errors.Is(err, os.ErrNotExist) && genuinePoolAbsence(root, rel)
			root.Close()
			if missing {
				return nil, true, nil
			}
			return nil, false, poolUnavailable("stored path cannot be resolved safely")
		}
		if !before.Mode().IsRegular() || before.Size() > danmaku.DefaultParseOptions().MaxBytes {
			root.Close()
			return nil, false, poolUnavailable("stored path is nonregular or exceeds the XML limit")
		}
		file, err := root.OpenFile(rel, regularReadFlags, 0)
		if err != nil {
			root.Close()
			return nil, false, poolUnavailable("stored file cannot be opened")
		}
		opened, err := file.Stat()
		if err != nil || !opened.Mode().IsRegular() || !os.SameFile(before, opened) || before.Size() != opened.Size() || !before.ModTime().Equal(opened.ModTime()) {
			file.Close()
			root.Close()
			return nil, false, errStoredPoolChanged
		}
		return &commentPoolHandle{root: root, relative: rel, file: file, info: opened}, false, nil
	}
	return nil, false, poolUnavailable("stored path is outside configured read roots")
}

func (h *commentPoolHandle) stable(bytes int64) bool {
	after, err := h.file.Stat()
	if err != nil {
		return false
	}
	named, err := h.root.Stat(h.relative)
	if err != nil {
		return false
	}
	return named.Mode().IsRegular() && os.SameFile(h.info, after) && os.SameFile(h.info, named) && h.info.Size() == after.Size() && h.info.Size() == named.Size() && h.info.Size() == bytes && h.info.ModTime().Equal(after.ModTime()) && h.info.ModTime().Equal(named.ModTime())
}

type poolCountingReader struct {
	r io.Reader
	n int64
}

func (r *poolCountingReader) Read(p []byte) (int, error) {
	n, err := r.r.Read(p)
	r.n += int64(n)
	return n, err
}

func (s *Server) inspectCommentPool(ctx context.Context, ep store.Row) (commentPoolState, error) {
	s.fileMu.RLock()
	defer s.fileMu.RUnlock()
	if s.fileFault.Load() {
		return commentPoolState{}, errors.New("storage recovery required")
	}
	return s.inspectCommentPoolLocked(ctx, ep)
}

func (s *Server) inspectCommentPoolLocked(ctx context.Context, ep store.Row) (commentPoolState, error) {
	if err := ctx.Err(); err != nil {
		return commentPoolState{}, err
	}
	h, missing, err := s.openCommentPool(str(ep["danmaku_file_path"]))
	if err != nil {
		return commentPoolState{}, err
	}
	if missing {
		return commentPoolState{missing: true}, nil
	}
	defer h.close()
	hash := sha256.New()
	reader := &poolCountingReader{r: contextReader{ctx, h.file}}
	count, err := danmaku.InspectWithOptions(io.TeeReader(reader, hash), danmaku.DefaultParseOptions())
	if err != nil {
		if ctx.Err() != nil {
			return commentPoolState{}, ctx.Err()
		}
		return commentPoolState{}, poolUnavailable("XML validation failed")
	}
	if !h.stable(reader.n) {
		return commentPoolState{}, errStoredPoolChanged
	}
	state := commentPoolState{count: int64(count), info: h.info}
	copy(state.digest[:], hash.Sum(nil))
	base := filepath.Base(s.normalizeStoredPath(str(ep["danmaku_file_path"])))
	if immutableObjectName.MatchString(base) {
		expected := strings.TrimSuffix(base[strings.LastIndex(base, "-")+1:], ".xml")
		if hex.EncodeToString(state.digest[:]) != expected {
			return commentPoolState{}, poolUnavailable("content-addressed filename checksum mismatch")
		}
	}
	return state, nil
}

// The snapshot already established XML readability and count. Rechecking the
// same anchored bytes avoids another XML parse while holding the write lock.
func (s *Server) recheckCommentPoolLocked(ctx context.Context, ep store.Row, want commentPoolState) (commentPoolState, error) {
	if err := ctx.Err(); err != nil {
		return commentPoolState{}, err
	}
	h, missing, err := s.openCommentPool(str(ep["danmaku_file_path"]))
	if err != nil {
		return commentPoolState{}, err
	}
	if missing {
		if want.missing {
			return want, nil
		}
		return commentPoolState{}, errStoredPoolChanged
	}
	defer h.close()
	if want.missing || !os.SameFile(want.info, h.info) || want.info.Size() != h.info.Size() || !want.info.ModTime().Equal(h.info.ModTime()) {
		return commentPoolState{}, errStoredPoolChanged
	}
	hash := sha256.New()
	n, err := io.Copy(hash, contextReader{ctx, io.LimitReader(h.file, danmaku.DefaultParseOptions().MaxBytes+1)})
	if err != nil {
		if ctx.Err() != nil {
			return commentPoolState{}, ctx.Err()
		}
		return commentPoolState{}, poolUnavailable("stored file read failed")
	}
	var digest [32]byte
	copy(digest[:], hash.Sum(nil))
	if !h.stable(n) || digest != want.digest {
		return commentPoolState{}, errStoredPoolChanged
	}
	return want, nil
}
