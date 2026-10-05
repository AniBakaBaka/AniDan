// SPDX-License-Identifier: AGPL-3.0-only
// Legacy endpoint names adapted from misaka_danmu_server/src/api/ui/system.py.
// Mutations intentionally require an explicit, short-lived confirmation.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/AniBakaBaka/AniDan/internal/containerctl"
	"github.com/AniBakaBaka/AniDan/internal/store"
)

func (s *Server) initContainer() error {
	var err error
	s.Container, err = containerctl.New(containerctl.Options{Enabled: s.Config.DockerEnabled, Socket: s.Config.DockerSocket, ContainerID: s.Config.DockerContainerID, AllowedImages: s.Config.DockerAllowedImages, ExternalController: s.Config.DockerExternalController})
	if err != nil {
		return err
	}
	s.Releases, err = containerctl.NewReleases(s.Config.ReleaseRepository, nil)
	return err
}
func (s *Server) registerContainer(m *http.ServeMux) {
	m.HandleFunc("GET /api/ui/docker/status", s.dockerStatus)
	m.HandleFunc("GET /api/ui/docker/stats", s.dockerStats)
	m.HandleFunc("POST /api/ui/docker/confirm", s.dockerConfirm)
	m.HandleFunc("POST /api/ui/restart", s.dockerRestart)
	m.HandleFunc("GET /api/ui/update/stream", s.dockerUpdate)
	m.HandleFunc("POST /api/ui/update/stream", s.dockerUpdate)
}

// Privileged Docker operations require a real authenticated UI identity. External
// API keys, URL tokens and trusted-IP bypasses do not grant daemon access.
func (s *Server) containerUser(w http.ResponseWriter, r *http.Request) (store.Row, bool) {
	user, _, err := s.authClaims(r)
	if err != nil {
		w.Header().Set("WWW-Authenticate", "Bearer")
		httpError(w, 401, "Authenticated UI session required")
		return nil, false
	}
	w.Header().Set("Cache-Control", "no-store")
	return user, true
}
func (s *Server) dockerStatus(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.containerUser(w, r); !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()
	writeJSON(w, 200, s.Container.Status(ctx))
}
func containerHTTPError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, containerctl.ErrDisabled):
		httpError(w, 503, err.Error())
	case errors.Is(err, containerctl.ErrConfirmation):
		httpError(w, 428, err.Error())
	case errors.Is(err, containerctl.ErrChanged), errors.Is(err, containerctl.ErrBusy):
		httpError(w, 409, err.Error())
	case errors.Is(err, containerctl.ErrManaged), errors.Is(err, containerctl.ErrExternal):
		httpError(w, 409, err.Error())
	default:
		httpError(w, 502, "Container operation could not be completed; check the configured target and image, and inspect the retained container if recovery is needed")
	}
}
func (s *Server) dockerConfirm(w http.ResponseWriter, r *http.Request) {
	user, ok := s.containerUser(w, r)
	if !ok {
		return
	}
	if !s.Container.Enabled() {
		containerHTTPError(w, containerctl.ErrDisabled)
		return
	}
	var request struct {
		Action string `json:"action"`
		Image  string `json:"image"`
	}
	dec := json.NewDecoder(io.LimitReader(r.Body, 4097))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&request); err != nil {
		httpError(w, 400, "Expected action and optional exact image reference")
		return
	}
	var extra any
	if dec.Decode(&extra) != io.EOF {
		httpError(w, 400, "Expected one JSON object")
		return
	}
	if request.Action != "restart" && request.Action != "update" {
		httpError(w, 400, "action must be restart or update")
		return
	}
	if request.Action == "update" && !containerctl.ValidImage(request.Image) || request.Action == "restart" && request.Image != "" {
		httpError(w, 400, "Updates require an exact fully qualified image; restarts do not accept an image")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()
	confirmation, err := s.Container.Confirm(ctx, str(user["id"]), request.Action, request.Image)
	if err != nil {
		containerHTTPError(w, err)
		return
	}
	writeJSON(w, 200, confirmation)
}
func confirmationHeader(r *http.Request) string {
	return strings.TrimSpace(r.Header.Get("X-AniDan-Confirmation"))
}
func (s *Server) dockerRestart(w http.ResponseWriter, r *http.Request) {
	user, ok := s.containerUser(w, r)
	if !ok {
		return
	}
	if confirmationHeader(r) == "" {
		containerHTTPError(w, containerctl.ErrConfirmation)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	if err := s.Container.Execute(ctx, str(user["id"]), "restart", "", confirmationHeader(r), nil); err != nil {
		containerHTTPError(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"success": true, "message": "Docker Engine accepted the configured target restart", "method": "docker_api"})
}
func (s *Server) dockerStats(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.containerUser(w, r); !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Minute)
	defer cancel()
	stop := context.AfterFunc(s.ctx, cancel)
	defer stop()
	send := containerSSE(w, cancel)
	if !s.Container.Enabled() {
		send(map[string]any{"available": false, "message": "Container management is disabled"})
		return
	}
	var previous *containerctl.Stats
	for {
		sampleCtx, sampleCancel := context.WithTimeout(ctx, 8*time.Second)
		sample, raw, err := s.Container.Sample(sampleCtx, previous)
		sampleCancel()
		if err != nil {
			if ctx.Err() == nil {
				send(map[string]any{"available": false, "message": "Configured Docker target statistics are unavailable"})
			}
			return
		}
		if !send(sample) {
			return
		}
		previous = &raw
		select {
		case <-ctx.Done():
			return
		case <-time.After(2 * time.Second):
		}
	}
}
func containerSSE(w http.ResponseWriter, cancel context.CancelFunc) func(any) bool {
	started := false
	return func(v any) bool {
		if !started {
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Cache-Control", "no-store")
			w.Header().Set("X-Accel-Buffering", "no")
			w.WriteHeader(200)
			started = true
		}
		controller := http.NewResponseController(w)
		_ = controller.SetWriteDeadline(time.Now().Add(15 * time.Second))
		body, err := json.Marshal(v)
		if err == nil {
			_, err = fmt.Fprintf(w, "data: %s\n\n", body)
		}
		if err == nil {
			err = controller.Flush()
		}
		if err != nil {
			cancel()
			return false
		}
		return true
	}
}
func (s *Server) dockerUpdate(w http.ResponseWriter, r *http.Request) {
	user, ok := s.containerUser(w, r)
	if !ok {
		return
	}
	if confirmationHeader(r) == "" {
		containerHTTPError(w, containerctl.ErrConfirmation)
		return
	}
	image := r.URL.Query().Get("image")
	if !containerctl.ValidImage(image) || r.URL.Query().Get("source") != "" {
		httpError(w, 400, "Use the exact confirmed image; inferred update sources are not supported")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Minute)
	defer cancel()
	stop := context.AfterFunc(s.ctx, cancel)
	defer stop()
	send := containerSSE(w, cancel)
	started := false
	err := s.Container.Execute(ctx, str(user["id"]), "update", image, confirmationHeader(r), func(p containerctl.Progress) { started = true; send(p) })
	if err != nil {
		if !started {
			containerHTTPError(w, err)
			return
		}
		send(containerctl.Progress{Status: "Update failed. The old container is retained; inspect its state before retrying or performing manual recovery", Event: "ERROR"})
	}
}
