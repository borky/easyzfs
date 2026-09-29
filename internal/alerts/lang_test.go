// lang_test.go — every notification reaches its reader in the language they
// chose: the shared channels in the configured notification language (or the
// most recently active admin's), e-mail in each recipient's.
package alerts

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"easyzfs/internal/channels"
	"easyzfs/internal/db"
	"easyzfs/internal/hub"
	"easyzfs/internal/notifier"
	"easyzfs/internal/settings"
)

func TestChannelsFollowTheNotificationLanguage(t *testing.T) {
	cases := []struct {
		name    string
		setting string
		admin   string // language, ui_lang of the only admin ("" = no admin)
		want    string
	}{
		{"explicit Spanish", "es", "", "Capacidad de pool"},
		{"explicit English over a Spanish admin", "en", "es,es", "Pool capacity"},
		{"auto follows the admin's UI", "auto", "auto,es", "Capacidad de pool"},
		{"auto, the admin's own choice wins", "auto", "en,es", "Pool capacity"},
		{"auto without admins: English", "auto", "", "Pool capacity"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d, err := db.Open(t.TempDir() + "/test.db")
			if err != nil {
				t.Fatal(err)
			}
			defer d.Close()
			if err := db.Migrate(context.Background(), d); err != nil {
				t.Fatal(err)
			}
			st, err := settings.NewStore(d)
			if err != nil {
				t.Fatal(err)
			}
			cur, _ := st.Load(context.Background())
			cur.Lang = c.setting
			if err := st.Save(context.Background(), cur); err != nil {
				t.Fatal(err)
			}
			if c.admin != "" {
				l, ui, _ := strings.Cut(c.admin, ",")
				if _, err := d.Exec("INSERT INTO users(user, pass_hash, role, language, ui_lang) VALUES ('root','x','admin',?,?)", l, ui); err != nil {
					t.Fatal(err)
				}
			}
			got := make(chan string, 1)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var p map[string]string
				_ = json.NewDecoder(r.Body).Decode(&p)
				got <- p["title"] + " | " + p["message"]
			}))
			defer srv.Close()
			a := New(d, hub.NewHub(), st)
			a.SetChannels(channels.New(channels.Config{NtfyURL: srv.URL}))
			a.RaiseKind(context.Background(), "warn", "pool.tank", "pools:tank",
				"Pool tank at 95% capacity (warning ≥ 90%)", "pool_capacity",
				map[string]any{"pool": "tank", "pct": 95, "threshold": 90})
			select {
			case m := <-got:
				if !strings.Contains(m, c.want) {
					t.Errorf("ntfy got %q, want %q in it", m, c.want)
				}
			case <-time.After(4 * time.Second):
				t.Fatal("nothing reached ntfy")
			}
		})
	}
}

// A user left on "auto" gets e-mail in the language their UI was last seen
// in (users.ui_lang), not the English default.
func TestEmailFollowsTheUILanguage(t *testing.T) {
	d, err := db.Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if err := db.Migrate(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	st, err := settings.NewStore(d)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec("INSERT INTO users(user, pass_hash, role, language, ui_lang, email) VALUES ('alice','x','user','auto','es','alice@example.com')"); err != nil {
		t.Fatal(err)
	}
	fake := newSMTPFake(t)
	_, port, _ := net.SplitHostPort(fake.ln.Addr().String())
	var p int
	fmt.Sscanf(port, "%d", &p)
	m, err := notifier.NewMailer(notifier.SMTP{Host: "127.0.0.1", Port: p, From: "easyzfs@example.com", Encryption: "none", Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	a := New(d, hub.NewHub(), st)
	a.SetEmail(m)
	a.RaiseKind(context.Background(), "warn", "smart.sdb", "disks:sdb", "SMART warnings on sdb: not available", "smart_status",
		map[string]any{"dev": "sdb", "detail": "not available"})
	select {
	case msg := <-fake.msgs:
		flat := strings.ReplaceAll(msg, "=\n", "") // quoted-printable soft breaks
		if !strings.Contains(flat, "no disponible") || strings.Contains(flat, "not available") {
			t.Errorf("e-mail not in Spanish (or its SMART detail untranslated):\n%s", msg)
		}
	case <-time.After(4 * time.Second):
		t.Fatal("no e-mail")
	}
}
