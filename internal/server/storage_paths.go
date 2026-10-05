// SPDX-License-Identifier: AGPL-3.0-only
package server

import (
	"context"
	"errors"
	"fmt"
	"github.com/AniBakaBaka/AniDan/internal/store"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

var templateVariable = regexp.MustCompile(`\$\{([A-Za-z]+)(?::0([1-9])d)?\}`)
var titleSeason = regexp.MustCompile(`(?i)(?:\s*(?:第[一二三四五六七八九十百0-9]+季|season\s*[0-9]+|s[0-9]+))$`)

func safeFilename(v string) string {
	v = strings.Map(func(r rune) rune {
		if r < 32 || strings.ContainsRune(`/\<>:"|?*`, r) {
			return '_'
		}
		return r
	}, v)
	v = strings.Trim(v, " .")
	if v == "" || v == ".." {
		return "Unknown"
	}
	return v
}
func (s *Server) storageTarget(raw string) (*os.Root, string, string, error) {
	raw = s.normalizeStoredPath(raw)
	absolute, e := filepath.Abs(raw)
	if e != nil {
		return nil, "", "", e
	}
	if e = s.guardWebMigrationPath(absolute); e != nil {
		return nil, "", "", e
	}
	roots := append([]string{s.DataDir}, s.Config.WriteRoots...)
	for _, base := range roots {
		base, e = filepath.Abs(base)
		if e != nil {
			continue
		}
		rel, e := filepath.Rel(base, absolute)
		if e != nil || !filepath.IsLocal(rel) {
			continue
		}
		root, e := os.OpenRoot(base)
		if e != nil {
			return nil, "", "", e
		}
		return root, rel, absolute, nil
	}
	return nil, "", "", errors.New("storage target must stay under DataDir or an explicit ANIDAN_WRITE_ROOTS directory")
}
func (s *Server) renderStorageTemplate(ctx context.Context, template string, anime, src, ep store.Row) (string, error) {
	allowed := map[string]string{"title": safeFilename(str(anime["title"])), "titleBase": safeFilename(titleSeason.ReplaceAllString(str(anime["title"]), "")), "season": str(anime["season"]), "episode": str(ep["episode_index"]), "year": str(anime["year"]), "provider": safeFilename(str(src["provider_name"])), "animeId": str(anime["id"]), "episodeId": str(ep["id"]), "sourceId": str(src["id"]), "tmdbId": ""}
	meta, e := s.Store.List(ctx, "anime_metadata", store.Row{"anime_id": anime["id"]}, 1, 0)
	if e != nil {
		return "", e
	}
	if len(meta) > 0 {
		allowed["tmdbId"] = safeFilename(str(meta[0]["tmdb_id"]))
	}
	var invalid error
	rendered := templateVariable.ReplaceAllStringFunc(template, func(token string) string {
		m := templateVariable.FindStringSubmatch(token)
		v, ok := allowed[m[1]]
		if !ok {
			invalid = fmt.Errorf("unknown template variable %s", m[1])
			return ""
		}
		if m[2] != "" {
			width, _ := strconv.Atoi(m[2])
			n, e := strconv.ParseInt(v, 10, 64)
			if e != nil {
				invalid = fmt.Errorf("cannot format %s as integer", m[1])
				return ""
			}
			return fmt.Sprintf("%0*d", width, n)
		}
		return v
	})
	if invalid != nil {
		return "", invalid
	}
	if strings.Contains(rendered, "${") {
		return "", errors.New("invalid template expression")
	}
	if !strings.HasSuffix(strings.ToLower(rendered), ".xml") {
		rendered += ".xml"
	}
	return rendered, nil
}
func (s *Server) storageTemplate(ctx context.Context, typ, custom string) (string, string, error) {
	base := s.setting(ctx, "tvDanmakuDirectoryPath", filepath.Join(s.DataDir, "danmaku"))
	template := "${animeId}/${episodeId}"
	switch typ {
	case "id", "":
	case "tv":
		template = "${title}/Season ${season:02d}/${title} - S${season:02d}E${episode:02d}"
	case "movie":
		base = s.setting(ctx, "movieDanmakuDirectoryPath", filepath.Join(s.DataDir, "danmaku"))
		template = "${title}/${title}"
	case "plex":
		template = "${title}/${title} - S${season:02d}E${episode:02d}"
	case "emby":
		template = "${title}/${title} S${season:02d}/${title} S${season:02d}E${episode:02d}"
	case "titleBase":
		template = "${titleBase}/Season ${season:02d}/${titleBase} - S${season:02d}E${episode:02d}"
	case "custom_tv":
		template = s.setting(ctx, "tvDanmakuFilenameTemplate", template)
	case "custom_movie":
		base = s.setting(ctx, "movieDanmakuDirectoryPath", filepath.Join(s.DataDir, "danmaku", "movies"))
		template = s.setting(ctx, "movieDanmakuFilenameTemplate", template)
	case "custom":
		if custom == "" {
			return "", "", errors.New("customTemplate required")
		}
		template = custom
	default:
		return "", "", errors.New("unknown templateType")
	}
	return base, template, nil
}

type storageRequest struct {
	AnimeIDs                                         []int64 `json:"animeIds"`
	TargetPath                                       string  `json:"targetPath"`
	KeepStructure                                    *bool   `json:"keepStructure"`
	Conflict                                         string  `json:"conflictAction"`
	Mode, Prefix, Suffix, RegexPattern, RegexReplace string
	Direct                                           []struct {
		EpisodeID int64  `json:"episodeId"`
		NewName   string `json:"newName"`
	} `json:"directRenames"`
	TemplateType   string `json:"templateType"`
	CustomTemplate string `json:"customTemplate"`
}
type storagePlan struct {
	Episode store.Row
	NewPath string
	Preview map[string]any
}

func (s *Server) registerStoragePaths(m *http.ServeMux) {
	m.HandleFunc("GET /api/ui/danmaku-storage/template-variables", s.operator(s.templateVariables))
	for _, op := range []string{"preview-migrate", "batch-migrate", "preview-rename", "batch-rename", "preview-template", "apply-template"} {
		m.HandleFunc("POST /api/ui/danmaku-storage/"+op, s.operator(s.storageOperation))
	}
	m.HandleFunc("GET /api/ui/config/customDanmakuPath", s.storageConfigGet)
	m.HandleFunc("PUT /api/ui/config/customDanmakuPath", s.operator(s.storageConfigPut))
}
func (s *Server) templateVariables(w http.ResponseWriter, r *http.Request) {
	vars := []map[string]any{}
	for _, v := range []string{"title", "titleBase", "season", "season:02d", "episode", "episode:02d", "episode:03d", "year", "provider", "animeId", "episodeId", "sourceId", "tmdbId"} {
		vars = append(vars, map[string]any{"name": "${" + v + "}", "desc": v, "example": "1"})
	}
	writeJSON(w, 200, vars)
}
func (s *Server) storagePlans(ctx context.Context, in storageRequest, operation string) ([]storagePlan, error) {
	if len(in.AnimeIDs) == 0 || len(in.AnimeIDs) > 10000 {
		return nil, errors.New("animeIds must contain1..10000 identifiers")
	}
	var re *regexp.Regexp
	if strings.Contains(operation, "rename") && in.Mode == "regex" {
		var e error
		re, e = regexp.Compile(in.RegexPattern)
		if e != nil {
			return nil, e
		}
	}
	plans := []storagePlan{}
	seen := map[int64]bool{}
	for _, aid := range in.AnimeIDs {
		if seen[aid] {
			continue
		}
		seen[aid] = true
		anime, e := s.Store.Get(ctx, "anime", aid)
		if e != nil {
			return nil, e
		}
		sources, e := s.Store.List(ctx, "anime_sources", store.Row{"anime_id": aid}, 1000, 0)
		if e != nil {
			return nil, e
		}
		for _, src := range sources {
			eps, e := s.Store.List(ctx, "episode", store.Row{"source_id": src["id"]}, 100000, 0)
			if e != nil {
				return nil, e
			}
			for _, ep := range eps {
				old := str(ep["danmaku_file_path"])
				if old == "" {
					continue
				}
				target := ""
				if strings.Contains(operation, "migrate") {
					if in.TargetPath == "" {
						return nil, errors.New("targetPath required")
					}
					relative := filepath.Base(old)
					if in.KeepStructure == nil || *in.KeepStructure {
						normal := old
						if strings.HasPrefix(normal, "/app/config/") {
							normal = filepath.Join(s.DataDir, strings.TrimPrefix(normal, "/app/config/"))
						}
						if r, e := filepath.Rel(filepath.Join(s.DataDir, "danmaku"), normal); e == nil && filepath.IsLocal(r) {
							relative = r
						}
					}
					target = filepath.Join(in.TargetPath, relative)
				} else if strings.Contains(operation, "rename") {
					name := filepath.Base(old)
					stem := strings.TrimSuffix(name, filepath.Ext(name))
					switch in.Mode {
					case "prefix":
						name = in.Prefix + stem + in.Suffix + filepath.Ext(name)
					case "regex":
						replacement := regexp.MustCompile(`\\([0-9]+)`).ReplaceAllString(in.RegexReplace, `$${$1}`)
						name = re.ReplaceAllString(stem, replacement) + filepath.Ext(name)
					case "direct":
						for _, d := range in.Direct {
							if d.EpisodeID == number(ep["id"]) {
								name = d.NewName
								break
							}
						}
					default:
						return nil, errors.New("mode must be prefix, regex or direct")
					}
					if filepath.Base(name) != name || name == "." || name == ".." {
						return nil, errors.New("new filename cannot contain paths")
					}
					target = filepath.Join(filepath.Dir(old), name)
				} else {
					base, template, e := s.storageTemplate(ctx, in.TemplateType, in.CustomTemplate)
					if e != nil {
						return nil, e
					}
					name, e := s.renderStorageTemplate(ctx, template, anime, src, ep)
					if e != nil {
						return nil, e
					}
					target = filepath.Join(base, name)
				}
				root, _, absolute, e := s.storageTarget(target)
				if e != nil {
					return nil, e
				}
				root.Close()
				exists := false
				if f, e := s.resolveDanmaku(old); e == nil {
					exists = true
					f.Close()
				}
				plan := storagePlan{ep, absolute, map[string]any{"animeId": aid, "animeTitle": anime["title"], "episodeId": ep["id"], "episodeIndex": ep["episode_index"], "oldPath": old, "newPath": absolute, "exists": exists}}
				plans = append(plans, plan)
				if len(plans) > 100000 {
					return nil, errors.New("storage batch too large")
				}
			}
		}
	}
	return plans, nil
}
func (s *Server) storageOperation(w http.ResponseWriter, r *http.Request) {
	var in storageRequest
	if e := readJSON(r, &in); e != nil {
		httpError(w, 400, e.Error())
		return
	}
	op := strings.TrimPrefix(r.URL.Path, "/api/ui/danmaku-storage/")
	plans, e := s.storagePlans(r.Context(), in, op)
	if e != nil {
		httpError(w, 400, e.Error())
		return
	}
	if strings.HasPrefix(op, "preview-") {
		out := []map[string]any{}
		for _, p := range plans {
			out = append(out, p.Preview)
		}
		writeJSON(w, 200, map[string]any{"totalCount": len(out), "previewItems": out})
		return
	}
	conflict := in.Conflict
	if conflict == "" {
		if op == "batch-migrate" {
			conflict = "skip"
		} else {
			conflict = "rename"
		}
	}
	if conflict != "skip" && conflict != "rename" && conflict != "overwrite" {
		httpError(w, 400, "invalid conflictAction")
		return
	}
	details := []map[string]any{}
	success, failed, skipped := 0, 0, 0
	s.fileMu.Lock()
	defer s.fileMu.Unlock()
	for _, p := range plans {
		status, path, e := s.copyStorageObject(r.Context(), p, conflict)
		d := map[string]any{"episodeId": p.Episode["id"], "status": status}
		if e != nil {
			d["reason"] = e.Error()
			failed++
		} else if status == "skipped" {
			skipped++
		} else {
			success++
			d["newPath"] = path
		}
		details = append(details, d)
	}
	writeJSON(w, 200, map[string]any{"success": failed == 0, "totalCount": len(plans), "successCount": success, "failedCount": failed, "skippedCount": skipped, "details": details, "sourceFilesRetained": true})
}
func (s *Server) copyStorageObject(ctx context.Context, p storagePlan, conflict string) (string, string, error) {
	if e := ctx.Err(); e != nil {
		return "failed", "", e
	}
	old := str(p.Episode["danmaku_file_path"])
	if old == p.NewPath {
		return "skipped", old, nil
	}
	src, e := s.resolveDanmaku(old)
	if e != nil {
		return "failed", "", e
	}
	defer src.Close()
	before, e := src.Stat()
	if e != nil {
		return "failed", "", e
	}
	root, rel, absolute, e := s.storageTarget(p.NewPath)
	if e != nil {
		return "failed", "", e
	}
	defer root.Close()
	if e = root.MkdirAll(filepath.Dir(rel), 0700); e != nil {
		return "failed", "", e
	}
	if _, e = root.Lstat(rel); e == nil {
		switch conflict {
		case "skip":
			return "skipped", absolute, nil
		case "overwrite":
			return "failed", "", errors.New("destination exists; destructive overwrite is refused, choose rename or an empty target")
		case "rename":
			base, ext := strings.TrimSuffix(rel, filepath.Ext(rel)), filepath.Ext(rel)
			for i := 1; ; i++ {
				rel = fmt.Sprintf("%s_%d%s", base, i, ext)
				if _, e = root.Lstat(rel); os.IsNotExist(e) {
					break
				}
				if e != nil {
					return "failed", "", e
				}
				if i > 10000 {
					return "failed", "", errors.New("too many filename conflicts")
				}
			}
			absolute = filepath.Join(root.Name(), rel)
		}
	} else if !os.IsNotExist(e) {
		return "failed", "", e
	}
	tmp := filepath.Join(filepath.Dir(rel), ".migrate-"+randomID()+".tmp")
	dst, e := root.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if e != nil {
		return "failed", "", e
	}
	defer root.Remove(tmp)
	n, e := io.Copy(dst, contextReader{ctx, io.LimitReader(src, 256<<20+1)})
	if e != nil {
		dst.Close()
		return "failed", "", e
	}
	if n > 256<<20 {
		dst.Close()
		return "failed", "", errors.New("file exceeds256MiB operation bound")
	}
	after, e := src.Stat()
	if e != nil || after.Size() != before.Size() || !after.ModTime().Equal(before.ModTime()) {
		dst.Close()
		return "failed", "", errors.New("source changed during copy")
	}
	if e = dst.Sync(); e != nil {
		dst.Close()
		return "failed", "", e
	}
	if e = dst.Close(); e != nil {
		return "failed", "", e
	}
	if e = root.Link(tmp, rel); e != nil {
		return "failed", "", e
	}
	published, _ := root.Stat(rel)
	s.invalidateCommentFile(absolute, nil, published)
	df, e := root.Open(filepath.Dir(rel))
	if e != nil {
		return "failed", "", e
	}
	e = df.Sync()
	df.Close()
	if e != nil {
		return "failed", "", e
	}
	res, e := s.Store.DB.ExecContext(ctx, s.Store.Rebind("UPDATE episode SET danmaku_file_path=? WHERE id=? AND danmaku_file_path=?"), absolute, p.Episode["id"], old)
	if e != nil {
		return "failed", "", e
	}
	count, e := res.RowsAffected()
	if e != nil {
		return "failed", "", e
	}
	if count != 1 {
		return "failed", "", errors.New("episode changed concurrently; new copy retained for recovery")
	}
	return "success", absolute, nil
}
func (s *Server) storageConfigGet(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{"enabled": s.setting(r.Context(), "customDanmakuPathEnabled", "false"), "template": s.setting(r.Context(), "customDanmakuPathTemplate", "/app/config/danmaku/${animeId}/${episodeId}")})
}
func (s *Server) storageConfigPut(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Enabled  string `json:"enabled"`
		Template string `json:"template"`
	}
	if e := readJSON(r, &in); e != nil {
		httpError(w, 400, e.Error())
		return
	}
	if in.Enabled != "true" && in.Enabled != "false" {
		httpError(w, 400, "enabled must be true or false")
		return
	}
	if len(in.Template) > 2048 || strings.Contains(in.Template, "..") || strings.ContainsRune(in.Template, 0) {
		httpError(w, 400, "invalid storage template")
		return
	}
	if e := s.setSettingsAtomic(r.Context(), map[string]string{"customDanmakuPathEnabled": in.Enabled, "customDanmakuPathTemplate": in.Template}); e != nil {
		httpError(w, 500, e.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"enabled": in.Enabled, "template": in.Template})
}
