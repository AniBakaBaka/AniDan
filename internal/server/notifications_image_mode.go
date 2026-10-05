// SPDX-License-Identifier: AGPL-3.0-only
package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/AniBakaBaka/AniDan/internal/notify"
)

const notificationThumbnailDir = "image/notification-public"

var notificationThumbnailMu sync.Mutex
var notificationThumbnailServeSlots = make(chan struct{}, 4)
var notificationThumbnailName = regexp.MustCompile(`^[a-f0-9]{64}\.jpg$`)

type notificationThumbnailLimits struct {
	count int
	bytes int64
	ttl   time.Duration
}

var notificationThumbnailDefaults = notificationThumbnailLimits{512, 64 << 20, 7 * 24 * time.Hour}

func notificationImageMode(c notify.Channel) string {
	mode := notify.Str(c.Config["image_mode"])
	if mode == "" {
		return "poster"
	}
	return mode
}

// Only explicitly classified poster/collage content is eligible for anonymous
// public persistence. Login QR/private bytes always retain the false default.
func (s *Server) notificationSend(ctx context.Context, c notify.Channel, m notify.Message) error {
	if err := notify.Validate(c); err != nil {
		return err
	}
	if !c.Enabled {
		return errors.New("notification channel is disabled")
	}
	if m.PublicImageURL != "" {
		return errors.New("notification callers must supply image bytes, not public URLs")
	}
	var err error
	ctx, err = s.Notify.PrepareOperation(ctx, c)
	if err != nil {
		return err
	}
	// Explicit private QR/image responses bypass automatic event formatting. They
	// remain byte uploads even if the channel selects public_url or text.
	if len(m.Image) > 0 && !m.ImageShareable {
		return s.Notify.Send(ctx, c, m)
	}
	mode := notificationImageMode(c)
	if m.ImageInteractive && mode != "public_url" {
		mode = "poster"
	}
	if mode == "text" || c.Type == "serverchan3" {
		m.Image = nil
		m.ImageShareable = false
	}
	if len(m.Image) == 0 {
		return s.Notify.Send(ctx, c, s.notificationText(ctx, c, m))
	}
	if mode == "separate" {
		picture := notify.Message{Recipient: m.Recipient, Image: m.Image, ImageShareable: m.ImageShareable}
		text := m
		text.Image = nil
		text.ImageShareable = false
		if text.Title == "" && text.Text == "" && len(text.Buttons) == 0 {
			return s.Notify.Send(ctx, c, picture)
		}
		return s.Notify.SendSeries(ctx, c, []notify.Message{picture, s.notificationText(ctx, c, text)})
	}
	if mode == "public_url" && m.ImageShareable {
		// Validate shape before creating a public file. Invalid recipients, oversized
		// captions and disabled channels must never cause publication side effects.
		if err := notify.ValidateMessageShape(c, m); err != nil {
			return err
		}
		base := s.notificationPublicBase(ctx, c)
		if base != "" {
			public, err := s.notificationPublishThumbnail(ctx, base, m, notificationThumbnailDefaults)
			if err == nil {
				m.Image = nil
				m.PublicImageURL = public
			} else if ctx.Err() != nil {
				return ctx.Err()
			} else {
				slog.Warn("public notification thumbnail unavailable; using private image upload")
			}
		}
	}
	// public_url for an unclassified/private image degrades to a byte upload,
	// never a publicly addressable file, including QR login and auth material.
	return s.Notify.Send(ctx, c, m)
}

func (s *Server) notificationPublicBase(ctx context.Context, c notify.Channel) string {
	return notificationPublicOrigin(s.setting(ctx, "custom_api_domain", ""), notify.Str(c.Config["webhook_base_url"]), notify.Str(c.Config["server_url"]))
}

// This selects a syntactically public HTTPS origin; it is not a DNS ownership or
// reachability check. The optional UI probe performs separate pinned-DNS I/O.
func notificationPublicOrigin(candidates ...string) string {
	for _, raw := range candidates {
		if len(raw) > 2048 {
			continue
		}
		base, err := notify.PublicDomain(raw)
		if err == nil && strings.HasPrefix(base, "https://") {
			return base
		}
	}
	return ""
}

func notificationDecodeImage(ctx context.Context, data []byte) (image.Image, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(data) == 0 || len(data) > 10<<20 {
		return nil, notify.ErrLimit
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || cfg.Width <= 0 || cfg.Height <= 0 || cfg.Width > 20000 || cfg.Height > 20000 || int64(cfg.Width)*int64(cfg.Height) > 20_000_000 {
		return nil, errors.New("notification image exceeds decoding limits")
	}
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, errors.New("notification image decoding failed")
	}
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	return img, nil
}

func (s *Server) notificationLoadImage(ctx context.Context, raw string, client libHTTPDoer) (image.Image, error) {
	if strings.HasPrefix(raw, "/data/images/") {
		u, err := url.Parse(raw)
		if err != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawPath != "" {
			return nil, errors.New("invalid managed poster path")
		}
		f, err := openWithin(filepath.Join(s.DataDir, "image"), strings.TrimPrefix(raw, "/data/images/"))
		if err != nil {
			return nil, errors.New("managed poster unavailable")
		}
		defer f.Close()
		data, err := io.ReadAll(io.LimitReader(f, (10<<20)+1))
		if err != nil {
			return nil, errors.New("managed poster read failed")
		}
		return notificationDecodeImage(ctx, data)
	}
	return notificationImage(ctx, raw, client)
}

func (s *Server) notificationPreparePoster(ctx context.Context, raw string, client libHTTPDoer) ([]byte, error) {
	select {
	case libPosterSlots <- struct{}{}:
		defer func() { <-libPosterSlots }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	img, err := s.notificationLoadImage(ctx, raw, client)
	if err != nil {
		return nil, err
	}
	return notificationPNG(notificationScale(img, 800, 800))
}

// The dedicated public-cache directory has a finite disk budget. It contains
// only re-encoded, shareable JPEGs, never source metadata or login material.
func (s *Server) notificationPublishThumbnail(ctx context.Context, base string, m notify.Message, limits notificationThumbnailLimits) (string, error) {
	if !m.ImageShareable || m.PublicImageURL != "" || len(m.Image) == 0 {
		return "", errors.New("private or absent image cannot be published")
	}
	origin, err := notify.PublicDomain(base)
	if err != nil || !strings.HasPrefix(origin, "https://") {
		return "", errors.New("public thumbnail requires a valid HTTPS origin")
	}
	select {
	case libPosterSlots <- struct{}{}:
		defer func() { <-libPosterSlots }()
	case <-ctx.Done():
		return "", ctx.Err()
	}
	img, err := notificationDecodeImage(ctx, m.Image)
	if err != nil {
		return "", err
	}
	thumb := notificationScale(img, 500, 2000)
	flat := image.NewRGBA(thumb.Bounds())
	draw.Draw(flat, flat.Bounds(), image.NewUniform(color.White), image.Point{}, draw.Src)
	draw.Draw(flat, flat.Bounds(), thumb, image.Point{}, draw.Over)
	var encoded bytes.Buffer
	if err = jpeg.Encode(&encoded, flat, &jpeg.Options{Quality: 85}); err != nil || encoded.Len() > 2<<20 {
		return "", errors.New("public thumbnail encoding failed or exceeds limits")
	}
	data := encoded.Bytes()
	sum := sha256.Sum256(data)
	name := hex.EncodeToString(sum[:]) + ".jpg"
	public := origin + "/data/images/notification-public/" + name
	if err = notify.ValidatePublicImageURL(public); err != nil {
		return "", err
	}
	notificationThumbnailMu.Lock()
	defer notificationThumbnailMu.Unlock()
	if err = ctx.Err(); err != nil {
		return "", err
	}
	baseRoot, err := os.OpenRoot(s.DataDir)
	if err != nil {
		return "", err
	}
	defer baseRoot.Close()
	if err = baseRoot.MkdirAll(notificationThumbnailDir, 0700); err != nil {
		return "", err
	}
	root, err := baseRoot.OpenRoot(notificationThumbnailDir)
	if err != nil {
		return "", err
	}
	defer root.Close()
	if err = notificationPruneThumbnails(ctx, root, name, data, limits); err != nil {
		return "", err
	}
	if old, err := root.Lstat(name); err == nil {
		if !old.Mode().IsRegular() {
			return "", errors.New("public thumbnail target is not a regular file")
		}
		if err = notificationVerifyThumbnail(root, name, data); err != nil {
			return "", err
		}
		if err = root.Chtimes(name, time.Now(), time.Now()); err != nil {
			return "", err
		}
		return public, nil
	} else if !os.IsNotExist(err) {
		return "", err
	}
	pending := ".thumbnail-" + randomID()
	f, err := root.OpenFile(pending, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return "", err
	}
	defer root.Remove(pending)
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return "", err
	}
	if err = ctx.Err(); err != nil {
		return "", err
	}
	if err = root.Link(pending, name); err != nil {
		return "", err
	}
	dir, err := root.Open(".")
	if err != nil {
		return "", err
	}
	defer dir.Close()
	if err = dir.Sync(); err != nil {
		return "", err
	}
	if err = root.Remove(pending); err != nil {
		return "", err
	}
	if err = dir.Sync(); err != nil {
		return "", err
	}
	return public, nil
}

func notificationVerifyThumbnail(root *os.Root, name string, expected []byte) error {
	// Lstat + the anchored nonblocking regular-file helper rejects links/FIFOs
	// before content verification. It cannot expose or remove changed content.
	info, err := root.Lstat(name)
	if err != nil || !info.Mode().IsRegular() || info.Size() > 2<<20 {
		return errors.New("public thumbnail is not a bounded regular file")
	}
	f, err := root.OpenFile(name, regularReadFlags, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	actual, err := f.Stat()
	if err != nil || !actual.Mode().IsRegular() || !os.SameFile(info, actual) {
		return errors.New("public thumbnail changed during open")
	}
	b, err := io.ReadAll(io.LimitReader(f, (2<<20)+1))
	if err != nil || len(b) > 2<<20 {
		return errors.New("public thumbnail read failed")
	}
	sum := sha256.Sum256(b)
	if name != hex.EncodeToString(sum[:])+".jpg" || (expected != nil && !bytes.Equal(expected, b)) {
		return errors.New("public thumbnail content changed")
	}
	return nil
}

func notificationPruneThumbnails(ctx context.Context, root *os.Root, target string, data []byte, limits notificationThumbnailLimits) error {
	if limits.count < 1 || limits.count > 512 || limits.bytes < int64(len(data)) || limits.bytes > 64<<20 || limits.ttl <= 0 {
		return notify.ErrLimit
	}
	f, err := root.Open(".")
	if err != nil {
		return err
	}
	items, readErr := f.ReadDir(514)
	f.Close()
	if readErr != nil && !errors.Is(readErr, io.EOF) {
		return readErr
	}
	if len(items) > 513 {
		return errors.New("public thumbnail directory exceeds its entry bound")
	}
	type item struct {
		name     string
		size     int64
		modified time.Time
	}
	entries := []item{}
	var used int64
	hasTarget := false
	for _, entry := range items {
		if !notificationThumbnailName.MatchString(entry.Name()) {
			return errors.New("unexpected public thumbnail directory entry; operator review required")
		}
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() || info.Size() < 1 || info.Size() > 2<<20 {
			return errors.New("invalid public thumbnail directory entry")
		}
		entries = append(entries, item{entry.Name(), info.Size(), info.ModTime()})
		used += info.Size()
		hasTarget = hasTarget || entry.Name() == target
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].modified.Before(entries[j].modified) })
	count := len(entries)
	extra := int64(len(data))
	if hasTarget {
		extra = 0
	} else {
		count++
	}
	for _, item := range entries {
		if item.name == target {
			continue
		}
		if time.Since(item.modified) < limits.ttl && count <= limits.count && used+extra <= limits.bytes {
			continue
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := notificationVerifyThumbnail(root, item.name, nil); err != nil {
			return err
		}
		if err := root.Remove(item.name); err != nil {
			return err
		}
		count--
		used -= item.size
	}
	if count > limits.count || used+extra > limits.bytes {
		return fmt.Errorf("%w: public thumbnail disk budget", notify.ErrLimit)
	}
	return nil
}

// Serving is deliberately narrower than the general cover-image mount. Hidden
// staging files, expired thumbnails and changed bytes are never public responses.
func (s *Server) notificationServeThumbnail(w http.ResponseWriter, r *http.Request) bool {
	const prefix = "/data/images/notification-public/"
	if !strings.HasPrefix(r.URL.Path, prefix) {
		return false
	}
	select {
	case notificationThumbnailServeSlots <- struct{}{}:
		defer func() { <-notificationThumbnailServeSlots }()
	case <-r.Context().Done():
		return true
	}
	name := strings.TrimPrefix(r.URL.Path, prefix)
	if !notificationThumbnailName.MatchString(name) || r.URL.RawPath != "" {
		http.NotFound(w, r)
		return true
	}
	root, err := os.OpenRoot(s.DataDir)
	if err != nil {
		http.NotFound(w, r)
		return true
	}
	defer root.Close()
	dir, err := root.OpenRoot(notificationThumbnailDir)
	if err != nil {
		http.NotFound(w, r)
		return true
	}
	defer dir.Close()
	info, err := dir.Lstat(name)
	if err != nil || !info.Mode().IsRegular() || info.Size() > 2<<20 {
		http.NotFound(w, r)
		return true
	}
	expires := info.ModTime().Add(notificationThumbnailDefaults.ttl)
	if !time.Now().Before(expires) {
		http.NotFound(w, r)
		return true
	}
	f, err := dir.OpenFile(name, regularReadFlags, 0)
	if err != nil {
		http.NotFound(w, r)
		return true
	}
	defer f.Close()
	actual, err := f.Stat()
	if err != nil || !actual.Mode().IsRegular() || !os.SameFile(info, actual) {
		http.NotFound(w, r)
		return true
	}
	data, err := io.ReadAll(io.LimitReader(f, (2<<20)+1))
	sum := sha256.Sum256(data)
	if err != nil || len(data) > 2<<20 || hex.EncodeToString(sum[:])+".jpg" != name {
		http.NotFound(w, r)
		return true
	}
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("ETag", `"`+strings.TrimSuffix(name, ".jpg")+`"`)
	w.Header().Set("Cache-Control", "public, max-age="+strconv.FormatInt(max(0, min(3600, int64(time.Until(expires)/time.Second))), 10))
	http.ServeContent(w, r, name, info.ModTime(), bytes.NewReader(data))
	return true
}
