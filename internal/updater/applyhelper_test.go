// applyhelper_test.go — deploy/easyzfs-apply-update, the root helper that
// installs a staged update only if it matches the official checksums.txt of
// the staged tag, whose signature verifies against the trusted key. Run
// unprivileged against a temp dir and a local TLS server standing in for
// GitHub releases. minisign itself is a stub that accepts exactly the
// signatures sign() makes: what is tested is that the helper asks for the
// right verification and obeys its answer, not minisign's cryptography.
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
	"sync"
	"testing"
)

type applyEnv struct {
	dir, upd, bin string
	env           []string
	sums          map[string]string // tag → checksums.txt body
	sigs          map[string]string // tag → checksums.txt.minisig body
	mu            sync.Mutex        // the server reads sums/sigs from its own goroutines
}

const trustedKey = "RWTtrustedKeyForTests"

// stubMinisign accepts "minisign -V -q -P <key> -m <file> -x <sig>" when the
// signature is "sig:<key>:<sha256 of file>".
const stubMinisign = `#!/bin/bash
while [ $# -gt 0 ]; do
  case "$1" in
    -P) key="$2"; shift ;;
    -m) msg="$2"; shift ;;
    -x) sig="$2"; shift ;;
  esac
  shift
done
[ -n "$key" ] && [ -f "$msg" ] && [ -f "$sig" ] || exit 2
[ "$(cat "$sig")" = "sig:${key}:$(sha256sum "$msg" | cut -d' ' -f1)" ]
`

func signature(key, body string) string {
	sum := sha256.Sum256([]byte(body))
	return "sig:" + key + ":" + hex.EncodeToString(sum[:])
}

func newApplyEnv(t *testing.T) *applyEnv {
	t.Helper()
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("curl not available")
	}
	e := &applyEnv{dir: t.TempDir(), sums: map[string]string{}, sigs: map[string]string{}}
	e.upd = filepath.Join(e.dir, "update")
	e.bin = filepath.Join(e.dir, "bin", "installed")
	for _, d := range []string{e.upd, filepath.Dir(e.bin), filepath.Join(e.dir, "stub")} {
		must(t, os.MkdirAll(d, 0o755))
	}
	must(t, os.WriteFile(filepath.Join(e.dir, "stub", "minisign"), []byte(stubMinisign), 0o755))
	if err := os.WriteFile(e.bin, []byte("old binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/")
		e.mu.Lock()
		defer e.mu.Unlock()
		var body string
		var found bool
		if tag, ok := strings.CutSuffix(path, "/checksums.txt"); ok {
			body, found = e.sums[tag]
		} else if tag, ok := strings.CutSuffix(path, "/checksums.txt.minisig"); ok {
			body, found = e.sigs[tag]
		}
		if !found {
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
		"EASYZFS_APPLY_PUBKEY="+trustedKey,
		"PATH="+filepath.Join(e.dir, "stub")+":"+os.Getenv("PATH"),
	)
	return e
}

// setenv overrides one variable (exec keeps the last duplicate).
func (e *applyEnv) setenv(k, v string) { e.env = append(e.env, k+"="+v) }

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
	e.sigs[tag] = signature(trustedKey, e.sums[tag])
}

func (e *applyEnv) refusal(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(e.upd, "apply-refused"))
	if err != nil {
		return ""
	}
	return string(b)
}

func (e *applyEnv) run(t *testing.T) (string, error) {
	t.Helper()
	e.mu.Lock() // orders the setup's map writes before the server's reads
	e.mu.Unlock()
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
	unit := filepath.Join(e.dir, "easyzfs.service")
	must(t, os.WriteFile(unit, []byte("[Service]\nUser=easyzfs\nProtectSystem=full\nReadWritePaths=/var/lib/easyzfs\n# ProtectHome=yes is explained here\nProtectHome=yes\nPrivateTmp=yes\nMemoryMax=256M\n"), 0o644))
	e.env = append(e.env, "EASYZFS_APPLY_UNIT="+unit)
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
	if r := e.refusal(t); r != "" {
		t.Errorf("a successful install left a refusal: %q", r)
	}
	// The private mount namespace goes, comments and the rest stay.
	b, _ := os.ReadFile(unit)
	if got := string(b); got != "[Service]\nUser=easyzfs\n# ProtectHome=yes is explained here\nMemoryMax=256M\n" {
		t.Errorf("unit after update:\n%s", got)
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
		{"no trusted key embedded", func(t *testing.T, e *applyEnv) {
			e.publish("v2.9.30", "genuine release")
			e.stage(t, "genuine release", "v2.9.30")
			e.setenv("EASYZFS_APPLY_PUBKEY", "")
		}},
		{"unsigned release", func(t *testing.T, e *applyEnv) {
			e.publish("v2.9.30", "genuine release")
			e.stage(t, "genuine release", "v2.9.30")
			delete(e.sigs, "v2.9.30")
		}},
		{"signed by another key", func(t *testing.T, e *applyEnv) {
			e.publish("v2.9.30", "genuine release")
			e.stage(t, "genuine release", "v2.9.30")
			e.sigs["v2.9.30"] = signature("RWTsomeoneElse", e.sums["v2.9.30"])
		}},
		{"garbage signature", func(t *testing.T, e *applyEnv) {
			e.publish("v2.9.30", "genuine release")
			e.stage(t, "genuine release", "v2.9.30")
			e.sigs["v2.9.30"] = "untrusted comment: nothing\nAAAA\n"
		}},
		// A matching binary and checksum pair, not the one that was signed:
		// what a compromised release pipeline would publish.
		{"checksums replaced after signing", func(t *testing.T, e *applyEnv) {
			e.publish("v2.9.30", "genuine release")
			sig := e.sigs["v2.9.30"]
			e.publish("v2.9.30", "tampered")
			e.sigs["v2.9.30"] = sig
			e.stage(t, "tampered", "v2.9.30")
		}},
		{"wrong architecture", func(t *testing.T, e *applyEnv) {
			e.publish("v2.9.30", "genuine release")
			e.stage(t, "genuine release", "v2.9.30")
			e.setenv("EASYZFS_APPLY_ARCH", "mips64")
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
			if e.refusal(t) == "" {
				t.Error("the refusal was not left for the daemon to report")
			}
			if _, err := os.Stat(filepath.Join(e.upd, ".restart-me")); !os.IsNotExist(err) {
				t.Error(".restart-me survived a refusal: the path unit would retry forever")
			}
		})
	}
}

// Without minisign nothing can be verified, so nothing is installed.
func TestApplyHelperRefusesWithoutMinisign(t *testing.T) {
	if _, err := exec.LookPath("minisign"); err == nil {
		t.Skip("minisign is installed on this machine; PATH cannot hide it")
	}
	e := newApplyEnv(t)
	e.setenv("PATH", os.Getenv("PATH"))
	e.publish("v2.9.30", "genuine release")
	e.stage(t, "genuine release", "v2.9.30")
	if out, err := e.run(t); err == nil || !strings.Contains(out, "minisign") {
		t.Fatalf("err=%v out=%s", err, out)
	}
	if got := e.installed(t); got != "old binary" {
		t.Fatalf("installed binary changed to %q", got)
	}
}

// An install that fails half way (here: the target directory refuses the
// temporary file) leaves the running binary as it was.
func TestApplyHelperFailedInstallKeepsOldBinary(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	e := newApplyEnv(t)
	e.publish("v2.9.30", "genuine release")
	e.stage(t, "genuine release", "v2.9.30")
	dir := filepath.Dir(e.bin)
	must(t, os.Chmod(dir, 0o555))
	t.Cleanup(func() { os.Chmod(dir, 0o755) })
	if out, err := e.run(t); err == nil {
		t.Fatalf("helper reported success:\n%s", out)
	}
	if got := e.installed(t); got != "old binary" {
		t.Fatalf("installed binary changed to %q", got)
	}
}

// The service account owns the update directory: a symlink it plants as
// apply-refused must not make root write through it.
func TestApplyHelperRefusalDoesNotFollowSymlink(t *testing.T) {
	e := newApplyEnv(t)
	victim := filepath.Join(e.dir, "victim")
	must(t, os.WriteFile(victim, []byte("untouched"), 0o644))
	must(t, os.Symlink(victim, filepath.Join(e.upd, "apply-refused")))
	e.stage(t, "genuine release", "v9.9.9") // unpublished → refused
	if _, err := e.run(t); err == nil {
		t.Fatal("helper installed an unpublished tag")
	}
	if b, _ := os.ReadFile(victim); string(b) != "untouched" {
		t.Fatalf("root wrote through the planted symlink: %q", b)
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
