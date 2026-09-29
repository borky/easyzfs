package httpapi

import (
	"context"
	"crypto/rand"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"easyzfs/internal/auth"
	"easyzfs/internal/config"
	"easyzfs/internal/db"
	"easyzfs/internal/users"
)

// The same refusal, asked for in English and in Spanish (the default).
func TestErrorsFollowTheUILanguage(t *testing.T) {
	h, c := setupReadOnlyServer(t)
	for lang, want := range map[string]string{
		"en":             "read-only mode: EasyZFS does not change storage",
		"en-GB,en;q=0.9": "read-only mode: EasyZFS does not change storage",
		"es":             "modo solo lectura: EasyZFS no modifica el almacenamiento",
		"":               "modo solo lectura: EasyZFS no modifica el almacenamiento",
	} {
		r := httptest.NewRequest("POST", "/api/pools/tank/scrub", strings.NewReader(`{}`))
		r.Header.Set("Content-Type", "application/json")
		if lang != "" {
			r.Header.Set("Accept-Language", lang)
		}
		r.AddCookie(c)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), want) {
			t.Errorf("Accept-Language %q: %d %s, want %q", lang, w.Code, w.Body.String(), want)
		}
		if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
			t.Errorf("Accept-Language %q: content type %q", lang, ct)
		}
	}
}

// ?lang= wins over the header: an EventSource cannot set one.
func TestRequestLang(t *testing.T) {
	r := httptest.NewRequest("GET", "/api/events?lang=en", nil)
	r.Header.Set("Accept-Language", "es-ES")
	if requestLang(r) != "en" {
		t.Error("?lang=en ignored")
	}
	r = httptest.NewRequest("GET", "/api/events?lang=es", nil)
	r.Header.Set("Accept-Language", "en-US")
	if requestLang(r) != "es" {
		t.Error("?lang=es ignored")
	}
}

// SSE events are translated one by one and still flushed as they come.
func TestSSEIsTranslatedAndStillStreams(t *testing.T) {
	rec := httptest.NewRecorder()
	lw := &langWriter{ResponseWriter: rec, code: 200}
	lw.Header().Set("Content-Type", "text/event-stream")
	var w http.ResponseWriter = lw
	if _, ok := w.(http.Flusher); !ok {
		t.Fatal("langWriter is not a Flusher: SSE handlers would refuse to stream")
	}
	w.Write([]byte(":ok\n\n"))
	w.Write([]byte("event: alert\ndata: {\"message\":\"Resilver iniciado en el pool tank\",\"pool\":\"tank\"}\n\n"))
	w.(http.Flusher).Flush()
	if !rec.Flushed {
		t.Error("not flushed")
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"message":"Resilver started on pool tank"`) || !strings.HasPrefix(body, ":ok\n\n") {
		t.Errorf("stream: %q", body)
	}
}

// The UI's language is recorded from its own header, not from a browser's
// Accept-Language on a plain navigation.
func TestRecordUILang(t *testing.T) {
	d, err := db.Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	if err := db.Migrate(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	secret := make([]byte, 32)
	rand.Read(secret)
	us := users.NewStore(d)
	if err := us.Create(context.Background(), "admin", "password123", "admin"); err != nil {
		t.Fatal(err)
	}
	h := NewServer(Deps{Cfg: &config.Config{Mock: true}, DB: d, Auth: auth.NewManager(d, secret, false), Users: us}).Handler()
	c := loginOK(t, h)
	uiLang := func() string {
		var v string
		if err := d.QueryRow("SELECT ui_lang FROM users WHERE user='admin'").Scan(&v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	do := func(hdr map[string]string) {
		r := httptest.NewRequest("GET", "/api/me", nil)
		for k, v := range hdr {
			r.Header.Set(k, v)
		}
		r.AddCookie(c)
		h.ServeHTTP(httptest.NewRecorder(), r)
	}
	uiLangMu.Lock()
	delete(uiLangSeen, "admin")
	uiLangMu.Unlock()
	do(map[string]string{"Accept-Language": "en-US"}) // a browser, not the UI
	if got := uiLang(); got != "" {
		t.Fatalf("recorded %q from a bare Accept-Language", got)
	}
	do(map[string]string{"X-UI-Lang": "en", "Accept-Language": "en"})
	if got := uiLang(); got != "en" {
		t.Fatalf("ui_lang = %q, want en", got)
	}
	do(map[string]string{"X-UI-Lang": "es"})
	if got := uiLang(); got != "es" {
		t.Fatalf("ui_lang = %q after switching back, want es", got)
	}
}
