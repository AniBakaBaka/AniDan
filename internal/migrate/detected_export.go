// SPDX-License-Identifier: AGPL-3.0-only
package migrate

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/AniBakaBaka/AniDan/internal/store"
	"github.com/go-sql-driver/mysql"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"golang.org/x/net/idna"
)

var errDetectedInnoDB = errors.New("detected MySQL source requires every visible base table to use InnoDB for a consistent snapshot")
var errDetectedNativeSource = errors.New("detected source contains AniDan migration or restore markers and is not an original installation")

// DetectedSourceConfig contains private, explicitly parsed original connection
// settings. Callers must distinguish an explicitly empty password from a missing
// password field before constructing this value. It accepts neither DSNs nor
// connection options. No field belongs in receipts, logs, or target config.
type DetectedSourceConfig struct {
	Driver   string `json:"-"`
	Host     string `json:"-"`
	Port     int    `json:"-"`
	User     string `json:"-"`
	Password string `json:"-"`
	Database string `json:"-"`
	Schema   string `json:"-"`
}

func (DetectedSourceConfig) String() string   { return "[private detected source configuration]" }
func (DetectedSourceConfig) GoString() string { return "[private detected source configuration]" }

// DetectedExportOptions must come from trusted local operator limits, never a
// source database, uploaded receipt, or request-controlled connection string.
type DetectedExportOptions struct {
	MaxBytes    int64
	MaxRowBytes int64
	Timeout     time.Duration
}

// DetectedSourceProvenance identifies the configured endpoint and database/schema
// actually selected by the snapshot transaction. It proves neither a physical
// server UUID nor that this account can see every hidden database object.
// CapturedAt is the local UTC capture start, not a distributed DB/files snapshot.
type DetectedSourceProvenance struct {
	Driver         string    `json:"driver"`
	Database       string    `json:"database"`
	Schema         string    `json:"schema"`
	EndpointSHA256 string    `json:"endpointSHA256"`
	CapturedAt     time.Time `json:"capturedAt"`
}

// ExportDetectedSource captures the original MySQL/PostgreSQL database exactly
// once into an exclusive, private source.json in a server-owned directory. The
// caller retains that immutable capture for inspect/prepare; this function must
// not be silently called again to replace it. It never initializes a database,
// acquires advisory locks, or runs DDL or data writes. Use a dedicated read-only
// account and quiesce source applications/files separately.
//
// The pinned original configuration has a single TCP endpoint, PostgreSQL public
// schema, and no custom TLS/DSN options. Unsupported options must be rejected by
// its parser. The explicit connectors never consult MySQL option files, pgpass,
// or PostgreSQL services. An ambient PGSERVICE causes a safe refusal.
// PostgreSQL tries TLS first (as the original asyncpg>=0.29 default does), using
// TLS 1.2+ and system-root/hostname validation; only an explicit no-TLS response
// permits plaintext. Invalid/self-signed certificates and handshake failures are
// refused, a deliberate stricter policy than asyncpg's unverified prefer mode.
func ExportDetectedSource(ctx context.Context, config DetectedSourceConfig, destination string, options DetectedExportOptions) (DetectedSourceProvenance, error) {
	var empty DetectedSourceProvenance
	host, err := canonicalDetectedHost(config.Host)
	if err != nil {
		return empty, err
	}
	config.Host = host
	if err := validateDetectedExport(config, destination, options); err != nil {
		return empty, err
	}
	ctx, cancel := context.WithTimeout(ctx, options.Timeout)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return empty, err
	}
	var db *sql.DB
	if config.Driver == "postgres" {
		cfg, err := detectedPostgresConfig(config, options.Timeout)
		if err != nil {
			return empty, err
		}
		db = sql.OpenDB(stdlib.GetConnector(*cfg))
	} else {
		cfg, err := detectedMySQLConfig(config, options.Timeout)
		if err != nil {
			return empty, err
		}
		connector, err := mysql.NewConnector(cfg)
		if err != nil {
			return empty, errors.New("cannot construct detected MySQL source connector")
		}
		db = sql.OpenDB(connector)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	s := &store.Store{DB: db, Dialect: config.Driver}
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true, Isolation: sql.LevelRepeatableRead})
	if err != nil {
		return empty, detectedExportError(ctx, "cannot start detected source read-only snapshot")
	}
	defer tx.Rollback()
	proof, err := verifyDetectedNamespace(ctx, s, tx, config)
	if err != nil {
		return empty, err
	}
	verifyStorage := func() error {
		if config.Driver == "mysql" {
			// VerifySchemaWith has opened each source table in this transaction,
			// holding MySQL metadata locks before storage engines are checked.
			if err := verifyDetectedInnoDB(ctx, tx); err != nil {
				return err
			}
		}
		return verifyDetectedOriginalMarkers(ctx, s, tx)
	}
	if err = exportSnapshotTransaction(ctx, s, tx, destination, options.MaxBytes, options.MaxRowBytes, proof.CapturedAt, verifyStorage); err != nil {
		if errors.Is(err, errDetectedInnoDB) {
			return empty, errDetectedInnoDB
		}
		if errors.Is(err, errDetectedNativeSource) {
			return empty, errDetectedNativeSource
		}
		// Database/scan/filesystem errors can contain credentials, paths, row
		// values, or names controlled by the source. Never expose or wrap them.
		return empty, detectedExportError(ctx, "detected source capture failed schema, data, byte-limit, or output validation")
	}
	return proof, nil
}

func validateDetectedExport(c DetectedSourceConfig, destination string, o DetectedExportOptions) error {
	if err := validateDetectedSourceConfig(c); err != nil {
		return err
	}
	if o.MaxBytes < 1 || o.MaxBytes > 1<<50 || o.MaxRowBytes < 1 || o.MaxRowBytes > 256<<20 || o.Timeout <= 0 || o.Timeout > 24*time.Hour {
		return errors.New("detected source requires positive bounded byte, row, and timeout limits")
	}
	if !filepath.IsAbs(destination) || filepath.Base(destination) != "source.json" {
		return errors.New("detected source output must be an absolute server-owned source.json path")
	}
	return nil
}

// Parsing and discovery use the same non-dialing endpoint checks as export.
// In particular, a misplaced credential-bearing DSN must never be projected as
// the public host before a later export validation rejects it.
func validateDetectedSourceConfig(c DetectedSourceConfig) error {
	if c.Driver != "mysql" && c.Driver != "postgres" {
		return errors.New("detected source requires mysql or postgres")
	}
	if c.Port < 1 || c.Port > 65535 {
		return errors.New("detected source requires one explicit TCP host and port")
	}
	if _, err := canonicalDetectedHost(c.Host); err != nil {
		return err
	}
	if c.User == "" || c.Database == "" || len(c.User) > 256 || len(c.Database) > 256 || len(c.Password) > 1<<20 {
		return errors.New("detected source requires bounded explicit user, password, and database settings")
	}
	for _, value := range []string{c.User, c.Password, c.Database} {
		if !utf8.ValidString(value) || strings.ContainsRune(value, 0) {
			return errors.New("detected source connection settings contain invalid text")
		}
	}
	if (c.Driver == "postgres" && c.Schema != "" && c.Schema != "public") || (c.Driver == "mysql" && c.Schema != "" && c.Schema != c.Database) {
		return errors.New("detected source namespace is unsupported by the pinned original")
	}
	return nil
}

// canonicalDetectedHost is the shared, non-dialing parser/connector boundary.
// DNS uses ASCII letters/digits/underscores with internal hyphens, 1..63-byte
// labels and at most 253 bytes excluding an optional final root dot. Underscores
// retain established Docker names. Only complete dotted IPv4 and bare IPv6 are
// accepted as addresses; scoped, bracketed and legacy numeric forms are refused.
//
// The original asyncpg delegates Unicode names to CPython's IDNA2003 codec.
// UTS46 (even transitional processing) is not an exact replacement. Accept only
// explicit ASCII A-labels, validated without remapping them; a raw Unicode name
// requires the original runtime's ASCII form, never a guessed Unicode mapping.
// https://github.com/python/cpython/blob/3.12/Lib/encodings/idna.py
// https://github.com/python/cpython/blob/3.12/Modules/socketmodule.c
func canonicalDetectedHost(host string) (string, error) {
	invalid := errors.New("detected source host must be one bounded ASCII DNS name, IPv4 address, or bare IPv6 address")
	if host == "" || len(host) > 254 || !utf8.ValidString(host) {
		return "", invalid
	}
	for i := 0; i < len(host); i++ {
		if host[i] >= utf8.RuneSelf {
			return "", errors.New("detected source Unicode host requires the original runtime's explicit ASCII IDNA hostname")
		}
	}
	if address, err := netip.ParseAddr(host); err == nil {
		if address.Zone() != "" {
			return "", invalid
		}
		return address.String(), nil
	}
	name := strings.TrimSuffix(strings.ToLower(host), ".")
	if name == "" || len(name) > 253 {
		return "", invalid
	}
	labels := strings.Split(name, ".")
	legacyNumeric := true
	for _, label := range labels {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return "", invalid
		}
		for i := 0; i < len(label); i++ {
			c := label[i]
			if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '_' || c == '-') {
				return "", invalid
			}
		}
		if strings.HasPrefix(label, "xn--") {
			decoded, err := idna.Lookup.ToUnicode(label)
			if err != nil {
				return "", invalid
			}
			encoded, err := idna.Lookup.ToASCII(decoded)
			if err != nil || encoded != label {
				return "", invalid
			}
		}
		// libc/Python may interpret shortened, octal or hexadecimal addresses
		// numerically while a Go resolver treats the same text as DNS. Refuse
		// that ambiguity instead of selecting a different original endpoint.
		digits := label
		hex := strings.HasPrefix(digits, "0x")
		if hex {
			digits = digits[2:]
		}
		if digits == "" {
			legacyNumeric = false
		}
		for i := 0; i < len(digits); i++ {
			c := digits[i]
			if !(c >= '0' && c <= '9' || hex && c >= 'a' && c <= 'f') {
				legacyNumeric = false
			}
		}
	}
	if legacyNumeric {
		return "", invalid
	}
	return strings.ToLower(host), nil
}

func detectedPostgresConfig(c DetectedSourceConfig, timeout time.Duration) (*pgx.ConnConfig, error) {
	host, err := canonicalDetectedHost(c.Host)
	if err != nil {
		return nil, err
	}
	c.Host = host
	roots, err := detectedSystemRoots()
	if err != nil {
		return nil, errors.New("cannot load system trust roots for detected PostgreSQL source")
	}
	// pgx requires ParseConfig to initialize private invariants. Parse only a
	// credential-free seed with all parsing-sensitive settings pinned. pgx's
	// service merge is unavoidable: servicefile=/dev/null makes PGSERVICE fail
	// closed, and passfile=/dev/null prevents fallback for an empty password.
	// Never mutate process environment (other connections may be opening).
	quote := func(value string) string {
		return "'" + strings.NewReplacer("\\", "\\\\", "'", "\\'").Replace(value) + "'"
	}
	seed := "host=127.0.0.1 port=5432 user=anidan_source_seed database=anidan_source_seed password='' sslmode=disable sslrootcert='' sslcert='' sslkey='' sslpassword='' sslsni=1 sslnegotiation=postgres connect_timeout=1 target_session_attrs=any servicefile=" + quote(os.DevNull) + " passfile=" + quote(os.DevNull)
	cfg, err := pgx.ParseConfig(seed)
	if err != nil {
		return nil, errors.New("detected PostgreSQL source configuration is unsupported; remove ambient PostgreSQL service selection")
	}
	cfg.Host, cfg.Port, cfg.User, cfg.Password, cfg.Database = c.Host, uint16(c.Port), c.User, c.Password, c.Database
	cfg.ConnectTimeout = timeout
	// DialFunc returns an already-negotiated connection. Leave pgx TLSConfig
	// and Fallbacks empty so it can neither negotiate twice nor retry plaintext
	// after a certificate/handshake failure.
	cfg.DialFunc = detectedPostgresDial(timeout, &tls.Config{MinVersion: tls.VersionTLS12, ServerName: c.Host, RootCAs: roots})
	cfg.LookupFunc = (&net.Resolver{}).LookupHost
	cfg.TLSConfig, cfg.Fallbacks, cfg.ValidateConnect, cfg.AfterConnect = nil, nil, nil, nil
	cfg.KerberosSrvName, cfg.KerberosSpn, cfg.SSLNegotiation = "", "", "postgres"
	cfg.RuntimeParams = map[string]string{"search_path": `"public"`, "client_encoding": "UTF8", "default_transaction_read_only": "on", "application_name": "anidan_source_capture"}
	return cfg, nil
}

func detectedSystemRoots() (*x509.CertPool, error) {
	if runtime.GOOS == "windows" || runtime.GOOS == "darwin" {
		return x509.SystemCertPool() // These platforms use the OS-native trust store.
	}
	// crypto/x509's Unix loader honors SSL_CERT_FILE/SSL_CERT_DIR. Read the
	// standard OS CA bundle locations directly, without changing global env or
	// loading caller-selected certificates. These are the Go platform defaults.
	for _, path := range []string{
		"/etc/ssl/certs/ca-certificates.crt",
		"/etc/pki/tls/certs/ca-bundle.crt",
		"/etc/ssl/ca-bundle.pem",
		"/etc/pki/tls/cacert.pem",
		"/etc/pki/ca-trust/extracted/pem/tls-ca-bundle.pem",
		"/etc/ssl/cert.pem",
		"/usr/local/share/certs/ca-root-nss.crt",
	} {
		file, err := os.Open(path)
		if err != nil {
			continue
		}
		data, err := io.ReadAll(io.LimitReader(file, (8<<20)+1))
		file.Close()
		if err != nil || len(data) > 8<<20 {
			return nil, errors.New("cannot load bounded system CA bundle")
		}
		roots := x509.NewCertPool()
		if roots.AppendCertsFromPEM(data) {
			return roots, nil
		}
	}
	return nil, errors.New("no supported system CA bundle is available")
}

// asyncpg's omitted-ssl default is documented at
// https://magicstack.github.io/asyncpg/current/api/index.html#asyncpg.connection.connect
// This narrower policy retains explicit no-TLS compatibility while requiring
// certificate verification whenever the configured endpoint offers TLS.
func detectedPostgresDial(timeout time.Duration, tlsConfig *tls.Config) func(context.Context, string, string) (net.Conn, error) {
	return func(ctx context.Context, network, address string) (conn net.Conn, err error) {
		if network != "tcp" {
			return nil, errors.New("detected PostgreSQL source requires TCP")
		}
		ctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		raw, err := (&net.Dialer{Timeout: timeout}).DialContext(ctx, network, address)
		if err != nil {
			return nil, err
		}
		stopped := make(chan struct{})
		stop := context.AfterFunc(ctx, func() {
			raw.Close()
			close(stopped)
		})
		defer func() {
			if !stop() {
				<-stopped
			}
			if ctx.Err() != nil {
				err = ctx.Err()
			}
			if err != nil {
				raw.Close()
				conn = nil
			}
		}()
		// PostgreSQL SSLRequest: length 8, request code 80877103. No startup
		// database, user, or password bytes are sent before TLS negotiation.
		request := []byte{0, 0, 0, 8, 4, 210, 22, 47}
		if n, e := raw.Write(request); e != nil || n != len(request) {
			if e == nil {
				e = io.ErrShortWrite
			}
			return nil, e
		}
		var response [1]byte
		if _, err = io.ReadFull(raw, response[:]); err != nil {
			return nil, err
		}
		switch response[0] {
		case 'N':
			return raw, nil
		case 'S':
			secure := tls.Client(raw, tlsConfig.Clone())
			if err = secure.HandshakeContext(ctx); err != nil {
				return nil, err
			}
			return secure, nil
		default:
			return nil, errors.New("detected PostgreSQL source returned invalid TLS negotiation")
		}
	}
}

func detectedMySQLConfig(c DetectedSourceConfig, timeout time.Duration) (*mysql.Config, error) {
	host, err := canonicalDetectedHost(c.Host)
	if err != nil {
		return nil, err
	}
	cfg := mysql.NewConfig()
	cfg.Net, cfg.Addr = "tcp", net.JoinHostPort(host, strconv.Itoa(c.Port))
	cfg.User, cfg.Passwd, cfg.DBName = c.User, c.Password, c.Database
	cfg.Timeout, cfg.ReadTimeout, cfg.WriteTimeout = timeout, timeout, timeout
	cfg.DialFunc = (&net.Dialer{Timeout: timeout}).DialContext
	cfg.ParseTime, cfg.Loc = true, time.UTC
	cfg.Collation = "utf8mb4_general_ci"
	cfg.Logger = log.New(io.Discard, "", 0)
	// NewConfig leaves file loading, cleartext/old password plugins, multiple
	// statements, DSN interpolation, and TLS/plaintext fallbacks disabled.
	return cfg, nil
}

func verifyDetectedNamespace(ctx context.Context, s *store.Store, tx *sql.Tx, config DetectedSourceConfig) (DetectedSourceProvenance, error) {
	proof := DetectedSourceProvenance{Driver: config.Driver, CapturedAt: time.Now().UTC().Truncate(time.Microsecond)}
	if config.Driver == "postgres" {
		var searchPath, isolation, readOnly string
		var exactNamespaces bool
		err := tx.QueryRowContext(ctx, `SELECT current_database(), current_schema(), current_setting('search_path'), current_setting('transaction_isolation'), current_setting('transaction_read_only'), current_schemas(true)=ARRAY['pg_catalog','public']::name[]`).Scan(&proof.Database, &proof.Schema, &searchPath, &isolation, &readOnly, &exactNamespaces)
		if err != nil {
			return DetectedSourceProvenance{}, detectedExportError(ctx, "cannot verify detected PostgreSQL source namespace")
		}
		if proof.Database != config.Database || proof.Schema != "public" || searchPath != `"public"` || isolation != "repeatable read" || readOnly != "on" || !exactNamespaces {
			return DetectedSourceProvenance{}, errors.New("detected PostgreSQL source database, schema, search path, or transaction does not match")
		}
	} else {
		if err := tx.QueryRowContext(ctx, "SELECT DATABASE()").Scan(&proof.Database); err != nil {
			return DetectedSourceProvenance{}, detectedExportError(ctx, "cannot verify detected MySQL source database")
		}
		if proof.Database != config.Database {
			return DetectedSourceProvenance{}, errors.New("detected MySQL source database does not match")
		}
		proof.Schema = proof.Database
	}
	parts := []string{"postgres", strings.ToLower(config.Host), strconv.Itoa(config.Port)}
	if config.Driver == "mysql" {
		parts = []string{"mysql", "tcp", net.JoinHostPort(strings.ToLower(config.Host), strconv.Itoa(config.Port))}
	}
	encoded, _ := json.Marshal(parts)
	digest := sha256.Sum256(encoded)
	proof.EndpointSHA256 = hex.EncodeToString(digest[:])
	return proof, nil
}

func verifyDetectedInnoDB(ctx context.Context, tx *sql.Tx) error {
	var incompatible int64
	err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM information_schema.tables WHERE table_schema=DATABASE() AND table_type='BASE TABLE' AND (engine IS NULL OR UPPER(engine)<>'INNODB')").Scan(&incompatible)
	if err != nil {
		return detectedExportError(ctx, "cannot verify detected MySQL source storage engines")
	}
	if incompatible != 0 {
		return errDetectedInnoDB
	}
	return nil
}

func verifyDetectedOriginalMarkers(ctx context.Context, s *store.Store, tx *sql.Tx) error {
	// These are the current native migration review and job recovery keys. Only
	// their presence is inspected, never their potentially large/private values.
	var marked bool
	err := tx.QueryRowContext(ctx, s.Rebind("SELECT EXISTS(SELECT 1 FROM "+s.Quote("config")+" WHERE "+s.Quote("config_key")+" IN (?,?))"), "anidan.migration.review_state", "anidan.restore.review_required").Scan(&marked)
	if err != nil {
		return detectedExportError(ctx, "cannot verify detected source origin markers")
	}
	if marked {
		return errDetectedNativeSource
	}
	owner, err := ReadRemoteOwnership(ctx, s.Dialect, tx)
	if err != nil {
		// Malformed/version-unknown ownership comments and lookup failures are
		// both refusals, but a failed lookup does not prove a native source.
		return detectedExportError(ctx, "cannot verify detected source ownership marker")
	}
	if owner != nil {
		return errDetectedNativeSource
	}
	return nil
}

func detectedExportError(ctx context.Context, message string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return errors.New(message)
}
