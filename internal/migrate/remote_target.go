// SPDX-License-Identifier: AGPL-3.0-only
package migrate

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/AniBakaBaka/AniDan/internal/store"
	"github.com/go-sql-driver/mysql"
	"github.com/jackc/pgx/v5"
)

const remoteOwnershipPrefix = "anidan:migration:v1:"

// RemoteTargetBinding identifies a configured endpoint and its actual namespace.
// Only credential-free digests are persisted; user names and DSNs are excluded.
type RemoteTargetBinding struct {
	Driver          string `json:"driver"`
	EndpointSHA256  string `json:"endpoint_sha256"`
	NamespaceSHA256 string `json:"namespace_sha256"`
}

func (b RemoteTargetBinding) SHA256() string { return remoteHash(b) }

type RemoteOwnership struct {
	Version             int    `json:"version"`
	MigrationID         string `json:"migration_id"`
	TargetBindingSHA256 string `json:"target_binding_sha256"`
	SnapshotSHA256      string `json:"snapshot_sha256"`
	SchemaSHA256        string `json:"schema_sha256"`
	TargetDirSHA256     string `json:"target_dir_sha256"`
}

func remoteHash(v any) string {
	b, _ := json.Marshal(v)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func validRemoteHex(s string, bytes int) bool {
	b, err := hex.DecodeString(s)
	return err == nil && len(b) == bytes && s == strings.ToLower(s)
}

func (o RemoteOwnership) validate() error {
	if o.Version != 1 || !validRemoteHex(o.MigrationID, 16) || !validRemoteHex(o.TargetBindingSHA256, 32) || !validRemoteHex(o.SnapshotSHA256, 32) || !validRemoteHex(o.SchemaSHA256, 32) || !validRemoteHex(o.TargetDirSHA256, 32) {
		return errors.New("invalid remote migration ownership marker")
	}
	return nil
}

func ownershipComment(o RemoteOwnership) (string, error) {
	if err := o.validate(); err != nil {
		return "", err
	}
	b, _ := json.Marshal(o)
	return remoteOwnershipPrefix + base64.RawURLEncoding.EncodeToString(b), nil
}

func parseOwnershipComment(comment string) (*RemoteOwnership, error) {
	if !strings.HasPrefix(comment, remoteOwnershipPrefix) {
		if strings.HasPrefix(comment, "anidan:migration:") {
			return nil, errors.New("unsupported remote migration ownership marker")
		}
		return nil, nil
	}
	if len(comment) > 2048 {
		return nil, errors.New("invalid remote migration ownership marker")
	}
	b, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(comment, remoteOwnershipPrefix))
	if err != nil {
		return nil, errors.New("invalid remote migration ownership marker")
	}
	var owner RemoteOwnership
	decoder := json.NewDecoder(strings.NewReader(string(b)))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&owner); err != nil {
		return nil, errors.New("invalid remote migration ownership marker")
	}
	// Requiring canonical encoding also rejects duplicate keys and trailing data.
	canonical, err := ownershipComment(owner)
	if err != nil || canonical != comment {
		return nil, errors.New("invalid remote migration ownership marker")
	}
	return &owner, nil
}

// ReadRemoteOwnership is a read-only lookup for runtime startup as well as the
// importer. It deliberately imposes no migration-only DSN or privilege rules.
// An absent config table or an ordinary unmarked table returns nil.
func ReadRemoteOwnership(ctx context.Context, dialect string, q store.SQLQueryer) (*RemoteOwnership, error) {
	var comment string
	var err error
	switch dialect {
	case "postgres":
		err = q.QueryRowContext(ctx, `SELECT LEFT(COALESCE(pg_catalog.obj_description(c.oid,'pg_class'),''),2049) FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname=current_schema() AND c.relname='config'`).Scan(&comment)
	case "mysql":
		err = q.QueryRowContext(ctx, `SELECT LEFT(COALESCE(table_comment,''),2049) FROM information_schema.tables WHERE table_schema=DATABASE() AND table_name='config'`).Scan(&comment)
	default:
		return nil, errors.New("remote ownership requires postgres or mysql")
	}
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, errors.New("cannot inspect remote migration ownership")
	}
	return parseOwnershipComment(comment)
}

type remoteTarget struct {
	store               *store.Store
	conn                *sql.Conn
	binding             RemoteTargetBinding
	database, namespace string
	sessionID           int64
	locked              bool
	schemaQuery         store.SQLQueryer
}

// remoteEndpoint parses configuration in memory. Driver errors are intentionally
// replaced rather than wrapped: driver messages may contain credentials/DSNs.
// explicitPostgresSettings checks selection before pgx can merge environment or
// service-file defaults. Duplicate keys and multiple endpoint selectors fail
// closed. Authentication may still use the driver's normal credential sources.
func explicitPostgresSettings(dsn string) (map[string]string, error) {
	bad := func() (map[string]string, error) {
		return nil, errors.New("PostgreSQL target requires explicit unambiguous host and database")
	}
	if len(dsn) == 0 || len(dsn) > 65536 {
		return bad()
	}
	settings := map[string]string{}
	put := func(key, value string) bool {
		if key == "dbname" {
			key = "database"
		}
		if _, exists := settings[key]; exists {
			return false
		}
		settings[key] = value
		return true
	}
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		parsed, err := url.Parse(dsn)
		if err != nil || parsed.Fragment != "" || parsed.Opaque != "" {
			return bad()
		}
		if parsed.Host != "" {
			if !put("host", parsed.Hostname()) {
				return bad()
			}
			if parsed.Port() != "" {
				put("port", parsed.Port())
			}
		}
		if parsed.Path != "" && parsed.Path != "/" {
			put("database", strings.TrimPrefix(parsed.Path, "/"))
		}
		query, err := url.ParseQuery(parsed.RawQuery)
		if err != nil {
			return bad()
		}
		for key, values := range query {
			if len(values) != 1 || !put(key, values[0]) {
				return bad()
			}
		}
	} else {
		rest := dsn
		for {
			rest = strings.TrimLeft(rest, " \t\r\n\v\f")
			if rest == "" {
				break
			}
			eq := strings.IndexByte(rest, '=')
			if eq <= 0 {
				return bad()
			}
			key := strings.TrimSpace(rest[:eq])
			for _, ch := range key {
				if !(ch >= 'a' && ch <= 'z' || ch == '_') {
					return bad()
				}
			}
			rest = strings.TrimLeft(rest[eq+1:], " \t\r\n\v\f")
			quoted := len(rest) > 0 && rest[0] == '\''
			if quoted {
				rest = rest[1:]
			}
			var value strings.Builder
			closed := !quoted
			consumed := 0
			for consumed < len(rest) {
				ch := rest[consumed]
				consumed++
				if ch == '\\' {
					if consumed == len(rest) {
						return bad()
					}
					value.WriteByte(rest[consumed])
					consumed++
					continue
				}
				if quoted && ch == '\'' {
					closed = true
					break
				}
				if !quoted && strings.ContainsRune(" \t\r\n\v\f", rune(ch)) {
					break
				}
				value.WriteByte(ch)
			}
			if !closed || !put(key, value.String()) {
				return bad()
			}
			rest = rest[consumed:]
			if quoted && len(rest) > 0 && !strings.ContainsRune(" \t\r\n\v\f", rune(rest[0])) {
				return bad()
			}
		}
	}
	if settings["host"] == "" || settings["database"] == "" || strings.ContainsAny(settings["host"], ",\x00") || strings.Contains(settings["port"], ",") {
		return bad()
	}
	for _, key := range []string{"service", "servicefile", "hostaddr", "target_session_attrs", "options"} {
		if _, ok := settings[key]; ok {
			return bad()
		}
	}
	if os.Getenv("PGSERVICE") != "" || os.Getenv("PGSERVICEFILE") != "" || (os.Getenv("PGTARGETSESSIONATTRS") != "" && os.Getenv("PGTARGETSESSIONATTRS") != "any") {
		return bad()
	}
	return settings, nil
}

func remoteEndpoint(driver, dsn string) (string, string, error) {
	switch strings.ToLower(driver) {
	case "postgres", "postgresql", "pgx":
		explicit, err := explicitPostgresSettings(dsn)
		if err != nil {
			return "", "", err
		}
		c, err := pgx.ParseConfig(dsn)
		if err != nil {
			return "", "", errors.New("invalid PostgreSQL target configuration")
		}
		expectedPort := uint64(5432)
		if explicit["port"] != "" {
			expectedPort, err = strconv.ParseUint(explicit["port"], 10, 16)
			if err != nil || expectedPort == 0 {
				return "", "", errors.New("invalid PostgreSQL target port")
			}
		}
		if c.Host != explicit["host"] || c.Database != explicit["database"] || uint64(c.Port) != expectedPort {
			return "", "", errors.New("remote target requires one explicit endpoint and database")
		}
		for _, f := range c.Fallbacks {
			if f.Host != c.Host || f.Port != c.Port {
				return "", "", errors.New("multiple PostgreSQL target endpoints are unsupported")
			}
		}
		if _, ok := c.RuntimeParams["options"]; ok {
			return "", "", errors.New("PostgreSQL target options are unsupported")
		}
		host := strings.ToLower(c.Host)
		if strings.HasPrefix(c.Host, "/") {
			host = filepath.Clean(c.Host)
		}
		return "postgres", remoteHash([]string{"postgres", host, strconv.Itoa(int(c.Port))}), nil
	case "mysql":
		c, err := mysql.ParseDSN(dsn)
		if err != nil {
			return "", "", errors.New("invalid MySQL target configuration")
		}
		addressPart := dsn
		if slash := strings.LastIndex(addressPart, "/"); slash >= 0 {
			addressPart = addressPart[:slash]
		}
		if at := strings.LastIndex(addressPart, "@"); at >= 0 {
			addressPart = addressPart[at+1:]
		}
		explicitAddress := strings.HasPrefix(addressPart, c.Net+"(") && strings.HasSuffix(addressPart, ")") && len(addressPart) > len(c.Net)+2
		if c.DBName == "" || c.MultiStatements || !explicitAddress {
			return "", "", errors.New("MySQL target requires one database and single statements")
		}
		var endpoint string
		switch c.Net {
		case "tcp":
			host, port, e := net.SplitHostPort(c.Addr)
			if e != nil || host == "" || strings.ContainsAny(host, ",\x00") {
				return "", "", errors.New("MySQL target requires one endpoint")
			}
			endpoint = net.JoinHostPort(strings.ToLower(host), port)
		case "unix":
			if !filepath.IsAbs(c.Addr) {
				return "", "", errors.New("MySQL target requires an absolute socket path")
			}
			endpoint = filepath.Clean(c.Addr)
		default:
			return "", "", errors.New("unsupported MySQL target network")
		}
		return "mysql", remoteHash([]string{"mysql", c.Net, endpoint}), nil
	default:
		return "", "", errors.New("remote target driver must be postgres or mysql")
	}
}

func openRemoteTarget(ctx context.Context, driver, dsn string) (*remoteTarget, error) {
	dialect, endpoint, err := remoteEndpoint(driver, dsn)
	if err != nil {
		return nil, err
	}
	s, err := store.Open(ctx, dialect, dsn)
	if err != nil {
		return nil, errors.New("cannot connect to remote migration target")
	}
	if actual, ok := s.RemoteEndpointSHA256(); !ok || actual != endpoint {
		s.Close()
		return nil, errors.New("opened remote target endpoint differs from migration configuration")
	}
	s.DB.SetMaxOpenConns(1)
	s.DB.SetMaxIdleConns(0)
	s.DB.SetConnMaxLifetime(0)
	c, err := s.DB.Conn(ctx)
	if err != nil {
		s.Close()
		return nil, errors.New("cannot pin remote migration target session")
	}
	r := &remoteTarget{store: s, conn: c, binding: RemoteTargetBinding{Driver: dialect, EndpointSHA256: endpoint}}
	if err = r.inspectSession(ctx, true); err != nil {
		r.Close()
		return nil, err
	}
	r.binding.NamespaceSHA256 = remoteHash([]string{dialect, r.database, r.namespace})
	return r, nil
}

func (r *remoteTarget) queryer() store.SQLQueryer {
	if r.schemaQuery != nil {
		return r.schemaQuery
	}
	return r.conn
}

// VerifyRemoteRuntimeSchema is a read-only complete schema proof for a target
// already recognized as migration-owned. MySQL requires the same demonstrable
// metadata visibility as migration; ordinary installations do not call this.
func VerifyRemoteRuntimeSchema(ctx context.Context, s *store.Store, q store.SQLQueryer) error {
	r := &remoteTarget{store: s, schemaQuery: q}
	if s.Dialect == "postgres" {
		var count int
		var enforced bool
		if err := q.QueryRowContext(ctx, `SELECT current_database(),COALESCE(current_schema(),''),cardinality(current_schemas(false)),current_setting('session_replication_role')='origin'`).Scan(&r.database, &r.namespace, &count, &enforced); err != nil || !enforced || count != 1 || r.namespace == "" || r.namespace == "information_schema" || strings.HasPrefix(r.namespace, "pg_") {
			return errors.New("cannot inspect remote runtime namespace")
		}
	} else if s.Dialect == "mysql" {
		var enforced, checks, unique int
		if err := q.QueryRowContext(ctx, `SELECT COALESCE(DATABASE(),''),@@SESSION.foreign_key_checks,@@SESSION.check_constraint_checks,@@SESSION.unique_checks`).Scan(&r.database, &enforced, &checks, &unique); err != nil || r.database == "" || enforced != 1 || checks != 1 || unique != 1 {
			return errors.New("cannot inspect remote runtime namespace")
		}
		r.namespace = r.database
	} else {
		return errors.New("remote runtime schema requires postgres or mysql")
	}
	tables, err := r.inventory(ctx)
	if err != nil {
		return err
	}
	return r.verifyPrefix(ctx, tables, true)
}

func (r *remoteTarget) Binding() RemoteTargetBinding { return r.binding }

func (r *remoteTarget) inspectSession(ctx context.Context, initial bool) error {
	var database, namespace string
	var id int64
	switch r.store.Dialect {
	case "postgres":
		var schemas int
		var permitted, enforced bool
		err := r.conn.QueryRowContext(ctx, `SELECT current_database(),COALESCE(current_schema(),''),cardinality(current_schemas(false)),pg_backend_pid(),current_setting('session_replication_role')='origin',COALESCE(has_schema_privilege(current_schema(),'USAGE') AND has_schema_privilege(current_schema(),'CREATE'),false)`).Scan(&database, &namespace, &schemas, &id, &enforced, &permitted)
		if err != nil {
			return errors.New("cannot inspect PostgreSQL target session")
		}
		if schemas != 1 || namespace == "" || namespace == "information_schema" || strings.HasPrefix(namespace, "pg_") {
			return errors.New("PostgreSQL migration target requires one non-system schema")
		}
		if !enforced || !permitted {
			return errors.New("PostgreSQL target requires schema access and enforced foreign keys")
		}
	case "mysql":
		var enforced, checks, unique int
		if err := r.conn.QueryRowContext(ctx, `SELECT COALESCE(DATABASE(),''),CONNECTION_ID(),@@SESSION.foreign_key_checks,@@SESSION.check_constraint_checks,@@SESSION.unique_checks`).Scan(&database, &id, &enforced, &checks, &unique); err != nil {
			return errors.New("cannot inspect MySQL target session")
		}
		namespace = database
		if database == "" || database == "mysql" || database == "information_schema" || database == "performance_schema" || database == "sys" || enforced != 1 || checks != 1 || unique != 1 {
			return errors.New("MySQL target requires a dedicated database and enforced foreign keys")
		}
	}
	if initial {
		r.database = database
		r.namespace = namespace
		r.sessionID = id
		return nil
	}
	if database != r.database || namespace != r.namespace || id != r.sessionID {
		return errors.New("remote target session or namespace changed")
	}
	return nil
}

func (r *remoteTarget) Acquire(ctx context.Context) error {
	if r.locked {
		return r.inspectSession(ctx, false)
	}
	if err := r.inspectSession(ctx, false); err != nil {
		return err
	}
	var acquired bool
	if r.store.Dialect == "postgres" {
		h := sha256.Sum256([]byte("anidan:migration:" + r.binding.NamespaceSHA256))
		if err := r.conn.QueryRowContext(ctx, "SELECT pg_try_advisory_lock($1)", int64(binary.BigEndian.Uint64(h[:8]))).Scan(&acquired); err != nil {
			return errors.New("cannot acquire remote migration lock")
		}
	} else {
		var acquiredInt sql.NullInt64
		if err := r.conn.QueryRowContext(ctx, "SELECT GET_LOCK(?,0)", "anidan:migrate:"+r.binding.NamespaceSHA256[:48]).Scan(&acquiredInt); err != nil || !acquiredInt.Valid {
			return errors.New("cannot acquire remote migration lock")
		}
		acquired = acquiredInt.Int64 == 1
	}
	if !acquired {
		return errors.New("remote target is locked by another migration")
	}
	r.locked = true
	return r.inspectSession(ctx, false)
}

func (r *remoteTarget) Ownership(ctx context.Context) (*RemoteOwnership, error) {
	if err := r.inspectSession(ctx, false); err != nil {
		return nil, err
	}
	return ReadRemoteOwnership(ctx, r.store.Dialect, r.conn)
}

func remoteTableOrder() []string {
	names := []string{"config"}
	for _, name := range store.Tables() {
		if name != "config" {
			names = append(names, name)
		}
	}
	return names
}

// Preflight performs the same ownership/visibility/prefix checks as schema
// preparation but never creates or changes a persistent database object.
func (r *remoteTarget) Preflight(ctx context.Context, owner RemoteOwnership, resume bool) error {
	if !r.locked {
		return errors.New("remote migration lock is required before preflight")
	}
	if err := owner.validate(); err != nil {
		return err
	}
	if owner.TargetBindingSHA256 != r.binding.SHA256() || owner.SchemaSHA256 != store.SchemaFingerprint() {
		return errors.New("remote migration ownership does not match target or schema")
	}
	if err := r.inspectSession(ctx, false); err != nil {
		return err
	}
	tables, err := r.inventory(ctx)
	if err != nil {
		return err
	}
	present, err := r.Ownership(ctx)
	if err != nil {
		return err
	}
	if len(tables) > 0 {
		if !resume || present == nil || *present != owner {
			return errors.New("remote target is nonempty or owned by a different migration")
		}
		return r.verifyPrefix(ctx, tables, false)
	}
	if present != nil {
		return errors.New("remote ownership exists without an inspectable config table")
	}
	return nil
}

// InspectRemoteBinding derives a read-only identity for a migration-owned
// runtime connection. Ordinary installations need not call this helper.
func InspectRemoteBinding(ctx context.Context, driver, dsn string, q store.SQLQueryer) (RemoteTargetBinding, error) {
	dialect, endpoint, err := remoteEndpoint(driver, dsn)
	if err != nil {
		return RemoteTargetBinding{}, err
	}
	var database, namespace string
	if dialect == "postgres" {
		var schemas int
		if err = q.QueryRowContext(ctx, `SELECT current_database(),COALESCE(current_schema(),''),cardinality(current_schemas(false))`).Scan(&database, &namespace, &schemas); err != nil {
			return RemoteTargetBinding{}, errors.New("cannot inspect remote target identity")
		}
		if schemas != 1 || namespace == "" || namespace == "information_schema" || strings.HasPrefix(namespace, "pg_") {
			return RemoteTargetBinding{}, errors.New("PostgreSQL migration target requires one non-system schema")
		}
	} else {
		if err = q.QueryRowContext(ctx, `SELECT COALESCE(DATABASE(),'')`).Scan(&database); err != nil {
			return RemoteTargetBinding{}, errors.New("cannot inspect remote target identity")
		}
		namespace = database
		if database == "" || database == "mysql" || database == "information_schema" || database == "performance_schema" || database == "sys" {
			return RemoteTargetBinding{}, errors.New("MySQL migration target requires a dedicated database")
		}
	}
	return RemoteTargetBinding{Driver: dialect, EndpointSHA256: endpoint, NamespaceSHA256: remoteHash([]string{dialect, database, namespace})}, nil
}

func (r *remoteTarget) PrepareSchema(ctx context.Context, owner RemoteOwnership, resume bool) error {
	if !r.locked {
		return errors.New("remote migration lock is required before schema preparation")
	}
	if err := owner.validate(); err != nil {
		return err
	}
	if owner.TargetBindingSHA256 != r.binding.SHA256() || owner.SchemaSHA256 != store.SchemaFingerprint() {
		return errors.New("remote migration ownership does not match target or schema")
	}
	if err := r.inspectSession(ctx, false); err != nil {
		return err
	}
	if err := r.Preflight(ctx, owner, resume); err != nil {
		return err
	}
	tables, err := r.inventory(ctx)
	if err != nil {
		return err
	}
	if len(tables) == 0 {
		comment, _ := ownershipComment(owner)
		ddl, _ := r.store.FreshTableDDL("config")
		if r.store.Dialect == "mysql" {
			if _, err = r.conn.ExecContext(ctx, ddl+" COMMENT='"+comment+"'"); err != nil {
				return errors.New("cannot create owned remote config table")
			}
		} else {
			tx, e := r.conn.BeginTx(ctx, nil)
			if e != nil {
				return errors.New("cannot begin remote ownership transaction")
			}
			if _, e = tx.ExecContext(ctx, ddl); e == nil {
				_, e = tx.ExecContext(ctx, "COMMENT ON TABLE "+r.store.Quote("config")+" IS '"+comment+"'")
			}
			if e != nil {
				tx.Rollback()
				return errors.New("cannot create owned remote config table")
			}
			if e = tx.Commit(); e != nil {
				return errors.New("remote ownership commit outcome is uncertain")
			}
		}
		tables = append(tables, "config")
	}
	existing := map[string]bool{}
	for _, name := range tables {
		existing[name] = true
	}
	for _, name := range remoteTableOrder() {
		if err = r.inspectSession(ctx, false); err != nil {
			return err
		}
		if !existing[name] {
			ddl, _ := r.store.FreshTableDDL(name)
			if _, err = r.conn.ExecContext(ctx, ddl); err != nil {
				return fmt.Errorf("cannot create remote table %s", name)
			}
		}
		missing, e := r.missingIndexes(ctx, name)
		if e != nil {
			return e
		}
		ddls, _ := r.store.FreshIndexDDL(name)
		for i := missing; i < len(ddls); i++ {
			if _, err = r.conn.ExecContext(ctx, ddls[i]); err != nil {
				return fmt.Errorf("cannot create remote index for %s", name)
			}
		}
	}
	return r.VerifySchema(ctx)
}

func (r *remoteTarget) VerifySchema(ctx context.Context) error {
	if err := r.inspectSession(ctx, false); err != nil {
		return err
	}
	tables, err := r.inventory(ctx)
	if err != nil {
		return err
	}
	return r.verifyPrefix(ctx, tables, true)
}

func (r *remoteTarget) ResetSequences(ctx context.Context) error {
	if !r.locked {
		return errors.New("remote migration lock is required before sequence reset")
	}
	if err := r.inspectSession(ctx, false); err != nil {
		return err
	}
	if err := r.store.ResetSequencesWith(ctx, r.conn); err != nil {
		return errors.New("cannot reset remote target sequences")
	}
	return nil
}

func (r *remoteTarget) Close() error {
	// Close the pool first: returning the pinned connection must destroy its
	// physical session, even if cancellation prevents explicit advisory unlock.
	var err error
	if r.store != nil {
		err = r.store.Close()
	}
	if r.conn != nil {
		if e := r.conn.Close(); err == nil && !errors.Is(e, sql.ErrConnDone) {
			err = e
		}
	}
	return err
}
