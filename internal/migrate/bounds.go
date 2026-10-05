// SPDX-License-Identifier: AGPL-3.0-only
package migrate

import (
	"fmt"
	"io"
)

// rowBoundReader limits each data row before the JSON decoder can allocate an
// unbounded token or object. It tracks JSON strings/escapes and structural depth,
// without interpreting or rewriting any data. The envelope/data/array/row layout
// has depth four. Metadata remains subject to the total decompressed-byte limit.
type rowBoundReader struct {
	reader         io.Reader
	max            int64
	depth          int
	quoted, escape bool
	offset, start  int64
	active         bool
}

func (r *rowBoundReader) Read(p []byte) (int, error) {
	n, e := r.reader.Read(p)
	for i, b := range p[:n] {
		r.offset++
		if r.active && r.offset-r.start > r.max {
			return i, fmt.Errorf("snapshot row/object exceeds %d-byte limit", r.max)
		}
		if r.quoted {
			if r.escape {
				r.escape = false
			} else if b == '\\' {
				r.escape = true
			} else if b == '"' {
				r.quoted = false
			}
			continue
		}
		switch b {
		case '"':
			r.quoted = true
		case '{', '[':
			r.depth++
			if !r.active && r.depth >= 4 {
				r.active = true
				r.start = r.offset
			}
		case '}', ']':
			r.depth--
			if r.active && r.depth < 4 {
				r.active = false
			}
		}
	}
	return n, e
}
