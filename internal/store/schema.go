// SPDX-License-Identifier: AGPL-3.0-only
// Schema translated from misaka_danmu_server src/db/orm_models.py at
// 01751526f6e4154bcc8f517481d02b68cb2684a9. See LICENSE and NOTICE.
package store

import (
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"sort"
)

type Row map[string]any

type Column struct {
	Name          string   `json:"name"`
	Kind          string   `json:"kind"`
	Nullable      bool     `json:"nullable"`
	Auto          bool     `json:"auto,omitempty"`
	Length        int      `json:"length,omitempty"`
	Precision     int      `json:"precision,omitempty"`
	Scale         int      `json:"scale,omitempty"`
	Medium        bool     `json:"medium,omitempty"`
	Enum          []string `json:"enum,omitempty"`
	Reference     string   `json:"reference,omitempty"`
	OnDelete      string   `json:"on_delete,omitempty"`
	HasDefault    bool     `json:"has_default,omitempty"`
	Default       any      `json:"default,omitempty"`
	NowDefault    bool     `json:"now_default,omitempty"`
	NowUpdate     bool     `json:"now_update,omitempty"`
	ServerDefault *string  `json:"server_default,omitempty"`
}
type Index struct {
	Name        string   `json:"name"`
	Columns     []string `json:"columns"`
	MySQLLength int      `json:"mysql_length,omitempty"`
}
type Table struct {
	Name       string     `json:"name"`
	Columns    []Column   `json:"columns"`
	PrimaryKey []string   `json:"primary_key"`
	Unique     [][]string `json:"unique"`
	Indexes    []Index    `json:"indexes"`
}

func (t Table) Column(name string) (Column, bool) {
	for _, c := range t.Columns {
		if c.Name == name {
			return c, true
		}
	}
	return Column{}, false
}

// Schema contains the complete allowlisted 36-table physical legacy schema.
// Treat this map and its contents as immutable.
var Schema map[string]Table

//go:embed schema.json
var schemaJSON []byte
var tableOrder []string

func init() {
	var ts []Table
	if err := json.Unmarshal(schemaJSON, &ts); err != nil {
		panic(err)
	}
	Schema = make(map[string]Table, len(ts))
	for _, t := range ts {
		Schema[t.Name] = t
	}
	seen := map[string]bool{}
	var add func(string)
	add = func(n string) {
		if seen[n] {
			return
		}
		seen[n] = true
		for _, c := range Schema[n].Columns {
			if c.Reference != "" {
				for i, ch := range c.Reference {
					if ch == '.' {
						add(c.Reference[:i])
						break
					}
				}
			}
		}
		tableOrder = append(tableOrder, n)
	}
	names := make([]string, 0, len(Schema))
	for n := range Schema {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		add(n)
	}
}
func Tables() []string          { return append([]string(nil), tableOrder...) }
func SchemaFingerprint() string { h := sha256.Sum256(schemaJSON); return hex.EncodeToString(h[:]) }
