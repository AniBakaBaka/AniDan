// SPDX-License-Identifier: AGPL-3.0-or-later
package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"
)

func (s *Server) registerDiagnosticEnvironment(m *http.ServeMux) {
	m.HandleFunc("GET /api/ui/diagnostics/environment", s.operator(s.environmentInfo))
	m.HandleFunc("GET /api/ui/diagnostics/log-analysis", s.operator(s.logAnalysis))
	m.HandleFunc("GET /api/ui/diagnostics/full", s.operator(s.fullDiagnostics))
}
func (s *Server) environmentSnapshot() map[string]any {
	writable := func(relative string) bool {
		root, err := os.OpenRoot(s.DataDir)
		if err != nil {
			return false
		}
		defer root.Close()
		name := filepath.Join(relative, ".diagnostic-"+randomID())
		f, err := root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			return false
		}
		closeErr := f.Close()
		removeErr := root.Remove(name)
		return closeErr == nil && removeErr == nil
	}
	data, _ := filepath.Abs(s.DataDir)
	_, dockerErr := os.Stat("/.dockerenv")
	backend := "none"
	var cacheHealth any
	if s.Cache != nil {
		h := s.Cache.Health()
		backend = h.Effective
		cacheHealth = h
	}
	return map[string]any{"appVersion": Version, "pythonVersion": "not used (Go runtime)", "goVersion": runtime.Version(), "platform": runtime.GOOS, "architecture": runtime.GOARCH, "osName": runtime.GOOS, "dbType": s.Store.Dialect, "cacheBackend": backend, "cacheHealth": cacheHealth, "configDir": data, "logsDir": filepath.Join(data, "logs"), "configDirWritable": writable("."), "logsDirWritable": writable("logs"), "timezone": s.Config.Timezone, "isDocker": dockerErr == nil, "uvloopEnabled": false}
}
func (s *Server) environmentInfo(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, s.environmentSnapshot())
}

type logRule struct {
	kind       string
	pattern    *regexp.Regexp
	suggestion string
}

var diagnosticLogRules = []logRule{
	{"proxy_error", regexp.MustCompile(`(?i)proxy.*(fail|error)|代理.*失败|CONNECT.*failed`), "Check proxy settings and availability"},
	{"timeout", regexp.MustCompile(`(?i)timeout|超时|timed?\s*out`), "Check remote latency and request deadlines"},
	{"source_error", regexp.MustCompile(`(?i)search.*fail|scraper.*error|搜索.*失败|源.*异常`), "Check provider availability and credentials"},
	{"db_error", regexp.MustCompile(`(?i)database.*(fail|error)|数据库.*错误|SQL.*(error|constraint)|deadlock`), "Inspect database connectivity, constraints and transaction boundaries"},
	{"ai_error", regexp.MustCompile(`(?i)AI.*fail|openai.*error|api.*key.*invalid|模型.*失败|quota.*(exceed|error)`), "Check configured model access and remote quota"},
	{"auth_error", regexp.MustCompile(`(?i)unauthorized|forbidden|认证.*失败|token.*expired|\b(401|403)\b`), "Check expired or revoked credentials"},
	{"disk_error", regexp.MustCompile(`(?i)disk.*full|磁盘.*满|No space|ENOSPC`), "Check free disk space before removing data"},
	{"memory_error", regexp.MustCompile(`(?i)MemoryError|内存.*不足|out of memory|\bOOM\b`), "Reduce concurrency or inspect memory pressure"},
}

func (s *Server) analyzeDiagnosticLogs(ctx context.Context, hours, lines int) ([]map[string]any, bool, error) {
	if hours < 1 || hours > 168 || lines < 1 || lines > 10000 {
		return nil, false, errors.New("hours must be 1..168 and max_lines 1..10000")
	}
	entries, err := os.ReadDir(filepath.Join(s.DataDir, "logs"))
	if err != nil {
		return nil, false, err
	}
	groups := map[string]map[string]any{}
	cutoff := time.Now().Add(-time.Duration(hours) * time.Hour)
	used, files := 0, 0
	truncated := false
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".log") || !diagnosticLogName.MatchString(entry.Name()) || !entry.Type().IsRegular() {
			continue
		}
		st, err := entry.Info()
		if err != nil {
			return nil, false, err
		}
		if st.ModTime().Before(cutoff) {
			continue
		}
		files++
		if files > 20 {
			truncated = true
			break
		}
		page, err := s.readLogPage(ctx, entry.Name(), lines, 0, "")
		if err != nil {
			return nil, false, err
		}
		for _, line := range page.Lines {
			used += len(line)
			if used > 16<<20 {
				truncated = true
				break
			}
			timestamp := ""
			var object struct {
				Time string `json:"time"`
			}
			if strings.HasPrefix(line, "{") {
				if json.Unmarshal([]byte(line), &object) == nil {
					timestamp = object.Time
				}
			}
			if timestamp != "" {
				if t, e := time.Parse(time.RFC3339Nano, timestamp); e == nil && t.Before(cutoff) {
					continue
				}
			}
			for _, rule := range diagnosticLogRules {
				if !rule.pattern.MatchString(line) {
					continue
				}
				item := groups[rule.kind]
				if item == nil {
					item = map[string]any{"errorType": rule.kind, "count": 0, "suggestion": rule.suggestion}
					groups[rule.kind] = item
				}
				item["count"] = item["count"].(int) + 1
				runes := []rune(line)
				if len(runes) > 200 {
					runes = runes[:200]
				}
				item["latestMessage"] = string(runes)
				item["latestTime"] = timestamp
				item["patternMatchOnly"] = true
				break
			}
		}
		if truncated {
			break
		}
	}
	out := []map[string]any{}
	for _, value := range groups {
		out = append(out, value)
	}
	sort.Slice(out, func(i, j int) bool { return out[i]["count"].(int) > out[j]["count"].(int) })
	return out, truncated, nil
}
func (s *Server) logAnalysis(w http.ResponseWriter, r *http.Request) {
	hours, err := queryInt(r, "hours", 24, 168)
	if err != nil {
		httpError(w, 400, err.Error())
		return
	}
	lines, err := queryInt(r, "max_lines", 5000, 10000)
	if err != nil {
		httpError(w, 400, err.Error())
		return
	}
	out, truncated, err := s.analyzeDiagnosticLogs(r.Context(), hours, lines)
	if err != nil {
		httpError(w, 503, err.Error())
		return
	}
	if truncated {
		w.Header().Set("X-Diagnostic-Truncated", "true")
	}
	writeJSON(w, 200, out)
}
func (s *Server) fullDiagnostics(w http.ResponseWriter, r *http.Request) {
	env := s.environmentSnapshot()
	logs, truncated, err := s.analyzeDiagnosticLogs(r.Context(), 24, 5000)
	checks := []map[string]any{}
	for _, item := range []struct{ name, key, path string }{{"config_dir", "configDirWritable", "configDir"}, {"logs_dir", "logsDirWritable", "logsDir"}} {
		status := "warning"
		if env[item.key] == true {
			status = "ok"
		}
		checks = append(checks, map[string]any{"name": item.name, "label": item.name, "status": status, "detail": env[item.path]})
	}
	checks = append(checks, map[string]any{"name": "go_runtime", "label": "Go runtime", "status": "ok", "detail": runtime.Version()})
	if err != nil {
		logs = []map[string]any{}
		checks = append(checks, map[string]any{"name": "log_analysis", "label": "Log analysis", "status": "unavailable", "detail": err.Error()})
	}
	writeJSON(w, 200, map[string]any{"environment": env, "logDiagnostics": logs, "checks": checks, "logAnalysisAvailable": err == nil, "logAnalysisTruncated": truncated})
}
