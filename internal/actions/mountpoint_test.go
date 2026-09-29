// mountpoint_test.go — the effective mountpoint check (§1), driven by the
// real 'zfs get' output in testdata: the Proxmox VE layout the /etc incident
// came from, and one dataset of each of the four sources zfs reports.
package actions

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// loadMountpointFixture — the mountpoint value and source of every dataset in
// one of the testdata files, read as zfs printed it.
func loadMountpointFixture(t *testing.T, name string) map[string][2]string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	out := map[string][2]string{}
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(line, "#") {
			continue
		}
		f := strings.Split(line, "\t")
		if len(f) == 4 && f[1] == "mountpoint" {
			out[f[0]] = [2]string{f[2], f[3]}
		}
	}
	if len(out) == 0 {
		t.Fatalf("%s: no mountpoint rows", name)
	}
	return out
}

// stubMountpoints makes the live read answer from a table. A dataset that is
// not in it does not exist — reported as the real reader reports it, wrapping
// ErrNoSuchDataset, since telling that apart from a failed read is what lets
// the resolver walk up to a parent at all.
func stubMountpoints(t *testing.T, props map[string][2]string) {
	t.Helper()
	saved := readMountpointProp
	readMountpointProp = func(_ context.Context, ds string) (string, string, error) {
		if p, ok := props[ds]; ok {
			return p[0], p[1], nil
		}
		return "", "", fmt.Errorf("%w: cannot open '%s': dataset does not exist", ErrNoSuchDataset, ds)
	}
	t.Cleanup(func() { readMountpointProp = saved })
}

// stubDefaultMountpoints — every dataset sits at /<its own name>, source
// "default", which is what a pool that nobody has reconfigured looks like
// (rpool, rpool/ROOT and rpool/data in the PVE fixture). newTestService
// installs it so the action tests exercise the ordinary case.
func stubDefaultMountpoints(t *testing.T) {
	t.Helper()
	saved := readMountpointProp
	readMountpointProp = func(_ context.Context, ds string) (string, string, error) {
		return "/" + ds, "default", nil
	}
	t.Cleanup(func() { readMountpointProp = saved })
}

// newMountTestService — the service with a fake zfs that accepts anything and
// logs its argv, so the mutations under test get as far as running. Call
// stubMountpoints *after* this: newTestService installs its own default.
func newMountTestService(t *testing.T) (*Service, string) {
	t.Helper()
	svc, _ := newTestService(t)
	dir := t.TempDir()
	logFile := filepath.Join(dir, "zfs-args.log")
	zfs := "#!/bin/sh\necho \"$@\" >> " + logFile + "\nexit 0\n"
	if err := os.WriteFile(filepath.Join(dir, "zfs"), []byte(zfs), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return svc, logFile
}

// The four sources zfs reports, resolved. A dataset that does not exist yet
// resolves to the nearest existing ancestor's path plus its relative name —
// what inheritance will give it.
func TestResolveEffectiveMountpoint(t *testing.T) {
	ctx := context.Background()
	props := loadMountpointFixture(t, "zfs_get_mountpoint_sources.txt")
	for k, v := range loadMountpointFixture(t, "zfs_get_mountpoint_pve.txt") {
		props[k] = v
	}
	stubMountpoints(t, props)
	stubPoolRoot(t, "/")

	for _, c := range []struct {
		name     string
		dataset  string
		path     string
		recorded bool
		grant    string
	}{
		{"local", "rpool/ROOT/pve-1", "/", true, ""},
		{"local elsewhere", "rpool/var-lib-vz", "/var/lib/vz", true, ""},
		{"default from the pool name", "rpool/data", "/rpool/data", false, ""},
		{"inherited", "rpool/data/ezsrc/child", "/rpool/data/ezmp/child", false, "/rpool/data/ezmp"},
		{"received", "rpool/data/ezdst", "/rpool/data/ezmp", true, ""},
		{"a volume mounts nothing", "rpool/data/ezvol", "", false, ""},
		// Datasets that do not exist yet: create, clone, rename target.
		{"new under a default parent", "rpool/data/new", "/rpool/data/new", false, ""},
		{"new under a local parent", "rpool/var-lib-vz/new", "/var/lib/vz/new", false, "/var/lib/vz"},
		{"new below the root dataset", "rpool/ROOT/pve-1/etc", "/etc", false, "/"},
		{"new several levels down", "rpool/data/a/b/c", "/rpool/data/a/b/c", false, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, err := newMountResolver().target(ctx, c.dataset)
			if err != nil {
				t.Fatalf("target(%s): %v", c.dataset, err)
			}
			if got.path != c.path || got.recorded != c.recorded || got.grant != c.grant {
				t.Fatalf("target(%s) = %+v, want path=%q recorded=%v grant=%q",
					c.dataset, got, c.path, c.recorded, c.grant)
			}
		})
	}
}

// The Proxmox layout, end to end: what the rules let through and what they
// refuse. The names come straight from the VM fixture.
func TestEffectiveMountpointOnProxmoxLayout(t *testing.T) {
	ctx := context.Background()
	stubMountpoints(t, loadMountpointFixture(t, "zfs_get_mountpoint_pve.txt"))
	// rpool's root dataset really is at /rpool; "/" is the worst case here,
	// and poolTrees has to ignore it.
	stubPoolRoot(t, "/")

	for _, c := range []struct {
		name    string
		dataset string
		refused string // a substring of the reason; "" = allowed
	}{
		// The incident. /etc is a system path whatever put a dataset there.
		{"a dataset inheriting /etc", "rpool/ROOT/pve-1/etc", "system path"},
		{"deeper under /etc", "rpool/ROOT/pve-1/etc/network", "system path"},
		// Not a system path, but still a new mount on the host's root, which
		// no allowed tree covers: an ancestor at / grants nothing.
		{"a dataset inheriting /srv-like junk", "rpool/ROOT/pve-1/stuff", "outside the allowed paths"},
		// The pool's own tree is where datasets belong.
		{"under rpool/data", "rpool/data/vm-100-disk-0", ""},
		{"under the pool root", "rpool/backups", ""},
		// A path recorded on the dataset itself: PVE put it there, and
		// refusing to mount it would not move it.
		{"the PVE storage dataset", "rpool/var-lib-vz", ""},
		{"a child of it", "rpool/var-lib-vz/dump", ""},
		// The root filesystem itself is never something to mount.
		{"the root dataset", "rpool/ROOT/pve-1", "system path"},
	} {
		t.Run(c.name, func(t *testing.T) {
			err := checkEffectiveMountpoint(ctx, c.dataset)
			if c.refused == "" {
				if err != nil {
					t.Fatalf("%s should be allowed: %v", c.dataset, err)
				}
				return
			}
			if !errors.Is(err, ErrInvalidInput) || !strings.Contains(err.Error(), c.refused) {
				t.Fatalf("%s: err = %v, want ErrInvalidInput mentioning %q", c.dataset, err, c.refused)
			}
		})
	}
}

// A received mountpoint is recorded on the dataset, so it is not held to the
// allowlist — but a received system path is still refused. This is the
// replication case: the stream decides, and the stream is not ours.
func TestEffectiveMountpointReceived(t *testing.T) {
	ctx := context.Background()
	stubPoolRoot(t, "")
	stubMountpoints(t, map[string][2]string{
		"bak":            {"/bak", "default"},
		"bak/ok":         {"/var/lib/vz-replica", "received"},
		"bak/bad":        {"/etc", "received"},
		"bak/ok/child":   {"/var/lib/vz-replica/child", "inherited from bak/ok"},
		"bak/bad/child":  {"/etc/child", "inherited from bak/bad"},
		"bak/outsideok":  {"/data/replica", "received"},
		"bak/outsidekid": {"/data/replica/child", "inherited from bak/outsideok"},
	})
	for ds, wantErr := range map[string]bool{
		"bak/ok":         false,
		"bak/bad":        true,
		"bak/ok/child":   false, // derived, but inside the tree bak/ok records
		"bak/bad/child":  true,
		"bak/outsideok":  false, // recorded outside the allowlist: not ours to undo
		"bak/outsidekid": false, // derived from it, so the same tree
	} {
		if err := checkEffectiveMountpoint(ctx, ds); (err != nil) != wantErr {
			t.Errorf("%s: err = %v, want error = %v", ds, err, wantErr)
		}
	}
}

// A pool may be called "etc" or "home": its root dataset mounts at /<pool>
// the moment the pool exists, before anything can set a property. Driven
// through PoolCreate, so the wiring is covered too — the disks are stubbed
// blank and the fake zpool logs whatever it is asked to run.
func TestPoolCreateRefusesASystemName(t *testing.T) {
	ctx := context.Background()
	svc, logFile := newTestService(t)
	for _, name := range []string{"etc", "usr", "boot", "root", "home", "srv", "mnt", "media", "var"} {
		err := svc.PoolCreate(ctx, "admin", name, "stripe", []string{"sdz"}, 0, true)
		if !errors.Is(err, ErrInvalidInput) {
			t.Errorf("a pool named %q would mount at /%s: err = %v, want ErrInvalidInput", name, name, err)
		}
	}
	if b, _ := os.ReadFile(logFile); strings.Contains(string(b), "create") {
		t.Fatalf("zpool create ran for a refused name:\n%s", b)
	}
	for _, name := range []string{"tank", "rpool", "bigtank", "ssd"} {
		if err := svc.PoolCreate(ctx, "admin", name, "stripe", []string{"sdz"}, 0, true); err != nil {
			t.Errorf("a pool named %q: %v", name, err)
		}
	}
}

// The mountpoint a dataset already has recorded is not held to the allowlist,
// and that is what keeps the standard root-on-ZFS /home dataset mountable
// while a *pool* named "home" still cannot land there.
func TestHomeIsDeniedOnlyWhereTheAppPicksIt(t *testing.T) {
	ctx := context.Background()
	stubPoolRoot(t, "")
	stubMountpoints(t, map[string][2]string{
		"rpool":      {"/rpool", "default"},
		"rpool/home": {"/home", "local"},   // the OpenZFS root-on-ZFS layout
		"home":       {"/home", "default"}, // a pool actually called "home"
	})
	if err := checkEffectiveMountpoint(ctx, "rpool/home"); err != nil {
		t.Errorf("a dataset recorded at /home must stay mountable: %v", err)
	}
	if err := checkEffectiveMountpoint(ctx, "home"); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("a pool named home derives /home: err = %v, want ErrInvalidInput", err)
	}
}

// A share directory chowned to its users is the ordinary Samba/NFS setup.
// Demanding root ownership all the way would refuse every dataset created
// under one, so that demand applies only where the app picks the place.
func TestRecordedMountpointBelowAChownedDirectory(t *testing.T) {
	ctx := context.Background()
	saved := mountTrustedUID
	mountTrustedUID = os.Getuid() + 1 // nothing here is owned by the trusted uid
	t.Cleanup(func() { mountTrustedUID = saved })

	base := t.TempDir()
	share := filepath.Join(base, "media")
	if err := os.MkdirAll(share, 0o775); err != nil { // group-writable, as a share is
		t.Fatal(err)
	}
	stubPoolRoot(t, base)
	stubMountpoints(t, map[string][2]string{
		"tank":            {base, "local"},
		"tank/media":      {share, "local"},
		"tank/media/kids": {filepath.Join(share, "kids"), "local"},
	})
	if err := checkEffectiveMountpoint(ctx, "tank/media/kids"); err != nil {
		t.Fatalf("a recorded mountpoint below a chowned share: %v", err)
	}
	// And the case that matters in practice: a dataset created under the share,
	// whose mountpoint is *derived*. This is what POST /api/datasets does.
	if err := checkEffectiveMountpoint(ctx, "tank/media/new"); err != nil {
		t.Fatalf("a dataset created below a chowned share: %v", err)
	}
	if err := checkEffectiveMountpointTree(ctx, "tank/media/new/deeper"); err != nil {
		t.Fatalf("zfs create -p below a chowned share: %v", err)
	}
	// A symlink on the way is still refused, recorded or not: that is the
	// attack the walk is for, and it needs no race to win.
	link := filepath.Join(share, "link")
	if err := os.Symlink("/etc", link); err != nil {
		t.Fatal(err)
	}
	stubMountpoints(t, map[string][2]string{
		"tank":           {base, "local"},
		"tank/media":     {share, "local"},
		"tank/media/bad": {filepath.Join(link, "sudoers.d"), "local"},
	})
	if err := checkEffectiveMountpoint(ctx, "tank/media/bad"); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("a symlink component: err = %v, want ErrInvalidInput", err)
	}
}

// Inheriting the mountpoint hands the dataset its parent's path. Under the
// PVE root dataset that is a system path, and acknowledge_risk does not buy
// its way past that.
func TestPropInheritMountpointChecksWhereItLands(t *testing.T) {
	ctx := context.Background()
	svc, _ := newMountTestService(t)
	props := loadMountpointFixture(t, "zfs_get_mountpoint_pve.txt")
	props["rpool/ROOT/pve-1/etc"] = [2]string{"/srv/safe", "local"}
	props["rpool/data/ok"] = [2]string{"/srv/safe", "local"}
	stubMountpoints(t, props)
	stubPoolRoot(t, "")

	err := svc.DatasetPropInherit(ctx, "admin", "rpool/ROOT/pve-1/etc", "mountpoint", true)
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("inherit into /etc: err = %v, want ErrInvalidInput", err)
	}
	if err := svc.DatasetPropInherit(ctx, "admin", "rpool/data/ok", "mountpoint", true); err != nil {
		t.Fatalf("inherit inside the pool's tree: %v", err)
	}
	// Without the acknowledgement the risk gate still comes first.
	if err := svc.DatasetPropInherit(ctx, "admin", "rpool/data/ok", "mountpoint", false); !errors.Is(err, ErrRiskAck) {
		t.Fatalf("inherit without acknowledge_risk: err = %v, want ErrRiskAck", err)
	}
}

// A rename carries a recorded mountpoint with it and re-derives an inherited
// one from the new parent; zfs remounts either way.
func TestDatasetRenameChecksTheNewPlace(t *testing.T) {
	ctx := context.Background()
	svc, _ := newMountTestService(t)
	props := loadMountpointFixture(t, "zfs_get_mountpoint_pve.txt")
	props["rpool/data/moving"] = [2]string{"/rpool/data/moving", "default"}
	props["rpool/data/pinned"] = [2]string{"/srv/pinned", "local"}
	props["rpool/data/onetc"] = [2]string{"/etc", "local"}
	stubMountpoints(t, props)
	stubPoolRoot(t, "")

	// Inherited: the new parent decides, and rpool/ROOT/pve-1 is at /.
	if err := svc.DatasetRename(ctx, "admin", "rpool/data/moving", "rpool/ROOT/pve-1/etc"); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("rename under the root dataset: err = %v, want ErrInvalidInput", err)
	}
	if err := svc.DatasetRename(ctx, "admin", "rpool/data/moving", "rpool/data/moved"); err != nil {
		t.Fatalf("rename inside the pool's tree: %v", err)
	}
	// Recorded: unchanged by the rename, so a new parent outside the
	// allowlist is not a reason to refuse.
	if err := svc.DatasetRename(ctx, "admin", "rpool/data/pinned", "rpool/var-lib-vz/pinned"); err != nil {
		t.Fatalf("rename of a dataset with its own mountpoint: %v", err)
	}
	// …but a recorded system path is refused wherever it is renamed to.
	if err := svc.DatasetRename(ctx, "admin", "rpool/data/onetc", "rpool/data/onetc2"); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("rename of a dataset mounted on /etc: err = %v, want ErrInvalidInput", err)
	}
}

// The one string match in the resolver: zfs's "does not exist" is what makes
// the walk up to a parent happen, and nothing else may. The messages are the
// shapes executil produces — stderr appended for an exit error, its own
// wording for a timeout.
func TestClassifyReadErr(t *testing.T) {
	for _, c := range []struct {
		msg    string
		absent bool
	}{
		{"zfs: cannot open 'tank/x': dataset does not exist", true},
		{"zfs: cannot open 'rpool/data/new': dataset does not exist", true},
		{"zfs: timeout running the command after 10s", false},
		// Our own messages say it too, now that they are English: never
		// zfs's answer (the purge would drop an entry never destroyed).
		{"the dataset does not exist", false},
		{"not allowed: privileged operation not allowed: the dataset does not exist", false},
		{"zfs: sudo: a password is required", false},
		{"zfs: Sorry, user easyzfs is not allowed to execute '/usr/sbin/zfs get' as root", false},
		{"zfs: cannot open 'tank': pool I/O is currently suspended", false},
		{"zfs: permission denied", false},
	} {
		got := classifyReadErr(errors.New(c.msg))
		if errors.Is(got, ErrNoSuchDataset) != c.absent {
			t.Errorf("%q: absent = %v, want %v", c.msg, !c.absent, c.absent)
		}
		if got == nil {
			t.Errorf("%q: the error must survive classification", c.msg)
		}
	}
	if classifyReadErr(nil) != nil {
		t.Error("nil stays nil")
	}
}

// A read that *fails* is not a dataset that is absent. Treating the two alike
// would make the resolver substitute the path a dataset would have inherited
// for the one it has recorded — and hand a mount over /etc a pass on the back
// of one timeout.
func TestEffectiveMountpointDistinguishesFailureFromAbsence(t *testing.T) {
	ctx := context.Background()
	stubPoolRoot(t, "")
	saved := readMountpointProp
	t.Cleanup(func() { readMountpointProp = saved })
	// tank/x really is mounted on /etc, but the read for it fails; its parent
	// answers normally.
	readMountpointProp = func(_ context.Context, ds string) (string, string, error) {
		switch ds {
		case "tank":
			return "/tank", "default", nil
		case "tank/x":
			return "", "", errors.New("zfs: cannot open '/dev/zfs': tras 10s")
		}
		return "", "", fmt.Errorf("%w: cannot open '%s': dataset does not exist", ErrNoSuchDataset, ds)
	}
	if err := checkEffectiveMountpoint(ctx, "tank/x"); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("a failed read must not fall back to the inherited path: err = %v", err)
	}
	// The same name, now genuinely absent: the walk up to the parent is what
	// resolves a dataset about to be created, so it must still work.
	readMountpointProp = func(_ context.Context, ds string) (string, string, error) {
		if ds == "tank" {
			return "/tank", "default", nil
		}
		return "", "", fmt.Errorf("%w: cannot open '%s': dataset does not exist", ErrNoSuchDataset, ds)
	}
	if err := checkEffectiveMountpoint(ctx, "tank/x"); err != nil {
		t.Fatalf("a dataset that does not exist yet resolves from its parent: %v", err)
	}
}

// When the mountpoint cannot be read at all, nothing is allowed through:
// where a dataset would land is exactly the thing this check is about, and an
// unknown is not a pass.
func TestEffectiveMountpointFailsClosed(t *testing.T) {
	ctx := context.Background()
	stubMountpoints(t, map[string][2]string{}) // nothing exists, not even the pool
	if err := checkEffectiveMountpoint(ctx, "tank/ds"); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("err = %v, want ErrInvalidInput", err)
	}
	// A value that is neither a path nor none/legacy is not something to
	// reason about either.
	stubMountpoints(t, map[string][2]string{"tank": {"garbage", "local"}})
	if err := checkEffectiveMountpoint(ctx, "tank"); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("unreadable value: err = %v, want ErrInvalidInput", err)
	}
}

// none and legacy mount nothing, so there is nothing to refuse — including
// for the children that inherit them.
func TestEffectiveMountpointNoneAndLegacy(t *testing.T) {
	ctx := context.Background()
	for _, v := range []string{"none", "legacy", "-"} {
		stubMountpoints(t, map[string][2]string{"tank/x": {v, "local"}})
		if err := checkEffectiveMountpoint(ctx, "tank/x"); err != nil {
			t.Errorf("mountpoint %q: %v", v, err)
		}
		if err := checkEffectiveMountpoint(ctx, "tank/x/child"); err != nil {
			t.Errorf("child of mountpoint %q: %v", v, err)
		}
	}
}

// The walk over an effective path looks for symlinks, not for owners. A
// world-writable directory on the way is accepted here and refused for a
// mountpoint somebody asks for (TestMountpointPathTrust): the difference is
// deliberate — a share directory is routinely group-writable, and every
// dataset under one had no check at all before this one existed.
func TestEffectiveMountpointWalksThePath(t *testing.T) {
	ctx := context.Background()
	stubPoolRoot(t, "")
	base := t.TempDir()
	open := filepath.Join(base, "open")
	if err := os.MkdirAll(open, 0o777); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink("/etc", link); err != nil {
		t.Fatal(err)
	}
	stubMountpoints(t, map[string][2]string{
		"tank":       {base, "local"},
		"tank/open":  {open, "local"},
		"tank/free":  {"/ezmount-nonexistent", "local"},
		"tank/link":  {link, "local"},
		"tank/under": {filepath.Join(link, "sudoers.d"), "local"},
	})
	for _, ds := range []string{"tank/free", "tank/open", "tank/open/child"} {
		if err := checkEffectiveMountpoint(ctx, ds); err != nil {
			t.Errorf("%s should be allowed: %v", ds, err)
		}
	}
	for _, ds := range []string{"tank/link", "tank/under"} {
		if err := checkEffectiveMountpoint(ctx, ds); !errors.Is(err, ErrInvalidInput) {
			t.Errorf("%s goes through a symlink: err = %v, want ErrInvalidInput", ds, err)
		}
	}
	// The explicit-mountpoint check keeps the stricter rule it has always had.
	// mountTrustedUID stays at its default, 0: "/" is root-owned and 0755
	// everywhere, /tmp is world-writable everywhere.
	stubPoolRoot(t, "/srv/tank")
	if err := checkMountpoint(ctx, "/srv/tank/x", "tank/x"); err != nil {
		t.Errorf("an explicit mountpoint below a root-owned /srv: %v", err)
	}
	if err := checkMountpoint(ctx, "/tmp/ezmount/child", "tank/x"); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("an explicit mountpoint below world-writable /tmp: err = %v, want ErrInvalidInput", err)
	}
}
