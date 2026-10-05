// SPDX-License-Identifier: AGPL-3.0-only
// Endpoint/model contracts adapted from the pinned upstream UI parameters,
// config_extra, and settings modules. Secrets and unsupported options fail closed.
package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/AniBakaBaka/AniDan/internal/integration"
)

const compatSecretMask = "********"

func (s *Server) registerCompatSettings(m *http.ServeMux) {
	m.HandleFunc("GET /api/ui/config/github-token", s.operator(s.compatGitHubGet))
	m.HandleFunc("POST /api/ui/config/github-token", s.operator(s.compatGitHubSave))
	m.HandleFunc("POST /api/ui/config/github-token/verify", s.operator(s.compatGitHubVerify))
	m.HandleFunc("GET /api/ui/config/provider/{providerName}", s.operator(s.metadataConfigGet))
	m.HandleFunc("PUT /api/ui/config/provider/{providerName}", s.operator(s.compatProviderPut))
	m.HandleFunc("GET /api/ui/settings/webhook", s.operator(s.compatWebhookGet))
	m.HandleFunc("PUT /api/ui/settings/webhook", s.operator(s.compatWebhookPut))
	s.registerCompatProxy(m)
}
func compatDecode(r *http.Request, v any) error {
	decoder := json.NewDecoder(io.LimitReader(r.Body, (64<<10)+1))
	decoder.UseNumber()
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(v); err != nil {
		return errors.New("invalid request body")
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return errors.New("expected one JSON value")
	}
	return nil
}
func (s *Server) compatGitHubGet(w http.ResponseWriter, r *http.Request) {
	token := ""
	if s.setting(r.Context(), "github_token", "") != "" {
		token = compatSecretMask
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, 200, map[string]any{"token": token, "configured": token != ""})
}

var compatGitHubTokenPattern = regexp.MustCompile(`^[A-Za-z0-9_]{10,1000}$`)

func (s *Server) compatGitHubSave(w http.ResponseWriter, r *http.Request) {
	var payload struct {
		Token *string `json:"token"`
	}
	if compatDecode(r, &payload) != nil || payload.Token == nil {
		httpError(w, 400, "A token string is required; an empty string clears it")
		return
	}
	token := *payload.Token
	if token == compatSecretMask {
		if s.setting(r.Context(), "github_token", "") == "" {
			httpError(w, 400, "No saved GitHub token exists to preserve")
			return
		}
		writeJSON(w, 200, map[string]string{"message": "保存成功"})
		return
	}
	if token != "" && !compatGitHubTokenPattern.MatchString(token) {
		httpError(w, 422, "Invalid GitHub token format")
		return
	}
	if err := s.setSettingWithHistory(r.Context(), "github_token", token, "github-token settings"); err != nil {
		httpError(w, 500, "GitHub token could not be saved")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, 200, map[string]string{"message": "保存成功"})
}
func (s *Server) compatGitHubVerify(w http.ResponseWriter, r *http.Request) {
	var payload struct {
		Token string `json:"token"`
	}
	if compatDecode(r, &payload) != nil {
		httpError(w, 400, "A token string is required")
		return
	}
	token := payload.Token
	if token == compatSecretMask {
		token = s.setting(r.Context(), "github_token", "")
	}
	if !compatGitHubTokenPattern.MatchString(token) {
		httpError(w, 400, "A valid-format GitHub token is required")
		return
	}
	result, status, err := compatVerifyGitHub(r.Context(), token, nil)
	if err != nil {
		httpError(w, status, err.Error())
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, 200, result)
}

// Fixed official endpoints only. The client parameter exists for fixture tests;
// production never accepts a request-controlled URL, transport, or redirect.
func compatVerifyGitHub(ctx context.Context, token string, client *http.Client) (map[string]any, int, error) {
	if !compatGitHubTokenPattern.MatchString(token) {
		return nil, 400, errors.New("Invalid GitHub token format")
	}
	if client == nil {
		transport := http.DefaultTransport.(*http.Transport).Clone()
		transport.Proxy = nil
		defer transport.CloseIdleConnections()
		client = &http.Client{Transport: transport}
	}
	copied := *client
	copied.Timeout = 10 * time.Second
	copied.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	fetch := func(endpoint string, out any) (int, error) {
		req, err := http.NewRequestWithContext(ctx, "GET", "https://api.github.com"+endpoint, nil)
		if err != nil {
			return 502, errors.New("GitHub verification request failed")
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Accept", "application/vnd.github+json")
		req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
		req.Header.Set("User-Agent", "AniDan-token-verification")
		res, err := copied.Do(req)
		if err != nil {
			return 502, errors.New("GitHub verification request failed")
		}
		defer res.Body.Close()
		if res.StatusCode == 401 {
			return 400, errors.New("GitHub rejected the token")
		}
		if res.StatusCode != 200 {
			return 502, errors.New("GitHub verification is unavailable or rate limited")
		}
		body, err := io.ReadAll(io.LimitReader(res.Body, (1<<20)+1))
		if err != nil || len(body) > 1<<20 || json.Unmarshal(body, out) != nil {
			return 502, errors.New("Invalid GitHub verification response")
		}
		return 200, nil
	}
	var user struct {
		Login string `json:"login"`
	}
	if status, err := fetch("/user", &user); err != nil {
		return nil, status, err
	}
	if !regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]{0,38}$`).MatchString(user.Login) {
		return nil, 502, errors.New("Invalid GitHub account response")
	}
	var rates struct {
		Rate *struct {
			Limit     *int64 `json:"limit"`
			Remaining *int64 `json:"remaining"`
			Reset     *int64 `json:"reset"`
		} `json:"rate"`
	}
	if status, err := fetch("/rate_limit", &rates); err != nil {
		return nil, status, err
	}
	if rates.Rate == nil || rates.Rate.Limit == nil || rates.Rate.Remaining == nil || rates.Rate.Reset == nil || *rates.Rate.Limit < 0 || *rates.Rate.Remaining < 0 || *rates.Rate.Reset < 0 {
		return nil, 502, errors.New("Invalid GitHub rate-limit response")
	}
	return map[string]any{"valid": true, "username": user.Login, "rateLimit": map[string]any{"limit": *rates.Rate.Limit, "remaining": *rates.Rate.Remaining, "reset": *rates.Rate.Reset}}, 200, nil
}
func (s *Server) compatProviderPut(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("providerName")
	if !integration.KnownProvider(name) {
		httpError(w, 404, "Unknown metadata provider")
		return
	}
	var payload map[string]any
	if compatDecode(r, &payload) != nil || payload == nil {
		httpError(w, 422, "Expected a provider settings object")
		return
	}
	descriptor := integration.MetadataConfigs()[name]
	allowed := map[string]bool{}
	bools := map[string]bool{}
	for _, v := range metadataList(descriptor["config_keys"]) {
		allowed[str(v)] = true
	}
	for _, v := range metadataList(descriptor["bool_config_keys"]) {
		bools[str(v)] = true
	}
	for key, info := range metadataObject(descriptor["configurable_fields"]) {
		allowed[key] = true
		if str(metadataObject(info)["type"]) == "boolean" {
			bools[key] = true
		}
		if values := metadataList(info); len(values) > 1 && str(values[1]) == "boolean" {
			bools[key] = true
		}
	}
	for _, key := range []string{"useProxy", "logRawResponses", "isFailoverEnabled"} {
		allowed[key] = true
		bools[key] = true
	}
	if descriptor["has_force_aux_search_toggle"] == true {
		allowed["forceAuxSearchEnabled"] = true
		bools["forceAuxSearchEnabled"] = true
	}
	for key, value := range payload {
		if !allowed[key] {
			httpError(w, 422, "Unrecognized provider setting")
			return
		}
		if bools[key] {
			switch v := value.(type) {
			case bool:
			case string:
				if v != "true" && v != "false" {
					httpError(w, 422, "Provider boolean settings must be true or false")
					return
				}
			default:
				httpError(w, 422, "Provider boolean settings must be true or false")
				return
			}
		} else if _, ok := value.(string); !ok {
			httpError(w, 422, "Provider configuration values must be strings")
			return
		}
		if len(str(value)) > 8192 {
			httpError(w, 422, "Provider setting exceeds size limit")
			return
		}
	}
	body, _ := json.Marshal(payload)
	r.Body = io.NopCloser(bytes.NewReader(body))
	s.metadataConfigPut(w, r)
}

var compatWebhookDefaults = map[string]string{"webhookEnabled": "true", "webhookDelayedImportEnabled": "false", "webhookDelayedImportHours": "24", "webhookCustomDomain": "", "webhookFilterMode": "blacklist", "webhookFilterRegex": "", "webhookLogRawRequest": "false", "webhookFallbackEnabled": "false", "webhookEnableTmdbSeasonMapping": "false", "webhookDeleteSyncEnabled": "false"}
var compatWebhookBools = map[string]bool{"webhookEnabled": true, "webhookDelayedImportEnabled": true, "webhookLogRawRequest": true, "webhookFallbackEnabled": true, "webhookEnableTmdbSeasonMapping": true, "webhookDeleteSyncEnabled": true}

func (s *Server) compatWebhookGet(w http.ResponseWriter, r *http.Request) {
	result := map[string]any{}
	for key, def := range compatWebhookDefaults {
		value := s.setting(r.Context(), key, def)
		if compatWebhookBools[key] {
			result[key] = strings.EqualFold(value, "true")
		} else if key == "webhookDelayedImportHours" {
			hours, err := strconv.Atoi(value)
			if err != nil || hours < 0 || hours > 8760 {
				hours = 24
			}
			result[key] = hours
		} else {
			result[key] = value
		}
	}
	writeJSON(w, 200, result)
}
func (s *Server) compatWebhookPut(w http.ResponseWriter, r *http.Request) {
	var payload map[string]any
	if compatDecode(r, &payload) != nil || payload == nil {
		httpError(w, 422, "Expected complete webhook settings")
		return
	}
	for key := range payload {
		if _, ok := compatWebhookDefaults[key]; !ok {
			httpError(w, 422, "Unrecognized webhook setting")
			return
		}
	}
	updates := map[string]string{}
	for key := range compatWebhookDefaults {
		value, ok := payload[key]
		if !ok {
			if key == "webhookDeleteSyncEnabled" {
				value = false
			} else {
				httpError(w, 422, "All webhook settings fields are required")
				return
			}
		}
		if compatWebhookBools[key] {
			v, ok := value.(bool)
			if !ok {
				httpError(w, 422, "Webhook switches must be JSON booleans")
				return
			}
			updates[key] = strconv.FormatBool(v)
		} else if key == "webhookDelayedImportHours" {
			v, ok := value.(json.Number)
			if !ok {
				httpError(w, 422, "Webhook delay must be an integer")
				return
			}
			hours, err := v.Int64()
			if err != nil || hours < 0 || hours > 8760 {
				httpError(w, 422, "Webhook delay must be 0..8760 hours")
				return
			}
			updates[key] = strconv.FormatInt(hours, 10)
		} else {
			v, ok := value.(string)
			if !ok || len(v) > 4096 {
				httpError(w, 422, "Invalid webhook text setting")
				return
			}
			updates[key] = v
		}
	}
	if mode := updates["webhookFilterMode"]; mode != "blacklist" && mode != "whitelist" {
		httpError(w, 422, "Webhook filter mode must be blacklist or whitelist")
		return
	}
	if _, err := regexp.Compile(updates["webhookFilterRegex"]); err != nil {
		httpError(w, 422, "Invalid webhook filter regular expression")
		return
	}
	if raw := updates["webhookCustomDomain"]; raw != "" {
		u, err := url.Parse(raw)
		if err != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.Opaque != "" || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
			httpError(w, 422, "Webhook custom domain must be an HTTP(S) origin without credentials, path or query")
			return
		}
		updates["webhookCustomDomain"] = strings.TrimRight(u.String(), "/")
	}
	if err := s.setSettingsAtomic(r.Context(), updates); err != nil {
		httpError(w, 500, "Webhook settings could not be saved atomically")
		return
	}
	w.WriteHeader(204)
}

func compatMetadataSecret(descriptor map[string]any, key string) bool {
	if sensitiveDiagnosticKey(key) {
		return true
	}
	info := metadataObject(descriptor["configurable_fields"])[key]
	if str(metadataObject(info)["type"]) == "password" {
		return true
	}
	values := metadataList(info)
	return len(values) > 1 && str(values[1]) == "password"
}
