// SPDX-License-Identifier: AGPL-3.0-only
package server

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/AniBakaBaka/AniDan/internal/boundedcache"
	"github.com/AniBakaBaka/AniDan/internal/cachebackend"
	"github.com/AniBakaBaka/AniDan/internal/config"
	"github.com/AniBakaBaka/AniDan/internal/containerctl"
	"github.com/AniBakaBaka/AniDan/internal/danmaku"
	"github.com/AniBakaBaka/AniDan/internal/integration"
	"github.com/AniBakaBaka/AniDan/internal/job"
	"github.com/AniBakaBaka/AniDan/internal/notify"
	"github.com/AniBakaBaka/AniDan/internal/provider"
	"github.com/AniBakaBaka/AniDan/internal/ratelimit"
	"github.com/AniBakaBaka/AniDan/internal/store"
)

const Version = "0.1.0-stage30"

type Server struct {
	MigrationActivation         *MigrationActivation
	webMigrationOnce            sync.Once
	webMigration                *webMigrationRuntime
	webhookDone                 chan struct{}
	capacityDone                chan struct{}
	limiterDone                 chan struct{}
	localReviewOnce             sync.Once
	localReviewSecret           [32]byte
	localReviewKeyError         error
	notificationProgressStoreMu sync.Mutex
	notificationProgress        *notificationProgressRuntime
	predownload                 *predownloadRuntime
	episodeFetchOnce            sync.Once
	episodeFetch                *episodeFetchState
	predownloadBudgetMu         sync.Mutex
	notificationEvents          *notificationEventRuntime
	Container                   *containerctl.Controller
	Releases                    *containerctl.Releases
	Store                       *store.Store
	Providers                   *provider.Registry
	RequestLimits               *ratelimit.Limiter
	Metadata                    *integration.Client
	Cache                       *cachebackend.Service
	notificationRuntime         *notificationRuntime
	Notify                      *notify.Service
	Jobs                        *job.Manager
	DataDir                     string
	Config                      config.Config
	authOnce                    sync.Once
	auth                        *authState
	ctx                         context.Context
	cancel                      context.CancelFunc
	mux                         *http.ServeMux
	start                       time.Time
	playerMu                    sync.Mutex
	playerFetch                 map[int64]string
	importMu                    sync.Mutex
	providerConfigMu            sync.Mutex
	proxyRouting                atomic.Pointer[proxyRoutingState]
	proxyTrustFault             func(string) error // deterministic local fault fixture; never configured by API
	metadataConfigMu            sync.Mutex
	metadataDatasetMu           sync.Mutex
	metadataScheduleError       atomic.Value
	fileMu                      sync.RWMutex
	schedulerStarted            atomic.Bool
	fileFault                   atomic.Bool
	commentObjectSerializations atomic.Uint64
	backupMu                    sync.Mutex
	sourceActionsOnce           sync.Once
	sourceActions               *sourceActionState
	providerCacheOnce           sync.Once
	providerSearchCache         *boundedcache.Cache[[]provider.SearchResult]
	providerEpisodeCache        *boundedcache.Cache[[]provider.Episode]
	parsedCache                 *boundedcache.Cache[[]danmaku.Comment]
	outputCache                 *boundedcache.Cache[[]PlayerComment]
}

func New(cfg config.Config, s *store.Store) (_ *Server, resultErr error) {
	defer func() {
		uncertain := IsStartupCleanupUncertain(resultErr)
		driver := cfg.Driver
		if s != nil {
			driver = s.Dialect
		}
		resultErr = remoteStartupError(driver, resultErr)
		if uncertain && !IsStartupCleanupUncertain(resultErr) {
			resultErr = errors.Join(resultErr, errStartupCleanupUncertain)
		}
	}()
	if e := cfg.Validate(); e != nil {
		return nil, e
	}
	if s == nil {
		return nil, errors.New("store is required")
	}
	absolute, e := filepath.Abs(cfg.DataDir)
	if e != nil {
		return nil, e
	}
	cfg.DataDir = absolute
	if _, e = verifyOpenedMigrationTarget(context.Background(), cfg, s); e != nil {
		return nil, e
	}
	if e = s.SetTimezone(cfg.Timezone); e != nil {
		return nil, e
	}
	for _, p := range []string{cfg.DataDir, filepath.Join(cfg.DataDir, "danmaku"), filepath.Join(cfg.DataDir, "image")} {
		if e := os.MkdirAll(p, 0700); e != nil {
			return nil, e
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	srv := &Server{Store: s, Providers: provider.DefaultRegistry(), DataDir: cfg.DataDir, Config: cfg, ctx: ctx, cancel: cancel, start: time.Now(), playerFetch: map[int64]string{}}
	initialized := false
	defer func() {
		if !initialized {
			if cleanupErr := srv.Close(); cleanupErr != nil {
				resultErr = errors.Join(resultErr, errStartupCleanupUncertain, cleanupErr)
			}
		}
	}()
	if e := srv.initializeMigrationReview(ctx); e != nil {
		cancel()
		return nil, e
	}
	if e := srv.initContainer(); e != nil {
		cancel()
		return nil, e
	}
	if e := srv.recoverFileWrites(ctx); e != nil {
		cancel()
		return nil, e
	}
	if e := srv.initializeAuth(ctx); e != nil {
		cancel()
		return nil, e
	}
	jobs, e := job.NewWithTimezone(ctx, s, cfg.Workers, cfg.QueueSize, cfg.Timezone)
	if e != nil {
		cancel()
		return nil, e
	}
	srv.Jobs = jobs
	jobs.PauseQueue() // Do not run recovered jobs until every service and handler is initialized.
	if e = jobs.Register("fetch_comments", func(ctx context.Context, params json.RawMessage, p func(int, string)) (any, error) {
		var v struct {
			EpisodeID   int64 `json:"episodeId"`
			MissingOnly bool  `json:"missingOnly"`
			Playback    bool  `json:"playback"`
		}
		if e := unmarshalExactJSON(params, &v); e != nil {
			return nil, e
		}
		ep, e := s.Get(ctx, "episode", v.EpisodeID)
		if e != nil {
			return nil, e
		}
		if v.MissingOnly {
			result, err := srv.fetchEpisodeSharedMissing(ctx, ep, p)
			if wire, ok := result.(map[string]any); err == nil && v.Playback && ok && number(wire["count"]) > 0 && !boolean(wire["noPublication"]) {
				srv.queuePredownload(ep)
			}
			return result, err
		}
		return srv.fetchComments(ctx, ep, p)
	}); e != nil {
		jobs.Close()
		cancel()
		return nil, e
	}

	if e = jobs.Register("generic_import", srv.runImport); e != nil {
		jobs.Close()
		cancel()
		return nil, e
	}
	for i, name := range srv.Providers.Names() {
		existing, err := s.Get(ctx, "scrapers", name)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			jobs.Close()
			cancel()
			return nil, err
		}
		if existing == nil {
			if _, err = s.Insert(ctx, "scrapers", store.Row{"provider_name": name, "is_enabled": true, "display_order": i, "use_proxy": false}); err != nil {
				jobs.Close()
				cancel()
				return nil, err
			}
		}
	}
	if e = jobs.Register("player_search", srv.runPlayerSearch); e != nil {
		jobs.Close()
		cancel()
		return nil, e
	}
	if e = srv.loadProviderConfig(ctx); e != nil {
		jobs.Close()
		cancel()
		return nil, e
	}
	if e = srv.initRequestLimiter(); e != nil {
		jobs.Close()
		cancel()
		return nil, e
	}
	srv.parsedCache = boundedcache.New[[]danmaku.Comment](16<<20, 5*time.Minute)
	srv.outputCache = boundedcache.New[[]PlayerComment](16<<20, 5*time.Minute)
	srv.Providers.SetSearchObserver(srv.recordProviderSearch)
	srv.Cache, e = cachebackend.New(ctx, cfg.Cache.Options(cfg.Timezone), s)
	if e != nil {
		jobs.Close()
		cancel()
		return nil, e
	}
	if cfg.Cache.Backend == "redis" || cfg.Cache.Backend == "valkey" {
		for _, note := range cfg.Cache.Diagnostics() {
			slog.Warn("cache compatibility setting", "note", note)
		}
	} else if cfg.Cache.RedisURL != "" {
		slog.Warn("configured cache backend does not use Redis URL", "backend", cfg.Cache.Backend)
	}
	if health := srv.Cache.Health(); health.Degraded {
		slog.Warn("cache startup degraded", "configured", health.Configured, "effective", health.Effective, "reason", health.Reason)
	}
	srv.Metadata = integration.NewClient(srv.setting)
	srv.Metadata.Cache = srv.Cache
	if e = srv.loadMetadataResponseLogging(ctx); e != nil {
		jobs.Close()
		cancel()
		return nil, e
	}
	srv.Notify = notify.NewService(nil)
	if e = srv.initJobHandlers(); e != nil {
		jobs.Close()
		cancel()
		return nil, e
	}
	mux := http.NewServeMux()
	srv.mux = mux
	mux.HandleFunc("GET /health", srv.health)
	mux.HandleFunc("GET /healthz", srv.health)
	srv.registerAuth(mux)
	srv.registerLibrary(mux)
	srv.registerPlayer(mux)
	srv.registerOperations(mux)
	srv.registerCacheBackend(mux)
	srv.registerSearch(mux)
	srv.registerSources(mux)
	srv.registerSourceActions(mux)
	srv.registerScheduler(mux)
	srv.registerBackups(mux)
	srv.registerWebMigration(mux)
	srv.registerDiagnostics(mux)
	srv.registerMetadata(mux)
	if e = srv.initMetadataJobs(); e != nil {
		jobs.Close()
		cancel()
		return nil, e
	}
	srv.registerEpisodeGroups(mux)
	srv.registerRecognition(mux)
	srv.registerMedia(mux)
	srv.registerCompatMediaUI(mux)
	srv.registerDanmakuEdit(mux)
	srv.registerNotifications(mux)
	srv.registerSettings(mux)
	srv.registerCompatSettings(mux)
	srv.registerContainer(mux)
	srv.registerVersion(mux)
	srv.registerStoragePaths(mux)
	srv.registerCalendar(mux)
	srv.registerSubscriptions(mux)
	if e = srv.initSubscriptionJobs(); e != nil {
		jobs.Close()
		cancel()
		return nil, e
	}
	if e = srv.initCalendarJobs(); e != nil {
		jobs.Close()
		cancel()
		return nil, e
	}
	srv.registerImportControl(mux)
	if e = jobs.StartScheduler(cfg.Timezone); e != nil {
		jobs.Close()
		cancel()
		return nil, e
	}
	srv.schedulerStarted.Store(true)
	srv.registerSourceOffer(mux)
	srv.registerControlDocs(mux)
	srv.registerMCP(mux)
	mux.HandleFunc("/", srv.frontend)
	if err := srv.initPredownload(mux); err != nil {
		jobs.Close()
		return nil, err
	}
	if err := srv.installNotificationLifecycle(); err != nil {
		jobs.Close()
		return nil, err
	}
	jobs.ResumeQueue()
	initialized = true
	return srv, nil
}
func (s *Server) Close() error {
	s.closePredownload()
	s.closeNotificationProgress()
	s.closeNotificationEvents()
	s.cancel()
	shutdown, cancelShutdown := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelShutdown()
	sharedErr := s.episodeFetchRuntime().close(shutdown)
	if s.Providers != nil {
		sharedErr = errors.Join(sharedErr, s.Providers.CloseContext(shutdown))
	}
	if s.predownload != nil {
		<-s.predownload.done
	}
	s.closeNotifications()
	if s.notificationEvents != nil {
		<-s.notificationEvents.done
	}
	var e error
	if s.Jobs != nil {
		e = s.Jobs.Close()
	}
	if s.Notify != nil {
		s.Notify.Close()
	}
	if s.Metadata != nil {
		s.Metadata.Close()
	}
	if s.Cache != nil {
		e = errors.Join(e, s.Cache.Close())
	}
	if s.notificationProgress != nil {
		select {
		case <-s.notificationProgress.done:
		default:
			e = errors.Join(e, errStartupCleanupUncertain)
		}
	}
	for _, done := range []<-chan struct{}{s.webhookDone, s.capacityDone, s.limiterDone} {
		if done == nil {
			continue
		}
		select {
		case <-done:
			continue
		default:
		}
		select {
		case <-done:
		case <-shutdown.Done():
			e = errors.Join(e, errStartupCleanupUncertain)
		}
	}
	return errors.Join(e, sharedErr)
}

var errStartupCleanupUncertain = errors.New("application workers did not finish shutdown")

// IsStartupCleanupUncertain prevents a lifecycle controller from opening a
// replacement database while a failed initialization may still own workers.
func IsStartupCleanupUncertain(err error) bool {
	return errors.Is(err, errStartupCleanupUncertain)
}
func (s *Server) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.fileFault.Load() {
			httpError(w, 503, "Storage recovery is required. Resolve the I/O/database failure and restart AniDan.")
			return
		}
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "same-origin")
		w.Header().Set("X-Request-ID", randomID())
		r.Body = http.MaxBytesReader(w, r.Body, s.Config.MaxBodyBytes)
		defer func() {
			if x := recover(); x != nil {
				slog.Error("request panic", "path", redactPath(r.URL.Path), "panic", fmt.Sprint(x))
				httpError(w, 500, "Internal server error")
			}
		}()
		if err := s.guardLegacyUIIdentifiers(r); err != nil {
			httpError(w, http.StatusPreconditionRequired, err.Error())
			return
		}
		s.mux.ServeHTTP(w, r)
	})
}
func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if e := s.Store.DB.PingContext(ctx); e != nil {
		httpError(w, 503, "database unavailable")
		return
	}
	writeJSON(w, 200, map[string]any{"status": "ok", "name": "AniDan", "version": Version})
}
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if status != 204 {
		_ = json.NewEncoder(w).Encode(v)
	}
}
func readJSON(r *http.Request, v any) error {
	if format := r.Header.Get(exactIDsHeader); format != "" {
		if format != exactIDsVersion {
			return errors.New("unsupported exact identifier request format")
		}
		return decodeExactJSON(r.Body, v)
	}
	if isUIMutation(r) {
		return decodeLegacyUIJSON(r.Body, v)
	}
	d := json.NewDecoder(r.Body)
	d.UseNumber()
	if e := d.Decode(v); e != nil {
		return e
	}
	var extra any
	if e := d.Decode(&extra); e != io.EOF {
		return errors.New("request must contain one JSON value")
	}
	return nil
}
func httpError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{"detail": msg})
}
func idParam(r *http.Request, key string) (int64, error) {
	v, e := strconv.ParseInt(r.PathValue(key), 10, 64)
	if e != nil || v < 0 {
		return 0, fmt.Errorf("invalid %s", key)
	}
	return v, nil
}
func randomID() string {
	var b [16]byte
	if _, e := rand.Read(b[:]); e != nil {
		panic(e)
	}
	return hex.EncodeToString(b[:])
}
func redactPath(p string) string {
	if strings.HasPrefix(p, "/api/v1/") {
		parts := strings.SplitN(p, "/", 5)
		if len(parts) == 5 {
			return "/api/v1/[redacted]/" + parts[4]
		}
	}
	return p
}
func (s *Server) frontend(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" && r.Method != "HEAD" {
		httpError(w, 404, "Not found")
		return
	}
	p := r.URL.Path
	if s.notificationServeThumbnail(w, r) {
		return
	}
	if strings.HasPrefix(p, "/api/") {
		httpError(w, 404, "Endpoint is not implemented in this build; see README.md for current compatibility limits")
		return
	}
	root := s.Config.WebDir
	rel := strings.TrimPrefix(p, "/")
	if strings.HasPrefix(p, "/data/images/") {
		root = filepath.Join(s.DataDir, "image")
		rel = strings.TrimPrefix(p, "/data/images/")
	}
	if strings.HasPrefix(p, "/static/") {
		root = s.Config.StaticDir
		rel = strings.TrimPrefix(p, "/static/")
	}
	if strings.HasPrefix(p, "/dist/") {
		rel = strings.TrimPrefix(p, "/dist/")
	}
	if rel == "" {
		rel = "index.html"
	}
	f, e := openWithin(root, rel)
	if e != nil {
		if filepath.Ext(p) != "" || strings.HasPrefix(p, "/assets/") || strings.HasPrefix(p, "/data/") || strings.HasPrefix(p, "/static/") {
			http.NotFound(w, r)
			return
		}
		f, e = openWithin(s.Config.WebDir, "index.html")
		if e != nil {
			httpError(w, 503, "Frontend assets are not built; run npm ci && npm run build in web/")
			return
		}
	}
	defer f.Close()
	st, e := f.Stat()
	if e != nil || st.IsDir() {
		http.NotFound(w, r)
		return
	}
	if strings.HasSuffix(st.Name(), ".html") || strings.Contains(st.Name(), "sw.js") || strings.Contains(st.Name(), "registerSW") {
		w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
	}
	http.ServeContent(w, r, st.Name(), st.ModTime(), f)
}
func openWithin(root, relative string) (*os.File, error) {
	if filepath.IsAbs(relative) || !filepath.IsLocal(relative) {
		return nil, errors.New("path escapes allowed root")
	}
	d, e := os.OpenRoot(root)
	if e != nil {
		return nil, e
	}
	defer d.Close()
	before, e := d.Stat(relative)
	if e != nil {
		return nil, e
	}
	if !before.Mode().IsRegular() {
		return nil, errors.New("expected a regular file within the allowed root")
	}
	// The Unix flag also handles a regular-to-FIFO swap after Stat: opening a
	// pipe must not block before request cancellation can reach its reader.
	f, e := d.OpenFile(relative, regularReadFlags, 0)
	if e != nil {
		return nil, e
	}
	after, e := f.Stat()
	if e != nil || !after.Mode().IsRegular() {
		f.Close()
		if e != nil {
			return nil, e
		}
		return nil, errors.New("expected a regular file within the allowed root")
	}
	return f, nil
}
func str(v any) string {
	if v == nil {
		return ""
	}
	switch x := v.(type) {
	case string:
		return x
	case []byte:
		return string(x)
	case time.Time:
		return x.Format("2006-01-02T15:04:05")
	default:
		return fmt.Sprint(x)
	}
}
func number(v any) int64 {
	switch x := v.(type) {
	case int64:
		return x
	case int:
		return int64(x)
	case json.Number:
		n, _ := x.Int64()
		return n
	case float64:
		return int64(x)
	default:
		n, _ := strconv.ParseInt(str(x), 10, 64)
		return n
	}
}
func boolean(v any) bool {
	switch x := v.(type) {
	case bool:
		return x
	default:
		s := strings.ToLower(str(x))
		return s == "true" || s == "1"
	}
}
func (s *Server) now() string {
	loc, _ := time.LoadLocation(s.Config.Timezone)
	return time.Now().In(loc).Format("2006-01-02T15:04:05")
}
func (s *Server) setting(ctx context.Context, key, def string) string {
	r, e := s.Store.Get(ctx, "config", key)
	if e != nil || r == nil {
		return def
	}
	return str(r["config_value"])
}
func (s *Server) setSetting(ctx context.Context, key, value string) error {
	r, e := s.Store.Get(ctx, "config", key)
	if e == nil && r != nil {
		return s.Store.Update(ctx, "config", key, store.Row{"config_value": value})
	}
	_, e = s.Store.Insert(ctx, "config", store.Row{"config_key": key, "config_value": value})
	return e
}
