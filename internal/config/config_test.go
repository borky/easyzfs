package config

import (
	"os/exec"
	"strings"
	"testing"
)

// Without LISTEN_ADDR the service listens on loopback only (remediation
// spec P8).
func TestDefaultListenIsLoopback(t *testing.T) {
	t.Setenv("LISTEN_ADDR", "")
	if got := Load().ListenAddr; got != "127.0.0.1:8080" {
		t.Fatalf("default ListenAddr = %q, want 127.0.0.1:8080", got)
	}
	t.Setenv("LISTEN_ADDR", ":9000")
	if got := Load().ListenAddr; got != ":9000" {
		t.Fatalf("explicit ListenAddr = %q", got)
	}
}

// An unattended install writes a loopback LISTEN_ADDR.
func TestInstallerDefaultListenIsLoopback(t *testing.T) {
	script := `
die() { echo "DIE $*"; exit 1; }
eval "$(sed -n '/^choose_listen_host() {/,/^}/p;/^listen_addr() {/,/^}/p' ../../deploy/install.sh)"
OPT_YES=1 LISTEN_HOST="" OPT_PORT=8080 DRY_RUN=1
choose_listen_host
listen_addr`
	out, err := exec.Command("bash", "-c", script).CombinedOutput()
	if err != nil || strings.TrimSpace(string(out)) != "127.0.0.1:8080" {
		t.Fatalf("unattended LISTEN_ADDR = %q (%v), want 127.0.0.1:8080", out, err)
	}
}
