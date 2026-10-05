// SPDX-License-Identifier: AGPL-3.0-only
// Poster and scan-index wire contracts adapted from Misaka 01751526f6e4154bcc8f517481d02b68cb2684a9.
package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/AniBakaBaka/AniDan/internal/media"
	"github.com/AniBakaBaka/AniDan/internal/store"
)

func (s *Server) registerCompatMediaUI(m *http.ServeMux) {
	// Index paths and cache invalidation are protected like the local scanner.
	m.HandleFunc("GET /api/ui/poster/local-image", s.operator(s.compatLocalPoster))
	m.HandleFunc("POST /api/ui/poster/download-to-local", s.operator(s.compatDownloadPosterHandler))
	m.HandleFunc("GET /api/ui/poster/fanart", s.operator(s.compatFanart))
	m.HandleFunc("GET /api/ui/local-scan/index-stats", s.operator(s.compatIndexStats))
	m.HandleFunc("GET /api/ui/local-scan/index-detail", s.operator(s.compatIndexDetail))
	m.HandleFunc("POST /api/ui/local-scan/rebuild-index", s.operator(s.compatIndexReset))
}

type compatPosterQuery struct {
	URL    string `json:"imageUrl"`
	Title  string `json:"title"`
	Season *int   `json:"season"`
	Year   *int   `json:"year"`
}

func (q compatPosterQuery) validate() error {
	if strings.TrimSpace(q.Title) == "" || len(q.Title) > 2048 || q.Season == nil {
		return errors.New("title and integer season are required")
	}
	return nil
}

func (s *Server) compatFindPoster(ctx context.Context, q compatPosterQuery) (store.Row, error) {
	where := "title = ? AND season = ?"
	args := []any{q.Title, *q.Season}
	if q.Year != nil && *q.Year != 0 {
		where += " AND year = ?"
		args = append(args, *q.Year)
	}
	row, err := s.libOne(ctx, s.Store.DB, "anime", where, args...)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return row, err
}

func (s *Server) compatLocalPoster(w http.ResponseWriter, r *http.Request) {
	q := compatPosterQuery{Title: r.URL.Query().Get("title")}
	season, err := strconv.Atoi(r.URL.Query().Get("season"))
	if err != nil {
		httpError(w, 422, "season must be an integer")
		return
	}
	q.Season = &season
	if raw := r.URL.Query().Get("year"); raw != "" {
		year, err := strconv.Atoi(raw)
		if err != nil {
			httpError(w, 422, "year must be an integer")
			return
		}
		q.Year = &year
	}
	if err := q.validate(); err != nil {
		httpError(w, 422, err.Error())
		return
	}
	row, err := s.compatFindPoster(r.Context(), q)
	if err != nil {
		httpError(w, 500, "Unable to look up poster")
		return
	}
	writeJSON(w, 200, map[string]any{"localImagePath": row["local_image_path"], "animeId": row["id"]})
}

func (s *Server) compatDownloadPoster(ctx context.Context, q compatPosterQuery, client libHTTPDoer) (map[string]any, error) {
	if err := q.validate(); err != nil {
		return nil, err
	}
	if len(q.URL) > 2048 {
		return nil, errors.New("image URL is too long")
	}
	select {
	case libPosterSlots <- struct{}{}:
		defer func() { <-libPosterSlots }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	data, ext, err := libFetchPoster(ctx, q.URL, client)
	if err != nil {
		return nil, errors.New("Poster download failed: invalid URL, unavailable remote image or image safety limits")
	}
	path, err := s.libCachePoster(data, ext)
	if err != nil {
		return nil, errors.New("Unable to cache poster")
	}
	row, err := s.compatFindPoster(ctx, q)
	if err != nil {
		return nil, errors.New("Unable to look up poster target")
	}
	if row != nil {
		if err = s.Store.Update(ctx, "anime", row["id"], store.Row{"image_url": q.URL, "local_image_path": path}); err != nil {
			return nil, errors.New("Unable to update poster target")
		}
	}
	return map[string]any{"localImagePath": path, "animeId": row["id"]}, nil
}

func (s *Server) compatDownloadPosterHandler(w http.ResponseWriter, r *http.Request) {
	var q compatPosterQuery
	if readJSON(r, &q) != nil || q.validate() != nil || q.URL == "" {
		httpError(w, 422, "imageUrl, title and integer season are required")
		return
	}
	out, err := s.compatDownloadPoster(r.Context(), q, libPosterClient())
	if err != nil {
		httpError(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, out)
}

type compatFanartPoster struct {
	URL   string  `json:"url"`
	Lang  *string `json:"lang"`
	Likes int64   `json:"likes"`
}

func compatFanartPosters(ctx context.Context, mediaType, id, key string, client libHTTPDoer) ([]compatFanartPoster, error) {
	path, field := "tv", "tvposter"
	if mediaType == "movie" {
		path, field = "movies", "movieposter"
	}
	u := "https://webservice.fanart.tv/v3/" + path + "/" + url.PathEscape(id) + "?" + url.Values{"api_key": {key}}.Encode()
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return nil, errors.New("Unable to create Fanart request")
	}
	req.Header.Set("Accept", "application/json")
	res, err := client.Do(req)
	if err != nil {
		return nil, errors.New("Fanart request failed")
	}
	defer res.Body.Close()
	if res.StatusCode == 404 {
		return []compatFanartPoster{}, nil
	}
	if res.StatusCode != 200 {
		return nil, fmt.Errorf("Fanart returned HTTP %d", res.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(res.Body, (2<<20)+1))
	if err != nil || len(data) > 2<<20 {
		return nil, errors.New("Fanart response exceeds safety limits or could not be read")
	}
	var payload map[string]json.RawMessage
	if json.Unmarshal(data, &payload) != nil {
		return nil, errors.New("Fanart returned malformed JSON")
	}
	items := []struct {
		URL   string          `json:"url"`
		Lang  *string         `json:"lang"`
		Likes json.RawMessage `json:"likes"`
	}{}
	if raw, ok := payload[field]; ok && string(raw) != "null" {
		if json.Unmarshal(raw, &items) != nil {
			return nil, errors.New("Fanart returned invalid poster data")
		}
	}
	if len(items) > 5000 {
		return nil, errors.New("Fanart poster count exceeds safety limit")
	}
	out := make([]compatFanartPoster, 0, len(items))
	for _, item := range items {
		if _, err := libPosterURL(item.URL); err != nil {
			return nil, errors.New("Fanart returned an unsafe poster URL")
		}
		likes := int64(0)
		if len(item.Likes) > 0 && string(item.Likes) != "null" {
			raw := strings.Trim(string(item.Likes), `"`)
			likes, err = strconv.ParseInt(raw, 10, 64)
			if err != nil || likes < 0 {
				return nil, errors.New("Fanart returned invalid likes")
			}
		}
		out = append(out, compatFanartPoster{item.URL, item.Lang, likes})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Likes > out[j].Likes })
	return out, nil
}

func compatFanartLookup(q url.Values) (string, string) {
	if q.Get("mediaType") == "movie" && q.Get("tmdbId") != "" {
		return "movie", q.Get("tmdbId")
	}
	id := q.Get("tvdbId")
	if id == "" {
		id = q.Get("tmdbId")
	}
	return "tv", id
}

func (s *Server) compatFanart(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	typ := q.Get("mediaType")
	if typ == "" {
		typ = "tv"
	}
	if typ != "tv" && typ != "movie" {
		httpError(w, 422, "mediaType must be tv or movie")
		return
	}
	lookupType, id := compatFanartLookup(q)
	if id == "" {
		httpError(w, 400, "tmdbId or tvdbId is required")
		return
	}
	if len(id) > 32 || strings.Trim(id, "0123456789") != "" {
		httpError(w, 422, "Provider ID must contain digits only")
		return
	}
	key := s.setting(r.Context(), "fanartApiKey", "")
	if key == "" || key == "********" {
		httpError(w, 412, "Configure your own fanartApiKey before searching Fanart.tv")
		return
	}
	client := libPosterClient()
	redirect := client.CheckRedirect
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if req.URL.Scheme != "https" || req.URL.Host != "webservice.fanart.tv" {
			return errors.New("Fanart redirect outside its fixed API host is not allowed")
		}
		return redirect(req, via)
	}
	posters, err := compatFanartPosters(r.Context(), lookupType, id, key, client)
	if err != nil {
		httpError(w, 502, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"posters": posters, "source": "fanart.tv"})
}

type compatIndexEntry struct {
	MTime float64 `json:"mtime"`
	Size  int64   `json:"size"`
}
type compatScanIndex struct {
	Files      map[string]compatIndexEntry `json:"files"`
	LastScanAt *string                     `json:"lastScanAt"`
	New        int                         `json:"lastNewCount"`
	Changed    int                         `json:"lastChangedCount"`
	Skipped    int                         `json:"lastSkippedCount"`
}

const compatIndexMaxBytes = 16 << 20
const compatIndexMaxFiles = 100000 // Store.List's hard maximum; never silently truncate.

func (s *Server) compatReadIndex(ctx context.Context) (compatScanIndex, error) {
	index := compatScanIndex{Files: map[string]compatIndexEntry{}}
	row, err := s.Store.Get(ctx, "config", "local_scan_index")
	if errors.Is(err, sql.ErrNoRows) {
		return index, nil
	}
	if err != nil {
		return index, err
	}
	raw := str(row["config_value"])
	if len(raw) > compatIndexMaxBytes {
		return index, errors.New("Scan index exceeds safety limit; rebuild the index")
	}
	if err = json.Unmarshal([]byte(raw), &index); err != nil {
		return index, errors.New("Scan index is invalid; rebuild the index")
	}
	if index.Files == nil {
		index.Files = map[string]compatIndexEntry{}
	}
	if len(index.Files) > compatIndexMaxFiles {
		return index, errors.New("Scan index exceeds file limit; rebuild the index")
	}
	return index, nil
}

// Called by localScan while importMu is held, after successfully persisting scan items.
// The scanner still parses all entries; unchanged files are not falsely counted as skipped.
func (s *Server) compatRecordScanIndex(ctx context.Context, root string) error {
	old, err := s.compatReadIndex(ctx)
	if err != nil {
		return err
	}
	count, err := s.Store.Count(ctx, "local_danmaku_items", nil)
	if err != nil {
		return err
	}
	if count > compatIndexMaxFiles {
		return errors.New("Local scan index exceeds file limit")
	}
	rows, err := s.Store.List(ctx, "local_danmaku_items", nil, compatIndexMaxFiles, 0)
	if err != nil {
		return err
	}
	next := compatScanIndex{Files: map[string]compatIndexEntry{}}
	metadataBudget := 0
	for p, entry := range old.Files {
		if !localContained(root, p) {
			next.Files[p] = entry
			metadataBudget += len(p)*2 + 100
		}
	}
	for _, row := range rows {
		if err := ctx.Err(); err != nil {
			return err
		}
		p := str(row["file_path"])
		if !filepath.IsAbs(p) || !localContained(root, p) {
			continue
		}
		st, err := os.Lstat(p)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if !st.Mode().IsRegular() {
			continue
		}
		if _, err = media.ParseLocal(root, p); err != nil {
			continue
		}
		f, err := s.localOpenAllowed(p)
		if err != nil {
			return err
		}
		st, err = f.Stat()
		f.Close()
		if err != nil {
			return err
		}
		entry := compatIndexEntry{float64(st.ModTime().UnixNano()) / 1e9, st.Size()}
		metadataBudget += len(p)*2 + 100
		if metadataBudget > compatIndexMaxBytes {
			return errors.New("Local scan index exceeds metadata size limit")
		}
		next.Files[p] = entry
		if previous, ok := old.Files[p]; !ok {
			next.New++
		} else if previous != entry {
			next.Changed++
		}
	}
	if len(next.Files) > compatIndexMaxFiles {
		return errors.New("Local scan index exceeds file limit")
	}
	now := s.now()
	next.LastScanAt = &now
	raw, err := json.Marshal(next)
	if err != nil {
		return err
	}
	if len(raw) > compatIndexMaxBytes {
		return errors.New("Local scan index exceeds metadata size limit")
	}
	return s.setSettingsAtomic(ctx, map[string]string{"local_scan_index": string(raw)})
}

func (s *Server) compatIndexStats(w http.ResponseWriter, r *http.Request) {
	index, err := s.compatReadIndex(r.Context())
	if err != nil {
		httpError(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"totalFiles": len(index.Files), "lastScanAt": index.LastScanAt, "newFiles": index.New, "changedFiles": index.Changed, "skippedFiles": index.Skipped})
}

func (s *Server) compatIndexDetail(w http.ResponseWriter, r *http.Request) {
	limit := 100
	if raw := r.URL.Query().Get("limit"); raw != "" {
		v, err := strconv.Atoi(raw)
		if err != nil || v < 1 || v > 500 {
			httpError(w, 422, "limit must be 1..500")
			return
		}
		limit = v
	}
	index, err := s.compatReadIndex(r.Context())
	if err != nil {
		httpError(w, 500, err.Error())
		return
	}
	paths := []string{}
	for p := range index.Files {
		if strings.HasPrefix(p, r.URL.Query().Get("path")) {
			paths = append(paths, p)
		}
	}
	sort.Strings(paths)
	items := []map[string]any{}
	for _, p := range paths[:min(limit, len(paths))] {
		entry := index.Files[p]
		items = append(items, map[string]any{"path": p, "mtime": entry.MTime, "size": entry.Size})
	}
	writeJSON(w, 200, map[string]any{"total": len(paths), "items": items})
}

func (s *Server) compatIndexReset(w http.ResponseWriter, r *http.Request) {
	s.importMu.Lock()
	defer s.importMu.Unlock()
	raw, _ := json.Marshal(compatScanIndex{Files: map[string]compatIndexEntry{}})
	if err := s.setSettingsAtomic(r.Context(), map[string]string{"local_scan_index": string(raw)}); err != nil {
		httpError(w, 500, "Unable to clear scan index")
		return
	}
	writeJSON(w, 200, map[string]any{"message": "ok", "detail": "索引已清除，下次扫描将全量遍历"})
}
