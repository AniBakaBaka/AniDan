// SPDX-License-Identifier: AGPL-3.0-only
// Package migrationlimits defines bounded operator-local verification settings.
package migrationlimits

import (
	"errors"
	"fmt"
)

// Limits are trusted local operator settings. Never derive
// them from a receipt, snapshot, SQL configuration row, or archive manifest.
type Limits struct {
	MaxReceiptBytes   int64 `json:"maxReceiptBytes"`
	MaxFiles          int   `json:"maxFiles"`
	MaxDatabaseBytes  int64 `json:"maxDatabaseBytes"`
	MaxRowBytes       int64 `json:"maxRowBytes"`
	MaxFileBytes      int64 `json:"maxFileBytes"`
	MaxTotalFileBytes int64 `json:"maxTotalFileBytes"`
	TimeoutSeconds    int   `json:"timeoutSeconds"`
}

func Defaults() Limits {
	return Limits{MaxReceiptBytes: 32 << 20, MaxFiles: 100000, MaxDatabaseBytes: 64 << 30, MaxRowBytes: 32 << 20, MaxFileBytes: 16 << 30, MaxTotalFileBytes: 1 << 40, TimeoutSeconds: 600}
}

func (l Limits) Validate() error {
	if l.MaxRowBytes < 1 || l.MaxRowBytes > 256<<20 {
		return errors.New("migrationReview.maxRowBytes must be 1..268435456 bytes")
	}
	if l.MaxReceiptBytes < 1 || l.MaxReceiptBytes > 256<<20 {
		return errors.New("migrationReview.maxReceiptBytes must be 1..268435456 bytes")
	}
	if l.MaxFiles < 1 || l.MaxFiles > 1000000 {
		return errors.New("migrationReview.maxFiles must be 1..1000000")
	}
	for _, item := range []struct {
		name  string
		value int64
	}{{"maxDatabaseBytes", l.MaxDatabaseBytes}, {"maxFileBytes", l.MaxFileBytes}, {"maxTotalFileBytes", l.MaxTotalFileBytes}} {
		if item.value < 1 || item.value > 1<<50 {
			return fmt.Errorf("migrationReview.%s must be 1..1125899906842624 bytes", item.name)
		}
	}
	if l.MaxFileBytes > l.MaxTotalFileBytes {
		return errors.New("migrationReview.maxFileBytes must not exceed maxTotalFileBytes")
	}
	if l.TimeoutSeconds < 1 || l.TimeoutSeconds > 86400 {
		return errors.New("migrationReview.timeoutSeconds must be 1..86400 seconds")
	}
	return nil
}
