// SPDX-License-Identifier: AGPL-3.0-only
package server

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// Migration operation records, uploaded configuration and unpublished targets
// are not ordinary library paths. After activation, only that selected target's
// own library remains available through existing file APIs.
func (s *Server) guardWebMigrationPath(raw string) error {
	bootstrap := s.DataDir
	if s.MigrationActivation != nil {
		bootstrap = s.MigrationActivation.BootstrapDataDir()
	}
	base, err := filepath.Abs(bootstrap)
	if err != nil {
		return err
	}
	if resolved, e := filepath.EvalSymlinks(base); e == nil {
		base = resolved
	}
	active, err := filepath.Abs(s.DataDir)
	if err != nil {
		return err
	}
	if resolved, e := filepath.EvalSymlinks(active); e == nil {
		active = resolved
	}
	p, err := filepath.Abs(raw)
	if err != nil {
		return err
	}
	// Resolve a bounded existing ancestor so aliases to private state are also
	// rejected when the final output file does not exist yet.
	ancestor := p
	for depth := 0; depth < 128; depth++ {
		resolved, e := filepath.EvalSymlinks(ancestor)
		if e == nil {
			rel, e := filepath.Rel(ancestor, p)
			if e != nil {
				return e
			}
			p = filepath.Join(resolved, rel)
			break
		}
		if !os.IsNotExist(e) || filepath.Dir(ancestor) == ancestor || depth == 127 {
			return errors.New("cannot verify library path outside private migration state")
		}
		ancestor = filepath.Dir(ancestor)
	}
	private := filepath.Join(base, ".web-migrations")
	if p == filepath.Join(base, migrationActivationFile) || p == filepath.Join(base, ".anidan-migration-activation-witness.json") ||
		(filepath.Dir(p) == base && (strings.HasPrefix(filepath.Base(p), migrationActivationFile+".tmp-") || strings.HasPrefix(filepath.Base(p), ".immutable-object-"))) ||
		localContained(filepath.Join(active, ".web-migrations"), p) ||
		(localContained(private, p) && (active == base || !localContained(active, p))) {
		return errors.New("private migration state is unavailable to library file operations")
	}
	return nil
}
