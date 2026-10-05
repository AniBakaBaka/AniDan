// SPDX-License-Identifier: AGPL-3.0-only
package config

import (
	"crypto/sha256"
	"fmt"
	"path/filepath"
)

// WithoutRemote makes a portable cache configuration without copying source
// endpoints or credentials. It never contacts or modifies the source cache.
func (c Cache) WithoutRemote() Cache {
	c.RedisURL = ""
	if c.Backend == "redis" || c.Backend == "valkey" {
		c.Backend = "hybrid"
	}
	return c
}

// ForIsolatedTarget preserves bounded cache settings but gives each restored or
// migrated target its own deterministic namespace, independent of source secrets.
// Environment overrides are deliberately not consulted and no network I/O occurs.
func (c Cache) ForIsolatedTarget(targetDir string) (Cache, error) {
	target, err := filepath.Abs(targetDir)
	if err != nil {
		return Cache{}, err
	}
	c = c.WithoutRemote()
	namespace := sha256.Sum256([]byte(target))
	c.Namespace = fmt.Sprintf("migrated-%x", namespace[:12])
	return c, c.Validate()
}
