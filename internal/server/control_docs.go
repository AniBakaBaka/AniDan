// SPDX-License-Identifier: AGPL-3.0-only
package server

import (
	"embed"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
)

//go:embed control_contract.json
var controlContractFS embed.FS
var controlContractOnce sync.Once
var controlContract map[string]any
var controlContractErr error

func loadControlContract() (map[string]any, error) {
	controlContractOnce.Do(func() {
		b, e := controlContractFS.ReadFile("control_contract.json")
		if e != nil {
			controlContractErr = e
			return
		}
		controlContractErr = json.Unmarshal(b, &controlContract)
	})
	return controlContract, controlContractErr
}

var controlParamRE = regexp.MustCompile(`\{[^{}]+\}`)

func (s *Server) controlRegistered(method, path string) bool {
	sample := controlParamRE.ReplaceAllString(path, "1")
	req := &http.Request{Method: strings.ToUpper(method), URL: &url.URL{Path: sample}}
	_, pattern := s.mux.Handler(req)
	want := strings.ToUpper(method) + " " + controlParamRE.ReplaceAllString(path, "{}")
	return controlParamRE.ReplaceAllString(pattern, "{}") == want
}
func (s *Server) registerControlDocs(m *http.ServeMux) {
	m.HandleFunc("GET /api/control/openapi.json", s.controlOpenAPI)
	m.HandleFunc("GET /api/control/docs", s.controlDocs)
}
func (s *Server) controlOpenAPI(w http.ResponseWriter, r *http.Request) {
	doc, e := loadControlContract()
	if e != nil {
		httpError(w, 500, "Control schema unavailable")
		return
	}
	b, _ := json.Marshal(doc)
	var result map[string]any
	_ = json.Unmarshal(b, &result)
	annotateResponseLoggingSchema(result)
	paths, _ := result["paths"].(map[string]any)
	for path, raw := range paths {
		ops, _ := raw.(map[string]any)
		for method, item := range ops {
			op, ok := item.(map[string]any)
			if !ok {
				continue
			}
			op["description"] = nativeResponseLoggingDescription(path, method, str(op["description"]))
			op["x-anidan-route-registered"] = s.controlRegistered(method, path)
			op["x-implementation-status"] = "registration is not semantic parity; see README.md for scope and docs/性能对比总结.md for measured performance"
		}
	}
	result["info"] = map[string]any{"title": "AniDan 外部控制 API", "version": Version, "description": "固定上游58项控制接口契约；x-anidan-route-registered 标识当前注册情况，不代表已完成全部行为等价验证。鉴权使用 X-API-KEY 或 api_key。"}
	writeJSON(w, 200, openAPI30(result))
}
func (s *Server) controlDocs(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'self' 'unsafe-inline'; script-src 'self'")
	fmt.Fprint(w, `<!doctype html><html lang="zh-CN"><head><meta charset="utf-8"><title>AniDan API</title><link rel="stylesheet" href="/static/swagger-ui/swagger-ui.css"></head><body><p>当前为开发版本。接口注册不代表全部语义等价，参见源码中的兼容性清单。</p><div id="swagger-ui"></div><script src="/static/swagger-ui/swagger-ui-bundle.js"></script><script src="/api/control/docs-initializer.js"></script></body></html>`)
}
func (s *Server) controlDocsJS(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
	fmt.Fprint(w, `window.addEventListener('load',function(){SwaggerUIBundle({url:'/api/control/openapi.json',dom_id:'#swagger-ui',persistAuthorization:false});});`)
}

type controlOperation struct {
	Path, Method, Name, Description string
	Params                          []any
	Body                            map[string]any
	Schema                          map[string]any
}

func resolveControlSchema(v any, doc map[string]any, depth int) any {
	if depth > 30 {
		return map[string]any{"description": "recursive schema; backend validation required"}
	}
	switch x := v.(type) {
	case map[string]any:
		if ref, ok := x["$ref"].(string); ok && strings.HasPrefix(ref, "#/components/schemas/") {
			parts := strings.TrimPrefix(ref, "#/components/schemas/")
			components, _ := doc["components"].(map[string]any)
			schemas, _ := components["schemas"].(map[string]any)
			if target, ok := schemas[parts]; ok {
				return resolveControlSchema(target, doc, depth+1)
			}
		}
		out := map[string]any{}
		for k, y := range x {
			out[k] = resolveControlSchema(y, doc, depth+1)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, y := range x {
			out[i] = resolveControlSchema(y, doc, depth+1)
		}
		return out
	default:
		return v
	}
}
func (s *Server) controlOperations() ([]controlOperation, error) {
	doc, e := loadControlContract()
	if e != nil {
		return nil, e
	}
	paths, _ := doc["paths"].(map[string]any)
	out := []controlOperation{}
	for path, raw := range paths {
		ops, _ := raw.(map[string]any)
		for method, item := range ops {
			op, ok := item.(map[string]any)
			if !ok {
				continue
			}
			params, _ := op["parameters"].([]any)
			props := map[string]any{}
			required := []any{}
			seen := map[string]bool{}
			for _, raw := range params {
				p, _ := raw.(map[string]any)
				if p["in"] != "path" && p["in"] != "query" {
					continue
				}
				name := str(p["name"])
				props[name] = resolveControlSchema(p["schema"], doc, 0)
				if p["required"] == true && !seen[name] {
					required = append(required, name)
					seen[name] = true
				}
			}
			var body map[string]any
			if req, ok := op["requestBody"].(map[string]any); ok {
				content, _ := req["content"].(map[string]any)
				j, _ := content["application/json"].(map[string]any)
				body, _ = resolveControlSchema(j["schema"], doc, 0).(map[string]any)
				fields, _ := body["properties"].(map[string]any)
				for name, field := range fields {
					props[name] = field
				}
				reqs, _ := body["required"].([]any)
				for _, x := range reqs {
					name := str(x)
					if !seen[name] {
						required = append(required, name)
						seen[name] = true
					}
				}
			}
			schema := map[string]any{"type": "object", "properties": props, "additionalProperties": false}
			if len(required) > 0 {
				schema["required"] = required
			}
			name := str(op["operationId"])
			schema["title"] = name + "Arguments"
			description := str(op["summary"])
			if str(op["description"]) != "" {
				description += "\n" + str(op["description"])
			}
			description = nativeResponseLoggingDescription(path, method, description)
			annotateResponseLoggingSchema(schema)
			out = append(out, controlOperation{Path: path, Method: strings.ToUpper(method), Name: name, Description: description, Params: params, Body: body, Schema: schema})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Pydantic2 emits JSON-Schema null branches even when FastAPI is asked for
// OpenAPI3.0.3. Translate optional branches for older bundled Swagger readers.
func openAPI30(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := map[string]any{}
		for k, y := range x {
			out[k] = openAPI30(y)
		}
		if branches, ok := out["anyOf"].([]any); ok {
			nonnull := []any{}
			nullable := false
			for _, b := range branches {
				m, _ := b.(map[string]any)
				if m["type"] == "null" {
					nullable = true
				} else {
					nonnull = append(nonnull, b)
				}
			}
			if nullable {
				delete(out, "anyOf")
				out["nullable"] = true
				if len(nonnull) == 1 {
					for k, y := range nonnull[0].(map[string]any) {
						if _, exists := out[k]; !exists {
							out[k] = y
						}
					}
				} else if len(nonnull) > 1 {
					out["anyOf"] = nonnull
				}
			}
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, y := range x {
			out[i] = openAPI30(y)
		}
		return out
	default:
		return v
	}
}
