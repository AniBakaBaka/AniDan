// SPDX-License-Identifier: AGPL-3.0-only
package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/AniBakaBaka/AniDan/internal/provider"
	"github.com/AniBakaBaka/AniDan/internal/store"
)

type tencentFixtureTransport struct{}

// Exercise the real Tencent RPC decoder with deliberately unordered tabs,
// duplicates, trailers and extras. No Tencent network requests are made.
func (tencentFixtureTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	var request struct {
		Params map[string]string `json:"page_params"`
	}
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		return nil, err
	}
	var module map[string]any
	if strings.HasPrefix(request.Params["page_context"], "cid=") {
		module = map[string]any{"module_params": map[string]any{"tabs": `[{"page_context":"later"},{"page_context":"earlier"}]`}}
	} else {
		items := []any{}
		add := func(id, title, trailer string) {
			items = append(items, map[string]any{"item_params": map[string]any{"vid": id, "union_title": title, "is_trailer": trailer}})
		}
		if request.Params["page_context"] == "later" {
			add("vid3", "第03集", "0")
			add("extra", "幕后 REACTION", "0")
			add("vid2", "02", "0")
		} else {
			add("vid1", "01", "0")
			add("vid3", "第03集", "0")
			add("trailer", "第04集", "1")
		}
		module = map[string]any{"item_data_lists": map[string]any{"item_datas": items}}
	}
	body, err := json.Marshal(map[string]any{"ret": 0, "data": map[string]any{"module_list_datas": []any{map[string]any{"module_datas": []any{module}}}}})
	if err != nil {
		return nil, err
	}
	return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(string(body))), Request: r}, nil
}

func tencentLegacyFixture(t *testing.T) *Server {
	t.Helper()
	db, err := store.Open(context.Background(), "sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	for _, statement := range []string{
		`CREATE TABLE config (config_key TEXT PRIMARY KEY, config_value TEXT)`,
		`CREATE TABLE anime_sources (id INTEGER PRIMARY KEY, anime_id INTEGER, provider_name TEXT, media_id TEXT, source_order INTEGER)`,
		`CREATE TABLE episode (id INTEGER PRIMARY KEY, source_id INTEGER, episode_index INTEGER, provider_episode_id TEXT, title TEXT, source_url TEXT, danmaku_file_path TEXT, comment_count INTEGER)`,
		`INSERT INTO anime_sources VALUES (7, 1, 'tencent', 'cover', 1)`,
		`INSERT INTO episode VALUES (101, 7, 1, 'vid1', '旧标题一', 'old-url-1', 'old-1.xml', 100)`,
		`INSERT INTO episode VALUES (103, 7, 2, 'vid3', '旧标题三', 'old-url-3', 'old-3.xml', 300)`,
	} {
		if _, err := db.DB.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	p := provider.NewLegacy("tencent", &http.Client{Transport: tencentFixtureTransport{}})
	registry := provider.NewRegistry(1, p)
	t.Cleanup(func() { registry.CloseContext(context.Background()) })
	return &Server{Store: db, Providers: registry}
}

func TestTencentLegacyOrderingFilteringAndBinding(t *testing.T) {
	s := tencentLegacyFixture(t)
	ctx := context.Background()
	// Provider blacklist acts on raw titles, before numeric-title formatting.
	if err := s.setSetting(ctx, "tencent_episode_blacklist_regex", `^02$|reaction`); err != nil {
		t.Fatal(err)
	}
	if err := s.setSetting(ctx, "globalEpisodeTitleFilterEnabled", "true"); err != nil {
		t.Fatal(err)
	}
	if err := s.setSetting(ctx, "globalEpisodeTitleFilterRegex", `^(?!第(?:01|03)集$).*`); err != nil {
		t.Fatal(err)
	}
	src, err := s.Store.Get(ctx, "anime_sources", 7)
	if err != nil {
		t.Fatal(err)
	}
	before, err := s.captureWorkflowEpisodes(ctx, src)
	if err != nil {
		t.Fatal(err)
	}
	eps, excluded, err := s.sourceEpisodes(ctx, "tencent", "cover")
	if err != nil || len(excluded) != 2 || len(eps) != 2 {
		t.Fatalf("listing: %v; excluded=%v; err=%v", eps, excluded, err)
	}
	eps, err = s.filterEpisodes(ctx, eps, "完美世界", "tencent", "cover", nil)
	if err != nil || len(eps) != 2 {
		t.Fatalf("global lookahead must see formatted titles: %v, %v", eps, err)
	}
	for i, ep := range eps {
		wantID := []string{"vid1", "vid3"}[i]
		if ep.Index != i+1 || ep.ID != wantID {
			t.Fatalf("legacy mapping: got %+v", eps)
		}
		old := before[int64(ep.Index)]
		got, err := s.prepareWorkflowEpisodeExpected(ctx, src, old, int64(ep.Index), ep.Title, ep.URL, ep.ID, true)
		if err != nil || !reflect.DeepEqual(got, old) {
			t.Fatalf("legacy record was rejected or rewritten: %v, %v", got, err)
		}
	}
	// A real conflict remains an error, including the ordinary admission path.
	for _, old := range []store.Row{nil, before[2]} {
		_, err := s.prepareWorkflowEpisodeExpected(ctx, src, old, 2, "wrong", "wrong", "another-video", true)
		if !errors.Is(err, errWorkflowEpisodeConflict) || !strings.Contains(err.Error(), "第 2 集") {
			t.Fatalf("real video conflict was not diagnosed: %v", err)
		}
	}
	after, err := s.captureWorkflowEpisodes(ctx, src)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("binding changed legacy IDs, paths or comments: %v, %v", after, err)
	}
	// Concurrent changes after capture must still invalidate the snapshot.
	if _, err := s.Store.DB.Exec(`UPDATE episode SET provider_episode_id='changed' WHERE id=103`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.prepareWorkflowEpisodeExpected(ctx, src, before[2], 2, "", "", "vid3", true); err == nil {
		t.Fatal("stale provider binding accepted")
	}
}

func TestTencentBlacklistStagesAndDefaults(t *testing.T) {
	for _, tc := range []struct {
		name, pattern, global string
		setPattern            bool
		indices               []int
		ids                   []string
	}{
		{name: "missing uses original default", indices: []int{1, 2, 3}, ids: []string{"vid1", "vid2", "vid3"}},
		{name: "explicit empty disables default", setPattern: true, indices: []int{1, 2, 3, 4}, ids: []string{"vid1", "vid2", "vid3", "extra"}},
		{name: "global exclusion retains provider indices", setPattern: true, pattern: `^02$|reaction`, global: `第01集`, indices: []int{2}, ids: []string{"vid3"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := tencentLegacyFixture(t)
			ctx := context.Background()
			if tc.setPattern {
				if err := s.setSetting(ctx, "tencent_episode_blacklist_regex", tc.pattern); err != nil {
					t.Fatal(err)
				}
			}
			if err := s.setSetting(ctx, "globalEpisodeTitleFilter", tc.global); err != nil {
				t.Fatal(err)
			}
			eps, _, err := s.sourceEpisodes(ctx, "tencent", "cover")
			if err != nil {
				t.Fatal(err)
			}
			indices, ids := []int{}, []string{}
			for _, ep := range eps {
				indices = append(indices, ep.Index)
				ids = append(ids, ep.ID)
			}
			if !reflect.DeepEqual(indices, tc.indices) || !reflect.DeepEqual(ids, tc.ids) {
				t.Fatalf("got %v %v", indices, ids)
			}
		})
	}
}

type sharedEpisodeListing struct {
	provider.Provider
	name     string
	episodes []provider.Episode
}

func (p sharedEpisodeListing) Name() string { return p.name }
func (p sharedEpisodeListing) Episodes(context.Context, string) ([]provider.Episode, error) {
	return p.episodes, nil
}

func TestSourceFilteringPreservesRawListingAndOtherProviderIndices(t *testing.T) {
	for _, name := range []string{"tencent", "bilibili"} {
		t.Run(name, func(t *testing.T) {
			s := tencentLegacyFixture(t)
			ctx := context.Background()
			raw := []provider.Episode{{ID: "skip", Title: "skip", Index: 5}, {ID: "keep", Title: "06", Index: 6}}
			before := append([]provider.Episode{}, raw...)
			s.Providers.Register(sharedEpisodeListing{name: name, episodes: raw})
			if err := s.setSetting(ctx, name+"_episode_blacklist_regex", "skip"); err != nil {
				t.Fatal(err)
			}
			eps, _, err := s.sourceEpisodes(ctx, name, "fixture")
			if err != nil || len(eps) != 1 {
				t.Fatalf("%v, %v", eps, err)
			}
			wantIndex, wantTitle := 6, "06"
			if name == "tencent" {
				wantIndex, wantTitle = 1, "第06集"
			}
			if eps[0].Index != wantIndex || eps[0].Title != wantTitle {
				t.Fatalf("wrong mapping: %+v", eps)
			}
			if !reflect.DeepEqual(raw, before) {
				t.Fatalf("raw/cache listing mutated: %+v", raw)
			}
		})
	}
}
