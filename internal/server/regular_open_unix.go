//go:build aix || darwin || dragonfly || freebsd || illumos || linux || netbsd || openbsd || solaris

// SPDX-License-Identifier: AGPL-3.0-only
package server

import (
	"golang.org/x/sys/unix"
	"os"
)

const regularReadFlags = os.O_RDONLY | unix.O_NONBLOCK
