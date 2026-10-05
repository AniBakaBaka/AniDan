// SPDX-License-Identifier: AGPL-3.0-only
package provider

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/AniBakaBaka/AniDan/internal/proxyroute"
)

// CacheIdentity hashes private request-affecting options without exposing them.
// Native adapter identities are stable across process restarts. Unknown in-process
// providers use an instance identity and must not promise cross-restart reuse.
// Providers must be immutable after registration (Registry.Configure publishes a
// detached copy); mutating a configured adapter directly is unsupported.
func CacheIdentity(p Provider) string {
	if p == nil {
		return "missing"
	}
	var h *HTTPProvider
	extra := map[string]string{}
	switch v := p.(type) {
	case *Bilibili:
		h = &v.HTTPProvider
	case *Dandanplay:
		h = &v.HTTPProvider
		extra["appId"] = v.AppID
		extra["appSecret"] = v.AppSecret
	case *Legacy:
		h = &v.HTTPProvider
		extra["youkuClientId"] = v.YoukuClientID
	default:
		sum := sha256.Sum256([]byte(fmt.Sprintf("process-provider:%T:%p:%s", p, p, p.Name())))
		return hex.EncodeToString(sum[:])
	}
	settings := httpConfiguration(p.Name(), h)
	settings["proxyURL"] = h.proxyURL
	settings["outboundRoute"] = h.routingIdentity
	for k, v := range extra {
		settings[k] = v
	}
	// Headers include the real cookie/authorization values, not the deliberately
	// masked Configuration DTO. Only the final digest leaves this package.
	raw, _ := json.Marshal([]any{"native-provider-cache-v2", p.Name(), settings, h.Headers, h.BaseURLs})
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// CacheSnapshot binds cache key identity to the same immutable provider instances
// that will do the load. A Configure call between key creation and Search cannot
// otherwise be allowed to store new-credential data under an old-credential key.
func (r *Registry) CacheSnapshot(names ...string) (*Registry, map[string]string) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	snapshot := &Registry{drain: r.drain, revision: r.revision, providers: make(map[string]Provider, len(r.providers)), Workers: r.Workers, Timeout: r.Timeout, searchObserver: r.searchObserver}
	identities := make(map[string]string, len(names))
	for name, p := range r.providers {
		snapshot.providers[name] = p
	}
	if len(names) == 0 {
		names = make([]string, 0, len(r.providers))
		for name := range r.providers {
			names = append(names, name)
		}
	}
	for _, name := range names {
		p := r.providers[strings.ToLower(name)]
		identity := CacheIdentity(p)
		switch p.(type) {
		case *Bilibili, *Dandanplay, *Legacy:
		default:
			sum := sha256.Sum256([]byte(fmt.Sprintf("%s:%p:%d", identity, r, r.revision)))
			identity = hex.EncodeToString(sum[:])
		}
		identities[name] = identity
	}
	return snapshot, identities
}

// ValidateRouting checks a captured provider's current authorization before a
// response cache lookup. A distinct identity cannot itself revoke an old
// snapshot; the server's callback does. Unknown providers retain their policy.
func ValidateRouting(ctx context.Context, p Provider) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	var h *HTTPProvider
	switch v := p.(type) {
	case *Bilibili:
		h = &v.HTTPProvider
	case *Dandanplay:
		h = &v.HTTPProvider
	case *Legacy:
		h = &v.HTTPProvider
	default:
		return nil
	}
	cfg := h.routingConfig
	if cfg.Blocked {
		return proxyroute.ErrUntrustedGateway
	}
	if err := proxyroute.Validate(cfg); err != nil {
		return err
	}
	if cfg.AccelerateURL != "" && cfg.Authorize == nil {
		return proxyroute.ErrUntrustedGateway
	}
	if cfg.Authorize != nil {
		if err := cfg.Authorize(ctx); err != nil {
			if errors.Is(err, context.Canceled) {
				return context.Canceled
			}
			if errors.Is(err, context.DeadlineExceeded) {
				return context.DeadlineExceeded
			}
			return proxyroute.ErrUntrustedGateway
		}
	}
	return nil
}
