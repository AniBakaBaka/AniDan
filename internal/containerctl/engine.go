// SPDX-License-Identifier: AGPL-3.0-only
// Package containerctl provides an opt-in Docker Engine client. It never discovers
// a socket, starts a daemon, shells out, or reads registry credentials.
package containerctl

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

var (
	ErrDisabled     = errors.New("container management is disabled")
	ErrConfirmation = errors.New("a fresh, matching one-use confirmation is required")
	ErrChanged      = errors.New("container configuration changed; request a new confirmation")
	ErrBusy         = errors.New("another container operation is running")
	ErrManaged      = errors.New("Compose and Swarm managed containers must be updated through their manager")
	ErrExternal     = errors.New("updates require an explicitly configured external controller")
	idPattern       = regexp.MustCompile(`^[a-f0-9]{64}$`)
	imagePattern    = regexp.MustCompile(`^([a-z0-9](?:[a-z0-9.-]*[a-z0-9])?(?::[0-9]{1,5})?)/([a-z0-9]+(?:(?:[._]|__|[-]+)[a-z0-9]+)*(?:/[a-z0-9]+(?:(?:[._]|__|[-]+)[a-z0-9]+)*)*)(?::[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}|@sha256:[a-f0-9]{64})$`)
)

// Options has no defaults for privileged access. Enabled=false does no I/O.
type Options struct {
	Enabled            bool
	Socket             string
	ContainerID        string
	AllowedImages      []string
	ExternalController bool
}

func ValidID(s string) bool { return idPattern.MatchString(s) }
func ValidImage(s string) bool {
	m := imagePattern.FindStringSubmatch(s)
	return len(s) <= 512 && m != nil && (strings.Contains(m[1], ".") || strings.Contains(m[1], ":") || m[1] == "localhost")
}
func (o Options) Validate() error {
	if !o.Enabled {
		return nil
	}
	if !filepath.IsAbs(o.Socket) || filepath.Clean(o.Socket) != o.Socket || strings.ContainsAny(o.Socket, "\x00\r\n") {
		return errors.New("Docker socket must be an explicit, clean absolute Unix socket path")
	}
	if !ValidID(o.ContainerID) {
		return errors.New("Docker container ID must be exactly 64 lowercase hexadecimal characters")
	}
	if len(o.AllowedImages) > 100 {
		return errors.New("at most 100 allowed Docker images may be configured")
	}
	for _, image := range o.AllowedImages {
		if !ValidImage(image) {
			return errors.New("allowed images must be fully qualified registry/repository references with an explicit tag or sha256 digest")
		}
	}
	return nil
}

// Inspection intentionally retains raw configuration internally; it must never
// be serialized in API responses or logs because Env and HostConfig can be secret.
type Inspection struct {
	ID              string                     `json:"Id"`
	Name            string                     `json:"Name"`
	Image           string                     `json:"Image"`
	Config          map[string]json.RawMessage `json:"Config"`
	HostConfig      map[string]json.RawMessage `json:"HostConfig"`
	Mounts          []Mount                    `json:"Mounts"`
	NetworkSettings struct {
		Networks map[string]map[string]json.RawMessage `json:"Networks"`
	} `json:"NetworkSettings"`
	State struct {
		Status    string `json:"Status"`
		Running   bool   `json:"Running"`
		StartedAt string `json:"StartedAt"`
		Health    *struct {
			Status string `json:"Status"`
		} `json:"Health"`
	} `json:"State"`
}
type Mount struct {
	Type        string `json:"Type"`
	Name        string `json:"Name"`
	Source      string `json:"Source"`
	Destination string `json:"Destination"`
	Mode        string `json:"Mode"`
	RW          bool   `json:"RW"`
}

func rawString(m map[string]json.RawMessage, k string) string {
	var s string
	_ = json.Unmarshal(m[k], &s)
	return s
}
func rawBool(m map[string]json.RawMessage, k string) bool {
	var b bool
	_ = json.Unmarshal(m[k], &b)
	return b
}

// Engine is injectable for fixture tests; production New uses only a Unix socket.
type Engine interface {
	Inspect(context.Context, string) (Inspection, error)
	Stats(context.Context, string) (Stats, error)
	Pull(context.Context, string, func(Progress)) error
	Restart(context.Context, string) error
	Stop(context.Context, string) error
	Rename(context.Context, string, string) error
	Create(context.Context, string, map[string]json.RawMessage) (string, error)
	Start(context.Context, string) error
	Remove(context.Context, string) error
}
type unixEngine struct{ client *http.Client }

func newUnixEngine(socket string) *unixEngine {
	tr := &http.Transport{Proxy: nil, MaxIdleConns: 8, MaxConnsPerHost: 8, IdleConnTimeout: 30 * time.Second, ResponseHeaderTimeout: 30 * time.Second,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "unix", socket)
		}}
	return &unixEngine{client: &http.Client{Transport: tr, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}
func (e *unixEngine) request(ctx context.Context, method, path string, body any) (*http.Response, error) {
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, errors.New("cannot encode Docker request")
		}
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://docker/v1.45"+path, reader)
	if err != nil {
		return nil, errors.New("invalid Docker request")
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := e.client.Do(req)
	if err != nil {
		return nil, errors.New("Docker Engine request failed")
	}
	alreadyDone := res.StatusCode == http.StatusNotModified && method == "POST" && (strings.HasSuffix(path, "/start") || strings.Contains(path, "/stop?"))
	if !alreadyDone && (res.StatusCode < 200 || res.StatusCode >= 300) {
		res.Body.Close()
		return nil, fmt.Errorf("Docker Engine returned HTTP %d", res.StatusCode)
	}
	return res, nil
}
func (e *unixEngine) json(ctx context.Context, method, path string, body, out any) error {
	res, err := e.request(ctx, method, path, body)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if out == nil {
		_, err = io.Copy(io.Discard, io.LimitReader(res.Body, 1<<20))
		return err
	}
	b, err := io.ReadAll(io.LimitReader(res.Body, (8<<20)+1))
	if err != nil || len(b) > 8<<20 {
		return errors.New("Docker response exceeded limit or could not be read")
	}
	if err = json.Unmarshal(b, out); err != nil {
		return errors.New("invalid Docker response")
	}
	return nil
}
func (e *unixEngine) Inspect(ctx context.Context, id string) (Inspection, error) {
	var v Inspection
	if !ValidID(id) {
		return v, errors.New("invalid container ID")
	}
	err := e.json(ctx, "GET", "/containers/"+id+"/json", nil, &v)
	if err == nil && v.ID != id {
		return Inspection{}, errors.New("Docker returned an unexpected container ID")
	}
	return v, err
}
func (e *unixEngine) Stats(ctx context.Context, id string) (Stats, error) {
	var v Stats
	if !ValidID(id) {
		return v, errors.New("invalid container ID")
	}
	err := e.json(ctx, "GET", "/containers/"+id+"/stats?stream=false", nil, &v)
	return v, err
}
func (e *unixEngine) action(ctx context.Context, id, action string) error {
	if !ValidID(id) {
		return errors.New("invalid container ID")
	}
	return e.json(ctx, "POST", "/containers/"+id+"/"+action, nil, nil)
}
func (e *unixEngine) Restart(ctx context.Context, id string) error {
	return e.action(ctx, id, "restart?t=10")
}
func (e *unixEngine) Stop(ctx context.Context, id string) error {
	return e.action(ctx, id, "stop?t=10")
}
func (e *unixEngine) Start(ctx context.Context, id string) error { return e.action(ctx, id, "start") }
func (e *unixEngine) Rename(ctx context.Context, id, name string) error {
	return e.action(ctx, id, "rename?name="+url.QueryEscape(name))
}
func (e *unixEngine) Remove(ctx context.Context, id string) error {
	if !ValidID(id) {
		return errors.New("invalid container ID")
	}
	return e.json(ctx, "DELETE", "/containers/"+id+"?force=true&v=false", nil, nil)
}
func (e *unixEngine) Create(ctx context.Context, name string, body map[string]json.RawMessage) (string, error) {
	var v struct {
		ID string `json:"Id"`
	}
	err := e.json(ctx, "POST", "/containers/create?name="+url.QueryEscape(name), body, &v)
	if err == nil && !ValidID(v.ID) {
		return "", errors.New("Docker returned an invalid new container ID")
	}
	return v.ID, err
}
func (e *unixEngine) Pull(ctx context.Context, image string, emit func(Progress)) error {
	if !ValidImage(image) {
		return errors.New("invalid image reference")
	}
	res, err := e.request(ctx, "POST", "/images/create?fromImage="+url.QueryEscape(image), nil)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	// No registry credentials and no daemon-supplied text are accepted or emitted.
	limited := &io.LimitedReader{R: res.Body, N: (16 << 20) + 1}
	dec := json.NewDecoder(limited)
	count := 0
	completed := false
	for {
		var event struct {
			Error       string          `json:"error"`
			ErrorDetail json.RawMessage `json:"errorDetail"`
			Status      string          `json:"status"`
		}
		err = dec.Decode(&event)
		if err == io.EOF {
			if limited.N <= 1 {
				return errors.New("Docker image pull exceeded response limit")
			}
			if count == 0 || !completed {
				return errors.New("Docker image pull did not report successful completion")
			}
			return nil
		}
		if err != nil {
			return errors.New("invalid or oversized Docker pull response")
		}
		count++
		if strings.HasPrefix(event.Status, "Status: Downloaded newer image for ") || strings.HasPrefix(event.Status, "Status: Image is up to date for ") {
			completed = true
		}
		if event.Error != "" || len(event.ErrorDetail) > 0 && string(event.ErrorDetail) != "null" {
			return errors.New("Docker image pull failed")
		}
		if emit != nil && count%10 == 1 {
			emit(Progress{Status: "Pulling the explicitly allowed image", Progress: 25})
		}
	}
}
