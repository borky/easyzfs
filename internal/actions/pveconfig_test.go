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
	if c, err := ReadPVEConfig(pveTree(t, "", nil)); err != nil || len(c.Guests) != 0 {
		t.Fatalf("fresh install: %v, %v", c, err)
	}
}

// storage.cfg edge cases (P6): truncated mid-section, shared definitions,
// nodes restrictions, backup-only dir storage.
func TestParseStorageCfgEdgeCases(t *testing.T) {
	zp, dirs := parseStorageCfg("zfspool: a\n\tpool tank/a\n\tnodes pve2\n\tsparse 1\n\ndir: dump\n\tpath /tank/dump\n\tcontent backup\n\tshared 1\n\nzfspool: b\n\tpo")
	if zp["tank/a"] != "a" || dirs["/tank/dump"] != "dump" || len(zp) != 1 {
		t.Fatalf("zfspools %v dirs %v", zp, dirs)
	}
}
