// SPDX-License-Identifier: AGPL-3.0-or-later
package integration

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"
)

//go:embed prompts.json
var promptJSON []byte

//go:embed ai_providers.json
var providerJSON []byte

func allPrompts() map[string]string {
	v := map[string]string{}
	_ = json.Unmarshal(promptJSON, &v)
	return v
}
func DefaultPrompts() map[string]string {
	v := allPrompts()
	delete(v, "aiSeasonMatchPrompt")
	return v
}
func AIProviders() []map[string]any { v := []map[string]any{}; _ = decode(providerJSON, &v); return v }
func aiProvider(id string) map[string]any {
	for _, p := range AIProviders() {
		if str(p["id"]) == id {
			return p
		}
	}
	return nil
}

type AIConfig struct {
	Provider  string `json:"provider"`
	APIKey    string `json:"apiKey"`
	BaseURL   string `json:"baseUrl"`
	Model     string `json:"model"`
	Thinking  bool   `json:"-"`
	Responses bool   `json:"-"`
}
type AIMetric struct {
	Method     string    `json:"method"`
	Success    bool      `json:"success"`
	DurationMS int64     `json:"durationMs"`
	TokensUsed int       `json:"tokensUsed"`
	Model      string    `json:"model"`
	Error      string    `json:"error"`
	CacheHit   bool      `json:"cacheHit"`
	Timestamp  time.Time `json:"timestamp"`
}
type aiCacheEntry struct {
	Content string
	Expires time.Time
}
type MatchDecision struct {
	Index      int    `json:"index"`
	Confidence int    `json:"confidence"`
	Reason     string `json:"reason"`
}

func (c *Client) AIConfig(ctx context.Context) AIConfig {
	return AIConfig{Provider: c.setting(ctx, "aiProvider", "deepseek"), APIKey: c.setting(ctx, "aiApiKey", ""), BaseURL: c.setting(ctx, "aiBaseUrl", ""), Model: c.setting(ctx, "aiModel", ""), Thinking: c.setting(ctx, "aiThinkingEnabled", "false") == "true", Responses: c.setting(ctx, "aiUseResponsesApi", "false") == "true"}
}
func aiConfigBase(cfg AIConfig) (string, error) {
	p := aiProvider(cfg.Provider)
	if p == nil {
		return "", &Error{"ai", 400, "unsupported provider"}
	}
	if cfg.APIKey == "" {
		return "", &Error{"ai", 412, "API key not configured"}
	}
	base := cfg.BaseURL
	if base == "" {
		base = str(p["defaultBaseUrl"])
	}
	if cfg.Provider == "gemini" && base == "" {
		base = "https://generativelanguage.googleapis.com/v1beta"
	}
	return strings.TrimRight(base, "/"), nil
}
func (c *Client) AIComplete(ctx context.Context, cfg AIConfig, system, user string, jsonMode bool, maxTokens int) (string, int, error) {
	base, e := aiConfigBase(cfg)
	if e != nil {
		return "", 0, e
	}
	if cfg.Model == "" {
		return "", 0, &Error{"ai", 412, "model not configured"}
	}
	if len(system)+len(user) > 1<<20 {
		return "", 0, &Error{"ai", 400, "prompt too large"}
	}
	var v any
	if cfg.Provider == "gemini" {
		model := strings.TrimPrefix(cfg.Model, "models/")
		if e := safeID(model); e != nil {
			return "", 0, e
		}
		gen := map[string]any{"temperature": 0}
		if maxTokens > 0 {
			gen["maxOutputTokens"] = maxTokens
		}
		if jsonMode {
			gen["responseMimeType"] = "application/json"
		}
		payload := map[string]any{"contents": []any{map[string]any{"role": "user", "parts": []any{map[string]any{"text": user}}}}, "generationConfig": gen}
		if system != "" {
			payload["systemInstruction"] = map[string]any{"parts": []any{map[string]any{"text": system}}}
		}
		v, e = c.json(ctx, "gemini", "POST", base+"/models/"+url.PathEscape(model)+":generateContent", nil, map[string]string{"x-goog-api-key": cfg.APIKey}, payload)
		if e != nil {
			return "", 0, e
		}
		var text strings.Builder
		for _, candidate := range array(object(v)["candidates"]) {
			for _, part := range array(nested(candidate, "content", "parts")) {
				text.WriteString(str(object(part)["text"]))
			}
			break
		}
		if text.Len() == 0 {
			return "", 0, &Error{"gemini", 502, "response contained no text"}
		}
		return text.String(), integer(nested(v, "usageMetadata", "totalTokenCount")), nil
	}
	headers := map[string]string{"Authorization": "Bearer " + cfg.APIKey}
	path := "/chat/completions"
	payload := map[string]any{"model": cfg.Model, "messages": []any{map[string]any{"role": "system", "content": system}, map[string]any{"role": "user", "content": user}}}
	if jsonMode {
		payload["response_format"] = map[string]any{"type": "json_object"}
	}
	if maxTokens > 0 {
		key := "max_completion_tokens"
		if cfg.Provider == "deepseek" || cfg.Provider == "siliconflow" {
			key = "max_tokens"
		}
		payload[key] = maxTokens
	}
	if cfg.Provider == "deepseek" {
		thinking := "disabled"
		if cfg.Thinking {
			thinking = "enabled"
		}
		payload["thinking"] = map[string]any{"type": thinking}
	}
	if cfg.Responses && cfg.Provider == "openai" {
		path = "/responses"
		payload = map[string]any{"model": cfg.Model, "instructions": system, "input": user, "store": false}
		if jsonMode {
			payload["text"] = map[string]any{"format": map[string]any{"type": "json_object"}}
		}
		if maxTokens > 0 {
			payload["max_output_tokens"] = maxTokens
		}
	}
	v, e = c.json(ctx, cfg.Provider, "POST", base+path, nil, headers, payload)
	if e != nil {
		return "", 0, e
	}
	content := ""
	if path == "/responses" {
		for _, o := range array(object(v)["output"]) {
			for _, p := range array(object(o)["content"]) {
				if str(object(p)["type"]) == "output_text" {
					content += str(object(p)["text"])
				}
			}
		}
	} else {
		choices := array(object(v)["choices"])
		if len(choices) > 0 {
			value := nested(choices[0], "message", "content")
			if s, ok := value.(string); ok {
				content = s
			} else {
				for _, p := range array(value) {
					content += str(object(p)["text"])
				}
			}
		}
	}
	if strings.TrimSpace(content) == "" {
		return "", 0, &Error{cfg.Provider, 502, "response contained no text"}
	}
	return content, integer(nested(v, "usage", "total_tokens")), nil
}
func (c *Client) recordMetric(m AIMetric) {
	c.mu.Lock()
	c.aiMetrics = append(c.aiMetrics, m)
	if len(c.aiMetrics) > 10000 {
		c.aiMetrics = append([]AIMetric(nil), c.aiMetrics[len(c.aiMetrics)-10000:]...)
	}
	c.mu.Unlock()
	if c.OnAIMetric != nil {
		c.OnAIMetric(m)
	}
}
func (c *Client) AICall(ctx context.Context, method, promptKey string, input any) (string, error) {
	if c.setting(ctx, "aiMatchEnabled", "false") != "true" {
		return "", &Error{"ai", 412, "AI matching is disabled"}
	}
	cfg := c.AIConfig(ctx)
	payload, e := json.Marshal(input)
	if e != nil {
		return "", e
	}
	prompt := c.setting(ctx, promptKey, allPrompts()[promptKey])
	if prompt == "" {
		return "", &Error{"ai", 400, "unknown prompt purpose"}
	}
	cfgBytes, _ := json.Marshal(cfg)
	hash := sha256.Sum256(append(append(cfgBytes, []byte(fmt.Sprintf("%s%s:%t:%t", method, prompt, cfg.Thinking, cfg.Responses))...), payload...))
	key := hex.EncodeToString(hash[:])
	enabled := c.setting(ctx, "aiCacheEnabled", "true") == "true"
	start := time.Now()
	metric := AIMetric{Method: method, Model: cfg.Model, Timestamp: start}
	if enabled {
		c.mu.Lock()
		entry, ok := c.aiCache[key]
		c.mu.Unlock()
		if ok && time.Now().Before(entry.Expires) {
			metric.Success = true
			metric.CacheHit = true
			c.recordMetric(metric)
			return entry.Content, nil
		}
	}
	timeout := integer(c.setting(ctx, "aiCallTimeout", "60"))
	if timeout < 1 {
		timeout = 60
	}
	if timeout > 300 {
		timeout = 300
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
	defer cancel()
	content, tokens, e := c.AIComplete(ctx, cfg, prompt, string(payload), true, 0)
	metric.DurationMS = time.Since(start).Milliseconds()
	metric.TokensUsed = tokens
	metric.Success = e == nil
	if e != nil {
		metric.Error = e.Error()
		c.recordMetric(metric)
		return "", e
	}
	content = strings.TrimSpace(content)
	if strings.HasPrefix(content, "```") {
		content = strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(content, "```json"), "```"), "```"))
	}
	var parsed any
	if decode([]byte(content), &parsed) != nil {
		metric.Success = false
		metric.Error = "AI response was not valid JSON"
		c.recordMetric(metric)
		return "", &Error{"ai", 502, metric.Error}
	}
	c.recordMetric(metric)
	if enabled {
		ttl := integer(c.setting(ctx, "aiCacheTtl", "3600"))
		if ttl < 1 {
			ttl = 3600
		}
		if ttl > 86400 {
			ttl = 86400
		}
		c.mu.Lock()
		if c.aiCache == nil {
			c.aiCache = map[string]aiCacheEntry{}
		}
		for k, v := range c.aiCache {
			if time.Now().After(v.Expires) {
				delete(c.aiCache, k)
			}
		}
		if len(c.aiCache) >= 1000 {
			var oldest string
			var t time.Time
			for k, v := range c.aiCache {
				if oldest == "" || v.Expires.Before(t) {
					oldest = k
					t = v.Expires
				}
			}
			delete(c.aiCache, oldest)
		}
		c.aiCache[key] = aiCacheEntry{content, time.Now().Add(time.Duration(ttl) * time.Second)}
		c.mu.Unlock()
	}
	return content, nil
}
func (c *Client) SelectMatch(ctx context.Context, query map[string]any, results []map[string]any) (*MatchDecision, error) {
	if len(results) == 0 {
		return nil, nil
	}
	v, e := c.AICall(ctx, "select_best_match", "aiPrompt", map[string]any{"query": query, "results": results})
	if e != nil {
		return nil, e
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal([]byte(v), &fields) != nil || fields["index"] == nil || fields["confidence"] == nil || fields["reason"] == nil {
		return nil, &Error{"ai", 502, "match decision omitted required fields"}
	}
	var decision MatchDecision
	if json.Unmarshal([]byte(v), &decision) != nil {
		return nil, &Error{"ai", 502, "invalid match decision"}
	}
	if decision.Index < 0 {
		return nil, nil
	}
	if decision.Index >= len(results) || decision.Confidence < 0 || decision.Confidence > 100 {
		return nil, &Error{"ai", 502, "match decision out of range"}
	}
	return &decision, nil
}
func (c *Client) AITransform(ctx context.Context, purpose string, input any) (map[string]any, error) {
	purposes := map[string]string{"recognition": "aiRecognitionPrompt", "alias_validation": "aiAliasValidationPrompt", "alias_expansion": "aiAliasExpansionPrompt", "name_conversion": "aiNameConversionPrompt", "season_mapping": "seasonMappingPrompt", "episode_group": "aiEpisodeGroupPrompt", "season_match": "aiSeasonMatchPrompt"}
	key, ok := purposes[purpose]
	if !ok {
		return nil, &Error{"ai", 400, "unknown transformation"}
	}
	v, e := c.AICall(ctx, purpose, key, input)
	if e != nil {
		return nil, e
	}
	var m map[string]any
	if decode([]byte(v), &m) != nil || m == nil {
		return nil, &Error{"ai", 502, "expected a JSON object"}
	}
	return m, nil
}
func (c *Client) GenerateRegex(ctx context.Context, description, existing, scene string) (string, error) {
	if strings.TrimSpace(description) == "" {
		return "", &Error{"ai", 400, "description is required"}
	}
	if c.setting(ctx, "aiMatchEnabled", "false") != "true" {
		return "", &Error{"ai", 412, "AI matching is disabled"}
	}
	system := "Generate a general regex for the requested filter. Return only the complete expression, no Markdown or explanations. Preserve all existing rules and add the requested behavior. The caller applies case-insensitive matching."
	if scene == "recognition_rules" {
		system = "Generate Misaka recognition DSL rules, not regex. Return rule text only, one per line. Preserve existing rules. Syntax: BLOCK:keyword; old title => new title; before <> after >> EP+1; title => {<search_season=2>}; title => {[title=name;season_offset=1>2]}. Use spaces around => <> >> &&."
	}
	input := fmt.Sprintf("Context: %s\nExisting rules:\n%s\nRequest:\n%s", scene, existing, description)
	start := time.Now()
	cfg := c.AIConfig(ctx)
	content, tokens, e := c.AIComplete(ctx, cfg, system, input, false, 4096)
	m := AIMetric{Method: "generate_regex", Success: e == nil, DurationMS: time.Since(start).Milliseconds(), TokensUsed: tokens, Model: cfg.Model, Timestamp: start}
	if e != nil {
		m.Error = e.Error()
	}
	c.recordMetric(m)
	if e != nil {
		return "", e
	}
	content = strings.TrimSpace(strings.Trim(content, "`"))
	content = strings.TrimSpace(strings.TrimPrefix(content, "regex"))
	if len(content) > 32768 {
		return "", &Error{"ai", 502, "generated rule too large"}
	}
	return content, nil
}
func (c *Client) ClearAICache() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := len(c.aiCache)
	c.aiCache = map[string]aiCacheEntry{}
	return n
}
func (c *Client) AICacheStats() map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()
	return map[string]any{"size": len(c.aiCache), "max_size": 1000}
}
func (c *Client) AIMetrics(hours int) []AIMetric {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := []AIMetric{}
	since := time.Now().Add(-time.Duration(hours) * time.Hour)
	for _, m := range c.aiMetrics {
		if !m.Timestamp.Before(since) {
			out = append(out, m)
		}
	}
	return out
}
func (c *Client) AIModels(ctx context.Context, provider string, refresh bool) (map[string]any, error) {
	cfg := c.AIConfig(ctx)
	cfg.Provider = provider
	if aiProvider(provider) == nil {
		return nil, &Error{"ai", 404, "unknown provider"}
	}
	result := map[string]any{"models": []any{}, "source": "hardcoded"}
	if !refresh {
		return result, nil
	}
	if cfg.APIKey == "" {
		result["error"] = "未配置API Key"
		return result, nil
	}
	base, e := aiConfigBase(cfg)
	if e != nil {
		return nil, e
	}
	headers := map[string]string{"Authorization": "Bearer " + cfg.APIKey}
	if provider == "gemini" {
		headers = map[string]string{"x-goog-api-key": cfg.APIKey}
	}
	v, e := c.json(ctx, provider, "GET", base+"/models", nil, headers, nil)
	if e != nil {
		return nil, e
	}
	rows := array(object(v)["data"])
	if provider == "gemini" {
		rows = array(object(v)["models"])
	}
	models := []map[string]any{}
	for _, x := range rows {
		o := object(x)
		id := first(o["id"], o["name"])
		if provider == "gemini" {
			id = strings.TrimPrefix(id, "models/")
		}
		if id != "" {
			models = append(models, map[string]any{"value": id, "label": first(o["displayName"], o["name"], id), "description": "官方模型", "isNew": true})
		}
	}
	sort.Slice(models, func(i, j int) bool { return str(models[i]["value"]) < str(models[j]["value"]) })
	return map[string]any{"models": models, "source": "merged", "dynamicCount": len(models), "newCount": len(models)}, nil
}
func (c *Client) AIBalance(ctx context.Context) map[string]any {
	cfg := c.AIConfig(ctx)
	result := map[string]any{"supported": cfg.Provider == "deepseek" || cfg.Provider == "siliconflow", "provider": cfg.Provider, "data": nil, "error": nil}
	if result["supported"] == false {
		result["error"] = cfg.Provider + " 不支持余额查询"
		return result
	}
	base, e := aiConfigBase(cfg)
	if e != nil {
		result["error"] = e.Error()
		return result
	}
	path := "/user/balance"
	if cfg.Provider == "siliconflow" {
		path = "/user/info"
	}
	v, e := c.json(ctx, cfg.Provider, "GET", base+path, nil, map[string]string{"Authorization": "Bearer " + cfg.APIKey}, nil)
	if e != nil {
		result["error"] = e.Error()
		return result
	}
	if cfg.Provider == "deepseek" {
		a := array(object(v)["balance_infos"])
		if object(v)["is_available"] != true || len(a) == 0 {
			result["error"] = "账户余额不可用"
			return result
		}
		result["data"] = a[0]
	} else {
		o := object(object(v)["data"])
		result["data"] = map[string]any{"currency": "CNY", "total_balance": first(o["totalBalance"], "0.00"), "granted_balance": first(o["balance"], "0.00"), "topped_up_balance": first(o["chargeBalance"], "0.00")}
	}
	return result
}
