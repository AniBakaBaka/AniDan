// SPDX-License-Identifier: AGPL-3.0-only
package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/AniBakaBaka/AniDan/internal/config"
)

func TestRuntimeStartsWithoutFileLogging(t *testing.T) {
	cfg := config.Defaults()
	cfg.DataDir = t.TempDir()
	cfg.DSN = filepath.Join(cfg.DataDir, "test.db")
	cfg.JWTSecret = "test-runtime-secret-at-least-16"
	cfg.AdminPassword = "test-password"
	// A legacy archive shape the bounded writer cannot manage must not prevent
	// database initialization or normal runtime startup. It must remain intact.
	logs := filepath.Join(cfg.DataDir, "logs")
	if err := os.Mkdir(logs, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(logs, "app.log.old")
	if err := os.WriteFile(path, []byte("legacy content"), 0600); err != nil {
		t.Fatal(err)
	}
	app, err := openApplicationRuntime(context.Background(), cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	if app.(*databaseApplication).logfile != nil {
		t.Error("incompatible log directory should use stderr")
	}
	if err := app.Close(); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != "legacy content" {
		t.Fatal("legacy archive was modified")
	}
}
