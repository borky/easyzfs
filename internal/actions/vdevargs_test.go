package actions

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
)

func TestPoolCreateAceptaByID(t *testing.T) {
	svc, logFile := newTestService(t)
	if err := svc.PoolCreate(context.Background(), "tester", "tank", "mirror",
		[]string{"/dev/disk/by-id/ata-WDC_WD40EFRX_WD-WCC4E1234567", "/dev/disk/by-id/ata-WDC_WD40EFRX_WD-WCC4E7654321"},
		12, true); err != nil {
		t.Fatalf("PoolCreate con by-id: %v", err)
	}
	out, _ := os.ReadFile(logFile)
	if got := strings.TrimSpace(string(out)); got != "create -o ashift=12 tank mirror /dev/disk/by-id/ata-WDC_WD40EFRX_WD-WCC4E1234567 /dev/disk/by-id/ata-WDC_WD40EFRX_WD-WCC4E7654321" {
		t.Fatalf("argv zpool = %q", got)
	}
}

func TestVdevAddAceptaByID(t *testing.T) {
	svc, logFile := newTestService(t)
	if err := svc.VdevAdd(context.Background(), "tester", "tank", "mirror",
		[]string{"/dev/disk/by-id/ata-WDC_WD40EFRX_WD-WCC4E1234567", "/dev/disk/by-id/ata-WDC_WD40EFRX_WD-WCC4E7654321"},
		true, true); err != nil {
		t.Fatalf("VdevAdd con by-id: %v", err)
	}
	out, _ := os.ReadFile(logFile)
	// Asked for, a checkpoint is taken first, so a wrong add can be rewound.
	want := "get -Hp -o value checkpoint tank\ncheckpoint tank\n" +
		"add tank mirror /dev/disk/by-id/ata-WDC_WD40EFRX_WD-WCC4E1234567 /dev/disk/by-id/ata-WDC_WD40EFRX_WD-WCC4E7654321"
	if got := strings.TrimSpace(string(out)); got != want {
		t.Fatalf("argv zpool = %q", got)
	}
}

// An existing checkpoint is refused, not reused: it would rewind to its own
// date. Without the option nothing touches checkpoints: one left behind
// blocks zpool replace and hot spares.
func TestVdevAddCheckpointIsOptIn(t *testing.T) {
	svc, logFile := newTestService(t)
	t.Setenv("FAKE_CHECKPOINT", "1726000000")
	if err := svc.VdevAdd(context.Background(), "tester", "tank", "mirror", []string{"sdx", "sdy"}, true, true); !errors.Is(err, ErrConflict) {
		t.Fatalf("existing checkpoint: %v, want ErrConflict", err)
	}
	if out, _ := os.ReadFile(logFile); strings.Contains(string(out), "add tank") {
		t.Fatalf("added despite the refusal:\n%s", out)
	}
	svc2, logFile2 := newTestService(t)
	if err := svc2.VdevAdd(context.Background(), "tester", "tank", "mirror", []string{"sdx", "sdy"}, true, false); err != nil {
		t.Fatal(err)
	}
	if out, _ := os.ReadFile(logFile2); strings.TrimSpace(string(out)) != "add tank mirror sdx sdy" {
		t.Fatalf("without the option: %q", out)
	}
}

func TestVdevArgsRechazaDiscoInvalido(t *testing.T) {
	svc, _ := newTestService(t)
	for _, bad := range []string{"/dev/sda", "sda;echo x", "../sda", ""} {
		if err := svc.VdevAdd(context.Background(), "tester", "tank", "mirror",
			[]string{bad, "sdb"}, true, false); !errors.Is(err, ErrInvalidDev) {
			t.Errorf("VdevAdd(%q) = %v, esperaba ErrInvalidDev", bad, err)
		}
	}
}
