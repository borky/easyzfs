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
	"time"
)

// usePVEHost points the host view at the captured files; pve=false models a
// plain Debian host, storageErr a storage.cfg sudo would not read. extra is
// appended to the captured 'zfs list' output.
func usePVEHost(t *testing.T, pve bool, storageErr error, extra ...string) {
	t.Helper()
	zl, err := os.ReadFile(filepath.Join("testdata", "zfs_list_pve_guests.txt"))
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range extra {
		zl = append(zl, []byte(l+"\n")...)
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

func loadView(t *testing.T) *HostView {
	t.Helper()
	h, err := LoadHostView(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func TestHostViewOnProxmox(t *testing.T) {
	usePVEHost(t, true, nil,
		"rpool/var-lib-vz/iso\tfilesystem\t/var/lib/vz/iso",
		"rpool/data/basevol-200-disk-0\tfilesystem\t/rpool/data/basevol-200-disk-0",
		"rpool/data/vm-100-mydisk\tvolume\t-")
	h := loadView(t)
	if h.RootDataset != "rpool/ROOT/pve-1" || !h.OSPools["rpool"] || !h.PVE || h.StorageRoots["rpool/data"] != "local-zfs" {
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
		"rpool/var-lib-vz/iso":            HostSystem,
		"rpool/data":                      HostStorage, // local-zfs
		"rpool/data/vm-100-disk-0":        HostGuest,
		"rpool/data/vm-100-disk-0@snap1":  HostGuest,
		"rpool/data/subvol-101-disk-0":    HostGuest,
		"rpool/data/basevol-200-disk-0":   HostGuest, // a container template
		"rpool/data/vm-100-mydisk":        HostGuest, // named by hand with pvesm alloc
		"rpool/data/media":                "",        // the admin's own data
		"tank/vmdata/vm-200-disk-1":       HostGuest, // on any pool
		"tank/vm-9-cloudinit":             HostGuest,
		"tank/media":                      "",
		"tank/ROOT":                       "", // a data pool's dataset named ROOT is data
	} {
		if got, why := h.DatasetKind(name); got != want {
			t.Errorf("%s = %q (%s), want %q", name, got, why, want)
		}
	}
}

// An ancestor of a dataset mounted outside the pool takes that mount along
// on a recursive destroy or a rename.
func TestHostViewAncestorOfOutsideMount(t *testing.T) {
	usePVEHost(t, true, nil,
		"rpool/srv\tfilesystem\t/rpool/srv",
		"rpool/srv/backups\tfilesystem\t/var/backups/pve")
	if k, why := loadView(t).DatasetKind("rpool/srv"); k != HostSystem {
		t.Fatalf("rpool/srv = %q (%s), want system", k, why)
	}
}

// A dir storage whose path is a dataset's mountpoint is Proxmox storage too.
func TestHostViewDirStorage(t *testing.T) {
	usePVEHost(t, true, nil, "tank\tfilesystem\t/tank", "tank/pve-backup\tfilesystem\t/tank/pve-backup")
	h := loadView(t)
	cfg := "dir: tank-backup\n\tpath /tank/pve-backup\n\tcontent backup\n"
	zp, dirs := parseStorageCfg(cfg)
	for name, e := range h.datasets {
		if id, ok := dirs[e.mountpoint]; ok {
			h.StorageRoots[name] = id
		}
	}
	_ = zp
	if k, _ := h.DatasetKind("tank/pve-backup"); k != HostStorage {
		t.Fatalf("dir storage dataset = %q, want storage", k)
	}
	if k, _ := h.PoolKind("tank"); k != HostStorage {
		t.Fatalf("its pool = %q, want storage", k)
	}
}

// Without storage.cfg, guest names still protect, and every top-level
// dataset counts as storage rather than guessing it is not.
func TestHostViewWithoutStorageCfg(t *testing.T) {
	usePVEHost(t, true, errors.New("sudo: a password is required"), "tank/images\tfilesystem\t/tank/images")
	h := loadView(t)
	if !h.StorageUnknown {
		t.Fatal("unreadable storage.cfg not reported")
	}
	if k, _ := h.DatasetKind("rpool/data"); k != HostStorage {
		t.Errorf("rpool/data (holds guest disks) = %q, want storage", k)
	}
	if k, _ := h.DatasetKind("tank/images"); k != HostStorage {
		t.Errorf("a top-level dataset with storage.cfg unknown = %q, want storage", k)
	}
	if k, _ := h.DatasetKind("tank/images/sub"); k != "" {
		t.Errorf("below it = %q, want data", k)
	}
}

// Ubuntu's root-on-ZFS has a separate boot pool at /boot.
func TestHostViewBootPool(t *testing.T) {
	usePVEHost(t, false, nil)
	mi := filepath.Join(t.TempDir(), "mountinfo")
	if err := os.WriteFile(mi, []byte("30 1 0:27 / / rw - zfs rpool/ROOT/ubuntu_x rw\n40 30 0:30 / /boot rw - zfs bpool/BOOT/ubuntu_x rw\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mountinfoPath = mi
	h := loadView(t)
	for _, p := range []string{"rpool", "bpool"} {
		if k, _ := h.PoolKind(p); k != HostSystem {
			t.Errorf("%s = %q, want system", p, k)
		}
	}
}

func TestHostRules(t *testing.T) {
	usePVEHost(t, true, nil)
	h := loadView(t)
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
		{OpDatasetChange, "", "rpool/data", false},   // compression on storage: fine
		{OpDatasetSensitive, "", "rpool/data", true}, // exec=off would stop its containers
		{OpDatasetSensitive, "", "rpool/data/media", false},
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
	if propHostOp("exec") != OpDatasetSensitive || propHostOp("quota") != OpDatasetSensitive || propHostOp("compression") != OpDatasetChange {
		t.Error("property classes")
	}
}

// A root filesystem that is not ZFS: no OS pool, only Proxmox's disks and
// storage are protected. Nothing EasyZFS created or recorded is involved.
func TestHostViewNonZFSRoot(t *testing.T) {
	usePVEHost(t, false, nil)
	mi := filepath.Join(t.TempDir(), "mountinfo")
	if err := os.WriteFile(mi, []byte("30 1 8:1 / / rw,relatime shared:1 - ext4 /dev/sda1 rw\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mountinfoPath = mi
	h := loadView(t)
	if len(h.OSPools) != 0 || h.PVE {
		t.Fatalf("view = %+v", h)
	}
	if k, _ := h.DatasetKind("rpool/ROOT/pve-1"); k == HostSystem {
		t.Error("no ZFS root, yet a system dataset")
	}
}

func TestSnapshotAndNameGuards(t *testing.T) {
	usePVEHost(t, true, nil)
	ctx := context.Background()
	if err := guardSnapshotCreate(ctx, "rpool/data", true); !errors.Is(err, ErrHostStorage) || !strings.Contains(err.Error(), "-disk-0") {
		t.Fatalf("recursive over guest disks: %v", err)
	}
	if err := guardSnapshotCreate(ctx, "rpool/data/media", true); err != nil {
		t.Fatalf("recursive over data: %v", err)
	}
	for _, bad := range []string{"rpool/ROOT/pve-1/ezx", "rpool/ROOT/ezx", "rpool/var-lib-vz/x", "rpool/data/vm-300-disk-0", "tank/vm-7-mine"} {
		if err := guardNewName(ctx, bad); !errors.Is(err, ErrHostStorage) {
			t.Errorf("new name %s: %v, want refused", bad, err)
		}
	}
	for _, ok := range []string{"rpool/data/media2", "rpool/ezdata", "tank/media"} {
		if err := guardNewName(ctx, ok); err != nil {
			t.Errorf("new name %s: %v", ok, err)
		}
	}
}

func TestParseStorageCfg(t *testing.T) {
	zp, dirs := parseStorageCfg("dir: local\n\tpath /var/lib/vz\n\nzfspool: local-zfs\n\tpool rpool/data\n\tsparse\n\nzfspool: tank-vm\n\tpool\ttank/vmdata\n\tcontent images\n# zfspool: old\n\tpool gone\nzfs: iscsi\n\tpool remote/x\nlvmthin: x\n\tvgname pve\n")
	if len(zp) != 2 || zp["local-zfs"] != "rpool/data" || zp["tank-vm"] != "tank/vmdata" {
		t.Fatalf("zfspools = %v", zp)
	}
	if dirs["/var/lib/vz"] != "local" || len(dirs) != 1 {
		t.Fatalf("dirs = %v", dirs)
	}
}

// Every action that can touch host storage calls the guard, and before
// anything runs: taken out, these fail. The fake zpool logs every call, so
// a checkpoint taken before the refusal shows up too.
func TestActionsAreGuarded(t *testing.T) {
	usePVEHost(t, true, nil)
	svc, logFile := newTestService(t)
	ctx := context.Background()
	for name, call := range map[string]func() error{
		"export rpool":          func() error { return svc.PoolExport(ctx, "t", "rpool", false, false) },
		"destroy rpool":         func() error { return svc.PoolExport(ctx, "t", "rpool", false, true) },
		"add vdev + checkpoint": func() error { return svc.VdevAdd(ctx, "t", "rpool", "mirror", []string{"sdx", "sdy"}, true, true) },
		"offline rpool disk":    func() error { return svc.VdevAction(ctx, "t", "rpool", "sda3", "offline", false) },
		"replace rpool disk":    func() error { return svc.Replace(ctx, "t", "rpool", "sda3", "sdz", true) },
		"expand rpool":          func() error { return svc.PoolExpand(ctx, "t", "rpool", "raidz1-0", "sdz", true) },
		"checkpoint rpool":      func() error { return svc.CheckpointCreate(ctx, "t", "rpool") },
		"discard checkpoint":    func() error { return svc.CheckpointDiscard(ctx, "t", "rpool") },
		"delete pve-1":          func() error { return svc.DatasetDelete(ctx, "t", "rpool/ROOT/pve-1", true) },
		"trash vm disk":         func() error { return svc.DatasetTrash(ctx, "t", "rpool/data/vm-100-disk-0", false) },
		"rename storage":        func() error { return svc.DatasetRename(ctx, "t", "rpool/data", "rpool/data2") },
		"rename into ROOT":      func() error { return svc.DatasetRename(ctx, "t", "rpool/data/media", "rpool/ROOT/media") },
		"promote":               func() error { return svc.DatasetPromote(ctx, "t", "rpool/ROOT/pve-1") },
		"unmount var-lib-vz":    func() error { return svc.DatasetUnmount(ctx, "t", "rpool/var-lib-vz") },
		"quota on storage":      func() error { q := uint64(1); return svc.DatasetPatch(ctx, "t", "rpool/data", &q, nil) },
		"exec=off on storage":   func() error { return svc.DatasetPropSet(ctx, "t", "rpool/data", "exec", "off", "fs", true) },
		"inherit on pve-1":      func() error { return svc.DatasetPropInherit(ctx, "t", "rpool/ROOT/pve-1", "compression", true) },
		"lock storage":          func() error { return svc.DatasetUnloadKey(ctx, "t", "rpool/data") },
		"change key of vm disk": func() error {
			return svc.DatasetChangeKey(ctx, "t", "rpool/data/vm-100-disk-0", "old-pass-1", "new-pass-12")
		},
		"rollback pve-1":     func() error { return svc.SnapshotRollback(ctx, "t", "rpool/ROOT/pve-1@x") },
		"delete vm snapshot": func() error { return svc.SnapshotDelete(ctx, "t", "rpool/data/vm-100-disk-0@x") },
		"snapshot vm disk":   func() error { return svc.SnapshotCreate(ctx, "t", "rpool/data/vm-100-disk-0", "x", false) },
		"create as vm disk name": func() error {
			return svc.DatasetCreate(ctx, "t", "rpool", "data/vm-100-disk-9", "fs", "lz4", 0, 0, false, "", "")
		},
		"clone into ROOT": func() error { return svc.SnapshotClone(ctx, "t", "rpool/data/media@s", "rpool/ROOT/c", "") },
	} {
		if err := call(); !errors.Is(err, ErrHostStorage) {
			t.Errorf("%s: err = %v, want ErrHostStorage", name, err)
		}
	}
	if out, _ := os.ReadFile(logFile); len(out) > 0 {
		t.Fatalf("zpool ran for refused actions:\n%s", out)
	}
	// What must stay possible: disk work on a data pool, maintenance, data.
	if err := svc.VdevAction(ctx, "t", "tank", "sdb", "offline", false); errors.Is(err, ErrHostStorage) {
		t.Errorf("offline on a data pool refused: %v", err)
	}
	if err := svc.DatasetPropSet(ctx, "t", "rpool/data", "compression", "zstd", "fs", false); errors.Is(err, ErrHostStorage) {
		t.Errorf("compression on storage refused: %v", err)
	}
}

// A scheduled job over a Proxmox storage tree snapshots the admin's datasets
// and leaves the guest disks out, in one atomic command.
func TestSnapshotTreeSkipsGuests(t *testing.T) {
	usePVEHost(t, true, nil)
	svc, _ := newTestService(t)
	var ran []string
	saved := runZFSSnap
	runZFSSnap = func(_ context.Context, args ...string) error { ran = args; return nil }
	t.Cleanup(func() { runZFSSnap = saved })
	skipped, err := svc.SnapshotTree(context.Background(), "scheduler", "rpool/data", "auto-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(skipped) != 2 {
		t.Fatalf("skipped = %v, want the VM disk and the container subvolume", skipped)
	}
	got := strings.Join(ran, " ")
	if got != "snapshot rpool/data@auto-1 rpool/data/media@auto-1" {
		t.Fatalf("ran %q", got)
	}
}

// N2: a Proxmox name anywhere in the path (zfs create -p makes the parents
// too), or a place inside a guest's volume, is refused.
func TestNewNameComponents(t *testing.T) {
	usePVEHost(t, true, nil)
	ctx := context.Background()
	for _, bad := range []string{"tank/vm-100-disk-5/x", "rpool/data/subvol-101-disk-0/x"} {
		if err := guardNewName(ctx, bad); !errors.Is(err, ErrHostStorage) {
			t.Errorf("%s: %v, want refused", bad, err)
		}
	}
}

// N3: with storage.cfg unreadable the pool root counts as storage too: its
// properties reach any storage root below it by inheritance.
func TestStorageUnknownCoversPoolRoot(t *testing.T) {
	usePVEHost(t, true, errors.New("denied"), "tank\tfilesystem\t/tank")
	h := loadView(t)
	if k, _ := h.DatasetKind("tank"); k != HostStorage {
		t.Fatalf("tank = %q, want storage", k)
	}
	if err := h.check(OpDatasetSensitive, "", "tank"); !errors.Is(err, ErrHostStorage) {
		t.Fatalf("exec=off on tank: %v", err)
	}
}

// N4: an admin's share mounted under /srv does not turn its parents into
// system storage; /var/lib/vz does.
func TestAdminShareIsNotSystem(t *testing.T) {
	usePVEHost(t, true, nil, "rpool/data/media\tfilesystem\t/srv/media")
	h := loadView(t)
	if k, _ := h.DatasetKind("rpool/data"); k != HostStorage {
		t.Fatalf("rpool/data = %q, want storage", k)
	}
	if k, _ := h.DatasetKind("rpool/data/media"); k != "" {
		t.Fatalf("rpool/data/media = %q, want data", k)
	}
}

// #7: a dir storage whose path is below a dataset's mountpoint belongs to
// that dataset.
func TestDirStorageBelowDataset(t *testing.T) {
	usePVEHost(t, true, nil, "tank\tfilesystem\t/tank", "tank/backup\tfilesystem\t/tank/backup")
	saved := readStorageCfg
	readStorageCfg = func(context.Context) ([]byte, error) {
		return []byte("dir: dump\n\tpath /tank/backup/dump\n\tcontent backup\n"), nil
	}
	t.Cleanup(func() { readStorageCfg = saved })
	h := loadView(t)
	if h.StorageRoots["tank/backup"] != "dump" {
		t.Fatalf("roots = %v", h.StorageRoots)
	}
}

// N5: a tree that is all guest disks is not a successful snapshot.
func TestSnapshotTreeAllGuests(t *testing.T) {
	usePVEHost(t, true, nil)
	svc, _ := newTestService(t)
	if _, err := svc.SnapshotTree(context.Background(), "scheduler", "rpool/data/vm-100-disk-0", "a"); !errors.Is(err, ErrHostStorage) {
		t.Fatalf("guest target: %v", err)
	}
}

// #14: an unreadable host refuses (409 host_unknown, not a claim about the
// target), while disk work on a data pool, decided from mountinfo alone,
// still goes through.
func TestFailClosedButDataPoolDiskWork(t *testing.T) {
	usePVEHost(t, true, nil)
	listAllDatasets = func(context.Context) ([]byte, error) { return nil, errors.New("zfs list timed out") }
	svc, _ := newTestService(t)
	ctx := context.Background()
	if err := svc.DatasetDelete(ctx, "t", "tank/media", false); !errors.Is(err, ErrHostUnknown) {
		t.Fatalf("delete with the host unreadable: %v, want ErrHostUnknown", err)
	}
	if err := svc.VdevAction(ctx, "t", "tank", "sdb", "offline", false); errors.Is(err, ErrHostUnknown) || errors.Is(err, ErrHostStorage) {
		t.Fatalf("offline on a data pool blocked by the listing: %v", err)
	}
	if err := svc.VdevAction(ctx, "t", "rpool", "sda3", "offline", false); !errors.Is(err, ErrHostStorage) {
		t.Fatalf("offline on rpool: %v", err)
	}
}

// N1: after a failed snapshot, only automatic snapshots on guest disks are
// pruned; the admin's own keep theirs.
func TestPruneGuestsOnly(t *testing.T) {
	svc, _ := newTestService(t)
	dir := t.TempDir()
	log := filepath.Join(dir, "destroyed")
	zfs := "#!/bin/sh\nif [ \"$1\" = list ]; then printf 'tank/media@easyzfs-auto-1\\t100\\ntank/vm-100-disk-0@easyzfs-auto-1\\t100\\n'; exit 0; fi\n" +
		"[ \"$1\" = destroy ] && echo \"$2\" >> " + log + "\nexit 0\n"
	if err := os.WriteFile(filepath.Join(dir, "zfs"), []byte(zfs), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	n, err := svc.SnapshotPrune(context.Background(), "scheduler", "tank", time.Now(), true)
	if err != nil || n != 1 {
		t.Fatalf("pruned %d, %v; want 1", n, err)
	}
	if b, _ := os.ReadFile(log); strings.TrimSpace(string(b)) != "tank/vm-100-disk-0@easyzfs-auto-1" {
		t.Fatalf("destroyed %q", b)
	}
}
