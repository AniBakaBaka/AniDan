// SPDX-License-Identifier: AGPL-3.0-only
package server

import (
	"database/sql"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/AniBakaBaka/AniDan/internal/store"
)

func (s *Server) registerLibraryGroups(m *http.ServeMux) {
	m.HandleFunc("GET /api/ui/anime/groups", s.libGroups)
	m.HandleFunc("POST /api/ui/anime/groups", s.libCreateGroup)
	m.HandleFunc("PATCH /api/ui/anime/groups/reorder", s.libReorderGroups)
	m.HandleFunc("PATCH /api/ui/anime/groups/{group_id}", s.libRenameGroup)
	m.HandleFunc("DELETE /api/ui/anime/groups/{group_id}", s.libDeleteGroup)
	// Equivalent to PATCH /api/ui/anime/{anime_id}/group. ServeMux cannot
	// express this alongside groups/{group_id} without an ambiguous overlap.
	m.HandleFunc("PATCH /api/ui/anime/{rest...}", func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(strings.Trim(r.PathValue("rest"), "/"), "/")
		if len(parts) != 2 || parts[1] != "group" {
			httpError(w, 404, "Not found")
			return
		}
		r.SetPathValue("anime_id", parts[0])
		s.libSetGroup(w, r)
	})
}
func libGroupPublic(v store.Row) store.Row {
	return store.Row{"id": v["id"], "name": v["name"], "sortOrder": v["sort_order"]}
}
func (s *Server) libGroups(w http.ResponseWriter, r *http.Request) {
	if !s.libAuthorized(w, r) {
		return
	}
	rows, e := s.libRows(r.Context(), s.Store.DB, "anime_groups", "SELECT * FROM anime_groups ORDER BY sort_order,created_at,id")
	if e != nil {
		libWriteError(w, e)
		return
	}
	out := []store.Row{}
	for _, v := range rows {
		out = append(out, libGroupPublic(v))
	}
	writeJSON(w, 200, out)
}
func libGroupName(r *http.Request) (string, error) {
	var b struct {
		Name string `json:"name"`
	}
	if readJSON(r, &b) != nil || strings.TrimSpace(b.Name) == "" || utf8.RuneCountInString(b.Name) > 200 {
		return "", libErr(422, "name must contain 1..200 characters")
	}
	return b.Name, nil
}
func (s *Server) libCreateGroup(w http.ResponseWriter, r *http.Request) {
	if !s.libAuthorized(w, r) {
		return
	}
	name, e := libGroupName(r)
	if e != nil {
		libWriteError(w, e)
		return
	}
	var id int64
	e = s.libTransaction(r.Context(), func(tx *sql.Tx) error {
		var order int64
		if e := tx.QueryRowContext(r.Context(), "SELECT COALESCE(MAX(sort_order),0)+1 FROM anime_groups").Scan(&order); e != nil {
			return e
		}
		var e error
		id, e = s.Store.InsertTx(r.Context(), tx, "anime_groups", store.Row{"name": name, "sort_order": order, "created_at": s.authNow()})
		return e
	})
	if e != nil {
		libWriteError(w, e)
		return
	}
	row, e := s.Store.Get(r.Context(), "anime_groups", id)
	if e != nil {
		libWriteError(w, e)
		return
	}
	writeJSON(w, 201, libGroupPublic(row))
}
func (s *Server) libRenameGroup(w http.ResponseWriter, r *http.Request) {
	if !s.libAuthorized(w, r) {
		return
	}
	id, e := idParam(r, "group_id")
	if e != nil {
		httpError(w, 422, "Invalid group ID")
		return
	}
	name, e := libGroupName(r)
	if e != nil {
		libWriteError(w, e)
		return
	}
	e = s.libTransaction(r.Context(), func(tx *sql.Tx) error {
		if _, e := s.libOne(r.Context(), tx, "anime_groups", "id = ?", id); e != nil {
			return e
		}
		return s.libUpdate(r.Context(), tx, "anime_groups", id, store.Row{"name": name})
	})
	if e != nil {
		libWriteError(w, e)
		return
	}
	row, e := s.Store.Get(r.Context(), "anime_groups", id)
	if e != nil {
		libWriteError(w, e)
		return
	}
	writeJSON(w, 200, libGroupPublic(row))
}
func (s *Server) libDeleteGroup(w http.ResponseWriter, r *http.Request) {
	if !s.libAuthorized(w, r) {
		return
	}
	id, e := idParam(r, "group_id")
	if e != nil {
		httpError(w, 422, "Invalid ID")
		return
	}
	e = s.libTransaction(r.Context(), func(tx *sql.Tx) error {
		if _, e := s.libOne(r.Context(), tx, "anime_groups", "id = ?", id); e != nil {
			return e
		}
		_, e := tx.ExecContext(r.Context(), s.Store.Rebind("DELETE FROM anime_groups WHERE id = ?"), id)
		return e
	})
	if e != nil {
		libWriteError(w, e)
		return
	}
	w.WriteHeader(204)
}
func (s *Server) libReorderGroups(w http.ResponseWriter, r *http.Request) {
	if !s.libAuthorized(w, r) {
		return
	}
	var b struct {
		IDs []int64 `json:"groupIds"`
	}
	if readJSON(r, &b) != nil || len(b.IDs) > 10000 {
		httpError(w, 422, "Invalid groupIds")
		return
	}
	seen := map[int64]bool{}
	e := s.libTransaction(r.Context(), func(tx *sql.Tx) error {
		for i, id := range b.IDs {
			if seen[id] {
				return libErr(422, "Duplicate group ID")
			}
			seen[id] = true
			if _, e := s.libOne(r.Context(), tx, "anime_groups", "id = ?", id); e != nil {
				return e
			}
			if e := s.libUpdate(r.Context(), tx, "anime_groups", id, store.Row{"sort_order": i}); e != nil {
				return e
			}
		}
		return nil
	})
	if e != nil {
		libWriteError(w, e)
		return
	}
	writeJSON(w, 200, store.Row{"success": true})
}
func (s *Server) libSetGroup(w http.ResponseWriter, r *http.Request) {
	if !s.libAuthorized(w, r) {
		return
	}
	id, e := idParam(r, "anime_id")
	if e != nil {
		httpError(w, 422, "Invalid ID")
		return
	}
	var b struct {
		ID *int64 `json:"groupId"`
	}
	if readJSON(r, &b) != nil {
		httpError(w, 422, "Invalid groupId")
		return
	}
	e = s.libTransaction(r.Context(), func(tx *sql.Tx) error {
		if _, e := s.libOne(r.Context(), tx, "anime", "id = ?", id); e != nil {
			return e
		}
		var gid any
		if b.ID != nil {
			if _, e := s.libOne(r.Context(), tx, "anime_groups", "id = ?", *b.ID); e != nil {
				return e
			}
			gid = *b.ID
		}
		return s.libUpdate(r.Context(), tx, "anime", id, store.Row{"group_id": gid})
	})
	if e != nil {
		libWriteError(w, e)
		return
	}
	writeJSON(w, 200, store.Row{"success": true, "animeId": id, "groupId": b.ID})
}
