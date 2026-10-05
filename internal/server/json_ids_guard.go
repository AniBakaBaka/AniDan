// SPDX-License-Identifier: AGPL-3.0-only
package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"net/http"
	"strings"
)

var errLegacyLargeUIID = errors.New("large-ID UI mutations require the current exact-identifier client; close and reload old pages")
var exactJSSafeInteger = big.NewInt(9007199254740991)

func isUIMutation(r *http.Request) bool {
	if !strings.HasPrefix(r.URL.Path, "/api/ui/") {
		return false
	}
	switch r.Method {
	case "POST", "PUT", "PATCH", "DELETE":
		return true
	}
	return false
}
func exactIDKey(key string) bool {
	switch key {
	case "id", "ids", "cid", "aid", "bvid", "vid", "pid", "mid", "gid", "uid", "egid":
		return true
	}
	return strings.HasSuffix(key, "Id") || strings.HasSuffix(key, "Ids") || strings.HasSuffix(key, "ID") || strings.HasSuffix(key, "IDs") || strings.HasSuffix(strings.ToLower(key), "_id") || strings.HasSuffix(strings.ToLower(key), "_ids")
}
func unsafeUIID(value any) bool {
	switch n := value.(type) {
	case json.Number:
		// Integer IDs have no exponent/fraction grammar. Reject that notation
		// before big-number parsing: a tiny exponent token can request enormous
		// allocation even when its source text is short.
		text := string(n)
		if len(text) == 0 || len(text) > 256 {
			return true
		}
		start := 0
		if text[0] == '-' {
			start = 1
		}
		if start == len(text) {
			return true
		}
		for _, ch := range text[start:] {
			if ch < '0' || ch > '9' {
				return true
			}
		}
		v, ok := new(big.Int).SetString(text, 10)
		return !ok || v.Abs(v).Cmp(exactJSSafeInteger) > 0
	case string:
		// Opaque IDs, text, credentials and digit substrings are never guessed.
		if n == "" {
			return false
		}
		start := 0
		if n[0] == '-' || n[0] == '+' {
			start = 1
		}
		if start == len(n) {
			return false
		}
		for _, ch := range n[start:] {
			if ch < '0' || ch > '9' {
				return false
			}
		}
		if len(n) > 256 {
			return true
		}
		v, ok := new(big.Int).SetString(n, 10)
		return ok && v.Abs(v).Cmp(exactJSSafeInteger) > 0
	}
	return false
}
func hasUnsafeUIIDs(value any, identifier bool) bool {
	switch v := value.(type) {
	case map[string]any:
		for key, child := range v {
			if hasUnsafeUIIDs(child, exactIDKey(key)) {
				return true
			}
		}
	case []any:
		for _, child := range v {
			if hasUnsafeUIIDs(child, identifier) {
				return true
			}
		}
	default:
		return identifier && unsafeUIID(v)
	}
	return false
}

// Only the obsolete/unmarked UI write path pays this defensive extra pass.
// Control/player contracts and marked exact-ID requests are unchanged. Preserve
// the original bytes for normal decoding and custom unmarshaler semantics.
func decodeLegacyUIJSON(reader io.Reader, target any) error {
	decoder := json.NewDecoder(reader)
	var raw json.RawMessage
	if err := decoder.Decode(&raw); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return errors.New("request must contain one JSON value")
	}
	audit := json.NewDecoder(bytes.NewReader(raw))
	audit.UseNumber()
	var value any
	if err := audit.Decode(&value); err != nil {
		return err
	}
	if hasUnsafeUIIDs(value, false) {
		return errLegacyLargeUIID
	}
	final := json.NewDecoder(bytes.NewReader(raw))
	final.UseNumber()
	return final.Decode(target)
}

func (s *Server) guardLegacyUIIdentifiers(r *http.Request) error {
	if !isUIMutation(r) {
		return nil
	}
	if marker := r.Header.Get(exactIDsHeader); marker != "" {
		if marker != exactIDsVersion {
			return errors.New("unsupported exact identifier request format")
		}
		return nil
	}
	for key, values := range r.URL.Query() {
		if exactIDKey(key) {
			for _, v := range values {
				if unsafeUIID(v) {
					return errLegacyLargeUIID
				}
			}
		}
	}
	// Use actual route wildcard names, not arbitrary numeric path substrings.
	_, pattern := s.mux.Handler(r)
	if _, rest, ok := strings.Cut(pattern, " "); ok {
		pattern = rest
	}
	route := strings.Split(strings.Trim(pattern, "/"), "/")
	actual := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	for i, part := range route {
		if i >= len(actual) {
			break
		}
		if strings.HasPrefix(part, "{") && strings.HasSuffix(part, "}") && exactIDKey(strings.Trim(part, "{}")) && unsafeUIID(actual[i]) {
			return errLegacyLargeUIID
		}
	}
	// The only compatibility dispatcher hiding an ID inside a rest wildcard.
	if pattern == "/api/ui/anime/{rest...}" && len(actual) == 5 && actual[4] == "group" && unsafeUIID(actual[3]) {
		return errLegacyLargeUIID
	}
	return nil
}
