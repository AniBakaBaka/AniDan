// SPDX-License-Identifier: AGPL-3.0-only
package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/AniBakaBaka/AniDan/internal/danmaku"
	"github.com/AniBakaBaka/AniDan/internal/store"
)

const localReviewFileLimit = 32 << 20

// These fields are evidence for the coordinator's snapshot, not a public DTO.
// The identities deliberately remain separate from the content digest: replacing
// a file with an identical copy invalidates the reviewed snapshot.
type localReviewFiles struct {
	SourcePath, PoolPath         string
	Bytes, Count                 int64
	SHA256                       string
	SourceIdentity, PoolIdentity string
	SourceModified, PoolModified string
}

type localReviewPoolDirectory struct {
	parent, child *os.Root
	name          string
	info          os.FileInfo
}

type localReviewPool struct {
	*commentPoolHandle
	path        string
	roots       []*os.Root
	directories []localReviewPoolDirectory
}

func (h *localReviewPool) close() {
	if h.commentPoolHandle != nil {
		h.file.Close()
	}
	for i := len(h.roots) - 1; i >= 0; i-- {
		h.roots[i].Close()
	}
}

func localReviewRegular(info os.FileInfo) bool {
	return info != nil && info.Mode().IsRegular() && info.Size() > 0 && info.Size() <= localReviewFileLimit && commentFileIdentity(info) != ""
}

func localReviewSameFile(a, b os.FileInfo) bool {
	return localReviewRegular(a) && localReviewRegular(b) && os.SameFile(a, b) && commentFileIdentity(a) == commentFileIdentity(b) && a.Size() == b.Size() && a.ModTime().Equal(b.ModTime())
}

// Source aliases use localOpenAllowed's existing policy. Managed pool paths
// instead require ordinary directories all the way below the configured root.
// Retaining each opened directory lets the final check detect a replaced named
// ancestor without reopening a path through an unchecked symlink.
func (s *Server) localOpenReviewPool(ctx context.Context, path string) (_ *localReviewPool, err error) {
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	if path == "" {
		return nil, errLocalImportBindingReview
	}
	base, err := filepath.Abs(s.DataDir)
	if err != nil {
		return nil, errLocalImportBindingReview
	}
	path, err = filepath.Abs(s.normalizeStoredPath(path))
	if err != nil {
		return nil, errLocalImportBindingReview
	}
	rel, err := filepath.Rel(base, path)
	if err != nil || !filepath.IsLocal(rel) || rel == "." {
		return nil, errLocalImportBindingReview
	}
	root, err := os.OpenRoot(base)
	if err != nil {
		return nil, errLocalImportBindingReview
	}
	h := &localReviewPool{path: path, roots: []*os.Root{root}}
	defer func() {
		if err != nil {
			h.close()
		}
	}()
	parts := strings.Split(rel, string(filepath.Separator))
	for _, part := range parts[:len(parts)-1] {
		if err = ctx.Err(); err != nil {
			return nil, err
		}
		before, e := root.Lstat(part)
		if e != nil || !before.IsDir() || before.Mode()&os.ModeSymlink != 0 || commentFileIdentity(before) == "" {
			return nil, errLocalImportBindingReview
		}
		child, e := root.OpenRoot(part)
		if e != nil {
			return nil, errLocalImportBindingReview
		}
		h.roots = append(h.roots, child)
		opened, openErr := child.Stat(".")
		named, nameErr := root.Lstat(part)
		if openErr != nil || nameErr != nil || !opened.IsDir() || !named.IsDir() || named.Mode()&os.ModeSymlink != 0 || !os.SameFile(before, opened) || !os.SameFile(opened, named) {
			return nil, errLocalImportBindingReview
		}
		h.directories = append(h.directories, localReviewPoolDirectory{root, child, part, opened})
		root = child
	}
	name := parts[len(parts)-1]
	before, err := root.Lstat(name)
	if err != nil || !localReviewRegular(before) {
		return nil, errLocalImportBindingReview
	}
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	f, err := root.OpenFile(name, regularReadFlags, 0)
	if err != nil {
		return nil, errLocalImportBindingReview
	}
	h.commentPoolHandle = &commentPoolHandle{root: root, relative: name, file: f}
	opened, openErr := f.Stat()
	named, nameErr := root.Lstat(name)
	if openErr != nil || nameErr != nil || !localReviewSameFile(before, opened) || !localReviewSameFile(opened, named) {
		return nil, errLocalImportBindingReview
	}
	h.info = opened
	return h, nil
}

func (h *localReviewPool) stable(size int64) bool {
	for _, dir := range h.directories {
		named, nameErr := dir.parent.Lstat(dir.name)
		opened, openErr := dir.child.Stat(".")
		if nameErr != nil || openErr != nil || !named.IsDir() || named.Mode()&os.ModeSymlink != 0 || !os.SameFile(dir.info, named) || !os.SameFile(dir.info, opened) {
			return false
		}
	}
	named, err := h.root.Lstat(h.relative)
	return err == nil && localReviewSameFile(h.info, named) && h.commentPoolHandle.stable(size)
}

// Reopening the named source uses exactly the allowed-source alias policy. Both
// its resolved assignment and file identity must still match the pinned reader.
func (s *Server) localReviewSourceStable(original, resolved string, f *os.File, before os.FileInfo) bool {
	after, err := f.Stat()
	if err != nil || !localReviewSameFile(before, after) {
		return false
	}
	now, err := s.localAllowedPath(original, false)
	if err != nil || now != resolved {
		return false
	}
	named, err := s.localOpenAllowed(now)
	if err != nil {
		return false
	}
	defer named.Close()
	info, err := named.Stat()
	return err == nil && localReviewSameFile(before, info)
}

// localReviewFilesLocked performs bounded, read-only evidence collection. Its
// caller owns importMu followed by fileMu; this helper never takes locks, opens
// SQL transactions, repairs missing files, or publishes any bytes.
func (s *Server) localReviewFilesLocked(ctx context.Context, row, ep store.Row) (out localReviewFiles, err error) {
	defer func() {
		if err != nil {
			out = localReviewFiles{}
			if cancelled := ctx.Err(); cancelled != nil {
				err = cancelled
			} else {
				err = errLocalImportBindingReview
			}
		}
	}()
	if err = ctx.Err(); err != nil {
		return out, err
	}
	if s.fileFault.Load() || str(row["file_path"]) == "" {
		return out, errLocalImportBindingReview
	}
	countFields, err := localBindingFields("episode", ep, "comment_count")
	if err != nil {
		return out, err
	}
	wantCount, ok := countFields["comment_count"].(int64)
	if !ok || wantCount <= 0 {
		return out, errLocalImportBindingReview
	}
	original := str(row["file_path"])
	path, err := s.localAllowedPath(original, false)
	if err != nil {
		return out, err
	}
	source, err := s.localOpenAllowed(path)
	if err != nil {
		return out, err
	}
	defer source.Close()
	info, err := source.Stat()
	if err != nil || !localReviewRegular(info) {
		return out, errLocalImportBindingReview
	}
	pool, err := s.localOpenReviewPool(ctx, str(ep["danmaku_file_path"]))
	if err != nil {
		return out, err
	}
	defer pool.close()
	if info.Size() != pool.info.Size() {
		return out, errLocalImportBindingReview
	}
	content := make([]byte, int(info.Size()))
	reader := contextReader{ctx, source}
	if _, err = io.ReadFull(reader, content); err != nil {
		return out, err
	}
	var buffer [32 << 10]byte
	if n, e := reader.Read(buffer[:1]); n != 0 || e != io.EOF {
		return out, errLocalImportBindingReview
	}
	options := danmaku.DefaultParseOptions()
	options.MaxBytes = localReviewFileLimit
	count, err := danmaku.InspectWithOptions(contextReader{ctx, bytes.NewReader(content)}, options)
	if err != nil || count <= 0 || int64(count) != wantCount {
		return out, errLocalImportBindingReview
	}
	reader = contextReader{ctx, pool.file}
	for offset := 0; offset < len(content); {
		n := min(len(buffer), len(content)-offset)
		if _, err = io.ReadFull(reader, buffer[:n]); err != nil {
			return out, err
		}
		if !bytes.Equal(buffer[:n], content[offset:offset+n]) {
			return out, errLocalImportBindingReview
		}
		offset += n
	}
	if n, e := reader.Read(buffer[:1]); n != 0 || e != io.EOF {
		return out, errLocalImportBindingReview
	}
	digest := sha256.Sum256(content)
	hash := hex.EncodeToString(digest[:])
	base := filepath.Base(pool.path)
	if immutableObjectName.MatchString(base) && strings.TrimSuffix(base[strings.LastIndex(base, "-")+1:], ".xml") != hash {
		return out, errLocalImportBindingReview
	}
	if !pool.stable(int64(len(content))) || !s.localReviewSourceStable(original, path, source, info) {
		return out, errLocalImportBindingReview
	}
	if err = ctx.Err(); err != nil {
		return out, err
	}
	return localReviewFiles{SourcePath: path, PoolPath: pool.path, Bytes: info.Size(), Count: int64(count), SHA256: hash, SourceIdentity: commentFileIdentity(info), PoolIdentity: commentFileIdentity(pool.info), SourceModified: info.ModTime().UTC().Format(time.RFC3339Nano), PoolModified: pool.info.ModTime().UTC().Format(time.RFC3339Nano)}, nil
}

// localReviewFileVersionsLocked is the cheap final check after the coordinator
// obtains its SQL row locks. Full byte evidence must have been collected before
// that transaction, with importMu and fileMu continuously held. No payload read
// or parse occurs here, and no file handles are retained in serialized evidence.
// This catches observable path, inode, size and mtime changes during the SQL
// wait; it does not serialize arbitrary external writers or detect malicious
// same-stat rewrites after the full evidence read.
func (s *Server) localReviewFileVersionsLocked(ctx context.Context, row, ep store.Row, want localReviewFiles) (err error) {
	defer func() {
		if err != nil {
			if cancelled := ctx.Err(); cancelled != nil {
				err = cancelled
			} else {
				err = errLocalImportBindingReview
			}
		}
	}()
	if err = ctx.Err(); err != nil {
		return err
	}
	if s.fileFault.Load() || want.Bytes <= 0 || want.Bytes > localReviewFileLimit || want.Count <= 0 || want.SourceIdentity == "" || want.PoolIdentity == "" || str(row["file_path"]) == "" {
		return errLocalImportBindingReview
	}
	countFields, err := localBindingFields("episode", ep, "comment_count")
	if err != nil || countFields["comment_count"] != want.Count {
		return errLocalImportBindingReview
	}
	original := str(row["file_path"])
	path, err := s.localAllowedPath(original, false)
	if err != nil || path != want.SourcePath {
		return errLocalImportBindingReview
	}
	source, err := s.localOpenAllowed(path)
	if err != nil {
		return err
	}
	defer source.Close()
	info, err := source.Stat()
	if err != nil || !localReviewRegular(info) || info.Size() != want.Bytes || commentFileIdentity(info) != want.SourceIdentity || info.ModTime().UTC().Format(time.RFC3339Nano) != want.SourceModified {
		return errLocalImportBindingReview
	}
	pool, err := s.localOpenReviewPool(ctx, str(ep["danmaku_file_path"]))
	if err != nil {
		return err
	}
	defer pool.close()
	if pool.path != want.PoolPath || pool.info.Size() != want.Bytes || commentFileIdentity(pool.info) != want.PoolIdentity || pool.info.ModTime().UTC().Format(time.RFC3339Nano) != want.PoolModified || !pool.stable(want.Bytes) || !s.localReviewSourceStable(original, path, source, info) {
		return errLocalImportBindingReview
	}
	return ctx.Err()
}
