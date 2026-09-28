// trash.go — the recycle bin (internal/actions/trash.go): list, restore,
// purge. The list comes from the database, the sizes from the collector's
// dataset cache; nothing here runs a command outside the actions.
package httpapi

import (
	"net/http"
	"strconv"

	"easyzfs/internal/actions"
)

type trashItem struct {
	actions.TrashEntry
	// UsedBytes — what emptying it would free, from the dataset cache;
	// null when the cache does not know the dataset (yet).
	UsedBytes *uint64 `json:"used_bytes"`
}

// listTrash — GET /api/trash.
func (s *Server) listTrash(w http.ResponseWriter, r *http.Request) {
	list, err := s.act.TrashList(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "db_error", err.Error())
		return
	}
	used := map[string]uint64{}
	for _, d := range s.pools.Datasets() {
		used[d.Name] = d.UsedBytes
	}
	out := make([]trashItem, 0, len(list))
	for _, e := range list {
		it := trashItem{TrashEntry: e}
		if u, ok := used[e.Trashed]; ok {
			it.UsedBytes = &u
		}
		out = append(out, it)
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": out, "days": actions.TrashDays})
}

func trashID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeErr(w, http.StatusBadRequest, "invalid_input", "id no válido")
		return 0, false
	}
	return id, true
}

// restoreTrash — POST /api/trash/{id}/restore → 204.
func (s *Server) restoreTrash(w http.ResponseWriter, r *http.Request) {
	id, ok := trashID(w, r)
	if !ok {
		return
	}
	if err := s.act.TrashRestore(r.Context(), actor(r), id); err != nil {
		actionErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// purgeTrash — DELETE /api/trash/{id} {confirm:"<original name>"} → 204.
// Irreversible: re-authentication at registration.
func (s *Server) purgeTrash(w http.ResponseWriter, r *http.Request) {
	id, ok := trashID(w, r)
	if !ok {
		return
	}
	var body struct {
		Confirm string `json:"confirm"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	list, err := s.act.TrashList(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "db_error", err.Error())
		return
	}
	target := ""
	for _, e := range list {
		if e.ID == id {
			target = e.Original
		}
	}
	if target == "" {
		writeErr(w, http.StatusNotFound, "not_found", "no existe en la papelera")
		return
	}
	if !requireConfirm(w, body.Confirm, target) {
		return
	}
	if err := s.act.TrashPurge(r.Context(), actor(r), id); err != nil {
		actionErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
