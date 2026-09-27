// csrf_test.go — the Origin check is on by default; CSRF_CHECK=0 turns it off.
package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// csrfPut sends an authenticated mutation with the given Origin ("" = none).
// httptest requests carry Host "example.com".
func csrfPut(t *testing.T, h http.Handler, cookie *http.Cookie, origin string) int {
	t.Helper()
	r := httptest.NewRequest("PUT", "/api/me/language", strings.NewReader(`{"language":"en"}`))
	r.Header.Set("Content-Type", "application/json")
	if origin != "" {
		r.Header.Set("Origin", origin)
	}
	r.AddCookie(cookie)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w.Code
}

func TestCSRFOnByDefault(t *testing.T) {
	t.Setenv("CSRF_CHECK", "")
	h := setup2FAServer(t)
	cookie := loginOK(t, h)

	if code := csrfPut(t, h, cookie, "https://evil.example"); code != http.StatusForbidden {
		t.Fatalf("cross-site mutation: %d, want 403", code)
	}
	if code := csrfPut(t, h, cookie, "http://example.com"); code == http.StatusForbidden {
		t.Fatalf("same-origin mutation was refused")
	}
	if code := csrfPut(t, h, cookie, ""); code == http.StatusForbidden {
		t.Fatalf("a non-browser client (no Origin) was refused")
	}
}

func TestCSRFCanBeTurnedOff(t *testing.T) {
	t.Setenv("CSRF_CHECK", "0")
	h := setup2FAServer(t)
	cookie := loginOK(t, h)
	if code := csrfPut(t, h, cookie, "https://evil.example"); code == http.StatusForbidden {
		t.Fatalf("CSRF_CHECK=0 still refused a cross-site mutation")
	}
}
