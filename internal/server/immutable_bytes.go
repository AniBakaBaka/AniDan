// SPDX-License-Identifier: AGPL-3.0-only
package server

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
)

// verifyImmutableBytes reads through a pinned regular-file descriptor. The
// expected bytes already belong to the caller; comparing small chunks avoids
// allocating a second complete XML/image payload for reuse or collision checks.
func verifyImmutableBytes(ctx context.Context, root *os.Root, name string, want []byte, limit int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	before, err := root.Lstat(name)
	if err != nil {
		return err
	}
	if !before.Mode().IsRegular() || before.Size() != int64(len(want)) || before.Size() > limit {
		return errors.New("existing immutable object is not a matching bounded regular file")
	}
	f, err := root.OpenFile(name, regularReadFlags, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil {
		return err
	}
	if !opened.Mode().IsRegular() || !os.SameFile(before, opened) || opened.Size() != before.Size() {
		return errors.New("immutable object changed while opening")
	}
	var buffer [32 << 10]byte
	reader := contextReader{ctx, f}
	for offset := 0; offset < len(want); {
		n := min(len(buffer), len(want)-offset)
		if _, err = io.ReadFull(reader, buffer[:n]); err != nil {
			return err
		}
		if !bytes.Equal(buffer[:n], want[offset:offset+n]) {
			return errors.New("content-addressed destination contains different bytes")
		}
		offset += n
	}
	if n, err := reader.Read(buffer[:1]); n != 0 || err != io.EOF {
		if err != nil {
			return err
		}
		return errors.New("immutable object grew during verification")
	}
	after, err := f.Stat()
	if err != nil {
		return err
	}
	named, err := root.Lstat(name)
	if err != nil {
		return err
	}
	if !named.Mode().IsRegular() || !os.SameFile(opened, named) || opened.Size() != after.Size() || !opened.ModTime().Equal(after.ModTime()) || named.Size() != after.Size() || !named.ModTime().Equal(after.ModTime()) {
		return errors.New("immutable object identity changed during verification")
	}
	return ctx.Err()
}

// publishImmutableBytes shares byte publication, not path or content policy.
// The caller owns root, the expected basename, and its XML/image-specific limit.
// Existing entries (including symlinks) are never replaced.
func publishImmutableBytes(ctx context.Context, root *os.Root, name string, data []byte, limit int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if limit <= 0 || int64(len(data)) > limit || !filepath.IsLocal(name) || filepath.Base(name) != name || name == "." {
		return errors.New("invalid immutable object name or byte limit")
	}
	syncDirectory := func() error {
		dir, err := root.Open(".")
		if err != nil {
			return err
		}
		defer dir.Close()
		return dir.Sync()
	}
	if _, err := root.Lstat(name); err == nil {
		if err = verifyImmutableBytes(ctx, root, name, data, limit); err != nil {
			return err
		}
		if err = syncDirectory(); err != nil {
			return err
		}
		return ctx.Err()
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	tmp := ".immutable-object-" + randomID()
	f, err := root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	defer root.Remove(tmp)
	defer f.Close()
	n, err := io.Copy(f, contextReader{ctx, bytes.NewReader(data)})
	if err != nil {
		return err
	}
	if n != int64(len(data)) {
		return io.ErrShortWrite
	}
	if err = f.Sync(); err != nil {
		return err
	}
	staged, err := f.Stat()
	if err != nil {
		return err
	}
	if !staged.Mode().IsRegular() || staged.Size() != n {
		return errors.New("new immutable object changed while writing")
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	if err = root.Link(tmp, name); err != nil {
		if !errors.Is(err, os.ErrExist) {
			return err
		}
		if err = verifyImmutableBytes(ctx, root, name, data, limit); err != nil {
			return err
		}
	} else {
		named, err := root.Lstat(name)
		if err != nil {
			return err
		}
		current, err := f.Stat()
		if err != nil {
			return err
		}
		if !named.Mode().IsRegular() || !os.SameFile(staged, named) || !os.SameFile(staged, current) || named.Size() != staged.Size() || current.Size() != staged.Size() || !named.ModTime().Equal(staged.ModTime()) || !current.ModTime().Equal(staged.ModTime()) {
			return errors.New("new immutable object identity changed during publication")
		}
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = root.Remove(tmp); err != nil {
		return err
	}
	if err = syncDirectory(); err != nil {
		return err
	}
	return ctx.Err()
}

// Managed outputs are always anchored from configured DataDir. A child
// directory cannot redirect publication outside that root. Source/read-root
// aliases have a separate policy in localOpenAllowed.
func (s *Server) writeManagedBytes(ctx context.Context, relative string, data []byte, limit int64) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if !filepath.IsLocal(relative) || filepath.Clean(relative) == "." || limit <= 0 || int64(len(data)) > limit {
		return "", errors.New("invalid managed object path")
	}
	root, err := os.OpenRoot(s.DataDir)
	if err != nil {
		return "", err
	}
	defer root.Close()
	parent := filepath.Dir(relative)
	if err = root.MkdirAll(parent, 0700); err != nil {
		return "", err
	}
	dir, err := root.OpenRoot(parent)
	if err != nil {
		return "", err
	}
	defer dir.Close()
	if err = publishImmutableBytes(ctx, dir, filepath.Base(relative), data, limit); err != nil {
		return "", err
	}
	// Newly created parent directories must also be durable before the caller
	// commits a SQL reference to the object. The leaf writer syncs its own root.
	if err = syncAnchoredDirectories(root, parent); err != nil {
		return "", err
	}
	if err = ctx.Err(); err != nil {
		return "", err
	}
	return filepath.Abs(filepath.Join(s.DataDir, relative))
}
