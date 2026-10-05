// SPDX-License-Identifier: AGPL-3.0-only
package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/AniBakaBaka/AniDan/internal/cachebackend"
)

// Cache configures only disposable response caches. Workflow state, player
// identities and notification receipts/cursors remain in the primary SQL store.
// Timeout and TTL fields are seconds in JSON, legacy YAML and environment values.
type Cache struct {
	Backend                   string `json:"backend"`
	RedisURL                  string `json:"redisUrl"`
	RedisMaxMemory            string `json:"redisMaxMemory"`
	RedisSocketTimeout        int    `json:"redisSocketTimeout"`
	RedisSocketConnectTimeout int    `json:"redisSocketConnectTimeout"`
	MemoryMaxsize             int    `json:"memoryMaxsize"`
	MemoryDefaultTTL          int    `json:"memoryDefaultTtl"`
	Namespace                 string `json:"namespace"`
	MaxBytes                  int64  `json:"maxBytes"`
	MaxValueBytes             int    `json:"maxValueBytes"`
	RedisPoolSize             int    `json:"redisPoolSize"`
	RedisFallback             bool   `json:"redisFallback"`
}

func CacheDefaults() Cache {
	return Cache{Backend: "hybrid", RedisMaxMemory: "256mb", RedisSocketTimeout: 30,
		RedisSocketConnectTimeout: 5, MemoryMaxsize: 1024, MemoryDefaultTTL: 600,
		Namespace: "default", MaxBytes: 32 << 20, MaxValueBytes: 1 << 20,
		RedisPoolSize: 4, RedisFallback: true}
}

// Options translates public second-based settings without performing network I/O.
// Call Validate before using Options so duration multiplication cannot overflow.
func (c Cache) Options(timezone string) cachebackend.Options {
	return cachebackend.Options{
		Backend: c.Backend, Namespace: c.Namespace, RedisURL: c.RedisURL,
		MaxEntries: c.MemoryMaxsize, MaxBytes: c.MaxBytes, MaxValueBytes: c.MaxValueBytes,
		DefaultTTL:     time.Duration(c.MemoryDefaultTTL) * time.Second,
		SocketTimeout:  time.Duration(c.RedisSocketTimeout) * time.Second,
		ConnectTimeout: time.Duration(c.RedisSocketConnectTimeout) * time.Second,
		RedisPoolSize:  c.RedisPoolSize, RedisFallback: c.RedisFallback, Timezone: timezone,
	}
}

var redisMemoryPattern = regexp.MustCompile(`(?i)^[0-9]+(k|kb|m|mb|g|gb)?$`)

func (c Cache) Validate() error {
	// Check in original units before converting untrusted ints to durations.
	if c.MemoryDefaultTTL < 1 || c.MemoryDefaultTTL > 604800 {
		return errors.New("cache.memoryDefaultTtl must be 1..604800 seconds")
	}
	if c.RedisSocketTimeout < 1 || c.RedisSocketTimeout > 30 {
		return errors.New("cache.redisSocketTimeout must be 1..30 seconds")
	}
	if c.RedisSocketConnectTimeout < 1 || c.RedisSocketConnectTimeout > 5 {
		return errors.New("cache.redisSocketConnectTimeout must be 1..5 seconds")
	}
	if len(c.RedisMaxMemory) > 32 || !redisMemoryPattern.MatchString(c.RedisMaxMemory) {
		return errors.New("cache.redisMaxMemory must be a legacy byte quantity (for example 256mb); it is diagnostic-only")
	}
	// Validate an explicitly supplied endpoint even when the selected mode does
	// not use it. Do not dial or include URL credentials in an error message.
	if c.RedisURL != "" || c.Backend == "redis" || c.Backend == "valkey" {
		if err := cachebackend.ValidateRedisURL(c.RedisURL); err != nil {
			return errors.New("cache.redisUrl must be a valid Redis/Valkey URL; redis/valkey backend requires a nonempty URL")
		}
	}
	if err := c.Options("UTC").Validate(); err != nil {
		return fmt.Errorf("cache: %w", err)
	}
	return nil
}

func (c Cache) Diagnostics() []string {
	return []string{"cache.redisMaxMemory is a legacy compatibility-only setting; Redis/Valkey server memory policy is externally managed and AniDan never issues CONFIG SET"}
}

// UnmarshalJSON overlays defaults while rejecting unknown, duplicate and null
// settings. Decoder errors may contain user input, so only fixed field names
// and sanitized diagnostics escape this boundary.
func (c *Cache) UnmarshalJSON(data []byte) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	tok, err := dec.Token()
	if err != nil || tok != json.Delim('{') {
		return errors.New("cache must be a JSON object")
	}
	fields := make(map[string]json.RawMessage)
	for dec.More() {
		tok, err = dec.Token()
		if err != nil {
			return errors.New("cache JSON is invalid")
		}
		key, ok := tok.(string)
		if !ok {
			return errors.New("cache JSON is invalid")
		}
		if _, exists := fields[key]; exists {
			return errors.New("cache contains a duplicate field")
		}
		var raw json.RawMessage
		if err = dec.Decode(&raw); err != nil {
			return errors.New("cache JSON is invalid")
		}
		if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return errors.New("cache fields must not be null")
		}
		fields[key] = raw
	}
	if _, err = dec.Token(); err != nil {
		return errors.New("cache JSON is invalid")
	}
	if _, err = dec.Token(); err != io.EOF {
		return errors.New("cache must contain exactly one JSON object")
	}
	// A type alias avoids recursively invoking this custom decoder.
	type plain Cache
	candidate := plain(*c)
	allowed := map[string]any{
		"backend": &candidate.Backend, "redisUrl": &candidate.RedisURL,
		"redisMaxMemory":            &candidate.RedisMaxMemory,
		"redisSocketTimeout":        &candidate.RedisSocketTimeout,
		"redisSocketConnectTimeout": &candidate.RedisSocketConnectTimeout,
		"memoryMaxsize":             &candidate.MemoryMaxsize, "memoryDefaultTtl": &candidate.MemoryDefaultTTL,
		"namespace": &candidate.Namespace, "maxBytes": &candidate.MaxBytes,
		"maxValueBytes": &candidate.MaxValueBytes, "redisPoolSize": &candidate.RedisPoolSize,
		"redisFallback": &candidate.RedisFallback,
	}
	keys := make([]string, 0, len(fields))
	for key := range fields {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		dst, ok := allowed[key]
		if !ok {
			return errors.New("cache contains an unknown field")
		}
		if err = json.Unmarshal(fields[key], dst); err != nil {
			return fmt.Errorf("cache.%s has an invalid type or value", key)
		}
	}
	*c = Cache(candidate)
	return nil
}

// Environment aliases are deliberately exact uppercase names, matching the
// existing native environment contract. A present empty value is not absence.
func (c *Cache) applyEnvironment() error {
	type field struct {
		name   string
		legacy bool
		dst    any
	}
	fields := []field{
		{"BACKEND", true, &c.Backend}, {"REDIS_URL", true, &c.RedisURL},
		{"REDIS_MAX_MEMORY", true, &c.RedisMaxMemory},
		{"REDIS_SOCKET_TIMEOUT", true, &c.RedisSocketTimeout},
		{"REDIS_SOCKET_CONNECT_TIMEOUT", true, &c.RedisSocketConnectTimeout},
		{"MEMORY_MAXSIZE", true, &c.MemoryMaxsize}, {"MEMORY_DEFAULT_TTL", true, &c.MemoryDefaultTTL},
		{"NAMESPACE", false, &c.Namespace}, {"MAX_BYTES", false, &c.MaxBytes},
		{"MAX_VALUE_BYTES", false, &c.MaxValueBytes}, {"REDIS_POOL_SIZE", false, &c.RedisPoolSize},
		{"REDIS_FALLBACK", false, &c.RedisFallback},
	}
	known := make(map[string]bool)
	for _, f := range fields {
		known["ANIDAN_CACHE_"+f.name] = true
		if f.legacy {
			known["DANMUAPI_CACHE__"+f.name] = true
		}
	}
	for _, env := range os.Environ() {
		key, _, _ := strings.Cut(env, "=")
		if (strings.HasPrefix(key, "ANIDAN_CACHE_") || strings.HasPrefix(key, "DANMUAPI_CACHE__")) && !known[key] {
			return errors.New("unknown cache environment option")
		}
	}
	for _, f := range fields {
		name := "ANIDAN_CACHE_" + f.name
		value, present := os.LookupEnv(name)
		if !present && f.legacy {
			name = "DANMUAPI_CACHE__" + f.name
			value, present = os.LookupEnv(name)
		}
		if !present {
			continue
		}
		switch dst := f.dst.(type) {
		case *string:
			*dst = value
		case *int:
			n, err := strconv.Atoi(value)
			if err != nil {
				return fmt.Errorf("%s must be an integer", name)
			}
			*dst = n
		case *int64:
			n, err := strconv.ParseInt(value, 10, 64)
			if err != nil {
				return fmt.Errorf("%s must be an integer", name)
			}
			*dst = n
		case *bool:
			b, err := strconv.ParseBool(value)
			if err != nil {
				return fmt.Errorf("%s must be boolean", name)
			}
			*dst = b
		}
	}
	return nil
}
