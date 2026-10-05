// SPDX-License-Identifier: AGPL-3.0-only
package config

import (
	"errors"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/go-sql-driver/mysql"
	"gopkg.in/yaml.v3"
)

// Legacy is a typed, inert YAML document. No Python objects or YAML tags execute.
type Legacy struct {
	Cache  LegacyCache `yaml:"cache"`
	Server struct {
		Host string `yaml:"host"`
		Port int    `yaml:"port"`
		IPv6 bool   `yaml:"ipv6"`
	} `yaml:"server"`
	Database struct {
		Type, Host, User, Password, Name string
		Port                             int
	} `yaml:"database"`
	JWT struct {
		Secret        string `yaml:"secret_key"`
		Algorithm     string `yaml:"algorithm"`
		ExpireMinutes int    `yaml:"access_token_expire_minutes"`
	} `yaml:"jwt"`
	Admin struct {
		User     string `yaml:"initial_user"`
		Password string `yaml:"initial_password"`
	} `yaml:"admin"`
	Timezone string `yaml:"tz"`
}

func ReadLegacy(path string) (Legacy, error) {
	var legacy Legacy
	f, e := os.Open(path)
	if e != nil {
		return legacy, e
	}
	defer f.Close()
	d := yaml.NewDecoder(io.LimitReader(f, 1<<20))
	var document yaml.Node
	if e = d.Decode(&document); e != nil {
		return legacy, errors.New("legacy configuration YAML is invalid")
	}
	if len(document.Content) != 1 || document.Content[0].Kind != yaml.MappingNode {
		return legacy, errors.New("legacy configuration must be a YAML mapping")
	}
	// Validate cache separately so YAML null does not silently masquerade as
	// an absent cache section. Other upstream sections stay backward compatible.
	root := document.Content[0]
	for i := 0; i < len(root.Content); i += 2 {
		if root.Content[i].Value == "cache" {
			if e = legacy.Cache.UnmarshalYAML(root.Content[i+1]); e != nil {
				return legacy, e
			}
		}
	}
	if e = document.Decode(&legacy); e != nil {
		return legacy, errors.New("legacy configuration YAML has an invalid or duplicate setting")
	}
	var trailing any
	if e = d.Decode(&trailing); e != io.EOF {
		return legacy, errors.New("legacy configuration must contain exactly one YAML document")
	}
	return legacy, nil
}
func (c *Config) applyLegacy(legacy Legacy, includeDatabase bool) error {
	c.Cache.applyLegacy(legacy.Cache)
	if legacy.Server.Port != 0 {
		if legacy.Server.Port < 1 || legacy.Server.Port > 65535 {
			return errors.New("legacy server port out of range")
		}
		host := legacy.Server.Host
		if host == "" {
			host = "0.0.0.0"
		}
		c.Listen = net.JoinHostPort(host, strconv.Itoa(legacy.Server.Port))
	}
	if legacy.JWT.Secret != "" {
		c.JWTSecret = legacy.JWT.Secret
	}
	if legacy.JWT.Algorithm != "" {
		c.JWTAlgorithm = legacy.JWT.Algorithm
	}
	if legacy.Timezone != "" {
		c.Timezone = legacy.Timezone
	}
	if legacy.Admin.User != "" {
		c.AdminUsername = legacy.Admin.User
	}
	if legacy.Admin.Password != "" {
		c.AdminPassword = legacy.Admin.Password
	}
	dbSpecified := legacy.Database.Type != "" || legacy.Database.Host != "" || legacy.Database.Port != 0 || legacy.Database.User != "" || legacy.Database.Password != "" || legacy.Database.Name != ""
	if includeDatabase && dbSpecified {
		db := legacy.Database
		if db.Type != "" {
			c.Driver = db.Type
		}
		switch strings.ToLower(c.Driver) {
		case "mysql":
			if db.Port == 0 {
				db.Port = 3306
			}
			v := mysql.NewConfig()
			v.User = db.User
			v.Passwd = db.Password
			v.Net = "tcp"
			v.Addr = net.JoinHostPort(db.Host, strconv.Itoa(db.Port))
			v.DBName = db.Name
			v.ParseTime = true
			v.Params = map[string]string{"charset": "utf8mb4"}
			c.DSN = v.FormatDSN()
		case "postgres", "postgresql", "pgx":
			if db.Port == 0 {
				db.Port = 5432
			}
			u := url.URL{Scheme: "postgres", Host: net.JoinHostPort(db.Host, strconv.Itoa(db.Port)), Path: "/" + db.Name, User: url.UserPassword(db.User, db.Password)}
			c.DSN = u.String()
		default:
			return errors.New("legacy config database type must be mysql or postgresql")
		}
	}
	return nil
}

// TranslateLegacyForMigration intentionally never copies source DB connection or
// plaintext initial admin password or source Redis URL into the new standalone
// target configuration. Environment overrides are deliberately not consulted.
func TranslateLegacyForMigration(path, targetDir string) (Config, error) {
	legacy, e := ReadLegacy(path)
	if e != nil {
		return Config{}, e
	}
	c := Defaults()
	if e = c.applyLegacy(legacy, false); e != nil {
		return c, e
	}
	target, e := filepath.Abs(targetDir)
	if e != nil {
		return c, e
	}
	c.DataDir = target
	c.Driver = "sqlite"
	c.DSN = filepath.Join(target, "anidan.db")
	c.AdminPassword = ""
	if c.Cache.RedisURL != "" {
		c.Warnings = append(c.Warnings, "Source Redis/Valkey endpoint and credentials were omitted from the target runtime configuration; the source cache was not contacted or cleared")
	}
	if c.Cache.Backend == "redis" || c.Cache.Backend == "valkey" {
		c.Warnings = append(c.Warnings, "Source Redis/Valkey cache selection was replaced with isolated target hybrid caching (memory plus target SQLite); configure an explicit target endpoint separately to opt into Redis/Valkey")
	}
	if c.Cache, e = c.Cache.ForIsolatedTarget(target); e != nil {
		return c, e
	}
	return c, c.Validate()
}
