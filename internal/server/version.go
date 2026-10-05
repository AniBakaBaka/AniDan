// SPDX-License-Identifier: AGPL-3.0-only
package server

import (
	"net/http"
	"strconv"
)

func (s *Server) registerVersion(m *http.ServeMux) {
	m.HandleFunc("GET /api/ui/version/check", s.versionCheck)
	m.HandleFunc("GET /api/ui/version/releases", s.versionReleases)
}
func releaseForce(r *http.Request) (bool, error) {
	if r.URL.Query().Get("force_refresh") == "" {
		return false, nil
	}
	return strconv.ParseBool(r.URL.Query().Get("force_refresh"))
}
func (s *Server) versionCheck(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.containerUser(w, r); !ok {
		return
	}
	force, err := releaseForce(r)
	if err != nil {
		httpError(w, 400, "force_refresh must be true or false")
		return
	}
	result, err := s.Releases.Check(r.Context(), Version, force)
	if err != nil {
		httpError(w, 502, "Configured GitHub release check failed; no update availability is being claimed")
		return
	}
	writeJSON(w, 200, result)
}
func (s *Server) versionReleases(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.containerUser(w, r); !ok {
		return
	}
	limit := 10
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 50 {
			httpError(w, 400, "limit must be 1..50")
			return
		}
		limit = n
	}
	force, err := releaseForce(r)
	if err != nil {
		httpError(w, 400, "force_refresh must be true or false")
		return
	}
	releases, err := s.Releases.List(r.Context(), limit, force)
	if err != nil {
		httpError(w, 502, "Configured GitHub release history is unavailable")
		return
	}
	result := map[string]any{"releases": releases, "configured": s.Releases.Configured()}
	if !s.Releases.Configured() {
		result["message"] = "Release repository is not configured; no request was made"
	}
	writeJSON(w, 200, result)
}
