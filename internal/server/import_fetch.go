// SPDX-License-Identifier: AGPL-3.0-only
package server

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/AniBakaBaka/AniDan/internal/job"
	"github.com/AniBakaBaka/AniDan/internal/store"
)

var errImportProviderFetch = errors.New("import provider request failed")

type importPublicationPlanKey struct{}

// An empty target denotes the content-addressed object. Legacy writable paths
// are captured canonically; live custom templates are deliberately not shared.
type importPublicationPlan struct{ target string }

func (s *Server) nativeImportPlan(ctx context.Context, ep store.Row) (importPublicationPlan, bool) {
	plan := importPublicationPlan{}
	if boolean(s.setting(ctx, "customDanmakuPathEnabled", "false")) {
		return plan, false
	}
	old := str(ep["danmaku_file_path"])
	if old != "" && !immutableObjectName.MatchString(filepath.Base(s.normalizeStoredPath(old))) {
		root, _, target, err := s.storageTarget(old)
		if err == nil {
			root.Close()
			plan.target = target
		}
	}
	return plan, true
}

// Only equivalent native generic imports share this flight. Empty results are
// skipped, larger pools retained, and each caller keeps its original deadline,
// mapping and deferred metadata work. No completed result is cached.
func (s *Server) fetchNativeImport(ctx context.Context, ep, src store.Row) (episodeFetchResult, error) {
	if err := job.Checkpoint(ctx); err != nil {
		return episodeFetchResult{}, err
	}
	_, _, adapter, key, err := s.episodeFetchSnapshot(ctx, ep, src)
	if err != nil {
		return episodeFetchResult{}, err
	}
	listingProof := mediaListingProofFromContext(ctx)
	if err := s.validateMediaListingRouting(ctx, listingProof, src, adapter); err != nil {
		return episodeFetchResult{}, err
	}
	plan, share := s.nativeImportPlan(ctx, ep)
	key.nativeImport, key.importTarget = true, plan.target
	if listingProof != nil {
		key.importListingIdentity = listingProof.identity
	}
	key.importGroupedOwnership = mediaGroupOwnershipRequired(ctx)
	group := mediaEpisodeGroupFromContext(ctx)
	groupTarget := mediaGroupTargetFromContext(ctx)
	if groupTarget != nil {
		key.importGroupTarget = groupTarget.fingerprint
	}
	if group != nil {
		key.importGroup = group.fingerprint
	}
	revalidate := func(work context.Context) error {
		_, _, _, fresh, err := s.episodeFetchSnapshot(work, ep, src)
		if err != nil {
			return err
		}
		fresh.nativeImport, fresh.importTarget = true, plan.target
		if listingProof != nil {
			fresh.importListingIdentity = listingProof.identity
			if err := s.validateMediaListingRouting(work, listingProof, src, adapter); err != nil {
				return err
			}
		}
		fresh.importGroupedOwnership = mediaGroupOwnershipRequired(work)
		if group != nil {
			fresh.importGroup = group.fingerprint
			if err := s.validateMediaGroupContext(work, s.Store.DB); err != nil {
				return err
			}
		}
		if groupTarget != nil {
			fresh.importGroupTarget = groupTarget.fingerprint
		}
		if fresh != key {
			return errors.New("import provider configuration or episode identity changed; reload and retry")
		}
		return nil
	}
	load := func(work context.Context) (episodeFetchResult, error) {
		if listingProof != nil {
			work = context.WithValue(work, mediaListingProofKey{}, listingProof)
		}
		// Shared workers intentionally discard caller contexts. Transfer only
		// immutable publication evidence, never one subscriber's pause/deadline.
		if group != nil {
			work = context.WithValue(work, mediaEpisodeGroupContextKey{}, group)
		}
		if groupTarget != nil {
			work = context.WithValue(work, mediaGroupTargetContextKey{}, groupTarget)
		}
		if key.importGroupedOwnership {
			work = context.WithValue(work, mediaGroupOwnershipContextKey{}, true)
		}
		if err := revalidate(work); err != nil {
			return episodeFetchResult{}, err
		}
		if _, err := s.inspectCommentPool(work, ep); err != nil {
			return episodeFetchResult{}, err
		}
		comments, err := adapter.Comments(work, key.providerEpisodeID)
		if err != nil {
			if group != nil {
				if guardErr := revalidate(work); guardErr != nil {
					return episodeFetchResult{}, guardErr
				}
			}
			return episodeFetchResult{}, fmt.Errorf("%w: %w", errImportProviderFetch, err)
		}
		if err = revalidate(work); err != nil {
			return episodeFetchResult{}, err
		}
		if len(comments) == 0 {
			if _, err := s.inspectCommentPool(work, ep); err != nil {
				return episodeFetchResult{}, err
			}
			err = s.libTransaction(work, func(tx *sql.Tx) error { return s.validateDownloadIdentity(work, tx, ep, src) })
			return episodeFetchResult{episodeID: key.episodeID, noPublication: true}, err
		}
		if share {
			if _, sharedWork := work.Value(episodeFetchPublicationKey{}).(*episodeFetchCall); sharedWork {
				if err = awaitEpisodePublication(work); err != nil {
					return episodeFetchResult{}, err
				}
			}
			if err = revalidate(work); err != nil {
				return episodeFetchResult{}, err
			}
			work = context.WithValue(work, importPublicationPlanKey{}, plan)
		}
		out, err := s.saveCommentsSnapshotOutcome(work, ep, src, comments, true)
		return episodeFetchResult{episodeID: key.episodeID, count: out.PoolCount, fetchedCount: len(comments), unchanged: out.Disposition == commentSmallerRetained, publication: out}, err
	}
	if !share {
		return load(ctx)
	}
	result, err := s.episodeFetchRuntime().do(ctx, key, false, nil, load, nil)
	if errors.Is(err, errEpisodeFetchBusy) {
		// Sharing is an optimization, not a new import rejection policy.
		// Saturation uses the original caller-owned path, still bounded by
		// the configured job workers and provider limits. No extra flight or
		// goroutine is admitted, including while canceled workers drain.
		return load(ctx)
	}
	return result, err
}
