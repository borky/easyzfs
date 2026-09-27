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
	if !forceFull(map[string]any{"force_full": true}) || forceFull(map[string]any{"force_full": false}) || forceFull(nil) {
		t.Fatal("forceFull must be true only for force_full=true")
	}
}
