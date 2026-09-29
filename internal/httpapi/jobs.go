// jobs.go — endpoints de tareas programadas.
package httpapi

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"easyzfs/internal/scheduler"
)

// listJobs — GET /api/jobs (con next_run calculado).
func (s *Server) listJobs(w http.ResponseWriter, r *http.Request) {
	jobs, err := s.jstore.List(r.Context())
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

// createJob — POST /api/jobs {tipo, target, schedule, retention?} → 201.
func (s *Server) createJob(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Tipo      string `json:"tipo"`
		Target    string `json:"target"`
		Schedule  string `json:"schedule"`
		Retention string `json:"retention"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if !scheduler.ValidTipos[body.Tipo] {
		writeErr(w, http.StatusBadRequest, "invalid_input",
			"invalid type (snapshot|scrub|trim|smart_short|smart_long)")
		return
	}
	if body.Target == "" {
		writeErr(w, http.StatusBadRequest, "invalid_input", "target required")
		return
	}
	if _, err := scheduler.ParseSchedule(body.Schedule); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_schedule", err.Error())
		return
	}
	if body.Retention != "" {
		if _, err := scheduler.ParseRetention(body.Retention); err != nil {
			writeErr(w, http.StatusBadRequest, "invalid_retention", err.Error())
			return
		}
	}
	j := &scheduler.Job{Tipo: body.Tipo, Target: body.Target,
		Schedule: body.Schedule, Retention: body.Retention}
	id, err := s.jstore.Create(r.Context(), j)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "db_error", err.Error())
		return
	}
	s.act.AuditOnly(r.Context(), actor(r), "job.create", body.Target,
		map[string]any{"id": id, "tipo": body.Tipo, "schedule": body.Schedule})
	writeJSON(w, http.StatusCreated, map[string]any{"id": id})
}

// patchJob — PATCH /api/jobs/{id} {enabled?, schedule?, retention?} → 204.
func (s *Server) patchJob(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(w, r)
	if !ok {
		return
	}
	var body struct {
		Enabled   *bool   `json:"enabled"`
		Schedule  *string `json:"schedule"`
		Retention *string `json:"retention"`
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
	if body.Retention != nil && *body.Retention != "" {
		if _, err := scheduler.ParseRetention(*body.Retention); err != nil {
			writeErr(w, http.StatusBadRequest, "invalid_retention", err.Error())
			return
		}
	}
	if _, err := s.jstore.Get(r.Context(), id); err != nil {
		writeErr(w, http.StatusNotFound, "not_found", "job not found")
		return
	}
	if err := s.jstore.Update(r.Context(), id, body.Enabled, body.Schedule, body.Retention); err != nil {
		writeErr(w, http.StatusInternalServerError, "db_error", err.Error())
		return
	}
	s.act.AuditOnly(r.Context(), actor(r), "job.patch", strconv.FormatInt(id, 10), body)
	w.WriteHeader(http.StatusNoContent)
}

// deleteJob — DELETE /api/jobs/{id} {confirm} → 204. confirm = target del job.
func (s *Server) deleteJob(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(w, r)
	if !ok {
		return
	}
	j, err := s.jstore.Get(r.Context(), id)
	if err != nil {
		writeErr(w, http.StatusNotFound, "not_found", "job not found")
		return
	}
	var body struct {
		Confirm string `json:"confirm"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if !requireConfirm(w, body.Confirm, j.Target) {
		return
	}
	if err := s.jstore.Delete(r.Context(), id); err != nil {
		writeErr(w, http.StatusInternalServerError, "db_error", err.Error())
		return
	}
	s.act.AuditOnly(r.Context(), actor(r), "job.delete", j.Target, map[string]any{"id": id})
	w.WriteHeader(http.StatusNoContent)
}

// runJob — POST /api/jobs/{id}/run → 202 (ejecución en segundo plano).
func (s *Server) runJob(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(w, r)
	if !ok {
		return
	}
	if err := s.sched.RunNow(r.Context(), id); err != nil {
		if errors.Is(err, scheduler.ErrNotFound) {
			writeErr(w, http.StatusNotFound, "not_found", "job not found")
			return
		}
		writeErr(w, http.StatusInternalServerError, "db_error", err.Error())
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

// jobsHistory — GET /api/jobs/history → últimas 100 ejecuciones.
func (s *Server) jobsHistory(w http.ResponseWriter, r *http.Request) {
	hist, err := s.jstore.History(r.Context(), 100)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "db_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, hist)
}

// parseID — {id} de path como int64; false = error ya escrito.
func parseID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_id", "invalid job id")
		return 0, false
	}
	return id, true
}
