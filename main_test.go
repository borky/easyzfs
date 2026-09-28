package main

import (
	"os"
	"path/filepath"
	"testing"

	"easyzfs/internal/config"
)

// Read-only and demo deployments start nothing that changes storage on its
// own (remediation spec P8).
func TestBackgroundJobs(t *testing.T) {
	for _, c := range []struct {
		name               string
		cfg                config.Config
		sched, repl, purge bool
	}{
		{"production", config.Config{}, true, true, true},
		{"mock", config.Config{Mock: true}, true, true, false},
		{"read-only", config.Config{ReadOnly: true}, false, false, false},
		{"demo", config.Config{Demo: true, Mock: true}, false, false, false},
		{"demo without mock", config.Config{Demo: true}, false, false, false},
	} {
		s, r, p := backgroundJobs(&c.cfg)
		if s != c.sched || r != c.repl || p != c.purge {
			t.Errorf("%s: sched=%v repl=%v purge=%v, want %v %v %v", c.name, s, r, p, c.sched, c.repl, c.purge)
		}
	}
}

// The Go memory limit follows the unit's MemoryMax (cgroup v2).
func TestCgroupMemoryMax(t *testing.T) {
	root := t.TempDir()
	unit := filepath.Join(root, "system.slice", "easyzfs.service")
	if err := os.MkdirAll(unit, 0o755); err != nil {
		t.Fatal(err)
	}
	proc := filepath.Join(t.TempDir(), "cgroup")
	write := func(p, s string) {
		t.Helper()
		if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(proc, "0::/system.slice/easyzfs.service\n")
	write(filepath.Join(unit, "memory.max"), "268435456\n")
	if got := cgroupMemoryMax(proc, root); got != 268435456 {
		t.Errorf("MemoryMax=256M: %d", got)
	}
	write(filepath.Join(unit, "memory.max"), "max\n")
	if got := cgroupMemoryMax(proc, root); got != 0 {
		t.Errorf("unlimited: %d, want 0", got)
	}
	write(proc, "12:memory:/system.slice/easyzfs.service\n") // cgroup v1
	if got := cgroupMemoryMax(proc, root); got != 0 {
		t.Errorf("cgroup v1: %d, want 0", got)
	}
}
