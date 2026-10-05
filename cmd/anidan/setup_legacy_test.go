// SPDX-License-Identifier: AGPL-3.0-only
package main

import (
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AniBakaBaka/AniDan/internal/config"
	"github.com/go-sql-driver/mysql"
)

const setupLegacyYAML = `database:
  type: mysql
  host: original-db
  port: 3307
  user: original-user
  password: 'original-password'
  name: original-library
jwt:
  secret_key: original-jwt-secret-at-least-16
  algorithm: HS384
tz: Asia/Tokyo
server:
  port: 7768
`

const setupLegacyCompose = `services:
  app:
    image: l429609201/danmu_api_server:latest
    container_name: danmu-api
    environment:
      DANMUAPI_DATABASE__TYPE: mysql
      DANMUAPI_DATABASE__HOST: compose-db
      DANMUAPI_DATABASE__PORT: 3306
      DANMUAPI_DATABASE__USER: compose-user
      DANMUAPI_DATABASE__PASSWORD: compose-password
      DANMUAPI_DATABASE__NAME: compose-library
      DANMUAPI_JWT__SECRET_KEY: compose-jwt-secret-at-least-16
      DANMUAPI_JWT__ALGORITHM: HS512
      DANMUAPI_TZ: Asia/Hong_Kong
    volumes:
      - ./config:/app/config
      - /var/run/docker.sock:/var/run/docker.sock
`

func setupSettings(t *testing.T, dir, name, raw string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestInstallationMountedLegacy(t *testing.T) {
	dir := t.TempDir()
	setupSettings(t, dir, "config.yml", setupLegacyYAML)
	base := config.Defaults()
	base.Listen = ":17769"
	t.Setenv("ANIDAN_DRIVER", "sqlite")
	t.Setenv("ANIDAN_JWT_SECRET", "unrelated-new-process-secret")
	c, err := installationConfig(base, installationInput{Legacy: true, DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	db, err := mysql.ParseDSN(c.DSN)
	if err != nil {
		t.Fatal(err)
	}
	if !c.LegacyDatabase || c.DataDir != dir || c.Listen != base.Listen || c.Driver != "mysql" || db.Addr != "original-db:3307" || db.User != "original-user" || db.Passwd != "original-password" || db.DBName != "original-library" || c.JWTSecret != "original-jwt-secret-at-least-16" || c.JWTAlgorithm != "HS384" || c.Timezone != "Asia/Tokyo" || c.AdminPassword != "" || c.Cache != base.Cache {
		t.Fatal("mounted settings or local runtime settings were not preserved")
	}
}

func TestInstallationMountedCompose(t *testing.T) {
	for _, name := range []string{"docker-compose.yml", "docker-compose.yaml", "compose.yml", "compose.yaml"} {
		for _, withConfig := range []bool{false, true} {
			t.Run(name+map[bool]string{false: "/compose-only", true: "/overlay"}[withConfig], func(t *testing.T) {
				dir := t.TempDir()
				if withConfig {
					setupSettings(t, dir, "config.yml", setupLegacyYAML)
				}
				setupSettings(t, dir, name, setupLegacyCompose)
				c, err := installationConfig(config.Defaults(), installationInput{Legacy: true, DataDir: dir, LegacyHost: "host.docker.internal"})
				if err != nil {
					t.Fatal(err)
				}
				db, err := mysql.ParseDSN(c.DSN)
				if err != nil || db.Addr != "host.docker.internal:3306" || db.User != "compose-user" || db.Passwd != "compose-password" || db.DBName != "compose-library" || c.JWTSecret != "compose-jwt-secret-at-least-16" || c.JWTAlgorithm != "HS512" || c.Timezone != "Asia/Hong_Kong" || c.DockerEnabled {
					t.Fatal("Compose precedence or host-only override failed")
				}
			})
		}
	}
}

func TestInstallationLegacyOverrides(t *testing.T) {
	dir := t.TempDir()
	setupSettings(t, dir, "config.yml", setupLegacyYAML)
	setupSettings(t, dir, "compose.yaml", "invalid: compose")
	in := installationInput{Legacy: true, DataDir: dir, LegacyConfig: filepath.Join(dir, "config.yml"), JWTSecret: "explicit-original-secret", Timezone: "UTC", Driver: "postgres", Host: "new-db", Port: 5433, Database: "original-library", User: "old-user", Password: "old-password", TLS: "require"}
	c, err := installationConfig(config.Defaults(), in)
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(c.DSN)
	if err != nil || c.Driver != "postgres" || u.Host != "new-db:5433" || u.Query().Get("sslmode") != "require" || c.JWTSecret != in.JWTSecret || c.Timezone != "UTC" {
		t.Fatal("explicit overrides did not win")
	}
	// Host-only overrides retain the original PostgreSQL credentials and port.
	setupSettings(t, dir, "config.yml", strings.ReplaceAll(strings.ReplaceAll(setupLegacyYAML, "type: mysql", "type: postgresql"), "3307", "5433"))
	in = installationInput{Legacy: true, DataDir: dir, LegacyConfig: filepath.Join(dir, "config.yml"), LegacyHost: "2001:db8::1"}
	c, err = installationConfig(config.Defaults(), in)
	if err != nil {
		t.Fatal(err)
	}
	u, err = url.Parse(c.DSN)
	if err != nil || u.Host != "[2001:db8::1]:5433" || u.User.Username() != "original-user" {
		t.Fatal("PostgreSQL host override changed connection identity")
	}
}

func TestInstallationLegacyMissingOrAmbiguousSettings(t *testing.T) {
	for _, scenario := range []string{"missing", "missing-secret", "multiple-compose", "compose-override", "unresolved-env", "oversized", "invalid-host"} {
		t.Run(scenario, func(t *testing.T) {
			dir := t.TempDir()
			in := installationInput{Legacy: true, DataDir: dir}
			if scenario != "missing" {
				setupSettings(t, dir, "config.yml", setupLegacyYAML)
			}
			switch scenario {
			case "missing-secret":
				setupSettings(t, dir, "config.yml", strings.ReplaceAll(setupLegacyYAML, "original-jwt-secret-at-least-16", ""))
			case "multiple-compose":
				setupSettings(t, dir, "compose.yaml", setupLegacyCompose)
				setupSettings(t, dir, "docker-compose.yml", setupLegacyCompose)
			case "compose-override":
				setupSettings(t, dir, "compose.override.yml", setupLegacyCompose)
			case "unresolved-env":
				setupSettings(t, dir, "compose.yaml", strings.ReplaceAll(setupLegacyCompose, "compose-password", "${DB_PASSWORD}"))
			case "oversized":
				setupSettings(t, dir, "config.yml", setupLegacyYAML+strings.Repeat(" ", 1<<20))
			case "invalid-host":
				in.LegacyHost = "https://new-db:3306"
			}
			base := config.Defaults()
			base.JWTSecret = "must-not-replace-original-secret"
			if _, err := installationConfig(base, in); err == nil {
				t.Fatal("unsafe or incomplete settings were accepted")
			}
		})
	}
}

func TestInstallationNewStillGeneratesSecret(t *testing.T) {
	c, err := installationConfig(config.Defaults(), installationInput{Driver: "sqlite", DataDir: t.TempDir(), AdminUsername: "owner", AdminPassword: "test-password"})
	if err != nil || c.LegacyDatabase || len(c.JWTSecret) < 16 || c.AdminUsername != "owner" {
		t.Fatal("new installation regressed")
	}
}
