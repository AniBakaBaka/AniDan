// SPDX-License-Identifier: AGPL-3.0-only
package server

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AniBakaBaka/AniDan/internal/config"
	"github.com/AniBakaBaka/AniDan/internal/store"
)

// SQLite fixtures exercise the reference audit only, not legacy SQL attachment.
func installationFilesFixture(t *testing.T) *Server {
	t.Helper()
	cfg := config.Defaults()
	cfg.DataDir = t.TempDir()
	s, err := store.Open(context.Background(), "sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	for _, statement := range []string{
		"CREATE TABLE episode (id INTEGER PRIMARY KEY, danmaku_file_path TEXT)",
		"CREATE TABLE anime (id INTEGER PRIMARY KEY, local_image_path TEXT, image_url TEXT)",
		"CREATE TABLE local_danmaku_items (id INTEGER PRIMARY KEY, file_path TEXT, nfo_path TEXT, poster_url TEXT)",
		"CREATE TABLE media_items (id INTEGER PRIMARY KEY, poster_url TEXT)",
		"CREATE TABLE external_calendar_item (id INTEGER PRIMARY KEY, image_url TEXT)",
	} {
		if _, err := s.DB.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	return &Server{Store: s, DataDir: cfg.DataDir, Config: cfg}
}

func TestInstallationStaleReferencesWarnAndPreserveRows(t *testing.T) {
	s := installationFilesFixture(t)
	ctx := context.Background()
	const stale = "/usr/local/lib/python3.11/site-packages/setuptools/command/launcher manifest.xml"
	for _, path := range []string{stale, "/app/config/missing.xml", s.DataDir} {
		if _, err := s.Store.DB.Exec("INSERT INTO local_danmaku_items(file_path) VALUES(?)", path); err != nil {
			t.Fatal(err)
		}
	}
	warnings, err := s.installationFileWarnings(ctx)
	if err != nil || len(warnings) != 1 || !strings.Contains(warnings[0], "3 项") {
		t.Fatalf("stale references must not block attachment: %v, %v", warnings, err)
	}
	rows, err := s.Store.List(ctx, "local_danmaku_items", nil, 10, 0)
	if err != nil || len(rows) != 3 || rows[0]["file_path"] != stale {
		t.Fatal("audit altered original references")
	}
	if strings.Contains(warnings[0], stale) {
		t.Fatal("summary should not expose arbitrary database paths")
	}
}

func TestInstallationAvailableReferencesAndExternalReadRoot(t *testing.T) {
	s := installationFilesFixture(t)
	external := t.TempDir()
	for _, dir := range []string{s.DataDir, external} {
		for _, name := range []string{"episode.xml", "poster.png"} {
			if err := os.WriteFile(filepath.Join(dir, name), []byte("fixture"), 0600); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, err := s.Store.DB.Exec("INSERT INTO episode VALUES(1, '/app/config/episode.xml')"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Store.DB.Exec("INSERT INTO anime VALUES(1, NULL, 'https://example.invalid/poster.png')"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Store.DB.Exec("INSERT INTO local_danmaku_items VALUES(1, ?, NULL, 'poster.png')", filepath.Join(external, "episode.xml")); err != nil {
		t.Fatal(err)
	}
	file, err := s.localOpenAllowed(s.normalizeStoredPath("/app/config/episode.xml"))
	if err != nil {
		t.Fatalf("mapped fixture is unreadable: %v", err)
	}
	file.Close()
	warnings, err := s.installationFileWarnings(context.Background())
	if err != nil || len(warnings) != 1 || !strings.Contains(warnings[0], "2 项") {
		t.Fatalf("unapproved external references must be reported: %v, %v", warnings, err)
	}
	s.Config.ReadRoots = []string{external}
	warnings, err = s.installationFileWarnings(context.Background())
	if err != nil || len(warnings) != 0 {
		t.Fatalf("mounted files, relative posters and remote URLs must work: %v, %v", warnings, err)
	}
}

func TestInstallationReferenceQueryFailureStillBlocks(t *testing.T) {
	s := installationFilesFixture(t)
	if _, err := s.Store.DB.Exec("DROP TABLE episode"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.installationFileWarnings(context.Background()); err == nil {
		t.Fatal("database failures must not become file warnings")
	}
}
