//go:build !linux

// SPDX-License-Identifier: AGPL-3.0-only
package migrate

import "errors"

func publishDirectory(from, to string) error {
	return errors.New("atomic no-replace directory publication currently requires Linux; use the Linux container for migration (dry-run/export are portable)")
}

func acquireStageLock(stage string) (func(), error) {
	return nil, errors.New("migration publication requires the Linux container")
}
