// SPDX-License-Identifier: AGPL-3.0-or-later
package integration

import (
	_ "embed"
	"encoding/json"
	"sync"
)

//go:embed metadata_configs.json
var metadataConfigJSON []byte

func MetadataConfigs() map[string]map[string]any {
	v := map[string]map[string]any{}
	_ = json.Unmarshal(metadataConfigJSON, &v)
	return v
}
func (c *Client) LockOAuth(key string) func() {
	m, _ := c.oauthLocks.LoadOrStore(key, &sync.Mutex{})
	mu := m.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}
