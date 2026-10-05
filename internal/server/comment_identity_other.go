//go:build !linux && !darwin && !freebsd && !openbsd && !netbsd && !dragonfly

// SPDX-License-Identifier: AGPL-3.0-only
package server

import "os"

// Native container targets use file identities. Other ports retain targeted
// normalized-path invalidation without relying on unsupported Sys structures.
func commentFileIdentity(os.FileInfo) string { return "" }
