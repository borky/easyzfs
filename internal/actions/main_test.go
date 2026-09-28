package actions

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"easyzfs/internal/executil"
)

// observed — every storage-tool argv the actions built during the run.
var (
	observedMu sync.Mutex
	observed   = map[string][]string{}
)

func observeArgv(tool string, args []string) {
	observedMu.Lock()
	defer observedMu.Unlock()
	observed[tool+" "+strings.Join(args, " ")] = append([]string{tool}, args...)
}

// TestMain gives every test in the package a neutral host: root not on ZFS,
// not Proxmox, no datasets. Without it the host-storage guard would read the
// machine running the tests, and refuse everything where zfs is missing.
// hoststorage_test.go replaces this with a captured Proxmox host.
//
// It also records every storage-tool command the actions build (executil's
// PrivObserve, and the recycle bin's fake zfs), and afterwards replays each
// through the privileged gateway's grammar: in production the gateway
// refuses a shape it does not know, which no other test would notice (they
// run without it), so a builder and the grammar drifting apart fails here.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "easyzfs-actions-test")
	if err != nil {
		panic(err)
	}
	mi := filepath.Join(dir, "mountinfo")
	if err := os.WriteFile(mi, []byte("30 1 8:1 / / rw,relatime shared:1 - ext4 /dev/sda1 rw\n"), 0o644); err != nil {
		panic(err)
	}
	mountinfoPath = mi
	pveDir = filepath.Join(dir, "no-pve")
	listAllDatasets = func(context.Context) ([]byte, error) { return nil, nil }
	executil.PrivObserve = observeArgv
	code := m.Run()
	executil.PrivObserve = nil
	if code == 0 {
		code = replayObserved()
	}
	os.RemoveAll(dir)
	os.Exit(code)
}

// replayObserved — every recorded argv must be inside the gateway's grammar.
// The checks behind the grammar (host, mountpoints, disks) run against a
// neutral host and may refuse for their own reasons; only ErrNotAllowed,
// "not a shape the gateway knows", fails.
func replayObserved() int {
	readMountpointProp = func(_ context.Context, ds string) (string, string, error) { return "/" + ds, "default", nil }
	poolRootMountpoint = func(context.Context, string) string { return "" }
	lsblkJSON = func(_ context.Context, args ...string) ([]byte, error) {
		name := strings.TrimPrefix(args[len(args)-1], "/dev/")
		return []byte(`{"blockdevices":[{"name":"` + name + `","type":"disk","fstype":null,"parttype":null,"mountpoints":[null]}]}`), nil
	}
	sysBlockDir, devByIDDir = os.TempDir(), os.TempDir()
	readOrigin = func(context.Context, string) (string, error) { return "-", nil }
	readMounted = func(context.Context, string) (string, error) { return "yes", nil }
	readReceivedMountpoint = func(context.Context, string) (string, error) { return "-", nil }
	zpoolListVHP = func(context.Context) ([]byte, error) { return nil, nil }
	runZFS = func(_ context.Context, _ time.Duration, args ...string) ([]byte, error) {
		return []byte("canmount\toff\tlocal\nmountpoint\tnone\tlocal\n"), nil
	}
	bad := 0
	for key, argv := range observed {
		err := PrivCheck(context.Background(), argv[0], argv[1:])
		if errors.Is(err, ErrNotAllowed) {
			fmt.Fprintf(os.Stderr, "FAIL: the gateway would refuse a command the actions build: %s (%v)\n", key, err)
			bad++
		}
	}
	if len(observed) == 0 {
		fmt.Fprintln(os.Stderr, "FAIL: no storage-tool command was recorded; the replay checks nothing")
		return 1
	}
	if bad > 0 {
		return 1
	}
	return 0
}
