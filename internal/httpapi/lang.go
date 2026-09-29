// lang.go — Spanish responses for a UI set to Spanish (internal/i18n).
//
// The API speaks English, and that is what every client that does not ask
// for anything else gets. The web UI sends its language with every request
// (Accept-Language, or ?lang= on an EventSource, which cannot set headers),
// and for "es" the prose in JSON bodies and SSE events is translated on the
// way out. Handlers do not change: whatever they write, in whatever helper,
// passes through here.
package httpapi

import (
	"bytes"
	"context"
	"net/http"
	"strings"
	"sync"
	"time"

	"easyzfs/internal/auth"
	"easyzfs/internal/i18n"
)

// requestLang — "en" (the default) or "es".
func requestLang(r *http.Request) string {
	if q := r.URL.Query().Get("lang"); q == "en" || q == "es" {
		return q
	}
	first, _, _ := strings.Cut(r.Header.Get("Accept-Language"), ",")
	first, _, _ = strings.Cut(first, ";")
	if strings.HasPrefix(strings.ToLower(strings.TrimSpace(first)), "es") {
		return "es"
	}
	return "en"
}

func langMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The body depends on the language: a cache in between must not
		// hand one user's English to another's Spanish.
		w.Header().Add("Vary", "Accept-Language")
		if requestLang(r) != "es" {
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
	uiLangMu    sync.Mutex // guards the two maps only, never held across I/O
	uiLangSeen  = map[string]string{}
	uiLangLocks = map[string]*sync.Mutex{} // one per user: serialises its writes
)

// forgetUILang drops a deleted user from the cache: a user created again
// under the same name starts with no recorded language, and a stale entry
// would keep it from being recorded.
func forgetUILang(user string) {
	uiLangMu.Lock()
	delete(uiLangSeen, user)
	uiLangMu.Unlock()
}

// noteUILang records lang for user when it changed. The common case (no
// change) only reads the cache. A write is serialised per user — two
// requests in different languages at once must leave the cache and the
// database agreeing — and bounded in time: the database has one connection,
// and a backup's VACUUM holding it must not stall this user's requests for
// long, nor anybody else's ever.
func (s *Server) noteUILang(ctx context.Context, user, lang string) {
	uiLangMu.Lock()
	if uiLangSeen[user] == lang {
		uiLangMu.Unlock()
		return
	}
	ul := uiLangLocks[user]
	if ul == nil {
		ul = &sync.Mutex{}
		uiLangLocks[user] = ul
	}
	uiLangMu.Unlock()

	ul.Lock()
	defer ul.Unlock()
	uiLangMu.Lock()
	done := uiLangSeen[user] == lang // another request got here first
	uiLangMu.Unlock()
	if done {
		return
	}
	wctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if _, err := s.users.SetUILang(wctx, user, lang); err == nil {
		uiLangMu.Lock()
		uiLangSeen[user] = lang
		uiLangMu.Unlock()
	}
}

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
			s.noteUILang(r.Context(), user, lang)
		}
		next.ServeHTTP(w, r)
	})
}
