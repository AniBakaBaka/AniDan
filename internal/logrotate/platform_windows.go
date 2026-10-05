//go:build windows

// SPDX-License-Identifier: AGPL-3.0-only
package logrotate

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

func openAnchored(root *os.Root, name string, flags int, mode os.FileMode) (*os.File, error) {
	return root.OpenFile(name, flags, mode)
}

func lockExclusive(f *os.File) error {
	return windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &windows.Overlapped{})
}

// Windows access control is operator-owned; FileMode does not expose its ACL.
func checkDirectory(os.FileInfo) error { return nil }

func checkSingleLink(f *os.File, _ os.FileInfo) error {
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(windows.Handle(f.Fd()), &info); err != nil {
		return err
	}
	if info.NumberOfLinks != 1 {
		return errors.New("log files must have exactly one hard link")
	}
	return nil
}
