// SPDX-License-Identifier: AGPL-3.0-only
package server

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/AniBakaBaka/AniDan/internal/migrate"
)

const webMigrationDetectedMaxBytes int64 = 8 << 30
const webMigrationDetectedTimeout = 10 * time.Minute
const webMigrationDetectedCandidates = 16

// This projection intentionally excludes users, credentials, config contents
// and credential-derived hashes. It describes a selected capture, not a live
// connection or proof of the original process's effective environment.
type webMigrationSourceDisplay struct {
	SettingsSource  string    `json:"settingsSource"`
	ConfigDirectory string    `json:"configDirectory"`
	SourceDriver    string    `json:"sourceDriver"`
	Host            string    `json:"host"`
	Port            int       `json:"port"`
	Database        string    `json:"database"`
	Schema          string    `json:"schema,omitempty"`
	CapturedAt      time.Time `json:"capturedAt,omitzero"`
}

type webMigrationCandidate struct {
	ID             string    `json:"id"`
	RootID         string    `json:"rootId"`
	Label          string    `json:"label"`
	Available      bool      `json:"available"`
	Reason         string    `json:"reason"`
	Warnings       []string  `json:"warnings"`
	SelectionToken string    `json:"selectionToken,omitempty"`
	ExpiresAt      time.Time `json:"expiresAt,omitempty"`
	webMigrationSourceDisplay
}

type webMigrationDetectedCandidate struct {
	view       webMigrationCandidate
	root       webMigrationRoot
	configRel  string
	composeRel string
	configDir  string
	config     []byte
	compose    []byte
	settings   migrate.LegacyInstallation
}

func webMigrationDetectionReason(rootReason string, roots int) string {
	if rootReason != "" {
		return rootReason
	}
	if roots > 32 {
		return "Automatic detection accepts at most 32 readable roots. Manual snapshot migration remains available."
	}
	if roots == 0 {
		return "Mount the original installation/config directory under a configured readable root, separate from the current AniDan data directory."
	}
	return ""
}

// No connection is made here. Only exact documented names at each allowed root
// and its direct child directories are considered, with shared work limits.
func (s *Server) webMigrationDiscover(ctx context.Context) ([]webMigrationDetectedCandidate, []string) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out := []webMigrationDetectedCandidate{}
	diagnostics := []string{}
	// Every return, including scan/candidate limits, must satisfy the public
	// response bound. Keep the latest limit reason visible when the list is full.
	diagnose := func(message string) {
		if len(diagnostics) < 32 {
			diagnostics = append(diagnostics, message)
		} else {
			diagnostics[len(diagnostics)-1] = message
		}
	}
	roots := s.webMigrationRoots()
	if len(roots) > 32 {
		return out, []string{"Too many configured readable roots; select at most 32 roots for automatic detection."}
	}
	seen := map[string]int{}
	directories := 0
	for _, root := range roots {
		bases := []string{"."}
		// Bound directory enumeration without recursively walking an old library.
		d, err := os.Open(root.Path)
		if err != nil {
			diagnose("A configured readable root could not be inspected.")
			continue
		}
		entries, readErr := d.ReadDir(129)
		d.Close()
		if readErr != nil && readErr != io.EOF {
			diagnose("A readable root directory listing failed.")
			continue
		}
		if len(entries) > 128 {
			diagnose("A readable root has more than 128 entries; only the root itself was checked. Mount the intended installation directly.")
		} else {
			for _, entry := range entries {
				if !utf8.ValidString(entry.Name()) {
					diagnose("A source directory name is not valid UTF-8 and was not inspected.")
					continue
				}
				if entry.IsDir() && entry.Type()&os.ModeSymlink == 0 && !strings.HasPrefix(entry.Name(), ".") {
					bases = append(bases, entry.Name())
				}
			}
		}
		sort.Strings(bases)
		for _, base := range bases {
			if ctx.Err() != nil || directories >= 128 || len(out) >= webMigrationDetectedCandidates {
				diagnose("Automatic detection reached its bounded scan limit. Mount the intended installation directly and detect again.")
				return out, diagnostics
			}
			directories++
			actual := filepath.Join(root.Path, base)
			if _, err := webMigrationCanonicalDirectory(actual); err != nil {
				continue
			}
			compose := filepath.Join(base, "docker-compose.yml")
			_, composeErr := os.Lstat(filepath.Join(root.Path, compose))
			if !os.IsNotExist(composeErr) {
				configRel := filepath.Join(base, "config", "config.yml")
				c := s.webMigrationLoadCandidate(ctx, root, configRel, compose)
				if index, exists := seen[c.configDir]; !exists {
					if len(out) >= webMigrationDetectedCandidates {
						diagnose("Automatic detection reached its candidate limit. Mount the intended installation directly.")
						return out, diagnostics
					}
					seen[c.configDir] = len(out)
					out = append(out, c)
				} else if out[index].composeRel == "" {
					out[index] = c
				}
			}
			for _, rel := range []string{filepath.Join(base, "config.yml"), filepath.Join(base, "config", "config.yml")} {
				if _, err := os.Lstat(filepath.Join(root.Path, rel)); os.IsNotExist(err) {
					continue
				}
				configDir := filepath.Dir(filepath.Join(root.Path, rel))
				if _, exists := seen[configDir]; exists {
					continue
				}
				if len(out) >= webMigrationDetectedCandidates {
					diagnose("Automatic detection reached its candidate limit. Mount the intended installation directly.")
					return out, diagnostics
				}
				seen[configDir] = len(out)
				out = append(out, s.webMigrationLoadCandidate(ctx, root, rel, ""))
				if len(out) >= webMigrationDetectedCandidates {
					break
				}
			}
		}
	}
	if len(out) == 0 {
		diagnose("No supported original config.yml or literal original docker-compose.yml was found at the mounted roots or their direct installation directories. config.example.yml is never used.")
	}
	return out, diagnostics
}

func (s *Server) webMigrationLoadCandidate(ctx context.Context, root webMigrationRoot, configRel, composeRel string) webMigrationDetectedCandidate {
	c := webMigrationDetectedCandidate{root: root, configRel: configRel, composeRel: composeRel, configDir: filepath.Dir(filepath.Join(root.Path, configRel))}
	if !utf8.ValidString(root.Path) || !utf8.ValidString(configRel) || !utf8.ValidString(composeRel) {
		c.view.Reason = "The source path cannot be represented as exact UTF-8 text."
		return c
	}
	identity, _ := json.Marshal([]string{root.ID, filepath.ToSlash(configRel), filepath.ToSlash(composeRel)})
	id := sha256.Sum256(identity)
	c.view = webMigrationCandidate{ID: hex.EncodeToString(id[:]), RootID: root.ID, Label: filepath.ToSlash(filepath.Dir(configRel)), Warnings: []string{}, webMigrationSourceDisplay: webMigrationSourceDisplay{ConfigDirectory: c.configDir, SettingsSource: "config"}}
	if composeRel != "" {
		c.view.SettingsSource = "compose"
	}
	for _, forbidden := range []string{s.DataDir, s.webMigrationBootstrap()} {
		if webMigrationWithin(forbidden, c.configDir) || webMigrationWithin(c.configDir, forbidden) {
			c.view.Reason = "The detected source overlaps this AniDan installation."
			return c
		}
	}
	if _, err := webMigrationCanonicalDirectory(c.configDir); err != nil {
		c.view.Reason = "The original config bind directory is missing, unreadable or uses a symbolic link."
		return c
	}
	var err error
	c.config, err = webMigrationReadDetected(ctx, root.Path, configRel, 1<<20)
	if err != nil && !(composeRel != "" && os.IsNotExist(err)) {
		c.view.Reason = "The original config.yml is unreadable, changed, nonregular, invalid or larger than 1 MiB."
		return c
	}
	if composeRel != "" {
		parent := filepath.Dir(composeRel)
		for _, name := range []string{"docker-compose.override.yml", "docker-compose.override.yaml", "compose.override.yml", "compose.override.yaml", "compose.yml", "compose.yaml"} {
			if _, e := os.Lstat(filepath.Join(root.Path, parent, name)); !os.IsNotExist(e) {
				c.view.Reason = "Additional Compose configuration makes effective source settings ambiguous; automatic detection cannot combine deployment overrides."
				return c
			}
		}
		c.compose, err = webMigrationReadDetected(ctx, root.Path, composeRel, 256<<10)
		if err != nil {
			c.view.Reason = "The original Compose file is unreadable, changed, nonregular or larger than 256 KiB."
			return c
		}
	}
	c.settings, err = migrate.ParseLegacyInstallation(c.config, c.compose)
	if err != nil {
		c.view.Reason = "Original settings are incomplete, ambiguous or unsupported. Provide effective original config.yml or supported literal Compose settings. Hosts must be literal DNS/IP/bare IPv6 or the original runtime's ASCII IDNA name, not URLs, connection strings or raw Unicode host text; unresolved environment values cannot be guessed."
		return c
	}
	if composeRel != "" && filepath.Clean(c.settings.ConfigMount) != "config" {
		c.view.Reason = "Only the documented ./config:/app/config bind is automatically mapped."
		return c
	}
	c.view.SourceDriver, c.view.Host, c.view.Port, c.view.Database = c.settings.Driver, c.settings.Host, c.settings.Port, c.settings.Database
	c.view.Warnings = append(c.view.Warnings, "Confirm these files describe the original application's effective environment. A static configuration cannot detect historical command-line or container overrides.", "The original database and file writers must be stopped before capture. Database and asset files do not share a distributed snapshot.", "Only documented config-directory asset paths are mapped automatically. Other external/local-scan file roots must use advanced migration with explicit mappings.")
	c.view.Available = true
	return c
}

// Anchored, nonblocking regular-file reads reject symlinks in every component,
// cap bytes, and compare the descriptor and named entry after the read.
func webMigrationReadDetected(ctx context.Context, rootPath, rel string, limit int64) ([]byte, error) {
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if !filepath.IsLocal(rel) || strings.ContainsAny(rel, "\\\x00") || limit < 1 {
		return nil, errors.New("unsafe original configuration path")
	}
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	parts := strings.Split(filepath.Clean(rel), string(filepath.Separator))
	for i := range parts {
		st, e := root.Lstat(filepath.Join(parts[:i+1]...))
		if e != nil {
			return nil, e
		}
		if st.Mode()&os.ModeSymlink != 0 || (i < len(parts)-1 && !st.IsDir()) {
			return nil, errors.New("unsafe original configuration entry")
		}
	}
	before, err := root.Lstat(rel)
	if err != nil {
		return nil, err
	}
	if !before.Mode().IsRegular() || before.Size() < 1 || before.Size() > limit {
		return nil, errors.New("invalid original configuration file")
	}
	f, err := root.OpenFile(rel, regularReadFlags, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(before, opened) {
		return nil, errors.New("original configuration changed")
	}
	var b bytes.Buffer
	n, err := backupCopy(ctx, &b, f, limit)
	if err != nil || n != before.Size() {
		return nil, errors.New("original configuration read did not complete")
	}
	after, err := f.Stat()
	if err != nil || after.Size() != before.Size() || !after.ModTime().Equal(before.ModTime()) {
		return nil, errors.New("original configuration changed")
	}
	named, err := root.Lstat(rel)
	if err != nil || !named.Mode().IsRegular() || !os.SameFile(before, named) || named.Size() != before.Size() || !named.ModTime().Equal(before.ModTime()) {
		return nil, errors.New("original configuration changed")
	}
	return b.Bytes(), nil
}

// The MAC binds private config bytes without publishing a guessable credential
// digest or putting credentials into a client-readable token claim.
func (s *Server) webMigrationCandidateMAC(actor string, c webMigrationDetectedCandidate, expiry string) (string, error) {
	key, err := s.localReviewKey()
	if err != nil {
		return "", err
	}
	h := hmac.New(sha256.New, key)
	for _, data := range [][]byte{[]byte("legacy-detection-v1"), []byte(actor), []byte(c.view.ID), []byte(expiry), c.config, c.compose} {
		h.Write([]byte(strconv.Itoa(len(data)) + ":"))
		h.Write(data)
	}
	return base64.RawURLEncoding.EncodeToString(h.Sum(nil)), nil
}
func (s *Server) webMigrationCandidateToken(actor string, c webMigrationDetectedCandidate) (string, time.Time, error) {
	expires := time.Now().UTC().Add(webMigrationTicketLifetime).Truncate(time.Second)
	e := strconv.FormatInt(expires.Unix(), 10)
	mac, err := s.webMigrationCandidateMAC(actor, c, e)
	return e + "." + mac, expires, err
}
func (s *Server) webMigrationCandidateVerify(actor string, c webMigrationDetectedCandidate, token string) bool {
	if len(token) > 128 {
		return false
	}
	p := strings.Split(token, ".")
	if len(p) != 2 {
		return false
	}
	expires, err := strconv.ParseInt(p[0], 10, 64)
	if err != nil || expires <= time.Now().Unix() || expires > time.Now().Add(webMigrationTicketLifetime).Unix()+1 || strconv.FormatInt(expires, 10) != p[0] {
		return false
	}
	mac, err := s.webMigrationCandidateMAC(actor, c, p[0])
	return err == nil && hmac.Equal([]byte(mac), []byte(p[1]))
}
func (s *Server) webMigrationDetectHTTP(w http.ResponseWriter, r *http.Request, actor string) {
	if r.URL.RawQuery != "" {
		httpError(w, 400, "Detection uses configured readable roots only.")
		return
	}
	candidates, diagnostics := s.webMigrationDiscover(r.Context())
	views := make([]webMigrationCandidate, 0, len(candidates))
	for _, c := range candidates {
		if c.view.Available {
			token, expiry, err := s.webMigrationCandidateToken(actor, c)
			if err != nil {
				httpError(w, 503, "Detection approval is unavailable.")
				return
			}
			c.view.SelectionToken, c.view.ExpiresAt = token, expiry
		}
		views = append(views, c.view)
	}
	writeJSON(w, 200, map[string]any{"candidates": views, "diagnostics": diagnostics})
}
