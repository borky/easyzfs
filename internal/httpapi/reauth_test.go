// reauth_test.go — irreversible operations ask for the password again, and
// the TOTP code when 2FA is on.
package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/pquerna/otp/totp"

	"easyzfs/internal/actions"
)

// reauthDelete deletes user "victim" through a re-auth protected route.
func reauthDelete(t *testing.T, h http.Handler, c *http.Cookie, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest("DELETE", "/api/users/victim", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.AddCookie(c)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func setupReauth(t *testing.T) (*Server, http.Handler, *http.Cookie) {
	t.Helper()
	srv, h := setup2FAServerWithLimiter(t)
	srv.act = actions.NewService(srv.db) // the delete handler audits through it
	if err := srv.users.Create(context.Background(), "victim", "password123", "user"); err != nil {
		t.Fatal(err)
	}
	return srv, h, loginOK(t, h)
}

func TestReauthRequiredAndChecked(t *testing.T) {
	srv, h, c := setupReauth(t)
	if w := reauthDelete(t, h, c, `{"confirm":"victim"}`); w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "reauth_required") {
		t.Fatalf("no password: %d %s, want 403 reauth_required", w.Code, w.Body.String())
	}
	if w := reauthDelete(t, h, c, `{"confirm":"victim","reauth_password":"wrong-one"}`); w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "reauth_failed") {
		t.Fatalf("wrong password: %d %s, want 403 reauth_failed", w.Code, w.Body.String())
	}
	srv.loginLimiter.mu.Lock()
	n := len(srv.loginLimiter.att)
	srv.loginLimiter.mu.Unlock()
	if n == 0 {
		t.Fatal("a wrong re-auth password did not count toward the login limiter")
	}
	if w := reauthDelete(t, h, c, `{"confirm":"victim","reauth_password":"password123"}`); w.Code == http.StatusForbidden || w.Code == http.StatusUnauthorized {
		t.Fatalf("right password: %d %s, want the handler to run", w.Code, w.Body.String())
	}
}

func TestReauthWithTwoFactor(t *testing.T) {
	_, h, c := setupReauth(t)
	secret := enable2FA(t, h, c)
	if w := reauthDelete(t, h, c, `{"confirm":"victim","reauth_password":"password123"}`); !strings.Contains(w.Body.String(), "reauth_code_required") {
		t.Fatalf("2FA on, password only: %d %s, want reauth_code_required", w.Code, w.Body.String())
	}
	if w := reauthDelete(t, h, c, `{"confirm":"victim","reauth_password":"password123","reauth_code":"000000"}`); !strings.Contains(w.Body.String(), "reauth_failed") {
		t.Fatalf("2FA on, wrong code: %d %s, want reauth_failed", w.Code, w.Body.String())
	}
	code, err := totp.GenerateCode(secret, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if w := reauthDelete(t, h, c, `{"confirm":"victim","reauth_password":"password123","reauth_code":"`+code+`"}`); w.Code == http.StatusForbidden {
		t.Fatalf("2FA on, right code: %d %s, want the handler to run", w.Code, w.Body.String())
	}
}

func TestForceFullPredicate(t *testing.T) {
	for body, want := range map[string]bool{
		`{"force_full":true}`:       true,
		`{"FORCE_FULL":true}`:       true, // json.Decoder matches keys case-insensitively, as the handler does
		`{"force_full":true} x`:     true, // the handler reads only the first value
		`not json`:                  true, // undecidable: ask
		`{"force_full":false}`:      false,
		`{"name":"job","raw":true}`: false,
	} {
		if got := forceFull([]byte(body)); got != want {
			t.Errorf("forceFull(%s) = %v, want %v", body, got, want)
		}
	}
}

// Confirming with the right password must not spend login attempts: an admin
// cleaning up several snapshots in a row was refused with 429 on the sixth.
func TestReauthSuccessesAreNotRateLimited(t *testing.T) {
	_, h, c := setupReauth(t)
	for i := 0; i < 2*loginMaxPerMinute; i++ {
		if w := reauthDelete(t, h, c, `{"confirm":"victim","reauth_password":"password123"}`); w.Code == http.StatusTooManyRequests {
			t.Fatalf("confirmation %d refused as rate limited", i+1)
		}
	}
	// Wrong answers still count: the limiter refuses after the fifth.
	var last int
	for i := 0; i < loginMaxPerMinute+1; i++ {
		last = reauthDelete(t, h, c, `{"confirm":"victim","reauth_password":"wrong-one"}`).Code
	}
	if last != http.StatusTooManyRequests {
		t.Fatalf("after %d wrong passwords got %d, want 429", loginMaxPerMinute+1, last)
	}
}

// The backup upload is not JSON: the answer travels in headers, and without
// them the server asks before reading the (possibly huge) body.
func TestReauthHeadersOnBackupImport(t *testing.T) {
	_, h, c := setupReauth(t)
	post := func(hdr map[string]string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "/api/backup/import", strings.NewReader("not a database"))
		r.Header.Set("Content-Type", "application/octet-stream")
		for k, v := range hdr {
			r.Header.Set(k, v)
		}
		r.AddCookie(c)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	if w := post(nil); w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "reauth_required") {
		t.Fatalf("no headers: %d %s, want 403 reauth_required", w.Code, w.Body.String())
	}
	if w := post(map[string]string{"X-Reauth-Password": "wrong-one"}); !strings.Contains(w.Body.String(), "reauth_failed") {
		t.Fatalf("wrong password: %d %s, want reauth_failed", w.Code, w.Body.String())
	}
	// Right password: the handler runs and rejects the junk body itself.
	if w := post(map[string]string{"X-Reauth-Password": "password123"}); w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "invalid_backup") {
		t.Fatalf("right password: %d %s, want the handler's 400 invalid_backup", w.Code, w.Body.String())
	}
}
