// SPDX-License-Identifier: AGPL-3.0-only
package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"strconv"
	"strings"
	"sync"
)

const exactIDsHeader = "X-AniDan-Exact-IDs"
const exactIDsVersion = "decimal-string-v1"

var rawJSONType = reflect.TypeFor[json.RawMessage]()
var jsonUnmarshalerType = reflect.TypeFor[json.Unmarshaler]()

// decodeExactJSON accepts decimal strings only where the destination schema
// expects an integer. It never guesses from names or converts string fields,
// opaque RawMessage documents, floats, booleans or interface/map values.
// The extra pass is reserved for explicitly marked UI requests and durable job
// parameter boundaries; ordinary requests retain the existing single decoder.
func decodeExactJSON(reader io.Reader, target any) error {
	t := reflect.TypeOf(target)
	if t == nil || t.Kind() != reflect.Pointer || reflect.ValueOf(target).IsNil() {
		return errors.New("JSON destination must be a nonnil pointer")
	}
	decoder := json.NewDecoder(reader)
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return errors.New("request must contain one JSON value")
	}
	value, err := normalizeIntegerStrings(value, t.Elem(), 0)
	if err != nil {
		return err
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return err
	}
	final := json.NewDecoder(bytes.NewReader(encoded))
	final.UseNumber()
	return final.Decode(target)
}

func unmarshalExactJSON(raw []byte, target any) error {
	return decodeExactJSON(bytes.NewReader(raw), target)
}

func normalizeIntegerStrings(value any, target reflect.Type, depth int) (any, error) {
	if value == nil || target == nil || target == rawJSONType {
		return value, nil
	}
	// Custom decoders own their wire contract, including integer-backed enums.
	if target.Implements(jsonUnmarshalerType) || (target.Kind() != reflect.Pointer && reflect.PointerTo(target).Implements(jsonUnmarshalerType)) {
		return value, nil
	}
	if depth > 128 {
		return nil, errors.New("typed JSON nesting exceeds the supported limit")
	}
	for target.Kind() == reflect.Pointer {
		target = target.Elem()
		if target == rawJSONType {
			return value, nil
		}
	}
	switch target.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		text, ok := value.(string)
		if !ok {
			return value, nil
		}
		// No exponent, decimal point, whitespace, truncation or float conversion.
		if len(text) == 0 || len(text) > 128 {
			return nil, errors.New("integer string has invalid length")
		}
		start := 0
		if text[0] == '-' || text[0] == '+' {
			start = 1
		}
		if start == len(text) {
			return nil, errors.New("integer string is not decimal")
		}
		for i := start; i < len(text); i++ {
			if text[i] < '0' || text[i] > '9' {
				return nil, errors.New("integer string is not decimal")
			}
		}
		if target.Kind() >= reflect.Uint && target.Kind() <= reflect.Uint64 {
			n, err := strconv.ParseUint(strings.TrimPrefix(text, "+"), 10, target.Bits())
			if err != nil {
				return nil, errors.New("integer string is outside the destination range")
			}
			return json.Number(strconv.FormatUint(n, 10)), nil
		}
		n, err := strconv.ParseInt(text, 10, target.Bits())
		if err != nil {
			return nil, errors.New("integer string is outside the destination range")
		}
		return json.Number(strconv.FormatInt(n, 10)), nil
	case reflect.Struct:
		object, ok := value.(map[string]any)
		if !ok {
			return value, nil
		}
		fields := exactJSONFields(target)
		seenFields := make(map[string]bool)
		for key, child := range object {
			field, ok := fields[key]
			resolvedName := key
			if !ok {
				for name, candidate := range fields {
					if !candidate.ambiguous && strings.EqualFold(name, key) {
						if ok {
							return nil, errors.New("ambiguous case-insensitive JSON field; use its exact declared name")
						}
						field, ok = candidate, true
						resolvedName = name
					}
				}
			}
			if !ok || field.quoted || field.ambiguous {
				continue
			}
			if seenFields[resolvedName] {
				return nil, errors.New("duplicate JSON aliases for the same declared field")
			}
			seenFields[resolvedName] = true
			var err error
			object[key], err = normalizeIntegerStrings(child, field.typ, depth+1)
			if err != nil {
				return nil, err
			}
		}
	case reflect.Slice, reflect.Array:
		array, ok := value.([]any)
		if !ok {
			return value, nil
		}
		for i, child := range array {
			var err error
			array[i], err = normalizeIntegerStrings(child, target.Elem(), depth+1)
			if err != nil {
				return nil, err
			}
		}
	case reflect.Map:
		object, ok := value.(map[string]any)
		if !ok {
			return value, nil
		}
		for key, child := range object {
			var err error
			object[key], err = normalizeIntegerStrings(child, target.Elem(), depth+1)
			if err != nil {
				return nil, err
			}
		}
	}
	return value, nil
}

type exactJSONField struct {
	typ                       reflect.Type
	depth                     int
	tagged, quoted, ambiguous bool
}

var exactJSONFieldCache sync.Map // finite application DTO types, never user schemas

func exactJSONFields(target reflect.Type) map[string]exactJSONField {
	if fields, ok := exactJSONFieldCache.Load(target); ok {
		return fields.(map[string]exactJSONField)
	}
	type pending struct {
		typ   reflect.Type
		depth int
	}
	queue := []pending{{typ: target}}
	seen := map[reflect.Type]int{}
	fields := map[string]exactJSONField{}
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		if depth, ok := seen[current.typ]; ok && depth < current.depth {
			continue
		}
		seen[current.typ] = current.depth
		for i := 0; i < current.typ.NumField(); i++ {
			field := current.typ.Field(i)
			if field.PkgPath != "" && !field.Anonymous {
				continue
			}
			tag := strings.Split(field.Tag.Get("json"), ",")
			name := tag[0]
			if name == "-" {
				continue
			}
			base := field.Type
			for base.Kind() == reflect.Pointer {
				base = base.Elem()
			}
			if field.Anonymous && name == "" && base.Kind() == reflect.Struct {
				if depth, ok := seen[base]; !ok || depth >= current.depth+1 {
					queue = append(queue, pending{typ: base, depth: current.depth + 1})
				}
				continue
			}
			if field.PkgPath != "" {
				continue
			}
			tagged := name != ""
			if name == "" {
				name = field.Name
			}
			candidate := exactJSONField{typ: field.Type, depth: current.depth, tagged: tagged}
			for _, option := range tag[1:] {
				candidate.quoted = candidate.quoted || option == "string"
			}
			old, ok := fields[name]
			if !ok || candidate.depth < old.depth || (candidate.depth == old.depth && candidate.tagged && !old.tagged) {
				fields[name] = candidate
			} else if candidate.depth == old.depth && candidate.tagged == old.tagged {
				old.ambiguous = true
				fields[name] = old
			}
		}
	}
	actual, _ := exactJSONFieldCache.LoadOrStore(target, fields)
	return actual.(map[string]exactJSONField)
}
