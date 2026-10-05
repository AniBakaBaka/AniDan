// SPDX-License-Identifier: AGPL-3.0-only
package danmaku

import (
	"bufio"
	"bytes"
	"embed"
	"fmt"
	"strings"
	"sync"
	"unicode/utf8"
)

//go:embed dictionary/*.txt
var dictionaryFiles embed.FS

type converter struct {
	once     sync.Once
	entries  map[string]string
	maxBytes int
	maxFirst [256]int
	err      error
}

var chineseConverters [2]converter

// Convert uses all entries in the pinned OpenCC s2t/t2s phrase and character
// dictionaries with longest-phrase matching. 0 preserves input, 1 converts
// traditional to simplified, 2 simplified to traditional. Other regional
// conversions and native OpenCC's complete segmentation pipeline are not claimed.
// Initialization is lazy, dictionaries immutable, and all request work bounded.
func Convert(text string, mode int) (string, error) {
	if mode < 0 || mode > 2 {
		return "", fmt.Errorf("unsupported Chinese conversion mode %d", mode)
	}
	if len(text) > DefaultParseOptions().MaxTextBytes {
		return "", ErrLimit
	}
	if !utf8.ValidString(text) {
		return "", fmt.Errorf("invalid UTF-8 for conversion")
	}
	if mode == 0 {
		return text, nil
	}
	c := &chineseConverters[mode-1]
	c.once.Do(func() {
		prefix := "TS"
		if mode == 2 {
			prefix = "ST"
		}
		c.entries = map[string]string{}
		for _, kind := range []string{"Characters", "Phrases"} {
			data, e := dictionaryFiles.ReadFile("dictionary/" + prefix + kind + ".txt")
			if e != nil {
				c.err = e
				return
			}
			scan := bufio.NewScanner(bytes.NewReader(data))
			scan.Buffer(make([]byte, 4096), 1<<20)
			for scan.Scan() {
				line := scan.Text()
				if line == "" || strings.HasPrefix(line, "#") {
					continue
				}
				key, value, ok := strings.Cut(line, "\t")
				values := strings.Fields(value)
				if !ok {
					fields := strings.Fields(line)
					if len(fields) >= 2 {
						key = fields[0]
						values = fields[1:]
						ok = true
					}
				}
				if !ok || key == "" || len(values) == 0 {
					c.err = fmt.Errorf("invalid embedded conversion dictionary entry")
					return
				}
				c.entries[key] = values[0]
				c.maxBytes = max(c.maxBytes, len(key))
				c.maxFirst[key[0]] = max(c.maxFirst[key[0]], len(key))
			}
			if e = scan.Err(); e != nil {
				c.err = e
				return
			}
		}
	})
	if c.err != nil {
		return "", c.err
	}
	var out strings.Builder
	out.Grow(len(text))
	for i := 0; i < len(text); {
		end := min(len(text), i+c.maxFirst[text[i]])
		value := ""
		matched := i
		for j := end; j > i; j-- {
			if j < len(text) && !utf8.RuneStart(text[j]) {
				continue
			}
			if v, ok := c.entries[text[i:j]]; ok {
				value = v
				matched = j
				break
			}
		}
		if matched == i {
			_, size := utf8.DecodeRuneInString(text[i:])
			matched = i + size
			value = text[i:matched]
		}
		if out.Len()+len(value) > DefaultParseOptions().MaxTextBytes {
			return "", ErrLimit
		}
		out.WriteString(value)
		i = matched
	}
	return out.String(), nil
}
