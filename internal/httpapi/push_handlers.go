// push_handlers.go — endpoints Web Push tras el middleware de sesión (misma
// protección que el resto de mutaciones: cookie HttpOnly SameSite=Lax).
//
//	GET    /api/push/vapid-public-key  → {"publicKey":"…"} (503 si no configurado)
//	POST   /api/push/subscribe         → upsert por endpoint
//	DELETE /api/push/unsubscribe       → borra solo suscripciones del propio usuario
//	GET    /api/push/preferences       → los 5 tipos con su enabled (default true)
//	PUT    /api/push/preferences       → upsert {tipo, enabled} (400 invalid_tipo)
//	GET    /api/push/quiet-hours       → {enabled, start, end, tz} (null si off)
//	PUT    /api/push/quiet-hours       → upsert {enabled, start, end} (400 invalid_hours)
package httpapi

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"strings"

	"easyzfs/internal/auth"
	"easyzfs/internal/push"
)

// getPushVapidKey — clave pública VAPID para pushManager.subscribe().
// La pública no es secreta, pero va tras sesión como el resto del API.
func (s *Server) getPushVapidKey(w http.ResponseWriter, r *http.Request) {
	if !s.cfg.PushEnabled() {
		writeErr(w, http.StatusServiceUnavailable, "push_not_configured",
			"push notifications not configured on the server (VAPID keys missing)")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"publicKey": s.cfg.VAPIDPublicKey})
}

// subscribeReq — body de POST /api/push/subscribe (lo que devuelve
// PushSubscription.toJSON() en el navegador + idioma y origin del dispositivo).
type subscribeReq struct {
	Endpoint string `json:"endpoint"`
	Keys     struct {
		P256dh string `json:"p256dh"`
		Auth   string `json:"auth"`
	} `json:"keys"`
	Lang   string `json:"lang"`   // 'es' | 'en' (idioma del dispositivo al suscribir)
	Origin string `json:"origin"` // window.location.origin (para URLs absolutas)
}

// postPushSubscribe — upsert por endpoint con el usuario de la sesión.
func (s *Server) postPushSubscribe(w http.ResponseWriter, r *http.Request) {
	if !s.cfg.PushEnabled() {
		writeErr(w, http.StatusServiceUnavailable, "push_not_configured",
			"push notifications not configured on the server (VAPID keys missing)")
		return
	}
	var req subscribeReq
	if !decodeJSON(w, r, &req) {
		return
	}
	// Validación: endpoint HTTPS (capability URL) y claves presentes.
	if req.Endpoint == "" || len(req.Endpoint) > 2048 || !strings.HasPrefix(req.Endpoint, "https://") {
		writeErr(w, http.StatusBadRequest, "invalid_endpoint", "invalid endpoint (an https:// URL is required)")
		return
	}
	// Formato de claves: p256dh = punto P256 sin comprimir (65 bytes) y
	// auth = secreto de 16 bytes, ambos en base64url.
	if !validKeys(req.Keys.P256dh, req.Keys.Auth) {
		writeErr(w, http.StatusBadRequest, "invalid_keys",
			"invalid subscription keys (p256dh must be 65 bytes of base64url and auth 16 bytes)")
		return
	}
	// Origin: vacío (fallback a URLs relativas) o http(s)://…
	if len(req.Origin) > 256 ||
		(req.Origin != "" && !strings.HasPrefix(req.Origin, "https://") && !strings.HasPrefix(req.Origin, "http://")) {
		writeErr(w, http.StatusBadRequest, "invalid_origin", "invalid origin (an http(s):// URL is required)")
		return
	}
	lang := req.Lang
	if lang != "en" && lang != "es" {
		lang = "" // unknown or absent: the insert falls back to 'en', the upsert keeps what was there
	}
	ua := r.UserAgent()
	if len(ua) > 512 {
		ua = ua[:512]
	}
	if err := s.push.Subscribe(r.Context(), auth.UserFromContext(r.Context()),
		req.Endpoint, req.Keys.P256dh, req.Keys.Auth, lang, req.Origin, ua); err != nil {
		writeErr(w, http.StatusInternalServerError, "push_subscribe_error", err.Error())
		return
	}
	writeJSON(w, http.StatusNoContent, nil)
}

// validKeys — p256dh base64url → 65 bytes (punto P256 sin comprimir);
// auth base64url → 16 bytes (secreto de autenticación del push service).
func validKeys(p256dh, auth string) bool {
	p, err := base64.RawURLEncoding.DecodeString(p256dh)
	if err != nil || len(p) != 65 {
		return false
	}
	a, err := base64.RawURLEncoding.DecodeString(auth)
	if err != nil || len(a) != 16 {
		return false
	}
	return true
}

// unsubscribeReq — body de DELETE /api/push/unsubscribe.
type unsubscribeReq struct {
	Endpoint string `json:"endpoint"`
}

// deletePushUnsubscribe — borra la suscripción SOLO si pertenece al usuario
// de la sesión (multiuser: nadie borra suscripciones ajenas).
func (s *Server) deletePushUnsubscribe(w http.ResponseWriter, r *http.Request) {
	var req unsubscribeReq
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Endpoint == "" {
		writeErr(w, http.StatusBadRequest, "invalid_endpoint", "the subscription endpoint is missing")
		return
	}
	if err := s.push.Unsubscribe(r.Context(), auth.UserFromContext(r.Context()), req.Endpoint); err != nil {
		writeErr(w, http.StatusInternalServerError, "push_unsubscribe_error", err.Error())
		return
	}
	writeJSON(w, http.StatusNoContent, nil)
}

// --- Preferencias de notificación (cada usuario gestiona LAS SUYAS) ---

// getPushPreferences — los 5 tipos del catálogo con su enabled (default true
// si no hay fila guardada).
func (s *Server) getPushPreferences(w http.ResponseWriter, r *http.Request) {
	prefs, err := s.push.Preferences(r.Context(), auth.UserFromContext(r.Context()))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "push_preferences_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"preferences": prefs})
}

// prefReq — body de PUT /api/push/preferences.
type prefReq struct {
	Tipo    string `json:"tipo"`
	Enabled bool   `json:"enabled"`
}

// putPushPreferences — upsert de un tipo; 400 invalid_tipo si es desconocido.
func (s *Server) putPushPreferences(w http.ResponseWriter, r *http.Request) {
	var req prefReq
	if !decodeJSON(w, r, &req) {
		return
	}
	if !push.TipoValido(req.Tipo) {
		writeErr(w, http.StatusBadRequest, "invalid_tipo",
			fmt.Sprintf("unknown alert type (valid: %s)", strings.Join(push.Tipos, ", ")))
		return
	}
	if err := s.push.SetPreference(r.Context(), auth.UserFromContext(r.Context()), req.Tipo, req.Enabled); err != nil {
		writeErr(w, http.StatusInternalServerError, "push_preferences_error", err.Error())
		return
	}
	writeJSON(w, http.StatusNoContent, nil)
}

// getPushQuietHours — horario silencioso del usuario (start/end null si off).
func (s *Server) getPushQuietHours(w http.ResponseWriter, r *http.Request) {
	q, err := s.push.QuietHours(r.Context(), auth.UserFromContext(r.Context()))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "push_quiet_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, q)
}

// quietReq — body de PUT /api/push/quiet-hours.
type quietReq struct {
	Enabled bool `json:"enabled"`
	Start   int  `json:"start"`
	End     int  `json:"end"`
}

// putPushQuietHours — upsert del horario silencioso: horas 0-23 y start≠end
// (la ventana puede cruzar medianoche). tz fija (Europe/Madrid) de momento.
func (s *Server) putPushQuietHours(w http.ResponseWriter, r *http.Request) {
	var req quietReq
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Enabled {
		if req.Start < 0 || req.Start > 23 || req.End < 0 || req.End > 23 || req.Start == req.End {
			writeErr(w, http.StatusBadRequest, "invalid_hours",
				"invalid hours: start and end between 0 and 23, and different from each other")
			return
		}
	}
	if err := s.push.SetQuietHours(r.Context(), auth.UserFromContext(r.Context()),
		req.Enabled, req.Start, req.End); err != nil {
		writeErr(w, http.StatusInternalServerError, "push_quiet_error", err.Error())
		return
	}
	writeJSON(w, http.StatusNoContent, nil)
}
