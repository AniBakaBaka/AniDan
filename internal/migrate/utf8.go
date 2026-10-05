// SPDX-License-Identifier: AGPL-3.0-only
package migrate

import (
	"errors"
	"io"
	"unicode/utf8"
)

// encoding/json substitutes invalid UTF-8 with U+FFFD. Migration must reject
// corrupt input instead of silently changing stored strings.
type strictUTF8Reader struct {
	reader io.Reader
	tail   []byte
}

func (r *strictUTF8Reader) Read(p []byte) (int, error) {
	n, e := r.reader.Read(p)
	buf := p[:n]
	if len(r.tail) > 0 {
		all := make([]byte, 0, len(r.tail)+n)
		all = append(all, r.tail...)
		buf = append(all, buf...)
		r.tail = nil
	}
	for len(buf) > 0 {
		if !utf8.FullRune(buf) {
			r.tail = append([]byte(nil), buf...)
			break
		}
		rune, size := utf8.DecodeRune(buf)
		if rune == utf8.RuneError && size == 1 {
			return 0, errors.New("snapshot contains invalid UTF-8; refusing lossy decoding")
		}
		buf = buf[size:]
	}
	if e == io.EOF && len(r.tail) > 0 {
		return 0, errors.New("snapshot ends with incomplete UTF-8")
	}
	return n, e
}
