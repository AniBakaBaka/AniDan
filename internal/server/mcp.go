// SPDX-License-Identifier: AGPL-3.0-only
// Stateless JSON-response Streamable HTTP, MCP2025-03-26/06-18/11-25.
package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

func (s *Server) registerMCP(m *http.ServeMux) {
	m.HandleFunc("/api/mcp", s.mcpHTTP)
	m.HandleFunc("GET /api/control/docs-initializer.js", s.controlDocsJS)
}
func mcpVersion(v string) bool { return v == "2025-03-26" || v == "2025-06-18" || v == "2025-11-25" }
func (s *Server) mcpOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, e := url.Parse(origin)
	if e != nil || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || (u.Scheme != "http" && u.Scheme != "https") {
		return false
	}
	if s.Config.PublicURL != "" {
		expected, e := url.Parse(s.Config.PublicURL)
		return e == nil && u.Scheme == expected.Scheme && strings.EqualFold(u.Host, expected.Host)
	}
	host := r.Host
	if h, _, e := net.SplitHostPort(host); e == nil {
		host = h
	}
	ip := net.ParseIP(host)
	if host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return false
	}
	return strings.EqualFold(u.Host, r.Host)
}

type mcpRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

func mcpError(w http.ResponseWriter, status int, id json.RawMessage, code int, msg string) {
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	writeJSON(w, status, map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": code, "message": msg}})
}
func mcpReply(w http.ResponseWriter, id json.RawMessage, result any) {
	writeJSON(w, 200, map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
}
func (s *Server) mcpHTTP(w http.ResponseWriter, r *http.Request) {
	if !s.mcpOrigin(r) {
		httpError(w, 403, "Origin not allowed")
		return
	}
	key := r.Header.Get("X-API-KEY")
	if key == "" {
		key = r.URL.Query().Get("apikey")
	}
	check := r.Clone(r.Context())
	u := *r.URL
	check.URL = &u
	q := url.Values{}
	q.Set("api_key", key)
	check.URL.RawQuery = q.Encode()
	if e := s.requireControl(check); e != nil {
		httpError(w, 401, "MCP API key required")
		return
	}
	if v := r.Header.Get("MCP-Protocol-Version"); v != "" && !mcpVersion(v) {
		mcpError(w, 400, nil, -32600, "Unsupported MCP protocol version")
		return
	}
	if r.Method != "POST" {
		w.Header().Set("Allow", "POST")
		httpError(w, 405, "Stateless MCP uses POST; optional server SSE/session deletion not offered")
		return
	}
	content, _, e := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if e != nil || content != "application/json" {
		httpError(w, 415, "application/json required")
		return
	}
	accept := r.Header.Get("Accept")
	if !strings.Contains(accept, "application/json") || !strings.Contains(accept, "text/event-stream") {
		httpError(w, 406, "Accept must include application/json and text/event-stream")
		return
	}
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	d.UseNumber()
	var in mcpRequest
	if e = d.Decode(&in); e != nil {
		mcpError(w, 400, nil, -32700, "Invalid JSON-RPC JSON")
		return
	}
	var trailing any
	if e = d.Decode(&trailing); e != io.EOF {
		mcpError(w, 400, in.ID, -32600, "Expected one JSON-RPC message")
		return
	}
	if in.JSONRPC != "2.0" || in.Method == "" {
		mcpError(w, 400, in.ID, -32600, "Invalid JSON-RPC request")
		return
	}
	if len(in.ID) == 0 {
		if strings.HasPrefix(in.Method, "notifications/") {
			w.WriteHeader(202)
			return
		}
		mcpError(w, 400, nil, -32600, "Request ID required")
		return
	}
	var id any
	iddec := json.NewDecoder(bytes.NewReader(in.ID))
	iddec.UseNumber()
	if iddec.Decode(&id) != nil {
		mcpError(w, 400, nil, -32600, "Invalid request ID")
		return
	}
	switch id.(type) {
	case string, json.Number:
	default:
		mcpError(w, 400, nil, -32600, "Request ID must be string or number")
		return
	}
	switch in.Method {
	case "initialize":
		var params struct {
			Protocol string `json:"protocolVersion"`
		}
		if json.Unmarshal(in.Params, &params) != nil {
			mcpError(w, 200, in.ID, -32602, "Invalid initialize params")
			return
		}
		version := params.Protocol
		if !mcpVersion(version) {
			version = "2025-11-25"
		}
		mcpReply(w, in.ID, map[string]any{"protocolVersion": version, "capabilities": map[string]any{"tools": map[string]any{"listChanged": false}}, "serverInfo": map[string]any{"name": "AniDan", "version": Version}, "instructions": "Tools expose registered control routes; compatibility remains partial; see README.md for current scope. Long operations return task IDs. Stateless authenticated JSON transport."})
	case "ping":
		mcpReply(w, in.ID, map[string]any{})
	case "tools/list":
		var params map[string]any
		if len(in.Params) > 0 && json.Unmarshal(in.Params, &params) != nil {
			mcpError(w, 200, in.ID, -32602, "Invalid list params")
			return
		}
		if str(params["cursor"]) != "" {
			mcpError(w, 200, in.ID, -32602, "Invalid pagination cursor")
			return
		}
		ops, e := s.controlOperations()
		if e != nil {
			mcpError(w, 200, in.ID, -32603, "Tool catalog unavailable")
			return
		}
		tools := []map[string]any{}
		for _, op := range ops {
			if !s.controlRegistered(op.Method, op.Path) {
				continue
			}
			tools = append(tools, map[string]any{"name": op.Name, "description": op.Description, "inputSchema": op.Schema, "annotations": map[string]any{"readOnlyHint": op.Method == "GET", "destructiveHint": op.Method != "GET", "openWorldHint": true}})
		}
		mcpReply(w, in.ID, map[string]any{"tools": tools})
	case "tools/call":
		var params struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		}
		pd := json.NewDecoder(bytes.NewReader(in.Params))
		pd.UseNumber()
		if pd.Decode(&params) != nil || params.Name == "" {
			mcpError(w, 200, in.ID, -32602, "Invalid tool call")
			return
		}
		ops, e := s.controlOperations()
		if e != nil {
			mcpError(w, 200, in.ID, -32603, "Tool catalog unavailable")
			return
		}
		var selected *controlOperation
		for i := range ops {
			if ops[i].Name == params.Name {
				selected = &ops[i]
				break
			}
		}
		if selected == nil || !s.controlRegistered(selected.Method, selected.Path) {
			mcpError(w, 200, in.ID, -32602, "Unknown or unavailable tool")
			return
		}
		if params.Arguments == nil {
			params.Arguments = map[string]any{}
		}
		result, e := s.callControlTool(r, key, *selected, params.Arguments)
		if e != nil {
			mcpReply(w, in.ID, map[string]any{"content": []map[string]any{{"type": "text", "text": e.Error()}}, "isError": true})
			return
		}
		mcpReply(w, in.ID, result)
	default:
		mcpError(w, 200, in.ID, -32601, "Method not supported")
	}
}

type mcpCapture struct {
	header   http.Header
	status   int
	buf      bytes.Buffer
	overflow bool
}

func (w *mcpCapture) Header() http.Header { return w.header }
func (w *mcpCapture) WriteHeader(n int) {
	if w.status == 0 {
		w.status = n
	}
}
func (w *mcpCapture) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = 200
	}
	if w.buf.Len()+len(p) > 4<<20 {
		w.overflow = true
		return 0, errors.New("MCP response exceeds4MiB; use paginated HTTP API")
	}
	return w.buf.Write(p)
}
func (w *mcpCapture) Flush() {}
func (s *Server) callControlTool(parent *http.Request, key string, op controlOperation, args map[string]any) (any, error) {
	props, _ := op.Schema["properties"].(map[string]any)
	for k := range args {
		if _, ok := props[k]; !ok {
			return nil, fmt.Errorf("unknown argument %s", k)
		}
	}
	required, _ := op.Schema["required"].([]any)
	for _, raw := range required {
		if _, ok := args[str(raw)]; !ok {
			return nil, fmt.Errorf("missing argument %s", raw)
		}
	}
	path := op.Path
	query := url.Values{}
	body := map[string]any{}
	for k, v := range args {
		body[k] = v
	}
	for _, raw := range op.Params {
		param, _ := raw.(map[string]any)
		name := str(param["name"])
		v, present := args[name]
		if !present {
			continue
		}
		switch param["in"] {
		case "path":
			value := mcpScalar(v)
			if value == "" || value == "." || value == ".." || strings.ContainsAny(value, "/\\\x00?#") {
				return nil, errors.New("invalid path argument")
			}
			path = strings.ReplaceAll(path, "{"+name+"}", url.PathEscape(value))
			delete(body, name)
		case "query":
			if values, ok := v.([]any); ok {
				for _, item := range values {
					query.Add(name, mcpScalar(item))
				}
			} else {
				query.Set(name, mcpScalar(v))
			}
			delete(body, name)
		}
	}
	if controlParamRE.MatchString(path) {
		return nil, errors.New("missing path arguments")
	}
	var reader io.Reader
	if op.Body != nil {
		b, e := json.Marshal(body)
		if e != nil {
			return nil, e
		}
		reader = bytes.NewReader(b)
	} else if len(body) > 0 {
		return nil, errors.New("unexpected body arguments")
	}
	ctx, cancel := context.WithTimeout(parent.Context(), 60*time.Second)
	defer cancel()
	target := path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	req, e := http.NewRequestWithContext(ctx, op.Method, target, reader)
	if e != nil {
		return nil, e
	}
	req.Host = parent.Host
	req.RemoteAddr = parent.RemoteAddr
	req.Header.Set("X-API-KEY", key)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	capture := &mcpCapture{header: http.Header{}}
	s.mux.ServeHTTP(capture, req)
	if capture.overflow {
		return nil, errors.New("Tool output exceeds4MiB; use the HTTP endpoint with pagination")
	}
	if capture.status >= 400 {
		return nil, fmt.Errorf("Control API HTTP%d: %s", capture.status, capture.buf.String())
	}
	if strings.Contains(capture.header.Get("Content-Type"), "text/event-stream") {
		return nil, errors.New("Use HTTP directly for streaming output")
	}
	return map[string]any{"content": []map[string]any{{"type": "text", "text": capture.buf.String()}}, "isError": false}, nil
}
func mcpScalar(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case json.Number:
		return x.String()
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(x)
	case nil:
		return ""
	default:
		return ""
	}
}
