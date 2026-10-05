// SPDX-License-Identifier: AGPL-3.0-only
package server

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/AniBakaBaka/AniDan/internal/integration"
	"github.com/AniBakaBaka/AniDan/internal/job"
	"github.com/AniBakaBaka/AniDan/internal/provider"
	"github.com/AniBakaBaka/AniDan/internal/recognition"
)

func (s *Server) registerImportControl(m *http.ServeMux) {
	m.HandleFunc("POST /api/ui/validate-url", s.operator(s.importValidateURL))
	m.HandleFunc("POST /api/ui/import-from-url", s.operator(s.importFromURL))
	if e := s.Jobs.Register("control_auto_import", s.runControlAuto); e != nil {
		panic(e)
	}
	m.HandleFunc("POST /api/control/import/auto", s.operator(s.controlSubmitAuto))
	m.HandleFunc("POST /api/control/import/xml", s.operator(s.controlSingleImport))
	m.HandleFunc("POST /api/control/import/url", s.operator(s.controlSingleImport))
	m.HandleFunc("GET /api/control/metadata/search", s.operator(s.controlMetadataSearch))
	m.HandleFunc("PUT /api/control/scrapers/{provider}", s.operator(s.controlScraperPut))
}

func (s *Server) controlSingleImport(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Source     int64  `json:"sourceId"`
		Index      *int64 `json:"episode_index"`
		CamelIndex *int64 `json:"episodeIndex"`
		Content    string `json:"content"`
		URL        string `json:"url"`
		Title      string `json:"title"`
	}
	if e := readJSON(r, &in); e != nil {
		httpError(w, 422, e.Error())
		return
	}
	if in.Index == nil {
		in.Index = in.CamelIndex
	}
	if in.Source <= 0 || in.Index == nil || *in.Index <= 0 {
		httpError(w, 422, "sourceId and positive episode_index required")
		return
	}
	row, e := s.Store.Get(r.Context(), "anime_sources", in.Source)
	if e != nil {
		libWriteError(w, e)
		return
	}
	item := libImportItem{Title: in.Title, Index: *in.Index}
	if strings.HasSuffix(r.URL.Path, "/xml") {
		if str(row["provider_name"]) != "custom" {
			httpError(w, 400, "XML imports require a custom source")
			return
		}
		if strings.TrimSpace(in.Content) == "" {
			httpError(w, 422, "content required")
			return
		}
		// Parse before enqueue so malformed requests cannot appear successfully imported.
		if _, e = libParseCustom(in.Content); e != nil {
			httpError(w, 422, e.Error())
			return
		}
		item.ContentRef, e = s.spoolImportPayload(r.Context(), []byte(in.Content))
		if e != nil {
			httpError(w, 507, e.Error())
			return
		}
	} else {
		name, _, _, err := provider.ResolveURL(in.URL)
		if err != nil {
			httpError(w, 422, err.Error())
			return
		}
		if name != str(row["provider_name"]) {
			httpError(w, 400, "URL provider does not match source")
			return
		}
		if _, ok := s.Providers.Get(name); !ok {
			httpError(w, 404, "Provider unavailable")
			return
		}
		item.URL = in.URL
		item.URLProvider = name
	}
	s.libSubmit(w, r, "library_import", "导入分集", libTaskParams{Source: in.Source, Items: []libImportItem{item}})
}

func controlMetadataType(p, typ string) string {
	if p == "tmdb" {
		if typ == "tv_series" {
			return "tv"
		}
		if typ == "" {
			return "multi"
		}
	}
	if p == "tvdb" {
		if typ == "tv_series" {
			return "series"
		}
		if typ == "movie" {
			return "movies"
		}
	}
	return typ
}
func (s *Server) controlMetadataSearch(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	p, key, id, typ := q.Get("provider"), strings.TrimSpace(q.Get("keyword")), strings.TrimSpace(q.Get("id")), q.Get("mediaType")
	if p == "" || (key == "") == (id == "") {
		httpError(w, 400, "provider and exactly one of keyword or id required")
		return
	}
	if typ != "" && typ != "tv_series" && typ != "movie" {
		httpError(w, 422, "Invalid mediaType")
		return
	}
	cred, e := s.metadataCredential(r.Context(), p, s.metadataUserID(r))
	if e != nil {
		metadataError(w, e)
		return
	}
	results := []integration.Metadata{}
	if id != "" {
		v, err := s.Metadata.Details(r.Context(), p, id, controlMetadataType(p, typ), cred)
		e = err
		if v != nil {
			results = append(results, *v)
		}
	} else {
		results, e = s.Metadata.Search(r.Context(), p, key, controlMetadataType(p, typ), cred)
	}
	if e != nil {
		metadataError(w, e)
		return
	}
	if results == nil {
		results = []integration.Metadata{}
	}
	writeJSON(w, 200, map[string]any{"results": results})
}

func (s *Server) controlScraperPut(w http.ResponseWriter, r *http.Request) {
	s.providerConfigMu.Lock()
	defer s.providerConfigMu.Unlock()
	name := r.PathValue("provider")
	if _, ok := s.Providers.Get(name); !ok {
		httpError(w, 404, "Provider unavailable")
		return
	}
	if _, e := s.Store.Get(r.Context(), "scrapers", name); e != nil {
		libWriteError(w, e)
		return
	}
	var in struct {
		Proxy     *bool   `json:"useProxy"`
		Blacklist *string `json:"episodeBlacklistRegex"`
		Log       *bool   `json:"logRawResponses"`
		Timeout   *int    `json:"searchTimeout"`
	}
	if e := readJSON(r, &in); e != nil {
		httpError(w, 422, e.Error())
		return
	}
	settings, values := map[string]string{}, map[string]string{}
	if in.Blacklist != nil {
		if *in.Blacklist != "" {
			if _, e := regexp.Compile(*in.Blacklist); e != nil {
				httpError(w, 422, e.Error())
				return
			}
		}
		settings[name+"_episode_blacklist_regex"] = *in.Blacklist
	}
	if in.Timeout != nil {
		if *in.Timeout < 1 || *in.Timeout > 120 {
			httpError(w, 422, "searchTimeout must be 1..120")
			return
		}
		v := strconv.Itoa(*in.Timeout)
		values["timeoutSeconds"] = v
		settings["scraper_"+name+"_search_timeout"] = v
		settings[sourceConfigKey(name, "timeoutSeconds")] = v
	}
	if in.Log != nil {
		v := strconv.FormatBool(*in.Log)
		values["logRawResponses"] = v
		settings["scraper_"+name+"_log_responses"] = v
		settings[sourceConfigKey(name, "logRawResponses")] = v
	}
	current, e := s.Store.Get(r.Context(), "scrapers", name)
	if e != nil {
		httpError(w, 500, "Provider settings could not be read")
		return
	}
	useProxy := boolean(current["use_proxy"])
	if in.Proxy != nil {
		useProxy = *in.Proxy
	}
	route := s.selectedProxyRouting(useProxy)
	{
		if e := s.Providers.ValidateConfigurationWithRouting(name, values, route); e != nil {
			httpError(w, 422, e.Error())
			return
		}
	}
	tx, e := s.Store.DB.BeginTx(r.Context(), nil)
	if e != nil {
		httpError(w, 500, e.Error())
		return
	}
	defer tx.Rollback()
	for k, v := range settings {
		if e = s.metadataConfigTx(r.Context(), tx, k, v); e != nil {
			httpError(w, 500, e.Error())
			return
		}
	}
	if in.Proxy != nil {
		if _, e = tx.ExecContext(r.Context(), s.Store.Rebind("UPDATE scrapers SET use_proxy=? WHERE provider_name=?"), *in.Proxy, name); e != nil {
			httpError(w, 500, e.Error())
			return
		}
	}
	if e = tx.Commit(); e != nil {
		httpError(w, 500, e.Error())
		return
	}
	{
		if e = s.Providers.ConfigureWithRouting(name, values, route); e != nil {
			httpError(w, 500, e.Error())
			return
		}
	}
	writeJSON(w, 200, map[string]any{"status": "success", "message": "弹幕源配置已更新"})
}

// Auto requests are durable descriptions; source discovery happens in the worker.
type controlAutoParams struct {
	SearchType string `json:"searchType"`
	Term       string `json:"searchTerm"`
	Season     *int   `json:"season"`
	Episodes   string `json:"episode"`
	Type       string `json:"mediaType"`
}

func controlEpisodeIndices(raw string) (map[int]bool, error) {
	out := map[int]bool{}
	if raw == "" {
		return out, nil
	}
	if len(raw) > 4096 {
		return nil, errors.New("episode expression too long")
	}
	for _, part := range strings.Split(raw, ",") {
		a, b, rangeMode := strings.Cut(strings.TrimSpace(part), "-")
		lo, e := strconv.Atoi(a)
		if e != nil || lo < 1 {
			return nil, errors.New("invalid episode number")
		}
		hi := lo
		if rangeMode {
			hi, e = strconv.Atoi(b)
			if e != nil || hi < lo {
				return nil, errors.New("invalid episode range")
			}
		}
		if hi-lo > 9999 {
			return nil, errors.New("episode range exceeds 10000")
		}
		for n := lo; n <= hi; n++ {
			out[n] = true
			if len(out) > 10000 {
				return nil, errors.New("episode selection exceeds 10000")
			}
			if n == hi {
				break
			}
		}
	}
	return out, nil
}
func (s *Server) controlSubmitAuto(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	in := controlAutoParams{SearchType: q.Get("searchType"), Term: strings.TrimSpace(q.Get("searchTerm")), Episodes: q.Get("episode"), Type: q.Get("mediaType")}
	switch in.SearchType {
	case "keyword", "tmdb", "tvdb", "douban", "imdb", "bangumi":
	default:
		httpError(w, 422, "Invalid searchType")
		return
	}
	if in.Term == "" || len(in.Term) > 2048 {
		httpError(w, 422, "searchTerm required (max 2048)")
		return
	}
	if v := q.Get("season"); v != "" {
		n, e := strconv.Atoi(v)
		if e != nil || n < 0 {
			httpError(w, 422, "Invalid season")
			return
		}
		in.Season = &n
	}
	if in.Episodes != "" && in.Season == nil {
		httpError(w, 422, "episode requires season")
		return
	}
	if _, e := controlEpisodeIndices(in.Episodes); e != nil {
		httpError(w, 422, e.Error())
		return
	}
	if in.Type != "" && in.Type != "tv_series" && in.Type != "movie" {
		httpError(w, 422, "Invalid mediaType")
		return
	}
	if in.SearchType == "keyword" && in.Type == "" {
		httpError(w, 400, "keyword search requires mediaType")
		return
	}
	if in.Type == "" {
		in.Type = "movie"
		if in.Season != nil {
			in.Type = "tv_series"
		}
	}
	b, _ := json.Marshal(in)
	sum := sha256.Sum256(b)
	id, e := s.Jobs.SubmitWithOptions("control_auto_import", in, nil, job.SubmitOptions{Title: "自动导入: " + in.Term, UniqueKey: fmt.Sprintf("auto:%x", sum)})
	if e != nil {
		httpError(w, 409, e.Error())
		return
	}
	writeJSON(w, 202, map[string]any{"status": "success", "message": "自动导入任务已提交", "taskId": id})
}

func (s *Server) runControlAuto(ctx context.Context, raw json.RawMessage, progress func(int, string)) (any, error) {
	var in controlAutoParams
	if e := unmarshalExactJSON(raw, &in); e != nil {
		return nil, e
	}
	keyword := in.Term
	var meta *integration.Metadata
	var year *int
	if in.SearchType != "keyword" {
		row, e := s.metadataEnsure(ctx, in.SearchType)
		if e != nil {
			return nil, e
		}
		if !boolean(row["is_enabled"]) {
			return nil, errors.New("metadata provider disabled")
		}
		cred, e := s.metadataCredential(ctx, in.SearchType, 0)
		if e != nil {
			return nil, e
		}
		meta, e = s.Metadata.Details(ctx, in.SearchType, in.Term, controlMetadataType(in.SearchType, in.Type), cred)
		if e != nil {
			return nil, e
		}
		if meta == nil || strings.TrimSpace(meta.Title) == "" {
			return nil, errors.New("metadata title not found")
		}
		keyword = meta.Title
		if meta.Year > 0 {
			year = &meta.Year
		}
	}
	// Explicit episode expressions override any episode suffix embedded in the title.
	if in.Episodes != "" {
		keyword = recognition.ParseSearchKeyword(keyword).Title
	}
	progress(10, "搜索匹配弹幕源")
	req, candidates, e := s.selectImportCandidate(ctx, keyword, in.Season, nil, year, in.Type)
	if e != nil {
		return job.DiagnosticResult{"candidates": candidates}, e
	}
	preview, rules, _, e := s.prepareStorage(ctx, req)
	if e != nil {
		return nil, e
	}
	if in.Season != nil && preview.Season == req.Season {
		req.Season = *in.Season
	}
	if in.Type != "" {
		req.Type = in.Type
	}
	if meta != nil {
		if req.Metadata == nil {
			req.Metadata = map[string]string{}
		}
		req.Metadata[in.SearchType+"Id"] = in.Term
		req.Aliases = metadataAliasFields(*meta)
		req.ImageURL = meta.ImageURL
		if meta.Year > 0 {
			req.Year = &meta.Year
		}
	}
	selected, e := controlEpisodeIndices(in.Episodes)
	if e != nil {
		return nil, e
	}
	if len(selected) > 0 && in.Type != "movie" {
		req.CurrentEpisodeIndex = nil
		preview, rules, _, e = s.prepareStorage(ctx, req)
		if e != nil {
			return nil, e
		}
		episodes, _, err := s.recognitionEpisodes(ctx, req.Provider, req.MediaID, req.Title, []string{preview.Title})
		if err != nil {
			return nil, err
		}
		seen := map[int]bool{}
		for _, ep := range episodes {
			out := rules.Postprocess(recognition.Input{Text: req.Title, Provider: req.Provider, Season: recognition.Int(preview.Season), Episode: recognition.Int(ep.Index)})
			target := ep.Index
			if out.Episode != nil {
				target = *out.Episode
			}
			if selected[target] {
				if seen[target] {
					return nil, errors.New("multiple source episodes map to the requested storage index")
				}
				seen[target] = true
				req.Episodes = append(req.Episodes, ImportEpisode{ID: ep.ID, Title: ep.Title, Index: ep.Index, URL: ep.URL})
			}
		}
		for target := range seen {
			delete(selected, target)
		}
		if len(selected) > 0 {
			return nil, errors.New("requested episodes are not available from selected source")
		}
	}
	progress(90, "提交下载导入任务")
	if e = job.Checkpoint(ctx); e != nil {
		return nil, e
	}
	encoded, _ := json.Marshal(req)
	sum := sha256.Sum256(encoded)
	id, e := s.Jobs.SubmitChild(ctx, "generic_import", req, nil, job.SubmitOptions{Title: "导入: " + req.Title, UniqueKey: fmt.Sprintf("import:%x", sum)})
	if e != nil {
		return nil, e
	}
	return map[string]any{"executionTaskId": id, "provider": req.Provider, "mediaId": req.MediaID}, nil
}

type importURLParams struct {
	URL              string `json:"url"`
	Provider         string `json:"provider"`
	Title            string `json:"title"`
	Type             string `json:"media_type"`
	Season           *int   `json:"season"`
	Mode             string `json:"import_mode"`
	CollectionSeason string `json:"collection_season_id"`
	CollectionMid    string `json:"collection_mid"`
}

func (s *Server) importValidateURL(w http.ResponseWriter, r *http.Request) {
	var in struct {
		URL string `json:"url"`
	}
	if e := readJSON(r, &in); e != nil {
		httpError(w, 422, e.Error())
		return
	}
	info, e := s.Providers.ResolveMedia(r.Context(), strings.TrimSpace(in.URL))
	if e != nil {
		writeJSON(w, 200, map[string]any{"isValid": false, "errorMessage": e.Error()})
		return
	}
	var year any
	if info.Year > 0 {
		year = info.Year
	}
	var collection *provider.CollectionInfo
	var collectionError any
	if pr, ok := s.Providers.Get(info.Provider); ok {
		if resolver, ok := pr.(interface {
			CollectionMetadata(context.Context, string) (*provider.CollectionInfo, error)
		}); ok {
			var err error
			collection, err = resolver.CollectionMetadata(r.Context(), strings.TrimSpace(in.URL))
			if err != nil {
				collection = nil
				collectionError = "Collection discovery failed; ordinary URL import remains available"
			}
		}
	}
	writeJSON(w, 200, map[string]any{"isValid": true, "provider": info.Provider, "mediaId": info.ID, "title": info.Title, "imageUrl": info.ImageURL, "mediaType": info.Type, "year": year, "episodeIndex": nil, "collection": collection, "collectionError": collectionError, "errorMessage": nil, "importScope": providerURLImportScope(strings.TrimSpace(in.URL))})
}

// Scope describes this URL's ordinary import-from-url operation. Manual and
// batch imports into an existing source keep their explicit target indices.
func providerURLImportScope(raw string) string {
	name, media, episode, err := provider.ResolveURL(raw)
	if err != nil {
		return ""
	}
	if episode != "" || name == "bilibili" && (strings.HasPrefix(media, "ep") || strings.Contains(media, ":p")) {
		return "episode"
	}
	return "media"
}
func (s *Server) importFromURL(w http.ResponseWriter, r *http.Request) {
	var in importURLParams
	if e := readJSON(r, &in); e != nil {
		httpError(w, 422, e.Error())
		return
	}
	in.URL = strings.TrimSpace(in.URL)
	name, media, episodeID, e := provider.ResolveURL(in.URL)
	if e != nil {
		httpError(w, 422, e.Error())
		return
	}
	if in.Provider != "" && in.Provider != name {
		httpError(w, 422, "Provider does not match URL")
		return
	}
	if _, ok := s.Providers.Get(name); !ok {
		httpError(w, 404, "Provider unavailable")
		return
	}
	if err := validateCollectionSelection(in.CollectionSeason, in.CollectionMid); err != nil {
		libWriteError(w, err)
		return
	}
	if in.Mode != "collection" && (in.CollectionSeason != "" || in.CollectionMid != "") {
		httpError(w, 422, "Collection identifiers require explicit collection mode")
		return
	}
	if in.Mode != "" && in.Mode != "single" && in.Mode != "collection" {
		httpError(w, 422, "Invalid import_mode")
		return
	}
	if in.Season != nil && *in.Season < 0 {
		httpError(w, 422, "Invalid season")
		return
	}
	if in.Type != "" && in.Type != "tv_series" && in.Type != "movie" && in.Type != "other" {
		httpError(w, 422, "Invalid media_type")
		return
	}
	req := ImportRequest{Provider: name, MediaID: media, Title: strings.TrimSpace(in.Title), Type: in.Type, Season: 1}
	info, err := s.Providers.ResolveMedia(r.Context(), in.URL)
	if err == nil {
		req.MediaID = info.ID
		req.ImageURL = info.ImageURL
		if req.Title == "" {
			req.Title = info.Title
		}
		if req.Type == "" {
			req.Type = info.Type
		}
		if info.Season > 0 {
			req.Season = info.Season
		}
		if info.Year > 0 {
			req.Year = &info.Year
		}
	} else {
		var unsupported *provider.UnsupportedError
		if !errors.As(err, &unsupported) {
			httpError(w, 502, err.Error())
			return
		}
		if req.Title == "" {
			httpError(w, 422, "This provider cannot resolve media title yet; supply title explicitly")
			return
		}
	}
	if in.Season != nil {
		req.Season = *in.Season
	}
	if req.Type == "" {
		req.Type = "tv_series"
	}
	// Collection selection must resolve membership from the original URL again.
	// A supplied ID is only an expected identity, never a request destination.
	if in.Mode == "collection" {
		pr, _ := s.Providers.Get(name)
		collection, eps, err := resolveCollectionSelection(r.Context(), pr, in.URL, in.CollectionSeason, in.CollectionMid)
		if err != nil {
			libWriteError(w, err)
			return
		}
		req.MediaID = collection.MediaID()
		if strings.TrimSpace(in.Title) == "" {
			req.Title = collection.Title
		}
		for _, ep := range eps {
			req.Episodes = append(req.Episodes, ImportEpisode{ID: ep.ID, Title: ep.Title, Index: ep.Index, URL: ep.URL})
		}
		req.Type = "tv_series"
	} else if episodeID != "" || (name == "bilibili" && (strings.HasPrefix(media, "ep") || strings.Contains(media, ":p"))) || req.MediaID == "" {
		ep, err := s.Providers.ResolveEpisode(r.Context(), in.URL)
		if err != nil {
			httpError(w, 422, err.Error())
			return
		}
		if req.MediaID == "" {
			req.MediaID = ep.ID
		}
		// Metadata may resolve a video-only URL to its true series identity.
		// Check membership against that verified identity, never the old empty
		// parsed media ID or a fabricated first-episode index.
		if episodeID != "" && req.MediaID != "" {
			p, _ := s.Providers.Get(name)
			eps, lookupErr := p.Episodes(r.Context(), req.MediaID)
			if lookupErr != nil {
				httpError(w, 502, lookupErr.Error())
				return
			}
			found := false
			for _, candidate := range eps {
				if candidate.ID == ep.ID {
					if found {
						httpError(w, 422, "URL episode has ambiguous indices in the media listing")
						return
					}
					ep = candidate
					found = true
				}
			}
			if !found {
				httpError(w, 422, "URL episode is absent from verified media listing")
				return
			}
		}
		req.Episodes = []ImportEpisode{{ID: ep.ID, Title: ep.Title, Index: ep.Index, URL: ep.URL}}
	}
	if req.MediaID == "" || req.Title == "" {
		httpError(w, 422, "URL lacks resolvable media identity")
		return
	}
	b, _ := json.Marshal(req)
	sum := sha256.Sum256(b)
	id, e := s.Jobs.SubmitWithOptions("generic_import", req, nil, job.SubmitOptions{Title: "URL导入: " + req.Title, UniqueKey: fmt.Sprintf("url-import:%x", sum)})
	if e != nil {
		httpError(w, 409, e.Error())
		return
	}
	writeJSON(w, 202, map[string]any{"message": "URL导入任务已提交", "taskId": id})
}
