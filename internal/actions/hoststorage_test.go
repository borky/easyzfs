// hoststorage_test.go — recognising the host's own storage, fed with what a
// Proxmox VE 8.4 VM (root on rpool) actually shows: its mountinfo, 'zfs list'
// with a VM disk and a container subvolume present, and its storage.cfg.
package actions

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// usePVEHost points the host view at the captured files; pve=false models a
// plain Debian host, storageErr a storage.cfg sudo would not read.
func usePVEHost(t *testing.T, pve bool, storageErr error) {
	t.Helper()
	zl, err := os.ReadFile(filepath.Join("testdata", "zfs_list_pve_guests.txt"))
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := os.ReadFile(filepath.Join("testdata", "storage_cfg_pve.txt"))
	if err != nil {
		t.Fatal(err)
	}
	savedMI, savedPVE, savedL, savedS := mountinfoPath, pveDir, listAllDatasets, readStorageCfg
	t.Cleanup(func() { mountinfoPath, pveDir, listAllDatasets, readStorageCfg = savedMI, savedPVE, savedL, savedS })
	mountinfoPath = filepath.Join("testdata", "mountinfo_pve.txt")
	pveDir = t.TempDir()
	if !pve {
		pveDir = filepath.Join(pveDir, "absent")
	}
	listAllDatasets = func(context.Context) ([]byte, error) { return zl, nil }
	readStorageCfg = func(context.Context) ([]byte, error) { return cfg, storageErr }
}

func TestHostViewOnProxmox(t *testing.T) {
	usePVEHost(t, true, nil)
	h, err := LoadHostView(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if h.RootDataset != "rpool/ROOT/pve-1" || h.HostPool != "rpool" || !h.PVE || h.StorageRoots["rpool/data"] != "local-zfs" {
		t.Fatalf("view = %+v", h)
	}
	if k, _ := h.PoolKind("rpool"); k != HostSystem {
		t.Errorf("rpool = %q, want system", k)
	}
	if k, _ := h.PoolKind("tank"); k != "" {
		t.Errorf("a pool the host knows nothing about = %q", k)
	}
	for name, want := range map[string]string{
		"rpool":                           HostSystem,
		"rpool/ROOT":                      HostSystem,
		"rpool/ROOT/pve-1":                HostSystem,
		"rpool/ROOT/pve-2":                HostSystem, // another boot environment
		"rpool/ROOT/pve-1@before-upgrade": HostSystem,
		"rpool/var-lib-vz":                HostSystem, // mounted at /var/lib/vz
		"rpool/var-lib-vz/sub":            HostSystem,
		"rpool/data":                      HostStorage, // local-zfs
		"rpool/data/vm-100-disk-0":        HostGuest,
		"rpool/data/vm-100-disk-0@snap1":  HostGuest,
		"rpool/data/subvol-101-disk-0":    HostGuest,
		"rpool/data/media":                "",        // the admin's own data
		"tank/vmdata/vm-200-disk-1":       HostGuest, // on any pool
		"tank/vm-9-cloudinit":             HostGuest,
		"tank/media":                      "",
	} {
		if got, why := h.DatasetKind(name); got != want {
			t.Errorf("%s = %q (%s), want %q", name, got, why, want)
		}
	}
}

// Without storage.cfg the guest names still protect: a dataset holding them
// is storage, the pool holding them cannot be removed.
func TestHostViewWithoutStorageCfg(t *testing.T) {
	usePVEHost(t, true, errors.New("sudo: a password is required"))
	h, err := LoadHostView(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !h.StorageUnknown {
		t.Fatal("unreadable storage.cfg not reported")
	}
	if k, _ := h.DatasetKind("rpool/data"); k != HostStorage {
		t.Errorf("rpool/data (holds guest disks) = %q, want storage", k)
	}
}

func TestHostRules(t *testing.T) {
	usePVEHost(t, true, nil)
	h, err := LoadHostView(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	h.guests = append(h.guests, "tank/vmdata/vm-200-disk-1") // a data pool used as Proxmox storage
	for _, c := range []struct {
		op          HostOp
		pool, ds    string
		wantRefused bool
	}{
		{OpPoolRemove, "rpool", "", true},
		{OpPoolLayout, "rpool", "", true}, // replacing a boot disk needs proxmox-boot-tool
		{OpPoolRemove, "tank", "", true},  // holds VM disks
		{OpPoolLayout, "tank", "", false}, // but its failed disks can be replaced
		{OpPoolRemove, "backup", "", false},
		{OpDatasetRemove, "", "rpool/ROOT/pve-1", true},
		{OpDatasetRemove, "", "rpool/data", true},
		{OpDatasetRemove, "", "rpool/data/vm-100-disk-0", true},
		{OpDatasetRemove, "", "rpool/data/media", false},
		{OpDatasetUnmount, "", "rpool/var-lib-vz", true},
		{OpDatasetChange, "", "rpool/data", false}, // compression on storage: fine
		{OpDatasetMountCfg, "", "rpool/data", true},
		{OpDatasetChange, "", "rpool", true}, // everything inherits from it
		{OpRollback, "", "rpool/ROOT/pve-1", true},
		{OpRollback, "", "rpool/data/media", false},
		{OpSnapshotCreate, "", "rpool/ROOT/pve-1", false},
		{OpSnapshotCreate, "", "rpool/data/vm-100-disk-0", true},
		{OpSnapshotDestroy, "", "rpool/data/vm-100-disk-0@x", true},
		{OpSnapshotDestroy, "", "rpool/ROOT/pve-1@x", false},
	} {
		err := h.check(c.op, c.pool, c.ds)
		if refused := errors.Is(err, ErrHostStorage); refused != c.wantRefused {
			t.Errorf("op %d on %q%q: err = %v, want refused=%v", c.op, c.pool, c.ds, err, c.wantRefused)
		}
	}
}

// A root filesystem that is not ZFS: no host pool, only Proxmox's disks and
// storage are protected. Nothing EasyZFS created or recorded is involved.
func TestHostViewNonZFSRoot(t *testing.T) {
	usePVEHost(t, false, nil)
	mi := filepath.Join(t.TempDir(), "mountinfo")
	if err := os.WriteFile(mi, []byte("30 1 8:1 / / rw,relatime shared:1 - ext4 /dev/sda1 rw\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mountinfoPath = mi
	h, err := LoadHostView(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if h.HostPool != "" || h.PVE {
		t.Fatalf("view = %+v", h)
	}
	if k, _ := h.DatasetKind("rpool/ROOT/pve-1"); k == HostSystem {
		t.Error("no ZFS root, yet a system dataset")
	}
}

// The recursive snapshot a scheduled job takes may not sweep guest disks up.
func TestRecursiveSnapshotSkipsGuestTrees(t *testing.T) {
	usePVEHost(t, true, nil)
	ctx := context.Background()
	if err := guardSnapshotCreate(ctx, "rpool/data", true); !errors.Is(err, ErrHostStorage) || !strings.Contains(err.Error(), "-disk-0") {
		t.Fatalf("recursive over guest disks: %v", err)
	}
	if err := guardSnapshotCreate(ctx, "rpool/data/media", true); err != nil {
		t.Fatalf("recursive over data: %v", err)
	}
	if err := guardRenameTarget(ctx, "rpool/ROOT/pve-1/ezx"); !errors.Is(err, ErrHostStorage) {
		t.Fatalf("rename into the OS tree: %v", err)
	}
	if err := guardRenameTarget(ctx, "rpool/data/vm-300-disk-0"); !errors.Is(err, ErrHostStorage) {
		t.Fatalf("rename to a Proxmox disk name: %v", err)
	}
	if err := guardRenameTarget(ctx, "rpool/data/media2"); err != nil {
		t.Fatalf("plain rename: %v", err)
	}
}

func TestParseStorageCfg(t *testing.T) {
	got := parseStorageCfg("dir: local\n\tpath /var/lib/vz\n\nzfspool: local-zfs\n\tpool rpool/data\n\tsparse\n\nzfspool: tank-vm\n\tpool tank/vmdata\n\tcontent images\n# zfspool: old\n\tpool gone\nlvmthin: x\n\tvgname pve\n")
	if len(got) != 2 || got["rpool/data"] != "local-zfs" || got["tank/vmdata"] != "tank-vm" {
		t.Fatalf("roots = %v", got)
	}
}
