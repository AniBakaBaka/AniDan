// SPDX-License-Identifier: AGPL-3.0-only
package config

import (
	"errors"
	"fmt"

	"gopkg.in/yaml.v3"
)

// LegacyCache contains exactly the upstream cache YAML fields. Pointer presence
// keeps an explicit empty string or zero distinct from an omitted default.
type LegacyCache struct {
	Backend                   *string `yaml:"backend"`
	RedisURL                  *string `yaml:"redis_url"`
	RedisMaxMemory            *string `yaml:"redis_max_memory"`
	RedisSocketTimeout        *int    `yaml:"redis_socket_timeout"`
	RedisSocketConnectTimeout *int    `yaml:"redis_socket_connect_timeout"`
	MemoryMaxsize             *int    `yaml:"memory_maxsize"`
	MemoryDefaultTTL          *int    `yaml:"memory_default_ttl"`
}

func (c *LegacyCache) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.MappingNode || node.Tag != "!!map" {
		return errors.New("legacy cache must be a YAML mapping")
	}
	var candidate LegacyCache
	fields := map[string]any{
		"backend": &candidate.Backend, "redis_url": &candidate.RedisURL,
		"redis_max_memory":             &candidate.RedisMaxMemory,
		"redis_socket_timeout":         &candidate.RedisSocketTimeout,
		"redis_socket_connect_timeout": &candidate.RedisSocketConnectTimeout,
		"memory_maxsize":               &candidate.MemoryMaxsize,
		"memory_default_ttl":           &candidate.MemoryDefaultTTL,
	}
	seen := make(map[string]bool)
	for i := 0; i < len(node.Content); i += 2 {
		key, value := node.Content[i], node.Content[i+1]
		dst, exists := fields[key.Value]
		if key.Kind != yaml.ScalarNode || key.Tag != "!!str" || !exists {
			return errors.New("legacy cache contains an unknown field")
		}
		if seen[key.Value] {
			return errors.New("legacy cache contains a duplicate field")
		}
		seen[key.Value] = true
		if value.Kind != yaml.ScalarNode || value.Tag == "!!null" {
			return fmt.Errorf("legacy cache.%s must be a non-null scalar", key.Value)
		}
		switch dst.(type) {
		case **string:
			if value.Tag != "!!str" {
				return fmt.Errorf("legacy cache.%s must be a string", key.Value)
			}
		case **int:
			if value.Tag != "!!int" {
				return fmt.Errorf("legacy cache.%s must be an integer", key.Value)
			}
		}
		if err := value.Decode(dst); err != nil {
			return fmt.Errorf("legacy cache.%s has an invalid type or value", key.Value)
		}
	}
	*c = candidate
	return nil
}

func (c *Cache) applyLegacy(old LegacyCache) {
	for _, field := range []struct{ src, dst *string }{
		{old.Backend, &c.Backend}, {old.RedisURL, &c.RedisURL}, {old.RedisMaxMemory, &c.RedisMaxMemory},
	} {
		if field.src != nil {
			*field.dst = *field.src
		}
	}
	for _, field := range []struct{ src, dst *int }{
		{old.RedisSocketTimeout, &c.RedisSocketTimeout},
		{old.RedisSocketConnectTimeout, &c.RedisSocketConnectTimeout},
		{old.MemoryMaxsize, &c.MemoryMaxsize}, {old.MemoryDefaultTTL, &c.MemoryDefaultTTL},
	} {
		if field.src != nil {
			*field.dst = *field.src
		}
	}
}
