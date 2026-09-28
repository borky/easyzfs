// rootmode_test.go — deploy/install.sh never installs root mode silently
// (remediation spec P4): unattended it needs --i-understand-root-mode, and
// interactively a prompt that defaults to no.
package actions

import (
	"os"
	"os/exec"
	"path/filepath"
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
		readonly, envRO     string
		wantMode            string
		wantPrompt, wantErr bool
	}{
		{"not root mode", "0", "0", "0", "0", "0", "0", "0", false, false},
		{"unattended with ack", "1", "1", "1", "0", "0", "0", "1", false, false},
		{"unattended without ack", "1", "1", "0", "0", "0", "0", "", false, true},
		{"interactive, confirmed", "1", "0", "0", "1", "0", "0", "1", true, false},
		{"interactive, declined", "1", "0", "0", "0", "0", "0", "0", true, false},
		{"interactive with ack", "1", "0", "1", "0", "0", "0", "1", false, false},
		// Picked from the menu under --read-only, or over an install whose
		// env file is read-only: refused, whatever was acknowledged.
		{"with --read-only", "1", "0", "1", "1", "1", "0", "", false, true},
		{"over a read-only env file", "1", "1", "1", "0", "0", "1", "", false, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			script := `
warn() { :; }; info() { :; }
die() { echo "DIE $*"; exit 1; }
confirm() { echo PROMPTED; [ "$ANS" = "1" ]; }
SVC_USER=easyzfs SUDO=() ENV_FILE="$ENVF"
[ "$ENVRO" = "1" ] && echo EASYZFS_READONLY=1 > "$ENV_FILE"
eval "$(sed -n '/^env_readonly() {/,/^}/p;/^root_mode_gate() {/,/^}/p' ../../deploy/install.sh)"
OPT_ROOT_MODE=$ROOT OPT_YES=$YES OPT_ROOT_ACK=$ACK OPT_READONLY=$RO
root_mode_gate
echo "MODE=$OPT_ROOT_MODE"`
			cmd := exec.Command("bash", "-c", script)
			cmd.Env = append(cmd.Environ(), "ROOT="+c.root, "YES="+c.yes, "ACK="+c.ack, "ANS="+c.ans,
				"RO="+c.readonly, "ENVRO="+c.envRO, "ENVF="+filepath.Join(t.TempDir(), "env"))
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

// Flag combinations that would leave an install inconsistent are refused
// before anything runs.
func TestInstallerRefusesRootModeCombinations(t *testing.T) {
	for _, args := range [][]string{
		{"--update", "--root-mode", "--binary", "/nonexistent"},
		{"--root-mode", "--read-only", "--yes", "--i-understand-root-mode"},
	} {
		cmd := exec.Command("bash", append([]string{"../../deploy/install.sh"}, args...)...)
		cmd.Env = append(cmd.Environ(), "DRY_RUN=1")
		out, err := cmd.CombinedOutput()
		if err == nil || !strings.Contains(string(out), "--root-mode") || strings.Contains(string(out), "[DRY-RUN]") {
			t.Errorf("%v: err=%v\n%s", args, err, out)
		}
	}
}

// confirm() under whiptail: a question whose default is no must open with
// No focused, or a bare Enter answers yes (it did, for root mode).
func TestConfirmWhiptailDefaults(t *testing.T) {
	bin := t.TempDir()
	stub := "#!/bin/bash\nprintf '%s\\n' \"$@\" > \"$ARGS_OUT\"\nexit 1\n"
	if err := os.WriteFile(filepath.Join(bin, "whiptail"), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, def := range []string{"0", "1"} {
		argsOut := filepath.Join(t.TempDir(), "args")
		script := `
eval "$(sed -n '/^confirm() {/,/^}/p' ../../deploy/install.sh)"
APP=EasyZFS OPT_YES=0 USE_WHIPTAIL=1
confirm "question" "$DEF" || true`
		cmd := exec.Command("bash", "-c", script)
		cmd.Env = append(cmd.Environ(), "PATH="+bin+":"+os.Getenv("PATH"), "ARGS_OUT="+argsOut, "DEF="+def)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("def=%s: %v\n%s", def, err, out)
		}
		b, err := os.ReadFile(argsOut)
		if err != nil {
			t.Fatalf("def=%s: whiptail not called: %v", def, err)
		}
		args := strings.Split(strings.TrimSpace(string(b)), "\n")
		hasNo := false
		for _, a := range args {
			if a == "--defaultyes" {
				t.Errorf("def=%s: --defaultyes is not a whiptail option", def)
			}
			hasNo = hasNo || a == "--defaultno"
		}
		if hasNo != (def == "0") {
			t.Errorf("def=%s: whiptail args %v", def, args)
		}
	}
}
