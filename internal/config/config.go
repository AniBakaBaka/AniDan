// SPDX-License-Identifier: AGPL-3.0-only
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	LegacyDatabase           bool            `json:"legacyDatabase,omitempty"` // Use the existing Misaka schema without DDL.
	RequireMigrationEvidence bool            `json:"-"`
	MigrationReview          MigrationReview `json:"migrationReview"`
	Cache                    Cache           `json:"cache"`
	Warnings                 []string        `json:"-"`
	DockerEnabled            bool            `json:"dockerEnabled"`
	DockerSocket             string          `json:"dockerSocket"`
	DockerContainerID        string          `json:"dockerContainerId"`
	DockerAllowedImages      []string        `json:"dockerAllowedImages"`
	DockerExternalController bool            `json:"dockerExternalController"`
	ReleaseRepository        string          `json:"releaseRepository"`
	Listen                   string          `json:"listen"`
	Driver                   string          `json:"driver"`
	DSN                      string          `json:"dsn"`
	DataDir                  string          `json:"dataDir"`
	JWTSecret                string          `json:"jwtSecret"`
	JWTAlgorithm             string          `json:"jwtAlgorithm"`
	SessionAudience          string          `json:"sessionAudience,omitempty"` // Separates sessions without changing the MFA encryption key.
	AdminUsername            string          `json:"adminUsername"`
	AdminPassword            string          `json:"adminPassword"`
	ExternalAPIKey           string          `json:"externalApiKey"`
	WebhookAPIKey            string          `json:"webhookApiKey"`
	PublicURL                string          `json:"publicURL"`
	Timezone                 string          `json:"timezone"`
	Workers                  int             `json:"workers"`
	QueueSize                int             `json:"queueSize"`
	WebDir                   string          `json:"webDir"`
	StaticDir                string          `json:"staticDir"`
	ReadRoots                []string        `json:"readRoots"`
	WriteRoots               []string        `json:"writeRoots"`
	MaxBodyBytes             int64           `json:"maxBodyBytes"`
}

func Defaults() Config {
	return Config{MigrationReview: MigrationReviewDefaults(), Cache: CacheDefaults(), Listen: ":7769", Driver: "sqlite", DataDir: "data", JWTAlgorithm: "HS256", AdminUsername: "admin", Timezone: "Asia/Shanghai", Workers: 2, QueueSize: 64, WebDir: "web/dist", StaticDir: "static", MaxBodyBytes: 32 << 20}
}
func Load(path string) (Config, error) {
	c := Defaults()
	if path != "" {
		if strings.HasSuffix(strings.ToLower(path), ".yml") || strings.HasSuffix(strings.ToLower(path), ".yaml") {
			legacy, e := ReadLegacy(path)
			if e != nil {
				return c, e
			}
			if e = c.applyLegacy(legacy, true); e != nil {
				return c, e
			}
		} else {
			f, e := os.Open(path)
			if e != nil {
				return c, e
			}
			defer f.Close()
			d := json.NewDecoder(f)
			d.DisallowUnknownFields()
			if e = d.Decode(&c); e != nil {
				return c, fmt.Errorf("configuration: %w", e)
			}
			var extra any
			if e = d.Decode(&extra); e != io.EOF {
				return c, errors.New("configuration must contain exactly one JSON value")
			}
		}
	}
	if e := c.Cache.applyEnvironment(); e != nil {
		return c, e
	}
	if e := c.MigrationReview.applyEnvironment(); e != nil {
		return c, e
	}
	// Explicit legacy environment variables remain readable during staged migration;
	// ANIDAN_* overrides always win. No legacy DB environment is guessed silently.
	for k, v := range map[string]*string{"DANMUAPI_JWT__SECRET_KEY": &c.JWTSecret, "DANMUAPI_JWT__ALGORITHM": &c.JWTAlgorithm, "DANMUAPI_TZ": &c.Timezone, "DANMUAPI_ADMIN__INITIAL_USER": &c.AdminUsername, "DANMUAPI_ADMIN__INITIAL_PASSWORD": &c.AdminPassword} {
		if x, ok := os.LookupEnv(k); ok {
			*v = x
		}
	}

	vals := map[string]*string{"LISTEN": &c.Listen, "DRIVER": &c.Driver, "DSN": &c.DSN, "DATA_DIR": &c.DataDir, "JWT_SECRET": &c.JWTSecret, "JWT_ALGORITHM": &c.JWTAlgorithm, "ADMIN_USERNAME": &c.AdminUsername, "ADMIN_PASSWORD": &c.AdminPassword, "EXTERNAL_API_KEY": &c.ExternalAPIKey, "WEBHOOK_API_KEY": &c.WebhookAPIKey, "PUBLIC_URL": &c.PublicURL, "TIMEZONE": &c.Timezone, "WEB_DIR": &c.WebDir, "STATIC_DIR": &c.StaticDir}
	vals["DOCKER_SOCKET"] = &c.DockerSocket
	vals["DOCKER_CONTAINER_ID"] = &c.DockerContainerID
	vals["RELEASE_REPOSITORY"] = &c.ReleaseRepository
	for k, v := range vals {
		if x, ok := os.LookupEnv("ANIDAN_" + k); ok {
			*v = x
		}
	}
	for k, v := range map[string]*int{"WORKERS": &c.Workers, "QUEUE_SIZE": &c.QueueSize} {
		if x, ok := os.LookupEnv("ANIDAN_" + k); ok {
			n, e := strconv.Atoi(x)
			if e != nil {
				return c, fmt.Errorf("ANIDAN_%s: %w", k, e)
			}
			*v = n
		}
	}
	for k, v := range map[string]*bool{"DOCKER_ENABLED": &c.DockerEnabled, "DOCKER_EXTERNAL_CONTROLLER": &c.DockerExternalController} {
		if x, ok := os.LookupEnv("ANIDAN_" + k); ok {
			b, e := strconv.ParseBool(x)
			if e != nil {
				return c, fmt.Errorf("ANIDAN_%s must be boolean", k)
			}
			*v = b
		}
	}
	if x, ok := os.LookupEnv("ANIDAN_DOCKER_ALLOWED_IMAGES"); ok {
		c.DockerAllowedImages = nil
		for _, v := range strings.Split(x, ",") {
			if v = strings.TrimSpace(v); v != "" {
				c.DockerAllowedImages = append(c.DockerAllowedImages, v)
			}
		}
	}
	if x, ok := os.LookupEnv("ANIDAN_WRITE_ROOTS"); ok {
		c.WriteRoots = filepath.SplitList(x)
	}
	if x, ok := os.LookupEnv("ANIDAN_READ_ROOTS"); ok {
		c.ReadRoots = filepath.SplitList(x)
	}
	if c.DSN == "" {
		if c.Driver != "sqlite" {
			return c, errors.New("ANIDAN_DSN is required for MySQL/PostgreSQL")
		}
		c.DSN = filepath.Join(c.DataDir, "anidan.db")
	}
	return c, c.Validate()
}
func (c Config) Validate() error {
	if c.SessionAudience != "" {
		if len(c.SessionAudience) != 64 || strings.IndexFunc(c.SessionAudience, func(r rune) bool {
			return !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f')
		}) >= 0 {
			return errors.New("sessionAudience must be exactly 64 lowercase hexadecimal characters")
		}
	}
	if e := c.MigrationReview.Validate(); e != nil {
		return e
	}
	if e := c.Cache.Validate(); e != nil {
		return e
	}
	if c.Listen == "" || c.DataDir == "" || c.Workers < 1 || c.Workers > 64 || c.QueueSize < 1 || c.QueueSize > 10000 {
		return errors.New("invalid listen/dataDir/workers/queueSize configuration")
	}
	if c.MaxBodyBytes < 1024 || c.MaxBodyBytes > 1<<30 {
		return errors.New("maxBodyBytes must be 1 KiB..1 GiB")
	}
	if _, e := time.LoadLocation(c.Timezone); e != nil {
		return e
	}
	switch strings.ToLower(c.Driver) {
	case "sqlite", "sqlite3", "mysql", "postgres", "postgresql", "pgx":
	default:
		return errors.New("driver must be sqlite, mysql or postgres")
	}
	if c.LegacyDatabase && (c.Driver == "sqlite" || c.Driver == "sqlite3") {
		return errors.New("legacyDatabase requires MySQL or PostgreSQL")
	}
	switch c.JWTAlgorithm {
	case "HS256", "HS384", "HS512":
	default:
		return errors.New("JWT algorithm must be HS256, HS384 or HS512")
	}
	return nil
}
