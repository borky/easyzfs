// privgate_test.go — the privileged gateway's policy (privgate.go), called as
// the root side calls it: with the literal argv, against a captured Proxmox
// host (hoststorage_test.go's fixtures) and its real 'zfs get mountpoint'
// output (testdata/zfs_get_mountpoint_pve.txt).
package actions

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"easyzfs/internal/executil"
)

// usePrivHost — the captured PVE host, its mountpoints, blank spare disks.
func usePrivHost(t *testing.T) {
	t.Helper()
	usePVEHost(t, true, nil)
	stubMountpoints(t, loadMountpointFixture(t, "zfs_get_mountpoint_pve.txt"))
	stubPoolRoot(t, "")
	stubBlankDisks(t)
}

func privArgv(s string) (string, []string) {
	f := strings.Fields(s)
	return f[0], f[1:]
}

// Outside the grammar is refused, whatever the service asks for: these are
// the shapes that made a zfs/zpool grant root-equivalent, and the ones the
// gateway exists to stop a compromised service account from running.
func TestPrivRefusesShapesOutsideTheGrammar(t *testing.T) {
	usePrivHost(t)
	for _, cmd := range []string{
		"zfs program rpool /tmp/x.lua",
		"zfs allow easyzfs create rpool",
		"zfs set sharenfs=rw rpool/data",
		"zfs create -o mountpoint=/etc rpool/data/x",
		"zfs create -p -o compression=lz4 -o mountpoint=/etc rpool/data/x",
		"zfs destroy -R rpool/data",
		"zfs recv -F rpool/data/x",
		"zfs mount -a",
		"zfs mount -o remount rpool/data",
		"zpool import rpool",
		"zpool import -d /var/lib/easyzfs tank",
		"zpool import -o altroot=/ tank",
		"zpool create -f tank sdb",
		"zpool create tank /var/lib/easyzfs/file",
		"zpool status -c upath rpool",
		"zpool iostat -vc smart",
		"zpool set cachefile=/etc/x rpool",
		"zpool labelclear -f /dev/sda",
		"smartctl -s off /dev/sda",
		"dd if=/dev/zero of=/dev/sdb bs=1M count=2048",
		"hdparm --security-erase x /dev/sdb",
		"bash -c id",
	} {
		tool, args := privArgv(cmd)
		if err := PrivCheck(context.Background(), tool, args); err == nil {
			t.Errorf("%s: allowed", cmd)
		}
	}
}

// The spec's list: no path on it is ever accepted as a mountpoint, whether
// set directly or reached by inheritance, and path boundaries are respected.
func TestPrivRefusesSystemMountpoints(t *testing.T) {
	usePrivHost(t)
	ctx := context.Background()
	for _, p := range []string{"/", "/etc", "/bin", "/sbin", "/lib", "/lib64", "/usr", "/var", "/boot", "/boot/efi",
		"/efi", "/dev", "/proc", "/sys", "/run", "/root", "/opt/easyzfs", "/var/lib/easyzfs", "/etc/pve", "/usr/local"} {
		if err := PrivCheck(ctx, "zfs", []string{"set", "mountpoint=" + p, "rpool/data/media"}); !errors.Is(err, ErrInvalidInput) && !errors.Is(err, ErrNotAllowed) {
			t.Errorf("set mountpoint=%s: %v, want refused", p, err)
		}
	}
	if !deniedMountpoint("/etc/x") || deniedMountpoint("/etc-backup") || deniedMountpoint("/srv/etcetera") {
		t.Error("path boundaries: /etc-backup is not under /etc")
	}
}

func TestPrivMountpointRules(t *testing.T) {
	usePrivHost(t)
	ctx := context.Background()
	for cmd, wantOK := range map[string]bool{
		// Inherited: a dataset under the root filesystem lands on /etc.
		"zfs create -p -o compression=lz4 rpool/ROOT/pve-1/etc": false,
		"zfs create -p -o compression=lz4 rpool/data/media":      true,
		// Recorded at /: mounting the root filesystem dataset again.
		"zfs mount rpool/ROOT/pve-1": false,
		"zfs mount rpool/data":       true,
		// Explicit values: safe, none and legacy.
		"zfs set mountpoint=/srv/media rpool/data/media": true,
		"zfs set mountpoint=none rpool/data/media":       true,
		"zfs set mountpoint=legacy rpool/data/media":     true,
		// Inheriting under the root filesystem lands on a system path.
		"zfs rename rpool/data/media rpool/ROOT/pve-1/media": false,
		// A pool named after a system path mounts there.
		"zpool create etc sdb": false,
		"zpool create tank sdb": true,
	} {
		tool, args := privArgv(cmd)
		err := PrivCheck(ctx, tool, args)
		if (err == nil) != wantOK {
			t.Errorf("%s: err = %v, want ok=%v", cmd, err, wantOK)
		}
	}
}

// A symlink on the way to a mountpoint is refused: root's mount would follow
// it wherever it points.
func TestPrivRefusesSymlinkedMountpoint(t *testing.T) {
	usePrivHost(t)
	base := t.TempDir()
	stubPoolRoot(t, base) // the pool's tree is the temp dir
	if err := os.Symlink("/etc", filepath.Join(base, "link")); err != nil {
		t.Fatal(err)
	}
	err := PrivCheck(context.Background(), "zfs", []string{"set", "mountpoint=" + filepath.Join(base, "link", "x"), "rpool/data/media"})
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("symlinked path: %v, want refused", err)
	}
}

// The host/guest policy holds at the boundary too, not only in the actions.
func TestPrivHostPolicy(t *testing.T) {
	usePrivHost(t)
	ctx := context.Background()
	for cmd, wantOK := range map[string]bool{
		"zpool destroy rpool":                        false,
		"zpool export rpool":                         false,
		"zpool offline rpool sda3":                   false,
		"zpool checkpoint rpool":                     false,
		"zpool scrub rpool":                          true,
		"zpool clear rpool":                          true,
		"zfs destroy -r rpool/ROOT/pve-1":            false,
		"zfs destroy rpool/data":                     false,
		"zfs destroy rpool/data/vm-100-disk-0":       false,
		"zfs destroy rpool/data/media":               true,
		"zfs destroy rpool/data/vm-100-disk-0@snap1": false,
		// EasyZFS's own snapshots on a guest disk are its own to remove.
		"zfs destroy rpool/data/vm-100-disk-0@easyzfs-auto-20260101-0300": true,
		"zfs destroy rpool/data/vm-100-disk-0@ezrepl-20260101-030000":     true,
		"zfs snapshot rpool/data/vm-100-disk-0@x":                         false,
		"zfs snapshot rpool/data/vm-100-disk-0@ezrepl-1":                  true,
		"zfs snapshot -r rpool/data@x":                                    false,
		"zfs snapshot rpool/data@x rpool/data/media@x":                    true,
		"zfs rollback -r rpool/ROOT/pve-1@x":                              false,
		"zfs rename rpool/data rpool/data2":                               false,
		"zfs rename rpool/data/media rpool/data/vm-300-disk-0":            false,
		"zfs unmount rpool/var-lib-vz":                                    false,
		"zfs set exec=off rpool/data":                                     false,
		"zfs set compression=zstd rpool/data":                             true,
		"zfs inherit compression rpool/ROOT/pve-1":                        false,
		"zfs unload-key rpool/data":                                       false,
		"zfs recv -s rpool/data/vm-100-disk-0":                            false,
		"zfs recv -s rpool/data/backup":                                   true,
	} {
		tool, args := privArgv(cmd)
		err := PrivCheck(ctx, tool, args)
		if (err == nil) != wantOK {
			t.Errorf("%s: err = %v, want ok=%v", cmd, err, wantOK)
		}
	}
}

// A host that cannot be read refuses the destructive ones, and says so as
// host_unknown rather than as host storage.
func TestPrivFailsClosed(t *testing.T) {
	usePrivHost(t)
	listAllDatasets = func(context.Context) ([]byte, error) { return nil, errors.New("zfs list timed out") }
	err := PrivCheck(context.Background(), "zfs", []string{"destroy", "tank/media"})
	if !errors.Is(err, ErrHostUnknown) || PrivCode(err) != "host_unknown" {
		t.Fatalf("err = %v (%s), want host_unknown", err, PrivCode(err))
	}
}

// Root mode: executil runs the same checks in-process, and a refusal means
// nothing ran.
func TestPrivGateInProcess(t *testing.T) {
	usePrivHost(t)
	executil.SetSudoForTest(false)
	executil.PrivGate = PrivCheck
	t.Cleanup(func() { executil.PrivGate = nil; executil.SetSudoForTest(true) })
	_, err := executil.Run(context.Background(), 0, "zfs", "destroy", "-r", "rpool/ROOT/pve-1")
	if !errors.Is(err, ErrHostStorage) {
		t.Fatalf("err = %v, want ErrHostStorage", err)
	}
}

// A refusal from the root side comes back as the same domain error.
func TestPrivErrorMapsBack(t *testing.T) {
	pe := &executil.PrivError{Code: "dev_in_use", Message: "sdb es un volumen físico LVM"}
	if !errors.Is(pe, ErrDiskInUse) {
		t.Fatal("dev_in_use does not unwrap to ErrDiskInUse")
	}
	for _, e := range []error{ErrHostStorage, ErrHostUnknown, ErrDiskInUse, ErrNotAllowed} {
		if !errors.Is(&executil.PrivError{Code: PrivCode(e)}, e) {
			t.Errorf("%v does not round-trip", e)
		}
	}
}
