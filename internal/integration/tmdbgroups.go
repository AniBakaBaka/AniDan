// SPDX-License-Identifier: AGPL-3.0-or-later
package integration

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type GroupEpisode struct {
	ID            int64   `json:"id"`
	Name          string  `json:"name"`
	EpisodeNumber int     `json:"episodeNumber"`
	SeasonNumber  int     `json:"seasonNumber"`
	Order         int     `json:"order"`
	AirDate       *string `json:"airDate"`
	Overview      string  `json:"overview"`
}
type EpisodeGroupSection struct {
	ID       string         `json:"id"`
	Name     string         `json:"name"`
	Order    int            `json:"order"`
	Episodes []GroupEpisode `json:"episodes"`
}
type EpisodeGroup struct {
	ID           string                `json:"id"`
	Name         string                `json:"name"`
	Description  string                `json:"description"`
	EpisodeCount int                   `json:"episodeCount"`
	GroupCount   int                   `json:"groupCount"`
	Groups       []EpisodeGroupSection `json:"groups"`
	Network      map[string]any        `json:"network"`
	Type         int                   `json:"type"`
}

func (c *Client) TMDBGroups(ctx context.Context, tvID int64) ([]map[string]any, error) {
	if tvID <= 0 {
		return nil, &Error{"tmdb", 400, "positive TV ID required"}
	}
	v, e := c.tmdbRequest(ctx, "/tv/"+strconv.FormatInt(tvID, 10)+"/episode_groups", nil)
	if e != nil {
		return nil, e
	}
	out := []map[string]any{}
	for _, x := range array(object(v)["results"]) {
		o := object(x)
		out = append(out, map[string]any{"description": o["description"], "episodeCount": o["episode_count"], "groupCount": o["group_count"], "id": o["id"], "name": o["name"], "network": o["network"], "type": o["type"]})
	}
	return out, nil
}
func (c *Client) TMDBEpisodeGroup(ctx context.Context, id string) (*EpisodeGroup, error) {
	if e := safeID(id); e != nil {
		return nil, e
	}
	v, e := c.tmdbRequest(ctx, "/tv/episode_group/"+url.PathEscape(id), nil)
	if e != nil {
		return nil, e
	}
	return DecodeEpisodeGroup(object(v), id, false)
}

func groupNumber(o map[string]any, allowMissing bool, keys ...string) (int, error) {
	for _, key := range keys {
		if v, ok := o[key]; ok {
			n, e := strconv.ParseInt(str(v), 10, 32)
			if e != nil {
				return 0, &Error{"episode-group", 422, "invalid numeric field " + key}
			}
			return int(n), nil
		}
	}
	if allowMissing {
		return 0, nil
	}
	return 0, &Error{"episode-group", 422, "missing numeric field " + keys[0]}
}

// DecodeEpisodeGroup accepts native snake_case and control camelCase structures.
// local=true generates synthetic IDs in input traversal order, as StrmAssistant
// import does. Group order is a real custom season number, including 0 specials.
func DecodeEpisodeGroup(data map[string]any, id string, local bool) (*EpisodeGroup, error) {
	rows, ok := data["groups"].([]any)
	if !ok || len(rows) == 0 || len(rows) > 1000 {
		return nil, &Error{"episode-group", 422, "groups must contain 1..1000 sections"}
	}
	group := &EpisodeGroup{ID: id, Name: first(data["name"], data["description"], "本地剧集组"), Description: str(data["description"]), Groups: []EpisodeGroupSection{}, Network: nil, Type: integer(data["type"])}
	if network, ok := data["network"].(map[string]any); ok {
		group.Network = network
	}
	seen := map[int64]bool{}
	for _, raw := range rows {
		o := object(raw)
		order, e := groupNumber(o, local, "order")
		if e != nil {
			return nil, e
		}
		if order < 0 || order > 9999 {
			return nil, &Error{"episode-group", 422, "invalid group order"}
		}
		section := EpisodeGroupSection{ID: str(o["id"]), Name: str(o["name"]), Order: order, Episodes: []GroupEpisode{}}
		episodes, ok := o["episodes"].([]any)
		if !ok {
			return nil, &Error{"episode-group", 422, "episodes array is required"}
		}
		for _, x := range episodes {
			p := object(x)
			sn, e := groupNumber(p, local, "seasonNumber", "season_number")
			if e != nil {
				return nil, e
			}
			en, e := groupNumber(p, local, "episodeNumber", "episode_number")
			if e != nil {
				return nil, e
			}
			eo, e := groupNumber(p, true, "order")
			if e != nil {
				return nil, e
			}
			ei := 0
			if !local {
				ei, e = groupNumber(p, false, "id")
				if e != nil {
					return nil, e
				}
			}
			ep := GroupEpisode{ID: int64(ei), Name: str(p["name"]), SeasonNumber: sn, EpisodeNumber: en, Order: eo, AirDate: ptr(first(p["airDate"], p["air_date"])), Overview: str(p["overview"])}
			group.EpisodeCount++
			if group.EpisodeCount > 100000 {
				return nil, &Error{"episode-group", 422, "too many episodes"}
			}
			if local {
				ep.ID = int64(group.EpisodeCount)
				ep.Name = ""
			} else if _, ok := p["id"]; !ok {
				return nil, &Error{"episode-group", 422, "episode ID is required"}
			}
			if ep.SeasonNumber < 0 || ep.EpisodeNumber < 0 || ep.Order < 0 || ep.ID < 0 || ep.ID > 2147483647 || len([]rune(ep.Name)) > 500 {
				return nil, &Error{"episode-group", 422, "invalid episode fields"}
			}
			if seen[ep.ID] {
				return nil, &Error{"episode-group", 422, "duplicate episode ID"}
			}
			seen[ep.ID] = true
			section.Episodes = append(section.Episodes, ep)
		}
		group.Groups = append(group.Groups, section)
	}
	group.GroupCount = len(group.Groups)
	if group.EpisodeCount == 0 {
		return nil, &Error{"episode-group", 400, "episode group has no episodes"}
	}
	return group, nil
}
func publicGroupURL(raw string) (*url.URL, error) {
	u, e := url.Parse(raw)
	if e != nil || u.Host == "" || u.User != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Fragment != "" {
		return nil, errors.New("invalid remote group URL")
	}
	if a, e := netip.ParseAddr(u.Hostname()); e == nil && !publicGroupIP(a) {
		return nil, errors.New("private network group URLs are not allowed")
	}
	host := strings.ToLower(u.Hostname())
	if host == "localhost" || strings.HasSuffix(host, ".localhost") || strings.HasSuffix(host, ".local") {
		return nil, errors.New("private network group URLs are not allowed")
	}
	return u, nil
}
func publicGroupIP(a netip.Addr) bool {
	a = a.Unmap()
	return a.IsGlobalUnicast() && !a.IsPrivate() && !a.IsLoopback() && !a.IsLinkLocalUnicast() && !a.IsMulticast() && !a.IsUnspecified() && !netip.MustParsePrefix("100.64.0.0/10").Contains(a)
}

// FetchEpisodeGroupURL is an unauthenticated, bounded document fetch. It never
// reuses API credentials, proxy credentials or cookies. DNS is pinned to a
// validated public address at dial time; redirects are revalidated.
func (c *Client) FetchEpisodeGroupURL(ctx context.Context, raw string) (map[string]any, error) {
	u, e := publicGroupURL(raw)
	if e != nil {
		return nil, &Error{"episode-group", 400, e.Error()}
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, e := net.SplitHostPort(address)
		if e != nil {
			return nil, e
		}
		addrs, e := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
		if e != nil || len(addrs) == 0 {
			return nil, errors.New("remote DNS lookup failed")
		}
		for _, a := range addrs {
			if !publicGroupIP(a) {
				return nil, errors.New("remote address resolves to private network")
			}
		}
		var last error
		for _, a := range addrs {
			conn, e := dialer.DialContext(ctx, network, net.JoinHostPort(a.String(), port))
			if e == nil {
				return conn, nil
			}
			last = e
		}
		return nil, last
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{Timeout: 15 * time.Second, Transport: transport, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return errors.New("too many redirects")
		}
		if _, e := publicGroupURL(req.URL.String()); e != nil {
			return e
		}
		if len(via) > 0 && via[0].URL.Scheme == "https" && req.URL.Scheme != "https" {
			return errors.New("HTTPS downgrade refused")
		}
		return nil
	}}
	req, e := http.NewRequestWithContext(ctx, "GET", u.String(), nil)
	if e != nil {
		return nil, &Error{"episode-group", 400, "invalid request"}
	}
	req.Header.Set("User-Agent", "AniDan/0.1")
	res, e := client.Do(req)
	if e != nil {
		return nil, &Error{"episode-group", 502, "remote group request failed"}
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return nil, &Error{"episode-group", 502, "remote group returned HTTP " + strconv.Itoa(res.StatusCode)}
	}
	b, e := io.ReadAll(io.LimitReader(res.Body, (4<<20)+1))
	if e != nil || len(b) > 4<<20 {
		return nil, &Error{"episode-group", 400, "group document unreadable or exceeds 4 MiB"}
	}
	out := map[string]any{}
	if decode(b, &out) != nil {
		return nil, &Error{"episode-group", 422, "invalid group JSON"}
	}
	if _, ok := out["groups"].([]any); !ok {
		return nil, &Error{"episode-group", 422, "group document is missing groups"}
	}
	return out, nil
}
