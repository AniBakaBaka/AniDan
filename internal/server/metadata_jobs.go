// SPDX-License-Identifier: AGPL-3.0-only
package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/AniBakaBaka/AniDan/internal/integration"
	"github.com/AniBakaBaka/AniDan/internal/job"
	"github.com/AniBakaBaka/AniDan/internal/store"
	"github.com/robfig/cron/v3"
)

// Registration makes queued sync/clear operations recoverable after a clean
// restart. Running work is still never automatically replayed by Manager.
func (s *Server) initMetadataJobs() error {
	if err := s.Jobs.Register("bangumiDataSync", s.runMetadataDatasetSync); err != nil {
		return err
	}
	if err := s.Jobs.Register("bangumiDataClear", s.runMetadataDatasetClear); err != nil {
		return err
	}
	// A staged restore must not activate new work as an initialization side effect.
	if s.Jobs.RecoveryReviewRequired() {
		return nil
	}
	enabled := s.setting(s.ctx, "bangumiDataSyncEnabled", "false")
	expression := s.setting(s.ctx, "bangumiDataSyncCron", "0 4 * * *")
	if enabled == "false" {
		var n int
		if err := s.Store.DB.QueryRowContext(s.ctx, "SELECT COUNT(*) FROM scheduled_tasks WHERE job_type='bangumiDataSync'").Scan(&n); err != nil {
			return err
		}
		if n == 0 {
			return nil
		}
	}
	err := s.metadataAtomicSettings(s.ctx, "", nil, map[string]string{"bangumiDataSyncEnabled": enabled, "bangumiDataSyncCron": expression})
	if err != nil && integration.Status(err) == 422 {
		// Keep migrated invalid values visible and permit authenticated correction.
		// Disable stale durable cadence rather than running it against invalid
		// global settings. A later valid save re-enables the same row.
		if _, disableErr := s.Store.DB.ExecContext(s.ctx, s.Store.Rebind("UPDATE scheduled_tasks SET is_enabled=?,next_run_at=NULL WHERE job_type=?"), false, "bangumiDataSync"); disableErr != nil {
			return disableErr
		}
		s.metadataScheduleError.Store(err.Error())
		slog.Warn("bangumi-data automatic schedule needs configuration correction", "error", err)
		return nil
	}
	return err
}

// Called inside the SAME transaction that stores the settings. A crash cannot
// leave an enabled setting without its durable cron definition, or vice versa.
func (s *Server) metadataScheduleTx(ctx context.Context, tx *sql.Tx, updates map[string]string, recoveryReview bool) ([]string, error) {
	_, a := updates["bangumiDataSyncEnabled"]
	_, b := updates["bangumiDataSyncCron"]
	if !a && !b {
		return nil, nil
	}
	read := func(key, fallback string) (string, error) {
		if v, ok := updates[key]; ok {
			return v, nil
		}
		var value string
		e := tx.QueryRowContext(ctx, s.Store.Rebind("SELECT config_value FROM config WHERE config_key=?"), key).Scan(&value)
		if errors.Is(e, sql.ErrNoRows) {
			return fallback, nil
		}
		return value, e
	}
	enabled, e := read("bangumiDataSyncEnabled", "false")
	if e != nil {
		return nil, e
	}
	if enabled != "true" && enabled != "false" {
		return nil, &integration.Error{Provider: "bangumi-data", Status: 422, Kind: "sync enabled must be a boolean"}
	}
	expression, e := read("bangumiDataSyncCron", "0 4 * * *")
	if e != nil {
		return nil, e
	}
	expression = strings.TrimSpace(expression)
	if expression == "" {
		expression = "0 4 * * *"
		updates["bangumiDataSyncCron"] = expression
	}
	parser := cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)
	parsed, e := parser.Parse(expression)
	if e != nil {
		return nil, &integration.Error{Provider: "bangumi-data", Status: 422, Kind: "invalid five-field sync cron expression"}
	}
	next := parsed.Next(time.Now().In(s.authLocation()))
	if next.IsZero() {
		return nil, &integration.Error{Provider: "bangumi-data", Status: 422, Kind: "sync cron has no future occurrence"}
	}
	if enabled == "true" && recoveryReview {
		return nil, &integration.Error{Provider: "bangumi-data", Status: 409, Kind: "restored jobs require explicit operator review before enabling synchronization"}
	}
	var id string
	e = tx.QueryRowContext(ctx, "SELECT id FROM scheduled_tasks WHERE job_type='bangumiDataSync' ORDER BY id LIMIT 1").Scan(&id)
	if e != nil && !errors.Is(e, sql.ErrNoRows) {
		return nil, e
	}
	if errors.Is(e, sql.ErrNoRows) {
		if enabled == "false" {
			return nil, nil
		}
		id = "anidan_bangumi_data_sync"
		_, e = s.Store.InsertTx(ctx, tx, "scheduled_tasks", store.Row{"id": id, "name": "bangumi-data 离线索引同步", "job_type": "bangumiDataSync", "cron_expression": expression, "is_enabled": true, "task_config": "{}", "next_run_at": next.Format("2006-01-02 15:04:05.000000")})
	} else {
		var nextArg any
		if enabled == "true" {
			nextArg = next.Format("2006-01-02 15:04:05.000000")
		}
		_, e = tx.ExecContext(ctx, s.Store.Rebind("UPDATE scheduled_tasks SET cron_expression=?,is_enabled=?,next_run_at=? WHERE id=?"), expression, enabled == "true", nextArg, id)
	}
	if e != nil {
		return nil, e
	}
	return []string{id}, nil
}

func (s *Server) runMetadataDatasetSync(ctx context.Context, raw json.RawMessage, progress func(int, string)) (any, error) {
	var params struct {
		Manual bool `json:"manual"`
	}
	if len(raw) > 0 && string(raw) != "null" {
		if e := unmarshalExactJSON(raw, &params); e != nil {
			return nil, e
		}
	}
	if !params.Manual && s.setting(ctx, "bangumiDataSyncEnabled", "false") != "true" {
		return map[string]any{"skipped": true, "reason": "scheduled synchronization disabled"}, nil
	}
	s.metadataDatasetMu.Lock()
	defer s.metadataDatasetMu.Unlock()
	if e := job.Checkpoint(ctx); e != nil {
		return nil, e
	}
	progress(10, "正在从 CDN 拉取 bangumi-data...")
	data, e := s.Metadata.FetchDataset(ctx)
	if e != nil {
		return nil, e
	}
	if e = job.Checkpoint(ctx); e != nil {
		return nil, e
	}
	progress(70, fmt.Sprintf("正在写入 %d 条离线索引", len(data.Items)))
	if e = s.metadataPersistDataset(ctx, data, progress); e != nil {
		return nil, e
	}
	progress(100, "同步完成")
	return map[string]any{"success": true, "count": len(data.Items), "attribution": integration.DatasetAttribution}, nil
}
func (s *Server) runMetadataDatasetClear(ctx context.Context, _ json.RawMessage, progress func(int, string)) (any, error) {
	s.metadataDatasetMu.Lock()
	defer s.metadataDatasetMu.Unlock()
	if e := job.Checkpoint(ctx); e != nil {
		return nil, e
	}
	progress(10, "正在清除离线索引")
	tx, e := s.Store.DB.BeginTx(ctx, nil)
	if e != nil {
		return nil, e
	}
	defer tx.Rollback()
	res, e := tx.ExecContext(ctx, "DELETE FROM "+s.Store.Quote("bangumi_data_index"))
	if e != nil {
		return nil, e
	}
	if e = s.metadataConfigTx(ctx, tx, "bangumiDataLocalLoadRecord", "{}"); e != nil {
		return nil, e
	}
	if e = s.metadataConfigTx(ctx, tx, "bangumiDataSiteMeta", "{}"); e != nil {
		return nil, e
	}
	if e = job.Checkpoint(ctx); e != nil {
		return nil, e
	}
	if e = tx.Commit(); e != nil {
		return nil, e
	}
	n, _ := res.RowsAffected()
	progress(100, "已清除离线索引")
	return map[string]any{"success": true, "count": n}, nil
}
