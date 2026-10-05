// SPDX-License-Identifier: AGPL-3.0-only
package server

import (
	"errors"
	"github.com/AniBakaBaka/AniDan/internal/danmaku"
	"net/http"
	"os"
	"strconv"
)

// uiComments preserves the legacy unauthenticated UI list envelope. Player and
// external-control comment endpoints have distinct envelopes and are unchanged.
func (s *Server) uiComments(w http.ResponseWriter, r *http.Request) {
	id, e := idParam(r, "episodeId")
	if e != nil {
		httpError(w, 422, e.Error())
		return
	}
	page, size := 1, 100
	for key, dst := range map[string]*int{"page": &page, "pageSize": &size} {
		if v := r.URL.Query().Get(key); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n < 1 {
				httpError(w, 422, "page and pageSize must be positive integers")
				return
			}
			*dst = n
		}
	}
	if size > 10000 {
		httpError(w, 422, "pageSize exceeds10000 bound")
		return
	}
	ep, e := s.Store.Get(r.Context(), "episode", id)
	if e != nil || ep == nil {
		httpError(w, 404, "Episode not found")
		return
	}
	comments, e := s.readComments(r.Context(), ep)
	if errors.Is(e, os.ErrNotExist) {
		comments = []danmaku.Comment{}
		e = nil
	}
	if e != nil {
		httpError(w, 500, "Cannot read comment file")
		return
	}
	start := len(comments)
	if page-1 <= len(comments)/size {
		start = (page - 1) * size
		if start > len(comments) {
			start = len(comments)
		}
	}
	end := start + size
	if end > len(comments) {
		end = len(comments)
	}
	out := make([]PlayerComment, 0, end-start)
	for i, c := range comments[start:end] {
		out = append(out, PlayerComment{CID: int64(start + i), P: c.P, M: c.M})
	}
	writeJSON(w, 200, map[string]any{"total": len(comments), "list": out})
}
