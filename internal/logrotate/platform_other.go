//go:build !darwin && !dragonfly && !freebsd && !illumos && !linux && !netbsd && !openbsd && !solaris && !windows

// SPDX-License-Identifier: AGPL-3.0-only
package logrotate

import (
	"errors"
	"os"
)

func openAnchored(*os.Root, string, int, os.FileMode) (*os.File, error) {
	return nil, errors.New("safe log file opening is unsupported on this platform")
}

func lockExclusive(*os.File) error {
	return errors.New("safe log locking is unsupported on this platform")
}

func checkDirectory(os.FileInfo) error {
	return errors.New("safe log directory validation is unsupported on this platform")
}

func checkSingleLink(*os.File, os.FileInfo) error {
	return errors.New("safe log file validation is unsupported on this platform")
}
