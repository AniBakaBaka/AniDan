// SPDX-License-Identifier: AGPL-3.0-only
package server

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"strings"

	"github.com/AniBakaBaka/AniDan/internal/notify"
	xdraw "golang.org/x/image/draw"
	"golang.org/x/image/font"
	"golang.org/x/image/font/basicfont"
	"golang.org/x/image/math/fixed"
)

// Downloads use the library poster boundary: public HTTP(S), every DNS answer
// checked, resolved addresses pinned, no environment proxy, bounded redirects,
// 10 MiB encoded and 20MP decoded caps. Only re-encoded, downscaled PNG is sent.
func notificationImage(ctx context.Context, raw string, client libHTTPDoer) (image.Image, error) {
	b, _, e := libFetchPoster(ctx, raw, client)
	if e != nil {
		return nil, errors.New("poster is unavailable or blocked by image safety limits")
	}
	if e = ctx.Err(); e != nil {
		return nil, e
	}
	src, _, e := image.Decode(bytes.NewReader(b))
	if e != nil {
		return nil, errors.New("poster decoding failed")
	}
	return src, nil
}
func notificationScale(src image.Image, w, h int) *image.RGBA {
	b := src.Bounds()
	scale := min(float64(w)/float64(b.Dx()), float64(h)/float64(b.Dy()), 1.0)
	width, height := max(1, int(float64(b.Dx())*scale)), max(1, int(float64(b.Dy())*scale))
	dst := image.NewRGBA(image.Rect(0, 0, width, height))
	xdraw.ApproxBiLinear.Scale(dst, dst.Bounds(), src, b, draw.Src, nil)
	return dst
}
func notificationPNG(img image.Image) ([]byte, error) {
	var b bytes.Buffer
	if e := png.Encode(&b, img); e != nil {
		return nil, e
	}
	if b.Len() > 2<<20 {
		return nil, notify.ErrLimit
	}
	return b.Bytes(), nil
}
func (s *Server) notificationPoster(ctx context.Context, c notify.Channel, u notify.Update, v *notificationSession, raw string) (notify.Message, error) {
	if c.Type == "serverchan3" {
		return notify.Message{}, notify.ErrUnsupported
	}
	select {
	case libPosterSlots <- struct{}{}:
		defer func() { <-libPosterSlots }()
	case <-ctx.Done():
		return notify.Message{}, ctx.Err()
	}
	src, e := s.notificationLoadImage(ctx, raw, libPosterClient())
	if e != nil {
		return notify.Message{}, e
	}
	b, e := notificationPNG(notificationScale(src, 800, 800))
	if e != nil {
		return notify.Message{}, e
	}
	m := s.notificationRender(c, u, v, "Poster", notificationChoice("Home", "home", ""))
	m.Image = b
	m.ImageShareable = true
	m.ImageInteractive = true
	return m, nil
}
func notificationBuildCollage(ctx context.Context, results []notificationSearchResult, start int, client libHTTPDoer) ([]byte, string, error) {
	return notificationBuildCollageWithLoader(ctx, results, start, func(ctx context.Context, raw string) (image.Image, error) { return notificationImage(ctx, raw, client) })
}

func (s *Server) notificationBuildCollage(ctx context.Context, results []notificationSearchResult, start int, client libHTTPDoer) ([]byte, string, error) {
	return notificationBuildCollageWithLoader(ctx, results, start, func(ctx context.Context, raw string) (image.Image, error) {
		return s.notificationLoadImage(ctx, raw, client)
	})
}

func notificationBuildCollageWithLoader(ctx context.Context, results []notificationSearchResult, start int, load func(context.Context, string) (image.Image, error)) ([]byte, string, error) {
	results = results[:min(len(results), 5)]
	canvas := image.NewRGBA(image.Rect(0, 0, 600, 640))
	draw.Draw(canvas, canvas.Bounds(), image.NewUniform(color.RGBA{26, 30, 38, 255}), image.Point{}, draw.Src)
	lines := []string{}
	loaded := 0
	for i, r := range results {
		if e := ctx.Err(); e != nil {
			return nil, "", e
		}
		col, row := i%3, i/3
		origin := image.Pt(col*200+10, row*320+10)
		drawer := font.Drawer{Dst: canvas, Src: image.NewUniform(color.White), Face: basicfont.Face7x13, Dot: fixed.P(origin.X, origin.Y+13)}
		drawer.DrawString(fmt.Sprintf("%d", start+i+1))
		label := fmt.Sprintf("%d. %s", start+i+1, notificationClip(r.Title, 65))
		if r.ImageURL == "" {
			lines = append(lines, label+" (no poster)")
			continue
		}
		src, e := load(ctx, r.ImageURL)
		if e != nil {
			lines = append(lines, label+" (poster unavailable)")
			continue
		}
		thumb := notificationScale(src, 180, 280)
		target := image.Rectangle{Min: origin.Add(image.Pt((180-thumb.Bounds().Dx())/2, 22)), Max: origin.Add(image.Pt((180-thumb.Bounds().Dx())/2+thumb.Bounds().Dx(), 22+thumb.Bounds().Dy()))}
		draw.Draw(canvas, target, thumb, thumb.Bounds().Min, draw.Src)
		loaded++
		lines = append(lines, label)
	}
	if loaded == 0 {
		return nil, "", errors.New("no safely retrievable posters in this page")
	}
	b, e := notificationPNG(canvas)
	return b, strings.Join(lines, "\n"), e
}
func (s *Server) notificationCollage(ctx context.Context, c notify.Channel, u notify.Update, v *notificationSession, page int) (notify.Message, error) {
	if c.Type == "serverchan3" {
		return notify.Message{}, notify.ErrUnsupported
	}
	page = max(0, min(page, max((len(v.results)-1)/5, 0)))
	start := page * 5
	select {
	case libPosterSlots <- struct{}{}:
		defer func() { <-libPosterSlots }()
	case <-ctx.Done():
		return notify.Message{}, ctx.Err()
	}
	b, legend, e := s.notificationBuildCollage(ctx, v.results[start:min(start+5, len(v.results))], start, libPosterClient())
	if e != nil {
		return notify.Message{}, e
	}
	m := s.notificationRender(c, u, v, legend, notificationOption{"Back to results", notificationAction{Kind: "search_page", Page: page}}, notificationChoice("Home", "home", ""))
	m.Image = b
	m.ImageShareable = true
	m.ImageInteractive = true
	return m, nil
}
