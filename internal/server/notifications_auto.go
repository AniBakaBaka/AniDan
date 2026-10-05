// SPDX-License-Identifier: AGPL-3.0-only
package server

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/AniBakaBaka/AniDan/internal/job"
	"github.com/AniBakaBaka/AniDan/internal/notify"
)

func (s *Server) notificationAuto(ctx context.Context, c notify.Channel, u notify.Update, v *notificationSession, a notificationAction) (notify.Message, error) {
	if !u.Admin {
		return notify.Message{}, notify.ErrForbidden
	}
	switch a.Kind {
	case "auto":
		options := []notificationOption{}
		for _, typ := range []string{"keyword", "tmdb", "tvdb", "douban", "imdb", "bangumi"} {
			options = append(options, notificationChoice(typ, "auto_search_type", typ))
		}
		return s.notificationRender(c, u, v, "Choose automatic import lookup. Metadata IDs require the corresponding enabled/configured metadata provider.", options...), nil
	case "auto_search_type":
		switch a.ID {
		case "keyword", "tmdb", "tvdb", "douban", "imdb", "bangumi":
		default:
			return notify.Message{}, notify.ErrForbidden
		}
		v.data = notificationAction{Auto: &controlAutoParams{SearchType: a.ID}}
		v.state = "auto_term"
		return s.notificationRender(c, u, v, "Send the "+a.ID+" search term or ID, or /cancel."), nil
	case "auto_media_type":
		if v.data.Auto == nil || (a.ID != "movie" && a.ID != "tv_series") {
			return notify.Message{}, notify.ErrForbidden
		}
		v.data.Auto.Type = a.ID
		if a.ID == "tv_series" {
			v.state = "auto_season"
			return s.notificationRender(c, u, v, "Send the season number (0–999; 0 is specials)."), nil
		}
		return s.notificationAutoConfirm(c, u, v), nil
	case "auto_submit":
		if !a.Confirm || a.Auto == nil {
			return notify.Message{}, notify.ErrForbidden
		}
		if a.Auto.SearchType != "keyword" {
			row, e := s.metadataEnsure(ctx, a.Auto.SearchType)
			if e != nil {
				return notify.Message{}, e
			}
			if !boolean(row["is_enabled"]) {
				return s.notificationRender(c, u, v, "That metadata provider is disabled. Enable and configure it in the authenticated UI first."), nil
			}
		}
		raw, _ := json.Marshal(a.Auto)
		sum := sha256.Sum256(raw)
		id, e := s.Jobs.SubmitWithOptions("control_auto_import", *a.Auto, nil, job.SubmitOptions{Title: "Bot automatic import: " + a.Auto.Term, UniqueKey: fmt.Sprintf("auto:%x", sum)})
		if e != nil {
			return notify.Message{}, e
		}
		return s.notificationRender(c, u, v, "Automatic import queued: "+id, notificationChoice("View task", "task", id)), nil
	}
	return notify.Message{}, notify.ErrUnsupported
}
func (s *Server) notificationAutoInput(c notify.Channel, u notify.Update, v *notificationSession, state, text string) (notify.Message, error) {
	if !u.Admin || v.data.Auto == nil {
		return notify.Message{}, notify.ErrForbidden
	}
	switch state {
	case "auto_term":
		if len(text) > 500 {
			v.state = state
			return s.notificationRender(c, u, v, "Search term is limited to 500 bytes."), nil
		}
		v.data.Auto.Term = text
		return s.notificationRender(c, u, v, "Choose the media type.", notificationChoice("TV series", "auto_media_type", "tv_series"), notificationChoice("Movie", "auto_media_type", "movie")), nil
	case "auto_season":
		n, e := strconv.Atoi(text)
		if e != nil || n < 0 || n > 999 {
			v.state = state
			return s.notificationRender(c, u, v, "Enter a season number from 0 to 999."), nil
		}
		v.data.Auto.Season = &n
		v.state = "auto_episodes"
		return s.notificationRender(c, u, v, "Send episode numbers/ranges such as 1,3-5, or all for the complete season."), nil
	case "auto_episodes":
		if strings.EqualFold(text, "all") {
			text = ""
		}
		if _, e := controlEpisodeIndices(text); e != nil {
			v.state = state
			return s.notificationRender(c, u, v, "Enter valid episode numbers/ranges such as 1,3-5, or all."), nil
		}
		v.data.Auto.Episodes = text
		return s.notificationAutoConfirm(c, u, v), nil
	}
	return notify.Message{}, notify.ErrUnsupported
}
func (s *Server) notificationAutoConfirm(c notify.Channel, u notify.Update, v *notificationSession) notify.Message {
	in := *v.data.Auto
	season := "none"
	if in.Season != nil {
		season = strconv.Itoa(*in.Season)
	}
	eps := in.Episodes
	if eps == "" {
		eps = "all"
	}
	return s.notificationConfirm(c, u, v, fmt.Sprintf("Automatically select a matching source and import?\nLookup: %s / %s\nType: %s\nSeason: %s\nEpisodes: %s", in.SearchType, notificationClip(in.Term, 150), in.Type, season, eps), notificationAction{Kind: "auto_submit", Auto: &in})
}
func (s *Server) notificationLibraryAction(ctx context.Context, c notify.Channel, u notify.Update, v *notificationSession, a notificationAction) (notify.Message, error) {
	if !u.Admin {
		return notify.Message{}, notify.ErrForbidden
	}
	id, e := strconv.ParseInt(a.ID, 10, 64)
	if e != nil || id <= 0 {
		return notify.Message{}, notify.ErrForbidden
	}
	table := "anime_sources"
	if a.Kind == "episode_delete" {
		table = "episode"
	}
	row, e := s.Store.Get(ctx, table, id)
	if e != nil {
		return notify.Message{}, e
	}
	if a.Kind == "source_actions" {
		opts := []notificationOption{}
		if s.notificationCanImport(str(row["provider_name"])) {
			opts = append(opts, notificationChoice("Refresh all source episodes", "source_refresh", a.ID), notificationChoice("Refresh new episodes only", "source_incremental", a.ID))
		}
		opts = append(opts, notificationChoice("Delete source records; retain XML", "source_delete", a.ID), notificationChoice("Back to source", "source", a.ID))
		return s.notificationRender(c, u, v, "Source management: "+str(row["provider_name"]), opts...), nil
	}
	if !a.Confirm {
		description := "Delete source and its episode database records permanently, retaining XML files"
		if a.Kind == "episode_delete" {
			description = "Delete episode database record permanently, retaining XML file"
		}
		if a.Kind == "source_refresh" {
			description = "Refresh all episodes from the provider"
		}
		if a.Kind == "source_incremental" {
			description = "Fetch and import only new provider episodes"
		}
		a.Text = notificationFingerprint(row)
		return s.notificationConfirm(c, u, v, description+"?\nID: "+a.ID, a), nil
	}
	if notificationFingerprint(row) != a.Text {
		return s.notificationRender(c, u, v, "This record changed since confirmation. Review it again."), nil
	}
	kind := "library_delete"
	params := libTaskParams{Table: table, IDs: []int64{id}, DeleteFiles: false}
	if a.Kind == "source_refresh" || a.Kind == "source_incremental" {
		if !s.notificationCanImport(str(row["provider_name"])) {
			return notify.Message{}, notify.ErrUnsupported
		}
		kind = "library_source_refresh"
		mode := "full"
		if a.Kind == "source_incremental" {
			mode = "incremental"
		}
		params = libTaskParams{Source: id, Mode: mode}
	}
	task, e := s.Jobs.SubmitRegistered(kind, params)
	if e != nil {
		return notify.Message{}, e
	}
	return s.notificationRender(c, u, v, "Library operation queued: "+task, notificationChoice("View task", "task", task)), nil
}
