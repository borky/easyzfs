// pveconfig_test.go — Proxmox's configs as a source of guest disks (spec P2,
// P6). testdata/pve_qemu_900.conf is a real VM config from the Proxmox VE 8.4
// test VM ('qm create' with an EFI disk, a data disk, a snapshot and an
// unused disk), random ids zeroed.
package actions

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// pveTree builds an /etc/pve with storage.cfg and guest configs per node.
func pveTree(t *testing.T, storageCfg string, guests map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "storage.cfg"), []byte(storageCfg), 0o644); err != nil {
		t.Fatal(err)
	}
	for rel, body := range guests { // "node/qemu-server/100.conf"
		p := filepath.Join(dir, "nodes", rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestGuestVolumeRefsFromARealConfig(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("testdata", "pve_qemu_900.conf"))
	if err != nil {
		t.Fatal(err)
	}
	refs := guestVolumeRefs(map[string]string{"qemu-server/900": string(b)})
	for _, v := range []string{"local-zfs:vm-900-disk-0", "local-zfs:vm-900-disk-1", "local-zfs:vm-900-disk-2"} {
		if got := refs[v]; len(got) != 1 || got[0] != "VM 900" {
			t.Errorf("%s: %v, want [VM 900]", v, got)
		}
	}
	if len(refs) != 3 {
		t.Errorf("refs = %v", refs)
	}
}

// A disk a guest config references is a guest disk whatever its name, on any
// node's guest, and the reason names the guest.
func TestConfigReferencedDiskIsGuest(t *testing.T) {
	usePVEHost(t, true, nil, "tank\tfilesystem\t/tank", "tank/vmstore\tfilesystem\t/tank/vmstore",
		"tank/vmstore/imported-disk\tvolume\t-")
	cfg, _ := os.ReadFile(filepath.Join("testdata", "storage_cfg_pve.txt"))
	dir := pveTree(t, string(cfg)+"\nzfspool: tank-vm\n\tpool tank/vmstore\n\tcontent images\n\tnodes pve2\n",
		map[string]string{
			// on another node: storage.cfg is cluster-wide, so are guests
			"pve2/qemu-server/200.conf": "scsi0: tank-vm:imported-disk,size=8G\n",
			"pve/lxc/101.conf":          "rootfs: local-zfs:subvol-101-disk-0,size=8G\nmp0: tank-vm:ctdata,mp=/data\n",
		})
	saved := readPVEConfig
	readPVEConfig = func(context.Context) (*PVEConfig, error) { return ReadPVEConfig(dir) }
	t.Cleanup(func() { readPVEConfig = saved })
	h := loadView(t)
	kind, why := h.DatasetKind("tank/vmstore/imported-disk")
	if kind != HostGuest || !strings.Contains(why, "VM 200") {
		t.Fatalf("imported-disk = %q (%s), want guest of VM 200", kind, why)
	}
	if k, _ := h.DatasetKind("tank/vmstore"); k != HostStorage {
		t.Errorf("tank/vmstore = %q, want storage (restricted to another node, still Proxmox's)", k)
	}
	if k, _ := h.PoolKind("tank"); k != HostStorage {
		t.Errorf("pool tank = %q, want storage", k)
	}
}

// A guest config that cannot be read, or /etc/pve unreadable, is unknown:
// the host view says so, and destructive actions do not treat it as clear.
func TestUnreadablePVEConfigIsUnknown(t *testing.T) {
	usePVEHost(t, true, nil, "tank\tfilesystem\t/tank", "tank/data\tfilesystem\t/tank/data")
	saved := readPVEConfig
	readPVEConfig = func(context.Context) (*PVEConfig, error) { return nil, errors.New("pmxcfs not responding") }
	t.Cleanup(func() { readPVEConfig = saved })
	h := loadView(t)
	if !h.StorageUnknown {
		t.Fatal("unreadable /etc/pve not reported as unknown")
	}
	if err := h.check(OpDatasetRemove, "", "tank/data"); !errors.Is(err, ErrHostStorage) {
		t.Fatalf("delete with Proxmox config unknown: %v", err)
	}
	if err := h.check(OpPoolRemove, "tank", ""); !errors.Is(err, ErrHostStorage) {
		t.Fatalf("export with Proxmox config unknown: %v", err)
	}
}

func TestReadPVEConfigFailsOnAnUnreadableGuest(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads everything")
	}
	dir := pveTree(t, "dir: local\n\tpath /var/lib/vz\n", map[string]string{"pve/qemu-server/100.conf": "scsi0: local-zfs:vm-100-disk-0\n"})
	if err := os.Chmod(filepath.Join(dir, "nodes", "pve", "qemu-server", "100.conf"), 0); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadPVEConfig(dir); err == nil {
		t.Fatal("a guest config that cannot be read was skipped: its disks would look like data")
	}
	// No nodes directory at all: a fresh install with no guests.
	if c, err := ReadPVEConfig(pveTree(t, "", nil)); err != nil || len(c.Refs) != 0 {
		t.Fatalf("fresh install: %v, %v", c, err)
	}
}

// storage.cfg edge cases (P6): truncated mid-section, shared definitions,
// nodes restrictions, backup-only dir storage.
func TestParseStorageCfgEdgeCases(t *testing.T) {
	zp, dirs := parseStorageCfg("zfspool: a\n\tpool tank/a\n\tnodes pve2\n\tsparse 1\n\ndir: dump\n\tpath /tank/dump\n\tcontent backup\n\tshared 1\n\nzfspool: b\n\tpo")
	if zp["a"] != "tank/a" || dirs["/tank/dump"] != "dump" || len(zp) != 1 {
		t.Fatalf("zfspools %v dirs %v", zp, dirs)
	}
	// Two storages on one dataset (per-node or per-content definitions):
	// a guest may name either.
	zp, _ = parseStorageCfg("zfspool: vm-a\n\tpool tank/vm\n\tnodes pve\n\nzfspool: vm-b\n\tpool tank/vm\n\tnodes pve2\n")
	if zp["vm-a"] != "tank/vm" || zp["vm-b"] != "tank/vm" {
		t.Fatalf("shared dataset: %v", zp)
	}
}

// Guests that use a dataset without going through a storage volume, and
// linked clones: all must still be recognised (review of 7755a22).
func TestGuestRefsByPathAndClone(t *testing.T) {
	refs := guestVolumeRefs(map[string]string{
		"lxc/101":         "rootfs: local-zfs:subvol-101-disk-0,size=8G\nmp0: /tank/media,mp=/media\nlxc.mount.entry: /tank/raw mnt/raw none bind,create=dir 0 0\ndev0: /dev/zvol/tank/lxcdev\n",
		"qemu-server/102": "scsi0: local-zfs:base-100-disk-0/vm-102-disk-0,size=8G\nscsi1: /dev/zvol/tank/rawvol-part1,size=4G\n",
	})
	for ref, who := range map[string]string{
		"/tank/media": "CT 101", "/tank/raw": "CT 101", "/dev/zvol/tank/lxcdev": "CT 101",
		"local-zfs:vm-102-disk-0": "VM 102", "/dev/zvol/tank/rawvol-part1": "VM 102",
	} {
		if got := refs[ref]; len(got) != 1 || got[0] != who {
			t.Errorf("%s: %v, want [%s]", ref, got, who)
		}
	}

	usePVEHost(t, true, nil, "tank\tfilesystem\t/tank", "tank/media\tfilesystem\t/tank/media",
		"tank/media/photos\tfilesystem\t/tank/media/photos", "tank/lxcdev\tvolume\t-",
		"tank/rawvol\tvolume\t-", "tank/raw\tfilesystem\t/tank/raw", "tank/other\tfilesystem\t/tank/other",
		"tank/vm\tfilesystem\t/tank/vm", "tank/vm/vm-disk-a\tvolume\t-")
	saved := readPVEConfig
	readPVEConfig = func(context.Context) (*PVEConfig, error) {
		return &PVEConfig{
			StorageCfg: "zfspool: vm-a\n\tpool tank/vm\n\tnodes pve\n\nzfspool: vm-b\n\tpool tank/vm\n\tnodes pve2\n",
			Refs: map[string][]string{
				"/tank/media": {"CT 101"}, "/tank/raw/sub": {"CT 101"}, "/dev/zvol/tank/lxcdev": {"CT 101"},
				"/dev/zvol/tank/rawvol-part1": {"VM 102"}, "vm-b:vm-disk-a": {"VM 103"},
				"/var/lib/something": {"CT 104"}, // on the OS root: not a dataset of ours
			},
		}, nil
	}
	t.Cleanup(func() { readPVEConfig = saved })
	h := loadView(t)
	for ds, who := range map[string]string{
		"tank/lxcdev": "CT 101", "tank/rawvol": "VM 102", "tank/vm/vm-disk-a": "VM 103",
	} {
		if k, why := h.DatasetKind(ds); k != HostGuest || !strings.Contains(why, who) {
			t.Errorf("%s = %q (%s), want guest of %s", ds, k, why, who)
		}
	}
	// Bind-mounted data is not a guest disk: Proxmox keeps no snapshots of
	// it, so snapshot jobs and rollback keep working; only removing it from
	// under the container is refused.
	for _, ds := range []string{"tank/media", "tank/media/photos", "tank/raw"} {
		k, why := h.DatasetKind(ds)
		if k != HostStorage || !strings.Contains(why, "CT 101") {
			t.Errorf("%s = %q (%s), want storage bound by CT 101", ds, k, why)
		}
		if !allowed(OpSnapshotCreate, k) || !allowed(OpRollback, k) || allowed(OpDatasetRemove, k) {
			t.Errorf("%s: bind-mounted data must keep snapshots and rollback, and refuse removal", ds)
		}
	}
	if k, _ := h.DatasetKind("tank"); k != HostStorage {
		t.Errorf("tank (holds a bind-mounted dataset) = %q, want storage", k)
	}
	if k, _ := h.DatasetKind("tank/other"); k == HostGuest {
		t.Error("tank/other is used by no guest")
	}
}

// A bind mount through a symlink is also recorded under the path it resolves
// to, where a dataset's mountpoint can match it.
func TestReadPVEConfigResolvesBindSymlinks(t *testing.T) {
	real := filepath.Join(t.TempDir(), "data")
	if err := os.MkdirAll(real, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	c, err := ReadPVEConfig(pveTree(t, "", map[string]string{"pve/lxc/105.conf": "mp0: " + link + ",mp=/data\n"}))
	if err != nil {
		t.Fatal(err)
	}
	resolved, _ := filepath.EvalSymlinks(real)
	for _, p := range []string{link, resolved} {
		if got := c.Refs[p]; len(got) != 1 || got[0] != "CT 105" {
			t.Errorf("%s: %v, want [CT 105]", p, got)
		}
	}
}
