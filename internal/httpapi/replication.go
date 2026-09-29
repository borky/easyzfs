// replication.go — endpoints de replicación ZFS send/recv (lote C).
// Mutaciones solo admin; la clave privada SSH nunca se expone (solo pública).
package httpapi

import (
	"errors"
	"net/http"
	"time"

	"easyzfs/internal/replication"
	"easyzfs/internal/scheduler"
)

// listReplication — GET /api/replication → jobs con next_run calculado.
func (s *Server) listReplication(w http.ResponseWriter, r *http.Request) {
	jobs, err := s.repl.Store().List(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "db_error", err.Error())
		return
	}
	now := time.Now()
	for i := range jobs {
		base := jobs[i].CreatedAt
		if jobs[i].LastRun != nil {
			base = *jobs[i].LastRun
		}
		if base.IsZero() {
			base = now
		}
		if next, err := scheduler.NextRun(jobs[i].Schedule, base); err == nil {
			jobs[i].NextRun = &next
		}
	}
	writeJSON(w, http.StatusOK, jobs)
}

// createReplication — POST /api/replication {source, dest_type, dest_dataset,
// host?, user?, port?, raw?, force_full?, schedule} → 201 {id}.
func (s *Server) createReplication(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Source      string `json:"source"`
		DestType    string `json:"dest_type"`
		DestDataset string `json:"dest_dataset"`
		Host        string `json:"host"`
		User        string `json:"user"`
		Port        int    `json:"port"`
		Raw         bool   `json:"raw"`
		ForceFull   bool   `json:"force_full"`
		Schedule    string `json:"schedule"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if body.DestType == "ssh" && body.Port == 0 {
		body.Port = 22
	}
	j := &replication.Job{
		Source: body.Source, DestType: body.DestType, DestDataset: body.DestDataset,
		Host: body.Host, User: body.User, Port: body.Port,
		Raw: body.Raw, ForceFull: body.ForceFull, Schedule: body.Schedule,
	}
	if err := j.Validate(); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_input", err.Error())
		return
	}
	if _, err := scheduler.ParseSchedule(body.Schedule); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_schedule", err.Error())
		return
	}
	id, err := s.repl.Store().Create(r.Context(), j)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "db_error", err.Error())
		return
	}
	s.act.AuditOnly(r.Context(), actor(r), "replication.create", replication.Target(j),
		map[string]any{"id": id, "schedule": body.Schedule, "raw": body.Raw})
	writeJSON(w, http.StatusCreated, map[string]any{"id": id})
}

// patchReplication — PATCH /api/replication/{id} {enabled?, schedule?,
// force_full?, raw?} → 204.
func (s *Server) patchReplication(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(w, r)
	if !ok {
		return
	}
	var body struct {
		Enabled   *bool   `json:"enabled"`
		Schedule  *string `json:"schedule"`
		ForceFull *bool   `json:"force_full"`
		Raw       *bool   `json:"raw"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if body.Schedule != nil {
		if _, err := scheduler.ParseSchedule(*body.Schedule); err != nil {
			writeErr(w, http.StatusBadRequest, "invalid_schedule", err.Error())
			return
		}
	}
	if _, err := s.repl.Store().Get(r.Context(), id); err != nil {
		writeErr(w, http.StatusNotFound, "not_found", "replication job not found")
		return
	}
	if err := s.repl.Store().Update(r.Context(), id, body.Enabled, body.ForceFull, body.Raw, body.Schedule); err != nil {
		writeErr(w, http.StatusInternalServerError, "db_error", err.Error())
		return
	}
	s.act.AuditOnly(r.Context(), actor(r), "replication.patch", r.PathValue("id"), body)
	w.WriteHeader(http.StatusNoContent)
}

// deleteReplication — DELETE /api/replication/{id} {confirm:"<source>"} → 204.
// Solo borra la definición: no toca snapshots, bookmarks ni el destino.
func (s *Server) deleteReplication(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(w, r)
	if !ok {
		return
	}
	j, err := s.repl.Store().Get(r.Context(), id)
	if err != nil {
		writeErr(w, http.StatusNotFound, "not_found", "replication job not found")
		return
	}
	var body struct {
		Confirm string `json:"confirm"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if !requireConfirm(w, body.Confirm, j.Source) {
		return
	}
	if err := s.repl.Store().Delete(r.Context(), id); err != nil {
		writeErr(w, http.StatusInternalServerError, "db_error", err.Error())
		return
	}
	s.act.AuditOnly(r.Context(), actor(r), "replication.delete", replication.Target(j), map[string]any{"id": id})
	w.WriteHeader(http.StatusNoContent)
}

// runReplication — POST /api/replication/{id}/run → 202 {op_id} (lanza la
// ejecución en segundo plano; 409 already_running si ya hay una del job).
func (s *Server) runReplication(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(w, r)
	if !ok {
		return
	}
	if err := s.repl.RunNow(r.Context(), id); err != nil {
		switch {
		case errors.Is(err, replication.ErrNotFound):
			writeErr(w, http.StatusNotFound, "not_found", "replication job not found")
		case errors.Is(err, replication.ErrAlreadyRunning):
			writeErr(w, http.StatusConflict, "already_running", err.Error())
		default:
			writeErr(w, http.StatusInternalServerError, "db_error", err.Error())
		}
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

// getReplicationSSHKey — GET /api/replication/sshkey → clave pública ed25519
// del daemon (la genera al primer uso) + instrucciones de instalación. La
// privada jamás sale del servidor.
func (s *Server) getReplicationSSHKey(w http.ResponseWriter, r *http.Request) {
	pub, err := s.repl.EnsureSSHKey()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "exec_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"public_key": pub,
		"instructions": "Add this key to ~/.ssh/authorized_keys of the destination user. " +
			"To avoid root: zfs allow -u <user> snapshot,send,receive,destroy,hold,bookmark <pool>",
	})
}

// testReplication — POST /api/replication/test {host, user, port} →
// {ok:true, remote_version} o {ok:false, error} legible (auth/red/zfs ausente).
func (s *Server) testReplication(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Host string `json:"host"`
		User string `json:"user"`
		Port int    `json:"port"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if body.Port == 0 {
		body.Port = 22
	}
	version, err := s.repl.TestConnection(r.Context(), body.Host, body.User, body.Port)
	if err != nil {
		code := "invalid_input"
		if errors.Is(err, replication.ErrNotFound) {
			code = "not_found"
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error(), "code": code})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "remote_version": version})
}
