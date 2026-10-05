// SPDX-License-Identifier: AGPL-3.0-only
// Format compatibility informed by misaka_danmu_server (AGPL-3.0),
// l429609201 and contributors. See LICENSES/provenance/PROVIDERS.md for provenance.
package danmaku

import (
	"bufio"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
	"unicode/utf8"
)

type Comment struct {
	CID int64   `json:"cid"`
	P   string  `json:"p"`
	M   string  `json:"m"`
	T   float64 `json:"t"`
}

var ErrLimit = errors.New("danmaku resource limit exceeded")

type ParseOptions struct {
	MaxBytes                  int64
	MaxComments, MaxTextBytes int
	Source                    string
	StrictNodes               bool
}

func DefaultParseOptions() ParseOptions {
	return ParseOptions{MaxBytes: 64 << 20, MaxComments: 1_000_000, MaxTextBytes: 64 << 10, Source: "[xml]"}
}

// Parse streams XML. Invalid comment fields are skipped; broken XML is an error.
// Invalid XML 1.0 control characters are removed, matching the legacy sanitizer.
// The byte, individual comment and count bounds apply even to malformed input.
func Parse(r io.Reader) ([]Comment, error) { return ParseWithOptions(r, DefaultParseOptions()) }
func ParseWithOptions(r io.Reader, o ParseOptions) ([]Comment, error) {
	out := make([]Comment, 0)
	_, err := scanXML(r, o, func(c Comment) { out = append(out, c) })
	if err != nil {
		return nil, err
	}
	return out, nil
}

// InspectWithOptions validates and counts comments using the same rules as
// ParseWithOptions, without retaining the comments. It returns zero on error.
func InspectWithOptions(r io.Reader, o ParseOptions) (int, error) {
	return scanXML(r, o, nil)
}

func scanXML(r io.Reader, o ParseOptions, emit func(Comment)) (int, error) {
	d := DefaultParseOptions()
	if o.MaxBytes <= 0 {
		o.MaxBytes = d.MaxBytes
	}
	if o.MaxComments <= 0 {
		o.MaxComments = d.MaxComments
	}
	if o.MaxTextBytes <= 0 {
		o.MaxTextBytes = d.MaxTextBytes
	}
	if o.Source == "" {
		o.Source = d.Source
	}
	lr := &io.LimitedReader{R: r, N: o.MaxBytes + 1}
	dec := xml.NewDecoder(&xmlSanitizer{r: bufio.NewReader(lr)})
	count := 0
	depth := 0
	roots := 0
	for {
		tok, err := dec.Token()
		if lr.N <= 0 {
			return 0, ErrLimit
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return 0, fmt.Errorf("danmaku XML: %w", err)
		}
		switch t := tok.(type) {
		case xml.StartElement:
			depth++
			if depth > 32 {
				return 0, fmt.Errorf("%w: XML nesting", ErrLimit)
			}
			if depth == 1 {
				roots++
				if roots != 1 || t.Name.Local != "i" {
					return 0, errors.New("danmaku XML requires one i root")
				}
			}
			if depth != 2 || t.Name.Local != "d" {
				continue
			}
			p := "0,1,25,16777215"
			for _, a := range t.Attr {
				if a.Name.Local == "p" {
					p = a.Value
				}
			}
			var text strings.Builder
			nested := 0
			for {
				v, e := dec.Token()
				if lr.N <= 0 {
					return 0, ErrLimit
				}
				if e != nil {
					return 0, fmt.Errorf("danmaku node: %w", e)
				}
				if c, ok := v.(xml.CharData); ok {
					if text.Len()+len(c) > o.MaxTextBytes {
						return 0, fmt.Errorf("%w: comment text", ErrLimit)
					}
					text.Write(c)
				}
				if _, ok := v.(xml.StartElement); ok {
					nested++
					if nested > 32 {
						return 0, ErrLimit
					}
				}
				if _, ok := v.(xml.EndElement); ok {
					if nested == 0 {
						break
					}
					nested--
				}
			}
			depth--
			c, e := Normalize(Comment{P: p, M: text.String()}, o.Source)
			if e != nil {
				if o.StrictNodes {
					return 0, e
				}
				continue
			}
			if count >= o.MaxComments {
				return 0, fmt.Errorf("%w: comment count", ErrLimit)
			}
			count++
			if emit != nil {
				emit(c)
			}
		case xml.EndElement:
			depth--
		case xml.Directive:
			return 0, errors.New("XML directives and external entities are not supported")
		case xml.CharData:
			if depth == 0 && strings.TrimSpace(string(t)) != "" {
				return 0, errors.New("text outside XML root")
			}
		}
	}
	if roots != 1 || depth != 0 {
		return 0, errors.New("incomplete danmaku XML")
	}
	return count, nil
}

type xmlSanitizer struct {
	r       *bufio.Reader
	pending []byte
}

func (s *xmlSanitizer) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	n := copy(p, s.pending)
	s.pending = s.pending[n:]
	for n < len(p) {
		r, size, err := s.r.ReadRune()
		if err != nil {
			if err == io.EOF && n > 0 {
				return n, nil
			}
			return n, err
		}
		if r == utf8.RuneError && size == 1 {
			return n, errors.New("invalid UTF-8 in XML")
		}
		if !validXMLRune(r) {
			continue
		}
		if r < utf8.RuneSelf {
			p[n] = byte(r)
			n++
			continue
		}
		if len(p)-n < utf8.RuneLen(r) {
			s.pending = utf8.AppendRune(s.pending, r)
			k := copy(p[n:], s.pending)
			n += k
			s.pending = s.pending[k:]
			break
		}
		n += utf8.EncodeRune(p[n:], r)
	}
	return n, nil
}

func validXMLRune(r rune) bool {
	return r == 9 || r == 10 || r == 13 || (r >= 0x20 && r <= 0xd7ff) || (r >= 0xe000 && r <= 0xfffd) || (r >= 0x10000 && r <= 0x10ffff)
}
func cleanText(s string) string {
	return strings.Map(func(r rune) rune {
		if validXMLRune(r) {
			return r
		}
		return -1
	}, s)
}
func finite(f float64) bool { return !math.IsNaN(f) && !math.IsInf(f, 0) }

// commentFields retains only metadata used by Normalize. Counts saturate at
// eight, but source discovery still accepts markers at any field position.
type commentFields struct {
	head          [8]string
	coreN, totalN int
	source        string
}

func scanCommentFields(p, source string) commentFields {
	out := commentFields{source: source}
	if p == "" {
		return out
	}
	foundSource := false
	for {
		field, rest, more := strings.Cut(p, ",")
		if !foundSource {
			if strings.Contains(field, "[") && strings.Contains(field, "]") {
				out.source = field
				foundSource = true
			} else if out.coreN < 8 {
				out.coreN++
			}
		}
		if out.totalN < 8 {
			out.head[out.totalN] = field
			out.totalN++
		}
		// A source before field eight does not suppress the raw eighth-field CID.
		if !more || (foundSource && out.totalN == 8) {
			break
		}
		p = rest
	}
	return out
}

// Normalize recognizes Bilibili 8-field, internal 4-field + source, and
// Dandanplay 3/4-field p strings. For ambiguous low RGB values use FromPlayer.
func Normalize(c Comment, source string) (Comment, error) {
	if source == "" {
		source = "[xml]"
	}
	parts := scanCommentFields(c.P, source)
	source = parts.source
	core := parts.head[:parts.coreN]
	var tm = "0"
	mode := "1"
	font := "25"
	color := "16777215"
	if len(core) > 0 {
		tm = core[0]
	}
	if len(core) > 1 {
		mode = core[1]
	}
	if len(core) == 3 {
		color = core[2]
	}
	if len(core) >= 4 {
		font = core[2]
		color = core[3]
		f, fe := strconv.Atoi(strings.TrimSpace(font))
		col, ce := strconv.Atoi(strings.TrimSpace(color))
		if len(core) < 8 && ((fe == nil && f > 1000) || ce != nil || col > 16777215) {
			font = "25"
			color = core[2]
		}
	}
	t, e := strconv.ParseFloat(strings.TrimSpace(tm), 64)
	if e != nil || !finite(t) {
		return Comment{}, fmt.Errorf("invalid comment time %q", tm)
	}
	m, e := strconv.Atoi(strings.TrimSpace(mode))
	if e != nil || m < 1 || m > 9 {
		return Comment{}, fmt.Errorf("invalid comment mode %q", mode)
	}
	f, e := strconv.Atoi(strings.TrimSpace(font))
	if e != nil || f <= 0 || f > 1000 {
		f = 25
	}
	col, e := ParseColor(color)
	if e != nil {
		return Comment{}, e
	}
	if parts.totalN >= 8 && c.CID == 0 {
		c.CID, _ = strconv.ParseInt(parts.head[7], 10, 64)
	}
	c.T = t
	c.M = cleanText(c.M)
	c.P = formatTime(t) + "," + strconv.Itoa(m) + "," + strconv.Itoa(f) + "," + strconv.Itoa(col) + "," + source
	return c, nil
}
func FromPlayer(c Comment, source string) (Comment, error) {
	p := strings.Split(c.P, ",")
	if len(p) < 3 {
		return Comment{}, errors.New("player p requires time, mode, color")
	}
	c.P = p[0] + "," + p[1] + ",25," + p[2]
	return Normalize(c, source)
}
func formatTime(t float64) string { return strconv.FormatFloat(t, 'f', -1, 64) }
func ParseColor(s string) (int, error) {
	s = strings.TrimSpace(s)
	base := 10
	if strings.HasPrefix(s, "#") {
		s = s[1:]
		base = 16
	} else if strings.HasPrefix(strings.ToLower(s), "0x") {
		s = s[2:]
		base = 16
	}
	n, e := strconv.ParseInt(s, base, 32)
	if e != nil || n < 0 || n > 0xffffff {
		return 0, fmt.Errorf("invalid RGB color %q", s)
	}
	return int(n), nil
}

// Write streams canonical XML and escapes both text and attributes. Raw legacy
// file migration must copy bytes, rather than using this normalizing writer.
func Write(w io.Writer, comments []Comment) error {
	w = &limitedWriter{w: w, remaining: DefaultParseOptions().MaxBytes}
	if len(comments) > DefaultParseOptions().MaxComments {
		return ErrLimit
	}
	if _, e := io.WriteString(w, xml.Header); e != nil {
		return e
	}
	enc := xml.NewEncoder(w)
	root := xml.StartElement{Name: xml.Name{Local: "i"}}
	if e := enc.EncodeToken(root); e != nil {
		return e
	}
	for _, c := range comments {
		if len(c.M) > DefaultParseOptions().MaxTextBytes {
			return ErrLimit
		}
		n, e := Normalize(c, "[xml]")
		if e != nil {
			return e
		}
		d := xml.StartElement{Name: xml.Name{Local: "d"}, Attr: []xml.Attr{{Name: xml.Name{Local: "p"}, Value: n.P}}}
		if e = enc.EncodeToken(d); e != nil {
			return e
		}
		if e = enc.EncodeToken(xml.CharData(n.M)); e != nil {
			return e
		}
		if e = enc.EncodeToken(d.End()); e != nil {
			return e
		}
	}
	if e := enc.EncodeToken(root.End()); e != nil {
		return e
	}
	return enc.Flush()
}

type limitedWriter struct {
	w         io.Writer
	remaining int64
}

func (w *limitedWriter) Write(p []byte) (int, error) {
	if int64(len(p)) > w.remaining {
		return 0, ErrLimit
	}
	n, e := w.w.Write(p)
	w.remaining -= int64(n)
	if e == nil && n != len(p) {
		e = io.ErrShortWrite
	}
	return n, e
}

// NewComment constructs canonical internal fields directly from a decoded
// provider record, avoiding a redundant format-then-parse cycle.
func NewComment(cid int64, seconds float64, mode, font, color int, message, source string) (Comment, error) {
	if !finite(seconds) {
		return Comment{}, errors.New("non-finite comment time")
	}
	if mode < 1 || mode > 9 {
		return Comment{}, errors.New("invalid comment mode")
	}
	if font <= 0 || font > 1000 {
		font = 25
	}
	if color < 0 || color > 0xffffff {
		return Comment{}, errors.New("invalid RGB color")
	}
	if len(message) > DefaultParseOptions().MaxTextBytes {
		return Comment{}, ErrLimit
	}
	if source == "" {
		source = "[xml]"
	}
	return Comment{CID: cid, T: seconds, P: formatTime(seconds) + "," + strconv.Itoa(mode) + "," + strconv.Itoa(font) + "," + strconv.Itoa(color) + "," + source, M: cleanText(message)}, nil
}
