// SPDX-License-Identifier: AGPL-3.0-only
package migrate

import (
	"context"
	"errors"

	"github.com/AniBakaBaka/AniDan/internal/store"
)

// VerifyRemoteTargetIdentity binds actual SQL namespace and retained ownership
// to the local receipt. Credentials are consumed in memory only. Ordinary
// non-migrated SQL databases do not require this migration-specific contract.
func VerifyRemoteTargetIdentity(ctx context.Context, s *store.Store, dsn string, q store.SQLQueryer, evidence *StartupEvidence) error {
	owner, err := ReadRemoteOwnership(ctx, s.Dialect, q)
	if err != nil {
		return err
	}
	if evidence == nil {
		if owner != nil {
			return errors.New("remote migration ownership exists but its local receipt and journal are missing; runtime initialization refused")
		}
		return nil
	}
	if evidence.Receipt.Version != "2.0" || evidence.Receipt.RemoteTarget == nil || evidence.Receipt.RemoteOwnership == nil {
		return errors.New("configured remote database does not match the migration receipt type")
	}
	if owner == nil || *owner != *evidence.Receipt.RemoteOwnership {
		return errors.New("remote migration ownership does not match its local receipt")
	}
	binding, err := InspectRemoteBinding(ctx, s.Dialect, dsn, q)
	if err != nil {
		return err
	}
	opened, valid := s.RemoteEndpointSHA256()
	if !valid || opened != binding.EndpointSHA256 {
		return errors.New("supplied SQL store endpoint differs from configured migration target")
	}
	if binding != *evidence.Receipt.RemoteTarget || owner.TargetBindingSHA256 != binding.SHA256() {
		return errors.New("configured remote target identity does not match the migration receipt")
	}
	return nil
}
