// trash_test.go — the recycle bin against an in-memory stand-in for zfs that
// answers the handful of commands trash.go runs.
package actions

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"
)

type fakeDS struct {
	mountpoint string // local value, "" = inherited
	readonly   string // local value, "" = inherited
	canmount   string
	mounted    bool
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
		prop := args[len(args)-2]
		var b strings.Builder
		for _, n := range f.tree(last) {
			if !strings.Contains(cmd, " -r ") && n != last {
				continue
			}
			d := f.ds[n]
			val, src := "", "inherited from x"
			switch prop {
			case "mountpoint":
				val = "/inherited"
				if d.mountpoint != "" {
					val, src = d.mountpoint, "local"
				}
			case "readonly":
				val = "off"
				if d.readonly != "" {
					val, src = d.readonly, "local"
				}
			case "canmount":
				val, src = d.canmount, "default"
			}
			fmt.Fprintf(&b, "%s\t%s\t%s\n", n, val, src)
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
		switch k {
		case "mountpoint":
			d.mountpoint = v
			d.mounted = v != "none"
		case "readonly":
			d.readonly = v
		}
	case "inherit":
		if args[1] == "readonly" {
			f.ds[last].readonly = ""
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
	saved := runZFS
	runZFS = f.run
	t.Cleanup(func() { runZFS = saved })
}

func TestTrashAndRestore(t *testing.T) {
	svc, _ := newTestService(t)
	f := newFakeZFS("tank", "tank/media", "tank/media/photos")
	f.ds["tank/media/photos"].mountpoint = "/srv/photos"
	f.ds["tank/media"].readonly = "off"
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
	if f.ds["tank/easyzfs-trash"] == nil || f.ds["tank/easyzfs-trash"].mountpoint != "none" {
		t.Fatal("trash root missing or mountable")
	}
	photos := f.ds[e.Trashed+"/photos"]
	if photos == nil || photos.mounted || photos.mountpoint != "none" {
		t.Fatalf("child with its own mountpoint left mounted: %+v", photos)
	}
	if f.ds[e.Trashed].readonly != "on" {
		t.Fatal("trashed dataset not readonly")
	}
	if got := e.PurgeAt.Sub(e.TrashedAt); got != TrashDays*24*time.Hour {
		t.Fatalf("purge after %v", got)
	}

	if err := svc.TrashRestore(ctx, "tester", e.ID); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if f.ds["tank/media/photos"] == nil || f.ds["tank/media/photos"].mountpoint != "/srv/photos" || !f.ds["tank/media/photos"].mounted {
		t.Fatalf("child not restored with its mountpoint: %+v", f.ds["tank/media/photos"])
	}
	if f.ds["tank/media"].readonly != "off" || !f.ds["tank/media"].mounted {
		t.Fatalf("dataset not restored as it was: %+v", f.ds["tank/media"])
	}
	if list, _ := svc.TrashList(ctx); len(list) != 0 {
		t.Fatalf("row kept after restore: %v", list)
	}
	if f.ds["tank/easyzfs-trash"] != nil {
		t.Fatal("empty recycle bin left on the pool")
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
	if err := svc.TrashRestore(ctx, "tester", list[0].ID); !errors.Is(err, ErrConflict) {
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
