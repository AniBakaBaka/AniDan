// SPDX-License-Identifier: AGPL-3.0-only
package migrate

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// A bare os.OpenRoot opens its final component before checking its type. Keep
// the trailing dot (do not use filepath.Join/Clean) so Unix must traverse name
// as a directory, rather than blocking while opening a substituted FIFO.
func openDirectoryRoot(name string) (*os.Root, error) {
	return os.OpenRoot(name + string(filepath.Separator) + ".")
}

func openDirectoryWithin(root *os.Root, name string) (*os.Root, error) {
	return root.OpenRoot(name + string(filepath.Separator) + ".")
}

// openRegularSource permits symlinks only within the already anchored source.
func openRegularSource(ctx context.Context, root *os.Root, name string) (*os.File, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	before, err := root.Stat(name)
	if err != nil {
		return nil, err
	}
	if !before.Mode().IsRegular() {
		return nil, fmt.Errorf("not a regular file: %s", name)
	}
	return openRegularChecked(ctx, root, name, regularReadFlags, nil)
}

// openRegularDestination rejects symlink components and anchors each parent
// before opening the leaf. A parent replaced during traversal cannot redirect
// the read, even to a different directory inside the allowed root.
func openRegularDestination(ctx context.Context, root *os.Root, name string) (*os.File, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !filepath.IsLocal(name) {
		return nil, errors.New("invalid migration destination")
	}
	pieces := strings.Split(filepath.Clean(name), string(filepath.Separator))
	parent := root
	for _, piece := range pieces[:len(pieces)-1] {
		before, err := parent.Lstat(piece)
		if err != nil {
			if parent != root {
				parent.Close()
			}
			return nil, err
		}
		if !before.IsDir() {
			if parent != root {
				parent.Close()
			}
			return nil, fmt.Errorf("non-directory destination component: %s", name)
		}
		next, err := openDirectoryWithin(parent, piece)
		if parent != root {
			parent.Close()
		}
		if err != nil {
			return nil, err
		}
		after, err := next.Stat(".")
		if err != nil || !os.SameFile(before, after) {
			next.Close()
			return nil, fmt.Errorf("destination directory changed: %s", name)
		}
		parent = next
	}
	if parent != root {
		defer parent.Close()
	}
	leaf := pieces[len(pieces)-1]
	before, err := parent.Lstat(leaf)
	if err != nil {
		return nil, err
	}
	if !before.Mode().IsRegular() {
		return nil, fmt.Errorf("nonregular destination rejected: %s", name)
	}
	return openRegularChecked(ctx, parent, leaf, regularReadFlags|regularNoFollow, before)
}

// Every successful open is checked before any read. The optional identity check
// also protects destination reads on platforms without O_NOFOLLOW.
func openRegularChecked(ctx context.Context, root *os.Root, name string, flags int, before os.FileInfo) (*os.File, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var f *os.File
	var err error
	if before != nil {
		f, err = openFileNoFollow(root, name, flags, 0)
	} else {
		f, err = root.OpenFile(name, flags, 0)
	}
	if err != nil {
		return nil, err
	}
	after, err := f.Stat()
	if err == nil && !after.Mode().IsRegular() {
		err = fmt.Errorf("not a regular file: %s", name)
	}
	if err == nil && before != nil && !os.SameFile(before, after) {
		err = fmt.Errorf("destination changed while opening: %s", name)
	}
	if err == nil && before != nil {
		// Retain an explicit pathname check for platforms without O_NOFOLLOW.
		current, statErr := root.Lstat(name)
		if statErr != nil {
			err = statErr
		} else if !current.Mode().IsRegular() || !os.SameFile(current, after) {
			err = fmt.Errorf("destination changed while opening: %s", name)
		}
	}
	if err == nil {
		err = ctx.Err()
	}
	if err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

func openRegularPath(ctx context.Context, name string) (*os.File, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// Keep support for a snapshot supplied through a symlink. Anchor its resolved
	// parent; a later leaf swap still goes through the descriptor type check.
	resolved, err := filepath.EvalSymlinks(name)
	if err != nil {
		return nil, err
	}
	root, err := openDirectoryRoot(filepath.Dir(resolved))
	if err != nil {
		return nil, err
	}
	defer root.Close()
	return openRegularSource(ctx, root, filepath.Base(resolved))
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}

func rejectNonregularIfExists(name string) error {
	info, err := os.Lstat(name)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("nonregular destination rejected: %s", name)
	}
	return nil
}
