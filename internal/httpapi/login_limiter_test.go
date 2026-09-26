// login_limiter_test.go — /api/login must not let an unauthenticated client
// grow the limiter map without bound, nor park an unbounded queue of requests
// behind the argon2 semaphore.
package httpapi

import (
	"context"
	"crypto/rand"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"easyzfs/internal/auth"
	"easyzfs/internal/config"
	"easyzfs/internal/db"
	"easyzfs/internal/users"
)

// One failed attempt under each of many fresh usernames: exactly the pattern
// that used to leave an uncollectable entry per request.
func TestLoginLimiterPurgesOneShotFailures(t *testing.T) {
	l := newLoginLimiter()
	t0 := time.Now()
	for i := 0; i < 2000; i++ {
		key := fmt.Sprintf("192.0.2.1|user%d", i)
		if ok, _ := l.allow(key, t0); !ok {
			t.Fatalf("attempt %d refused", i)
		}
		l.failure(key, t0)
	}

	// A key that is currently blocked must survive the purge.
	blocked := "192.0.2.2|target"
	l.allow(blocked, t0)
	for i := 0; i < loginBlockAfter; i++ {
		l.failure(blocked, t0.Add(loginBlockDuration))
	}

	// Past the idle threshold, any call triggers the purge.
	later := t0.Add(loginBlockDuration + time.Minute)
	l.allow("192.0.2.3|someone", later)

	l.mu.Lock()
	n := len(l.att)
	_, keptBlocked := l.att[blocked]
	l.mu.Unlock()
	if n > 10 {
		t.Fatalf("map still holds %d entries after the purge; one-shot failures were not collected", n)
	}
	if !keptBlocked {
		t.Fatal("a currently blocked key was purged; that would lift the lockout")
	}
}

// The purge walks the whole map, so it must not run on every request.
func TestLoginLimiterPurgeIsThrottled(t *testing.T) {
	l := newLoginLimiter()
	t0 := time.Now()
	for i := 0; i < 1100; i++ {
		l.allow(fmt.Sprintf("192.0.2.1|u%d", i), t0)
	}
	l.mu.Lock()
	first := l.lastPurge
	l.mu.Unlock()
	l.allow("192.0.2.1|again", t0.Add(time.Second))
	l.mu.Lock()
	second := l.lastPurge
	l.mu.Unlock()
	if !first.Equal(second) {
		t.Fatal("the purge ran again one second later; it should wait a full window")
	}
}

// With the admission queue full, a login is refused before it touches the
// limiter or waits on argonSem.
func TestLoginRefusedWhenQueueFull(t *testing.T) {
	h := setup2FAServer(t)
	for i := 0; i < cap(loginQueue); i++ {
		loginQueue <- struct{}{}
	}
	t.Cleanup(func() {
		for len(loginQueue) > 0 {
			<-loginQueue
		}
	})

	rec, _ := loginReal(t, h, `{"user":"admin","password":"password123"}`)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status %d with a full queue, want 429 (%s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "rate_limited") {
		t.Fatalf("body %s, want error rate_limited", rec.Body.String())
	}
}

// Once the queue drains, normal logins work again: the slot is released even
// when the request fails.
func TestLoginQueueSlotReleased(t *testing.T) {
	h := setup2FAServer(t)
	for i := 0; i < cap(loginQueue)+5; i++ {
		loginReal(t, h, `{"user":"admin","password":"wrong-password"}`)
	}
	if n := len(loginQueue); n != 0 {
		t.Fatalf("%d queue slots still held after the requests returned", n)
	}
}

// The limiter key embeds the username, so an oversized one must be refused
// before it can create any state: otherwise each fresh key could hold up to the
// 1 MiB decodeJSON allows, for the 15 minutes an idle key is kept.
func TestLoginRefusesOversizedUsernameWithoutState(t *testing.T) {
	d, h := setup2FAServerWithLimiter(t)
	huge := strings.Repeat("a", 512*1024)
	rec, _ := loginReal(t, h, `{"user":"`+huge+`","password":"x"}`)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status %d, want 401", rec.Code)
	}
	d.loginLimiter.mu.Lock()
	n := len(d.loginLimiter.att)
	d.loginLimiter.mu.Unlock()
	if n != 0 {
		t.Fatalf("%d limiter entries after an oversized username; want none", n)
	}
}

// setup2FAServerWithLimiter is setup2FAServer, but also hands back the Server
// so a test can inspect the limiter's state directly.
func setup2FAServerWithLimiter(t *testing.T) (*Server, http.Handler) {
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
		t.Fatalf("create: %v", err)
	}
	srv := NewServer(Deps{
		Cfg:   &config.Config{Mock: true},
		DB:    d,
		Auth:  auth.NewManager(d, secret, false),
		Users: us,
	})
	return srv, srv.Handler()
}
