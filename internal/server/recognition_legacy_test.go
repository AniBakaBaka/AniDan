// SPDX-License-Identifier: AGPL-3.0-only
package server

import (
	"context"
	"testing"

	"github.com/AniBakaBaka/AniDan/internal/provider"
	"github.com/AniBakaBaka/AniDan/internal/store"
)

func TestLegacyGlobalAndSingleEpisodeLookahead(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, "sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.DB.Exec("CREATE TABLE config (config_key TEXT PRIMARY KEY, config_value TEXT)"); err != nil {
		t.Fatal(err)
	}
	s := &Server{Store: db}
	set := func(key, value string) {
		t.Helper()
		if err := s.setSetting(ctx, key, value); err != nil {
			t.Fatal(err)
		}
	}
	// Tencent episode-shaped fixtures, not a live provider fetch.
	episodes := []provider.Episode{{ID: "main", Title: "完美世界 第268集", Index: 268}, {ID: "preview", Title: "完美世界 预告", Index: 269}}
	set("globalEpisodeTitleFilterEnabled", "true")
	set("globalEpisodeTitleFilterRegex", `^(?!.*第[0-9]+集).*$`)
	got, err := s.filterEpisodes(ctx, episodes, "完美世界", "tencent", "fixture", nil)
	if err != nil || len(got) != 1 || got[0].ID != "main" {
		t.Fatalf("global filter: %v, %v", got, err)
	}
	set("globalEpisodeTitleFilterEnabled", "false")
	set("singleEpisodeFilterRules", `完美世界 => {[rules=^(?!.*第[0-9]+集).*$;provider=tencent]}`)
	got, err = s.filterEpisodes(ctx, episodes, "完美世界", "tencent", "fixture", nil)
	if err != nil || len(got) != 1 || got[0].ID != "main" {
		t.Fatalf("single filter: %v, %v", got, err)
	}
	got, err = s.filterEpisodes(ctx, episodes, "完美世界", "bilibili", "fixture", nil)
	if err != nil || len(got) != 2 {
		t.Fatal("provider-scoped filter leaked")
	}
}
