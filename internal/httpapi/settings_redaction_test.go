// settings_redaction_test.go — GET /api/settings is not admin-only (the UI
// needs the thresholds for every user), so the webhook URL is redacted for
// anyone who is not an admin.
package httpapi

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"easyzfs/internal/auth"
	"easyzfs/internal/config"
	"easyzfs/internal/db"
	"easyzfs/internal/settings"
	"easyzfs/internal/users"
)

const testWebhook = "https://hooks.example.com/services/T000/B000/secret-token"

func setupSettingsServer(t *testing.T) http.Handler {
	t.Helper()
	d, err := db.Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatalf("db: %v", err)
	}
	if err := db.Migrate(context.Background(), d); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(func() { d.Close() })

	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		t.Fatal(err)
	}
	us := users.NewStore(d)
	if err := us.Create(context.Background(), "admin", "password123", "admin"); err != nil {
		t.Fatalf("create admin: %v", err)
	}
	if err := us.Create(context.Background(), "viewer", "password123", "user"); err != nil {
		t.Fatalf("create viewer: %v", err)
	}

	st, err := settings.NewStore(d)
	if err != nil {
		t.Fatalf("settings: %v", err)
	}
	cur, err := st.Load(context.Background())
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	cur.Webhook = testWebhook
	if err := st.Save(context.Background(), cur); err != nil {
		t.Fatalf("save: %v", err)
	}

	srv := NewServer(Deps{
		Cfg:      &config.Config{Mock: true},
		DB:       d,
		Auth:     auth.NewManager(d, secret, false),
		Users:    us,
		Settings: st,
	})
	return srv.Handler()
}

// settingsWebhookFor returns the webhook field as the given user sees it.
func settingsWebhookFor(t *testing.T, h http.Handler, user string) string {
	t.Helper()
	body := `{"user":"` + user + `","password":"password123"}`
	rec, cookie := loginReal(t, h, body)
	if rec.Code != 200 {
		t.Fatalf("login %s: %d (%s)", user, rec.Code, rec.Body.String())
	}
	r := httptest.NewRequest("GET", "/api/settings", nil)
	r.AddCookie(cookie)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("settings %s: %d (%s)", user, w.Code, w.Body.String())
	}
	var m map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
		t.Fatal(err)
	}
	s, _ := m["webhook"].(string)
	return s
}

func TestSettingsWebhookRedactadoParaNoAdmin(t *testing.T) {
	h := setupSettingsServer(t)

	if got := settingsWebhookFor(t, h, "admin"); got != testWebhook {
		t.Fatalf("el admin debe ver el webhook completo, vio %q", got)
	}
	if got := settingsWebhookFor(t, h, "viewer"); got != "" {
		t.Fatalf("un usuario no admin no debe ver el webhook, vio %q", got)
	}
}

// Everything else still arrives: redaction must not break the plain user's
// UI, which needs the thresholds and the language.
func TestSettingsNoAdminConservaElResto(t *testing.T) {
	h := setupSettingsServer(t)
	rec, cookie := loginReal(t, h, `{"user":"viewer","password":"password123"}`)
	if rec.Code != 200 {
		t.Fatalf("login: %d", rec.Code)
	}
	r := httptest.NewRequest("GET", "/api/settings", nil)
	r.AddCookie(cookie)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	for _, key := range []string{"cap_warn_pct", "cap_crit_pct", "disk_temp_c", "lang"} {
		if !strings.Contains(w.Body.String(), `"`+key+`"`) {
			t.Fatalf("falta %q en la respuesta: %s", key, w.Body.String())
		}
	}
}
