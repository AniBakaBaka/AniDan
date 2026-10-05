// SPDX-License-Identifier: AGPL-3.0-only
package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/AniBakaBaka/AniDan/internal/job"
)

const localWarningResultBytes = 16 << 10

type localWarningListRow struct {
	ItemID string `json:"itemId,omitempty"`
	Code   string `json:"code"`
}

// List responses expose a small allowlisted projection rather than arbitrary
// persisted task results or provider-supplied message strings.
func localWarningProjection(raw []byte) json.RawMessage {
	unavailable := json.RawMessage(`{"warningsUnavailable":true}`)
	if len(raw) > localWarningResultBytes {
		return unavailable
	}
	var stored struct {
		Count     *json.Number    `json:"warningCount"`
		Warnings  json.RawMessage `json:"warnings"`
		Truncated bool            `json:"warningsTruncated"`
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	if err := d.Decode(&stored); err != nil {
		return unavailable
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		return unavailable
	}
	if stored.Count == nil && len(stored.Warnings) == 0 {
		return nil
	}
	if stored.Count == nil {
		return unavailable
	}
	count, err := strconv.Atoi(stored.Count.String())
	if err != nil || count < 0 || count > 1_000_000 || strconv.Itoa(count) != stored.Count.String() {
		return unavailable
	}
	if count == 0 && len(stored.Warnings) == 0 {
		return nil
	}
	d = json.NewDecoder(bytes.NewReader(stored.Warnings))
	d.UseNumber()
	start, err := d.Token()
	if err != nil || start != json.Delim('[') {
		return unavailable
	}
	rows := []localWarningListRow{}
	truncated := stored.Truncated
	for d.More() {
		if len(rows) == localPosterWarningLimit {
			if count <= len(rows) {
				return unavailable
			}
			truncated = true
			break
		}
		var record json.RawMessage
		if err := d.Decode(&record); err != nil || len(record) == 0 || record[0] != '{' {
			return unavailable
		}
		var warning struct {
			ItemID json.RawMessage `json:"itemId"`
			Code   string          `json:"code"`
		}
		if err := json.Unmarshal(record, &warning); err != nil {
			return unavailable
		}
		code := warning.Code
		if localPosterWarning(0, code).Message == "" {
			code = "tmdb_poster_unknown"
		}
		id := ""
		if len(warning.ItemID) <= 64 {
			text := string(warning.ItemID)
			if len(text) > 0 && text[0] == '"' {
				if json.Unmarshal(warning.ItemID, &text) != nil {
					text = ""
				}
			}
			if n, err := strconv.ParseInt(text, 10, 64); err == nil && strconv.FormatInt(n, 10) == text {
				id = text
			}
		}
		rows = append(rows, localWarningListRow{ItemID: id, Code: code})
	}
	if count < len(rows) {
		return unavailable
	}
	if count == 0 {
		return nil
	}
	encoded, err := json.Marshal(struct {
		Warnings  []localWarningListRow `json:"warnings"`
		Count     int                   `json:"warningCount"`
		Truncated bool                  `json:"warningsTruncated"`
	}{rows, count, truncated || count > len(rows)})
	if err != nil {
		return unavailable
	}
	return encoded
}

// This is only called after UI pagination. One bounded query avoids detail
// requests per task and does not change control lists or durable job storage.
func (s *Server) localImportWarningSummaries(ctx context.Context, page []any) error {
	indexes := map[string]int{}
	args := []any{}
	for i, value := range page {
		task, ok := value.(job.Task)
		if !ok || task.Kind != "import_local_items" || len(task.ID) > 256 {
			continue
		}
		key := "anidan.job.result." + task.ID
		indexes[key] = i
		args = append(args, key)
	}
	if len(args) == 0 {
		return nil
	}
	if len(args) > 1000 {
		return errors.New("local warning page exceeds limit")
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	query := "SELECT config_key,SUBSTR(config_value,1,16385) FROM config WHERE config_key IN (" + strings.TrimSuffix(strings.Repeat("?,", len(args)), ",") + ")"
	rows, err := s.Store.DB.QueryContext(ctx, s.Store.Rebind(query), args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		if err := ctx.Err(); err != nil {
			return err
		}
		var key, value string
		if err = rows.Scan(&key, &value); err != nil {
			return err
		}
		i, ok := indexes[key]
		if !ok {
			continue
		} // Do not adopt a collation-equivalent key.
		task := page[i].(job.Task)
		task.Result = localWarningProjection([]byte(value))
		page[i] = task
	}
	return rows.Err()
}
