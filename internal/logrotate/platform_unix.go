//go:build darwin || dragonfly || freebsd || illumos || linux || netbsd || openbsd || solaris

// SPDX-License-Identifier: AGPL-3.0-only
package logrotate

import (
	"errors"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

func openAnchored(root *os.Root, name string, flags int, mode os.FileMode) (*os.File, error) {
	// Root.OpenFile can resolve symlinks even when O_NOFOLLOW was supplied.
	// Use openat on a descriptor obtained from the pinned root for flat names.
	d, err := root.Open(".")
	if err != nil {
		return nil, err
	}
	defer d.Close()
	fd, err := unix.Openat(int(d.Fd()), name, flags|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, uint32(mode.Perm()))
	if err != nil {
		return nil, &os.PathError{Op: "openat", Path: name, Err: err}
	}
	return os.NewFile(uintptr(fd), name), nil
}

func lockExclusive(f *os.File) error {
	return unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
}

func checkDirectory(info os.FileInfo) error {
	if info.Mode().Perm()&0022 != 0 {
		return errors.New("log directory must not be group- or world-writable")
	}
	return nil
}

func checkSingleLink(_ *os.File, info os.FileInfo) error {
	if info.Mode().Perm()&0022 != 0 {
		return errors.New("log files must not be group- or world-writable")
	}
	s, ok := info.Sys().(*syscall.Stat_t)
	if !ok || s.Nlink != 1 {
		return errors.New("log files must have exactly one hard link")
	}
	return nil
}
