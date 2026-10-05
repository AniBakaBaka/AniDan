// SPDX-License-Identifier: AGPL-3.0-only
package containerctl

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

var repositoryPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]{0,38}/[A-Za-z0-9][A-Za-z0-9_.-]{0,99}$`)

func ValidRepository(s string) bool { return s == "" || repositoryPattern.MatchString(s) }

type Release struct {
	Version     string  `json:"version"`
	Changelog   string  `json:"changelog"`
	PublishedAt *string `json:"publishedAt"`
	ReleaseURL  *string `json:"releaseUrl"`
}
type VersionCheck struct {
	CurrentVersion  string  `json:"currentVersion"`
	LatestVersion   *string `json:"latestVersion"`
	HasUpdate       bool    `json:"hasUpdate"`
	ReleaseURL      *string `json:"releaseUrl"`
	Changelog       *string `json:"changelog"`
	PublishedAt     *string `json:"publishedAt"`
	Configured      bool    `json:"configured"`
	ComparisonKnown bool    `json:"comparisonKnown"`
	Message         string  `json:"message,omitempty"`
}
type githubRelease struct {
	Tag         string  `json:"tag_name"`
	Body        string  `json:"body"`
	PublishedAt *string `json:"published_at"`
	URL         string  `json:"html_url"`
	Draft       bool    `json:"draft"`
	Prerelease  bool    `json:"prerelease"`
}
type releaseCache struct {
	Body    []byte
	Expires time.Time
}
type Releases struct {
	repository string
	client     *http.Client
	now        func() time.Time
	mu         sync.Mutex
	cache      map[string]releaseCache
}

func NewReleases(repository string, client *http.Client) (*Releases, error) {
	if !ValidRepository(repository) {
		return nil, errors.New("release repository must be empty or an exact GitHub owner/repository")
	}
	if client == nil {
		client = &http.Client{Transport: &http.Transport{Proxy: nil, ResponseHeaderTimeout: 5 * time.Second, IdleConnTimeout: 30 * time.Second, MaxIdleConns: 4}}
	}
	clone := *client
	clone.Timeout = 10 * time.Second
	clone.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &Releases{repository: repository, client: &clone, now: time.Now, cache: map[string]releaseCache{}}, nil
}
func (r *Releases) Configured() bool { return r != nil && r.repository != "" }
func (r *Releases) fetch(ctx context.Context, suffix string, force bool) ([]byte, error) {
	if !r.Configured() {
		return nil, errors.New("release repository is not configured")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if c, ok := r.cache[suffix]; ok && !force && c.Expires.After(r.now()) {
		return append([]byte(nil), c.Body...), nil
	}
	req, err := http.NewRequestWithContext(ctx, "GET", "https://api.github.com/repos/"+r.repository+"/releases"+suffix, nil)
	if err != nil {
		return nil, errors.New("invalid release request")
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "AniDan-release-check")
	response, err := r.client.Do(req)
	if err != nil {
		return nil, errors.New("GitHub release request failed")
	}
	defer response.Body.Close()
	if response.StatusCode == 404 {
		return nil, errors.New("configured GitHub repository or release was not found")
	}
	if response.StatusCode != 200 {
		return nil, fmt.Errorf("GitHub release API returned HTTP %d", response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, (4<<20)+1))
	if err != nil || len(body) > 4<<20 {
		return nil, errors.New("GitHub release response could not be read or exceeded limit")
	}
	if !json.Valid(body) {
		return nil, errors.New("invalid GitHub release response")
	}
	r.cache[suffix] = releaseCache{append([]byte(nil), body...), r.now().Add(30 * time.Minute)}
	return body, nil
}
func (r *Releases) normalize(v githubRelease) Release {
	var link *string
	u, err := url.Parse(v.URL)
	// Return only a release page under the explicitly configured repository.
	if err == nil && u.Scheme == "https" && u.Host == "github.com" && u.User == nil && u.RawQuery == "" && path.Clean(u.Path) == u.Path && strings.HasPrefix(u.Path, "/"+r.repository+"/releases/") {
		s := u.String()
		link = &s
	}
	return Release{Version: strings.TrimPrefix(v.Tag, "v"), Changelog: v.Body, PublishedAt: v.PublishedAt, ReleaseURL: link}
}
func (r *Releases) Check(ctx context.Context, current string, force bool) (VersionCheck, error) {
	out := VersionCheck{CurrentVersion: current, Configured: r.Configured()}
	if !r.Configured() {
		out.Message = "Release repository is not configured; no request was made"
		return out, nil
	}
	body, err := r.fetch(ctx, "/latest", force)
	if err != nil {
		return out, err
	}
	var v githubRelease
	if json.Unmarshal(body, &v) != nil || v.Tag == "" || v.Draft || v.Prerelease {
		return out, errors.New("invalid published release response")
	}
	release := r.normalize(v)
	out.LatestVersion = &release.Version
	out.Changelog = &release.Changelog
	out.ReleaseURL = release.ReleaseURL
	out.PublishedAt = release.PublishedAt
	out.HasUpdate, out.ComparisonKnown = newer(current, release.Version)
	if !out.ComparisonKnown {
		out.Message = "Version tags could not be compared as semantic versions"
	}
	return out, nil
}
func (r *Releases) List(ctx context.Context, limit int, force bool) ([]Release, error) {
	if limit < 1 || limit > 50 {
		return nil, errors.New("limit must be 1..50")
	}
	out := []Release{}
	if !r.Configured() {
		return out, nil
	}
	body, err := r.fetch(ctx, "?per_page="+strconv.Itoa(limit), force)
	if err != nil {
		return nil, err
	}
	var raw []githubRelease
	if json.Unmarshal(body, &raw) != nil {
		return nil, errors.New("invalid GitHub release list")
	}
	if len(raw) > 50 {
		return nil, errors.New("GitHub returned too many releases")
	}
	for _, v := range raw {
		if v.Draft || v.Tag == "" {
			continue
		}
		out = append(out, r.normalize(v))
		if len(out) == limit {
			break
		}
	}
	return out, nil
}

var semverPattern = regexp.MustCompile(`^v?(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(?:-([0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*))?(?:\+[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*)?$`)

func numericIdentifier(s string) bool {
	if s == "" {
		return false
	}
	for _, ch := range s {
		if ch < '0' || ch > '9' {
			return false
		}
	}
	return true
}
func compareNumeric(a, b string) int {
	if len(a) < len(b) {
		return -1
	}
	if len(a) > len(b) {
		return 1
	}
	return strings.Compare(a, b)
}
func newer(current, latest string) (bool, bool) {
	a, b := semverPattern.FindStringSubmatch(current), semverPattern.FindStringSubmatch(latest)
	if a == nil || b == nil {
		return false, false
	}
	for _, v := range []string{a[4], b[4]} {
		for _, part := range strings.Split(v, ".") {
			if numericIdentifier(part) && len(part) > 1 && part[0] == '0' {
				return false, false
			}
		}
	}
	for i := 1; i <= 3; i++ {
		if cmp := compareNumeric(a[i], b[i]); cmp != 0 {
			return cmp < 0, true
		}
	}
	if a[4] == b[4] {
		return false, true
	}
	if a[4] == "" {
		return false, true
	}
	if b[4] == "" {
		return true, true
	}
	ap, bp := strings.Split(a[4], "."), strings.Split(b[4], ".")
	for i := 0; i < len(ap) && i < len(bp); i++ {
		if ap[i] == bp[i] {
			continue
		}
		an, bn := numericIdentifier(ap[i]), numericIdentifier(bp[i])
		if an && bn {
			return compareNumeric(ap[i], bp[i]) < 0, true
		}
		if an {
			return true, true
		}
		if bn {
			return false, true
		}
		return bp[i] > ap[i], true
	}
	return len(bp) > len(ap), true
}
