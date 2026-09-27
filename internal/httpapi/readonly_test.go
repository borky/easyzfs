// readonly_test.go — EASYZFS_READONLY: storage mutations are refused, the
// app's own settings stay editable.
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

func setupReadOnlyServer(t *testing.T) (http.Handler, *http.Cookie) {
	t.Helper()
	d, err := db.Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	secret := make([]byte, 32)
	rand.Read(secret)
	us := users.NewStore(d)
	if err := us.Create(context.Background(), "admin", "password123", "admin"); err != nil {
		t.Fatal(err)
	}
	h := NewServer(Deps{
		Cfg:   &config.Config{Mock: true, ReadOnly: true},
		DB:    d,
		Auth:  auth.NewManager(d, secret, false),
		Users: us,
	}).Handler()
	return h, loginOK(t, h)
}

func roDo(h http.Handler, c *http.Cookie, method, path, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.AddCookie(c)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestReadOnlyRefusesStorageMutations(t *testing.T) {
	h, c := setupReadOnlyServer(t)
	for _, req := range [][2]string{
		{"POST", "/api/pools/tank/scrub"},
		{"POST", "/api/pools/tank/export"},
		{"DELETE", "/api/datasets/tank%2Fx"},
		{"POST", "/api/snapshots/tank%2Fx%40s/rollback"},
		{"POST", "/api/disks/sda/poweroff"},
		{"POST", "/api/jobs"},
		{"POST", "/api/replication"},
		{"POST", "/api/system-timers/schedule"},
		{"POST", "/api/update/apply"},
	} {
		w := roDo(h, c, req[0], req[1], `{}`)
		if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "read_only") {
			t.Errorf("%s %s: %d %s, want 403 read_only", req[0], req[1], w.Code, w.Body.String())
		}
	}
}

func TestReadOnlyKeepsAppSettingsEditable(t *testing.T) {
	h, c := setupReadOnlyServer(t)
	w := roDo(h, c, "PUT", "/api/me/language", `{"language":"en"}`)
	if strings.Contains(w.Body.String(), "read_only") {
		t.Fatalf("a per-user setting was refused in read-only mode: %s", w.Body.String())
	}
}

func TestStorageRouteClassifier(t *testing.T) {
	for p, want := range map[string]bool{
		"/api/pools": true, "/api/pools/tank/scrub": true, "/api/datasets/a": true,
		"/api/updates/history": true, "/api/update/apply": true,
		"/api/settings": false, "/api/channels/ntfy": false, "/api/me/language": false,
		"/api/users": false, "/api/poolsX": false,
	} {
		if got := storageRoute(p); got != want {
			t.Errorf("storageRoute(%q) = %v, want %v", p, got, want)
		}
	}
}
