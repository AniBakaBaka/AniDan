// SPDX-License-Identifier: AGPL-3.0-only
package logrotate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPreserveLegacyArchiveOutsideRetention(t *testing.T) {
	dir := t.TempDir()
	legacy := strings.Repeat("original log\n", 10)
	if err := os.WriteFile(filepath.Join(dir, "app.log.5"), []byte(legacy), 0600); err != nil {
		t.Fatal(err)
	}
	for run := 0; run < 2; run++ {
		w, err := Open(dir, Options{MaxBytes: 8, Backups: 3})
		if err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 10; i++ {
			if _, err := w.Write([]byte("record\n")); err != nil {
				w.Close()
				t.Fatal(err)
			}
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
	}
	got, err := os.ReadFile(filepath.Join(dir, "app.log.5"))
	if err != nil || string(got) != legacy {
		t.Fatal("old archive was changed by rotation")
	}
	for _, name := range []string{"app.log", "app.log.1", "app.log.2", "app.log.3"} {
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil || info.Size() > 8 {
			t.Fatalf("managed log bound was not preserved: %s", name)
		}
	}
}

func TestManagedOversizeLogStillPreservedAndRejected(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log")
	if err := os.WriteFile(path, []byte("original oversized log"), 0600); err != nil {
		t.Fatal(err)
	}
	if w, err := Open(dir, Options{MaxBytes: 8, Backups: 3}); err == nil {
		w.Close()
		t.Fatal("oversized managed log was accepted")
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != "original oversized log" {
		t.Fatal("rejected log was modified")
	}
}
