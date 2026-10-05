// SPDX-License-Identifier: AGPL-3.0-only
package provider

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"golang.org/x/net/html"
)

var leInfoStart = regexp.MustCompile(`^\s*var\s+__INFO__\s*=\s*\{`)
var leDetailVideo = regexp.MustCompile(`(?:^|[,{}])\s*video\s*:\s*(\{[^{}]*\})`)
var lePlayIdentity = regexp.MustCompile(`(?m)^\s*(pid|vid)\s*:\s*([0-9]+)\s*,`)
var leVideoStart = regexp.MustCompile(`(?m)^\s*video\s*:\s*\{`)

func leInfoScript(doc *html.Node) (string, error) {
	var found string
	for _, n := range nodes(doc, func(n *html.Node) bool { return n.Type == html.ElementNode && n.Data == "script" }) {
		s := nodeText(n)
		if !leInfoStart.MatchString(s) {
			continue
		}
		if found != "" || len(s) > 512<<10 {
			return "", errors.New("le media metadata ambiguous or oversized")
		}
		found = s
	}
	if found == "" {
		return "", errors.New("le source media metadata missing")
	}
	return found, nil
}

// leResolveMedia reads source-declared identities and the scoped detail heading.
// It never evaluates JavaScript or substitutes an episode title for a series.
func (l *Legacy) leResolveMedia(ctx context.Context, raw string) (Result, error) {
	name, media, episode, err := ResolveURL(raw)
	if err != nil {
		return Result{}, err
	}
	if name != "le" || l.Name() != "le" {
		return Result{}, errors.New("URL provider mismatch")
	}
	if len(media) > 64 || len(episode) > 64 {
		return Result{}, errors.New("le media identity too long")
	}
	u, _ := url.Parse(raw)
	target := "https://www.le.com" + u.Path
	h := l.HTTPProvider
	if h.MaxBytes <= 0 || h.MaxBytes > 4<<20 {
		h.MaxBytes = 4 << 20
	}
	fetch := func(target string) (*html.Node, error) {
		b, e := h.request(ctx, http.MethodGet, target, nil, nil, http.Header{"Accept": {"text/html"}})
		if e != nil {
			return nil, e
		}
		return leHTML(b)
	}
	doc, err := fetch(target)
	if err != nil {
		return Result{}, err
	}
	info, err := leInfoScript(doc)
	if err != nil {
		return Result{}, err
	}
	if episode != "" {
		boundary := leVideoStart.FindStringIndex(info)
		if boundary == nil {
			return Result{}, errors.New("le playback identity missing")
		}
		ids := map[string]string{}
		for _, m := range lePlayIdentity.FindAllStringSubmatch(info[:boundary[0]], -1) {
			if ids[m[1]] != "" {
				return Result{}, errors.New("le playback identity ambiguous")
			}
			ids[m[1]] = m[2]
		}
		if ids["vid"] != episode || !numericID.MatchString(ids["pid"]) || len(ids["pid"]) > 64 {
			return Result{}, errors.New("le playback identity mismatch")
		}
		media = ids["pid"]
		var detail string
		for _, n := range nodes(doc, func(n *html.Node) bool {
			return n.Type == html.ElementNode && n.Data == "a" && class(n, "briefIntro_more")
		}) {
			link, e := url.Parse(attr(n, "href"))
			if e != nil {
				continue
			}
			link = u.ResolveReference(link)
			provider, id, ep, e := ResolveURL(link.String())
			if e != nil || provider != "le" || id != media || ep != "" {
				return Result{}, errors.New("le playback detail identity mismatch")
			}
			if detail != "" {
				return Result{}, errors.New("le playback detail link ambiguous")
			}
			detail = "https://www.le.com" + link.Path
		}
		if detail == "" {
			return Result{}, errors.New("le playback detail link missing")
		}
		doc, err = fetch(detail)
		if err != nil {
			return Result{}, err
		}
		info, err = leInfoScript(doc)
		if err != nil {
			return Result{}, err
		}
	}
	matches := leDetailVideo.FindAllStringSubmatch(info, -1)
	if len(matches) != 1 {
		return Result{}, errors.New("le detail media identity missing or ambiguous")
	}
	fields, err := leStringObject(matches[0][1])
	if err != nil {
		return Result{}, err
	}
	if fields["pid"] != media {
		return Result{}, errors.New("le detail media identity mismatch")
	}
	headings := nodes(doc, func(n *html.Node) bool { return n.Type == html.ElementNode && class(n, "top_tit") })
	if len(headings) != 1 {
		return Result{}, errors.New("le detail title missing or ambiguous")
	}
	title := nodeText(findNode(headings[0], func(n *html.Node) bool { return n.Data == "h2" }))
	if title == "" || len(title) > 4096 {
		return Result{}, errors.New("le detail title missing or oversized")
	}
	category := nodeText(findNode(headings[0], func(n *html.Node) bool { return class(n, "top_name") }))
	typ := map[string]string{"电视剧": "tv_series", "电影": "movie", "动漫": "tv_series", "短剧": "tv_series"}[category]
	if typ == "" {
		return Result{}, errors.New("le detail media category unsupported")
	}
	cover := ""
	pic := findNode(doc, func(n *html.Node) bool { return attr(n, "id") == "play_pic" })
	if img := findNode(pic, func(n *html.Node) bool { return n.Data == "img" }); img != nil {
		cover = attr(img, "src")
		if strings.HasPrefix(cover, "//") {
			cover = "https:" + cover
		}
		parsed, e := url.Parse(cover)
		if e != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.User != nil || len(cover) > 4096 {
			cover = ""
		}
	}
	year := 0
	details := findNode(doc, func(n *html.Node) bool { return attr(n, "id") == "play_info" })
	for _, li := range nodes(details, func(n *html.Node) bool { return n.Data == "li" }) {
		if nodeText(findNode(li, func(n *html.Node) bool { return n.Data == "b" })) == "年代：" {
			year = yearFromDate(attr(findNode(li, func(n *html.Node) bool { return n.Data == "span" }), "title"))
			break
		}
	}
	return Result{ID: media, Title: title, Type: typ, ImageURL: cover, Year: year}, nil
}
