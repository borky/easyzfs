// reauth.go — irreversible operations ask for the password again (and the
// TOTP code when 2FA is on). A logged-in session alone, or typing the target's
// name, is not enough to destroy a pool: a hijacked session or a stray click
// should not be able to.
package httpapi

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"easyzfs/internal/totp"
)

// reauthFields — added by the client to the operation's own JSON body.
type reauthFields struct {
	Password string `json:"reauth_password"`
	Code     string `json:"reauth_code"`
}

// requireReauth wraps a destructive handler.
func (s *Server) requireReauth(next http.HandlerFunc) http.HandlerFunc {
	return s.requireReauthIf(nil, next)
}

// requireReauthIf asks only when pred says the request is destructive (e.g. a
// replication job with force_full, which can wipe its destination). pred gets
// the request body decoded as a JSON object.
//
// Answers are 403, never 401: the frontend treats any 401 as an expired
// session and logs the user out. Wrong passwords and codes count toward the
// login limiter, so the prompt cannot be used to guess the password from a
// hijacked session. Recovery codes are not accepted here: each confirmation
// would burn one.
func (s *Server) requireReauthIf(pred func(map[string]any) bool, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.cfg.Demo { // every mutation is refused in demo mode anyway
			next(w, r)
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
		if err != nil {
			writeErr(w, http.StatusBadRequest, "bad_json", "body demasiado grande o ilegible")
			return
		}
		// The handler reads the same body afterwards.
		r.Body = io.NopCloser(bytes.NewReader(body))
		r.ContentLength = int64(len(body))

		if pred != nil {
			var raw map[string]any
			_ = json.Unmarshal(body, &raw)
			if !pred(raw) {
				next(w, r)
				return
			}
		}
		var re reauthFields
		_ = json.Unmarshal(body, &re)
		if re.Password == "" {
			writeErr(w, http.StatusForbidden, "reauth_required",
				"esta acción no se puede deshacer: confírmala con tu contraseña")
			return
		}

		user := actor(r)
		key := loginKey(r, user)
		now := time.Now()
		if ok, retry := s.loginLimiter.allow(key, now); !ok {
			w.Header().Set("Retry-After", strconv.Itoa(int(retry.Seconds())+1))
			writeErr(w, http.StatusTooManyRequests, "rate_limited", "demasiados intentos; inténtalo más tarde")
			return
		}
		argonSem <- struct{}{}
		_, verr := s.users.Verify(r.Context(), user, re.Password)
		<-argonSem
		if verr != nil {
			s.loginLimiter.failure(key, now)
			writeErr(w, http.StatusForbidden, "reauth_failed", "contraseña incorrecta")
			return
		}
		if on, err := s.users.TOTPEnabled(r.Context(), user); err != nil || on {
			secret, serr := s.users.TOTPSecret(r.Context(), user)
			switch {
			case err != nil || serr != nil || secret == "":
				writeErr(w, http.StatusInternalServerError, "db_error", "no se pudo comprobar la verificación en dos pasos")
				return
			case strings.TrimSpace(re.Code) == "":
				writeErr(w, http.StatusForbidden, "reauth_code_required",
					"confírmala también con el código de tu app de autenticación")
				return
			case !totp.Validate(strings.TrimSpace(re.Code), secret, now):
				s.loginLimiter.failure(key, now)
				writeErr(w, http.StatusForbidden, "reauth_failed", "código incorrecto")
				return
			}
		}
		s.loginLimiter.success(key)
		s.loginLimiter.refund(key, now)
		next(w, r)
	}
}

// forceFull — the replication predicate: only a job that may destroy its
// destination to start over needs the password.
func forceFull(raw map[string]any) bool {
	v, _ := raw["force_full"].(bool)
	return v
}
