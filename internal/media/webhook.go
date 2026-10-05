// SPDX-License-Identifier: AGPL-3.0-only
package media

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

type WebhookEvent struct {
	Source    string `json:"webhookSource"`
	Title     string `json:"animeTitle"`
	MediaType string `json:"mediaType"`
	Season    int    `json:"season"`
	Episode   *int   `json:"currentEpisodeIndex"`
	Year      *int   `json:"year"`
	TMDBID    string `json:"tmdbId,omitempty"`
	TVDBID    string `json:"tvdbId,omitempty"`
	IMDBID    string `json:"imdbId,omitempty"`
	SeriesID  string `json:"mediaServerSeriesId,omitempty"`
	SeasonID  string `json:"mediaServerSeasonId,omitempty"`
	EpisodeID string `json:"mediaServerEpisodeId,omitempty"`
	Delete    bool   `json:"delete,omitempty"`
	ItemType  string `json:"itemType,omitempty"`
}

var embySeason = regexp.MustCompile(`(?i)S(\d+)`)
var embyEpisodes = regexp.MustCompile(`(?i)E(\d+)(?:\s*-\s*E?(\d+))?`)

func ParseWebhook(kind string, p map[string]any) ([]WebhookEvent, error) {
	e := WebhookEvent{Source: kind, Season: 1}
	one := 1
	e.Episode = &one
	switch kind {
	case "emby":
		event := text(p["Event"])
		if event != "library.new" && event != "library.deleted" {
			return nil, nil
		}
		item, _ := p["Item"].(map[string]any)
		if item == nil {
			return nil, errors.New("Emby payload requires Item")
		}
		typ := text(item["Type"])
		e.ItemType = typ
		e.Delete = event == "library.deleted"
		e.Title = text(item["Name"])
		e.SeriesID = text(item["SeriesId"])
		e.SeasonID = text(item["SeasonId"])
		e.EpisodeID = text(item["Id"])
		ids, _ := item["ProviderIds"].(map[string]any)
		e.TMDBID = text(ids["Tmdb"])
		e.TVDBID = text(ids["Tvdb"])
		e.IMDBID = text(ids["IMDB"])
		if e.IMDBID == "" {
			e.IMDBID = text(ids["Imdb"])
		}
		e.Year = intPointer(item["ProductionYear"])
		e.MediaType = "movie"
		if typ == "Episode" {
			e.MediaType = "tv_series"
			e.Title = text(item["SeriesName"])
			sn := intPointer(item["ParentIndexNumber"])
			e.Episode = intPointer(item["IndexNumber"])
			if !e.Delete && (sn == nil || e.Episode == nil) {
				return nil, errors.New("Emby episode requires season and episode")
			}
			if sn != nil {
				e.Season = *sn
			}
		} else if typ == "Series" || typ == "Season" {
			e.MediaType = "tv_series"
			if e.Delete {
				if typ == "Series" {
					e.SeriesID = e.EpisodeID
				} else {
					e.SeasonID = e.EpisodeID
				}
				break
			}
			sm := embySeason.FindStringSubmatch(text(p["Description"]))
			em := embyEpisodes.FindStringSubmatch(text(p["Description"]))
			if sm == nil || em == nil {
				return nil, errors.New("aggregate Emby event requires Sxx Exx[-Exx] description")
			}
			e.Season, _ = strconv.Atoi(sm[1])
			start, _ := strconv.Atoi(em[1])
			end := start
			if em[2] != "" {
				end, _ = strconv.Atoi(em[2])
			}
			if end < start || end-start > 1000 {
				return nil, errors.New("Emby episode range is invalid or too large")
			}
			e.SeriesID = e.EpisodeID
			e.EpisodeID = ""
			out := []WebhookEvent{}
			for i := start; i <= end; i++ {
				copy := e
				n := i
				copy.Episode = &n
				out = append(out, copy)
			}
			return out, nil
		} else if typ != "Movie" {
			return nil, nil
		}
		if typ == "Movie" {
			e.SeriesID = e.EpisodeID
		}
	case "jellyfin":
		event := text(p["NotificationType"])
		if event != "ItemAdded" && event != "ItemRemoved" {
			return nil, nil
		}
		e.Delete = event == "ItemRemoved"
		typ := text(p["ItemType"])
		e.ItemType = typ
		e.Title = text(p["Name"])
		e.EpisodeID = text(p["ItemId"])
		e.SeriesID = text(p["SeriesId"])
		e.SeasonID = text(p["SeasonId"])
		e.MediaType = "movie"
		e.TMDBID = text(p["Provider_tmdb"])
		e.TVDBID = text(p["Provider_tvdb"])
		e.IMDBID = text(p["Provider_imdb"])
		if date := text(p["PremiereDate"]); len(date) >= 4 {
			e.Year = intPointer(date[:4])
		}
		if typ == "Episode" {
			e.MediaType = "tv_series"
			e.Title = text(p["SeriesName"])
			sn := intPointer(p["SeasonNumber"])
			e.Episode = intPointer(p["EpisodeNumber"])
			if !e.Delete && (sn == nil || e.Episode == nil) {
				return nil, errors.New("Jellyfin episode requires season and episode")
			}
			if sn != nil {
				e.Season = *sn
			}
		} else if typ == "Movie" {
			e.SeriesID = e.EpisodeID
		} else if e.Delete && typ == "Series" {
			e.SeriesID = e.EpisodeID
		} else if e.Delete && typ == "Season" {
			e.SeasonID = e.EpisodeID
		} else {
			return nil, nil
		}
	case "plex":
		if _, ok := p["media_type"]; ok {
			action := strings.ToLower(text(p["action"]))
			if action != "" && action != "created" {
				return nil, nil
			}
			typ := text(p["media_type"])
			e.ItemType = typ
			e.Title = text(p["show_name"])
			if e.Title == "" {
				e.Title = text(p["title"])
			}
			e.MediaType = "movie"
			e.EpisodeID = text(p["rating_key"])
			e.SeasonID = text(p["parent_rating_key"])
			e.SeriesID = text(p["grandparent_rating_key"])
			if typ == "episode" || typ == "season" {
				e.MediaType = "tv_series"
				sn := intPointer(p["season"])
				if sn == nil {
					return nil, errors.New("Tautulli requires season")
				}
				e.Season = *sn
				eps, err := parseRanges(text(p["episode"]))
				if err != nil {
					return nil, err
				}
				out := []WebhookEvent{}
				for _, ep := range eps {
					copy := e
					n := ep
					copy.Episode = &n
					if copy.Title == "" {
						return nil, errors.New("Tautulli title required")
					}
					out = append(out, copy)
				}
				return out, nil
			} else if typ != "movie" {
				return nil, nil
			}
			if date := text(p["release_date"]); len(date) >= 4 {
				e.Year = intPointer(date[:4])
			}
			break
		}
		if text(p["event"]) != "library.new" {
			return nil, nil
		}
		m, _ := p["Metadata"].(map[string]any)
		if m == nil {
			return nil, errors.New("Plex payload requires Metadata")
		}
		e.ItemType = text(m["type"])
		e.MediaType = "movie"
		e.Title = text(m["title"])
		e.EpisodeID = text(m["ratingKey"])
		e.Year = intPointer(m["year"])
		e.SeasonID = text(m["parentRatingKey"])
		e.SeriesID = text(m["grandparentRatingKey"])
		if e.ItemType == "episode" {
			e.MediaType = "tv_series"
			e.Title = text(m["grandparentTitle"])
			sn := intPointer(m["parentIndex"])
			e.Episode = intPointer(m["index"])
			if sn == nil || e.Episode == nil {
				return nil, errors.New("Plex episode requires season and episode")
			}
			e.Season = *sn
		} else if e.ItemType != "movie" {
			return nil, nil
		}
		if guids, ok := m["Guid"].([]any); ok {
			for _, g := range guids {
				v, _ := g.(map[string]any)
				guid := text(v["id"])
				if strings.HasPrefix(guid, "tmdb://") {
					e.TMDBID = strings.TrimPrefix(guid, "tmdb://")
				}
				if strings.HasPrefix(guid, "tvdb://") {
					e.TVDBID = strings.TrimPrefix(guid, "tvdb://")
				}
				if strings.HasPrefix(guid, "imdb://") {
					e.IMDBID = strings.TrimPrefix(guid, "imdb://")
				}
			}
		}
	default:
		return nil, fmt.Errorf("unsupported webhook type %q", kind)
	}
	if e.Delete {
		if e.EpisodeID == "" && e.SeriesID == "" && e.SeasonID == "" {
			return nil, errors.New("deletion event requires exact media server IDs")
		}
		return []WebhookEvent{e}, nil
	}
	if strings.TrimSpace(e.Title) == "" {
		return nil, errors.New("webhook media title required")
	}
	if e.Season < 0 || e.Episode != nil && *e.Episode < 0 {
		return nil, errors.New("negative season/episode")
	}
	return []WebhookEvent{e}, nil
}
func parseRanges(s string) ([]int, error) {
	seen := map[int]bool{}
	out := []int{}
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		p := strings.Split(part, "-")
		if len(p) > 2 || len(p) == 0 {
			return nil, errors.New("invalid episode range")
		}
		start, e := strconv.Atoi(p[0])
		if e != nil || start < 0 {
			return nil, errors.New("invalid episode number")
		}
		end := start
		if len(p) == 2 {
			end, e = strconv.Atoi(p[1])
			if e != nil || end < start {
				return nil, errors.New("invalid episode range")
			}
		}
		if end-start > 1000 {
			return nil, errors.New("episode range exceeds 1000")
		}
		for n := start; n <= end; n++ {
			if !seen[n] {
				seen[n] = true
				out = append(out, n)
			}
			if len(out) > 1000 {
				return nil, errors.New("episode selection exceeds 1000")
			}
		}
	}
	return out, nil
}
