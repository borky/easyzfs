// sudoers_test.go — deploy/install.sh writes the service's sudo grant
// (pinned_sudoers). Storage tools reach root only through the privileged
// gateway; a direct rule for any of them would let a compromised service
// account skip the gateway's checks, with nothing in CI to say so.
package actions

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// pinnedSudoers runs the installer's generator with the default paths.
func pinnedSudoers(t *testing.T) string {
	t.Helper()
	script := `eval "$(sed -n '/^pinned_sudoers() {/,/^}/p' ../../deploy/install.sh)"
SVC_USER=easyzfs SYSD_HELPER=/usr/local/libexec/easyzfs-sysd
pinned_sudoers /usr/local/bin/easyzfs /usr/bin/lsblk /usr/bin/crontab /usr/bin/fuser /usr/bin/cat`
	gen, err := exec.Command("bash", "-c", script).Output()
	if err != nil {
		t.Fatal(err)
	}
	return string(gen)
}

// No storage tool is granted directly: only the gateway reaches them.
func TestSudoersGrantNoStorageToolDirectly(t *testing.T) {
	gen := pinnedSudoers(t)
	for _, tool := range []string{"/zfs", "/zpool", "/smartctl", "/dd ", "/hdparm", "/udisksctl"} {
		for _, l := range strings.Split(gen, "\n") {
			if strings.Contains(l, "NOPASSWD:") && strings.Contains(l, tool) {
				t.Errorf("direct grant of %s: %s", tool, l)
			}
		}
	}
	if !strings.Contains(gen, "NOPASSWD: /usr/local/bin/easyzfs priv *") {
		t.Errorf("no gateway rule:\n%s", gen)
	}
}

// The static deploy/easyzfs.sudoers (manual installs) must grant exactly what
// the installer grants.
func TestStaticSudoersMatchesInstaller(t *testing.T) {
	gen := []byte(pinnedSudoers(t))
	static, err := os.ReadFile("../../deploy/easyzfs.sudoers")
	if err != nil {
		t.Fatal(err)
	}
	rules := func(b []byte) string {
		var out []string
		for _, l := range strings.Split(string(b), "\n") {
			if strings.Contains(l, "NOPASSWD:") {
				out = append(out, l)
			}
		}
		return strings.Join(out, "\n")
	}
	if g, s := rules(gen), rules(static); g == "" || g != s {
		t.Errorf("deploy/easyzfs.sudoers differs from pinned_sudoers; regenerate it.\ninstaller:\n%s\nstatic:\n%s", g, s)
	}
}
