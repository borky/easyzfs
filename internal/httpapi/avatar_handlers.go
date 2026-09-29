// avatar_handlers.go — foto de perfil del usuario.
// PUT /api/me/avatar (body binario image/webp|image/jpeg, máx. 512 KB) →
// guarda en <datadir>/avatars/<user>.<ext> y actualiza users.avatar.
// DELETE /api/me/avatar → borra el fichero y vacía la columna.
// GET /api/avatars/{name} → sirve la imagen (cualquier usuario autenticado;
// el avatar no es un secreto, es decoración de la UI).
package httpapi

import (
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"easyzfs/internal/auth"
	"easyzfs/internal/users"
)

const avatarMaxBytes = 512 << 10 // 512 KB (el crop dialog exporta ~256px webp)

// avatarDir — <datadir>/avatars (se crea bajo demanda).
func (s *Server) avatarDir() string {
	return filepath.Join(s.cfg.DataDir(), "avatars")
}

// putMyAvatar — PUT /api/me/avatar → 204. El body es la imagen cruda
// (el crop dialog del front ya recorta y re-codifica en cliente).
func (s *Server) putMyAvatar(w http.ResponseWriter, r *http.Request) {
	user := auth.UserFromContext(r.Context())

	ct := r.Header.Get("Content-Type")
	var ext string
	switch {
	case strings.Contains(ct, "webp"):
		ext = ".webp"
	case strings.Contains(ct, "jpeg"), strings.Contains(ct, "jpg"):
		ext = ".jpeg"
	default:
		writeErr(w, http.StatusUnsupportedMediaType, "invalid_type",
			"the avatar must be image/webp or image/jpeg")
		return
	}

	if err := os.MkdirAll(s.avatarDir(), 0o700); err != nil {
		writeErr(w, http.StatusInternalServerError, "io_error", err.Error())
		return
	}

	tmp, err := os.CreateTemp(s.avatarDir(), user+"-*"+ext)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "io_error", err.Error())
		return
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath) // no-op si el rename tuvo éxito

	r.Body = http.MaxBytesReader(w, r.Body, avatarMaxBytes)
	n, err := io.Copy(tmp, r.Body)
	tmp.Close()
	if err != nil {
		writeErr(w, http.StatusBadRequest, "avatar_too_large", "image too large (max. 512 KB)")
		return
	}
	if n == 0 {
		writeErr(w, http.StatusBadRequest, "empty_body", "the body is empty")
		return
	}

	filename := user + ext
	final := filepath.Join(s.avatarDir(), filename)
	// Sustitución atómica: rename pisa cualquier versión anterior.
	if err := os.Rename(tmpPath, final); err != nil {
		writeErr(w, http.StatusInternalServerError, "io_error", err.Error())
		return
	}
	// Limpieza de versiones previas con otra extensión (p. ej. .webp viejo).
	for _, e := range []string{".webp", ".jpeg"} {
		if e != ext {
			os.Remove(filepath.Join(s.avatarDir(), user+e))
		}
	}

	if err := s.users.SetAvatar(r.Context(), user, filename); err != nil {
		writeErr(w, http.StatusInternalServerError, "db_error", err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// deleteMyAvatar — DELETE /api/me/avatar → 204.
func (s *Server) deleteMyAvatar(w http.ResponseWriter, r *http.Request) {
	user := auth.UserFromContext(r.Context())
	u, err := s.users.Get(r.Context(), user)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "db_error", err.Error())
		return
	}
	if u.Avatar != "" {
		os.Remove(filepath.Join(s.avatarDir(), u.Avatar))
	}
	if err := s.users.SetAvatar(r.Context(), user, ""); err != nil {
		writeErr(w, http.StatusInternalServerError, "db_error", err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// getAvatar — GET /api/avatars/{name} → la imagen (404 si no tiene).
func (s *Server) getAvatar(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	u, err := s.users.Get(r.Context(), name)
	if err != nil {
		if errors.Is(err, users.ErrNotFound) {
			writeErr(w, http.StatusNotFound, "not_found", "user not found")
			return
		}
		writeErr(w, http.StatusInternalServerError, "db_error", err.Error())
		return
	}
	if u.Avatar == "" {
		writeErr(w, http.StatusNotFound, "no_avatar", "the user has no profile picture")
		return
	}
	// u.Avatar viene de la BD (lo escribe el server con user+ext), pero se
	// sanea igualmente por si acaso.
	clean := filepath.Base(u.Avatar)
	if clean != u.Avatar || strings.Contains(clean, "..") {
		writeErr(w, http.StatusNotFound, "no_avatar", "invalid avatar")
		return
	}
	path := filepath.Join(s.avatarDir(), clean)
	if strings.HasSuffix(clean, ".webp") {
		w.Header().Set("Content-Type", "image/webp")
	} else {
		w.Header().Set("Content-Type", "image/jpeg")
	}
	w.Header().Set("Cache-Control", "no-cache") // el usuario puede cambiarla
	http.ServeFile(w, r, path)
}
