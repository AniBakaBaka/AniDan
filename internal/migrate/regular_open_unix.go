//go:build aix || darwin || dragonfly || freebsd || illumos || linux || netbsd || openbsd || solaris

// SPDX-License-Identifier: AGPL-3.0-only
package migrate

import (
	"errors"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// O_NONBLOCK keeps a regular-to-FIFO swap from blocking before fstat can reject it.
const regularReadFlags = os.O_RDONLY | unix.O_NONBLOCK
const regularNoFollow = unix.O_NOFOLLOW

func openFileNoFollow(root *os.Root, name string, flags int, perm os.FileMode) (*os.File, error) {
	if name == "." || !filepath.IsLocal(name) || filepath.Base(name) != name {
		return nil, errors.New("no-follow open requires a destination leaf")
	}
	// Root.OpenFile resolves symlinks itself, including when O_NOFOLLOW was
	// supplied. Use openat on its anchored directory to reject the final link.
	dir, err := root.Open(".")
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	fd, err := unix.Openat(int(dir.Fd()), name, flags|unix.O_NOFOLLOW|unix.O_CLOEXEC, uint32(perm.Perm()))
	if err != nil {
		return nil, &os.PathError{Op: "openat", Path: name, Err: err}
	}
	return os.NewFile(uintptr(fd), filepath.Join(root.Name(), name)), nil
}
