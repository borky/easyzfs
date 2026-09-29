// lang.go — English responses for a UI set to English (internal/i18n).
//
// The API speaks Spanish; that is its contract, and what every client that
// does not ask for anything else gets. The web UI sends its language with
// every request (Accept-Language, or ?lang= on an EventSource, which cannot
// set headers), and for "en" the prose in JSON bodies and SSE events is
// translated on the way out. Handlers do not change: whatever they write,
// in whatever helper, passes through here.
package httpapi

import (
	"bytes"
	"net/http"
	"strings"
	"sync"

	"easyzfs/internal/auth"
	"easyzfs/internal/i18n"
)

// requestLang — "en" or "es".
func requestLang(r *http.Request) string {
	if q := r.URL.Query().Get("lang"); q == "en" || q == "es" {
		return q
	}
	first, _, _ := strings.Cut(r.Header.Get("Accept-Language"), ",")
	first, _, _ = strings.Cut(first, ";")
	if strings.HasPrefix(strings.ToLower(strings.TrimSpace(first)), "en") {
		return "en"
	}
	return "es"
}

func langMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The body depends on the language: a cache in between must not
		// hand one user's English to another's Spanish.
		w.Header().Add("Vary", "Accept-Language")
		if requestLang(r) != "en" {
			next.ServeHTTP(w, r)
			return
		}
		lw := &langWriter{ResponseWriter: w, code: http.StatusOK}
		next.ServeHTTP(lw, r)
		lw.finish()
	})
}

const (
	modeUndecided = iota
	modeJSON      // buffered, translated whole at the end
	modeSSE       // translated event by event, never buffered
	modePass      // anything else (backups, avatars, the SPA)
)

type langWriter struct {
	http.ResponseWriter
	mode int
	code int
	buf  bytes.Buffer
}

func (w *langWriter) decide() {
	if w.mode != modeUndecided {
		return
	}
	switch ct := w.Header().Get("Content-Type"); {
	case strings.HasPrefix(ct, "application/json"):
		w.mode = modeJSON
	case strings.HasPrefix(ct, "text/event-stream"):
		w.mode = modeSSE
	default:
		w.mode = modePass
	}
}

func (w *langWriter) WriteHeader(code int) {
	if w.mode != modeUndecided {
		return // as net/http: only the first status counts
	}
	w.code = code
	w.decide()
	if w.mode != modeJSON {
		w.ResponseWriter.WriteHeader(code)
	}
}

func (w *langWriter) Write(b []byte) (int, error) {
	if w.mode == modeUndecided {
		w.WriteHeader(http.StatusOK)
	}
	switch w.mode {
	case modeJSON:
		return w.buf.Write(b)
	case modeSSE:
		if _, err := w.ResponseWriter.Write(translateSSE(b)); err != nil {
			return 0, err
		}
		return len(b), nil
	}
	return w.ResponseWriter.Write(b)
}

// Flush keeps SSE streaming; a JSON body is only written at the end.
func (w *langWriter) Flush() {
	if w.mode == modeJSON {
		return
	}
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *langWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *langWriter) finish() {
	if w.mode != modeJSON {
		return
	}
	out, _ := i18n.JSON(w.buf.Bytes())
	w.Header().Del("Content-Length")
	w.ResponseWriter.WriteHeader(w.code)
	w.ResponseWriter.Write(out)
}

// translateSSE translates the JSON on each "data: " line of one or more
// events; anything that is not JSON is left as it is.
func translateSSE(b []byte) []byte {
	if !bytes.Contains(b, []byte("data: ")) {
		return b
	}
	lines := bytes.Split(b, []byte("\n"))
	for i, l := range lines {
		payload, ok := bytes.CutPrefix(l, []byte("data: "))
		if !ok {
			continue
		}
		if out, ok := i18n.JSON(payload); ok {
			lines[i] = append([]byte("data: "), bytes.TrimRight(out, "\n")...)
		}
	}
	return bytes.Join(lines, []byte("\n"))
}

// uiLangSeen — per user, the UI language last recorded (a cache in front of
// users.ui_lang, so the database is written only when it changes).
var (
	uiLangMu   sync.Mutex
	uiLangSeen = map[string]string{}
)

// recordUILang notes the language each user's UI shows, for what the server
// writes to them outside a response: e-mail, push, and the shared channels
// when their language is "auto" (users.NotifyLang). Only the UI says it: its
// X-UI-Lang header, or ?lang= on an EventSource. A browser's own
// Accept-Language on a plain navigation (a backup download) is not a choice
// the user made in EasyZFS.
func (s *Server) recordUILang(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lang := r.Header.Get("X-UI-Lang")
		if lang == "" {
			lang = r.URL.Query().Get("lang")
		}
		user := auth.UserFromContext(r.Context())
		if (lang == "es" || lang == "en") && user != "" && s.users != nil {
			uiLangMu.Lock()
			seen := uiLangSeen[user]
			uiLangMu.Unlock()
			if seen != lang {
				if _, err := s.users.SetUILang(r.Context(), user, lang); err == nil {
					uiLangMu.Lock()
					uiLangSeen[user] = lang
					uiLangMu.Unlock()
				}
			}
		}
		next.ServeHTTP(w, r)
	})
}
