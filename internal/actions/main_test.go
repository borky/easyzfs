package actions

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// TestMain gives every test in the package a neutral host: root not on ZFS,
// not Proxmox, no datasets. Without it the host-storage guard would read the
// machine running the tests, and refuse everything where zfs is missing.
// hoststorage_test.go replaces this with a captured Proxmox host.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "easyzfs-actions-test")
	if err != nil {
		panic(err)
	}
	mi := filepath.Join(dir, "mountinfo")
	if err := os.WriteFile(mi, []byte("30 1 8:1 / / rw,relatime shared:1 - ext4 /dev/sda1 rw\n"), 0o644); err != nil {
		panic(err)
	}
	mountinfoPath = mi
	pveDir = filepath.Join(dir, "no-pve")
	listAllDatasets = func(context.Context) ([]byte, error) { return nil, nil }
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}
