// SPDX-License-Identifier: AGPL-3.0-only
package server

type commentPublicationDisposition string

const (
	commentPublished       commentPublicationDisposition = "published"
	commentSmallerRetained commentPublicationDisposition = "smaller_retained"
	commentEmptySkipped    commentPublicationDisposition = "empty_skipped"
)

// This is an internal observation of one write attempt, not a delivery receipt.
// PoolCount is authoritative only when PoolCountKnown is true. An error after
// staging began may leave an immutable object and must not release speculative
// reservations as though no storage work had happened. Committed can be true
// together with an error when journal cleanup fails after a successful commit.
type commentPublicationOutcome struct {
	FetchedCount       int
	PoolCount          int64
	PoolCountKnown     bool
	Disposition        commentPublicationDisposition
	StagingStarted     bool
	TargetWriteStarted bool
	Committed          bool
	CommitUncertain    bool
}
