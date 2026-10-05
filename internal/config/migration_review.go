// SPDX-License-Identifier: AGPL-3.0-only
package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"

	"github.com/AniBakaBaka/AniDan/internal/migrationlimits"
)

type MigrationReview migrationlimits.Limits

func MigrationReviewDefaults() MigrationReview {
	return MigrationReview(migrationlimits.Defaults())
}
func (c MigrationReview) Options() migrationlimits.Limits {
	return migrationlimits.Limits(c)
}
func (c MigrationReview) Validate() error { return c.Options().Validate() }

func (c *MigrationReview) UnmarshalJSON(data []byte) error {
	d := json.NewDecoder(bytes.NewReader(data))
	token, err := d.Token()
	if err != nil || token != json.Delim('{') {
		return errors.New("migrationReview must be a JSON object")
	}
	candidate := *c
	seen := map[string]bool{}
	allowed := map[string]any{"maxReceiptBytes": &candidate.MaxReceiptBytes, "maxFiles": &candidate.MaxFiles, "maxDatabaseBytes": &candidate.MaxDatabaseBytes, "maxRowBytes": &candidate.MaxRowBytes, "maxFileBytes": &candidate.MaxFileBytes, "maxTotalFileBytes": &candidate.MaxTotalFileBytes, "timeoutSeconds": &candidate.TimeoutSeconds}
	for d.More() {
		token, err = d.Token()
		if err != nil {
			return errors.New("invalid migrationReview JSON")
		}
		key, ok := token.(string)
		if !ok || seen[key] {
			return errors.New("duplicate migrationReview field")
		}
		field, ok := allowed[key]
		if !ok {
			return errors.New("unknown migrationReview field")
		}
		seen[key] = true
		var raw json.RawMessage
		if err = d.Decode(&raw); err != nil {
			return errors.New("invalid migrationReview JSON")
		}
		if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return errors.New("migrationReview fields must not be null")
		}
		if err = json.Unmarshal(raw, field); err != nil {
			return fmt.Errorf("migrationReview.%s must be an integer within its configured bounds", key)
		}
	}
	if _, err = d.Token(); err != nil {
		return errors.New("invalid migrationReview JSON")
	}
	if _, err = d.Token(); err != io.EOF {
		return errors.New("trailing migrationReview JSON")
	}
	if err = candidate.Validate(); err != nil {
		return err
	}
	*c = candidate
	return nil
}

func (c *MigrationReview) applyEnvironment() error {
	for _, item := range []struct {
		name  string
		value *int64
	}{
		{"ANIDAN_MIGRATION_REVIEW_MAX_RECEIPT_BYTES", &c.MaxReceiptBytes},
		{"ANIDAN_MIGRATION_REVIEW_MAX_DATABASE_BYTES", &c.MaxDatabaseBytes},
		{"ANIDAN_MIGRATION_REVIEW_MAX_ROW_BYTES", &c.MaxRowBytes},
		{"ANIDAN_MIGRATION_REVIEW_MAX_FILE_BYTES", &c.MaxFileBytes},
		{"ANIDAN_MIGRATION_REVIEW_MAX_TOTAL_FILE_BYTES", &c.MaxTotalFileBytes},
	} {
		if raw, exists := os.LookupEnv(item.name); exists {
			value, err := strconv.ParseInt(raw, 10, 64)
			if err != nil {
				return fmt.Errorf("%s must be an integer", item.name)
			}
			*item.value = value
		}
	}
	for _, item := range []struct {
		name  string
		value *int
	}{
		{"ANIDAN_MIGRATION_REVIEW_MAX_FILES", &c.MaxFiles},
		{"ANIDAN_MIGRATION_REVIEW_TIMEOUT_SECONDS", &c.TimeoutSeconds},
	} {
		if raw, exists := os.LookupEnv(item.name); exists {
			value, err := strconv.Atoi(raw)
			if err != nil {
				return fmt.Errorf("%s must be an integer", item.name)
			}
			*item.value = value
		}
	}
	return c.Validate()
}
