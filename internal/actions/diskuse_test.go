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
	// Same name on both sides is no longer a free pass: after a reboot the
	// letter can belong to another disk.
	if err := svc.Replace(context.Background(), "tester", "tank", "sdc", "sdc", true); !errors.Is(err, ErrDiskInUse) {
		t.Fatalf("same name, disk with an ESP: err = %v, want ErrDiskInUse", err)
	}
	withLabel := func(pool string) {
		lsblkJSON = func(context.Context, ...string) ([]byte, error) {
			return []byte(`{"blockdevices":[{"name":"sdc","type":"disk","fstype":null,"label":null,"parttype":null,"mountpoints":[null],
				"children":[{"name":"sdc1","type":"part","fstype":"zfs_member","label":"` + pool + `","parttype":null,"mountpoints":[null]}]}]}`), nil
		}
	}
	withLabel("tank") // this pool's old disk, reseated
	if err := svc.Replace(context.Background(), "tester", "tank", "sdc", "sdc", true); errors.Is(err, ErrDiskInUse) {
		t.Fatalf("replacing a disk with itself was refused: %v", err)
	}
	withLabel("bigtank") // another pool's disk under the same letter
	if err := svc.Replace(context.Background(), "tester", "tank", "sdc", "sdc", true); !errors.Is(err, ErrDiskInUse) {
		t.Fatalf("same name, another pool's disk: err = %v, want ErrDiskInUse", err)
	}
	if err := svc.Replace(context.Background(), "tester", "tank", "sdb", "sdc", true); !errors.Is(err, ErrDiskInUse) {
		t.Fatalf("another disk carrying a label: err = %v, want ErrDiskInUse", err)
	}
}

// Power-off refuses only what is in use now: the disks of an exported pool
// and unmounted filesystems are what someone powers off to pull.
func TestPowerOffRules(t *testing.T) {
	s := func(v string) *string { return &v }
	imported := map[string]bool{"tank": true}
	for _, c := range []struct {
		name string
		node lsblkNode
		want string // "" = may power off
	}{
		{"exported pool", lsblkNode{Name: "sdx", Children: []lsblkNode{{Name: "sdx1", FSType: s("zfs_member"), Label: s("bigtank")}}}, ""},
		{"imported pool", lsblkNode{Name: "sdx", Children: []lsblkNode{{Name: "sdx1", FSType: s("zfs_member"), Label: s("tank")}}}, "tank"},
		{"unlabelled zfs", lsblkNode{Name: "sdx", FSType: s("zfs_member")}, "ZFS"},
		{"unmounted ext4", lsblkNode{Name: "sdx", Children: []lsblkNode{{Name: "sdx1", FSType: s("ext4")}}}, ""},
		{"mounted ext4", lsblkNode{Name: "sdx", Children: []lsblkNode{{Name: "sdx1", FSType: s("ext4"), Mountpoints: []*string{s("/srv/x")}}}}, "/srv/x"},
		{"old lsblk mount", lsblkNode{Name: "sdx", Mountpoint: s("/srv/y")}, "/srv/y"},
		{"LVM", lsblkNode{Name: "sdx", FSType: s("LVM2_member")}, "LVM"},
		{"ESP", lsblkNode{Name: "sdx", Children: []lsblkNode{{Name: "sdx1", PartType: s(partTypeESP)}}}, "EFI"},
		{"swap", lsblkNode{Name: "sdx", Mountpoints: []*string{s("[SWAP]")}}, "swap"},
	} {
		got := classify(c.node, imported, "")
		if (c.want == "") != (got == "") || !strings.Contains(got, c.want) {
			t.Errorf("%s: %q, want one mentioning %q", c.name, got, c.want)
		}
		// The takeover rules keep refusing all of them but a blank disk.
		if diskUseReason(c.node) == "" {
			t.Errorf("%s: takeover rules found nothing", c.name)
		}
	}
}

// util-linux < 2.37 rejects the MOUNTPOINTS column; the check retries with
// MOUNTPOINT instead of refusing every disk.
func TestDiskUseFallsBackToMountpointColumn(t *testing.T) {
	savedL, savedS, savedB, savedP := lsblkJSON, sysBlockDir, devByIDDir, zpoolListVHP
	t.Cleanup(func() { lsblkJSON, sysBlockDir, devByIDDir, zpoolListVHP = savedL, savedS, savedB, savedP })
	sysBlockDir, devByIDDir = t.TempDir(), t.TempDir()
	lsblkJSON = func(_ context.Context, args ...string) ([]byte, error) {
		if strings.Contains(strings.Join(args, " "), "MOUNTPOINTS") {
			return nil, errors.New("lsblk: unknown column: MOUNTPOINTS")
		}
		return []byte(`{"blockdevices":[{"name":"sdx","type":"disk","fstype":null,"label":null,"parttype":null,"mountpoint":null,
			"children":[{"name":"sdx1","type":"part","fstype":"ext4","label":null,"parttype":null,"mountpoint":"/srv/data"}]}]}`), nil
	}
	zpoolListVHP = func(context.Context) ([]byte, error) { return nil, nil }
	for _, f := range []func(context.Context, string) (string, error){DiskUse, DiskActiveUse} {
		got, err := f(context.Background(), "sdx")
		if err != nil || !strings.Contains(got, "/srv/data") {
			t.Errorf("got %q, %v; want the mount found through MOUNTPOINT", got, err)
		}
	}
}

// Membership comes from ZFS, not from udev's labels: a disk whose partition
// is a vdev of an imported pool is refused even when lsblk shows no ZFS
// signature on it. Output format as captured on Proxmox VE 8.4.
func TestPowerOffAsksZFSForMembers(t *testing.T) {
	savedL, savedS, savedB, savedP, savedC := lsblkJSON, sysBlockDir, devByIDDir, zpoolListVHP, sysClassBlock
	t.Cleanup(func() {
		lsblkJSON, sysBlockDir, devByIDDir, zpoolListVHP, sysClassBlock = savedL, savedS, savedB, savedP, savedC
	})
	sysBlockDir, devByIDDir = t.TempDir(), t.TempDir()
	// sysfs: sdb1 is a partition of sdb.
	sysClassBlock = t.TempDir()
	devs := t.TempDir()
	must := func(err error) {
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.MkdirAll(filepath.Join(devs, "block", "sdb", "sdb1"), 0o755))
	must(os.WriteFile(filepath.Join(devs, "block", "sdb", "sdb1", "partition"), []byte("1\n"), 0o644))
	must(os.Symlink(filepath.Join(devs, "block", "sdb", "sdb1"), filepath.Join(sysClassBlock, "sdb1")))
	must(os.WriteFile(filepath.Join(devByIDDir, "ata-X-part1"), nil, 0o644))
	vdev := filepath.Join(t.TempDir(), "sdb1")
	must(os.WriteFile(vdev, nil, 0o644))
	must(os.Symlink(vdev, filepath.Join(devByIDDir, "link-part1")))
	zpoolListVHP = func(context.Context) ([]byte, error) {
		return []byte("tank\t31G\t1.59G\t29.4G\t-\t-\t0%\t5%\t1.00x\tONLINE\t-\n" +
			"\t" + filepath.Join(devByIDDir, "link-part1") + "\t31.5G\t1.59G\t29.4G\t-\t-\t0%\t5.12%\t-\tONLINE\n"), nil
	}
	lsblkJSON = func(context.Context, ...string) ([]byte, error) {
		// udev never re-probed: no signature at all
		return []byte(`{"blockdevices":[{"name":"sdb","type":"disk","fstype":null,"label":null,"parttype":null,"mountpoints":[null],
			"children":[{"name":"sdb1","type":"part","fstype":null,"label":null,"parttype":null,"mountpoints":[null]}]}]}`), nil
	}
	got, err := DiskActiveUse(context.Background(), "sdb")
	if err != nil || !strings.Contains(got, "tank") {
		t.Fatalf("got %q, %v; want sdb refused as a member of tank", got, err)
	}
}
