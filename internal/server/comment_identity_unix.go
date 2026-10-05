//go:build linux || darwin || freebsd || openbsd || netbsd || dragonfly

// SPDX-License-Identifier: AGPL-3.0-only
package server

import (
	"fmt"
	"os"
	"syscall"
)

func commentFileIdentity(info os.FileInfo) string {
	if info == nil {
		return ""
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); ok {
		return fmt.Sprintf("file:%d:%d", stat.Dev, stat.Ino)
	}
	return ""
}
