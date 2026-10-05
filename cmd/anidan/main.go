// SPDX-License-Identifier: AGPL-3.0-only
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"github.com/AniBakaBaka/AniDan/internal/config"
	"github.com/AniBakaBaka/AniDan/internal/logrotate"
	"github.com/AniBakaBaka/AniDan/internal/migrate"
	"github.com/AniBakaBaka/AniDan/internal/server"
	"github.com/AniBakaBaka/AniDan/internal/store"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	_ "time/tzdata"
)

func main() {
	if e := run(); e != nil {
		slog.Error("AniDan stopped", "error", e)
		os.Exit(1)
	}
}
func run() error {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "version", "--version":
			fmt.Println("AniDan", server.Version)
			return nil
		case "migrate":
			return migration(os.Args[2:])
		case "export":
			return export(os.Args[2:])
		case "help", "--help", "-h":
			fmt.Println("AniDan: pure-Go danmaku server\nUsage: anidan -setup\n       anidan [-config config.json]\n       anidan migrate -snapshot backup.json.gz -target ./data-new -root /app/config=/old/config -source-quiesced\n       anidan migrate -snapshot backup.json.gz -target ./data-new -target-driver postgres -target-dsn-env TARGET_DSN -source-quiesced\n       anidan export -driver mysql|postgres -dsn-env VARIABLE -output backup.json\n       anidan version\nUse -setup for first-run installation, or set ANIDAN_ADMIN_PASSWORD for unattended initialization; see README.md.")
			return nil
		}
	}
	fs := flag.NewFlagSet("anidan", flag.ContinueOnError)
	path := fs.String("config", "", "JSON configuration path")
	setup := fs.Bool("setup", false, "serve first-run installation when no saved configuration exists")
	if e := fs.Parse(os.Args[1:]); e != nil {
		return e
	}
	configPath := *path
	configDir := os.Getenv("ANIDAN_CONFIG_DIR")
	if configDir == "" {
		configDir = os.Getenv("ANIDAN_DATA_DIR")
	}
	if configDir == "" {
		configDir = "data"
	}
	if configPath == "" {
		saved := filepath.Join(configDir, "anidan.json")
		if _, err := os.Stat(saved); err == nil {
			configPath = saved
		} else if !os.IsNotExist(err) {
			return err
		}
	}
	cfg, e := config.Load(configPath)
	if e != nil {
		return e
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if *setup && configPath == "" {
		cfg.DataDir = configDir
		if cfg.Driver == "sqlite" {
			cfg.DSN = filepath.Join(configDir, "anidan.db")
		}
		cfg, e = runSetup(ctx, cfg)
		if e != nil {
			return e
		}
	}
	activation, cfg, e := server.NewMigrationActivation(ctx, cfg)
	if e != nil {
		return e
	}
	if e = server.PreflightMigrationTarget(ctx, cfg); e != nil {
		return e
	}
	return runApplicationLifecycle(ctx, cfg, activation, openApplicationRuntime)
}

// A runtime owns all background services and its database, so another runtime
// may start only after Close positively confirms both have stopped.
type applicationRuntime interface {
	Handler() http.Handler
	NotifyStarted()
	Close() error
}
type applicationFactory func(context.Context, config.Config, *server.MigrationActivation) (applicationRuntime, error)
type databaseApplication struct {
	*server.Server
	db      *store.Store
	logfile *logrotate.Writer
}

func (a *databaseApplication) Close() error {
	if err := a.Server.Close(); err != nil {
		return err
	}
	err := a.db.Close()
	// The next runtime owns its own DataDir/logs. Switch the process fallback
	// only after all old services have stopped, before releasing their writer.
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, nil)))
	if a.logfile != nil {
		err = errors.Join(err, a.logfile.Close())
	}
	return err
}

func openApplicationRuntime(ctx context.Context, cfg config.Config, activation *server.MigrationActivation) (_ applicationRuntime, resultErr error) {
	if err := server.PreflightMigrationTarget(ctx, cfg); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(cfg.DataDir, "logs"), 0700); err != nil {
		fmt.Fprintf(os.Stderr, "AniDan: cannot create log directory; stderr logging continues: %v\n", err)
	}
	logfile, err := logrotate.Open(filepath.Join(cfg.DataDir, "logs"), logrotate.Options{
		MaxBytes: logrotate.DefaultMaxBytes,
		Backups:  logrotate.DefaultBackups,
		OnFailure: func() {
			fmt.Fprintln(os.Stderr, "AniDan: app.log file logging disabled after an I/O or safety error; stderr logging continues")
		},
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "AniDan: app.log file logging unavailable; stderr logging continues: %v\n", err)
	}
	initialized := false
	defer func() {
		if !initialized {
			slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, nil)))
			if logfile != nil {
				if closeErr := logfile.Close(); closeErr != nil {
					resultErr = &runtimeCloseUncertain{errors.Join(resultErr, closeErr)}
				}
			}
		}
	}()
	var logOutput io.Writer = os.Stderr
	if logfile != nil {
		logOutput = io.MultiWriter(os.Stderr, logfile)
	}
	slog.SetDefault(slog.New(slog.NewJSONHandler(logOutput, nil)))
	db, err := store.Open(ctx, cfg.Driver, cfg.DSN)
	if err != nil {
		switch strings.ToLower(cfg.Driver) {
		case "postgres", "postgresql", "pgx", "mysql":
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, errors.New("cannot open configured remote database")
		default:
			return nil, err
		}
	}
	if err = server.InitializeStore(ctx, cfg, db); err != nil {
		if closeErr := db.Close(); closeErr != nil {
			return nil, &runtimeCloseUncertain{errors.Join(err, closeErr)}
		}
		return nil, err
	}
	app, err := server.New(cfg, db)
	if err != nil {
		closeErr := db.Close()
		if server.IsStartupCleanupUncertain(err) || closeErr != nil {
			return nil, &runtimeCloseUncertain{errors.Join(err, closeErr)}
		}
		return nil, err
	}
	app.MigrationActivation = activation
	initialized = true
	return &databaseApplication{Server: app, db: db, logfile: logfile}, nil
}

type runtimeCloseUncertain struct{ error }

func runApplicationLifecycle(ctx context.Context, cfg config.Config, activation *server.MigrationActivation, open applicationFactory) error {
	listener, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return err
	}
	// In particular, keep a dynamically allocated :0 port on an in-process swap.
	address := listener.Addr().String()
	current, err := open(ctx, cfg, activation)
	if err != nil {
		listener.Close()
		return err
	}
	defer func() {
		if current != nil {
			_ = current.Close()
		}
	}()
	var srv *http.Server
	var stopped chan error
	var cancelRequests context.CancelFunc
	defer func() {
		if cancelRequests != nil {
			cancelRequests()
		}
	}()
	start := func() {
		requestContext, cancel := context.WithCancel(context.Background())
		cancelRequests = cancel
		srv = &http.Server{Addr: address, Handler: current.Handler(), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 1 << 20, BaseContext: func(net.Listener) context.Context { return requestContext }}
		// Shutdown closes listeners but otherwise leaves streaming request
		// contexts alive. Cancel this generation's requests, then wait for their
		// handlers to return before closing any application services or database.
		srv.RegisterOnShutdown(cancel)
		stopped = make(chan error, 1)
		serving, socket, result := srv, listener, stopped
		go func() { result <- serving.Serve(socket) }()
		current.NotifyStarted()
		slog.Info("AniDan listening", "address", address, "version", server.Version, "database", cfg.Driver)
	}
	start()
	for {
		select {
		case err = <-stopped:
			if errors.Is(err, http.ErrServerClosed) {
				return nil
			}
			return err
		case <-ctx.Done():
			shutdown, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			err = srv.Shutdown(shutdown)
			cancel()
			if err != nil {
				_ = srv.Close()
			}
			return err
		case request := <-activation.Requests():
			next, verifyErr := activation.Verify(ctx, request)
			if verifyErr != nil {
				activation.Complete(request, verifyErr)
				continue
			}
			shutdown, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			err = srv.Shutdown(shutdown)
			cancel()
			if err != nil {
				_ = srv.Close()
				return errors.New("activation stopped: HTTP requests did not drain safely")
			}
			old := current
			current = nil
			if err = old.Close(); err != nil {
				return errors.New("activation stopped: previous application did not close safely")
			}
			listener, err = net.Listen("tcp", address)
			if err != nil {
				return errors.New("activation stopped: listening address could not be reserved")
			}
			// This socket remains reserved during candidate preparation and rollback.
			// No candidate HTTP request can execute before the selector commits.
			next, err = activation.Verify(ctx, request)
			var candidate applicationRuntime
			if err == nil {
				candidate, err = open(ctx, next, activation)
			}
			if err == nil {
				committed, commitErr := activation.Commit(ctx, request)
				if commitErr == nil {
					current, cfg = candidate, next
					activation.Complete(request, nil)
					start()
					continue
				}
				err = commitErr
				closeErr := candidate.Close()
				if committed || closeErr != nil {
					listener.Close()
					return errors.New("activation stopped: selection or candidate shutdown is uncertain; restart to verify durable selection")
				}
			}
			var uncertain *runtimeCloseUncertain
			if errors.As(err, &uncertain) {
				listener.Close()
				return errors.New("activation stopped: failed candidate could not be closed safely")
			}
			// A known uncommitted failure with fully stopped services may reopen the
			// retained old root. Recheck its durable selector rather than guessing.
			previous, rollbackErr := activation.CurrentConfiguration(ctx)
			if rollbackErr == nil {
				current, rollbackErr = open(ctx, previous, activation)
			}
			if rollbackErr != nil {
				listener.Close()
				return errors.New("activation failed and the previous verified application could not reopen")
			}
			cfg = previous
			activation.Complete(request, err)
			start()
		}
	}
}

type rootsFlag []migrate.RootMapping

func (r *rootsFlag) String() string { return fmt.Sprint([]migrate.RootMapping(*r)) }
func (r *rootsFlag) Set(v string) error {
	parts := strings.SplitN(v, "=", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return errors.New("root must be legacy-prefix=read-only-directory")
	}
	*r = append(*r, migrate.RootMapping{From: parts[0], To: parts[1]})
	return nil
}
func migrationConfig(args []string) (migrate.Config, error) {
	fs := flag.NewFlagSet("migrate", flag.ContinueOnError)
	var c migrate.Config
	var roots rootsFlag
	var dsnEnv, targetDSNEnv string
	var legacyConfig string
	fs.StringVar(&c.Snapshot, "snapshot", "", "legacy v2 backup JSON/gzip")
	fs.StringVar(&legacyConfig, "legacy-config", "", "optional legacy config.yml translated securely into new target config.json")
	fs.StringVar(&c.SourceDriver, "source-driver", "", "live source driver mysql/postgres")
	fs.StringVar(&dsnEnv, "source-dsn-env", "", "environment variable holding live read-only source DSN")
	fs.StringVar(&c.TargetDir, "target", "data-migrated", "new destination directory (never old source)")
	fs.StringVar(&c.TargetDriver, "target-driver", "sqlite", "destination database: sqlite (default), postgres or mysql; remote namespace must be dedicated and empty")
	fs.StringVar(&targetDSNEnv, "target-dsn-env", "", "environment variable holding target DSN; never stored in generated configuration")
	fs.Var(&roots, "root", "legacy-prefix=read-only-root (repeatable)")
	fs.BoolVar(&c.DryRun, "dry-run", false, "verify without publishing destination")
	fs.BoolVar(&c.Resume, "resume", false, "resume matching staged migration")
	fs.BoolVar(&c.SourceQuiesced, "source-quiesced", false, "confirm legacy writers are stopped")
	fs.StringVar(&c.ExpectedSHA256, "sha256", "", "expected backup SHA256")
	fs.Int64Var(&c.MaxBytes, "max-bytes", 8<<30, "maximum decoded snapshot bytes; remote targets also bound retained encoded snapshot and canonical logical proof")
	fs.Int64Var(&c.MaxRowBytes, "max-row-bytes", 32<<20, "maximum one decoded row bytes")
	fs.IntVar(&c.MaxFiles, "max-files", 100000, "maximum referenced file manifest entries")
	if e := fs.Parse(args); e != nil {
		return c, e
	}
	if fs.NArg() != 0 {
		return c, errors.New("migration does not accept positional arguments")
	}
	c.TargetDriver = strings.ToLower(strings.TrimSpace(c.TargetDriver))
	switch c.TargetDriver {
	case "sqlite":
		if targetDSNEnv != "" {
			return c, errors.New("target-dsn-env requires a postgres or mysql target")
		}
	case "postgres", "mysql":
		if c.MaxRowBytes > 256<<20 {
			return c, errors.New("remote migration max-row-bytes must not exceed 256 MiB")
		}
		if !migrationEnvName(targetDSNEnv) {
			return c, errors.New("remote target requires a valid target-dsn-env variable name")
		}
		c.TargetDSN = os.Getenv(targetDSNEnv)
		if strings.TrimSpace(c.TargetDSN) == "" {
			return c, errors.New("target DSN environment variable is empty")
		}
	default:
		return c, errors.New("target-driver must be sqlite, postgres or mysql")
	}
	c.Roots = roots
	if dsnEnv != "" {
		c.SourceDSN = os.Getenv(dsnEnv)
		if c.SourceDSN == "" {
			return c, errors.New("source DSN environment variable is empty")
		}
	}
	if legacyConfig != "" || c.TargetDriver != "sqlite" {
		translated := config.Defaults()
		var e error
		if legacyConfig != "" {
			translated, e = config.TranslateLegacyForMigration(legacyConfig, c.TargetDir)
			if e != nil {
				return c, e
			}
		} else {
			translated.DataDir, e = filepath.Abs(c.TargetDir)
			if e != nil {
				return c, e
			}
			translated.Cache, e = translated.Cache.ForIsolatedTarget(translated.DataDir)
			if e != nil {
				return c, e
			}
		}
		if c.TargetDriver != "sqlite" {
			translated.Driver = c.TargetDriver
			translated.DSN = "" // Operator supplies ANIDAN_DSN at cutover.
			if c.MaxRowBytes > 0 {
				translated.MigrationReview.MaxRowBytes = c.MaxRowBytes
			}
			if c.MaxFiles > 0 {
				translated.MigrationReview.MaxFiles = c.MaxFiles
			}
			if c.MaxBytes > translated.MigrationReview.MaxDatabaseBytes {
				translated.MigrationReview.MaxDatabaseBytes = c.MaxBytes
			}
			for i, warning := range translated.Warnings {
				translated.Warnings[i] = strings.ReplaceAll(warning, "target SQLite", "target database")
			}
		}
		if e = translated.Validate(); e != nil {
			return c, e
		}
		for _, warning := range translated.Warnings {
			slog.Warn(warning)
		}
		c.RuntimeConfig, e = json.MarshalIndent(translated, "", "  ")
		if e != nil {
			return c, e
		}
	}
	return c, nil
}

// Restrict the option to a variable name so accidental inline credentials are
// rejected without quoting them in diagnostics or reading unrelated variables.
func migrationEnvName(name string) bool {
	if name == "" {
		return false
	}
	for i, c := range name {
		if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || c == '_' || (i > 0 && c >= '0' && c <= '9') {
			continue
		}
		return false
	}
	return true
}

func migration(args []string) error {
	c, e := migrationConfig(args)
	if e != nil {
		return e
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	receipt, e := migrate.Run(ctx, c)
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(receipt); err != nil {
		return err
	}
	return e
}
func export(args []string) error {
	fs := flag.NewFlagSet("export", flag.ContinueOnError)
	driver := fs.String("driver", "", "source database driver")
	env := fs.String("dsn-env", "", "environment variable holding readonly DSN")
	out := fs.String("output", "", "new snapshot path")
	if e := fs.Parse(args); e != nil {
		return e
	}
	dsn := os.Getenv(*env)
	if dsn == "" || *out == "" {
		return errors.New("dsn-env and output are required")
	}
	return migrate.Export(context.Background(), *driver, dsn, *out)
}
