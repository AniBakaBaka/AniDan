//go:build !(aix || darwin || dragonfly || freebsd || illumos || linux || netbsd || openbsd || solaris)

// SPDX-License-Identifier: AGPL-3.0-only
package migrate

import "os"

// Other platforms retain pre-open and descriptor checks. Atomic migration
// publication is Linux-only; no Unix FIFO-swap guarantee is made here.
const regularReadFlags = os.O_RDONLY
const regularNoFollow = 0

func openFileNoFollow(root *os.Root, name string, flags int, perm os.FileMode) (*os.File, error) {
	return root.OpenFile(name, flags, perm)
}
