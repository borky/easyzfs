// applyhelper_test.go — deploy/easyzfs-apply-update, the root helper that
// installs a staged update only if it matches the official checksums.txt of
// the staged tag. Run unprivileged against a temp dir and a local TLS server
// standing in for GitHub releases.
package updater

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

type applyEnv struct {
	dir, upd, bin string
	env           []string
	sums          map[string]string // tag → checksums.txt body
}

func newApplyEnv(t *testing.T) *applyEnv {
	t.Helper()
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("curl not available")
	}
	e := &applyEnv{dir: t.TempDir(), sums: map[string]string{}}
	e.upd = filepath.Join(e.dir, "update")
	e.bin = filepath.Join(e.dir, "installed")
	if err := os.MkdirAll(e.upd, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(e.bin, []byte("old binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tag, ok := strings.CutSuffix(strings.TrimPrefix(r.URL.Path, "/"), "/checksums.txt")
		body, found := e.sums[tag]
		if !ok || !found {
			http.NotFound(w, r)
			return
		}
		fmt.Fprint(w, body)
	}))
	t.Cleanup(srv.Close)
	ca := filepath.Join(e.dir, "ca.pem")
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	if err := os.WriteFile(ca, pemBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	e.env = append(os.Environ(),
		"EASYZFS_RELEASES_BASE="+srv.URL,
		"EASYZFS_APPLY_ARCH=x86_64",
		"EASYZFS_APPLY_NO_RESTART=1",
		"CURL_CA_BUNDLE="+ca,
	)
	return e
}

// stage leaves what the updater would: the binary, its tag, the flag.
func (e *applyEnv) stage(t *testing.T, content, tag string) {
	t.Helper()
	must(t, os.WriteFile(filepath.Join(e.upd, "easyzfs.new"), []byte(content), 0o755))
	must(t, os.WriteFile(filepath.Join(e.upd, "easyzfs.new.tag"), []byte(tag+"\n"), 0o644))
	must(t, os.WriteFile(filepath.Join(e.upd, ".restart-me"), nil, 0o644))
}

func (e *applyEnv) publish(tag, content string) {
	sum := sha256.Sum256([]byte(content))
	e.sums[tag] = hex.EncodeToString(sum[:]) + "  easyzfs_linux_amd64\n" +
		strings.Repeat("0", 64) + "  easyzfs_linux_arm64\n"
}

func (e *applyEnv) run(t *testing.T) (string, error) {
	t.Helper()
	cmd := exec.Command("bash", "../../deploy/easyzfs-apply-update", e.upd, e.bin)
	cmd.Env = e.env
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func (e *applyEnv) installed(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(e.bin)
	must(t, err)
	return string(b)
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func TestApplyHelperInstallsMatchingRelease(t *testing.T) {
	e := newApplyEnv(t)
	e.publish("v2.9.30", "genuine release")
	e.stage(t, "genuine release", "v2.9.30")
	if out, err := e.run(t); err != nil {
		t.Fatalf("helper refused a genuine release: %v\n%s", err, out)
	}
	if got := e.installed(t); got != "genuine release" {
		t.Fatalf("installed %q", got)
	}
	if _, err := os.Stat(filepath.Join(e.upd, ".restart-me")); !os.IsNotExist(err) {
		t.Error(".restart-me was not consumed")
	}
}

func TestApplyHelperRefuses(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T, e *applyEnv)
	}{
		{"swapped binary", func(t *testing.T, e *applyEnv) {
			e.publish("v2.9.30", "genuine release")
			e.stage(t, "tampered", "v2.9.30")
		}},
		{"no tag", func(t *testing.T, e *applyEnv) {
			e.publish("v2.9.30", "genuine release")
			e.stage(t, "genuine release", "v2.9.30")
			must(t, os.Remove(filepath.Join(e.upd, "easyzfs.new.tag")))
		}},
		{"tag with a path in it", func(t *testing.T, e *applyEnv) {
			e.publish("v2.9.30", "genuine release")
			e.stage(t, "genuine release", "v2.9.30/../../evil")
		}},
		{"unpublished tag", func(t *testing.T, e *applyEnv) {
			e.stage(t, "genuine release", "v9.9.9")
		}},
		{"checksums without our asset", func(t *testing.T, e *applyEnv) {
			e.sums["v2.9.30"] = strings.Repeat("0", 64) + "  easyzfs_linux_arm64\n"
			e.stage(t, "genuine release", "v2.9.30")
		}},
		{"empty placeholder", func(t *testing.T, e *applyEnv) {
			e.publish("v2.9.30", "")
			e.stage(t, "", "v2.9.30")
		}},
		{"symlinked binary", func(t *testing.T, e *applyEnv) {
			e.publish("v2.9.30", "genuine release")
			e.stage(t, "genuine release", "v2.9.30")
			target := filepath.Join(e.dir, "elsewhere")
			must(t, os.WriteFile(target, []byte("genuine release"), 0o755))
			nb := filepath.Join(e.upd, "easyzfs.new")
			must(t, os.Remove(nb))
			must(t, os.Symlink(target, nb))
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newApplyEnv(t)
			c.setup(t, e)
			if out, err := e.run(t); err == nil {
				t.Fatalf("helper installed it:\n%s", out)
			}
			if got := e.installed(t); got != "old binary" {
				t.Fatalf("installed binary changed to %q", got)
			}
		})
	}
}

// Rollback must stage the old binary with its own tag, or the root helper
// would verify it against the release it is replacing.
func TestRollbackCarriesTheTag(t *testing.T) {
	for _, withTag := range []bool{true, false} {
		u := New("2.9.9", t.TempDir(), "")
		must(t, os.MkdirAll(u.updateDir(), 0o755))
		must(t, os.WriteFile(u.NewBinary(), []byte("new"), 0o755))
		must(t, os.WriteFile(u.NewTag(), []byte("v2.9.30\n"), 0o644))
		must(t, os.WriteFile(u.NewBinary()+".old", []byte("old"), 0o755))
		if withTag {
			must(t, os.WriteFile(u.oldTag(), []byte("v2.9.20\n"), 0o644))
		}
		must(t, u.Rollback())
		b, err := os.ReadFile(u.NewTag())
		switch {
		case withTag && (err != nil || string(b) != "v2.9.20\n"):
			t.Errorf("tag after rollback = %q, %v; want v2.9.20", b, err)
		case !withTag && !os.IsNotExist(err):
			t.Errorf("an untagged backup kept the newer tag %q", b)
		}
	}
}
