// SPDX-License-Identifier: AGPL-3.0-only
package provider

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/AniBakaBaka/AniDan/internal/proxyroute"
)

type ConfigField struct {
	Key         string `json:"key"`
	Title       string `json:"title"`
	Type        string `json:"type"`
	Secret      bool   `json:"secret"`
	Description string `json:"description,omitempty"`
}

// Configurable.Configure is for a detached provider before registration.
// Use Registry.Configure for atomic runtime changes while requests are active.
type Configurable interface {
	Configure(map[string]string) error
	ConfigSchema() []ConfigField
	Configuration() map[string]string
}

func schema(name string) []ConfigField {
	f := []ConfigField{{Key: "logRawResponses", Title: "Sanitized response diagnostics", Type: "boolean", Description: "Opt-in bounded JSON structure logs; strings/unknown fields and non-JSON bodies omitted; never literal raw bytes"}, {Key: "proxyURL", Title: "Outbound proxy URL", Type: "string", Secret: true, Description: "Explicit http/https/socks5/socks5h proxy; credentials masked; empty disables proxy"}, {Key: "userAgent", Title: "User-Agent", Type: "string"}, {Key: "timeoutSeconds", Title: "Request timeout", Type: "integer", Description: "1–120 seconds"}, {Key: "segmentWorkers", Title: "Parallel segment downloads", Type: "integer", Description: "1–4 concurrent segments; default4"}, {Key: "maxSegments", Title: "Maximum pages/segments", Type: "integer", Description: "1–1000; exceeding it returns an explicit error"}, {Key: "maxComments", Title: "Maximum comments", Type: "integer", Description: "1–1000000"}, {Key: "maxDownloadBytes", Title: "Maximum decoded download budget", Type: "integer", Description: "1MiB–512MiB aggregate text/parameter +64B/comment estimate"}, {Key: "maxResponseBytes", Title: "Maximum response bytes", Type: "integer", Description: "1024–67108864"}}
	if name == "dandanplay" {
		f = append(f, ConfigField{Key: "dandanplay_app_id", Title: "Dandanplay App ID", Type: "string"}, ConfigField{Key: "dandanplay_app_secret", Title: "Dandanplay App secret", Type: "string", Secret: true})
	} else {
		f = append(f, ConfigField{Key: name + "Cookie", Title: "Provider cookie", Type: "string", Secret: true, Description: "Used for this provider's fixed API URLs; an explicitly trusted accelerate gateway also receives it"})
	}
	if name == "youku" {
		f = append(f, ConfigField{Key: "youkuClientId", Title: "Registered Youku application client ID", Type: "string", Secret: true, Description: "Your registered OpenAPI client ID; no third-party partner credential is bundled"})
	}
	return f
}
func canonical(name, key string) string {
	switch key {
	case name + "UserAgent":
		return "userAgent"
	case "cookie", name + "Cookie":
		return name + "Cookie"
	case "dandanplayAppId", "appId":
		if name == "dandanplay" {
			return "dandanplay_app_id"
		}
	case "dandanplayAppSecret", "appSecret":
		if name == "dandanplay" {
			return "dandanplay_app_secret"
		}
	}
	return key
}
func configureHTTP(name string, h *HTTPProvider, settings map[string]string) (map[string]string, error) {
	h.flights = nextCommentFlights(h.flights)
	h.Headers = h.Headers.Clone()
	if h.Headers == nil {
		h.Headers = make(http.Header)
	}
	if h.Client == nil {
		lifetime := h.flights
		*h = newHTTP(nil)
		h.flights = lifetime
	} else {
		c := *h.Client
		h.Client = &c
	}
	extra := map[string]string{}
	allowed := map[string]bool{}
	for _, f := range schema(name) {
		allowed[f.Key] = true
	}
	for raw, value := range settings {
		key := canonical(name, raw)
		if !allowed[key] {
			return nil, fmt.Errorf("unsupported %s setting %q", name, raw)
		}
		if len(value) > 65536 || strings.ContainsAny(value, "\r\n\x00") {
			return nil, errors.New("invalid provider setting value")
		}
		if value == "********" && (key == name+"Cookie" || key == "dandanplay_app_secret" || key == "proxyURL" || key == "youkuClientId") {
			continue
		}
		switch key {
		case "logRawResponses":
			if value != "true" && value != "false" {
				return nil, errors.New("logRawResponses must be true or false")
			}
			h.logResponses = value == "true"
		case "proxyURL":
			if e := configureProxy(h, value); e != nil {
				return nil, e
			}
		case "userAgent":
			h.Headers.Set("User-Agent", value)
		case name + "Cookie":
			if value == "" {
				h.Headers.Del("Cookie")
			} else {
				h.Headers.Set("Cookie", value)
			}
		case "dandanplay_app_id", "dandanplay_app_secret":
			if len(value) > 4096 {
				return nil, errors.New("Dandanplay setting too long")
			}
			extra[key] = value
		case "youkuClientId":
			if value != "" && (!alphaID.MatchString(value) || len(value) > 128) {
				return nil, errors.New("invalid registered Youku client ID")
			}
			extra[key] = value
		default:
			n, e := strconv.Atoi(value)
			if e != nil {
				return nil, fmt.Errorf("%s must be an integer", key)
			}
			switch key {
			case "timeoutSeconds":
				if n < 1 || n > 120 {
					return nil, errors.New("timeoutSeconds out of range")
				}
				h.Client.Timeout = time.Duration(n) * time.Second
			case "segmentWorkers":
				if n < 1 || n > 4 {
					return nil, errors.New("segmentWorkers out of range")
				}
				h.SegmentWorkers = n
			case "maxSegments":
				if n < 1 || n > 1000 {
					return nil, errors.New("maxSegments out of range")
				}
				h.MaxSegments = n
			case "maxComments":
				if n < 1 || n > 1000000 {
					return nil, errors.New("maxComments out of range")
				}
				h.MaxComments = n
			case "maxDownloadBytes":
				if n < 1<<20 || n > 512<<20 {
					return nil, errors.New("maxDownloadBytes out of range")
				}
				h.MaxDownloadBytes = int64(n)
			case "maxResponseBytes":
				if n < 1024 || n > 64<<20 {
					return nil, errors.New("maxResponseBytes out of range")
				}
				h.MaxBytes = int64(n)
			}
		}
	}
	return extra, nil
}
func httpConfiguration(name string, h *HTTPProvider) map[string]string {
	timeout := 20
	if h.Client != nil {
		timeout = int(h.Client.Timeout / time.Second)
	}
	out := map[string]string{"logRawResponses": strconv.FormatBool(h.logResponses), "userAgent": h.Headers.Get("User-Agent"), "timeoutSeconds": strconv.Itoa(timeout), "segmentWorkers": strconv.Itoa(h.SegmentWorkers), "maxSegments": strconv.Itoa(h.maxSegments()), "maxComments": strconv.Itoa(h.MaxComments), "maxDownloadBytes": strconv.FormatInt(h.MaxDownloadBytes, 10), "maxResponseBytes": strconv.FormatInt(h.MaxBytes, 10)}
	out["proxyURL"] = ""
	if h.proxyURL != "" {
		out["proxyURL"] = "********"
	}
	if name != "dandanplay" {
		out[name+"Cookie"] = ""
		if h.Headers.Get("Cookie") != "" {
			out[name+"Cookie"] = "********"
		}
	}
	return out
}
func (b *Bilibili) ConfigSchema() []ConfigField { return schema(b.Name()) }
func (b *Bilibili) Configuration() map[string]string {
	return httpConfiguration(b.Name(), &b.HTTPProvider)
}
func (b *Bilibili) Configure(s map[string]string) error {
	_, e := configureHTTP(b.Name(), &b.HTTPProvider, s)
	return e
}
func (d *Dandanplay) ConfigSchema() []ConfigField { return schema(d.Name()) }
func (d *Dandanplay) Configuration() map[string]string {
	out := httpConfiguration(d.Name(), &d.HTTPProvider)
	out["dandanplay_app_id"] = d.AppID
	out["dandanplay_app_secret"] = ""
	if d.AppSecret != "" {
		out["dandanplay_app_secret"] = "********"
	}
	return out
}
func (d *Dandanplay) Configure(s map[string]string) error {
	x, e := configureHTTP(d.Name(), &d.HTTPProvider, s)
	if e != nil {
		return e
	}
	if v, ok := x["dandanplay_app_id"]; ok {
		d.AppID = v
	}
	if v, ok := x["dandanplay_app_secret"]; ok {
		d.AppSecret = v
	}
	return nil
}
func (l *Legacy) ConfigSchema() []ConfigField { return schema(l.Name()) }
func (l *Legacy) Configuration() map[string]string {
	out := httpConfiguration(l.Name(), &l.HTTPProvider)
	if l.Name() == "youku" {
		out["youkuClientId"] = ""
		if l.YoukuClientID != "" {
			out["youkuClientId"] = "********"
		}
	}
	return out
}
func (l *Legacy) Configure(s map[string]string) error {
	x, e := configureHTTP(l.Name(), &l.HTTPProvider, s)
	if e != nil {
		return e
	}
	if v, ok := x["youkuClientId"]; ok {
		l.YoukuClientID = v
	}
	return nil
}

// Configure clones and validates before publishing a new instance. In-flight
// calls finish against their immutable old config. Secrets never appear in
// Configuration, Catalog, errors or logs. Unknown legacy options are errors.
func (r *Registry) Configure(name string, s map[string]string) error {
	return r.configure(name, s, nil)
}

// ConfigureWithRouting publishes one detached snapshot containing both source
// settings and the server-authorized route. Accelerate is intentionally absent
// from the standalone source schema, so settings cannot create gateway trust.
func (r *Registry) ConfigureWithRouting(name string, s map[string]string, cfg proxyroute.Config) error {
	return r.configure(name, s, &cfg)
}

func (r *Registry) configure(name string, s map[string]string, cfg *proxyroute.Config) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.drain != nil && r.drain.isClosed() {
		return ErrRegistryClosed
	}
	key := strings.ToLower(name)
	p, ok := r.providers[key]
	if !ok {
		return fmt.Errorf("unknown provider %q", name)
	}
	var next Provider
	switch x := p.(type) {
	case *Bilibili:
		v := *x
		next = &v
	case *Dandanplay:
		v := *x
		next = &v
	case *Legacy:
		v := *x
		next = &v
	default:
		return errors.New("provider does not support atomic runtime configuration")
	}
	if e := next.(Configurable).Configure(s); e != nil {
		return e
	}
	if cfg != nil {
		var h *HTTPProvider
		switch x := next.(type) {
		case *Bilibili:
			h = &x.HTTPProvider
		case *Dandanplay:
			h = &x.HTTPProvider
		case *Legacy:
			h = &x.HTTPProvider
		}
		if e := configureRouting(h, *cfg); e != nil {
			return e
		}
	}
	r.providers[key] = next
	r.revision++
	return nil
}
func (r *Registry) Configuration(name string) (map[string]string, error) {
	p, ok := r.Get(name)
	if !ok {
		return nil, fmt.Errorf("unknown provider %q", name)
	}
	c, ok := p.(Configurable)
	if !ok {
		return nil, errors.New("provider is not configurable")
	}
	return c.Configuration(), nil
}
func (r *Registry) ConfigSchema(name string) ([]ConfigField, error) {
	p, ok := r.Get(name)
	if !ok {
		return nil, fmt.Errorf("unknown provider %q", name)
	}
	c, ok := p.(Configurable)
	if !ok {
		return nil, errors.New("provider is not configurable")
	}
	return c.ConfigSchema(), nil
}

// ValidateConfiguration performs the same detached validation as Configure,
// without changing registry state. Persist settings transactionally after this
// check, then Configure the same values to activate them.
func (r *Registry) ValidateConfiguration(name string, s map[string]string) error {
	p, ok := r.Get(name)
	if !ok {
		return fmt.Errorf("unknown provider %q", name)
	}
	isolated := NewRegistry(1, p)
	return isolated.Configure(name, s)
}

// ValidateConfigurationWithRouting validates the same detached snapshot without
// publishing it, allowing persistence to complete before route activation.
func (r *Registry) ValidateConfigurationWithRouting(name string, s map[string]string, cfg proxyroute.Config) error {
	p, ok := r.Get(name)
	if !ok {
		return fmt.Errorf("unknown provider %q", name)
	}
	isolated := NewRegistry(1, p)
	return isolated.ConfigureWithRouting(name, s, cfg)
}

func configureProxy(h *HTTPProvider, raw string) error {
	return configureRouting(h, proxyroute.Config{ProxyURL: raw})
}

func configureRouting(h *HTTPProvider, cfg proxyroute.Config) error {
	client, err := proxyroute.Client(h.Client, cfg)
	if err != nil {
		return err
	}
	h.Client = client
	h.proxyURL = cfg.ProxyURL
	h.routingIdentity = proxyroute.Identity(cfg)
	h.routingConfig = cfg
	return nil
}
