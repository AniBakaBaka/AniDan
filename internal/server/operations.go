// SPDX-License-Identifier: AGPL-3.0-only
package server

import (
	"errors"
	"github.com/AniBakaBaka/AniDan/internal/job"
	"github.com/AniBakaBaka/AniDan/internal/store"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

func (s *Server) operator(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var e error
		if strings.HasPrefix(r.URL.Path, "/api/control/") {
			e = s.requireControl(r)
		} else {
			_, e = s.requireUser(r)
		}
		if e != nil {
			httpError(w, 401, "Authentication required")
			return
		}
		h(w, r)
	}
}
func (s *Server) registerOperations(m *http.ServeMux) {
	m.HandleFunc("GET /api/health", s.health)
	for _, p := range []string{"/api/ui", "/api/control"} {
		m.HandleFunc("GET "+p+"/tokens", s.operator(s.tokensList))
		m.HandleFunc("POST "+p+"/tokens", s.operator(s.tokensCreate))
		m.HandleFunc("GET "+p+"/tokens/{tokenId}", s.operator(s.tokenGet))
		m.HandleFunc("PUT "+p+"/tokens/{tokenId}", s.operator(s.tokenUpdate))
		m.HandleFunc("DELETE "+p+"/tokens/{tokenId}", s.operator(s.tokenDelete))
		m.HandleFunc("PUT "+p+"/tokens/{tokenId}/toggle", s.operator(s.tokenToggle))
		m.HandleFunc("POST "+p+"/tokens/{tokenId}/reset", s.operator(s.tokenReset))
		m.HandleFunc("GET "+p+"/tokens/{tokenId}/logs", s.operator(s.tokenLogs))
		m.HandleFunc("GET "+p+"/tasks", s.operator(s.tasksList))
		m.HandleFunc("GET "+p+"/tasks/{taskId}", s.operator(s.taskGet))
		m.HandleFunc("DELETE "+p+"/tasks/{taskId}", s.operator(s.taskDelete))
		m.HandleFunc("POST "+p+"/tasks/{taskId}/pause", s.operator(s.taskPause))
		m.HandleFunc("POST "+p+"/tasks/{taskId}/resume", s.operator(s.taskResume))
		m.HandleFunc("POST "+p+"/tasks/{taskId}/abort", s.operator(s.taskAbort))
		m.HandleFunc("POST "+p+"/tasks/{taskId}/retry", s.operator(s.taskRetry))
	}
	m.HandleFunc("GET /api/ui/config/{config_key}", s.operator(s.configGet))
	m.HandleFunc("PUT /api/ui/config/{config_key}", s.operator(s.configPut))
	m.HandleFunc("POST /api/ui/config/externalApiKey/regenerate", s.operator(s.regenerateKey))
	m.HandleFunc("POST /api/ui/config/webhookApiKey/regenerate", s.operator(s.regenerateKey))
	m.HandleFunc("GET /api/ui/cache/stats", s.operator(s.cacheStats))
	m.HandleFunc("GET /api/ui/cache/list", s.operator(s.cacheList))
	m.HandleFunc("GET /api/ui/cache/detail", s.operator(s.cacheDetail))
	m.HandleFunc("DELETE /api/ui/cache/key", s.operator(s.cacheDelete))
	m.HandleFunc("DELETE /api/ui/cache/clear", s.operator(s.cacheClear))
	m.HandleFunc("GET /api/ui/scrapers", s.operator(s.scrapersList))
	m.HandleFunc("GET /api/ui/scrapers/load-check", s.operator(s.scrapersLoadCheck))
	m.HandleFunc("PUT /api/ui/scrapers", s.operator(s.scrapersUpdate))
	m.HandleFunc("GET /api/ui/ua-rules", s.operator(s.uaList))
	m.HandleFunc("POST /api/ui/ua-rules", s.operator(s.uaCreate))
	m.HandleFunc("DELETE /api/ui/ua-rules/{ruleId}", s.operator(s.uaDelete))
}
func camel(s string) string {
	p := strings.Split(s, "_")
	for i := 1; i < len(p); i++ {
		if p[i] != "" {
			p[i] = strings.ToUpper(p[i][:1]) + p[i][1:]
		}
	}
	return strings.Join(p, "")
}
func camelRow(r store.Row) map[string]any {
	m := map[string]any{}
	for k, v := range r {
		m[camel(k)] = v
	}
	return m
}
func tokenView(r store.Row) map[string]any {
	return map[string]any{"id": r["id"], "name": r["name"], "token": r["token"], "isEnabled": boolean(r["is_enabled"]), "createdAt": r["created_at"], "expiresAt": r["expires_at"], "dailyCallLimit": r["daily_call_limit"], "dailyCallCount": r["daily_call_count"]}
}
func (s *Server) action(w http.ResponseWriter, r *http.Request, message string) {
	if strings.HasPrefix(r.URL.Path, "/api/control/") {
		writeJSON(w, 200, map[string]any{"status": "success", "message": message, "animeId": nil, "sourceId": nil})
	} else {
		w.WriteHeader(204)
	}
}
func (s *Server) tokensList(w http.ResponseWriter, r *http.Request) {
	rows, e := s.Store.List(r.Context(), "api_tokens", nil, 10000, 0)
	if e != nil {
		httpError(w, 500, e.Error())
		return
	}
	out := []any{}
	for _, v := range rows {
		out = append(out, tokenView(v))
	}
	writeJSON(w, 200, out)
}

var customTokenRE = regexp.MustCompile(`^[a-zA-Z0-9_-]{5,100}$`)

func (s *Server) tokenPayload(r *http.Request, update bool) (store.Row, error) {
	var in map[string]any
	if e := readJSON(r, &in); e != nil {
		return nil, e
	}
	name := str(in["name"])
	if name == "" || len([]rune(name)) > 50 {
		return nil, errors.New("name must contain 1..50 characters")
	}
	limit := int64(500)
	if x, ok := in["dailyCallLimit"]; ok {
		limit = number(x)
	}
	if limit < -1 {
		return nil, errors.New("dailyCallLimit must be -1 or nonnegative")
	}
	row := store.Row{"name": name, "daily_call_limit": limit}
	period := str(in["validityPeriod"])
	if period == "" {
		if update {
			period = "custom"
		} else {
			period = "permanent"
		}
	}
	if period != "custom" {
		if period == "permanent" {
			row["expires_at"] = nil
		} else {
			days, e := strconv.Atoi(strings.TrimSuffix(period, "d"))
			if e != nil || days < 1 || days > 36500 {
				return nil, errors.New("invalid validityPeriod")
			}
			loc, _ := time.LoadLocation(s.Config.Timezone)
			row["expires_at"] = time.Now().In(loc).AddDate(0, 0, days).Format("2006-01-02T15:04:05")
		}
	}
	if token := str(in["customToken"]); token != "" {
		if !customTokenRE.MatchString(token) {
			return nil, errors.New("customToken must be 5..100 letters, digits, underscore or hyphen")
		}
		row["token"] = token
	}
	return row, nil
}
func (s *Server) tokensCreate(w http.ResponseWriter, r *http.Request) {
	row, e := s.tokenPayload(r, false)
	if e != nil {
		httpError(w, 400, e.Error())
		return
	}
	if row["token"] == nil {
		row["token"] = randomID()
	}
	row["is_enabled"] = true
	row["created_at"] = s.now()
	row["daily_call_count"] = 0
	id, e := s.Store.Insert(r.Context(), "api_tokens", row)
	if e != nil {
		httpError(w, 409, "Token could not be created; name or token may already exist")
		return
	}
	out, e := s.Store.Get(r.Context(), "api_tokens", id)
	if e != nil {
		httpError(w, 500, e.Error())
		return
	}
	writeJSON(w, 201, tokenView(out))
}
func (s *Server) tokenRow(w http.ResponseWriter, r *http.Request) (store.Row, int64, bool) {
	id, e := idParam(r, "tokenId")
	if e != nil {
		httpError(w, 400, e.Error())
		return nil, 0, false
	}
	row, e := s.Store.Get(r.Context(), "api_tokens", id)
	if e != nil || row == nil {
		httpError(w, 404, "Token not found")
		return nil, 0, false
	}
	return row, id, true
}
func (s *Server) tokenGet(w http.ResponseWriter, r *http.Request) {
	row, _, ok := s.tokenRow(w, r)
	if ok {
		writeJSON(w, 200, tokenView(row))
	}
}
func (s *Server) tokenUpdate(w http.ResponseWriter, r *http.Request) {
	_, id, ok := s.tokenRow(w, r)
	if !ok {
		return
	}
	row, e := s.tokenPayload(r, true)
	if e != nil {
		httpError(w, 400, e.Error())
		return
	}
	if e = s.Store.Update(r.Context(), "api_tokens", id, row); e != nil {
		httpError(w, 409, "Token update conflict")
		return
	}
	s.action(w, r, "Token信息更新成功。")
}
func (s *Server) tokenDelete(w http.ResponseWriter, r *http.Request) {
	_, id, ok := s.tokenRow(w, r)
	if !ok {
		return
	}
	if e := s.Store.Delete(r.Context(), "api_tokens", id); e != nil {
		httpError(w, 500, e.Error())
		return
	}
	s.action(w, r, "Token 删除成功。")
}
func (s *Server) tokenToggle(w http.ResponseWriter, r *http.Request) {
	row, id, ok := s.tokenRow(w, r)
	if !ok {
		return
	}
	if e := s.Store.Update(r.Context(), "api_tokens", id, store.Row{"is_enabled": !boolean(row["is_enabled"])}); e != nil {
		httpError(w, 500, e.Error())
		return
	}
	s.action(w, r, "Token状态已更新。")
}
func (s *Server) tokenReset(w http.ResponseWriter, r *http.Request) {
	_, id, ok := s.tokenRow(w, r)
	if !ok {
		return
	}
	if e := s.Store.Update(r.Context(), "api_tokens", id, store.Row{"daily_call_count": 0}); e != nil {
		httpError(w, 500, e.Error())
		return
	}
	s.action(w, r, "Token调用次数已重置为0。")
}
func (s *Server) tokenLogs(w http.ResponseWriter, r *http.Request) {
	_, id, ok := s.tokenRow(w, r)
	if !ok {
		return
	}
	rows, e := s.Store.List(r.Context(), "token_access_logs", store.Row{"token_id": id}, 1000, 0)
	if e != nil {
		httpError(w, 500, e.Error())
		return
	}
	out := []any{}
	for _, v := range rows {
		out = append(out, camelRow(v))
	}
	writeJSON(w, 200, out)
}
func pageParams(r *http.Request) (page, size int) {
	page, _ = strconv.Atoi(r.URL.Query().Get("page"))
	size, _ = strconv.Atoi(r.URL.Query().Get("pageSize"))
	if page < 1 {
		page = 1
	}
	if page > 1000000 {
		page = 1000000
	}
	if size < 1 {
		size = 20
	}
	if size > 1000 {
		size = 1000
	}
	return
}
func (s *Server) tasksList(w http.ResponseWriter, r *http.Request) {
	all := s.Jobs.List()
	out := []any{}
	search := strings.ToLower(r.URL.Query().Get("search"))
	status := r.URL.Query().Get("status")
	queue := r.URL.Query().Get("queueType")
	for _, t := range all {
		if search != "" && !strings.Contains(strings.ToLower(t.Title), search) {
			continue
		}
		done := t.Status == "已完成" || t.Status == "失败"
		if status == "completed" && !done || status == "in_progress" && done {
			continue
		}
		if queue != "" && queue != "all" && t.QueueType != queue {
			continue
		}
		out = append(out, t)
	}
	if strings.HasPrefix(r.URL.Path, "/api/control/") {
		writeJSON(w, 200, out)
		return
	}
	total := len(out)
	page, size := pageParams(r)
	start := (page - 1) * size
	if start > total {
		start = total
	}
	end := start + size
	if end > total {
		end = total
	}
	if err := s.localImportWarningSummaries(r.Context(), out[start:end]); err != nil {
		httpError(w, 500, "Unable to read local import warnings")
		return
	}
	writeJSON(w, 200, map[string]any{"total": total, "list": out[start:end]})
}
func (s *Server) taskGet(w http.ResponseWriter, r *http.Request) {
	t, ok := s.Jobs.Get(r.PathValue("taskId"))
	if !ok {
		httpError(w, 404, "Task not found")
		return
	}
	writeJSON(w, 200, t)
}
func (s *Server) taskPause(w http.ResponseWriter, r *http.Request) {
	if e := s.Jobs.Pause(r.PathValue("taskId")); e != nil {
		httpError(w, 409, e.Error())
		return
	}
	s.action(w, r, "任务已暂停。")
}
func (s *Server) taskResume(w http.ResponseWriter, r *http.Request) {
	if e := s.Jobs.Resume(r.PathValue("taskId")); e != nil {
		httpError(w, 409, e.Error())
		return
	}
	s.action(w, r, "任务已恢复。")
}
func (s *Server) taskAbort(w http.ResponseWriter, r *http.Request) {
	if e := s.Jobs.Cancel(r.PathValue("taskId")); e != nil {
		httpError(w, 409, e.Error())
		return
	}
	s.action(w, r, "中止任务的请求已发送。")
}
func (s *Server) taskRetry(w http.ResponseWriter, r *http.Request) {
	if task, ok := s.Jobs.Get(r.PathValue("taskId")); ok && notificationTaskNeedsReview(task) {
		httpError(w, 409, "Legacy notification requires manual audience/attempt review; retained task was not retried")
		return
	}
	id, e := s.Jobs.Retry(r.PathValue("taskId"))
	if e != nil {
		httpError(w, 409, e.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"message": "任务已重新提交", "newTaskId": id})
}
func (s *Server) taskDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("taskId")
	if t, ok := s.Jobs.Get(id); ok && (t.Status == "运行中" || t.Status == "已暂停" || t.Status == "排队中") {
		if e := s.Jobs.Cancel(id); e != nil {
			httpError(w, 409, e.Error())
			return
		}
	}
	tx, err := s.Store.DB.BeginTx(r.Context(), nil)
	if err != nil {
		httpError(w, 500, "Cannot remove task history")
		return
	}
	defer tx.Rollback()
	for _, statement := range []struct {
		q    string
		args []any
	}{{"DELETE FROM config WHERE config_key IN (?,?)", []any{"anidan.job.result." + id, job.ParentMetadataPrefix + id}}, {"DELETE FROM task_state_cache WHERE task_id=?", []any{id}}, {"DELETE FROM task_history WHERE id=?", []any{id}}} {
		if _, err = tx.ExecContext(r.Context(), s.Store.Rebind(statement.q), statement.args...); err != nil {
			httpError(w, 500, "Cannot remove task history")
			return
		}
	}
	if err = tx.Commit(); err != nil {
		httpError(w, 500, "Cannot commit task deletion")
		return
	}
	s.action(w, r, "删除任务的请求已处理。")
}
func (s *Server) configGet(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("config_key")
	if !validPublicConfigKey(key) {
		httpError(w, 422, "Configuration keys must use printable ASCII without whitespace")
		return
	}
	if protectedDiagnosticKey(key) {
		httpError(w, 403, "Use the dedicated, redacted settings or diagnostics endpoint")
		return
	}
	if strings.EqualFold(key, "proxyUrl") {
		key = "proxyUrl"
	}
	value := s.setting(r.Context(), key, "")
	if sensitiveDiagnosticKey(key) && value != "" {
		value = compatSecretMask
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, 200, map[string]any{"key": key, "value": value})
}
func (s *Server) configPut(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("config_key")
	if !validPublicConfigKey(key) {
		httpError(w, 422, "Configuration keys must use printable ASCII without whitespace")
		return
	}
	if strings.EqualFold(key, "proxyUrl") {
		s.legacyHTTPProxyURLPut(w, r)
		return
	}
	if proxyConfigurationKey(key) {
		httpError(w, 422, "Proxy configuration and trust must be changed through the dedicated proxy settings flow")
		return
	}
	if key == "mysqlBinlogRetentionDays" || key == "scraperAutoUpdateInterval" {
		httpError(w, 501, "This legacy setting is preserved for migration but not active in the native runtime; manage database binlogs externally or update the native application build")
		return
	}
	var in map[string]any
	if e := readJSON(r, &in); e != nil {
		httpError(w, 400, e.Error())
		return
	}
	v, ok := in["value"]
	if !ok || v == nil {
		httpError(w, 400, "Missing 'value' in request body")
		return
	}
	if sensitiveDiagnosticKey(key) && str(v) == compatSecretMask {
		if s.setting(r.Context(), key, "") == "" {
			httpError(w, 400, "No saved secret exists to preserve")
			return
		}
		w.WriteHeader(204)
		return
	}
	if isCacheTTLKey(key) {
		n, e := strconv.Atoi(str(v))
		if e != nil || n < 0 || n > 604800 {
			httpError(w, 422, "Cache TTL must be 0..604800 seconds")
			return
		}
	}
	if key == "jwtExpireMinutes" && (number(v) < 1 || number(v) > 43200) {
		httpError(w, 400, "JWT 有效期必须在 1～43200 分钟之间")
		return
	}
	if err := notificationAggregationSetting(key, str(v)); err != nil {
		httpError(w, 422, err.Error())
		return
	}
	if err := validatePredownloadSetting(key, str(v)); err != nil {
		httpError(w, 422, err.Error())
		return
	}
	if e := s.setSettingWithHistory(r.Context(), key, str(v), "ui"); e != nil {
		httpError(w, 500, e.Error())
		return
	}
	if strings.HasPrefix(key, "notificationSurge") {
		_ = s.reloadNotificationAggregation(r.Context())
	}
	if key == "preDownloadNextEpisodeEnabled" || key == "searchFallbackEnabled" || key == "matchFallbackEnabled" {
		s.cancelDisabledPredownload(r.Context())
	}
	w.WriteHeader(204)
}
func (s *Server) regenerateKey(w http.ResponseWriter, r *http.Request) {
	key := "externalApiKey"
	if strings.Contains(r.URL.Path, "webhookApiKey") {
		key = "webhookApiKey"
	}
	v := randomID() + randomID()
	if e := s.setSetting(r.Context(), key, v); e != nil {
		httpError(w, 500, e.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"key": key, "value": v})
}
func (s *Server) scrapersList(w http.ResponseWriter, r *http.Request) {
	rows, e := s.Store.List(r.Context(), "scrapers", nil, 1000, 0)
	if e != nil {
		httpError(w, 500, e.Error())
		return
	}
	out := []any{}
	for _, v := range rows {
		row := camelRow(v)
		_, available := s.Providers.Get(str(v["provider_name"]))
		row["available"] = available
		fields := map[string]any{}
		if schema, e := s.Providers.ConfigSchema(str(v["provider_name"])); e == nil {
			for _, f := range schema {
				if f.Key == "proxyURL" || f.Key == "logRawResponses" {
					continue
				}
				fields[f.Key] = []any{f.Title, f.Type, f.Description, map[string]any{"secret": f.Secret}}
			}
		}
		row["configurableFields"] = fields
		row["isLoggable"] = false
		row["logRawResponses"] = false
		if cfg, err := s.Providers.Configuration(str(v["provider_name"])); err == nil {
			_, row["isLoggable"] = cfg["logRawResponses"]
			row["logRawResponses"] = cfg["logRawResponses"] == "true"
		}
		row["version"] = Version
		row["displayName"] = v["provider_name"]
		row["actions"] = []any{}
		row["episodeBlacklistRegex"] = s.setting(r.Context(), str(v["provider_name"])+"_episode_blacklist_regex", "")
		row["searchTimeout"] = number(s.setting(r.Context(), "anidan.source."+str(v["provider_name"])+".timeoutSeconds", "20"))
		row["verificationEnabled"] = false
		row["isEnabled"] = boolean(v["is_enabled"])
		row["useProxy"] = boolean(v["use_proxy"])
		out = append(out, row)
	}
	writeJSON(w, 200, out)
}
func (s *Server) scrapersLoadCheck(w http.ResponseWriter, r *http.Request) {
	missing := map[string]string{}
	for _, cap := range s.Providers.Catalog() {
		if !cap.Search || !cap.Episodes || !cap.Comments {
			missing[cap.Name] = cap.Blocker
		}
	}
	writeJSON(w, 200, map[string]any{"appVersion": Version, "globalSkip": nil, "skipped": missing, "ok": len(missing) == 0, "capabilities": s.Providers.Catalog()})
}

func (s *Server) scrapersUpdate(w http.ResponseWriter, r *http.Request) {
	s.providerConfigMu.Lock()
	defer s.providerConfigMu.Unlock()
	var in []map[string]any
	if e := readJSON(r, &in); e != nil {
		httpError(w, 400, e.Error())
		return
	}
	if len(in) > 100 {
		httpError(w, 422, "Too many provider settings")
		return
	}
	for _, v := range in {
		name := str(v["providerName"])
		if name == "" {
			httpError(w, 400, "providerName required")
			return
		}
		if _, e := s.Store.Get(r.Context(), "scrapers", name); e != nil {
			httpError(w, 404, "Provider settings not found")
			return
		}
		if _, e := s.Providers.ConfigSchema(name); e == nil {
			if e := s.Providers.ValidateConfigurationWithRouting(name, map[string]string{}, s.selectedProxyRouting(boolean(v["useProxy"]))); e != nil {
				httpError(w, 422, "Invalid provider route")
				return
			}
		}
	}
	tx, e := s.Store.DB.BeginTx(r.Context(), nil)
	if e != nil {
		httpError(w, 500, "Provider settings could not be changed")
		return
	}
	defer tx.Rollback()
	for _, v := range in {
		if _, e = tx.ExecContext(r.Context(), s.Store.Rebind("UPDATE scrapers SET is_enabled=?,display_order=?,use_proxy=? WHERE provider_name=?"), boolean(v["isEnabled"]), number(v["displayOrder"]), boolean(v["useProxy"]), str(v["providerName"])); e != nil {
			httpError(w, 500, "Provider settings could not be changed")
			return
		}
	}
	if e = tx.Commit(); e != nil {
		httpError(w, 500, "Provider settings commit is uncertain")
		return
	}
	if e = s.loadProviderConfigUnlocked(r.Context()); e != nil {
		httpError(w, 500, "Provider settings saved but runtime activation failed")
		return
	}
	w.WriteHeader(204)
}
func (s *Server) uaList(w http.ResponseWriter, r *http.Request) {
	rows, e := s.Store.List(r.Context(), "ua_rules", nil, 10000, 0)
	if e != nil {
		httpError(w, 500, e.Error())
		return
	}
	out := []any{}
	for _, v := range rows {
		out = append(out, camelRow(v))
	}
	writeJSON(w, 200, out)
}
func (s *Server) uaCreate(w http.ResponseWriter, r *http.Request) {
	var in map[string]any
	if e := readJSON(r, &in); e != nil {
		httpError(w, 400, e.Error())
		return
	}
	ua := str(in["uaString"])
	if ua == "" || len(ua) > 500 {
		httpError(w, 400, "uaString must contain 1..500 characters")
		return
	}
	id, e := s.Store.Insert(r.Context(), "ua_rules", store.Row{"ua_string": ua, "created_at": s.now()})
	if e != nil {
		httpError(w, 409, "User agent rule already exists")
		return
	}
	row, _ := s.Store.Get(r.Context(), "ua_rules", id)
	writeJSON(w, 201, camelRow(row))
}
func (s *Server) uaDelete(w http.ResponseWriter, r *http.Request) {
	id, e := idParam(r, "ruleId")
	if e != nil {
		httpError(w, 400, e.Error())
		return
	}
	if e = s.Store.Delete(r.Context(), "ua_rules", id); e != nil {
		httpError(w, 500, e.Error())
		return
	}
	w.WriteHeader(204)
}
