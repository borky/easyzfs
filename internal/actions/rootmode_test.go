// rootmode_test.go — deploy/install.sh never installs root mode silently
// (remediation spec P4): unattended it needs --i-understand-root-mode, and
// interactively a prompt that defaults to no.
package actions

import (
	"os/exec"
	"strings"
	"testing"
)

// The real installer, unattended, refuses before doing anything.
func TestInstallerRefusesUnacknowledgedRootMode(t *testing.T) {
	cmd := exec.Command("bash", "../../deploy/install.sh", "--root-mode", "--yes")
	cmd.Env = append(cmd.Environ(), "DRY_RUN=1")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("installer accepted --root-mode --yes without the acknowledgement:\n%s", out)
	}
	if !strings.Contains(string(out), "--i-understand-root-mode") {
		t.Fatalf("refusal does not name the flag:\n%s", out)
	}
	if strings.Contains(string(out), "[DRY-RUN]") {
		t.Fatalf("installer did work before refusing:\n%s", out)
	}
}

// root_mode_gate's decisions, with confirm stubbed to a scripted answer.
func TestRootModeGate(t *testing.T) {
	cases := []struct {
		name                string
		root, yes, ack, ans string
		wantMode            string
		wantPrompt, wantErr bool
	}{
		{"not root mode", "0", "0", "0", "0", "0", false, false},
		{"unattended with ack", "1", "1", "1", "0", "1", false, false},
		{"unattended without ack", "1", "1", "0", "0", "", false, true},
		{"interactive, confirmed", "1", "0", "0", "1", "1", true, false},
		{"interactive, declined", "1", "0", "0", "0", "0", true, false},
		{"interactive with ack", "1", "0", "1", "0", "1", false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			script := `
warn() { :; }; info() { :; }
die() { echo "DIE $*"; exit 1; }
confirm() { echo PROMPTED; [ "$ANS" = "1" ]; }
SVC_USER=easyzfs
eval "$(sed -n '/^root_mode_gate() {/,/^}/p' ../../deploy/install.sh)"
OPT_ROOT_MODE=$ROOT OPT_YES=$YES OPT_ROOT_ACK=$ACK
root_mode_gate
echo "MODE=$OPT_ROOT_MODE"`
			cmd := exec.Command("bash", "-c", script)
			cmd.Env = append(cmd.Environ(), "ROOT="+c.root, "YES="+c.yes, "ACK="+c.ack, "ANS="+c.ans)
			b, err := cmd.CombinedOutput()
			out := string(b)
			if c.wantErr {
				if err == nil || !strings.Contains(out, "DIE") {
					t.Fatalf("want refusal, got:\n%s", out)
				}
				return
			}
			if err != nil {
				t.Fatalf("%v\n%s", err, out)
			}
			if !strings.Contains(out, "MODE="+c.wantMode) {
				t.Errorf("want MODE=%s, got:\n%s", c.wantMode, out)
			}
			if strings.Contains(out, "PROMPTED") != c.wantPrompt {
				t.Errorf("prompted=%v, want %v:\n%s", strings.Contains(out, "PROMPTED"), c.wantPrompt, out)
			}
		})
	}
}
