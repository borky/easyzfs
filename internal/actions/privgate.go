// privgate.go — the policy of the privileged gateway ('easyzfs priv', run
// as root through sudo, see cmd dispatch in main.go and executil.PrivBin).
//
// Every zfs, zpool, smartctl, dd, hdparm and udisksctl command the service
// runs with root rights arrives here first, as the literal argv the actions
// built. PrivCheck matches it against a closed grammar — the same shapes the
// pinned sudoers file used to allow — and then applies, as root and right
// before the command runs, the checks the action itself makes: name
// whitelists, the host/guest storage policy (hoststorage.go), the effective
// mountpoint rules (mountpoint.go, props.go) and the live disk checks
// (diskuse.go). The service side keeps its own copies of these checks for
// quick, well-worded refusals; this copy is the one a compromised service
// account cannot skip, because sudoers no longer lets it run any of these
// tools directly.
//
// In root mode (the service itself running as root, no sudo) executil calls
// PrivCheck in-process instead, through executil.PrivGate.
package actions

import (
	"context"
	"errors"
	"fmt"
	"path"
	"regexp"
	"strings"
	"time"

	"easyzfs/internal/executil"
	"easyzfs/internal/model"
)

// ErrNotAllowed — an argv outside the gateway's grammar. Mapped to 403.
var ErrNotAllowed = errors.New("operación privilegiada no permitida")

// replSnapPrefix — internal/replication's snapshot prefix (SnapPrefix there;
// not imported, the dependency goes the other way).
const replSnapPrefix = "ezrepl-"

// reKernelDev — /dev/<kernel name>, as smartctl, dd, hdparm and udisksctl
// get them.
var reKernelDev = regexp.MustCompile(`^/dev/[a-zA-Z0-9][a-zA-Z0-9_.-]{0,63}$`)

func init() {
	// Refusals from the root side come back to the service as executil
	// PrivErrors; these make errors.Is see the domain error through them.
	for code, err := range map[string]error{
		"host_storage":  ErrHostStorage,
		"host_unknown":  ErrHostUnknown,
		"invalid_input": ErrInvalidInput,
		"invalid_name":  ErrInvalidName,
		"invalid_dev":   ErrInvalidDev,
		"dev_in_use":    ErrDiskInUse,
		"conflict":      ErrConflict,
		"not_allowed":   ErrNotAllowed,
	} {
		executil.RegisterPrivCode(code, err)
	}
}

// PrivCode — the wire code for a refusal PrivCheck returned.
func PrivCode(err error) string {
	for _, c := range []struct {
		code string
		err  error
	}{
		{"host_storage", ErrHostStorage}, {"host_unknown", ErrHostUnknown},
		{"dev_in_use", ErrDiskInUse}, {"conflict", ErrConflict},
		{"invalid_name", ErrInvalidName}, {"invalid_dev", ErrInvalidDev},
		{"not_allowed", ErrNotAllowed},
	} {
		if errors.Is(err, c.err) {
			return c.code
		}
	}
	return "invalid_input"
}

func notAllowed(tool string, args []string) error {
	return fmt.Errorf("%w: %s %s", ErrNotAllowed, tool, strings.Join(args, " "))
}

// PrivCheck — may tool run with args, as root, now?
func PrivCheck(ctx context.Context, tool string, args []string) error {
	switch tool {
	case "zfs":
		return privZFS(ctx, args)
	case "zpool":
		return privZpool(ctx, args)
	case "smartctl":
		if len(args) == 3 && args[0] == "-j" && args[1] == "-a" && reKernelDev.MatchString(args[2]) {
			return nil
		}
		if len(args) == 3 && args[0] == "-t" && (args[1] == "short" || args[1] == "long") && reKernelDev.MatchString(args[2]) {
			return nil
		}
	case "dd":
		// The identify read, nothing else: dd writes wherever of= says.
		if len(args) == 4 && strings.HasPrefix(args[0], "if=") && reKernelDev.MatchString(strings.TrimPrefix(args[0], "if=")) &&
			args[1] == "of=/dev/null" && args[2] == "bs=1M" && args[3] == "count=2048" {
			return nil
		}
	case "hdparm", "udisksctl":
		var dev string
		switch {
		case tool == "hdparm" && len(args) == 2 && args[0] == "-y":
			dev = args[1]
		case tool == "udisksctl" && len(args) == 3 && args[0] == "power-off" && args[1] == "-b":
			dev = args[2]
		default:
			return notAllowed(tool, args)
		}
		if !reKernelDev.MatchString(dev) {
			return ErrInvalidDev
		}
		// Powering a disk off: nothing on it may be in use, read live.
		reason, err := DiskActiveUse(ctx, strings.TrimPrefix(dev, "/dev/"))
		if err != nil {
			return fmt.Errorf("%w: %v", ErrHostUnknown, err)
		}
		if reason != "" {
			return fmt.Errorf("%w: %s", ErrDiskInUse, reason)
		}
		return nil
	}
	return notAllowed(tool, args)
}

// isSnap / isBookmark / isDataset — the name shapes, with the same whitelists
// the actions use.
func isDataset(s string) bool { return reDataset.MatchString(s) }
func isSnap(s string) bool {
	ds, snap, ok := strings.Cut(s, "@")
	return ok && reDataset.MatchString(ds) && reSnapName.MatchString(snap)
}
func isBookmark(s string) bool {
	ds, bm, ok := strings.Cut(s, "#")
	return ok && reDataset.MatchString(ds) && reSnapName.MatchString(bm)
}

// ownSnapshot — a snapshot EasyZFS made itself (scheduled or replication);
// on a guest disk those are ours to remove, not Proxmox's.
func ownSnapshot(name string) bool {
	_, snap, _ := strings.Cut(name, "@")
	return strings.HasPrefix(snap, model.AutoSnapPrefix) || strings.HasPrefix(snap, replSnapPrefix)
}

// readOnlyToken — an argument of zfs/zpool list|get|status|iostat: no -c
// (status/iostat -c runs scripts), nothing shell-like.
var reReadToken = regexp.MustCompile(`^(-[A-Zabd-z]+|--json|--version|[A-Za-z0-9_./@#][A-Za-z0-9_.,/@#:=-]*)$`)

func readArgs(args []string) bool {
	for _, a := range args {
		if !reReadToken.MatchString(a) {
			return false
		}
	}
	return true
}

func privZFS(ctx context.Context, a []string) error {
	if len(a) == 0 {
		return notAllowed("zfs", a)
	}
	n := len(a)
	switch a[0] {
	case "list", "get":
		if readArgs(a[1:]) {
			return nil
		}
	case "version":
		if n == 1 {
			return nil
		}
	case "diff":
		if n == 4 && a[1] == "-FHt" && isSnap(a[2]) && (isSnap(a[3]) || isDataset(a[3])) {
			return nil
		}
	case "send":
		// Reads a stream out; what receives it is checked on its own side.
		rest := a[1:]
		if len(rest) >= 2 && rest[0] == "-v" {
			rest = rest[1:]
			if len(rest) > 0 && rest[0] == "-w" {
				rest = rest[1:]
			}
			if len(rest) == 3 && rest[0] == "-i" && isBookmark(rest[1]) && isSnap(rest[2]) {
				return nil
			}
			if len(rest) == 1 && isSnap(rest[0]) {
				return nil
			}
		}
	case "recv":
		// A receive creates and mounts its destination, and a later
		// force_full may destroy it: same checks as the replication runner.
		if n == 3 && a[1] == "-s" && isDataset(a[2]) {
			if err := guardHost(ctx, OpDatasetRemove, "", a[2]); err != nil {
				return err
			}
			return checkEffectiveMountpoint(ctx, a[2])
		}
	case "snapshot":
		rest, recursive := a[1:], false
		if len(rest) > 0 && rest[0] == "-r" {
			rest, recursive = rest[1:], true
		}
		if len(rest) == 0 || (recursive && len(rest) != 1) {
			break
		}
		h, err := LoadHostView(ctx)
		if err != nil {
			return fmt.Errorf("%w: %v; no se hace nada", ErrHostUnknown, err)
		}
		for _, t := range rest {
			if !isSnap(t) {
				return ErrInvalidName
			}
			ds, snap, _ := strings.Cut(t, "@")
			// Replication snapshots a guest disk it copies off-host, on purpose.
			if !strings.HasPrefix(snap, replSnapPrefix) {
				if err := h.check(OpSnapshotCreate, "", ds); err != nil {
					return err
				}
			}
			if recursive {
				for _, g := range h.guests {
					if strings.HasPrefix(g, ds+"/") {
						return fmt.Errorf("%w: el snapshot recursivo de %s incluiría discos de Proxmox (%s…)", ErrHostStorage, ds, g)
					}
				}
			}
		}
		return nil
	case "bookmark":
		if n == 3 && isSnap(a[1]) && isBookmark(a[2]) {
			return nil
		}
	case "destroy":
		rest := a[1:]
		if len(rest) > 0 && rest[0] == "-r" {
			rest = rest[1:]
		}
		if len(rest) != 1 {
			break
		}
		t := rest[0]
		switch {
		case isBookmark(t):
			return nil
		case isSnap(t):
			if ownSnapshot(t) {
				return nil
			}
			ds, _, _ := strings.Cut(t, "@")
			return guardHost(ctx, OpSnapshotDestroy, "", ds)
		case isDataset(t):
			if InTrash(t) {
				// The bin's own purge. What is in the bin was checked on the
				// way in, and only bin paths pass here.
				return nil
			}
			return guardHost(ctx, OpDatasetRemove, "", t)
		}
		return ErrInvalidName
	case "rollback":
		if n == 3 && a[1] == "-r" && isSnap(a[2]) {
			ds, _, _ := strings.Cut(a[2], "@")
			return guardHost(ctx, OpRollback, "", ds)
		}
	case "create":
		return privZFSCreate(ctx, a[1:])
	case "clone":
		rest, mp := a[1:], ""
		if len(rest) == 4 && rest[0] == "-o" && strings.HasPrefix(rest[1], "mountpoint=") {
			mp, rest = strings.TrimPrefix(rest[1], "mountpoint="), rest[2:]
		}
		if len(rest) != 2 || !isSnap(rest[0]) || !isDataset(rest[1]) {
			break
		}
		if InTrash(rest[1]) {
			return fmt.Errorf("%w: %s está reservado para la papelera", ErrInvalidInput, TrashDir)
		}
		if err := guardNewName(ctx, rest[1]); err != nil {
			return err
		}
		if mp != "" {
			return checkMountpoint(ctx, mp, rest[1])
		}
		ds, _, _ := strings.Cut(rest[0], "@")
		vol, err := (&Service{}).isVolume(ctx, ds)
		if err != nil {
			return err
		}
		if vol {
			return nil
		}
		return checkEffectiveMountpoint(ctx, rest[1])
	case "rename":
		if n != 3 || !isDataset(a[1]) || !isDataset(a[2]) {
			break
		}
		// Moving into or out of the recycle bin is a rename too; its
		// own checks (trash.go) ran on the way, and these hold for it.
		if err := guardHost(ctx, OpDatasetRemove, "", a[1]); err != nil {
			return err
		}
		if err := guardNewName(ctx, a[2]); err != nil {
			return err
		}
		return checkRenameMount(ctx, a[1], a[2])
	case "promote":
		if n == 2 && isDataset(a[1]) {
			if err := guardHost(ctx, OpDatasetRemove, "", a[1]); err != nil {
				return err
			}
			return checkMountDanger(ctx, a[1])
		}
	case "mount":
		if n == 2 && isDataset(a[1]) {
			return checkEffectiveMountpoint(ctx, a[1])
		}
	case "unmount":
		if n == 2 && isDataset(a[1]) {
			return guardHost(ctx, OpDatasetUnmount, "", a[1])
		}
	case "share":
		if n == 2 && isDataset(a[1]) {
			return nil
		}
	case "load-key":
		if (n == 2 && isDataset(a[1])) || (n == 5 && a[1] == "-n" && a[2] == "-L" && a[3] == "prompt" && isDataset(a[4])) {
			return nil
		}
	case "unload-key":
		if n == 2 && isDataset(a[1]) {
			return guardHost(ctx, OpDatasetSensitive, "", a[1])
		}
	case "change-key":
		if n == 4 && a[1] == "-o" && a[2] == "keyformat=passphrase" && isDataset(a[3]) {
			return guardHost(ctx, OpDatasetSensitive, "", a[3])
		}
	case "set":
		return privZFSSet(ctx, a[1:])
	case "inherit":
		return privZFSInherit(ctx, a[1:])
	case "rewrite":
		if n == 4 && a[1] == "-r" && a[2] == "-x" && reMountpoint.MatchString(a[3]) {
			return privRewrite(ctx, path.Clean(a[3]))
		}
	}
	return notAllowed("zfs", a)
}

// privZFSCreate — the two create shapes: a dataset (DatasetCreate) and the
// recycle bin's root (trash.go).
func privZFSCreate(ctx context.Context, a []string) error {
	if len(a) == 5 && a[0] == "-o" && a[1] == "mountpoint=none" && a[2] == "-o" && a[3] == "canmount=off" {
		pool, rest, ok := strings.Cut(a[4], "/")
		if ok && rePool.MatchString(pool) && rest == TrashDir {
			return nil
		}
		return notAllowed("zfs", append([]string{"create"}, a...))
	}
	if len(a) < 4 || a[0] != "-p" {
		return notAllowed("zfs", append([]string{"create"}, a...))
	}
	name := a[len(a)-1]
	volume := false
	for i := 1; i < len(a)-1; i++ {
		switch {
		case a[i] == "-o" && i+1 < len(a)-1 && allowedCreateOpt(a[i+1]):
			i++
		case a[i] == "-V" && i+1 < len(a)-1 && reDigits.MatchString(a[i+1]):
			volume = true
			i++
		default:
			return notAllowed("zfs", append([]string{"create"}, a...))
		}
	}
	if !isDataset(name) {
		return ErrInvalidName
	}
	if InTrash(name) {
		return fmt.Errorf("%w: %s está reservado para la papelera", ErrInvalidInput, TrashDir)
	}
	if err := guardNewName(ctx, name); err != nil {
		return err
	}
	if volume {
		// -p still creates and mounts the missing parents of a volume.
		if parent, ok := parentDataset(name); ok {
			if _, _, err := readMountpointProp(ctx, parent); errors.Is(err, ErrNoSuchDataset) {
				return checkEffectiveMountpointTree(ctx, parent)
			}
		}
		return nil
	}
	return checkEffectiveMountpointTree(ctx, name)
}

var reDigits = regexp.MustCompile(`^[0-9]+$`)

// allowedCreateOpt — the -o options DatasetCreate passes.
func allowedCreateOpt(o string) bool {
	switch o {
	case "compression=lz4", "compression=zstd", "compression=off",
		"atime=on", "atime=off", "atime=relatime",
		"encryption=aes-256-gcm", "keyformat=passphrase", "keylocation=prompt":
		return true
	}
	return strings.HasPrefix(o, "quota=") && reDigits.MatchString(strings.TrimPrefix(o, "quota="))
}

// privZFSSet — DatasetPropSet's rules for one 'prop=value dataset'.
func privZFSSet(ctx context.Context, a []string) error {
	if len(a) != 2 || !isDataset(a[1]) {
		return notAllowed("zfs", append([]string{"set"}, a...))
	}
	prop, value, ok := strings.Cut(a[0], "=")
	spec, known := propValidators[prop]
	if !ok || !known || !spec.valid(value) {
		return notAllowed("zfs", append([]string{"set"}, a...))
	}
	name := a[1]
	if err := guardHost(ctx, propHostOp(prop), "", name); err != nil {
		return err
	}
	switch {
	case prop == "mountpoint" && value == "none" && InTrash(name):
		return nil // moving into the bin (trash.go)
	case prop == "mountpoint":
		// Putting a recorded mountpoint back on restore is a value the
		// dataset had before; the explicit-mountpoint rules still apply.
		return checkMountpoint(ctx, value, name)
	case prop == "canmount" && value == "on":
		return checkMountDanger(ctx, name)
	}
	return nil
}

// privZFSInherit — DatasetPropInherit's rules; -S (back to the received
// value) is how the recycle bin restores a received mountpoint.
func privZFSInherit(ctx context.Context, a []string) error {
	received := false
	if len(a) == 3 && a[0] == "-S" {
		received, a = true, a[1:]
	}
	if len(a) != 2 || !isDataset(a[1]) {
		return notAllowed("zfs", append([]string{"inherit"}, a...))
	}
	prop, name := a[0], a[1]
	if _, ok := propValidators[prop]; !ok {
		return notAllowed("zfs", append([]string{"inherit"}, a...))
	}
	if err := guardHost(ctx, propHostOp(prop), "", name); err != nil {
		return err
	}
	switch prop {
	case "mountpoint":
		if received {
			v, err := readReceivedMountpoint(ctx, name)
			if err != nil {
				return err
			}
			if strings.HasPrefix(v, "/") {
				r := newMountResolver()
				return r.checkTarget(ctx, name, mountTarget{path: path.Clean(v), recorded: true}, false)
			}
		}
		return checkInheritedMountpoint(ctx, name)
	case "canmount":
		return checkMountDanger(ctx, name)
	}
	return nil
}

// readReceivedMountpoint — the received value of mountpoint ("-" if none).
var readReceivedMountpoint = func(ctx context.Context, name string) (string, error) {
	out, err := executil.RunRead(ctx, 10*time.Second, "zfs", "get", "-H", "-o", "received", "mountpoint", name)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrHostUnknown, err)
	}
	return strings.TrimSpace(string(out)), nil
}

// privRewrite — 'zfs rewrite -r -x <path>' rewrites what is mounted at path;
// the dataset mounted there must not be host or guest storage.
func privRewrite(ctx context.Context, mp string) error {
	h, err := LoadHostView(ctx)
	if err != nil {
		return fmt.Errorf("%w: %v; no se hace nada", ErrHostUnknown, err)
	}
	for _, d := range h.names {
		if e := h.datasets[d]; e.typ == "filesystem" && path.Clean(e.mountpoint) == mp {
			return h.check(OpDatasetRemove, "", d)
		}
	}
	return fmt.Errorf("%w: ningún dataset está montado en %s", ErrInvalidInput, mp)
}

func privZpool(ctx context.Context, a []string) error {
	if len(a) == 0 {
		return notAllowed("zpool", a)
	}
	n := len(a)
	pool := func(i int) bool { return i < n && rePool.MatchString(a[i]) }
	switch a[0] {
	case "list", "get", "status", "iostat":
		if readArgs(a[1:]) {
			return nil
		}
	case "--version":
		if n == 1 {
			return nil
		}
	case "events":
		if n == 2 && a[1] == "-f" {
			return nil
		}
	case "history":
		if n == 3 && a[1] == "-i" && pool(2) {
			return nil
		}
	case "import":
		if n == 1 { // list what could be imported
			return nil
		}
		// Only -N: the mounting form would mount wherever the pool says,
		// /etc included; datasets are then mounted one by one through
		// 'zfs mount', which is checked here.
		if n == 3 && a[1] == "-N" && pool(2) {
			return nil
		}
	case "online", "clear":
		if (n == 2 && pool(1)) || (n == 3 && pool(1) && validNewDev(a[2])) {
			return nil
		}
	case "scrub":
		if (n == 2 && pool(1)) || (n == 3 && (a[1] == "-p" || a[1] == "-s") && pool(2)) {
			return nil
		}
	case "trim":
		if n == 2 && pool(1) {
			return nil
		}
	case "set":
		if n == 3 && (a[1] == "autotrim=on" || a[1] == "autotrim=off") && pool(2) {
			return nil
		}
	case "destroy":
		if n == 2 && pool(1) {
			return guardHost(ctx, OpPoolRemove, a[1], "")
		}
	case "export":
		if (n == 2 && pool(1)) || (n == 3 && a[1] == "-f" && pool(2)) {
			return guardHost(ctx, OpPoolRemove, a[n-1], "")
		}
	case "checkpoint":
		if (n == 2 && pool(1)) || (n == 3 && a[1] == "-d" && pool(2)) {
			return guardHost(ctx, OpPoolLayout, a[n-1], "")
		}
	case "offline", "detach":
		if n == 3 && pool(1) && validNewDev(a[2]) {
			return guardHost(ctx, OpPoolLayout, a[1], "")
		}
	case "replace":
		if n == 4 && pool(1) && validNewDev(a[2]) && validNewDev(a[3]) {
			if err := guardHost(ctx, OpPoolLayout, a[1], ""); err != nil {
				return err
			}
			own := ""
			if kernelName(a[2]) == kernelName(a[3]) {
				own = a[1]
			}
			return requireFreeDiskFor(ctx, a[3], own)
		}
	case "attach":
		if n == 4 && pool(1) && validNewDev(a[2]) && validNewDev(a[3]) {
			if err := guardHost(ctx, OpPoolLayout, a[1], ""); err != nil {
				return err
			}
			return requireFreeDisk(ctx, a[3])
		}
	case "add":
		if n >= 3 && pool(1) {
			if err := guardHost(ctx, OpPoolLayout, a[1], ""); err != nil {
				return err
			}
			return privVdevDisks(ctx, a[2:])
		}
	case "create":
		rest := a[1:]
		if len(rest) >= 2 && rest[0] == "-o" && strings.HasPrefix(rest[1], "ashift=") &&
			reDigits.MatchString(strings.TrimPrefix(rest[1], "ashift=")) {
			rest = rest[2:]
		}
		if len(rest) < 2 || !rePool.MatchString(rest[0]) {
			break
		}
		name := rest[0]
		if err := newMountResolver().checkTarget(ctx, name, mountTarget{path: "/" + name}, true); err != nil {
			return err
		}
		return privVdevDisks(ctx, rest[1:])
	}
	return notAllowed("zpool", a)
}

// privVdevDisks — '[topology] disk…' of create/add: every disk free, live.
func privVdevDisks(ctx context.Context, a []string) error {
	if len(a) > 0 {
		switch a[0] {
		case "mirror", "raidz1", "raidz2", "raidz3":
			a = a[1:]
		}
	}
	if len(a) == 0 {
		return ErrInvalidDev
	}
	for _, d := range a {
		if !validNewDev(d) {
			return ErrInvalidDev
		}
		if err := requireFreeDisk(ctx, d); err != nil {
			return err
		}
	}
	return nil
}
