// longops.go — operaciones largas: listado, cancelación y lanzamiento de
// 'zfs rewrite' como operación monitorizada (runner internal/longops).
package httpapi

import (
	"errors"
	"net/http"

	"easyzfs/internal/actions"
	"easyzfs/internal/longops"
)

// listLongOps — GET /api/longops → lista (más reciente primero; TTL 1 h en
// las terminadas; el registro es en memoria: un reinicio las pierde).
func (s *Server) listLongOps(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.longOps.List())
}

// cancelLongOp — POST /api/longops/{id}/cancel (admin) → 204.
func (s *Server) cancelLongOp(w http.ResponseWriter, r *http.Request) {
	err := s.longOps.Cancel(r.PathValue("id"))
	switch {
	case errors.Is(err, longops.ErrNotFound):
		writeErr(w, http.StatusNotFound, "not_found", err.Error())
	case errors.Is(err, longops.ErrNotRunning):
		writeErr(w, http.StatusConflict, "not_running", err.Error())
	case err != nil:
		actionErr(w, err)
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

// rewriteDataset — POST /api/datasets/{name}/rewrite {confirm:"<dataset>"}
// (admin) → 202 {op_id}. Gate: solo si capabilities.rewrite (OpenZFS ≥ 2.3.4);
// si no, 400 not_supported (y la UI no muestra el botón).
//
// Lanza 'zfs rewrite -r -x <mountpoint>' vía longops (proceso desacoplado:
// sobrevive a esta request; se observa en GET /api/longops / longop.update).
func (s *Server) rewriteDataset(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var body struct {
		Confirm string `json:"confirm"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if !s.caps.Capabilities().Rewrite {
		writeErr(w, http.StatusBadRequest, "not_supported",
			"zfs rewrite requiere OpenZFS ≥ 2.3.4 en este host")
		return
	}
	if !requireConfirm(w, body.Confirm, name) {
		return
	}
	// Rewriting the running OS or a guest disk under a live VM is not a
	// maintenance task EasyZFS should start (analysis §21). A mock
	// deployment has no host to read.
	if !s.cfg.Mock {
		if err := actions.CheckHostDataset(r.Context(), actions.OpDatasetRemove, name); err != nil {
			actionErr(w, err)
			return
		}
	}
	// El dataset debe existir, ser filesystem y estar montado (rewrite actúa
	// sobre el árbol de ficheros del mountpoint).
	mount := ""
	for _, d := range s.pools.Datasets() {
		if d.Name == name {
			if d.Type == "fs" && d.Mountpoint != "" && d.Mountpoint != "-" &&
				d.Mountpoint != "none" && d.Mountpoint != "legacy" {
				mount = d.Mountpoint
			}
			break
		}
	}
	if mount == "" {
		writeErr(w, http.StatusBadRequest, "invalid_input",
			"dataset inexistente, no es filesystem o no está montado")
		return
	}
	if s.longOps.RunningFor(name) {
		writeErr(w, http.StatusConflict, "already_running",
			"ya hay una operación en curso sobre "+name)
		return
	}
	s.act.AuditOnly(r.Context(), actor(r), "dataset.rewrite", name, map[string]any{"mountpoint": mount})
	var op *longops.Op
	var err error
	if s.cfg.Mock {
		// MOCK=1: sin zfs real; op simulada que completa a los pocos segundos.
		op, err = s.longOps.Start("rewrite", name, "sleep", "4")
	} else {
		op, err = s.longOps.Start("rewrite", name, "zfs", "rewrite", "-r", "-x", mount)
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "exec_error",
			"lanzar rewrite: "+err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"op_id": op.ID})
}
