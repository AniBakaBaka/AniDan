//go:build linux

// SPDX-License-Identifier: AGPL-3.0-only
package migrate

import (
	"fmt"
	"golang.org/x/sys/unix"
	"os"
)

// RENAME_NOREPLACE gives atomic publication without replacing even an empty
// concurrently-created directory. Staging is a sibling on the same filesystem.
func publishDirectory(from, to string) error {
	return unix.Renameat2(unix.AT_FDCWD, from, unix.AT_FDCWD, to, unix.RENAME_NOREPLACE)
}

func acquireStageLock(stage string) (func(), error) {
	root, e := openDirectoryRoot(stage)
	if e != nil {
		return nil, e
	}
	defer root.Close()
	if st, err := root.Lstat(".migration-lock"); err == nil && !st.Mode().IsRegular() {
		return nil, fmt.Errorf("migration lock is not a regular file")
	} else if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	f, e := openFileNoFollow(root, ".migration-lock", os.O_CREATE|os.O_RDWR|unix.O_NONBLOCK, 0600)
	if e != nil {
		return nil, e
	}
	st, e := f.Stat()
	if e != nil || !st.Mode().IsRegular() {
		f.Close()
		if e != nil {
			return nil, e
		}
		return nil, fmt.Errorf("migration lock is not a regular file")
	}
	if e = unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); e != nil {
		f.Close()
		return nil, fmt.Errorf("another migration is using this staging directory: %w", e)
	}
	return func() { unix.Flock(int(f.Fd()), unix.LOCK_UN); f.Close() }, nil
}
