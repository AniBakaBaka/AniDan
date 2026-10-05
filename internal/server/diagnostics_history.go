// SPDX-License-Identifier: AGPL-3.0-or-later
package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strings"
)

type configChangeRecord struct {
	Key        string  `json:"key"`
	OldValue   *string `json:"oldValue"`
	NewValue   *string `json:"newValue"`
	ChangedAt  string  `json:"changedAt"`
	Source     string  `json:"source"`
	Restorable bool    `json:"restorable"`
}

func (s *Server) registerDiagnosticHistory(m *http.ServeMux) {
	m.HandleFunc("GET /api/ui/config-history/list", s.operator(s.configHistoryList))
	m.HandleFunc("POST /api/ui/config-history/rollback", s.operator(s.configHistoryRollback))
	m.HandleFunc("POST /api/ui/config-history/clear", s.operator(s.configHistoryClear))
}
func protectedDiagnosticKey(key string) bool {
	key = strings.ToLower(strings.TrimSpace(key))
	return strings.HasPrefix(key, "anidan.") || key == "config_change_history" || key == "security_audit_log"
}
func protectedHistoryKey(key string) bool {
	return !validPublicConfigKey(key) || protectedDiagnosticKey(key) || proxyConfigurationKey(key)
}
func sensitiveDiagnosticKey(key string) bool {
	if !validPublicConfigKey(key) {
		return true
	}
	k := strings.ToLower(key)
	// Singular credential tokens are secrets. Token-selection flags and lists
	// such as matchFallbackTokens and posterProxyTokens are not credentials.
	if k == "github_token" || k == "proxyurl" || k == "accelerateproxyurl" || strings.HasSuffix(k, "token") {
		return true
	}
	for _, part := range []string{"password", "secret", "apikey", "api_key", "cookie", "credential", "accesstoken", "refreshtoken"} {
		if strings.Contains(k, part) {
			return true
		}
	}
	return false
}
func (s *Server) setSettingWithHistory(ctx context.Context, key, value, source string) error {
	return s.setSettingsWithHistory(ctx, map[string]string{key: value}, source)
}

// setSettingsWithHistory persists one bounded settings form and its rollback records atomically.
func (s *Server) setSettingsWithHistory(ctx context.Context, values map[string]string, source string) error {
	if len(values) < 1 || len(values) > 64 {
		return errors.New("configuration batch must contain 1..64 keys")
	}
	keys := make([]string, 0, len(values))
	totalBytes := 0
	for key, value := range values {
		totalBytes += len(value)
		if totalBytes > 2<<20 {
			return errors.New("configuration batch exceeds safety bound")
		}
		if key == "" || len(key) > 500 || protectedHistoryKey(key) {
			return errors.New("protected or invalid configuration key")
		}
		if len(value) > 2<<20 {
			return errors.New("configuration value exceeds safety bound")
		}
		keys = append(keys, key)
	}
	sort.Strings(keys)
	tx, err := s.Store.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	q := "SELECT config_value FROM config WHERE config_key=?"
	if s.Store.Dialect != "sqlite" {
		q += " FOR UPDATE"
	}
	var raw string
	err = tx.QueryRowContext(ctx, s.Store.Rebind(q), "config_change_history").Scan(&raw)
	historyMissing := errors.Is(err, sql.ErrNoRows)
	if err != nil && !historyMissing {
		return err
	}
	if raw == "" {
		raw = "[]"
	}
	if len(raw) > 4<<20 {
		return errors.New("configuration history exceeds safety bound")
	}
	var history []configChangeRecord
	if err = json.Unmarshal([]byte(raw), &history); err != nil {
		return err
	}
	for _, key := range keys {
		value := values[key]
		var old string
		err = tx.QueryRowContext(ctx, s.Store.Rebind(q), key).Scan(&old)
		missing := errors.Is(err, sql.ErrNoRows)
		if err != nil && !missing {
			return err
		}
		if missing {
			_, err = tx.ExecContext(ctx, s.Store.Rebind("INSERT INTO config(config_key,config_value,description) VALUES(?,?,?)"), key, value, "User configuration")
		} else {
			_, err = tx.ExecContext(ctx, s.Store.Rebind("UPDATE config SET config_value=? WHERE config_key=?"), value, key)
		}
		if err != nil {
			return err
		}
		record := configChangeRecord{Key: key, OldValue: &old, NewValue: &value, ChangedAt: s.now(), Source: source, Restorable: true}
		if sensitiveDiagnosticKey(key) || len(old) > 65536 || len(value) > 65536 {
			record.OldValue = nil
			record.NewValue = nil
			record.Restorable = false
		}
		history = append(history, record)
	}
	// Keep up to 20 records per key and 200 total, without truncating rollback data.
	counts := map[string]int{}
	kept := make([]configChangeRecord, 0, min(200, len(history)))
	for i := len(history) - 1; i >= 0 && len(kept) < 200; i-- {
		if counts[history[i].Key] >= 20 {
			continue
		}
		counts[history[i].Key]++
		entry := history[i]
		if sensitiveDiagnosticKey(entry.Key) || protectedHistoryKey(entry.Key) {
			entry.OldValue = nil
			entry.NewValue = nil
			entry.Restorable = false
		}
		kept = append(kept, entry)
	}
	for i, j := 0, len(kept)-1; i < j; i, j = i+1, j-1 {
		kept[i], kept[j] = kept[j], kept[i]
	}
	encoded, err := json.Marshal(kept)
	if err != nil {
		return err
	}
	if len(encoded) > 4<<20 {
		return errors.New("configuration history payload exceeds safety bound")
	}
	if historyMissing {
		_, err = tx.ExecContext(ctx, s.Store.Rebind("INSERT INTO config(config_key,config_value,description) VALUES(?,?,?)"), "config_change_history", string(encoded), "Configuration change history")
	} else {
		_, err = tx.ExecContext(ctx, s.Store.Rebind("UPDATE config SET config_value=? WHERE config_key=?"), string(encoded), "config_change_history")
	}
	if err != nil {
		return err
	}
	return tx.Commit()
}
func (s *Server) configHistoryList(w http.ResponseWriter, r *http.Request) {
	limit, err := queryInt(r, "limit", 20, 100)
	if err != nil || limit < 1 {
		httpError(w, 400, "limit must be 1..100")
		return
	}
	raw := s.setting(r.Context(), "config_change_history", "[]")
	if len(raw) > 4<<20 {
		httpError(w, 413, "configuration history exceeds safety bound")
		return
	}
	var history []configChangeRecord
	if err = json.Unmarshal([]byte(raw), &history); err != nil {
		httpError(w, 500, "Invalid stored configuration history")
		return
	}
	out := []configChangeRecord{}
	for _, entry := range history {
		if key := r.URL.Query().Get("key"); key != "" && entry.Key != key {
			continue
		}
		if sensitiveDiagnosticKey(entry.Key) || protectedHistoryKey(entry.Key) {
			entry.OldValue = nil
			entry.NewValue = nil
			entry.Restorable = false
		} else if entry.OldValue != nil && !strings.HasSuffix(*entry.OldValue, "...") {
			entry.Restorable = true
		}
		out = append(out, entry)
	}
	if len(out) > limit {
		out = out[len(out)-limit:]
	}
	writeJSON(w, 200, out)
}
func (s *Server) configHistoryRollback(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Key   string `json:"key"`
		Value string `json:"value"`
	}
	if err := readJSON(r, &in); err != nil {
		httpError(w, 400, err.Error())
		return
	}
	if protectedHistoryKey(in.Key) || sensitiveDiagnosticKey(in.Key) {
		httpError(w, 400, "Protected or secret configuration must be changed through its dedicated settings flow")
		return
	}
	raw := s.setting(r.Context(), "config_change_history", "[]")
	if len(raw) > 4<<20 {
		httpError(w, 413, "configuration history exceeds safety bound")
		return
	}
	var history []configChangeRecord
	if err := json.Unmarshal([]byte(raw), &history); err != nil {
		httpError(w, 500, "Invalid stored configuration history")
		return
	}
	found := false
	for _, entry := range history {
		if entry.Key == in.Key && ((entry.OldValue != nil && *entry.OldValue == in.Value) || (entry.NewValue != nil && *entry.NewValue == in.Value)) {
			found = true
		}
	}
	if !found || strings.HasSuffix(in.Value, "...") {
		httpError(w, 409, "No complete matching historical value is available; refusing a truncated or fabricated rollback")
		return
	}
	if err := s.setSettingWithHistory(r.Context(), in.Key, in.Value, "rollback"); err != nil {
		httpError(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"message": "ok", "success": true})
}
func (s *Server) configHistoryClear(w http.ResponseWriter, r *http.Request) {
	if err := s.setSetting(r.Context(), "config_change_history", "[]"); err != nil {
		httpError(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]string{"message": "ok"})
}
