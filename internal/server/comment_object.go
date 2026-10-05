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

	"github.com/AniBakaBaka/AniDan/internal/danmaku"
	"github.com/AniBakaBaka/AniDan/internal/job"
)

// Supplied only after the saver has inspected a readable current immutable pool.
// Only already-existing managed objects matching the incoming basename can reuse.
type commentObjectCurrentPathKey struct{}
type commentObjectCurrentPath struct{ path string }

// Existing content-addressed objects are never replaced. The hash is checked
// against bytes from an anchored regular-file handle, not trusted from its name.
func verifyCommentObject(ctx context.Context, root *os.Root, relative, expected string) (os.FileInfo, error) {
	before, err := root.Lstat(relative)
	if err != nil {
		return nil, err
	}
	if !before.Mode().IsRegular() {
		return nil, errors.New("existing comment object is not a regular file; original preserved")
	}
	if before.Size() > danmaku.DefaultParseOptions().MaxBytes {
		return nil, errors.New("existing comment object exceeds the XML limit; original preserved")
	}
	f, err := root.OpenFile(relative, regularReadFlags, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !opened.Mode().IsRegular() || !os.SameFile(before, opened) {
		return nil, errors.New("comment object changed while opening; original preserved")
	}
	h := sha256.New()
	n, err := io.Copy(h, contextReader{ctx, io.LimitReader(f, danmaku.DefaultParseOptions().MaxBytes+1)})
	if err != nil {
		return nil, err
	}
	after, err := f.Stat()
	if err != nil {
		return nil, err
	}
	named, err := root.Lstat(relative)
	if err != nil {
		return nil, err
	}
	if !named.Mode().IsRegular() || !os.SameFile(opened, named) || named.Size() != after.Size() || !named.ModTime().Equal(after.ModTime()) || opened.Size() != after.Size() || !opened.ModTime().Equal(after.ModTime()) || n != after.Size() || n > danmaku.DefaultParseOptions().MaxBytes || hex.EncodeToString(h.Sum(nil)) != expected {
		return nil, errors.New("comment object checksum or identity mismatch; original preserved")
	}
	return after, nil
}

func (s *Server) writeCommentObject(ctx context.Context, animeID, episodeID int64, comments []danmaku.Comment) (string, error) {
	if err := job.Checkpoint(ctx); err != nil {
		return "", err
	}
	root, err := os.OpenRoot(s.DataDir)
	if err != nil {
		return "", err
	}
	defer root.Close()
	dir := filepath.Join("danmaku", fmt.Sprint(animeID))
	if err = root.MkdirAll(dir, 0700); err != nil {
		return "", err
	}
	tmp := filepath.Join(dir, ".object-"+randomID()+".tmp")
	f, err := root.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return "", err
	}
	defer root.Remove(tmp)
	defer f.Close()
	h := sha256.New()
	s.commentObjectSerializations.Add(1)
	// danmaku.Write enforces the same 64MiB encoded byte bound as the reader.
	if err = danmaku.Write(io.MultiWriter(f, h), comments); err != nil {
		return "", err
	}
	if err = f.Sync(); err != nil {
		return "", err
	}
	staged, err := f.Stat()
	if err != nil {
		return "", err
	}
	if err = job.Checkpoint(ctx); err != nil {
		return "", err
	}
	digest := hex.EncodeToString(h.Sum(nil))
	basename := fmt.Sprintf("%d-%s.xml", episodeID, digest)
	canonical := filepath.Join(dir, basename)
	reuse := func(relative string) (string, error) {
		if _, err := verifyCommentObject(ctx, root, relative, digest); err != nil {
			return "", err
		}
		if err := syncAnchoredDirectories(root, filepath.Dir(relative)); err != nil {
			return "", err
		}
		return filepath.Join(s.DataDir, relative), nil
	}
	if current, ok := ctx.Value(commentObjectCurrentPathKey{}).(commentObjectCurrentPath); ok && filepath.Base(current.path) == basename {
		managed, err := filepath.Abs(filepath.Join(s.DataDir, "danmaku"))
		if err != nil {
			return "", err
		}
		candidate, err := filepath.Abs(current.path)
		if err != nil {
			return "", err
		}
		inside, err := filepath.Rel(managed, candidate)
		if err == nil && inside != "." && filepath.IsLocal(inside) {
			// Never recreate a disappeared current object: its validated state
			// must still exist, or the save fails without activating any alias.
			return reuse(filepath.Join("danmaku", inside))
		}
	}
	if _, err := root.Lstat(canonical); err == nil {
		return reuse(canonical)
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	// A different episode may already refer (directly or through a dangling
	// link) to the future canonical filename. Fresh private generation paths
	// cannot activate that reference before the locked SQL publication.
	base := filepath.Join(dir, "objects")
	if err = root.MkdirAll(base, 0700); err != nil {
		return "", err
	}
	dir = filepath.Join(base, randomID())
	if err = root.Mkdir(dir, 0700); err != nil {
		return "", err
	}
	relative := filepath.Join(dir, basename)
	// Link is atomic and fails if any entry already exists. Rename would destroy
	// a damaged existing object before the later SQL count/identity decision.
	if err = root.Link(tmp, relative); err != nil {
		return "", err
	}
	// This inode was just streamed, hashed and fsynced through f. Existing
	// current/canonical reuse above is separately verified byte-for-byte.
	after, err := root.Lstat(relative)
	if err != nil {
		return "", err
	}
	current, err := f.Stat()
	if err != nil {
		return "", err
	}
	if !after.Mode().IsRegular() || !os.SameFile(staged, after) || !os.SameFile(staged, current) || staged.Size() != current.Size() || staged.Size() != after.Size() || !staged.ModTime().Equal(current.ModTime()) || !staged.ModTime().Equal(after.ModTime()) {
		return "", errors.New("staged comment object identity changed")
	}
	s.invalidateCommentFile(filepath.Join(s.DataDir, relative), nil, after)
	if err = syncAnchoredDirectories(root, dir); err != nil {
		return "", err
	}
	return filepath.Join(s.DataDir, relative), nil
}
