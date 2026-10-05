// SPDX-License-Identifier: AGPL-3.0-only
package provider

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

func resolveEpisode(ctx context.Context, p Provider, raw string) (Episode, error) {
	name, media, ep, e := ResolveURL(raw)
	if e != nil {
		return Episode{}, e
	}
	if name != p.Name() {
		return Episode{}, errors.New("URL provider mismatch")
	}
	if ep != "" {
		return Episode{ID: ep, URL: raw, Index: 1}, nil
	}
	if media == "" {
		return Episode{}, errors.New("URL does not identify media or episode")
	}
	eps, e := p.Episodes(ctx, media)
	if e != nil {
		return Episode{}, e
	}
	if len(eps) != 1 {
		return Episode{}, fmt.Errorf("URL resolves to %d episodes; select a specific episode", len(eps))
	}
	return eps[0], nil
}
func (r *Registry) ResolveEpisode(ctx context.Context, raw string) (Episode, error) {
	name, _, _, e := ResolveURL(raw)
	if e != nil {
		return Episode{}, e
	}
	p, ok := r.Get(name)
	if !ok {
		return Episode{}, errors.New("provider not registered")
	}
	if resolver, ok := p.(interface {
		ResolveEpisode(context.Context, string) (Episode, error)
	}); ok {
		return resolver.ResolveEpisode(ctx, raw)
	}
	return resolveEpisode(ctx, p, raw)
}
func (b *Bilibili) ResolveEpisode(ctx context.Context, raw string) (Episode, error) {
	return resolveEpisode(ctx, b, raw)
}
func (l *Legacy) ResolveEpisode(ctx context.Context, raw string) (Episode, error) {
	if l.Source == "iqiyi" {
		_, video, err := l.iqiyiURLVideo(ctx, raw)
		if err != nil {
			return Episode{}, err
		}
		index := 1
		if video.ChannelID != "1" && video.ChannelName != "电影" {
			if video.Order == nil || *video.Order < 0 || *video.Order > 1000000 {
				return Episode{}, errors.New("iqiyi URL lacks a bounded exact episode index")
			}
			index = *video.Order
		}
		id := video.TVID.String()
		if id == "" {
			id = video.VideoID.String()
		}
		if strings.TrimSpace(video.Name) == "" || len(video.Name) > 4096 {
			return Episode{}, errors.New("iqiyi URL lacks episode title")
		}
		return Episode{ID: id, Title: video.Name, URL: video.PlayURL, Index: index}, nil
	}
	return resolveEpisode(ctx, l, raw)
}
func (r *Registry) Collection(ctx context.Context, raw string) ([]Episode, error) {
	name, _, _, e := ResolveURL(raw)
	if e != nil {
		return nil, e
	}
	p, ok := r.Get(name)
	if !ok {
		return nil, errors.New("provider not registered")
	}
	if c, ok := p.(interface {
		Collection(context.Context, string) ([]Episode, error)
	}); ok {
		return c.Collection(ctx, raw)
	}
	return nil, &UnsupportedError{name, "collection", "collection expansion is only available for Bilibili UGC seasons"}
}
