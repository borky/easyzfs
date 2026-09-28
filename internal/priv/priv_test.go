package priv

import (
	"os"
	"testing"
)

// The gateway only ever runs as root (through sudo); anyone else calling it
// directly gets nothing run.
func TestMainRefusesWithoutRoot(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root")
	}
	if got := Main([]string{"zfs", "list"}); got != 3 {
		t.Fatalf("exit %d, want 3", got)
	}
}
