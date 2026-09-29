// users_handlers.go — gestión de usuarios (solo admin).
package httpapi

import (
	"errors"
	"net/http"

	"easyzfs/internal/auth"
	"easyzfs/internal/users"
)

// listUsers — GET /api/users → [{user, role, last_login, sessions}]
func (s *Server) listUsers(w http.ResponseWriter, r *http.Request) {
	list, err := s.users.List(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "db_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, list)
}

// createUser — POST /api/users {user, password, role} → 201. 409 si existe.
func (s *Server) createUser(w http.ResponseWriter, r *http.Request) {
	var body struct {
		User     string `json:"user"`
		Password string `json:"password"`
		Role     string `json:"role"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if body.Role == "" {
		body.Role = "user"
	}
	if err := s.users.Create(r.Context(), body.User, body.Password, body.Role); err != nil {
		switch {
		case errors.Is(err, users.ErrExists):
			writeErr(w, http.StatusConflict, "user_exists", err.Error())
		case errors.Is(err, users.ErrInvalidName), errors.Is(err, users.ErrInvalidRole),
			errors.Is(err, users.ErrWeakPassword):
			writeErr(w, http.StatusBadRequest, "invalid_input", err.Error())
		default:
			writeErr(w, http.StatusInternalServerError, "db_error", err.Error())
		}
		return
	}
	s.act.AuditOnly(r.Context(), actor(r), "user.create", body.User, map[string]any{"role": body.Role})
	writeJSON(w, http.StatusCreated, map[string]string{"user": body.User, "role": body.Role})
}

// deleteUser — DELETE /api/users/{name} {confirm} → 204.
// No puede borrarse a sí mismo ni al último admin.
func (s *Server) deleteUser(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var body struct {
		Confirm string `json:"confirm"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if !requireConfirm(w, body.Confirm, name) {
		return
	}
	self := auth.UserFromContext(r.Context())
	if name == self {
		writeErr(w, http.StatusBadRequest, "self_delete", "no puedes borrar tu propio usuario")
		return
	}
	target, err := s.users.Get(r.Context(), name)
	if err != nil {
		writeErr(w, http.StatusNotFound, "not_found", "usuario no encontrado")
		return
	}
	if target.Role == "admin" {
		n, err := s.users.CountAdmins(r.Context())
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "db_error", err.Error())
			return
		}
		if n <= 1 {
			writeErr(w, http.StatusBadRequest, "last_admin", "no se puede borrar al último admin")
			return
		}
	}
	if err := s.users.Delete(r.Context(), name); err != nil {
		writeErr(w, http.StatusInternalServerError, "db_error", err.Error())
		return
	}
	forgetUILang(name)
	s.act.AuditOnly(r.Context(), actor(r), "user.delete", name, nil)
	w.WriteHeader(http.StatusNoContent)
}

// setUserLanguage — PUT /api/users/{name}/language {language} → 204 (admin).
// Cualquier admin puede asignar el idioma de cualquier usuario.
func (s *Server) setUserLanguage(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var body struct {
		Language string `json:"language"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if err := s.users.SetLanguage(r.Context(), name, body.Language); err != nil {
		switch {
		case errors.Is(err, users.ErrInvalidLang):
			writeErr(w, http.StatusBadRequest, "invalid_language", err.Error())
		case errors.Is(err, users.ErrNotFound):
			writeErr(w, http.StatusNotFound, "not_found", err.Error())
		default:
			writeErr(w, http.StatusInternalServerError, "db_error", err.Error())
		}
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// setUserPassword — POST /api/users/{name}/password {new, close_sessions?} → 204 (admin).
// close_sessions por defecto es true (si el campo no viene, se cierran las sesiones).
func (s *Server) setUserPassword(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var body struct {
		New           string `json:"new"`
		CloseSessions *bool  `json:"close_sessions"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	closeSessions := true
	if body.CloseSessions != nil {
		closeSessions = *body.CloseSessions
	}
	if err := s.users.SetPassword(r.Context(), name, body.New); err != nil {
		switch {
		case errors.Is(err, users.ErrNotFound):
			writeErr(w, http.StatusNotFound, "not_found", err.Error())
		case errors.Is(err, users.ErrWeakPassword):
			writeErr(w, http.StatusBadRequest, "weak_password", err.Error())
		default:
			writeErr(w, http.StatusInternalServerError, "db_error", err.Error())
		}
		return
	}
	if closeSessions {
		if err := s.auth.DestroyUserSessions(r.Context(), name, ""); err != nil {
			writeErr(w, http.StatusInternalServerError, "db_error", err.Error())
			return
		}
	}
	s.act.AuditOnly(r.Context(), actor(r), "user.password", name, nil)
	w.WriteHeader(http.StatusNoContent)
}
