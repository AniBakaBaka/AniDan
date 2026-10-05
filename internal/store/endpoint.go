// SPDX-License-Identifier: AGPL-3.0-only
package store

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"net"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/go-sql-driver/mysql"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
)

// endpointProof binds only the connector's configured endpoint to the exact
// pool created from that parsed configuration. It contains no user name, DSN,
// credential material, or credential-derived digest.
type endpointProof struct {
	db      *sql.DB
	dialect string
	digest  string
}

// RemoteEndpointSHA256 returns credential-free provenance for the actual pool.
// Replacing the public DB or Dialect fields invalidates the proof. A caller-
// supplied pool or multi-endpoint connector deliberately has no such proof.
func (s *Store) RemoteEndpointSHA256() (string, bool) {
	if s == nil || s.DB == nil || s.endpoint.db != s.DB || s.endpoint.dialect != s.Dialect || s.endpoint.digest == "" {
		return "", false
	}
	return s.endpoint.digest, true
}

func endpointDigest(parts ...string) string {
	b, _ := json.Marshal(parts)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// openRemoteConnector parses exactly once. The same immutable driver config
// determines both physical connections and the retained endpoint proof, even if
// process environment defaults later change. Ordinary multi-host installs keep
// working, but cannot be treated as a single owned migration endpoint.
func openRemoteConnector(dialect, dsn string) (*sql.DB, string, error) {
	if dialect == "postgres" {
		config, err := pgx.ParseConfig(dsn)
		if err != nil {
			return nil, "", err
		}
		digest := ""
		single := config.Host != "" && !strings.ContainsAny(config.Host, ",\x00")
		for _, fallback := range config.Fallbacks {
			if fallback.Host != config.Host || fallback.Port != config.Port {
				single = false
			}
		}
		if single {
			host := strings.ToLower(config.Host)
			if strings.HasPrefix(config.Host, "/") {
				host = filepath.Clean(config.Host)
			}
			digest = endpointDigest("postgres", host, strconv.Itoa(int(config.Port)))
		}
		return sql.OpenDB(stdlib.GetConnector(*config)), digest, nil
	}
	config, err := mysql.ParseDSN(dsn)
	if err != nil {
		return nil, "", err
	}
	connector, err := mysql.NewConnector(config)
	if err != nil {
		return nil, "", err
	}
	digest := ""
	switch config.Net {
	case "tcp":
		host, port, e := net.SplitHostPort(config.Addr)
		if e == nil && host != "" && !strings.ContainsAny(host, ",\x00") {
			digest = endpointDigest("mysql", config.Net, net.JoinHostPort(strings.ToLower(host), port))
		}
	case "unix":
		if filepath.IsAbs(config.Addr) {
			digest = endpointDigest("mysql", config.Net, filepath.Clean(config.Addr))
		}
	}
	return sql.OpenDB(connector), digest, nil
}
