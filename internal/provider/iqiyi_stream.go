// SPDX-License-Identifier: AGPL-3.0-only
package provider

import (
	"bytes"
	"compress/zlib"
	"context"
	"encoding/xml"
	"errors"
	"io"
	"strings"

	"github.com/AniBakaBaka/AniDan/internal/danmaku"
)

// Canonical output retains the existing MaxDownloadBytes budget. The additional
// compressed-input window has allowance max(MaxBytes, MaxDownloadBytes), with a
// full MaxBytes reservation per worker. A single valid response always fits;
// concurrency therefore cannot cause a new canonical-output budget rejection.
// Defaults32/64MiB admit two workers. This is tracked payload accounting, not an
// RSS promise: Go backing-array growth, TLS, XML token scratch and GC add costs.
func (l *Legacy) iqiyiFetchWorkers() int {
	response := l.MaxBytes
	if response <= 0 {
		response = 32 << 20
	}
	allowance := l.MaxDownloadBytes
	if allowance <= 0 {
		allowance = 64 << 20
	}
	if allowance < response {
		allowance = response
	}
	workers := l.SegmentWorkers
	if workers < 1 {
		workers = 4
	}
	return min(workers, 4, int(min(allowance/response, 4)))
}

type iqiyiContextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r iqiyiContextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	// Keep cancellation checkpoints during inflation even for a large token.
	if len(p) > 32<<10 {
		p = p[:32<<10]
	}
	n, e := r.r.Read(p)
	if err := r.ctx.Err(); err != nil {
		return n, err
	}
	return n, e
}
func (l *Legacy) scanIQiyiSegment(ctx context.Context, raw []byte, emit func(danmaku.Comment) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	zr, e := zlib.NewReader(iqiyiContextReader{ctx, bytes.NewReader(raw)})
	if e != nil {
		return e
	}
	defer zr.Close()
	maximum := l.MaxBytes
	if maximum <= 0 {
		maximum = 32 << 20
	}
	if maximum >= int64(^uint64(0)>>1) {
		return danmaku.ErrLimit
	}
	limited := &io.LimitedReader{R: iqiyiContextReader{ctx, zr}, N: maximum + 1}
	// Peek/remove optional UTF8 BOM without materializing the whole XML document.
	var prefix [3]byte
	n, e := io.ReadFull(limited, prefix[:])
	if e != nil && e != io.EOF && e != io.ErrUnexpectedEOF {
		return e
	}
	var reader io.Reader = io.MultiReader(bytes.NewReader(prefix[:n]), limited)
	if n == 3 && bytes.Equal(prefix[:], []byte{0xef, 0xbb, 0xbf}) {
		reader = limited
	}
	return scanIQiyiXML(ctx, reader, limited, emit)
}
func parseIQiyiXML(raw []byte) ([]danmaku.Comment, error) {
	out := []danmaku.Comment{}
	err := scanIQiyiXML(context.Background(), bytes.NewReader(bytes.TrimPrefix(raw, []byte{0xef, 0xbb, 0xbf})), nil, func(c danmaku.Comment) error { out = append(out, c); return nil })
	if err != nil {
		return nil, err
	}
	return out, nil
}

type iqiyiXMLScanner struct {
	ctx   context.Context
	d     *xml.Decoder
	limit *io.LimitedReader
	depth int
	root  bool
}

func (s *iqiyiXMLScanner) token() (xml.Token, error) {
	if e := s.ctx.Err(); e != nil {
		return nil, e
	}
	tok, e := s.d.Token()
	if s.limit != nil && s.limit.N <= 0 {
		return nil, danmaku.ErrLimit
	}
	if err := s.ctx.Err(); err != nil {
		return nil, err
	}
	if e != nil {
		return nil, e
	}
	switch x := tok.(type) {
	case xml.Directive:
		return nil, errors.New("iqiyi XML directive rejected")
	case xml.StartElement:
		s.depth++
		if s.depth > 32 {
			return nil, danmaku.ErrLimit
		}
		if s.depth == 1 {
			if s.root || x.Name.Local != "danmu" {
				return nil, errors.New("iqiyi XML requires danmu root")
			}
			s.root = true
		}
	case xml.EndElement:
		s.depth--
	}
	return tok, nil
}
func scanIQiyiXML(ctx context.Context, r io.Reader, limit *io.LimitedReader, emit func(danmaku.Comment) error) error {
	s := iqiyiXMLScanner{ctx: ctx, d: xml.NewDecoder(iqiyiContextReader{ctx, r}), limit: limit}
	count := 0
	for {
		tok, e := s.token()
		if e == io.EOF {
			break
		}
		if e != nil {
			return e
		}
		if x, ok := tok.(xml.StartElement); ok && x.Name.Local == "bulletInfo" {
			c, e := s.item()
			if e != nil {
				return e
			}
			if count >= 500000 {
				return danmaku.ErrLimit
			}
			count++
			if e = emit(c); e != nil {
				return e
			}
		}
	}
	if !s.root || s.depth != 0 {
		return errors.New("invalid iqiyi XML")
	}
	return ctx.Err()
}
func (s *iqiyiXMLScanner) item() (danmaku.Comment, error) {
	itemDepth := s.depth
	fields := map[string]string{}
	active := ""
	activeDepth := 0
	contentTooLarge := false
	finalContentTooLarge := false
	var value strings.Builder
	for {
		tok, e := s.token()
		if e != nil {
			return danmaku.Comment{}, e
		}
		switch x := tok.(type) {
		case xml.StartElement:
			if s.depth == itemDepth+1 {
				switch x.Name.Local {
				case "contentId", "content", "showTime", "color":
					active = x.Name.Local
					activeDepth = s.depth
					contentTooLarge = false
					value.Reset()
				}
			}
		case xml.CharData:
			if active != "" && s.depth == activeDepth {
				if active == "content" && (contentTooLarge || len(x) > (64<<10)-value.Len()) {
					// encoding/xml keeps the last repeated field. Remember an oversized
					// content field without retaining it; a later content may replace it.
					contentTooLarge = true
				} else {
					value.Write(x)
				}
			}
		case xml.EndElement:
			if active != "" && s.depth == activeDepth-1 {
				fields[active] = value.String()
				if active == "content" {
					finalContentTooLarge = contentTooLarge
				}
				active = ""
			}
			if s.depth == itemDepth-1 {
				if finalContentTooLarge {
					return danmaku.Comment{}, danmaku.ErrLimit
				}
				color := 0xffffff
				if fields["color"] != "" {
					color, e = danmaku.ParseColor("#" + fields["color"])
					if e != nil {
						return danmaku.Comment{}, e
					}
				}
				return comment(fields["contentId"], number(fields["showTime"]), 1, color, fields["content"], "iqiyi")
			}
		}
	}
}
