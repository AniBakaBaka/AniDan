// SPDX-License-Identifier: AGPL-3.0-only
package containerctl

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

const confirmationTTL = 2 * time.Minute

type Controller struct {
	opts       Options
	engine     Engine
	now        func() time.Time
	hostname   string
	mu         sync.Mutex
	pending    map[[32]byte]ticket
	superseded bool
	operation  sync.Mutex
}
type ticket struct {
	User, Action, Image, Fingerprint string
	Expires                          time.Time
}
type Confirmation struct {
	Token       string    `json:"confirmationToken"`
	Action      string    `json:"action"`
	ContainerID string    `json:"containerId"`
	Image       string    `json:"image"`
	ExpiresAt   time.Time `json:"expiresAt"`
	Warning     string    `json:"warning"`
}
type Progress struct {
	Status              string `json:"status"`
	Progress            int    `json:"progress,omitempty"`
	Event               string `json:"event,omitempty"`
	ContainerID         string `json:"containerId,omitempty"`
	RollbackContainerID string `json:"rollbackContainerId,omitempty"`
}
type Status struct {
	Enabled         bool     `json:"enabled"`
	SDKInstalled    bool     `json:"sdkInstalled"`
	SocketAvailable bool     `json:"socketAvailable"`
	SocketPath      string   `json:"socketPath"`
	CanRestart      bool     `json:"canRestart"`
	CanUpdate       bool     `json:"canUpdate"`
	Message         string   `json:"message"`
	ContainerID     string   `json:"containerId,omitempty"`
	AllowedImages   []string `json:"allowedImages"`
}

func New(o Options) (*Controller, error) {
	if err := o.Validate(); err != nil {
		return nil, err
	}
	var e Engine
	if o.Enabled {
		e = newUnixEngine(o.Socket)
	}
	return NewWithEngine(o, e)
}

// NewWithEngine supports deterministic tests without a Docker daemon.
func NewWithEngine(o Options, e Engine) (*Controller, error) {
	if err := o.Validate(); err != nil {
		return nil, err
	}
	if o.Enabled && e == nil {
		return nil, errors.New("Docker engine is required when enabled")
	}
	o.AllowedImages = append([]string(nil), o.AllowedImages...)
	sort.Strings(o.AllowedImages)
	c := &Controller{opts: o, engine: e, now: time.Now, pending: map[[32]byte]ticket{}}
	if o.Enabled {
		c.hostname, _ = os.Hostname()
	}
	return c, nil
}
func (c *Controller) Enabled() bool { return c != nil && c.opts.Enabled }
func (c *Controller) TargetID() string {
	if c == nil {
		return ""
	}
	return c.opts.ContainerID
}
func (c *Controller) allowed(image string) bool {
	if !ValidImage(image) {
		return false
	}
	for _, v := range c.opts.AllowedImages {
		if image == v {
			return true
		}
	}
	return false
}
func (c *Controller) inspect(ctx context.Context) (Inspection, error) {
	if !c.Enabled() {
		return Inspection{}, ErrDisabled
	}
	v, err := c.engine.Inspect(ctx, c.opts.ContainerID)
	if err != nil {
		return Inspection{}, err
	}
	if v.ID != c.opts.ContainerID || v.Config == nil || v.HostConfig == nil {
		return Inspection{}, errors.New("Docker returned an incomplete or unexpected target")
	}
	return v, nil
}
func (c *Controller) Status(ctx context.Context) Status {
	out := Status{Enabled: c.Enabled(), SDKInstalled: true, Message: ErrDisabled.Error(), AllowedImages: []string{}}
	if !c.Enabled() {
		return out
	}
	out.SocketPath = c.opts.Socket
	out.ContainerID = c.opts.ContainerID
	// This is only an authenticated UI hint; Confirm/Execute still validate the
	// exact reference. Return a copy so callers cannot mutate controller policy.
	out.AllowedImages = append(out.AllowedImages, c.opts.AllowedImages...)
	v, err := c.inspect(ctx)
	if err != nil {
		out.Message = "Configured Docker target is unavailable"
		return out
	}
	out.SocketAvailable = true
	if err = c.validateMutation(v, "restart", ""); err != nil {
		out.Message = err.Error()
		return out
	}
	out.CanRestart = true
	if err = c.validateMutation(v, "update", rawString(v.Config, "Image")); err == nil {
		out.CanUpdate = true
	}
	out.Message = "Native Docker Engine API is available; changes require a one-use confirmation"
	return out
}
func (c *Controller) validateMutation(v Inspection, action, image string) error {
	c.mu.Lock()
	superseded := c.superseded
	c.mu.Unlock()
	if superseded {
		return errors.New("the target was replaced; explicitly configure the returned new container ID before another change")
	}
	if action != "restart" && action != "update" {
		return errors.New("action must be restart or update")
	}
	var labels map[string]string
	if err := json.Unmarshal(v.Config["Labels"], &labels); len(v.Config["Labels"]) > 0 && err != nil {
		return errors.New("invalid container labels")
	}
	for key := range labels {
		if strings.HasPrefix(key, "com.docker.compose.") || strings.HasPrefix(key, "com.docker.swarm.") {
			return ErrManaged
		}
	}
	if !c.allowed(rawString(v.Config, "Image")) {
		return errors.New("the target's exact image reference is not allowed")
	}
	if action == "restart" && image != "" {
		return errors.New("restart confirmation must not specify an image")
	}
	if action == "update" {
		if !c.opts.ExternalController {
			return ErrExternal
		}
		if !c.allowed(image) {
			return errors.New("the requested exact image reference is not allowed")
		}
		if rawString(v.Config, "Hostname") == c.hostname || strings.TrimPrefix(v.Name, "/") == c.hostname || strings.HasPrefix(v.ID, c.hostname) && len(c.hostname) >= 12 {
			return errors.New("refusing to recreate the controller's own container")
		}
		if rawBool(v.HostConfig, "AutoRemove") {
			return errors.New("AutoRemove targets cannot retain a rollback container")
		}
		if !v.State.Running {
			return errors.New("the target must be running before an update")
		}
		if _, err := recreateConfig(v, image); err != nil {
			return err
		}
	}
	return nil
}
func (c *Controller) fingerprint(v Inspection) string {
	b, _ := json.Marshal(struct {
		Options            Options
		ID, Name, Image    string
		Config, HostConfig map[string]json.RawMessage
		Mounts             []Mount
		Networks           map[string]map[string]json.RawMessage
	}{c.opts, v.ID, v.Name, v.Image, v.Config, v.HostConfig, v.Mounts, v.NetworkSettings.Networks})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
func (c *Controller) Confirm(ctx context.Context, user, action, image string) (Confirmation, error) {
	if user == "" {
		return Confirmation{}, errors.New("authenticated user is required")
	}
	v, err := c.inspect(ctx)
	if err != nil {
		return Confirmation{}, err
	}
	if err = c.validateMutation(v, action, image); err != nil {
		return Confirmation{}, err
	}
	var b [32]byte
	if _, err = rand.Read(b[:]); err != nil {
		return Confirmation{}, errors.New("cannot generate confirmation")
	}
	token := hex.EncodeToString(b[:])
	expires := c.now().Add(confirmationTTL)
	c.mu.Lock()
	defer c.mu.Unlock()
	for k, t := range c.pending {
		if !t.Expires.After(c.now()) {
			delete(c.pending, k)
		}
	}
	if len(c.pending) >= 256 {
		return Confirmation{}, errors.New("too many pending confirmations")
	}
	c.pending[sha256.Sum256([]byte(token))] = ticket{user, action, image, c.fingerprint(v), expires}
	warning := "Restarting interrupts active requests and jobs"
	if action == "update" {
		warning = "Updating stops the target, preserves its settings and volumes, and retains the stopped old container. Back up data first: application migrations may prevent data rollback"
	}
	return Confirmation{token, action, c.opts.ContainerID, image, expires, warning}, nil
}
func (c *Controller) consume(user, action, image, token string) (ticket, error) {
	if !c.Enabled() {
		return ticket{}, ErrDisabled
	}
	if len(token) != 64 {
		return ticket{}, ErrConfirmation
	}
	key := sha256.Sum256([]byte(token))
	c.mu.Lock()
	defer c.mu.Unlock()
	t, ok := c.pending[key]
	// Every attempt consumes the token, including mismatched users and actions.
	delete(c.pending, key)
	if !ok || !t.Expires.After(c.now()) || t.User != user || t.Action != action || t.Image != image {
		return ticket{}, ErrConfirmation
	}
	return t, nil
}
func (c *Controller) Execute(ctx context.Context, user, action, image, token string, emit func(Progress)) error {
	t, err := c.consume(user, action, image, token)
	if err != nil {
		return err
	}
	if !c.operation.TryLock() {
		return ErrBusy
	}
	defer c.operation.Unlock()
	v, err := c.inspect(ctx)
	if err != nil {
		return err
	}
	if c.fingerprint(v) != t.Fingerprint {
		return ErrChanged
	}
	if err = c.validateMutation(v, action, image); err != nil {
		return err
	}
	if action == "restart" {
		return c.engine.Restart(ctx, v.ID)
	}
	return c.update(ctx, v, image, t.Fingerprint, emit)
}
func (c *Controller) update(ctx context.Context, old Inspection, image, fingerprint string, emit func(Progress)) error {
	send := func(p Progress) {
		if emit != nil {
			emit(p)
		}
	}
	send(Progress{Status: "Pulling the explicitly allowed image", Progress: 5})
	if err := c.engine.Pull(ctx, image, emit); err != nil {
		return err
	}
	// Pulls can be slow. Never apply the previously confirmed snapshot after drift.
	current, err := c.inspect(ctx)
	if err != nil {
		return err
	}
	if c.fingerprint(current) != fingerprint {
		return ErrChanged
	}
	if err = c.validateMutation(current, "update", image); err != nil {
		return err
	}
	body, err := recreateConfig(old, image)
	if err != nil {
		return err
	}
	name := strings.TrimPrefix(old.Name, "/")
	if name == "" || len(name) > 200 {
		return errors.New("target has an invalid container name")
	}
	rollbackName := fmt.Sprintf("%s-anidan-rollback-%d-%s", name, c.now().UnixNano(), old.ID[:12])
	send(Progress{Status: "Stopping the old container; its volumes will be retained", Progress: 60})
	if err = ctx.Err(); err != nil {
		return err
	}
	renamed := false
	newID := ""
	// Rollback has its own bounded context so client cancellation cannot abandon
	// a stopped target. No old container or volume is ever deleted by this package.
	rollback := func(cause error) error {
		recovery, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer cancel()
		if newID != "" {
			if e := c.engine.Remove(recovery, newID); e != nil {
				return errors.New("update failed; replacement cleanup failed; old container is retained and manual recovery is required")
			}
		}
		if renamed {
			actual, inspectErr := c.engine.Inspect(recovery, old.ID)
			if inspectErr != nil || strings.TrimPrefix(actual.Name, "/") != name {
				if e := c.engine.Rename(recovery, old.ID, name); e != nil {
					return errors.New("update failed; rollback name restore failed; old container is retained and manual recovery is required")
				}
			}
		}
		if e := c.engine.Start(recovery, old.ID); e != nil {
			return errors.New("update failed; rollback start failed; old container is retained and manual recovery is required")
		}
		return fmt.Errorf("update failed; old container restarted: %w", cause)
	}
	if err = c.engine.Stop(ctx, old.ID); err != nil {
		// An interrupted response is not proof that Docker did not stop it.
		return rollback(err)
	}
	renamed = true // A failed response may still follow a successful rename.
	if err = c.engine.Rename(ctx, old.ID, rollbackName); err != nil {
		return rollback(err)
	}
	send(Progress{Status: "Creating the replacement with preserved configuration and volumes", Progress: 75})
	newID, err = c.engine.Create(ctx, name, body)
	if err != nil {
		return rollback(err)
	}
	if !ValidID(newID) || newID == old.ID {
		return errors.New("Docker returned an invalid replacement ID; old container is retained and manual recovery is required")
	}
	if err = c.engine.Start(ctx, newID); err != nil {
		return rollback(err)
	}
	// Do not report an update as successful merely because create/start returned.
	healthCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	for {
		replacement, e := c.engine.Inspect(healthCtx, newID)
		if e != nil {
			return rollback(e)
		}
		if replacement.ID != newID || !replacement.State.Running {
			return rollback(errors.New("replacement is not running"))
		}
		if replacement.State.Health == nil || replacement.State.Health.Status == "healthy" {
			break
		}
		if replacement.State.Health.Status == "unhealthy" {
			return rollback(errors.New("replacement is unhealthy"))
		}
		select {
		case <-healthCtx.Done():
			return rollback(errors.New("replacement health verification timed out"))
		case <-time.After(time.Second):
		}
	}
	c.mu.Lock()
	c.superseded = true
	clear(c.pending)
	c.mu.Unlock()
	send(Progress{Status: "Replacement is running; old container retained. Configure the new container ID before another change", Progress: 100, Event: "DONE", ContainerID: newID, RollbackContainerID: old.ID})
	return nil
}

func copyRaw(in map[string]json.RawMessage) map[string]json.RawMessage {
	out := make(map[string]json.RawMessage, len(in))
	for k, v := range in {
		out[k] = append(json.RawMessage(nil), v...)
	}
	return out
}
func putRaw(m map[string]json.RawMessage, key string, v any) { m[key], _ = json.Marshal(v) }
func recreateConfig(v Inspection, image string) (map[string]json.RawMessage, error) {
	body := copyRaw(v.Config)
	host := copyRaw(v.HostConfig)
	putRaw(body, "Image", image)
	var mounts []map[string]json.RawMessage
	if b := host["Mounts"]; len(b) > 0 && string(b) != "null" {
		if json.Unmarshal(b, &mounts) != nil {
			return nil, errors.New("invalid mount configuration")
		}
	}
	var binds []string
	if b := host["Binds"]; len(b) > 0 && string(b) != "null" {
		if json.Unmarshal(b, &binds) != nil {
			return nil, errors.New("invalid bind configuration")
		}
	}
	// Reuse anonymous volume names from inspect, rather than creating empty data
	// volumes from Config.Volumes when the new container is created.
	for _, m := range v.Mounts {
		if m.Type != "volume" {
			continue
		}
		if m.Name == "" || m.Destination == "" {
			return nil, errors.New("volume identity is missing; refusing lossy recreation")
		}
		found := false
		for _, mount := range mounts {
			if rawString(mount, "Target") == m.Destination {
				if rawString(mount, "Type") != "volume" {
					return nil, errors.New("conflicting mount configuration")
				}
				putRaw(mount, "Source", m.Name)
				found = true
			}
		}
		for i, b := range binds {
			parts := strings.Split(b, ":")
			dest := parts[0]
			if len(parts) >= 2 {
				dest = parts[1]
			}
			if dest == m.Destination {
				mode := "rw"
				if !m.RW {
					mode = "ro"
				}
				if len(parts) >= 3 {
					mode = parts[2]
				}
				binds[i] = m.Name + ":" + m.Destination + ":" + mode
				found = true
			}
		}
		if !found {
			mount := map[string]json.RawMessage{}
			putRaw(mount, "Type", "volume")
			putRaw(mount, "Source", m.Name)
			putRaw(mount, "Target", m.Destination)
			putRaw(mount, "ReadOnly", !m.RW)
			putRaw(mount, "VolumeOptions", map[string]bool{"NoCopy": true})
			mounts = append(mounts, mount)
		}
	}
	if len(mounts) > 0 {
		putRaw(host, "Mounts", mounts)
	}
	if len(binds) > 0 {
		putRaw(host, "Binds", binds)
	}
	putRaw(body, "HostConfig", host)
	endpoints := map[string]map[string]json.RawMessage{}
	for name, network := range v.NetworkSettings.Networks {
		ep := map[string]json.RawMessage{}
		for _, key := range []string{"IPAMConfig", "Links", "Aliases", "DriverOpts", "MacAddress", "GwPriority"} {
			if b, ok := network[key]; ok {
				ep[key] = b
			}
		}
		// Docker's generated old-container alias must not survive as a stale alias.
		var aliases []string
		_ = json.Unmarshal(ep["Aliases"], &aliases)
		clean := []string{}
		for _, a := range aliases {
			if a != v.ID && a != v.ID[:12] {
				clean = append(clean, a)
			}
		}
		if len(aliases) > 0 {
			putRaw(ep, "Aliases", clean)
		}
		endpoints[name] = ep
	}
	if len(endpoints) > 0 {
		putRaw(body, "NetworkingConfig", map[string]any{"EndpointsConfig": endpoints})
	}
	return body, nil
}
