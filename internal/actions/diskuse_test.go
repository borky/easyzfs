// diskuse_test.go — the live disk classifier, fed lsblk output captured on a
// Proxmox VE 8.4 VM (root on ZFS): the root disk, an LVM physical volume, a
// disk with an EFI system partition, and a blank disk.
package actions

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// useFixture makes DiskUse read testdata/lsblk_pve_<name>.json.
func useFixture(t *testing.T, name string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "lsblk_pve_"+name+".json"))
	if err != nil {
		t.Fatal(err)
	}
	savedL, savedS, savedB := lsblkJSON, sysBlockDir, devByIDDir
	lsblkJSON = func(context.Context, ...string) ([]byte, error) { return data, nil }
	sysBlockDir, devByIDDir = t.TempDir(), t.TempDir()
	t.Cleanup(func() { lsblkJSON, sysBlockDir, devByIDDir = savedL, savedS, savedB })
}

func TestDiskUseOnRealProxmoxOutput(t *testing.T) {
	for _, c := range []struct{ fixture, dev, want string }{
		// No partition of a ZFS-root disk shows a mountpoint (ZFS mounts
		// datasets, not devices): it must be caught by its boot partitions.
		{"sda", "sda", "BIOS boot"},
		{"sdb", "sdb", "LVM"},
		{"sdc", "sdc", "EFI"},
		{"blank", "sdc", ""},
	} {
		useFixture(t, c.fixture)
		got, err := DiskUse(context.Background(), c.dev)
		if err != nil {
			t.Fatalf("%s: %v", c.fixture, err)
		}
		if (c.want == "") != (got == "") || !strings.Contains(got, c.want) {
			t.Errorf("%s: reason %q, want one mentioning %q", c.fixture, got, c.want)
		}
	}
}

func TestDiskUseClassifiesSignatures(t *testing.T) {
	s := func(v string) *string { return &v }
	for _, c := range []struct {
		node lsblkNode
		want string
	}{
		{lsblkNode{Name: "sdx", FSType: s("zfs_member")}, "ZFS"},
		{lsblkNode{Name: "sdx", FSType: s("crypto_LUKS")}, "LUKS"},
		{lsblkNode{Name: "sdx", FSType: s("ceph_bluestore")}, "Ceph"},
		{lsblkNode{Name: "sdx", FSType: s("linux_raid_member")}, "RAID"},
		{lsblkNode{Name: "sdx", FSType: s("ext4")}, "ext4"},
		{lsblkNode{Name: "sdx1", Mountpoints: []*string{s("[SWAP]")}}, "swap"},
		{lsblkNode{Name: "sdx", Children: []lsblkNode{{Name: "sdx1", Mountpoints: []*string{s("/srv/x")}}}}, "/srv/x"},
		{lsblkNode{Name: "sdx", Children: []lsblkNode{{Name: "vg-lv", Type: "lvm"}}}, "lvm"},
		{lsblkNode{Name: "sdx", Mountpoints: []*string{nil}}, ""},
	} {
		if got := diskUseReason(c.node); (c.want == "") != (got == "") || !strings.Contains(got, c.want) {
			t.Errorf("%+v: %q, want one mentioning %q", c.node, got, c.want)
		}
	}
}

// The actions refuse a busy disk before running anything.
func TestPoolCreateRefusesBusyDisk(t *testing.T) {
	svc, logFile := newTestService(t)
	useFixture(t, "sdb") // LVM physical volume
	err := svc.PoolCreate(context.Background(), "tester", "tank", "stripe", []string{"sdb"}, 0, true)
	if !errors.Is(err, ErrDiskInUse) {
		t.Fatalf("err = %v, want ErrDiskInUse", err)
	}
	if out, _ := os.ReadFile(logFile); len(out) != 0 {
		t.Fatalf("zpool ran for a busy disk: %s", out)
	}
}

func TestReplaceRefusesBusyNewDiskButAllowsSameDisk(t *testing.T) {
	svc, _ := newTestService(t)
	useFixture(t, "sdc") // ESP
	if err := svc.Replace(context.Background(), "tester", "tank", "sdb", "sdc", true); !errors.Is(err, ErrDiskInUse) {
		t.Fatalf("replace onto a disk with an ESP: err = %v, want ErrDiskInUse", err)
	}
	if err := svc.Replace(context.Background(), "tester", "tank", "sdc", "sdc", true); errors.Is(err, ErrDiskInUse) {
		t.Fatalf("replacing a disk with itself was refused: %v", err)
	}
}
