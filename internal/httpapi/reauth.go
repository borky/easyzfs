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
	"net/url"
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
// the raw request body.
//
// Answers are 403, never 401: the frontend treats any 401 as an expired
// session and logs the user out. Wrong passwords and codes count toward the
// login limiter, so the prompt cannot be used to guess the password from a
// hijacked session. Recovery codes are not accepted here: each confirmation
// would burn one.
func (s *Server) requireReauthIf(pred func([]byte) bool, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.cfg.Demo { // every mutation is refused in demo mode anyway
			next(w, r)
			return
		}
		var re reauthFields
		switch {
		case r.Header.Get("X-Reauth-Password") != "":
			// Routes whose body is not JSON (a backup upload of up to 4 GiB)
			// carry the answer in headers, URI-encoded because a header must
			// be Latin-1. The body is then left unread for the handler.
			re.Password, _ = url.QueryUnescape(r.Header.Get("X-Reauth-Password"))
			re.Code, _ = url.QueryUnescape(r.Header.Get("X-Reauth-Code"))
		case pred == nil && r.ContentLength != 0 &&
			!strings.HasPrefix(r.Header.Get("Content-Type"), "application/json"):
			// Not JSON and no headers: ask, without reading the body.
			writeErr(w, http.StatusForbidden, "reauth_required",
				"esta acción no se puede deshacer: confírmala con tu contraseña")
			return
		default:
			body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
			if err != nil {
				writeErr(w, http.StatusBadRequest, "bad_json", "body demasiado grande o ilegible")
				return
			}
			// The handler reads the same body afterwards.
			r.Body = io.NopCloser(bytes.NewReader(body))
			r.ContentLength = int64(len(body))
			if pred != nil {
				if !pred(body) {
					next(w, r)
					return
				}
			}
			_ = json.Unmarshal(body, &re)
		}
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
// destination to start over needs the password. It decodes exactly as the
// handler's decodeJSON does (json.Decoder: case-insensitive keys, first value
// only). A map lookup did not: {"FORCE_FULL":true}, or a valid object with
// trailing bytes, read as false here and as true in the handler. Anything it
// cannot decode asks for the password.
func forceFull(body []byte) bool {
	var v struct {
		ForceFull *bool `json:"force_full"`
	}
	if err := json.NewDecoder(bytes.NewReader(body)).Decode(&v); err != nil {
		return true
	}
	return v.ForceFull != nil && *v.ForceFull
}

// volsizeChange — the properties predicate: changing a volume's size can
// shrink it, which destroys the data past the new end, so it needs the
// password on top of the risk acknowledgement. Decoded as the handler does
// (see forceFull); undecodable asks.
func volsizeChange(body []byte) bool {
	var v struct {
		Property string `json:"property"`
	}
	if err := json.NewDecoder(bytes.NewReader(body)).Decode(&v); err != nil {
		return true
	}
	return strings.EqualFold(strings.TrimSpace(v.Property), "volsize")
}
