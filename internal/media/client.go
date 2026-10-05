// SPDX-License-Identifier: AGPL-3.0-only
// API mapping derived from the pinned legacy media_servers package; see LICENSE.
package media

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const MaxResponseBytes int64 = 16 << 20
const MaxScanItems = 1_000_000

type Library struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Type string `json:"type"`
}
type Item struct {
	MediaID   string `json:"mediaId"`
	Title     string `json:"title"`
	MediaType string `json:"mediaType"`
	Year      *int   `json:"year"`
	Season    *int   `json:"season"`
	Episode   *int   `json:"episode"`
	TMDBID    string `json:"tmdbId"`
	TVDBID    string `json:"tvdbId"`
	IMDBID    string `json:"imdbId"`
	PosterURL string `json:"posterUrl"`
	LibraryID string `json:"libraryId"`
	SeriesID  string `json:"seriesId"`
	SeasonID  string `json:"seasonId"`
	EpisodeID string `json:"episodeId"`
}
type Client struct {
	kind  string
	base  *url.URL
	token string
	http  *http.Client
}

func NewClient(kind, base, token string, httpClient *http.Client) (*Client, error) {
	if kind != "emby" && kind != "jellyfin" && kind != "plex" {
		return nil, fmt.Errorf("unsupported media server type %q", kind)
	}
	u, e := url.Parse(base)
	if e != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("media URL must be HTTP(S), without credentials/query/fragment")
	}
	u.Path = strings.TrimRight(u.Path, "/")
	if strings.TrimSpace(token) == "" {
		return nil, errors.New("media API token is required")
	}
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	copy := *httpClient
	old := copy.CheckRedirect
	copy.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if req.URL.Scheme != u.Scheme || req.URL.Host != u.Host {
			return errors.New("media redirect changed origin; credentials not forwarded")
		}
		if len(via) > 5 {
			return errors.New("too many redirects")
		}
		if old != nil {
			return old(req, via)
		}
		return nil
	}
	return &Client{kind, u, token, &copy}, nil
}
func (c *Client) endpoint(p string, q url.Values) (string, error) {
	if !strings.HasPrefix(p, "/") || strings.Contains(p, "..") || strings.HasPrefix(p, "//") {
		return "", errors.New("invalid media endpoint")
	}
	u := *c.base
	u.Path += p
	u.RawQuery = q.Encode()
	return u.String(), nil
}
func (c *Client) request(ctx context.Context, p string, q url.Values) (*http.Response, error) {
	target, e := c.endpoint(p, q)
	if e != nil {
		return nil, e
	}
	req, e := http.NewRequestWithContext(ctx, "GET", target, nil)
	if e != nil {
		return nil, e
	}
	req.Header.Set("Accept", "application/json")
	if c.kind == "plex" {
		req.Header.Set("X-Plex-Token", c.token)
		req.Header.Set("X-Plex-Product", "AniDan")
		req.Header.Set("X-Plex-Client-Identifier", "anidan-media-client")
	} else {
		req.Header.Set("X-Emby-Token", c.token)
	}
	res, e := c.http.Do(req)
	if e != nil {
		return nil, fmt.Errorf("media request failed: %s", redactHTTPError(e))
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		res.Body.Close()
		return nil, fmt.Errorf("media server returned HTTP %d", res.StatusCode)
	}
	return res, nil
}
func redactHTTPError(e error) string {
	var u *url.Error
	if errors.As(e, &u) {
		return u.Err.Error()
	}
	return e.Error()
}
func (c *Client) get(ctx context.Context, p string, q url.Values, v any) error {
	res, e := c.request(ctx, p, q)
	if e != nil {
		return e
	}
	defer res.Body.Close()
	lr := &io.LimitedReader{R: res.Body, N: MaxResponseBytes + 1}
	d := json.NewDecoder(lr)
	d.UseNumber()
	if e = d.Decode(v); e != nil {
		return fmt.Errorf("invalid media JSON: %w", e)
	}
	var extra any
	if e = d.Decode(&extra); e != io.EOF {
		return errors.New("media response has trailing JSON")
	}
	if lr.N <= 0 {
		return errors.New("media response exceeds byte limit")
	}
	return nil
}
func (c *Client) Info(ctx context.Context) (map[string]any, error) {
	if c.kind == "plex" {
		var r struct {
			Container map[string]any `json:"MediaContainer"`
		}
		if e := c.get(ctx, "/", nil, &r); e != nil {
			return nil, e
		}
		if r.Container == nil {
			return nil, errors.New("Plex did not return MediaContainer")
		}
		return map[string]any{"ServerName": r.Container["friendlyName"], "Version": r.Container["version"], "Id": r.Container["machineIdentifier"]}, nil
	}
	var r map[string]any
	if e := c.get(ctx, "/System/Info", nil, &r); e != nil {
		return nil, e
	}
	if _, ok := r["ServerName"]; !ok {
		return nil, errors.New("media server info has no ServerName")
	}
	return map[string]any{"ServerName": r["ServerName"], "Version": r["Version"], "Id": r["Id"]}, nil
}
func (c *Client) Libraries(ctx context.Context) ([]Library, error) {
	out := []Library{}
	if c.kind == "plex" {
		var r struct {
			Container struct {
				Directories []map[string]any `json:"Directory"`
			} `json:"MediaContainer"`
		}
		if e := c.get(ctx, "/library/sections", nil, &r); e != nil {
			return nil, e
		}
		for _, x := range r.Container.Directories {
			typ := "mixed"
			if text(x["type"]) == "movie" {
				typ = "movies"
			} else if text(x["type"]) == "show" {
				typ = "tvshows"
			}
			out = append(out, Library{text(x["key"]), text(x["title"]), typ})
		}
		return out, nil
	}
	var r []map[string]any
	if e := c.get(ctx, "/Library/VirtualFolders", nil, &r); e != nil {
		return nil, e
	}
	for _, x := range r {
		typ := text(x["CollectionType"])
		if typ != "movies" && typ != "tvshows" {
			typ = "mixed"
		}
		out = append(out, Library{text(x["ItemId"]), text(x["Name"]), typ})
	}
	return out, nil
}
func (c *Client) pages(ctx context.Context, endpoint string, params url.Values, emit func(map[string]any) error) error {
	if params == nil {
		params = url.Values{}
	}
	const size = 200
	count := 0
	previous := ""
	for start := 0; start < MaxScanItems; start += size {
		if e := ctx.Err(); e != nil {
			return e
		}
		if c.kind == "plex" {
			params.Set("X-Plex-Container-Start", strconv.Itoa(start))
			params.Set("X-Plex-Container-Size", strconv.Itoa(size))
		} else {
			params.Set("StartIndex", strconv.Itoa(start))
			params.Set("Limit", strconv.Itoa(size))
		}
		var items []map[string]any
		total := -1
		if c.kind == "plex" {
			var r struct {
				Container struct {
					Metadata []map[string]any `json:"Metadata"`
					Total    *int             `json:"totalSize"`
				} `json:"MediaContainer"`
			}
			if e := c.get(ctx, endpoint, params, &r); e != nil {
				return e
			}
			items = r.Container.Metadata
			if r.Container.Total != nil {
				total = *r.Container.Total
			}
		} else {
			var r struct {
				Items []map[string]any `json:"Items"`
				Total *int             `json:"TotalRecordCount"`
			}
			if e := c.get(ctx, endpoint, params, &r); e != nil {
				return e
			}
			items = r.Items
			if r.Total != nil {
				total = *r.Total
			}
		}
		if len(items) == 0 {
			if total > count {
				return errors.New("media pagination ended before declared total")
			}
			return nil
		}
		signature := text(items[0]["Id"]) + text(items[0]["ratingKey"]) + "/" + text(items[len(items)-1]["Id"]) + text(items[len(items)-1]["ratingKey"])
		if start > 0 && signature == previous {
			return errors.New("media server repeated a pagination page")
		}
		previous = signature
		for _, r := range items {
			if e := emit(r); e != nil {
				return e
			}
			count++
		}
		if total >= 0 && count >= total {
			return nil
		}
		if total < 0 && len(items) < size {
			return nil
		}
	}
	return errors.New("media scan exceeds one million items")
}
func (c *Client) Scan(ctx context.Context, libraryID, mediaType string, emit func(Item) error) error {
	if libraryID == "" {
		return errors.New("library ID required")
	}
	if c.kind == "plex" {
		params := url.Values{"includeGuids": {"1"}}
		if mediaType == "movie" {
			params.Set("type", "1")
		} else if mediaType == "tv_series" {
			params.Set("type", "2")
		}
		return c.pages(ctx, "/library/sections/"+url.PathEscape(libraryID)+"/all", params, func(r map[string]any) error {
			typ := text(r["type"])
			if typ == "movie" {
				return emit(c.plexItem(r, libraryID, nil))
			}
			if typ != "show" {
				return nil
			}
			series := c.plexItem(r, libraryID, nil)
			return c.pages(ctx, "/library/metadata/"+url.PathEscape(series.MediaID)+"/allLeaves", url.Values{"includeGuids": {"1"}}, func(ep map[string]any) error {
				if text(ep["type"]) != "episode" {
					return nil
				}
				return emit(c.plexItem(ep, libraryID, &series))
			})
		})
	}
	types := "Movie,Series"
	if mediaType == "movie" {
		types = "Movie"
	} else if mediaType == "tv_series" {
		types = "Series"
	}
	return c.pages(ctx, "/Items", url.Values{"ParentId": {libraryID}, "Recursive": {"true"}, "IncludeItemTypes": {types}, "Fields": {"ProviderIds,ProductionYear"}}, func(r map[string]any) error {
		switch text(r["Type"]) {
		case "Movie":
			return emit(c.embyItem(r, libraryID, nil, ""))
		case "Series":
			series := c.embyItem(r, libraryID, nil, "")
			return c.pages(ctx, "/Items", url.Values{"ParentId": {series.MediaID}, "Recursive": {"false"}, "IncludeItemTypes": {"Season"}, "Fields": {"ProviderIds,ChildCount"}}, func(season map[string]any) error {
				if text(season["Type"]) != "Season" {
					return nil
				}
				seasonID := text(season["Id"])
				return c.pages(ctx, "/Items", url.Values{"ParentId": {seasonID}, "Recursive": {"false"}, "IncludeItemTypes": {"Episode"}, "Fields": {"ProviderIds,ProductionYear"}}, func(ep map[string]any) error {
					if text(ep["Type"]) != "Episode" {
						return nil
					}
					item := c.embyItem(ep, libraryID, &series, seasonID)
					if item.Season == nil {
						item.Season = intPointer(season["IndexNumber"])
					}
					return emit(item)
				})
			})
		}
		return nil
	})
}
func (c *Client) embyItem(r map[string]any, library string, parent *Item, seasonID string) Item {
	ids, _ := r["ProviderIds"].(map[string]any)
	item := Item{MediaID: text(r["Id"]), Title: text(r["Name"]), MediaType: "movie", Year: intPointer(r["ProductionYear"]), TMDBID: text(ids["Tmdb"]), TVDBID: text(ids["Tvdb"]), IMDBID: text(ids["Imdb"]), LibraryID: library}
	typ := text(r["Type"])
	if typ == "Series" || typ == "Episode" {
		item.MediaType = "tv_series"
	}
	posterID := item.MediaID
	if typ == "Episode" {
		item.Title = text(r["SeriesName"])
		item.Season = intPointer(r["ParentIndexNumber"])
		item.Episode = intPointer(r["IndexNumber"])
		item.SeriesID = text(r["SeriesId"])
		item.SeasonID = seasonID
		item.EpisodeID = item.MediaID
		posterID = item.SeriesID
		if parent != nil {
			item.Title = parent.Title
			item.Year = parent.Year
			item.TMDBID = parent.TMDBID
			item.TVDBID = parent.TVDBID
			item.IMDBID = parent.IMDBID
			item.SeriesID = parent.MediaID
			posterID = parent.MediaID
		}
	}
	if posterID != "" {
		item.PosterURL = "/Items/" + url.PathEscape(posterID) + "/Images/Primary"
	}
	return item
}
func (c *Client) plexItem(r map[string]any, library string, parent *Item) Item {
	item := Item{MediaID: text(r["ratingKey"]), Title: text(r["title"]), MediaType: "movie", Year: intPointer(r["year"]), LibraryID: library, PosterURL: text(r["thumb"])}
	if guids, ok := r["Guid"].([]any); ok {
		for _, g := range guids {
			m, _ := g.(map[string]any)
			v := text(m["id"])
			switch {
			case strings.HasPrefix(v, "tmdb://"):
				item.TMDBID = strings.TrimPrefix(v, "tmdb://")
			case strings.HasPrefix(v, "tvdb://"):
				item.TVDBID = strings.TrimPrefix(v, "tvdb://")
			case strings.HasPrefix(v, "imdb://"):
				item.IMDBID = strings.TrimPrefix(v, "imdb://")
			}
		}
	}
	if typ := text(r["type"]); typ == "show" || typ == "episode" {
		item.MediaType = "tv_series"
		if typ == "episode" {
			item.Title = text(r["grandparentTitle"])
			item.Season = intPointer(r["parentIndex"])
			item.Episode = intPointer(r["index"])
			item.SeriesID = text(r["grandparentRatingKey"])
			item.SeasonID = text(r["parentRatingKey"])
			item.EpisodeID = item.MediaID
			item.PosterURL = text(r["grandparentThumb"])
			if parent != nil {
				item.Title = parent.Title
				item.Year = parent.Year
				item.TMDBID = parent.TMDBID
				item.TVDBID = parent.TVDBID
				item.IMDBID = parent.IMDBID
				item.SeriesID = parent.MediaID
				item.PosterURL = parent.PosterURL
			}
		}
	}
	return item
}
func (c *Client) Image(ctx context.Context, p string) (io.ReadCloser, string, error) {
	if !(strings.HasPrefix(p, "/Items/") && strings.Contains(p, "/Images/") || strings.HasPrefix(p, "/library/metadata/") && strings.Contains(p, "/thumb/")) {
		return nil, "", errors.New("unsupported image path")
	}
	res, e := c.request(ctx, p, nil)
	if e != nil {
		return nil, "", e
	}
	typ := res.Header.Get("Content-Type")
	if !strings.HasPrefix(typ, "image/") {
		res.Body.Close()
		return nil, "", errors.New("media image endpoint did not return an image")
	}
	return res.Body, typ, nil
}
func text(v any) string {
	if v == nil {
		return ""
	}
	switch x := v.(type) {
	case string:
		return x
	case json.Number:
		return string(x)
	default:
		return fmt.Sprint(v)
	}
}
func intPointer(v any) *int {
	if v == nil {
		return nil
	}
	i, e := strconv.Atoi(text(v))
	if e != nil {
		return nil
	}
	return &i
}
