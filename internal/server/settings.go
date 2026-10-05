// SPDX-License-Identifier: AGPL-3.0-only
// config_schema.json is adapted from pinned Misaka CONFIG_SCHEMA, with explicit
// native cache bounds and operator-provided Fanart credentials.
package server

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"github.com/AniBakaBaka/AniDan/internal/proxyroute"
	"github.com/AniBakaBaka/AniDan/internal/store"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

//go:embed config_schema.json
var parameterSchema []byte

func (s *Server) registerSettings(m *http.ServeMux) {
	m.HandleFunc("GET /api/ui/config/schema/parameters", s.operator(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Write(parameterSchema)
	}))
	m.HandleFunc("GET /api/control/settings/danmaku-output", s.operator(s.outputSettingsGet))
	m.HandleFunc("PUT /api/control/settings/danmaku-output", s.operator(s.outputSettingsPut))
	m.HandleFunc("GET /api/control/config", s.operator(s.controlConfigGet))
	m.HandleFunc("PUT /api/control/config", s.operator(s.controlConfigPut))
	m.HandleFunc("GET /api/ui/config/proxy", s.operator(s.proxyGet))
	m.HandleFunc("PUT /api/ui/config/proxy", s.operator(s.proxyPut))
	for _, key := range []string{"matchFallbackTokens", "posterProxyTokens", "searchFallbackEnabled"} {
		m.HandleFunc("GET /api/ui/config/"+key, s.operator(func(w http.ResponseWriter, r *http.Request) {
			key := strings.TrimPrefix(r.URL.Path, "/api/ui/config/")
			def := "[]"
			if key == "searchFallbackEnabled" {
				def = "false"
			}
			writeJSON(w, 200, map[string]any{"value": s.setting(r.Context(), key, def)})
		}))
		m.HandleFunc("PUT /api/ui/config/"+key, s.operator(func(w http.ResponseWriter, r *http.Request) {
			r.SetPathValue("config_key", strings.TrimPrefix(r.URL.Path, "/api/ui/config/"))
			s.configPut(w, r)
		}))
	}
	m.HandleFunc("GET /api/ui/config/tmdbReverseLookup", s.operator(s.reverseLookupGet))
	m.HandleFunc("POST /api/ui/config/tmdbReverseLookup", s.operator(s.reverseLookupPut))
	m.HandleFunc("GET /api/control/tasks/{taskId}/execution", s.operator(s.executionTask))
	m.HandleFunc("GET /api/ui/comment/{episodeId}", s.uiComments)
	m.HandleFunc("POST /api/ui/cache/clear", s.operator(s.cacheClear))
}
func (s *Server) setSettingsAtomic(ctx context.Context, values map[string]string) error {
	tx, e := s.Store.DB.BeginTx(ctx, nil)
	if e != nil {
		return e
	}
	defer tx.Rollback()
	keys := []string{}
	for k := range values {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		var n int
		if e = tx.QueryRowContext(ctx, s.Store.Rebind("SELECT COUNT(*) FROM config WHERE config_key=?"), k).Scan(&n); e != nil {
			return e
		}
		if n > 0 {
			_, e = tx.ExecContext(ctx, s.Store.Rebind("UPDATE config SET config_value=? WHERE config_key=?"), values[k], k)
		} else {
			_, e = s.Store.InsertTx(ctx, tx, "config", store.Row{"config_key": k, "config_value": values[k]})
		}
		if e != nil {
			return e
		}
	}
	return tx.Commit()
}
func (s *Server) outputSettingsGet(w http.ResponseWriter, r *http.Request) {
	n, _ := strconv.Atoi(s.setting(r.Context(), "danmakuOutputLimitPerSource", "-1"))
	writeJSON(w, 200, map[string]any{"limit_per_source": n, "merge_output_enabled": boolean(s.setting(r.Context(), "danmakuMergeOutputEnabled", "false"))})
}
func (s *Server) outputSettingsPut(w http.ResponseWriter, r *http.Request) {
	var in map[string]any
	if e := readJSON(r, &in); e != nil {
		httpError(w, 400, e.Error())
		return
	}
	limit, ok := in["limit_per_source"]
	if !ok {
		limit, ok = in["limitPerSource"]
	}
	merge, exists := in["merge_output_enabled"]
	if !exists {
		merge, exists = in["mergeOutputEnabled"]
	}
	if !ok || !exists || number(limit) < -1 || number(limit) > 1000000 {
		httpError(w, 422, "limit_per_source and merge_output_enabled required")
		return
	}
	if _, ok := merge.(bool); !ok {
		httpError(w, 422, "merge_output_enabled must be boolean")
		return
	}
	if e := s.setSettingsAtomic(r.Context(), map[string]string{"danmakuOutputLimitPerSource": str(limit), "danmakuMergeOutputEnabled": strconv.FormatBool(boolean(merge))}); e != nil {
		httpError(w, 500, e.Error())
		return
	}
	s.action(w, r, "弹幕输出设置已更新。")
}

var controlSettings = map[string]string{"webhookEnabled": "boolean", "webhookDelayedImportEnabled": "boolean", "webhookDelayedImportHours": "integer", "webhookFilterMode": "string", "webhookFilterRegex": "string", "titleRecognition": "text", "aiMatchPrompt": "text", "aiRecognitionPrompt": "text", "aiAliasValidationPrompt": "text", "danmakuSourceTagEnabled": "boolean", "danmakuSourceTagAlias": "string"}

func (s *Server) controlConfigGet(w http.ResponseWriter, r *http.Request) {
	keys := []string{}
	for k := range controlSettings {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if r.URL.Query().Get("type") == "help" {
		writeJSON(w, 200, map[string]any{"available_keys": keys, "description": "可通过外部API管理的配置项列表"})
		return
	}
	out := []map[string]any{}
	for _, k := range keys {
		typ := controlSettings[k]
		def := ""
		if typ == "boolean" {
			def = "false"
		} else if typ == "integer" {
			def = "0"
		}
		value := s.setting(r.Context(), k, def)
		if k == "titleRecognition" {
			rows, e := s.Store.List(r.Context(), "title_recognition", nil, 1, 0)
			if e != nil {
				httpError(w, 500, e.Error())
				return
			}
			if len(rows) > 0 {
				value = str(rows[0]["content"])
			}
		}
		out = append(out, map[string]any{"key": k, "value": value, "type": typ, "description": k})
	}
	writeJSON(w, 200, map[string]any{"configs": out})
}
func (s *Server) controlConfigPut(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Key   string `json:"key"`
		Value string `json:"value"`
	}
	if e := readJSON(r, &in); e != nil {
		httpError(w, 400, e.Error())
		return
	}
	typ, ok := controlSettings[in.Key]
	if !ok {
		httpError(w, 400, "Configuration key is not allowed")
		return
	}
	if typ == "boolean" && in.Value != "true" && in.Value != "false" {
		httpError(w, 400, "value must be true or false")
		return
	}
	if typ == "integer" {
		if _, e := strconv.Atoi(in.Value); e != nil {
			httpError(w, 400, "value must be an integer")
			return
		}
	}
	var e error
	if in.Key == "titleRecognition" {
		rows, err := s.Store.List(r.Context(), "title_recognition", nil, 1, 0)
		if err != nil {
			httpError(w, 500, err.Error())
			return
		}
		if len(rows) == 0 {
			_, e = s.Store.Insert(r.Context(), "title_recognition", store.Row{"content": in.Value, "created_at": s.now(), "updated_at": s.now()})
		} else {
			e = s.Store.Update(r.Context(), "title_recognition", rows[0]["id"], store.Row{"content": in.Value, "updated_at": s.now()})
		}
	} else {
		e = s.setSetting(r.Context(), in.Key, in.Value)
	}
	if e != nil {
		httpError(w, 500, e.Error())
		return
	}
	w.WriteHeader(204)
}
func (s *Server) proxyURL(ctx context.Context) (string, error) {
	mode := s.setting(ctx, "proxyMode", "none")
	if mode == "none" && boolean(s.setting(ctx, "proxyEnabled", "false")) {
		mode = "http_socks"
	}
	switch mode {
	case "none":
		return "", nil
	case "http_socks":
		raw := s.setting(ctx, "proxyUrl", "")
		if raw == "" {
			return "", errors.New("proxy is enabled but proxyUrl is empty")
		}
		return raw, nil
	case "accelerate":
		return "", errors.New("Selected accelerate gateway requires the explicit trusted routing flow")
	default:
		return "", errors.New("Unknown proxy mode")
	}
}
func (s *Server) proxyGet(w http.ResponseWriter, r *http.Request) {
	values, err := s.proxySettings(r.Context())
	if err != nil {
		httpError(w, 503, "Proxy configuration is unavailable")
		return
	}
	mode := values["proxyMode"]
	if mode == "" {
		mode = "none"
	}
	enabled := boolean(values["proxyEnabled"])
	if mode == "none" && enabled {
		mode = "http_socks"
	}
	// Invalid imported values may contain URL credentials or unbounded text. Keep
	// them in SQL evidence, but display only a valid canonical gateway here.
	displayGateway, _ := proxyroute.CanonicalGateway(values["accelerateProxyUrl"])
	out := map[string]any{"proxyMode": mode, "proxyEnabled": enabled, "proxyProtocol": "http", "proxyHost": nil, "proxyPort": nil, "proxyUsername": nil, "proxyPassword": nil, "proxySslVerify": true, "accelerateProxyUrl": displayGateway, "accelerateTrusted": false, "accelerateTrustRequired": mode == "accelerate", "accelerateTrustedGateway": nil, "accelerateTrustVersion": 1, "effectiveMode": mode, "accelerateTrustError": nil}
	if u, e := url.Parse(values["proxyUrl"]); e == nil && u.Host != "" {
		out["proxyProtocol"] = u.Scheme
		out["proxyHost"] = u.Hostname()
		port, _ := strconv.Atoi(u.Port())
		out["proxyPort"] = port
		if u.User != nil {
			out["proxyUsername"] = u.User.Username()
			if _, ok := u.User.Password(); ok {
				out["proxyPassword"] = "********"
			}
		}
	}
	if mode == "accelerate" {
		out["effectiveMode"] = "blocked"
		out["accelerateTrustError"] = "approval_required"
		gateway, e := proxyroute.CanonicalGateway(values["accelerateProxyUrl"])
		if e != nil {
			out["accelerateTrustError"] = "gateway_invalid"
		} else {
			out["accelerateProxyUrl"] = gateway
			state := s.proxyRouting.Load()
			if state != nil && state.mode == mode && state.gateway == gateway && state.approvalID != "" && state.approvalID == values[accelerateApprovalKey] && state.blocked == "" {
				out["accelerateTrusted"] = true
				out["accelerateTrustRequired"] = false
				out["accelerateTrustedGateway"] = gateway
				out["effectiveMode"] = "accelerate"
				out["accelerateTrustError"] = nil
			} else if state != nil && state.blocked != "" {
				out["accelerateTrustError"] = state.blocked
			}
		}
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, 200, out)
}
func (s *Server) proxyPut(w http.ResponseWriter, r *http.Request) {
	s.providerConfigMu.Lock()
	defer s.providerConfigMu.Unlock()
	var in map[string]any
	if e := readJSON(r, &in); e != nil {
		httpError(w, 400, e.Error())
		return
	}
	mode := str(in["proxyMode"])
	if mode == "" {
		mode = "none"
	}
	if mode != "none" && mode != "http_socks" && mode != "accelerate" {
		httpError(w, 422, "proxyMode must be none, http_socks or accelerate")
		return
	}
	if v, ok := in["proxySslVerify"]; ok && !boolean(v) {
		httpError(w, 422, "Disabling TLS verification is not supported")
		return
	}
	raw, gateway := "", ""
	var approval *accelerateTrustRecord
	if mode == "http_socks" {
		protocol := str(in["proxyProtocol"])
		if protocol == "" {
			protocol = "http"
		}
		port := number(in["proxyPort"])
		host := str(in["proxyHost"])
		if host == "" || port < 1 || port > 65535 {
			httpError(w, 422, "proxyHost and valid proxyPort required")
			return
		}
		u := url.URL{Scheme: protocol, Host: net.JoinHostPort(host, strconv.FormatInt(port, 10))}
		username, password := str(in["proxyUsername"]), str(in["proxyPassword"])
		if password == "********" {
			old, _ := url.Parse(s.setting(r.Context(), "proxyUrl", ""))
			if old == nil || old.User == nil || old.Scheme != u.Scheme || old.Host != u.Host || old.User.Username() != username {
				httpError(w, 422, "Masked proxy password only preserves the exact saved endpoint and username")
				return
			}
			password, _ = old.User.Password()
		}
		if username != "" {
			u.User = url.UserPassword(username, password)
		}
		raw = u.String()
	}
	if mode == "accelerate" {
		var e error
		gateway, e = proxyroute.CanonicalGateway(strings.TrimSpace(str(in["accelerateProxyUrl"])))
		if e != nil {
			httpError(w, 422, "A valid HTTPS accelerate gateway with no credentials, query or fragment is required")
			return
		}
		if value, ok := in["accelerateTrust"]; ok && value != nil {
			trust, ok := value.(map[string]any)
			if !ok || len(trust) != 2 || str(trust["confirmation"]) != accelerateTrustConfirmation {
				httpError(w, 422, "Explicit gateway trust confirmation is required")
				return
			}
			selected, e := proxyroute.CanonicalGateway(strings.TrimSpace(str(trust["gateway"])))
			if e != nil || selected != gateway {
				httpError(w, 422, "Trust confirmation must identify the exact selected gateway")
				return
			}
			approval = &accelerateTrustRecord{Version: 1, DataRoot: filepath.Clean(s.DataDir), Gateway: gateway, ApprovalID: randomID(), ApprovedAt: time.Now().UTC().Format(time.RFC3339Nano)}
		} else {
			current := s.proxyRouting.Load()
			if current == nil || current.mode != "accelerate" || current.gateway != gateway || current.blocked != "" || current.approvalID == "" {
				httpError(w, 412, "Save requires explicit trust: the gateway receives origin URLs, queries, request bodies, cookies and tokens")
				return
			}
			approval, e = s.readAccelerateTrust()
			if e != nil || approval == nil || approval.Gateway != gateway || approval.ApprovalID != current.approvalID {
				httpError(w, 412, "Local gateway approval is unavailable; explicitly review and trust the gateway again")
				return
			}
		}
	}
	route := proxyroute.Config{ProxyURL: raw, AccelerateURL: gateway}
	if approval != nil {
		route.AuthorizationIdentity = approval.ApprovalID
		route.Authorize = s.accelerateAuthorizer(gateway, approval.ApprovalID)
	}
	if e := proxyroute.Validate(route); e != nil {
		httpError(w, 422, "Invalid outbound proxy configuration")
		return
	}
	for _, name := range s.Providers.Names() {
		if _, e := s.Providers.ConfigSchema(name); e != nil {
			continue
		}
		if e := s.Providers.ValidateConfigurationWithRouting(name, map[string]string{}, route); e != nil {
			httpError(w, 422, "Provider does not support the selected outbound route")
			return
		}
	}
	approvalID := ""
	if approval != nil {
		approvalID = approval.ApprovalID
	}
	if e := s.commitProxyChange(r.Context(), map[string]string{"proxyMode": mode, "proxyUrl": raw, "proxyEnabled": strconv.FormatBool(mode != "none"), "proxySslVerify": "true", "accelerateProxyUrl": gateway, accelerateApprovalKey: approvalID}, approval); e != nil {
		httpError(w, 500, e.Error())
		return
	}
	w.WriteHeader(204)
}

// commitProxyChange is called only with providerConfigMu held and validated values.
func (s *Server) commitProxyChange(ctx context.Context, values map[string]string, approval *accelerateTrustRecord) error {
	mode, raw, gateway, approvalID := values["proxyMode"], values["proxyUrl"], values["accelerateProxyUrl"], values[accelerateApprovalKey]
	// A commit error can be ambiguous. Suspend old gateway authorizations before
	// any persistent change, including revocation; never keep forwarding merely
	// because the database driver could not confirm its final result.
	s.proxyRouting.Store(&proxyRoutingState{mode: "accelerate", blocked: "approval_pending"})
	if e := s.beginProxyChange(ctx); e != nil {
		return errors.New("Cannot durably record the gateway change; no SQL settings were changed. Revocation is unconfirmed: resolve local storage before restarting")
	}
	if e := s.proxyChangeFault("before-sql"); e != nil {
		return errors.New("Proxy change is pending local review; gateway forwarding is suspended")
	}
	if e := s.setSettingsAtomic(ctx, values); e != nil {
		return errors.New("Proxy configuration commit failed or is uncertain; gateway forwarding is suspended until settings are reviewed")
	}
	if e := s.proxyChangeFault("after-sql"); e != nil {
		return errors.New("Proxy change is pending local review; gateway forwarding is suspended")
	}
	// Fail closed between SQL commit, durable local approval and provider activation.
	s.proxyRouting.Store(&proxyRoutingState{mode: "accelerate", blocked: "approval_pending"})
	if e := s.writeAccelerateTrust(ctx, approval); e != nil {
		return errors.New("Proxy settings were saved but local approval is unavailable or uncertain; refresh status before proceeding")
	}
	if e := s.proxyChangeFault("after-local-approval"); e != nil {
		return errors.New("Local approval completion is uncertain; gateway forwarding remains suspended until settings are reviewed")
	}
	saved, e := s.proxySettings(ctx)
	if e != nil || saved["proxyMode"] != mode || saved["proxyUrl"] != raw || saved["accelerateProxyUrl"] != gateway || saved[accelerateApprovalKey] != approvalID {
		return errors.New("Saved proxy configuration could not be verified; pending review retained")
	}
	stored, e := s.readAccelerateTrust()
	if e != nil || approval == nil && stored != nil || approval != nil && (stored == nil || stored.Gateway != approval.Gateway || stored.ApprovalID != approval.ApprovalID) {
		return errors.New("Local gateway approval could not be verified; pending review retained")
	}
	if e = s.finishProxyChange(ctx); e != nil {
		return errors.New("Gateway change finalization is uncertain; forwarding remains suspended and local storage must be reviewed")
	}
	if e := s.loadProviderConfigUnlocked(ctx); e != nil {
		_ = s.beginProxyChange(context.Background())
		return errors.New("Proxy settings were saved but activation failed; refresh status before proceeding")
	}
	return nil
}
func (s *Server) reverseLookupGet(w http.ResponseWriter, r *http.Request) {
	var sources []string
	if json.Unmarshal([]byte(s.setting(r.Context(), "tmdbReverseLookupSources", `["imdb","tvdb"]`)), &sources) != nil {
		sources = []string{"imdb", "tvdb"}
	}
	writeJSON(w, 200, map[string]any{"enabled": boolean(s.setting(r.Context(), "tmdbReverseLookupEnabled", "false")), "sources": sources})
}
func (s *Server) reverseLookupPut(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Enabled bool     `json:"enabled"`
		Sources []string `json:"sources"`
	}
	if e := readJSON(r, &in); e != nil {
		httpError(w, 400, e.Error())
		return
	}
	for _, v := range in.Sources {
		if v != "imdb" && v != "tvdb" {
			httpError(w, 400, "sources must be imdb or tvdb")
			return
		}
	}
	b, _ := json.Marshal(in.Sources)
	if e := s.setSettingsAtomic(r.Context(), map[string]string{"tmdbReverseLookupEnabled": strconv.FormatBool(in.Enabled), "tmdbReverseLookupSources": string(b)}); e != nil {
		httpError(w, 500, e.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"message": "设置已保存"})
}
func (s *Server) executionTask(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("taskId")
	task, ok := s.Jobs.Get(id)
	if !ok {
		httpError(w, 404, "Task not found")
		return
	}
	var result map[string]any
	_ = json.Unmarshal(task.Result, &result)
	out := map[string]any{"schedulerTaskId": id, "executionTaskId": nil, "status": nil}
	if child := str(result["executionTaskId"]); child != "" {
		out["executionTaskId"] = child
		if t, ok := s.Jobs.Get(child); ok {
			out["status"] = t.Status
		}
	}
	writeJSON(w, 200, out)
}
