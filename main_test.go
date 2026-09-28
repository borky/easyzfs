package main

import (
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
