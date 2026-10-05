// SPDX-License-Identifier: AGPL-3.0-only
package integration

// The fixed provider set bounds this bitmap. The empty bitmap is a single cheap
// atomic check on the default-off path; no DB read, parsing, or capture buffer.
func responseLogBit(provider string) uint64 {
	switch provider {
	case "bangumi":
		return 1 << 0
	case "tmdb":
		return 1 << 1
	case "tvdb":
		return 1 << 2
	case "imdb":
		return 1 << 3
	case "douban":
		return 1 << 4
	case "trakt":
		return 1 << 5
	case "anibt":
		return 1 << 6
	case "360":
		return 1 << 7
	}
	return 0
}

// SetResponseLogging changes only future requests. Server callers publish after
// committing configuration, so a failed transaction never activates logging.
func (c *Client) SetResponseLogging(provider string, enabled bool) {
	bit := responseLogBit(provider)
	if bit == 0 {
		return
	}
	set := uint64(0)
	if enabled {
		set = bit
	}
	c.updateResponseLogMask(set, bit)
}

// SetResponseLoggingBatch publishes a committed multi-provider patch in one
// atomic operation; unrelated provider bits remain unchanged.
func (c *Client) SetResponseLoggingBatch(providers map[string]bool) {
	var set, clear uint64
	for provider, enabled := range providers {
		bit := responseLogBit(provider)
		clear |= bit
		if enabled {
			set |= bit
		}
	}
	if clear != 0 {
		c.updateResponseLogMask(set, clear)
	}
}
func (c *Client) updateResponseLogMask(set, clear uint64) {
	for {
		old := c.responseLogMask.Load()
		if c.responseLogMask.CompareAndSwap(old, old&^clear|set) {
			return
		}
	}
}
func (c *Client) ResponseLoggingEnabled(provider string) bool {
	mask := c.responseLogMask.Load()
	return mask != 0 && mask&responseLogBit(provider) != 0
}
