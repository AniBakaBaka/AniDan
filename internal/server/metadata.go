// SPDX-License-Identifier: AGPL-3.0-or-later
package server

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/AniBakaBaka/AniDan/internal/integration"
	"github.com/AniBakaBaka/AniDan/internal/job"
	"github.com/AniBakaBaka/AniDan/internal/provider"
	"github.com/AniBakaBaka/AniDan/internal/proxyroute"
	"github.com/AniBakaBaka/AniDan/internal/store"
)

func (s *Server) registerMetadata(m *http.ServeMux) {
	if s.Metadata == nil {
		s.Metadata = integration.NewClient(s.setting)
	}
	s.Metadata.Routing = func(ctx context.Context, p string) (proxyroute.Config, error) {
		use := p == "bangumi-data"
		if !use {
			row, e := s.Store.Get(ctx, "metadata_sources", p)
			if e != nil && !errors.Is(e, sql.ErrNoRows) {
				return proxyroute.Config{}, e
			}
			use = boolean(row["use_proxy"])
		}
		// Preserve the established metadata client/environment policy when no
		// application proxy is selected. Native sources remain explicitly direct.
		state := s.proxyRouting.Load()
		if !use || state != nil && state.mode == "none" {
			return proxyroute.Config{InheritTransport: true}, nil
		}
		return s.selectedProxyRouting(use), nil
	}
	s.Metadata.SetSettings = func(ctx context.Context, values map[string]string) error {
		return s.metadataAtomicSettings(ctx, "", nil, values)
	}
	s.Metadata.OnAIMetric = func(v integration.AIMetric) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = s.Store.Insert(ctx, "ai_metrics_log", store.Row{"timestamp": v.Timestamp.In(s.authLocation()).Format("2006-01-02 15:04:05.000000"), "method": v.Method, "success": v.Success, "duration_ms": v.DurationMS, "tokens_used": v.TokensUsed, "model": v.Model, "error": metadataNull(v.Error), "cache_hit": v.CacheHit})
	}
	m.HandleFunc("GET /api/ui/metadata-sources", s.operator(s.metadataSources))
	m.HandleFunc("PUT /api/ui/metadata-sources", s.operator(s.metadataSourcesUpdate))
	m.HandleFunc("GET /api/ui/metadata-sources/{providerName}/config", s.operator(s.metadataConfigGet))
	m.HandleFunc("PUT /api/ui/metadata-sources/{providerName}/config", s.operator(s.metadataConfigPut))
	m.HandleFunc("GET /api/ui/metadata/{provider}/search", s.operator(s.metadataSearch))
	m.HandleFunc("GET /api/ui/metadata/{provider}/details/{item_id}", s.operator(s.metadataDetails))
	m.HandleFunc("GET /api/ui/metadata/{provider}/details/{mediaType}/{item_id}", s.operator(s.metadataDetails))
	m.HandleFunc("POST /api/ui/metadata/{provider}/actions/{action_name}", s.operator(s.metadataAction))
	m.HandleFunc("POST /api/metadata/bangumi/auth/exchange_code", s.operator(s.metadataBangumiExchange))
	m.HandleFunc("POST /api/bangumi/auth/exchange_code", s.operator(s.metadataBangumiExchange))
	m.HandleFunc("GET /api/ui/bangumi-data/status", s.operator(s.metadataDatasetStatus))
	m.HandleFunc("POST /api/ui/bangumi-data/sync", s.operator(s.metadataDatasetSync))
	m.HandleFunc("POST /api/ui/bangumi-data/clear", s.operator(s.metadataDatasetClear))
	m.HandleFunc("GET /api/ui/bangumi-data/platforms/{bangumi_id}", s.operator(s.metadataDatasetPlatforms))
	m.HandleFunc("GET /api/ui/bangumi-data/danmaku-sources/{bangumi_id}", s.operator(s.metadataDatasetPlatforms))
	m.HandleFunc("POST /api/ui/config/ai/test", s.operator(s.metadataAITest))
	m.HandleFunc("GET /api/ui/config/ai/default-prompts", s.operator(func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, integration.DefaultPrompts()) }))
	m.HandleFunc("GET /api/ui/config/ai/providers", s.operator(func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, integration.AIProviders()) }))
	m.HandleFunc("GET /api/ui/config/ai/models", s.operator(s.metadataAIModels))
	m.HandleFunc("GET /api/ui/config/ai/balance", s.operator(func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, s.Metadata.AIBalance(r.Context())) }))
	m.HandleFunc("POST /api/ui/config/ai/cache/clear", s.operator(s.metadataAIClear))
	m.HandleFunc("POST /api/ui/config/ai/generate-regex", s.operator(s.metadataAIRegex))
	m.HandleFunc("GET /api/ui/config/ai/metrics", s.operator(s.metadataAIMetrics))
	m.HandleFunc("GET /api/ui/ai-explain/recent-matches", s.operator(s.metadataAIRecent))
	m.HandleFunc("GET /api/ui/ai-explain/stats", s.operator(s.metadataAIStats))
	m.HandleFunc("GET /api/ui/ai-explain/low-confidence", s.operator(s.metadataAILowConfidence))
}
func metadataNull(s string) any {
	if s == "" {
		return nil
	}
	return s
}
func metadataObject(v any) map[string]any {
	m, _ := v.(map[string]any)
	if m == nil {
		return map[string]any{}
	}
	return m
}
func metadataList(v any) []any { a, _ := v.([]any); return a }
func metadataError(w http.ResponseWriter, e error) {
	code := integration.Status(e)
	if code == 401 || code == 403 {
		code = 502
	}
	if code == 499 {
		return
	}
	httpError(w, code, e.Error())
}
func (s *Server) metadataEnsure(ctx context.Context, p string) (store.Row, error) {
	if !integration.KnownProvider(p) {
		return nil, &integration.Error{Provider: p, Status: 404, Kind: "unknown provider"}
	}
	r, e := s.Store.Get(ctx, "metadata_sources", p)
	if errors.Is(e, sql.ErrNoRows) {
		order := 0
		for i, name := range integration.Providers {
			if name == p {
				order = i
			}
		}
		_, e = s.Store.Insert(ctx, "metadata_sources", store.Row{"provider_name": p, "is_enabled": true, "is_aux_search_enabled": true, "display_order": order, "use_proxy": false, "is_failover_enabled": false, "log_raw_responses": false})
		if e != nil {
			return nil, e
		}
		r, e = s.Store.Get(ctx, "metadata_sources", p)
	}
	return r, e
}
func (s *Server) metadataSources(w http.ResponseWriter, r *http.Request) {
	out := []map[string]any{}
	for _, p := range integration.Providers {
		row, e := s.metadataEnsure(r.Context(), p)
		if e != nil {
			metadataError(w, e)
			return
		}
		v := camelRow(row)
		code, message := "warning", "已配置；尚未执行在线连通性检查"
		if !boolean(row["is_enabled"]) {
			code, message = "disabled", "已禁用"
		} else if (p == "tmdb" && s.setting(r.Context(), "tmdbApiKey", "") == "") || (p == "tvdb" && s.setting(r.Context(), "tvdbApiKey", "") == "") {
			code, message = "unconfigured", "API Key 未配置"
		}
		v["statusCode"] = code
		v["status"] = message
		v["log_raw_responses"] = boolean(row["log_raw_responses"])
		v["isSearchSupplementSource"] = p == "360" || p == "douban" || p == "bangumi"
		v["isSearchSupplementEnabled"] = s.setting(r.Context(), p+"_searchSupplementEnabled", "false") == "true"
		out = append(out, v)
	}
	sort.SliceStable(out, func(i, j int) bool { return number(out[i]["displayOrder"]) < number(out[j]["displayOrder"]) })
	writeJSON(w, 200, out)
}
func (s *Server) metadataSourcesUpdate(w http.ResponseWriter, r *http.Request) {
	var items []map[string]any
	if e := readJSON(r, &items); e != nil {
		httpError(w, 422, "Invalid settings array")
		return
	}
	if len(items) > len(integration.Providers) {
		httpError(w, 400, "Too many providers")
		return
	}
	seen := map[string]bool{}
	logging := map[string]bool{}
	for _, v := range items {
		p := str(v["providerName"])
		if !integration.KnownProvider(p) {
			httpError(w, 404, "Unknown provider")
			return
		}
		if seen[p] {
			httpError(w, 422, "Duplicate provider")
			return
		}
		seen[p] = true
		if enabled, ok := v["logRawResponses"]; ok {
			if str(enabled) != "true" && str(enabled) != "false" {
				httpError(w, 422, "logRawResponses must be true or false")
				return
			}
			logging[p] = boolean(enabled)
		}
	}
	s.metadataConfigMu.Lock()
	defer s.metadataConfigMu.Unlock()
	tx, e := s.Store.DB.BeginTx(r.Context(), nil)
	if e != nil {
		metadataError(w, e)
		return
	}
	defer tx.Rollback()
	for _, v := range items {
		p := str(v["providerName"])
		var count int
		if e = tx.QueryRowContext(r.Context(), s.Store.Rebind("SELECT COUNT(*) FROM metadata_sources WHERE provider_name=?"), p).Scan(&count); e != nil {
			metadataError(w, e)
			return
		}
		if count == 0 {
			order := 0
			for i, name := range integration.Providers {
				if name == p {
					order = i
				}
			}
			if _, e = s.Store.InsertTx(r.Context(), tx, "metadata_sources", store.Row{"provider_name": p, "is_enabled": true, "is_aux_search_enabled": true, "display_order": order, "use_proxy": false, "is_failover_enabled": false, "log_raw_responses": false}); e != nil {
				metadataError(w, e)
				return
			}
		}
		parts, args := []string{}, []any{}
		for _, field := range [][2]string{{"isAuxSearchEnabled", "is_aux_search_enabled"}, {"isEnabled", "is_enabled"}, {"useProxy", "use_proxy"}, {"displayOrder", "display_order"}, {"logRawResponses", "log_raw_responses"}} {
			if value, ok := v[field[0]]; ok {
				var stored any = boolean(value)
				if field[0] == "displayOrder" {
					stored = number(value)
				}
				args = append(args, stored)
				parts = append(parts, s.Store.Quote(field[1])+"="+s.Store.Placeholder(len(args)))
			}
		}
		if len(parts) > 0 {
			args = append(args, p)
			if _, e = tx.ExecContext(r.Context(), "UPDATE "+s.Store.Quote("metadata_sources")+" SET "+strings.Join(parts, ",")+" WHERE "+s.Store.Quote("provider_name")+"="+s.Store.Placeholder(len(args)), args...); e != nil {
				metadataError(w, e)
				return
			}
		}
	}
	if e = tx.Commit(); e != nil {
		metadataError(w, e)
		return
	}
	s.Metadata.SetResponseLoggingBatch(logging)
	w.WriteHeader(204)
}
func (s *Server) metadataConfigGet(w http.ResponseWriter, r *http.Request) {
	p := r.PathValue("providerName")
	row, e := s.metadataEnsure(r.Context(), p)
	if e != nil {
		metadataError(w, e)
		return
	}
	desc := integration.MetadataConfigs()[p]
	out := camelRow(row)
	bools := map[string]bool{}
	for _, x := range metadataList(desc["bool_config_keys"]) {
		bools[str(x)] = true
	}
	for _, x := range metadataList(desc["config_keys"]) {
		key := str(x)
		def := ""
		if p == "bangumi" && key == "authMode" {
			def = "token"
		}
		meta := metadataObject(metadataObject(desc["configurable_fields"])[key])
		if v, ok := meta["default"]; ok {
			def = str(v)
		}
		v := s.setting(r.Context(), key, def)
		if bools[key] {
			out[key] = v == "true" || v == ""
		} else {
			out[key] = v
		}
	}
	out["isFailoverSource"] = desc["is_failover_source"] == true
	cf := metadataObject(desc["configurable_fields"])
	if len(cf) > 0 {
		out["configurableFields"] = cf
	}
	for key, info := range cf {
		meta := metadataObject(info)
		storageKey := key
		declared := false
		for _, x := range metadataList(desc["config_keys"]) {
			if str(x) == key {
				declared = true
			}
		}
		if !declared {
			storageKey = p + "_" + key
		}
		if str(meta["configKey"]) != "" {
			storageKey = str(meta["configKey"])
		}
		fieldType := str(meta["type"])
		if a := metadataList(info); len(a) > 1 {
			fieldType = str(a[1])
		}
		v := s.setting(r.Context(), storageKey, str(meta["default"]))
		if fieldType == "boolean" {
			out[key] = v == "true"
		} else {
			out[key] = v
		}
	}
	if desc["has_force_aux_search_toggle"] == true {
		out["forceAuxSearchEnabled"] = s.setting(r.Context(), p+"_force_aux_search", "false") == "true"
	}
	for key, value := range out {
		if compatMetadataSecret(desc, key) && str(value) != "" {
			out[key] = compatSecretMask
		}
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, 200, out)
}
func (s *Server) metadataConfigPut(w http.ResponseWriter, r *http.Request) {
	p := r.PathValue("providerName")
	if _, e := s.metadataEnsure(r.Context(), p); e != nil {
		metadataError(w, e)
		return
	}
	var in map[string]any
	if readJSON(r, &in) != nil {
		httpError(w, 422, "Invalid configuration")
		return
	}
	desc := integration.MetadataConfigs()[p]
	allowed := map[string]string{}
	for _, x := range metadataList(desc["config_keys"]) {
		allowed[str(x)] = str(x)
	}
	for key, info := range metadataObject(desc["configurable_fields"]) {
		if _, ok := allowed[key]; !ok {
			allowed[key] = p + "_" + key
		}
		if v := str(metadataObject(info)["configKey"]); v != "" {
			allowed[key] = v
		}
	}
	allowed["forceAuxSearchEnabled"] = p + "_force_aux_search"
	updates := map[string]string{}
	row := store.Row{}
	for k, v := range in {
		if k == "logRawResponses" && str(v) != "true" && str(v) != "false" {
			httpError(w, 422, "logRawResponses must be true or false")
			return
		}
		if key, ok := allowed[k]; ok {
			if compatMetadataSecret(desc, k) && str(v) == compatSecretMask {
				if s.setting(r.Context(), key, "") == "" {
					httpError(w, 422, "No saved metadata credential exists to preserve")
					return
				}
				continue
			}
			updates[key] = str(v)
		}
		if column, ok := map[string]string{"useProxy": "use_proxy", "logRawResponses": "log_raw_responses", "isFailoverEnabled": "is_failover_enabled"}[k]; ok {
			row[column] = boolean(v)
		}
	}
	if e := s.metadataAtomicSettings(r.Context(), p, row, updates); e != nil {
		metadataError(w, e)
		return
	}
	if value, ok := updates["anibtRssUrl"]; ok && p == "anibt" {
		if e := s.syncAniBTSubscription(r.Context(), value); e != nil {
			httpError(w, 502, "Configuration saved, but RSS subscription reconciliation failed: "+e.Error())
			return
		}
	}
	w.WriteHeader(204)
}
func (s *Server) metadataAtomicSettings(ctx context.Context, p string, row store.Row, updates map[string]string) error {
	s.metadataConfigMu.Lock()
	defer s.metadataConfigMu.Unlock()
	recoveryReview := s.Jobs.RecoveryReviewRequired()
	tx, e := s.Store.DB.BeginTx(ctx, nil)
	if e != nil {
		return e
	}
	defer tx.Rollback()
	scheduleIDs, e := s.metadataScheduleTx(ctx, tx, updates, recoveryReview)
	if e != nil {
		return e
	}
	for k, v := range updates {
		if e = s.metadataConfigTx(ctx, tx, k, v); e != nil {
			return e
		}
	}
	if len(row) > 0 {
		parts, args := []string{}, []any{}
		for _, key := range []string{"use_proxy", "log_raw_responses", "is_failover_enabled"} {
			if v, ok := row[key]; ok {
				args = append(args, v)
				parts = append(parts, s.Store.Quote(key)+"="+s.Store.Placeholder(len(args)))
			}
		}
		args = append(args, p)
		_, e = tx.ExecContext(ctx, "UPDATE "+s.Store.Quote("metadata_sources")+" SET "+strings.Join(parts, ",")+" WHERE "+s.Store.Quote("provider_name")+"="+s.Store.Placeholder(len(args)), args...)
		if e != nil {
			return e
		}
	}
	if e = tx.Commit(); e != nil {
		return e
	}
	if enabled, ok := row["log_raw_responses"]; ok && s.Metadata != nil {
		s.Metadata.SetResponseLogging(p, boolean(enabled))
	}
	for _, id := range scheduleIDs {
		if !s.schedulerStarted.Load() {
			continue
		}
		if e = s.Jobs.ReloadSchedule(id); e != nil {
			s.metadataScheduleError.Store(e.Error())
			return fmt.Errorf("configuration and schedule saved, but scheduler activation failed: %w", e)
		}
	}
	if len(scheduleIDs) > 0 {
		s.metadataScheduleError.Store("")
	}
	return nil
}
func (s *Server) metadataConfigTx(ctx context.Context, tx *sql.Tx, key, value string) error {
	var count int
	q := s.Store.Quote
	ph := s.Store.Placeholder
	if e := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+q("config")+" WHERE "+q("config_key")+"="+ph(1), key).Scan(&count); e != nil {
		return e
	}
	if count > 0 {
		_, e := tx.ExecContext(ctx, "UPDATE "+q("config")+" SET "+q("config_value")+"="+ph(1)+" WHERE "+q("config_key")+"="+ph(2), value, key)
		return e
	}
	_, e := s.Store.InsertTx(ctx, tx, "config", store.Row{"config_key": key, "config_value": value})
	return e
}
func (s *Server) metadataUserID(r *http.Request) int64 {
	u, e := s.requireUser(r)
	if e != nil {
		return 0
	}
	return number(u["id"])
}
func (s *Server) metadataCredential(ctx context.Context, p string, id int64) (integration.Credential, error) {
	cred := integration.Credential{}
	if p != "bangumi" && p != "trakt" {
		return cred, nil
	}
	unlock := s.Metadata.LockOAuth(fmt.Sprintf("%s:%d", p, id))
	defer unlock()
	table := "oauth_credentials"
	var key any = store.Row{"user_id": id, "provider": p}
	if p == "bangumi" {
		table = "bangumi_auth"
		key = id
		cred.ClientID = s.setting(ctx, "bangumiClientId", "")
		cred.ClientSecret = s.setting(ctx, "bangumiClientSecret", "")
	}
	row, e := s.Store.Get(ctx, table, key)
	if errors.Is(e, sql.ErrNoRows) {
		return cred, nil
	}
	if e != nil {
		return cred, e
	}
	cred.AccessToken = str(row["access_token"])
	cred.RefreshToken = str(row["refresh_token"])
	cred.RedirectURI = str(row["redirect_uri"])
	if p == "trakt" {
		var extra map[string]any
		_ = json.Unmarshal([]byte(str(row["extra_data"])), &extra)
		cred.ClientID = str(extra["clientId"])
	}
	if date, e := s.authDate(row["expires_at"]); e == nil {
		cred.ExpiresAt = date
	}
	threshold := 24 * time.Hour
	if p == "bangumi" {
		threshold = 72 * time.Hour
	}
	if cred.RefreshToken != "" && !cred.ExpiresAt.IsZero() && time.Until(cred.ExpiresAt) < threshold {
		token, e := s.Metadata.RefreshOAuth(ctx, p, cred)
		if e != nil {
			if cred.ExpiresAt.Before(time.Now()) {
				return cred, e
			}
			return cred, nil
		}
		changes := store.Row{"access_token": token.AccessToken, "refresh_token": token.RefreshToken}
		if token.ExpiresIn > 0 {
			changes["expires_at"] = time.Now().Add(time.Duration(token.ExpiresIn) * time.Second).In(s.authLocation()).Format("2006-01-02 15:04:05.000000")
		}
		if e = s.Store.Update(ctx, table, key, changes); e != nil {
			return cred, e
		}
		cred.AccessToken = token.AccessToken
		cred.RefreshToken = token.RefreshToken
	}
	return cred, nil
}
func (s *Server) metadataSearch(w http.ResponseWriter, r *http.Request) {
	p := r.PathValue("provider")
	cred, e := s.metadataCredential(r.Context(), p, s.metadataUserID(r))
	if e != nil {
		metadataError(w, e)
		return
	}
	out, e := s.Metadata.Search(r.Context(), p, r.URL.Query().Get("keyword"), r.URL.Query().Get("mediaType"), cred)
	if e != nil {
		metadataError(w, e)
		return
	}
	writeJSON(w, 200, out)
}
func (s *Server) metadataDetails(w http.ResponseWriter, r *http.Request) {
	p := r.PathValue("provider")
	cred, e := s.metadataCredential(r.Context(), p, s.metadataUserID(r))
	if e != nil {
		metadataError(w, e)
		return
	}
	typ := r.PathValue("mediaType")
	if typ == "" {
		typ = r.URL.Query().Get("mediaType")
	}
	out, e := s.Metadata.Details(r.Context(), p, r.PathValue("item_id"), typ, cred)
	if e != nil {
		metadataError(w, e)
		return
	}
	if out == nil {
		httpError(w, 404, "未找到详情")
		return
	}
	writeJSON(w, 200, out)
}

func (s *Server) metadataAction(w http.ResponseWriter, r *http.Request) {
	p, action := r.PathValue("provider"), r.PathValue("action_name")
	if !integration.KnownProvider(p) {
		httpError(w, 404, "Unknown provider")
		return
	}
	in := map[string]any{}
	if r.ContentLength != 0 {
		if e := readJSON(r, &in); e != nil {
			httpError(w, 422, "Invalid action payload")
			return
		}
	}
	uid := s.metadataUserID(r)
	if p == "bangumi" || p == "trakt" {
		if s.metadataOAuthAction(w, r, p, action, in, uid) {
			return
		}
	}
	if p == "tmdb" {
		v, e := s.Metadata.TMDBAction(r.Context(), action, in)
		if e != nil {
			metadataError(w, e)
			return
		}
		if action == "get_episode_groups" {
			out := []map[string]any{}
			for _, x := range metadataList(metadataObject(v)["results"]) {
				a := metadataObject(x)
				out = append(out, map[string]any{"description": a["description"], "episodeCount": a["episode_count"], "groupCount": a["group_count"], "id": a["id"], "name": a["name"], "network": a["network"], "type": a["type"]})
			}
			v = out
		}
		if action == "update_mappings" {
			if e = s.metadataSaveMappings(r.Context(), number(in["tmdbId"]), str(in["groupId"]), metadataObject(v)); e != nil {
				metadataError(w, e)
				return
			}
			v = map[string]any{"message": "映射更新成功"}
		}
		writeJSON(w, 200, v)
		return
	}
	if p == "anibt" && action == "discoverSeason" {
		params := url.Values{}
		if season := str(in["season"]); season != "" && season != "current" {
			params.Set("season", season)
		}
		v, e := s.Metadata.AniBTDiscoverSeason(r.Context(), params)
		if e != nil {
			metadataError(w, e)
			return
		}
		writeJSON(w, 200, v)
		return
	}
	httpError(w, 400, "This metadata provider does not support this action")
}
func (s *Server) metadataSaveMappings(ctx context.Context, tvID int64, groupID string, data map[string]any) error {
	group, e := integration.DecodeEpisodeGroup(data, groupID, false)
	if e != nil {
		return e
	}
	return s.episodeGroupSave(ctx, tvID, group, "upsert", nil)
}
func (s *Server) metadataRedirect(r *http.Request, raw string) error {
	u, e := url.Parse(raw)
	if e != nil || u.Host == "" || u.User != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Fragment != "" || u.RawQuery != "" || u.Path != "/bgm-oauth-callback" {
		return errors.New("invalid OAuth redirect URI")
	}
	host := r.Host
	if s.Config.PublicURL != "" {
		v, e := url.Parse(s.Config.PublicURL)
		if e != nil {
			return errors.New("invalid public URL")
		}
		host = v.Host
	}
	if !strings.EqualFold(u.Host, host) {
		return errors.New("OAuth redirect must use this server's public origin")
	}
	return nil
}
func metadataStateSuffix(redirect string) string {
	h := sha256.Sum256([]byte(redirect))
	return hex.EncodeToString(h[:8])
}
func (s *Server) metadataOAuthAction(w http.ResponseWriter, r *http.Request, p, action string, in map[string]any, uid int64) bool {
	if p == "bangumi" && action == "get_auth_url" {
		redirect := str(in["redirect_uri"])
		if e := s.metadataRedirect(r, redirect); e != nil {
			httpError(w, 400, e.Error())
			return true
		}
		clientID := s.setting(r.Context(), "bangumiClientId", "")
		secret := s.setting(r.Context(), "bangumiClientSecret", "")
		if clientID == "" || secret == "" {
			httpError(w, 412, "Bangumi OAuth credentials are not configured")
			return true
		}
		state := randomID() + "." + metadataStateSuffix(redirect)
		_, e := s.Store.Insert(r.Context(), "oauth_states", store.Row{"state_key": state, "user_id": uid, "provider": "bangumi", "expires_at": time.Now().Add(10 * time.Minute).In(s.authLocation()).Format("2006-01-02 15:04:05.000000")})
		if e != nil {
			metadataError(w, e)
			return true
		}
		base := s.setting(r.Context(), "bangumiOAuthBaseUrl", "https://bgm.tv")
		values := url.Values{"client_id": {clientID}, "response_type": {"code"}, "redirect_uri": {redirect}, "state": {state}}
		writeJSON(w, 200, map[string]any{"url": strings.TrimRight(base, "/") + "/oauth/authorize?" + values.Encode(), "state": state})
		return true
	}
	if (p == "bangumi" && action == "get_auth_state") || (p == "trakt" && action == "get_auth_status") {
		_, _ = s.metadataCredential(r.Context(), p, uid)
		writeJSON(w, 200, s.metadataOAuthStatus(r.Context(), p, uid))
		return true
	}
	if p == "bangumi" && action == "refresh_token" {
		unlock := s.Metadata.LockOAuth(fmt.Sprintf("%s:%d", p, uid))
		defer unlock()
		row, e := s.Store.Get(r.Context(), "bangumi_auth", uid)
		if e != nil {
			writeJSON(w, 200, map[string]any{"success": false, "message": "当前未授权，请先完成 OAuth 授权"})
			return true
		}
		cred := integration.Credential{AccessToken: str(row["access_token"]), RefreshToken: str(row["refresh_token"]), RedirectURI: str(row["redirect_uri"]), ClientID: s.setting(r.Context(), "bangumiClientId", ""), ClientSecret: s.setting(r.Context(), "bangumiClientSecret", "")}
		token, e := s.Metadata.RefreshOAuth(r.Context(), p, cred)
		if e != nil {
			writeJSON(w, 200, map[string]any{"success": false, "message": e.Error()})
			return true
		}
		changes := store.Row{"access_token": token.AccessToken, "refresh_token": token.RefreshToken}
		if token.ExpiresIn > 0 {
			changes["expires_at"] = time.Now().Add(time.Duration(token.ExpiresIn) * time.Second).In(s.authLocation()).Format("2006-01-02 15:04:05.000000")
		}
		if e = s.Store.Update(r.Context(), "bangumi_auth", uid, changes); e != nil {
			metadataError(w, e)
			return true
		}
		writeJSON(w, 200, map[string]any{"success": true, "message": "Token 续期成功", "authInfo": s.metadataOAuthStatus(r.Context(), p, uid)})
		return true
	}
	if (p == "bangumi" && action == "logout") || (p == "trakt" && action == "revoke_auth") {
		table := "bangumi_auth"
		var key any = uid
		if p == "trakt" {
			table = "oauth_credentials"
			key = store.Row{"user_id": uid, "provider": p}
		}
		if e := s.Store.Delete(r.Context(), table, key); e != nil {
			metadataError(w, e)
			return true
		}
		writeJSON(w, 200, map[string]any{"success": true, "message": "已撤销授权"})
		return true
	}
	if p == "trakt" && action == "save_oauth" {
		access := str(in["accessToken"])
		clientID := str(in["clientId"])
		if access == "" || clientID == "" {
			httpError(w, 400, "accessToken and clientId required")
			return true
		}
		cred := integration.Credential{AccessToken: access, ClientID: clientID}
		profile, e := s.Metadata.TraktProfile(r.Context(), cred)
		if e != nil {
			metadataError(w, e)
			return true
		}
		extra, _ := json.Marshal(map[string]any{"clientId": clientID})
		row := store.Row{"access_token": access, "refresh_token": metadataNull(firstMetadata(in["refreshToken"], in["refresh_token"])), "provider_user_id": firstMetadata(metadataObject(profile["ids"])["trakt"], in["userId"]), "provider_username": firstMetadata(profile["username"], in["username"]), "authorized_at": s.authNow(), "extra_data": string(extra)}
		expires := number(in["expiresIn"])
		if expires == 0 {
			expires = number(in["expires_in"])
		}
		if expires > 0 && expires < 3650*86400 {
			row["expires_at"] = time.Now().Add(time.Duration(expires) * time.Second).In(s.authLocation()).Format("2006-01-02 15:04:05.000000")
		}
		if e = s.metadataSaveOAuth(r.Context(), "oauth_credentials", store.Row{"user_id": uid, "provider": "trakt"}, row); e != nil {
			metadataError(w, e)
			return true
		}
		writeJSON(w, 200, map[string]any{"success": true, "message": "Trakt 授权成功"})
		return true
	}
	return false
}
func firstMetadata(values ...any) string {
	for _, v := range values {
		if s := str(v); s != "" {
			return s
		}
	}
	return ""
}
func (s *Server) metadataOAuthStatus(ctx context.Context, p string, uid int64) map[string]any {
	table := "bangumi_auth"
	var key any = uid
	if p == "trakt" {
		table = "oauth_credentials"
		key = store.Row{"user_id": uid, "provider": p}
	}
	row, e := s.Store.Get(ctx, table, key)
	if e != nil {
		return map[string]any{"isAuthenticated": false}
	}
	out := camelRow(row)
	delete(out, "accessToken")
	delete(out, "refreshToken")
	delete(out, "extraData")
	out["isAuthenticated"] = true
	out["daysLeft"] = 0
	if exp, e := s.authDate(row["expires_at"]); e == nil {
		out["daysLeft"] = int(time.Until(exp).Hours() / 24)
		if exp.Before(time.Now()) {
			out["isAuthenticated"] = false
			out["isExpired"] = true
		}
	}
	return out
}
func (s *Server) metadataSaveOAuth(ctx context.Context, table string, key store.Row, row store.Row) error {
	existing, e := s.Store.Get(ctx, table, key)
	if e == nil && existing != nil {
		return s.Store.Update(ctx, table, key, row)
	}
	if !errors.Is(e, sql.ErrNoRows) {
		return e
	}
	for k, v := range key {
		row[k] = v
	}
	_, e = s.Store.Insert(ctx, table, row)
	return e
}
func (s *Server) metadataBangumiExchange(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Code     string `json:"code"`
		State    string `json:"state"`
		Redirect string `json:"redirect_uri"`
	}
	if readJSON(r, &in) != nil || in.Code == "" || in.State == "" {
		httpError(w, 422, "code, state and redirect_uri are required")
		return
	}
	if e := s.metadataRedirect(r, in.Redirect); e != nil {
		httpError(w, 400, e.Error())
		return
	}
	uid := s.metadataUserID(r)
	state, e := s.Store.Get(r.Context(), "oauth_states", in.State)
	invalid := e != nil || number(state["user_id"]) != uid || str(state["provider"]) != "bangumi" || !strings.HasSuffix(in.State, "."+metadataStateSuffix(in.Redirect))
	if !invalid {
		exp, e := s.authDate(state["expires_at"])
		invalid = e != nil || time.Now().After(exp)
	}
	if invalid {
		writeJSON(w, 200, map[string]any{"success": false, "message": "State 验证失败，请重新授权"})
		return
	}
	q, ph := s.Store.Quote, s.Store.Placeholder
	res, e := s.Store.DB.ExecContext(r.Context(), "DELETE FROM "+q("oauth_states")+" WHERE "+q("state_key")+"="+ph(1)+" AND "+q("user_id")+"="+ph(2), in.State, uid)
	if e != nil {
		metadataError(w, e)
		return
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		writeJSON(w, 200, map[string]any{"success": false, "message": "State 已使用"})
		return
	}
	cred := integration.Credential{ClientID: s.setting(r.Context(), "bangumiClientId", ""), ClientSecret: s.setting(r.Context(), "bangumiClientSecret", ""), RedirectURI: in.Redirect}
	token, e := s.Metadata.ExchangeBangumi(r.Context(), in.Code, cred)
	if e != nil {
		metadataError(w, e)
		return
	}
	row := store.Row{"access_token": token.AccessToken, "refresh_token": metadataNull(token.RefreshToken), "redirect_uri": in.Redirect, "authorized_at": s.authNow()}
	if token.ExpiresIn > 0 {
		row["expires_at"] = time.Now().Add(time.Duration(token.ExpiresIn) * time.Second).In(s.authLocation()).Format("2006-01-02 15:04:05.000000")
	}
	if e = s.metadataSaveOAuth(r.Context(), "bangumi_auth", store.Row{"user_id": uid}, row); e != nil {
		metadataError(w, e)
		return
	}
	profile, e := s.Metadata.BangumiProfile(r.Context(), token.AccessToken)
	if e != nil {
		metadataError(w, e)
		return
	}
	image := str(metadataObject(profile["avatar"])["large"])
	if strings.HasPrefix(image, "//") {
		image = "https:" + image
	}
	e = s.Store.Update(r.Context(), "bangumi_auth", uid, store.Row{"bangumi_user_id": number(profile["id"]), "nickname": metadataNull(str(profile["nickname"])), "username": metadataNull(str(profile["username"])), "sign": metadataNull(str(profile["sign"])), "avatar_url": metadataNull(image)})
	if e != nil {
		metadataError(w, e)
		return
	}
	writeJSON(w, 200, map[string]any{"success": true, "message": "授权成功"})
}

func (s *Server) metadataDatasetStatus(w http.ResponseWriter, r *http.Request) {
	n, e := s.Store.Count(r.Context(), "bangumi_data_index", nil)
	if e != nil {
		metadataError(w, e)
		return
	}
	warning, _ := s.metadataScheduleError.Load().(string)
	writeJSON(w, 200, map[string]any{"ready": true, "count": n, "attribution": integration.DatasetAttribution, "scheduleError": warning})
}
func (s *Server) metadataDatasetSync(w http.ResponseWriter, r *http.Request) {
	id, e := s.Jobs.SubmitWithOptions("bangumiDataSync", map[string]any{"manual": true}, nil, job.SubmitOptions{Title: "bangumi-data 离线索引同步（手动）", QueueType: "management", UniqueKey: "bangumi-data-sync-manual"})
	if e != nil {
		httpError(w, 409, e.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"message": "bangumi-data 同步任务已提交", "taskId": id})
}
func (s *Server) metadataPersistDataset(ctx context.Context, data integration.Dataset, progress func(int, string)) error {
	tx, e := s.Store.DB.BeginTx(ctx, nil)
	if e != nil {
		return e
	}
	defer tx.Rollback()
	q := s.Store.Quote
	if _, e = tx.ExecContext(ctx, "DELETE FROM "+q("bangumi_data_index")); e != nil {
		return e
	}
	loc := s.authLocation()
	for i, item := range data.Items {
		if i%250 == 0 {
			if e = job.Checkpoint(ctx); e != nil {
				return e
			}
		}
		row := store.Row(integration.DatasetRow(item, int64(i+1), loc))
		raw, e := json.Marshal(row["sites"])
		if e != nil {
			return e
		}
		row["sites"] = string(raw)
		if _, e = s.Store.InsertTx(ctx, tx, "bangumi_data_index", row); e != nil {
			return e
		}
	}
	if len(data.SiteMeta) > 0 {
		raw, _ := json.Marshal(data.SiteMeta)
		if e = s.metadataConfigTx(ctx, tx, "bangumiDataSiteMeta", string(raw)); e != nil {
			return e
		}
	}
	record, _ := json.Marshal(map[string]any{"hash": data.SHA256, "count": len(data.Items), "loadedAt": s.authNow(), "source": data.Source, "license": "CC BY 4.0", "attribution": integration.DatasetAttribution})
	if e = s.metadataConfigTx(ctx, tx, "bangumiDataLocalLoadRecord", string(record)); e != nil {
		return e
	}
	return tx.Commit()
}
func (s *Server) metadataDatasetClear(w http.ResponseWriter, r *http.Request) {
	id, e := s.Jobs.SubmitWithOptions("bangumiDataClear", map[string]any{}, nil, job.SubmitOptions{Title: "bangumi-data 离线索引清除", QueueType: "management", UniqueKey: "bangumi-data-clear-manual"})
	if e != nil {
		httpError(w, 409, e.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"message": "bangumi-data 清除任务已提交", "taskId": id})
}
func (s *Server) metadataDatasetPlatforms(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("bangumi_id")
	rows, e := s.Store.List(r.Context(), "bangumi_data_index", store.Row{"bangumi_id": id}, 1, 0)
	if e != nil {
		metadataError(w, e)
		return
	}
	sites := []any{}
	if len(rows) > 0 {
		if e = json.Unmarshal([]byte(str(rows[0]["sites"])), &sites); e != nil {
			httpError(w, 500, "Stored site mapping is invalid")
			return
		}
	}
	meta := map[string]any{}
	if e = json.Unmarshal([]byte(s.setting(r.Context(), "bangumiDataSiteMeta", "{}")), &meta); e != nil {
		httpError(w, 500, "Stored site metadata is invalid")
		return
	}
	platforms := integration.PlatformURLs(sites, meta)
	if strings.Contains(r.URL.Path, "/danmaku-sources/") {
		for _, item := range platforms {
			item["provider"] = nil
			item["available"] = false
			if name, ok := provider.ProviderForURL(str(item["url"])); ok && s.Providers != nil {
				for _, cap := range s.Providers.Catalog() {
					if cap.Name == name && cap.Comments {
						item["provider"] = name
						item["available"] = true
						break
					}
				}
			}
		}
		writeJSON(w, 200, map[string]any{"bangumiId": id, "sources": platforms})
		return
	}
	writeJSON(w, 200, map[string]any{"bangumiId": id, "platforms": platforms})
}
func (s *Server) metadataAITest(w http.ResponseWriter, r *http.Request) {
	var cfg integration.AIConfig
	if readJSON(r, &cfg) != nil || cfg.Provider == "" || cfg.APIKey == "" || cfg.Model == "" {
		httpError(w, 422, "provider, apiKey and model are required")
		return
	}
	if cfg.BaseURL == "" && cfg.Provider != "gemini" {
		cfg.BaseURL = s.setting(r.Context(), "aiBaseUrl", "")
	}
	start := time.Now()
	_, _, e := s.Metadata.AIComplete(r.Context(), cfg, "", "Hello", false, 10)
	result := map[string]any{"success": e == nil, "message": "AI连接测试成功", "latency": float64(time.Since(start).Microseconds()) / 1000, "error": nil}
	if e != nil {
		result["message"] = "AI连接测试失败"
		result["error"] = e.Error()
	}
	writeJSON(w, 200, result)
}
func (s *Server) metadataAIModels(w http.ResponseWriter, r *http.Request) {
	out, e := s.Metadata.AIModels(r.Context(), r.URL.Query().Get("provider"), boolean(r.URL.Query().Get("refresh")))
	if e != nil {
		metadataError(w, e)
		return
	}
	writeJSON(w, 200, out)
}
func (s *Server) metadataAIClear(w http.ResponseWriter, r *http.Request) {
	if s.setting(r.Context(), "aiMatchEnabled", "false") != "true" || s.setting(r.Context(), "aiCacheEnabled", "true") != "true" {
		httpError(w, 400, "AI匹配器或缓存未启用")
		return
	}
	s.Metadata.ClearAICache()
	writeJSON(w, 200, map[string]any{"success": true, "message": "AI缓存已清空"})
}
func (s *Server) metadataAIRegex(w http.ResponseWriter, r *http.Request) {
	var in map[string]any
	if readJSON(r, &in) != nil {
		httpError(w, 422, "Invalid request")
		return
	}
	out, e := s.Metadata.GenerateRegex(r.Context(), str(in["description"]), str(in["existingRegex"]), str(in["context"]))
	if e != nil {
		metadataError(w, e)
		return
	}
	writeJSON(w, 200, map[string]any{"regex": out})
}
func metadataHours(r *http.Request) (int, error) {
	raw := r.URL.Query().Get("hours")
	if raw == "" {
		return 24, nil
	}
	n, e := strconv.Atoi(raw)
	if e != nil || n < 1 || n > 720 {
		return 0, errors.New("hours must be 1..720")
	}
	return n, nil
}
func (s *Server) metadataMetricRows(ctx context.Context, hours, limit int) ([]store.Row, error) {
	q, ph := s.Store.Quote, s.Store.Placeholder
	query := "SELECT * FROM " + q("ai_metrics_log")
	args := []any{}
	if hours > 0 {
		query += " WHERE " + q("timestamp") + ">=" + ph(1)
		args = append(args, time.Now().Add(-time.Duration(hours)*time.Hour).In(s.authLocation()).Format("2006-01-02 15:04:05.000000"))
	}
	query += " ORDER BY " + q("timestamp") + " DESC, " + q("id") + " DESC"
	if limit > 0 {
		query += " LIMIT " + strconv.Itoa(limit)
	}
	rows, e := s.Store.DB.QueryContext(ctx, query, args...)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []store.Row{}
	for rows.Next() {
		v, e := store.ScanRow(rows, store.Schema["ai_metrics_log"])
		if e != nil {
			return nil, e
		}
		out = append(out, v)
	}
	return out, rows.Err()
}
func (s *Server) metadataAIRecent(w http.ResponseWriter, r *http.Request) {
	limit := 20
	if v := r.URL.Query().Get("limit"); v != "" {
		n, e := strconv.Atoi(v)
		if e != nil || n < 1 || n > 100 {
			httpError(w, 422, "limit must be 1..100")
			return
		}
		limit = n
	}
	rows, e := s.metadataMetricRows(r.Context(), 0, limit)
	if e != nil {
		metadataError(w, e)
		return
	}
	out := []map[string]any{}
	for _, v := range rows {
		out = append(out, camelRow(v))
	}
	writeJSON(w, 200, out)
}
func metricSummary(rows []store.Row, hours int) map[string]any {
	var success, tokens, hits, duration int64
	for _, r := range rows {
		if boolean(r["success"]) {
			success++
		}
		tokens += number(r["tokens_used"])
		duration += number(r["duration_ms"])
		if boolean(r["cache_hit"]) {
			hits++
		}
	}
	n := int64(len(rows))
	successRate, hitRate, average := float64(0), float64(0), float64(0)
	if n > 0 {
		successRate = float64(success) / float64(n)
		hitRate = float64(hits) / float64(n)
		average = float64(duration) / float64(n)
	}
	return map[string]any{"totalCalls": n, "successCalls": success, "successRate": successRate * 100, "totalTokens": tokens, "cacheHits": hits, "cacheHitRate": hitRate * 100, "avgDurationMs": average, "hours": hours}
}
func (s *Server) metadataAIStats(w http.ResponseWriter, r *http.Request) {
	hours, e := metadataHours(r)
	if e != nil {
		httpError(w, 422, e.Error())
		return
	}
	rows, e := s.metadataMetricRows(r.Context(), hours, 100000)
	if e != nil {
		metadataError(w, e)
		return
	}
	writeJSON(w, 200, metricSummary(rows, hours))
}
func (s *Server) metadataAIMetrics(w http.ResponseWriter, r *http.Request) {
	hours, e := metadataHours(r)
	if e != nil {
		httpError(w, 422, e.Error())
		return
	}
	source := r.URL.Query().Get("source")
	if source == "" {
		source = "db"
	}
	var rows []store.Row
	if source == "memory" {
		rows = []store.Row{}
		for _, m := range s.Metadata.AIMetrics(hours) {
			rows = append(rows, store.Row{"method": m.Method, "success": m.Success, "tokens_used": m.TokensUsed, "duration_ms": m.DurationMS, "cache_hit": m.CacheHit, "error": m.Error, "timestamp": m.Timestamp.Format(time.RFC3339)})
		}
	} else if source == "db" {
		rows, e = s.metadataMetricRows(r.Context(), hours, 100000)
		if e != nil {
			metadataError(w, e)
			return
		}
	} else {
		httpError(w, 422, "source must be db or memory")
		return
	}
	summary := metricSummary(rows, hours)
	byMethod := map[string]any{}
	errorsList := []map[string]any{}
	for _, row := range rows {
		method := str(row["method"])
		v, ok := byMethod[method].(map[string]any)
		if !ok {
			v = map[string]any{"calls": int64(0), "success": int64(0), "tokens": int64(0), "duration_ms": int64(0), "cache_hits": int64(0)}
		}
		v["calls"] = number(v["calls"]) + 1
		if boolean(row["success"]) {
			v["success"] = number(v["success"]) + 1
		} else if len(errorsList) < 10 {
			errorsList = append(errorsList, map[string]any{"timestamp": row["timestamp"], "method": method, "error": row["error"]})
		}
		v["tokens"] = number(v["tokens"]) + number(row["tokens_used"])
		v["duration_ms"] = number(v["duration_ms"]) + number(row["duration_ms"])
		if boolean(row["cache_hit"]) {
			v["cache_hits"] = number(v["cache_hits"]) + 1
		}
		byMethod[method] = v
	}
	stats := map[string]any{"period_hours": hours, "total_calls": summary["totalCalls"], "success_rate": summary["successRate"].(float64) / 100, "total_tokens": summary["totalTokens"], "avg_duration_ms": summary["avgDurationMs"], "cache_hit_rate": summary["cacheHitRate"].(float64) / 100, "by_method": byMethod, "errors": errorsList}
	writeJSON(w, 200, map[string]any{"ai_stats": stats, "cache_stats": s.Metadata.AICacheStats(), "source": source})
}
func (s *Server) metadataAILowConfidence(w http.ResponseWriter, r *http.Request) {
	out := []any{}
	if json.Unmarshal([]byte(s.setting(r.Context(), "ai_low_confidence_matches", "[]")), &out) != nil {
		out = []any{}
	}
	if len(out) > 50 {
		out = out[len(out)-50:]
	}
	writeJSON(w, 200, out)
}

// loadMetadataResponseLogging restores only committed, per-provider opt-ins.
func (s *Server) loadMetadataResponseLogging(ctx context.Context) error {
	logging := make(map[string]bool, len(integration.Providers))
	for _, p := range integration.Providers {
		row, err := s.Store.Get(ctx, "metadata_sources", p)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		logging[p] = boolean(row["log_raw_responses"])
	}
	s.Metadata.SetResponseLoggingBatch(logging)
	return nil
}
