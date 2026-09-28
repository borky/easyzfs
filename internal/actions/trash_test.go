// trash_test.go — the recycle bin against an in-memory stand-in for zfs that
// answers the handful of commands trash.go runs.
package actions

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

type fakeDS struct {
	mountpoint string // local value, "" = inherited
	received   string // received mountpoint, "" = none
	canmount   string
	keystatus  string
	mounted    bool
	volume     bool
}

type fakeZFS struct {
	ds    map[string]*fakeDS
	calls []string
	fail  map[string]error // command prefix → error
}

func newFakeZFS(names ...string) *fakeZFS {
	f := &fakeZFS{ds: map[string]*fakeDS{}, fail: map[string]error{}}
	for _, n := range names {
		f.ds[n] = &fakeDS{canmount: "on", mounted: true}
	}
	return f
}

// tree — name and its descendants, sorted.
func (f *fakeZFS) tree(name string) []string {
	var out []string
	for n := range f.ds {
		if n == name || strings.HasPrefix(n, name+"/") {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}

func (f *fakeZFS) run(_ context.Context, _ time.Duration, args ...string) ([]byte, error) {
	cmd := strings.Join(args, " ")
	f.calls = append(f.calls, cmd)
	for prefix, err := range f.fail {
		if strings.HasPrefix(cmd, prefix) {
			return nil, err
		}
	}
	last := args[len(args)-1]
	switch args[0] {
	case "get":
		if strings.Contains(cmd, "-o value type") {
			if _, ok := f.ds[last]; !ok {
				return nil, errors.New("dataset does not exist")
			}
			return []byte("filesystem\n"), nil
		}
		props := strings.Split(args[len(args)-2], ",")
		withName := strings.Contains(cmd, "-o name,property,value,source")
		var b strings.Builder
		for _, n := range f.tree(last) {
			if !strings.Contains(cmd, " -r ") && n != last {
				continue
			}
			d := f.ds[n]
			for _, prop := range props {
				val, src := "", "default"
				switch prop {
				case "mountpoint":
					val, src = "/inherited", "inherited from x"
					switch {
					case d.mountpoint != "":
						val, src = d.mountpoint, "local"
					case d.received != "":
						val, src = d.received, "received"
					}
				case "canmount":
					val = d.canmount
				case "keystatus":
					val = "-"
					if d.keystatus != "" {
						val = d.keystatus
					}
				case "mounted":
					val = "no"
					if d.mounted && !d.volume {
						val = "yes"
					}
				case "type":
					val = "filesystem"
					if d.volume {
						val = "volume"
					}
				}
				if withName {
					fmt.Fprintf(&b, "%s\t%s\t%s\t%s\n", n, prop, val, src)
				} else {
					fmt.Fprintf(&b, "%s\t%s\t%s\n", prop, val, src)
				}
			}
		}
		if b.Len() == 0 {
			return nil, errors.New("dataset does not exist")
		}
		return []byte(b.String()), nil
	case "list":
		var b strings.Builder
		for _, n := range f.tree(last) {
			if strings.Count(n, "/") <= strings.Count(last, "/")+1 {
				b.WriteString(n + "\n")
			}
		}
		if b.Len() == 0 {
			return nil, errors.New("dataset does not exist")
		}
		return []byte(b.String()), nil
	case "create":
		f.ds[last] = &fakeDS{mountpoint: "none", canmount: "off"}
	case "set":
		k, v, _ := strings.Cut(args[1], "=")
		d := f.ds[last]
		if d == nil {
			return nil, errors.New("dataset does not exist")
		}
		if k == "mountpoint" {
			d.mountpoint = v
			d.mounted = v != "none"
		}
	case "inherit":
		if args[1] == "-S" && args[2] == "mountpoint" {
			f.ds[last].mountpoint = ""
			f.ds[last].mounted = f.ds[last].received != ""
		}
	case "rename":
		from, to := args[1], args[2]
		if _, ok := f.ds[to]; ok {
			return nil, errors.New("dataset already exists")
		}
		for _, n := range f.tree(from) {
			f.ds[to+strings.TrimPrefix(n, from)] = f.ds[n]
			delete(f.ds, n)
		}
	case "mount":
		f.ds[last].mounted = true
	case "destroy":
		if _, ok := f.ds[last]; !ok {
			return nil, errors.New("dataset does not exist")
		}
		for _, n := range f.tree(last) {
			delete(f.ds, n)
		}
	}
	return nil, nil
}

func useFakeZFS(t *testing.T, f *fakeZFS) {
	saved, savedUse, savedDir := runZFS, inUse, zvolDir
	runZFS = f.run
	inUse = func(context.Context, string, bool) (bool, error) { return false, nil }
	zvolDir = t.TempDir()
	t.Cleanup(func() { runZFS, inUse, zvolDir = saved, savedUse, savedDir })
}

// In use — a VM on a zvol, a container on a subvolume — is refused, as
// destroy refused it with "busy"; so is not being able to tell.
func TestTrashRefusesWhatIsInUse(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		name string
		busy string // path reported busy
		fail bool   // the check itself fails
		want bool   // trash refused
	}{
		{"idle", "", false, false},
		{"mounted filesystem busy", "/inherited", false, true},
		{"zvol open", "zd0", false, true},
		{"cannot tell", "", true, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			svc, _ := newTestService(t)
			f := newFakeZFS("tank", "tank/data", "tank/data/vm-100-disk-0")
			f.ds["tank/data/vm-100-disk-0"].volume = true
			useFakeZFS(t, f)
			// the zvol's device node
			must := func(err error) {
				if err != nil {
					t.Fatal(err)
				}
			}
			dev := filepath.Join(t.TempDir(), "zd0")
			must(os.WriteFile(dev, nil, 0o644))
			must(os.MkdirAll(filepath.Join(zvolDir, "tank", "data"), 0o755))
			must(os.Symlink(dev, filepath.Join(zvolDir, "tank", "data", "vm-100-disk-0")))
			inUse = func(_ context.Context, path string, _ bool) (bool, error) {
				if c.fail {
					return false, errors.New("sudo: a password is required")
				}
				return c.busy != "" && strings.HasSuffix(path, c.busy), nil
			}
			err := svc.DatasetTrash(ctx, "tester", "tank/data", true)
			if refused := errors.Is(err, ErrConflict); refused != c.want {
				t.Fatalf("err = %v, want refused=%v", err, c.want)
			}
			if c.want && f.ds["tank/data"] == nil {
				t.Fatal("refused, yet moved")
			}
		})
	}
}

func TestTrashAndRestore(t *testing.T) {
	svc, _ := newTestService(t)
	f := newFakeZFS("tank", "tank/media", "tank/media/photos", "tank/media/backup")
	f.ds["tank/media/photos"].mountpoint = "/srv/photos"
	f.ds["tank/media/backup"].received = "/srv/replica" // a replication target
	useFakeZFS(t, f)
	ctx := context.Background()

	if err := svc.DatasetTrash(ctx, "tester", "tank/media", false); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("children without recursive: %v, want ErrInvalidInput", err)
	}
	if err := svc.DatasetTrash(ctx, "tester", "tank", true); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("pool root: %v, want ErrInvalidInput", err)
	}
	if err := svc.DatasetTrash(ctx, "tester", "tank/media", true); err != nil {
		t.Fatalf("trash: %v", err)
	}
	list, err := svc.TrashList(ctx)
	if err != nil || len(list) != 1 {
		t.Fatalf("list = %v, %v", list, err)
	}
	e := list[0]
	if !strings.HasPrefix(e.Trashed, "tank/easyzfs-trash/media-") || !InTrash(e.Trashed) {
		t.Fatalf("trashed as %q", e.Trashed)
	}
	if root := f.ds["tank/easyzfs-trash"]; root == nil || root.mountpoint != "none" || root.canmount != "off" {
		t.Fatalf("trash root missing or mountable: %+v", root)
	}
	for _, child := range []string{"/photos", "/backup"} {
		if d := f.ds[e.Trashed+child]; d == nil || d.mounted {
			t.Fatalf("%s left mounted in the bin: %+v", child, d)
		}
	}
	for _, c := range f.calls {
		if strings.Contains(c, "readonly") {
			t.Fatalf("trash touched readonly (breaks in-use zvols): %s", c)
		}
	}
	if got := e.PurgeAt.Sub(e.TrashedAt); got != TrashDays*24*time.Hour {
		t.Fatalf("purge after %v", got)
	}

	warnings, err := svc.TrashRestore(ctx, "tester", e.ID)
	if err != nil || len(warnings) != 0 {
		t.Fatalf("restore: %v, warnings %v", err, warnings)
	}
	if d := f.ds["tank/media/photos"]; d == nil || d.mountpoint != "/srv/photos" || !d.mounted {
		t.Fatalf("child not restored with its mountpoint: %+v", d)
	}
	if d := f.ds["tank/media/backup"]; d == nil || d.mountpoint != "" || d.received != "/srv/replica" || !d.mounted {
		t.Fatalf("received mountpoint not restored as received: %+v", d)
	}
	if !f.ds["tank/media"].mounted {
		t.Fatalf("dataset not mounted again: %+v", f.ds["tank/media"])
	}
	if list, _ := svc.TrashList(ctx); len(list) != 0 {
		t.Fatalf("row kept after restore: %v", list)
	}
	if f.ds["tank/easyzfs-trash"] != nil {
		t.Fatal("empty recycle bin left on the pool")
	}
}

// A dataset someone created as tank/easyzfs-trash by hand is not the bin:
// nothing is moved into it, and it is never destroyed.
func TestTrashRefusesForeignRoot(t *testing.T) {
	svc, _ := newTestService(t)
	f := newFakeZFS("tank", "tank/easyzfs-trash", "tank/docs")
	useFakeZFS(t, f)
	if err := svc.DatasetTrash(context.Background(), "tester", "tank/docs", false); !errors.Is(err, ErrConflict) {
		t.Fatalf("foreign bin root: %v, want ErrConflict", err)
	}
	svc.dropEmptyTrashRoot(context.Background(), "tank")
	if f.ds["tank/easyzfs-trash"] == nil || f.ds["tank/docs"] == nil {
		t.Fatal("a dataset that is not the app's bin was touched")
	}
}

// If the row cannot be written the dataset goes back where it was: a trashed
// dataset without a row would be hidden and never restored or purged.
func TestTrashUndoesWhenTheRowCannotBeWritten(t *testing.T) {
	svc, _ := newTestService(t)
	f := newFakeZFS("tank", "tank/docs", "tank/docs/sub")
	f.ds["tank/docs/sub"].mountpoint = "/srv/sub"
	useFakeZFS(t, f)
	if _, err := svc.db.Exec("DROP TABLE trash"); err != nil {
		t.Fatal(err)
	}
	if err := svc.DatasetTrash(context.Background(), "tester", "tank/docs", true); err == nil {
		t.Fatal("trash reported success without its row")
	}
	if d := f.ds["tank/docs/sub"]; d == nil || d.mountpoint != "/srv/sub" || !d.mounted {
		t.Fatalf("not put back as it was: %+v", d)
	}
}

// Two datasets mapping to the same bin name in the same second.
func TestTrashNameCollision(t *testing.T) {
	svc, _ := newTestService(t)
	f := newFakeZFS("tank", "tank/a_b", "tank/a", "tank/a/b")
	useFakeZFS(t, f)
	ctx := context.Background()
	if err := svc.DatasetTrash(ctx, "tester", "tank/a_b", false); err != nil {
		t.Fatal(err)
	}
	if err := svc.DatasetTrash(ctx, "tester", "tank/a/b", false); err != nil {
		t.Fatalf("second dataset with the same bin name: %v", err)
	}
	if list, _ := svc.TrashList(ctx); len(list) != 2 || list[0].Trashed == list[1].Trashed {
		t.Fatalf("entries: %+v", list)
	}
}

func TestTrashRestoreRefusesTakenName(t *testing.T) {
	svc, _ := newTestService(t)
	f := newFakeZFS("tank", "tank/docs")
	useFakeZFS(t, f)
	ctx := context.Background()
	if err := svc.DatasetTrash(ctx, "tester", "tank/docs", false); err != nil {
		t.Fatal(err)
	}
	f.ds["tank/docs"] = &fakeDS{canmount: "on"} // someone made a new one
	list, _ := svc.TrashList(ctx)
	if _, err := svc.TrashRestore(ctx, "tester", list[0].ID); !errors.Is(err, ErrConflict) {
		t.Fatalf("restore over an existing name: %v, want ErrConflict", err)
	}
}

func TestTrashFailedUnmountUndoes(t *testing.T) {
	svc, _ := newTestService(t)
	f := newFakeZFS("tank", "tank/vm", "tank/vm/a", "tank/vm/b")
	f.ds["tank/vm/a"].mountpoint = "/srv/a"
	f.ds["tank/vm/b"].mountpoint = "/srv/b"
	useFakeZFS(t, f)
	f.fail["rename"] = errors.New("cannot unmount: dataset is busy")
	if err := svc.DatasetTrash(context.Background(), "tester", "tank/vm", true); err == nil {
		t.Fatal("trash succeeded with a failing rename")
	}
	for _, n := range []string{"tank/vm/a", "tank/vm/b"} {
		if d := f.ds[n]; d == nil || d.mountpoint == "none" || !d.mounted {
			t.Fatalf("%s left unmounted after a failed trash: %+v", n, d)
		}
	}
	if list, _ := svc.TrashList(context.Background()); len(list) != 0 {
		t.Fatal("row written for a failed trash")
	}
}

func TestPurgeExpiredOnlyTouchesTheBin(t *testing.T) {
	svc, db := newTestService(t)
	_ = db
	f := newFakeZFS("tank", "tank/old", "tank/keep")
	useFakeZFS(t, f)
	ctx := context.Background()
	if err := svc.DatasetTrash(ctx, "tester", "tank/old", false); err != nil {
		t.Fatal(err)
	}
	// A row pointing outside the bin (a tampered or buggy database) must
	// never be destroyed.
	if _, err := svc.db.Exec(`INSERT INTO trash(pool, original, trashed, trashed_at) VALUES ('tank','tank/keep','tank/keep','2020-01-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	svc.PurgeExpired(ctx, time.Now().Add(time.Duration(TrashDays-1)*24*time.Hour))
	if len(f.tree("tank/easyzfs-trash")) != 2 {
		t.Fatal("purged before its time")
	}
	svc.PurgeExpired(ctx, time.Now().Add(time.Duration(TrashDays+1)*24*time.Hour))
	if len(f.tree("tank/easyzfs-trash")) != 0 {
		t.Fatalf("expired entry not purged, or empty bin left: %v", f.tree("tank"))
	}
	if f.ds["tank/keep"] == nil {
		t.Fatal("PurgeExpired destroyed a dataset outside the recycle bin")
	}
}

func TestInTrash(t *testing.T) {
	for name, want := range map[string]bool{
		"tank/easyzfs-trash":       true,
		"tank/easyzfs-trash/x-1":   true,
		"tank/easyzfs-trash/x-1@s": true,
		"tank/easyzfs-trashy":      false,
		"tank/data/easyzfs-trash":  false,
		"easyzfs-trash":            false,
		"tank/data@easyzfs-trash":  false,
	} {
		if got := InTrash(name); got != want {
			t.Errorf("InTrash(%q) = %v, want %v", name, got, want)
		}
	}
}

// A trashed dataset destroyed from the CLI leaves no stale row behind.
func TestPurgeDropsVanishedEntries(t *testing.T) {
	svc, _ := newTestService(t)
	f := newFakeZFS("tank", "tank/gone")
	useFakeZFS(t, f)
	ctx := context.Background()
	if err := svc.DatasetTrash(ctx, "tester", "tank/gone", false); err != nil {
		t.Fatal(err)
	}
	list, _ := svc.TrashList(ctx)
	delete(f.ds, list[0].Trashed)
	svc.PurgeExpired(ctx, time.Now())
	if list, _ := svc.TrashList(ctx); len(list) != 0 {
		t.Fatalf("stale row kept: %v", list)
	}
}
