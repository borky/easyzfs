// modesweep_test.go — every mutating route, found in the source rather than
// listed by hand, against read-only and demo mode (remediation spec P8). A
// route added later is covered without anyone remembering this file.
package httpapi

import (
	"context"
	"crypto/rand"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"easyzfs/internal/auth"
	"easyzfs/internal/config"
	"easyzfs/internal/db"
	"easyzfs/internal/users"
)

var reRoute = regexp.MustCompile(`Handle(?:Func)?\("(POST|PUT|PATCH|DELETE) (/api/[^"]*)"`)
var reParam = regexp.MustCompile(`\{[^}]*\}`)

// mutatingRoutes — method and a concrete path for every mutating route
// registered in this package ({param} filled with a plausible value).
func mutatingRoutes(t *testing.T) [][2]string {
	t.Helper()
	files, _ := filepath.Glob("*.go")
	var out [][2]string
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range reRoute.FindAllStringSubmatch(string(b), -1) {
			out = append(out, [2]string{m[1], reParam.ReplaceAllString(m[2], "tank")})
		}
	}
	if len(out) < 50 {
		t.Fatalf("found only %d mutating routes: the pattern no longer matches how routes are registered", len(out))
	}
	return out
}

// appRoutes — mutating routes that change only the app's own state (users,
// sessions, alert channels, its database backup) and so stay usable in
// read-only mode. Anything else must be a storageRoute.
var appRoutes = []string{"/api/alerts", "/api/backup", "/api/channels", "/api/keys",
	"/api/login", "/api/logout", "/api/me", "/api/push", "/api/settings", "/api/users"}

// sweepDo — one request from its own client address: the mutation rate
// limit (30/min per IP) sits in front of the mode guards and would answer
// most of a sweep with 429.
func sweepDo(h http.Handler, c *http.Cookie, i int, method, path string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(`{}`))
	r.Header.Set("Content-Type", "application/json")
	r.RemoteAddr = fmt.Sprintf("198.51.100.%d:1234", i%250+1)
	r.AddCookie(c)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func isAppRoute(p string) bool {
	for _, pre := range appRoutes {
		if p == pre || strings.HasPrefix(p, pre+"/") {
			return true
		}
	}
	return false
}

func TestReadOnlySweep(t *testing.T) {
	h, c := setupReadOnlyServer(t)
	for i, r := range mutatingRoutes(t) {
		switch {
		case storageRoute(r[1]):
			w := sweepDo(h, c, i, r[0], r[1])
			if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "read_only") {
				t.Errorf("%s %s: %d %s, want 403 read_only", r[0], r[1], w.Code, w.Body.String())
			}
		case !isAppRoute(r[1]):
			t.Errorf("%s %s mutates but is neither a storageRoute nor an app-settings route: classify it", r[0], r[1])
		}
	}
}

func TestDemoSweep(t *testing.T) {
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
		Cfg: &config.Config{Mock: true, Demo: true}, DB: d,
		Auth: auth.NewManager(d, secret, false), Users: us,
	}).Handler()
	c := loginOK(t, h)
	for i, r := range mutatingRoutes(t) {
		p := r[1]
		// Outside the guarded tree (login), or allowed on purpose (logout,
		// acknowledging an alert).
		if strings.HasPrefix(p, "/api/login") || p == "/api/logout" ||
			(strings.HasPrefix(p, "/api/alerts/") && strings.HasSuffix(p, "/ack")) {
			continue
		}
		w := sweepDo(h, c, i, r[0], p)
		if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "demo_mode") {
			t.Errorf("%s %s: %d %s, want 403 demo_mode", r[0], p, w.Code, w.Body.String())
		}
	}
}
