// SPDX-License-Identifier: AGPL-3.0-or-later
package integration

import (
	"bytes"
	"context"
	"encoding/xml"
	"net/url"
	"regexp"
	"strings"
	"sync"
)

// ValidateAniBTRSSURL restricts credential-bearing feeds to the configured AniBT
// API origin (or the default HTTPS anibt.net origin). It never logs the URL.
func (c *Client) ValidateAniBTRSSURL(ctx context.Context, raw string) error {
	u, e := url.Parse(strings.TrimSpace(raw))
	base, e2 := url.Parse(c.base(ctx, "anibtApiBaseUrl", "https://anibt.net"))
	if e != nil || e2 != nil || u.Host == "" || u.User != nil || u.Fragment != "" || u.Host != base.Host || u.Scheme != base.Scheme || (u.Scheme != "https" && u.Scheme != "http") || !strings.HasPrefix(u.Path, "/rss") {
		return &Error{"anibt", 400, "RSS URL must use the configured AniBT origin and /rss path"}
	}
	return nil
}

type rssNode struct {
	XMLName  xml.Name
	Attrs    []xml.Attr `xml:",any,attr"`
	Text     string     `xml:",chardata"`
	Children []rssNode  `xml:",any"`
}

func (n rssNode) text(name string) string {
	for _, c := range n.Children {
		if c.XMLName.Local == name {
			if name == "link" {
				for _, a := range c.Attrs {
					if a.Name.Local == "href" {
						return a.Value
					}
				}
			}
			return strings.TrimSpace(c.Text)
		}
	}
	return ""
}

// AniBTRSS returns provider/externalId/title/animeType/subscriptionType/status/
// extraData maps compatible with the upstream subscription scanner. XML external
// entities are unsupported by encoding/xml. Payload and item counts are bounded.
func (c *Client) AniBTRSS(ctx context.Context, raw string) ([]map[string]any, error) {
	if e := c.ValidateAniBTRSSURL(ctx, raw); e != nil {
		return nil, e
	}
	body, e := c.request(ctx, "anibt", "GET", raw, nil, nil, nil, 8<<20)
	if e != nil {
		return nil, e
	}
	decoder := xml.NewDecoder(bytes.NewReader(body))
	var root rssNode
	if decoder.Decode(&root) != nil {
		return nil, &Error{"anibt", 502, "invalid RSS/Atom XML"}
	}
	entries := []rssNode{}
	var walk func(rssNode, int) error
	walk = func(n rssNode, depth int) error {
		if depth > 64 {
			return &Error{"anibt", 502, "RSS XML nesting too deep"}
		}
		if n.XMLName.Local == "item" || n.XMLName.Local == "entry" {
			entries = append(entries, n)
			if len(entries) > 1000 {
				return &Error{"anibt", 502, "RSS entry limit exceeded"}
			}
			return nil
		}
		for _, child := range n.Children {
			if e := walk(child, depth+1); e != nil {
				return e
			}
		}
		return nil
	}
	if e = walk(root, 0); e != nil {
		return nil, e
	}
	ids, titles := []string{}, []string{}
	seen := map[string]bool{}
	pattern := regexp.MustCompile(`(?i)(?:bgm(?:Id)?[=:/-]?|subject/)([0-9]{1,10})`)
	for _, entry := range entries {
		title := entry.text("title")
		match := pattern.FindStringSubmatch(strings.Join([]string{title, entry.text("link"), entry.text("guid"), entry.text("id")}, " "))
		if len(match) < 2 || seen[match[1]] {
			continue
		}
		seen[match[1]] = true
		ids = append(ids, match[1])
		titles = append(titles, title)
	}
	if len(ids) > 200 {
		return nil, &Error{"anibt", 502, "RSS subject limit exceeded"}
	}
	out := make([]map[string]any, len(ids))
	errs := make([]error, len(ids))
	sem := make(chan struct{}, 4)
	var wg sync.WaitGroup
	var launchErr error
launch:
	for i, id := range ids {
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			launchErr = ctx.Err()
			break launch
		}
		wg.Add(1)
		go func(i int, id string) {
			defer wg.Done()
			defer func() { <-sem }()
			m, e := c.anibtDetails(ctx, id)
			if e != nil && Status(e) != 404 {
				errs[i] = e
				return
			}
			title, typ := titles[i], "tv_series"
			extra := map[string]any{"bangumiId": id, "year": nil, "imageUrl": nil}
			if m != nil {
				title = m.Title
				typ = m.Type
				if m.Year > 0 {
					extra["year"] = m.Year
				}
				extra["imageUrl"] = nullable(m.ImageURL)
			}
			out[i] = map[string]any{"provider": "anibt", "externalId": "bgm-" + id, "title": title, "animeType": typ, "subscriptionType": "anibt_subject", "status": "pending", "extraData": extra}
		}(i, id)
	}
	wg.Wait()
	if launchErr != nil {
		return nil, launchErr
	}
	for _, e := range errs {
		if e != nil {
			return nil, e
		}
	}
	return out, nil
}
