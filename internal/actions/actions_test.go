// actions_test.go — acciones sobre pools con binarios falsos en PATH
// (fake zpool registra sus argumentos; fake sudo los pasa tal cual).
package actions

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"easyzfs/internal/db"
)

// newTestService crea el servicio con SQLite temporal (audit_log real) y un
// directorio de binarios falsos al frente del PATH. Devuelve el servicio y la
// ruta del log donde el fake zpool anota cada invocación.
func newTestService(t *testing.T) (*Service, string) {
	t.Helper()
	dir := t.TempDir()
	logFile := filepath.Join(dir, "zpool-args.log")

	// zpool falso: anota los args y sale 0.
	// FAKE_CHECKPOINT: what 'zpool get … checkpoint' prints (VdevAdd).
	zpool := "#!/bin/sh\necho \"$@\" >> " + logFile + "\n[ \"$1\" = get ] && [ -n \"$FAKE_CHECKPOINT\" ] && echo \"$FAKE_CHECKPOINT\"\nexit 0\n"
	// sudo falso: executil antepone 'sudo -n' cuando no somos root; lo ignora.
	sudo := "#!/bin/sh\nwhile [ $# -gt 0 ]; do case \"$1\" in -*) shift;; *) break;; esac; done\nexec \"$@\"\n"
	// dd falso: anota los args y sale 0 (IdentifyDisk).
	dd := "#!/bin/sh\necho \"$@\" >> " + logFile + "\nexit 0\n"
	for name, body := range map[string]string{"zpool": zpool, "sudo": sudo, "dd": dd} {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	stubPoolRoot(t, "")
	stubBlankDisks(t)
	// Every dataset at /<its own name>, source default: the ordinary case, so
	// that the effective-mountpoint check (mountpoint.go) is not what these
	// tests are about. Tests that do care install their own table afterwards.
	stubDefaultMountpoints(t)

	d, err := db.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	if err := db.Migrate(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	return NewService(d), logFile
}

func TestTrim(t *testing.T) {
	svc, logFile := newTestService(t)

	if err := svc.Trim(context.Background(), "tester", "tank"); err != nil {
		t.Fatalf("Trim: %v", err)
	}
	out, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatalf("el fake zpool no registró la llamada: %v", err)
	}
	if got := strings.TrimSpace(string(out)); got != "trim tank" {
		t.Fatalf("args de zpool = %q, esperaba %q", got, "trim tank")
	}

	// Auditoría: acción pool.trim con actor, sin confirm (no destructiva).
	var action, actor string
	var confirmed int
	err = svc.db.QueryRow(
		"SELECT action, actor, confirmed FROM audit_log WHERE target='tank'").Scan(&action, &actor, &confirmed)
	if err != nil {
		t.Fatalf("audit_log: %v", err)
	}
	if action != "pool.trim" || actor != "tester" || confirmed != 0 {
		t.Fatalf("audit = (%q,%q,%d), esperaba (pool.trim,tester,0)", action, actor, confirmed)
	}
}

func TestTrimNombreInvalido(t *testing.T) {
	svc, _ := newTestService(t)
	for _, bad := range []string{"", "tan k", "tank;rm -rf /", "../etc", strings.Repeat("a", 65)} {
		if err := svc.Trim(context.Background(), "tester", bad); !errors.Is(err, ErrInvalidName) {
			t.Errorf("Trim(%q) = %v, esperaba ErrInvalidName", bad, err)
		}
	}
}

func TestPoolCreateAshift(t *testing.T) {
	svc, logFile := newTestService(t)
	if err := svc.PoolCreate(context.Background(), "tester", "tank", "mirror",
		[]string{"sda", "sdb"}, 12, true); err != nil {
		t.Fatalf("PoolCreate: %v", err)
	}
	out, _ := os.ReadFile(logFile)
	if got := strings.TrimSpace(string(out)); got != "create -o ashift=12 tank mirror sda sdb" {
		t.Fatalf("argv zpool = %q, esperaba %q", got, "create -o ashift=12 tank mirror sda sdb")
	}
	var action string
	var confirmed int
	err := svc.db.QueryRow(
		"SELECT action, confirmed FROM audit_log WHERE target='tank'").Scan(&action, &confirmed)
	if err != nil {
		t.Fatalf("audit_log: %v", err)
	}
	if action != "pool.create" || confirmed != 1 {
		t.Fatalf("audit = (%q,%d), esperaba (pool.create,1)", action, confirmed)
	}
}

func TestPoolCreateAshiftAuto(t *testing.T) {
	svc, logFile := newTestService(t)
	if err := svc.PoolCreate(context.Background(), "tester", "tank", "mirror",
		[]string{"sda", "sdb"}, 0, true); err != nil {
		t.Fatalf("PoolCreate: %v", err)
	}
	out, _ := os.ReadFile(logFile)
	if got := strings.TrimSpace(string(out)); got != "create tank mirror sda sdb" {
		t.Fatalf("argv zpool = %q, esperaba %q", got, "create tank mirror sda sdb")
	}
}

func TestPoolCreateAshiftInvalido(t *testing.T) {
	svc, _ := newTestService(t)
	if err := svc.PoolCreate(context.Background(), "tester", "tank", "mirror",
		[]string{"sda", "sdb"}, 5, true); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("ashift=5 = %v, esperaba ErrInvalidInput", err)
	}
	if err := svc.PoolCreate(context.Background(), "tester", "tank", "mirror",
		[]string{"sda", "sdb"}, 17, true); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("ashift=17 = %v, esperaba ErrInvalidInput", err)
	}
}

func TestIdentifyDisk(t *testing.T) {
	svc, logFile := newTestService(t)

	if err := svc.IdentifyDisk(context.Background(), "tester", "sda"); err != nil {
		t.Fatalf("IdentifyDisk: %v", err)
	}
	out, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatalf("el fake dd no registró la llamada: %v", err)
	}
	want := "if=/dev/sda of=/dev/null bs=1M count=2048"
	if got := strings.TrimSpace(string(out)); got != want {
		t.Fatalf("argv de dd = %q, esperaba %q", got, want)
	}

	// Auditoría: acción disk.identify, sin confirm (no destructiva).
	var action, actor string
	var confirmed int
	err = svc.db.QueryRow(
		"SELECT action, actor, confirmed FROM audit_log WHERE target='sda'").Scan(&action, &actor, &confirmed)
	if err != nil {
		t.Fatalf("audit_log: %v", err)
	}
	if action != "disk.identify" || actor != "tester" || confirmed != 0 {
		t.Fatalf("audit = (%q,%q,%d), esperaba (disk.identify,tester,0)", action, actor, confirmed)
	}
}

func TestIdentifyDiskDevInvalido(t *testing.T) {
	svc, _ := newTestService(t)
	for _, bad := range []string{"", "/dev/sda", "sda;rm", "s d", "../sda", "sda/part1"} {
		if err := svc.IdentifyDisk(context.Background(), "tester", bad); !errors.Is(err, ErrInvalidDev) {
			t.Errorf("IdentifyDisk(%q) = %v, esperaba ErrInvalidDev", bad, err)
		}
	}
}

// SnapshotClone takes -o mountpoint, which unvalidated lets the clone be
// mounted over any path as root ('zfs clone -o mountpoint=/etc' shadows
// sudoers, shadow and the units). It must use the same validator as props.go.
func TestSnapshotCloneMountpointInvalido(t *testing.T) {
	svc, _ := newTestService(t)
	malos := []string{
		"/",                     // the root itself
		"/etc",                  // the case that motivates the check
		"/etc/cron.d",           // below a system root
		"/mnt/../etc",           // the regex allows '.', so clean first
		"/usr", "/boot", "/dev", // other protected roots
		"relativa/sin/barra",   // not absolute
		"/con espacios/dentro", // spaces
		"/etc/$(id)",           // shell substitution
		"/etc/`id`",            // backticks
		"/etc/x;reboot",        // command separator
		"/etc/x\nreboot",       // newline
	}
	for _, mp := range malos {
		err := svc.SnapshotClone(context.Background(), "admin", "tank/d@s", "tank/clone", mp)
		if !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("mountpoint %q: err=%v, esperaba ErrInvalidInput", mp, err)
		}
	}
}

func TestSnapshotCloneMountpointValido(t *testing.T) {
	svc, _ := newTestService(t)
	// A fake zfs of this test's own, prepended after newTestService. Adding it
	// to the shared helper instead would shadow the one props_test installs
	// before calling that helper.
	dir := t.TempDir()
	logFile := filepath.Join(dir, "zfs-args.log")
	zfs := "#!/bin/sh\necho \"$@\" >> " + logFile + "\nexit 0\n"
	if err := os.WriteFile(filepath.Join(dir, "zfs"), []byte(zfs), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	for _, mp := range []string{"", "/tank/clone-mp", "none", "legacy"} {
		if err := svc.SnapshotClone(context.Background(), "admin", "tank/d@s", "tank/clone", mp); err != nil {
			t.Fatalf("mountpoint %q: %v", mp, err)
		}
	}
	out, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "mountpoint=/tank/clone-mp") {
		t.Fatalf("no se pasó el mountpoint válido a zfs: %s", out)
	}
}

// stubPoolRoot makes the pool-root lookup return mp instead of asking zfs.
func stubPoolRoot(t *testing.T, mp string) {
	t.Helper()
	saved := poolRootMountpoint
	poolRootMountpoint = func(context.Context, string) string { return mp }
	t.Cleanup(func() { poolRootMountpoint = saved })
}

// The allowlist: the dataset's own pool tree, and strictly below /mnt,
// /media, /srv and /home. Everything else is refused before the filesystem is
// even looked at.
func TestMountpointAllowlist(t *testing.T) {
	stubPoolRoot(t, "")
	tank := []string{"/tank"}
	for _, p := range []string{"/tank", "/tank/a/b", "/mnt/x", "/media/usb", "/srv/share", "/home/alice"} {
		if !underAllowedRoot(p, tank) {
			t.Errorf("%q should be inside the allowlist for pool tank", p)
		}
	}
	for _, p := range []string{
		"/", "/etc", "/opt/x", "/data", "/var/lib/foo", "/usr/local/x",
		"/mnt", "/srv", "/home", // the roots themselves: mounting there hides everything below
		"/tankx/y", // a prefix of the pool name is not the pool
	} {
		if underAllowedRoot(p, tank) {
			t.Errorf("%q should be outside the allowlist for pool tank", p)
		}
	}
	for _, bad := range []string{"/etc", "/mnt/../etc", "/opt", "/var/lib/dpkg/info", "relative", "/a b"} {
		if err := checkMountpoint(context.Background(), bad, "tank/ds"); !errors.Is(err, ErrInvalidInput) {
			t.Errorf("checkMountpoint(%q) = %v, want ErrInvalidInput", bad, err)
		}
	}
	for _, ok := range []string{"none", "legacy"} {
		if err := checkMountpoint(context.Background(), ok, "tank/ds"); err != nil {
			t.Errorf("checkMountpoint(%q) = %v", ok, err)
		}
	}
}

// A pool may legally be called "etc"; its tree is allowed, but the system
// denylist underneath still refuses /etc.
func TestMountpointDenylistBehindAllowlist(t *testing.T) {
	stubPoolRoot(t, "")
	if err := checkMountpoint(context.Background(), "/etc/x", "etc/ds"); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("pool named etc: checkMountpoint(/etc/x) = %v, want refusal", err)
	}
	saved := append([]string(nil), systemMountpoints...)
	t.Cleanup(func() { systemMountpoints = saved })
	ProtectMountpoint("/srv/easyzfs-data")
	for _, p := range []string{"/srv/easyzfs-data", "/srv/easyzfs-data/update"} {
		if !deniedMountpoint(p) {
			t.Errorf("%q is the registered data dir or below it", p)
		}
	}
	if deniedMountpoint("/srv/other") {
		t.Error("an unrelated /srv path was denied")
	}
}

// ProtectMountpoint makes a relative DB_PATH absolute and also protects where
// a symlinked data dir really lives.
func TestProtectMountpointResolvesAndAbsolutises(t *testing.T) {
	saved := append([]string(nil), systemMountpoints...)
	t.Cleanup(func() { systemMountpoints = saved })
	dir := t.TempDir()
	real := filepath.Join(dir, "real-data")
	if err := os.Mkdir(real, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "data-link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	ProtectMountpoint(link)
	realResolved, _ := filepath.EvalSymlinks(real)
	if !deniedMountpoint(realResolved) {
		t.Errorf("%q is where the protected data dir really lives", realResolved)
	}
	wd, _ := os.Getwd()
	ProtectMountpoint("relative-data")
	if !deniedMountpoint(filepath.Join(wd, "relative-data")) {
		t.Error("a relative data dir was not protected")
	}
}

// The path-trust walk, staged in a temp tree with the trusted owner set to
// the test user. Anyone else able to write to a directory on the way could
// swap the next component for a symlink before root mounts on it.
func TestMountpointPathTrust(t *testing.T) {
	saved := mountTrustedUID
	mountTrustedUID = os.Getuid()
	t.Cleanup(func() { mountTrustedUID = saved })

	base := t.TempDir()
	mk := func(rel string, mode os.FileMode) string {
		p := filepath.Join(base, rel)
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(p, mode); err != nil {
			t.Fatal(err)
		}
		return p
	}
	mk("good", 0o755)
	mk("open", 0o777)
	mk("groupw", 0o775)
	if err := os.Symlink("/etc", filepath.Join(base, "good", "link")); err != nil {
		t.Fatal(err)
	}

	for _, c := range []struct {
		rel string
		ok  bool
	}{
		{"good/new/dir", true}, // missing components: zfs creates them as root
		{"good", true},         // an existing, trusted directory
		{"good/link", false},   // a symlink component
		{"good/link/sudoers.d", false},
		{"open/x", false},   // parent writable by others
		{"open", true},      // the mountpoint dir itself may be open; its parent is trusted
		{"groupw/x", false}, // parent writable by the group
	} {
		err := checkMountpointPath(base, filepath.Join(base, c.rel), true)
		if (err == nil) != c.ok {
			t.Errorf("%s: err=%v, want ok=%v", c.rel, err, c.ok)
		}
	}

	if os.Geteuid() != 0 {
		priv := mk("priv", 0o755)
		mk("priv/x", 0o755)
		if err := os.Chmod(priv, 0); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { os.Chmod(priv, 0o755) })
		if err := checkMountpointPath(base, filepath.Join(priv, "x"), true); err == nil {
			t.Error("a component behind an unreadable directory was accepted")
		}
	}
}

// With a different owner, even a tidy 0755 directory is not trusted.
func TestMountpointPathTrustRequiresOwner(t *testing.T) {
	saved := mountTrustedUID
	mountTrustedUID = os.Getuid() + 1
	t.Cleanup(func() { mountTrustedUID = saved })
	base := t.TempDir()
	if err := checkMountpointPath(base, filepath.Join(base, "x"), true); err == nil {
		t.Error("a directory owned by someone other than the trusted owner was accepted")
	}
}

// A pool is not always at /<pool>: when its root dataset is mounted at /data,
// /data is the pool's tree too. A root mounted at "/" must not put the whole
// filesystem on the allowlist, and none/legacy are not paths.
func TestMountpointPoolRootElsewhere(t *testing.T) {
	ctx := context.Background()
	stubPoolRoot(t, "/data")
	trees := poolTrees(ctx, "tank")
	if !underAllowedRoot("/data/share", trees) || !underAllowedRoot("/tank/x", trees) {
		t.Fatalf("trees %v should cover both /tank and /data", trees)
	}
	if underAllowedRoot("/datax/y", trees) {
		t.Fatal("a prefix of the root mountpoint is not the pool's tree")
	}
	for _, root := range []string{"/", "none", "legacy", "", "relative"} {
		stubPoolRoot(t, root)
		if got := poolTrees(ctx, "tank"); len(got) != 1 || got[0] != "/tank" {
			t.Errorf("root mountpoint %q gave trees %v, want only /tank", root, got)
		}
	}
	// With the root at "/", a system path is still refused.
	stubPoolRoot(t, "/")
	if err := checkMountpoint(ctx, "/opt/x", "tank/ds"); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("root at / let /opt/x through: %v", err)
	}
}

// stubBlankDisks makes every disk read as blank, so action tests never look
// at the machine running them (where "sda" is a real, busy disk).
func stubBlankDisks(t *testing.T) {
	t.Helper()
	savedL, savedS, savedB := lsblkJSON, sysBlockDir, devByIDDir
	lsblkJSON = func(_ context.Context, args ...string) ([]byte, error) {
		name := strings.TrimPrefix(args[len(args)-1], "/dev/")
		return []byte(`{"blockdevices":[{"name":"` + name + `","type":"disk","fstype":null,"parttype":null,"mountpoints":[null]}]}`), nil
	}
	sysBlockDir, devByIDDir = t.TempDir(), t.TempDir()
	t.Cleanup(func() { lsblkJSON, sysBlockDir, devByIDDir = savedL, savedS, savedB })
}

