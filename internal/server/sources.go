// SPDX-License-Identifier: AGPL-3.0-only
package server

import (
	"context"
	"fmt"
	"github.com/AniBakaBaka/AniDan/internal/provider"
	"github.com/AniBakaBaka/AniDan/internal/recognition"
	"github.com/AniBakaBaka/AniDan/internal/store"
	"net/http"
	"strings"
)

const commonEpisodeBlacklist = `^(.*?)((.+?版)|(特(别|典))|((导|演)员|嘉宾|角色)访谈|福利|彩蛋|花絮|预告|特辑|专访|访谈|幕后|周边|资讯|看点|速看|回顾|盘点|合集|PV|MV|CM|OST|ED|OP|BD|特典|SP|NCOP|NCED|MENU|Web-DL|rip|x264|x265|aac|flac)(.*?)$`

func defaultEpisodeBlacklist(name string) string {
	if name == "tencent" {
		return provider.TencentEpisodeBlacklistDefault
	}
	return ""
}

func sourceConfigKey(name, key string) string {
	if key == name+"Cookie" || strings.HasPrefix(key, "dandanplay_") {
		return key
	}
	return "anidan.source." + name + "." + key
}
func (s *Server) loadProviderConfig(ctx context.Context) error {
	s.providerConfigMu.Lock()
	defer s.providerConfigMu.Unlock()
	return s.loadProviderConfigUnlocked(ctx)
}
func (s *Server) loadProviderConfigUnlocked(ctx context.Context) (resultErr error) {
	defer func() {
		if resultErr != nil {
			s.proxyRouting.Store(&proxyRoutingState{mode: "accelerate", blocked: "activation_failed"})
		}
	}()
	if e := s.reloadProxyRouting(ctx); e != nil {
		return e
	}
	for _, name := range s.Providers.Names() {
		fields, e := s.Providers.ConfigSchema(name)
		if e != nil {
			continue
		}
		values := map[string]string{}
		for _, f := range fields {
			v := s.setting(ctx, sourceConfigKey(name, f.Key), "")
			if v == "" && f.Key == "userAgent" {
				v = s.setting(ctx, name+"UserAgent", "")
			}
			if v == "" && f.Key == "logRawResponses" {
				v = s.setting(ctx, "scraper_"+name+"_log_responses", "false")
			}
			if v == "" && f.Key == "timeoutSeconds" {
				v = s.setting(ctx, "scraper_"+name+"_search_timeout", "")
			}
			if v != "" {
				values[f.Key] = v
			}
		}
		row, err := s.Store.Get(ctx, "scrapers", name)
		route := s.selectedProxyRouting(err == nil && boolean(row["use_proxy"]))
		delete(values, "proxyURL")
		{
			if e = s.Providers.ConfigureWithRouting(name, values, route); e != nil {
				return e
			}
		}
	}
	return nil
}
func (s *Server) registerSources(m *http.ServeMux) {
	m.HandleFunc("GET /api/ui/scrapers/{providerName}/config", s.operator(s.sourceConfigGet))
	m.HandleFunc("PUT /api/ui/scrapers/{providerName}/config", s.operator(s.sourceConfigPut))
	m.HandleFunc("GET /api/ui/scrapers/common-blacklist", s.operator(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"commonBlacklist": commonEpisodeBlacklist})
	}))
	m.HandleFunc("GET /api/ui/scrapers/{providerName}/default-blacklist", s.operator(func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("providerName")
		writeJSON(w, 200, map[string]any{"providerName": name, "defaultBlacklist": defaultEpisodeBlacklist(name)})
	}))
	m.HandleFunc("GET /api/control/scrapers", s.operator(s.scrapersList))
	m.HandleFunc("PUT /api/control/scrapers", s.operator(s.scrapersUpdate))
}
func (s *Server) sourceConfigGet(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("providerName")
	cfg, e := s.Providers.Configuration(name)
	if e != nil {
		httpError(w, 404, e.Error())
		return
	}
	out := map[string]any{}
	for k, v := range cfg {
		if k == "proxyURL" {
			continue
		}
		if name == "dandanplay" {
			k = camel(k)
		}
		if k == "logRawResponses" {
			out[k] = v == "true"
		} else {
			out[k] = v
		}
	}
	row, e := s.Store.Get(r.Context(), "scrapers", name)
	if e == nil {
		out["useProxy"] = boolean(row["use_proxy"])
	}
	out[camel("scraper_"+name+"_log_responses")] = cfg["logRawResponses"] == "true"
	out[name+"EpisodeBlacklistRegex"] = s.setting(r.Context(), name+"_episode_blacklist_regex", defaultEpisodeBlacklist(name))
	out["scraper_"+name+"_search_timeout"] = cfg["timeoutSeconds"]
	writeJSON(w, 200, out)
}
func (s *Server) sourceConfigPut(w http.ResponseWriter, r *http.Request) {
	s.providerConfigMu.Lock()
	defer s.providerConfigMu.Unlock()
	name := r.PathValue("providerName")
	fields, e := s.Providers.ConfigSchema(name)
	if e != nil {
		httpError(w, 404, e.Error())
		return
	}
	var in map[string]any
	if e = readJSON(r, &in); e != nil {
		httpError(w, 400, e.Error())
		return
	}
	values := map[string]string{}
	persist := map[string]string{}
	allowed := map[string]bool{}
	for _, f := range fields {
		if f.Key == "proxyURL" {
			continue
		}
		allowed[f.Key] = true
		allowed[camel(f.Key)] = true
	}
	for _, alias := range []string{"scraper_" + name + "_log_responses", camel("scraper_" + name + "_log_responses")} {
		if v, ok := in[alias]; ok {
			if previous, exists := in["logRawResponses"]; exists && str(previous) != str(v) {
				httpError(w, 422, "Conflicting response logging settings")
				return
			}
			in["logRawResponses"] = v
			delete(in, alias)
		}
	}
	for k, v := range in {
		if k == "useProxy" {
			continue
		}
		if k == name+"EpisodeBlacklistRegex" || k == name+"_episode_blacklist_regex" {
			pattern := str(v)
			if pattern != "" {
				if _, e = recognition.CompileRegexCase(pattern, false); e != nil {
					httpError(w, 400, e.Error())
					return
				}
			}
			persist[name+"_episode_blacklist_regex"] = pattern
			continue
		}
		if k == "scraper_"+name+"_search_timeout" {
			k = "timeoutSeconds"
		}
		if !allowed[k] {
			httpError(w, 422, "Unsupported provider configuration field: "+k)
			return
		}
		key := k
		for _, f := range fields {
			if camel(f.Key) == k {
				key = f.Key
			}
		}
		if key == "logRawResponses" && str(v) != "true" && str(v) != "false" {
			httpError(w, 400, "logRawResponses must be true or false")
			return
		}
		if str(v) == "********" {
			continue
		}
		values[key] = str(v)
		persist[sourceConfigKey(name, key)] = str(v)
		if key == "logRawResponses" {
			persist["scraper_"+name+"_log_responses"] = str(v)
		}
	}
	row, err := s.Store.Get(r.Context(), "scrapers", name)
	if err != nil {
		httpError(w, 404, "Provider settings not found")
		return
	}
	useProxy := boolean(row["use_proxy"])
	if value, ok := in["useProxy"]; ok {
		useProxy = boolean(value)
	}
	route := s.selectedProxyRouting(useProxy)
	// Configuration and routing are validated on one detached snapshot.
	if e = s.Providers.ValidateConfigurationWithRouting(name, values, route); e != nil {
		httpError(w, 400, e.Error())
		return
	}
	tx, e := s.Store.DB.BeginTx(r.Context(), nil)
	if e != nil {
		httpError(w, 500, e.Error())
		return
	}
	defer tx.Rollback()
	for k, v := range persist {
		res, err := tx.ExecContext(r.Context(), s.Store.Rebind("UPDATE config SET config_value=? WHERE config_key=?"), v, k)
		if err != nil {
			httpError(w, 500, err.Error())
			return
		}
		n, err := res.RowsAffected()
		if err != nil {
			httpError(w, 500, err.Error())
			return
		}
		if n == 0 {
			var exists int
			err = tx.QueryRowContext(r.Context(), s.Store.Rebind("SELECT COUNT(*) FROM config WHERE config_key=?"), k).Scan(&exists)
			if err != nil {
				httpError(w, 500, err.Error())
				return
			}
			if exists == 0 {
				if _, err = s.Store.InsertTx(r.Context(), tx, "config", store.Row{"config_key": k, "config_value": v}); err != nil {
					httpError(w, 500, err.Error())
					return
				}
			}
		}
	}
	if _, ok := in["useProxy"]; ok {
		if _, e = tx.ExecContext(r.Context(), s.Store.Rebind("UPDATE scrapers SET use_proxy=? WHERE provider_name=?"), boolean(in["useProxy"]), name); e != nil {
			httpError(w, 500, e.Error())
			return
		}
	}
	if e = tx.Commit(); e != nil {
		httpError(w, 500, e.Error())
		return
	}
	if e = s.Providers.ConfigureWithRouting(name, values, route); e != nil {
		httpError(w, 500, e.Error())
		return
	}
	w.WriteHeader(204)
}
func (s *Server) sourceEpisodes(ctx context.Context, name, media string) ([]provider.Episode, []map[string]any, error) {
	return s.sourceEpisodesWithCache(ctx, name, media, false)
}
func (s *Server) sourceEpisodesWithCache(ctx context.Context, name, media string, preview bool) ([]provider.Episode, []map[string]any, error) {
	p, ok := s.Providers.Get(name)
	if !ok {
		return nil, nil, &provider.UnsupportedError{Provider: name, Operation: "episodes", Reason: "adapter unavailable"}
	}
	var eps []provider.Episode
	var e error
	if preview {
		eps, e = s.cachedProviderEpisodes(ctx, p, media)
	} else if call := mediaListingCallFromContext(ctx); call != nil {
		eps, e = s.loadMediaListing(ctx, call, p, name, media)
	} else {
		eps, e = p.Episodes(ctx, media)
	}
	if e != nil {
		return nil, nil, e
	}
	patterns := make([]*recognition.Regex, 2)
	for i, key := range []string{name + "_episode_blacklist_regex", "globalEpisodeTitleFilter"} {
		fallback := ""
		if i == 0 {
			fallback = defaultEpisodeBlacklist(name)
		}
		if raw := s.setting(ctx, key, fallback); raw != "" {
			re, e := recognition.CompileRegexCase(raw, name == "tencent" && i == 0)
			if e != nil {
				return nil, nil, fmt.Errorf("%s: %w", key, e)
			}
			patterns[i] = re
		}
	}
	out := append([]provider.Episode{}, eps...)
	excluded := []map[string]any{}
	for stage, re := range patterns {
		kept := out[:0]
		for _, ep := range out {
			if err := ctx.Err(); err != nil {
				return nil, nil, err
			}
			matched := false
			if re != nil {
				var err error
				matched, err = re.MatchString(ep.Title)
				if err != nil {
					return nil, nil, err
				}
			}
			if matched {
				excluded = append(excluded, map[string]any{"provider": name, "episodeId": ep.ID, "title": ep.Title, "episodeIndex": ep.Index, "url": ep.URL, "filterReason": "分集标题黑名单"})
			} else {
				kept = append(kept, ep)
			}
		}
		out = kept
		if name == "tencent" && stage == 0 {
			// Misaka numbers Tencent episodes after its provider blacklist, then
			// formats numeric titles. Global/single-series filters run afterwards
			// and must retain these indices. Never mutate a cached raw listing.
			for i := range out {
				out[i].Index = i + 1
				if title := strings.TrimSpace(out[i].Title); title != "" && strings.Trim(title, "0123456789") == "" {
					out[i].Title = "第" + title + "集"
				}
			}
		}
	}
	return out, excluded, nil
}
