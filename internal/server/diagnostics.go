// SPDX-License-Identifier: AGPL-3.0-or-later
package server

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/jackc/pgx/v5"
)

func (s *Server) registerDiagnostics(m *http.ServeMux) {
	s.registerDiagnosticHealth(m)
	s.registerDiagnosticHistory(m)
	s.registerDiagnosticMetrics(m)
	s.registerDiagnosticRate(m)
	s.registerDiagnosticEnvironment(m)
	m.HandleFunc("GET /api/ui/version", s.versionInfo)
	m.HandleFunc("GET /api/ui/database-info", s.operator(s.databaseInfo))
	m.HandleFunc("GET /api/ui/system/stats", s.operator(s.runtimeStats))
	m.HandleFunc("GET /api/ui/external-logs", s.operator(s.externalLogs))
	m.HandleFunc("GET /api/ui/audit/logs", s.operator(s.auditLogs))
	m.HandleFunc("GET /api/ui/audit/session-stats", s.operator(s.auditSessionStats))
	m.HandleFunc("POST /api/ui/audit/clear", s.operator(s.auditClear))
	for _, prefix := range []string{"/api/ui", "/api/control"} {
		m.HandleFunc("GET "+prefix+"/logs", s.operator(s.recentLogs))
		m.HandleFunc("GET "+prefix+"/logs/files", s.operator(s.logFiles))
		m.HandleFunc("GET "+prefix+"/logs/files/{filename}", s.operator(s.logFile))
		m.HandleFunc("GET "+prefix+"/logs/stream", s.operator(s.logStream))
	}
}
func (s *Server) versionInfo(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]string{"version": Version, "docsUrl": "https://github.com/AniBakaBaka/AniDan"})
}
func (s *Server) databaseInfo(w http.ResponseWriter, r *http.Request) {
	stats := s.Store.DB.Stats()
	host, name := "", ""
	port := 0
	switch s.Store.Dialect {
	case "sqlite":
		name = filepath.Base(strings.SplitN(s.Config.DSN, "?", 2)[0])
	case "mysql":
		if cfg, err := mysql.ParseDSN(s.Config.DSN); err == nil {
			name = cfg.DBName
			host = cfg.Addr
		}
	case "postgres":
		if cfg, err := pgx.ParseConfig(s.Config.DSN); err == nil {
			name = cfg.Database
			host = cfg.Host
			port = int(cfg.Port)
		}
	}
	if h, p, err := net.SplitHostPort(host); err == nil {
		host = h
		port, _ = strconv.Atoi(p)
	}
	recycle := 0
	if s.Store.Dialect != "sqlite" {
		recycle = 300
	}
	backend := "none"
	configured := s.Config.Cache.Backend
	connected := false
	var cacheHealth any
	cacheObserved := false
	if s.Cache != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		_, cacheErr := s.Cache.Stats(ctx, "")
		cancel()
		health := s.Cache.Health()
		cacheHealth = health
		backend = health.Effective
		configured = health.Configured
		cacheObserved = cacheErr == nil && !health.Closed
		connected = cacheObserved && health.Effective == "redis"
	}
	writeJSON(w, 200, map[string]any{"dbType": s.Store.Dialect, "dbHost": host, "dbPort": port, "dbName": name, "dbPoolType": "database/sql", "dbPoolSize": stats.MaxOpenConnections, "dbActiveConnections": stats.InUse, "dbIdleConnections": stats.Idle, "dbOverflow": 0, "dbMaxOverflow": 0, "dbPoolRecycle": recycle, "cacheBackend": backend, "configuredCacheBackend": configured, "cacheHealth": cacheHealth, "cacheOperationObserved": cacheObserved, "redisUrl": nil, "redisConnected": connected, "waitCount": stats.WaitCount, "waitDurationMs": stats.WaitDuration.Milliseconds(), "schedulerErrors": s.Jobs.SchedulerErrors()})
}
func (s *Server) runtimeStats(w http.ResponseWriter, r *http.Request) {
	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)
	writeJSON(w, 200, map[string]any{"uptimeSeconds": int64(time.Since(s.start).Seconds()), "goVersion": runtime.Version(), "goroutines": runtime.NumGoroutine(), "heapBytes": mem.HeapAlloc, "heapObjects": mem.HeapObjects, "gcCycles": mem.NumGC, "jobs": s.Jobs.Stats(), "schedulerErrors": s.Jobs.SchedulerErrors()})
}
func queryInt(r *http.Request, key string, def, maximum int) (int, error) {
	v := r.URL.Query().Get(key)
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 || n > maximum {
		return 0, fmt.Errorf("%s must be 0..%d", key, maximum)
	}
	return n, nil
}
func (s *Server) externalLogs(w http.ResponseWriter, r *http.Request) {
	limit, err := queryInt(r, "limit", 100, 1000)
	if err != nil || limit == 0 {
		httpError(w, 400, "limit must be 1..1000")
		return
	}
	rows, err := s.Store.DB.QueryContext(r.Context(), s.Store.Rebind("SELECT id FROM external_api_logs ORDER BY access_time DESC, id DESC LIMIT ?"), limit)
	if err != nil {
		httpError(w, 500, err.Error())
		return
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			httpError(w, 500, err.Error())
			return
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		httpError(w, 500, err.Error())
		return
	}
	out := []map[string]any{}
	for _, id := range ids {
		row, e := s.Store.Get(r.Context(), "external_api_logs", id)
		if e != nil {
			httpError(w, 500, e.Error())
			return
		}
		out = append(out, camelRow(row))
	}
	writeJSON(w, 200, out)
}
func (s *Server) auditLogs(w http.ResponseWriter, r *http.Request) {
	limit, err := queryInt(r, "limit", 50, 200)
	if err != nil || limit == 0 {
		httpError(w, 400, "limit must be 1..200")
		return
	}
	raw := s.setting(r.Context(), "security_audit_log", "[]")
	if len(raw) > 4<<20 {
		httpError(w, 413, "Audit log exceeds safety bound")
		return
	}
	var entries []map[string]any
	if err = json.Unmarshal([]byte(raw), &entries); err != nil {
		httpError(w, 500, "Stored audit log is not valid JSON")
		return
	}
	filtered := []map[string]any{}
	kind := r.URL.Query().Get("event_type")
	for _, entry := range entries {
		if kind == "" || str(entry["eventType"]) == kind {
			filtered = append(filtered, entry)
		}
	}
	if len(filtered) > limit {
		filtered = filtered[len(filtered)-limit:]
	}
	writeJSON(w, 200, filtered)
}
func (s *Server) auditSessionStats(w http.ResponseWriter, r *http.Request) {
	var total, active int64
	err := s.Store.DB.QueryRowContext(r.Context(), "SELECT COUNT(*) FROM user_sessions").Scan(&total)
	if err != nil {
		httpError(w, 500, err.Error())
		return
	}
	err = s.Store.DB.QueryRowContext(r.Context(), s.Store.Rebind("SELECT COUNT(*) FROM user_sessions WHERE is_revoked = ?"), false).Scan(&active)
	if err != nil {
		httpError(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]int64{"totalSessions": total, "activeSessions": active})
}
func (s *Server) auditClear(w http.ResponseWriter, r *http.Request) {
	if err := s.setSetting(r.Context(), "security_audit_log", "[]"); err != nil {
		httpError(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]string{"message": "ok"})
}

var diagnosticLogName = regexp.MustCompile(`^[A-Za-z0-9_-]+\.log(?:\.[0-9]+)?$`)

const maxLogBytes int64 = 64 << 20
const maxLogLine = 1 << 20

type logPage struct {
	Lines   []string `json:"lines"`
	HasMore bool     `json:"hasMore"`
	Total   int      `json:"total"`
	end     int64
	stat    os.FileInfo
}

func (s *Server) readLogPage(ctx context.Context, name string, limit, offset int, keyword string) (logPage, error) {
	out := logPage{Lines: []string{}}
	if !diagnosticLogName.MatchString(name) || filepath.Base(name) != name {
		return out, errors.New("invalid log filename")
	}
	if limit < 1 || limit > 10000 || offset < 0 || offset > 10000 || len(keyword) > 256 {
		return out, errors.New("invalid log pagination/filter bounds")
	}
	f, err := openWithin(filepath.Join(s.DataDir, "logs"), name)
	if err != nil {
		return out, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return out, err
	}
	if !st.Mode().IsRegular() || st.Size() > maxLogBytes {
		return out, errors.New("log is not regular or exceeds 64 MiB; rotate it before viewing")
	}
	out.end = st.Size()
	out.stat = st
	scanner := bufio.NewScanner(io.LimitReader(f, st.Size()))
	scanner.Buffer(make([]byte, 64<<10), maxLogLine)
	ring := make([]string, limit+offset)
	keyword = strings.ToLower(keyword)
	for scanner.Scan() {
		if err = ctx.Err(); err != nil {
			return out, err
		}
		line := scanner.Text()
		if strings.TrimSpace(line) == "" || (keyword != "" && !strings.Contains(strings.ToLower(line), keyword)) {
			continue
		}
		ring[out.Total%len(ring)] = line
		out.Total++
	}
	if err = scanner.Err(); err != nil {
		return out, err
	}
	end := max(0, out.Total-offset)
	start := max(0, end-limit)
	out.HasMore = start > 0
	for i := start; i < end; i++ {
		out.Lines = append(out.Lines, ring[i%len(ring)])
	}
	return out, nil
}
func (s *Server) recentLogs(w http.ResponseWriter, r *http.Request) {
	page, err := s.readLogPage(r.Context(), "app.log", 200, 0, "")
	if err != nil {
		if os.IsNotExist(err) {
			httpError(w, 503, "Application file logging is not configured or no log file exists yet")
		} else {
			backupAPIError(w, err)
		}
		return
	}
	for i, j := 0, len(page.Lines)-1; i < j; i, j = i+1, j-1 {
		page.Lines[i], page.Lines[j] = page.Lines[j], page.Lines[i]
	}
	writeJSON(w, 200, page.Lines)
}
func (s *Server) logFiles(w http.ResponseWriter, r *http.Request) {
	root := filepath.Join(s.DataDir, "logs")
	entries, err := os.ReadDir(root)
	if os.IsNotExist(err) {
		writeJSON(w, 200, []any{})
		return
	}
	if err != nil {
		backupAPIError(w, err)
		return
	}
	out := []map[string]any{}
	for _, entry := range entries {
		if !diagnosticLogName.MatchString(entry.Name()) || !entry.Type().IsRegular() {
			continue
		}
		st, e := entry.Info()
		if e != nil {
			backupAPIError(w, e)
			return
		}
		out = append(out, map[string]any{"name": entry.Name(), "size": st.Size(), "modified": float64(st.ModTime().UnixNano()) / 1e9})
		if len(out) > 1000 {
			httpError(w, 413, "Too many log files")
			return
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i]["modified"].(float64) > out[j]["modified"].(float64) })
	writeJSON(w, 200, out)
}
func (s *Server) logFile(w http.ResponseWriter, r *http.Request) {
	limit, err := queryInt(r, "tail", 200, 1000)
	if err != nil {
		backupAPIError(w, err)
		return
	}
	offset, err := queryInt(r, "offset", 0, 10000)
	if err != nil {
		backupAPIError(w, err)
		return
	}
	page, err := s.readLogPage(r.Context(), r.PathValue("filename"), limit, offset, r.URL.Query().Get("keyword"))
	if err != nil {
		backupAPIError(w, err)
		return
	}
	writeJSON(w, 200, page)
}

var activeLogStreams atomic.Int32

func (s *Server) logStream(w http.ResponseWriter, r *http.Request) {
	if activeLogStreams.Add(1) > 32 {
		activeLogStreams.Add(-1)
		httpError(w, 503, "Log stream capacity reached")
		return
	}
	defer activeLogStreams.Add(-1)
	page, err := s.readLogPage(r.Context(), "app.log", 200, 0, "")
	if err != nil {
		backupAPIError(w, err)
		return
	}
	if _, ok := w.(http.Flusher); !ok {
		httpError(w, 500, "Streaming is unavailable")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache, no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	controller := http.NewResponseController(w)
	send := func(line string) error {
		_ = controller.SetWriteDeadline(time.Now().Add(10 * time.Second))
		if _, e := fmt.Fprintf(w, "data: %s\n\n", strings.ReplaceAll(strings.ReplaceAll(line, "\r", ""), "\n", "\ndata: ")); e != nil {
			return e
		}
		return controller.Flush()
	}
	for _, line := range page.Lines {
		if err = send(line); err != nil {
			return
		}
	}
	offset, last := page.end, page.stat
	pending := ""
	tick := time.NewTicker(time.Second)
	heartbeat := time.NewTicker(25 * time.Second)
	defer tick.Stop()
	defer heartbeat.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-s.ctx.Done():
			return
		case <-heartbeat.C:
			_ = controller.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if _, err = io.WriteString(w, ": heartbeat\n\n"); err != nil {
				return
			}
			if err = controller.Flush(); err != nil {
				return
			}
		case <-tick.C:
			f, e := openWithin(filepath.Join(s.DataDir, "logs"), "app.log")
			if e != nil {
				continue
			}
			st, e := f.Stat()
			if e != nil || !st.Mode().IsRegular() {
				f.Close()
				continue
			}
			if !os.SameFile(last, st) || st.Size() < offset {
				offset = 0
				pending = ""
			}
			last = st
			if st.Size() == offset {
				f.Close()
				continue
			}
			size := min(st.Size()-offset, int64(1<<20))
			data, e := io.ReadAll(io.NewSectionReader(f, offset, size))
			f.Close()
			if e != nil {
				return
			}
			offset += int64(len(data))
			pending += string(data)
			if len(pending) > maxLogLine {
				_ = send("Log line exceeded the 1 MiB stream safety limit")
				return
			}
			lines := strings.Split(pending, "\n")
			pending = lines[len(lines)-1]
			for _, line := range lines[:len(lines)-1] {
				if line != "" {
					if err = send(line); err != nil {
						return
					}
				}
			}
		}
	}
}

// recordAuditEvent persists only caller-supplied event descriptions. Never pass
// passwords, tokens, OTP codes, credential bodies or raw request headers here.
func (s *Server) recordAuditEvent(ctx context.Context, eventType, ipAddress, userAgent, detail string, success bool) error {
	trim := func(v string, n int) string {
		r := []rune(v)
		if len(r) > n {
			return string(r[:n])
		}
		return v
	}
	entry := map[string]any{"eventType": trim(eventType, 100), "ipAddress": trim(ipAddress, 200), "userAgent": trim(userAgent, 200), "detail": trim(detail, 500), "timestamp": s.now(), "success": success}
	for attempt := 0; attempt < 5; attempt++ {
		tx, err := s.Store.DB.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		query := "SELECT config_value FROM config WHERE config_key = ?"
		if s.Store.Dialect != "sqlite" {
			query += " FOR UPDATE"
		}
		var raw string
		err = tx.QueryRowContext(ctx, s.Store.Rebind(query), "security_audit_log").Scan(&raw)
		missing := errors.Is(err, sql.ErrNoRows)
		if err != nil && !missing {
			tx.Rollback()
			return err
		}
		if raw == "" {
			raw = "[]"
		}
		if len(raw) > 4<<20 {
			tx.Rollback()
			return errors.New("audit log exceeds safety bound")
		}
		var entries []map[string]any
		if err = json.Unmarshal([]byte(raw), &entries); err != nil {
			tx.Rollback()
			return err
		}
		entries = append(entries, entry)
		if len(entries) > 500 {
			entries = entries[len(entries)-500:]
		}
		encoded, err := json.Marshal(entries)
		if err != nil {
			tx.Rollback()
			return err
		}
		if missing {
			q := "INSERT INTO config (config_key, config_value, description) VALUES (?, ?, ?) ON CONFLICT (config_key) DO NOTHING"
			if s.Store.Dialect == "mysql" {
				q = "INSERT IGNORE INTO config (config_key, config_value, description) VALUES (?, ?, ?)"
			}
			var res sql.Result
			res, err = tx.ExecContext(ctx, s.Store.Rebind(q), "security_audit_log", string(encoded), "Security audit events")
			if err == nil {
				var n int64
				n, err = res.RowsAffected()
				if err == nil && n == 0 {
					tx.Rollback()
					continue
				}
			}
		} else {
			_, err = tx.ExecContext(ctx, s.Store.Rebind("UPDATE config SET config_value = ? WHERE config_key = ?"), string(encoded), "security_audit_log")
		}
		if err != nil {
			tx.Rollback()
			return err
		}
		return tx.Commit()
	}
	return errors.New("concurrent security audit insertion did not settle")
}
