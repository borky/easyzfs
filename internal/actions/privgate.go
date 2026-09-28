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
		// A stream is the dataset's full contents on stdout, readable by the
		// service account: never the running OS (/etc/shadow, host keys, the
		// cluster database). Guest disks and data may be replicated off-host;
		// that is what replication is for.
		rest := a[1:]
		if len(rest) >= 2 && rest[0] == "-v" {
			rest = rest[1:]
			if len(rest) > 0 && rest[0] == "-w" {
				rest = rest[1:]
			}
			var snap string
			switch {
			case len(rest) == 3 && rest[0] == "-i" && isBookmark(rest[1]) && isSnap(rest[2]):
				snap = rest[2]
			case len(rest) == 1 && isSnap(rest[0]):
				snap = rest[0]
			default:
				return notAllowed("zfs", a)
			}
			ds, _, _ := strings.Cut(snap, "@")
			h, err := LoadHostView(ctx)
			if err != nil {
				return fmt.Errorf("%w: %v; no se hace nada", ErrHostUnknown, err)
			}
			if kind, why := h.DatasetKind(ds); kind == HostSystem {
				return fmt.Errorf("%w: %s; su contenido no se envía fuera del sistema", ErrHostStorage, why)
			}
			return nil
		}
	case "recv":
		// Only the hardened forms (RecvFSArgs, RecvVolArgs): a stream is
		// whatever the sender put in it, setuid-root files and device nodes
		// included, so a filesystem is received unmounted, with setuid,
		// devices and exec forced off and the stream's own mountpoint and
		// share settings ignored; mounted later, it goes through the mount
		// check. Which form fits is decided by the stream itself, which only
		// the gateway process can read (internal/priv, RecvStreamIsVolume):
		// volmode is silently ignored for a filesystem stream, so the volume
		// form must never receive one.
		//
		// Into an existing dataset only if a receive of ours created it
		// (easyzfs:replica=on, which only a receive sets): an incremental
		// stream built against any dataset's latest snapshot would otherwise
		// plant files in it. A later force_full may destroy it, hence the
		// host check.
		if n >= 3 && isDataset(a[n-1]) && (argsEqual(a[1:n-1], RecvFSArgs) || argsEqual(a[1:n-1], RecvVolArgs)) {
			dest := a[n-1]
			if err := guardHost(ctx, OpDatasetRemove, "", dest); err != nil {
				return err
			}
			mark, err := readReplicaMark(ctx, dest)
			switch {
			case errors.Is(err, ErrNoSuchDataset):
				// a new replica
			case err != nil:
				return fmt.Errorf("%w: %v", ErrHostUnknown, err)
			case mark != "on":
				return fmt.Errorf("%w: %s ya existe y no es una réplica creada por EasyZFS; no se recibe encima", ErrConflict, dest)
			}
			if argsEqual(a[1:n-1], RecvFSArgs) {
				return checkEffectiveMountpoint(ctx, dest)
			}
			return nil
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
		rest, recursive := a[1:], false
		if len(rest) > 0 && rest[0] == "-r" {
			rest, recursive = rest[1:], true
		}
		if len(rest) != 1 {
			break
		}
		t := rest[0]
		switch {
		case isBookmark(t):
			return nil
		case isSnap(t):
			// -r on a snapshot destroys the same-named snapshot in every
			// descendant, guest disks included, while the check below sees
			// only the top one. Nothing in the service needs it.
			if recursive {
				return notAllowed("zfs", a)
			}
			if ownSnapshot(t) {
				return nil
			}
			ds, _, _ := strings.Cut(t, "@")
			return guardHost(ctx, OpSnapshotDestroy, "", ds)
		case isDataset(t):
			if InTrash(t) {
				// The bin's own purge. What is in the bin was checked on the
				// way in, and only bin paths pass here — of a bin EasyZFS
				// made, not a dataset someone named easyzfs-trash by hand.
				pool, _, _ := strings.Cut(t, "/")
				if _, ours := ownTrashRoot(ctx, TrashRoot(pool)); !ours {
					return fmt.Errorf("%w: %s no es la papelera de EasyZFS", ErrHostStorage, TrashRoot(pool))
				}
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
		// Always with setuid and devices off (SnapshotClone): a clone
		// inherits them from its new parent, not from its origin.
		rest, mp := a[1:], ""
		if len(rest) < 4 || rest[0] != "-o" || rest[1] != "setuid=off" || rest[2] != "-o" || rest[3] != "devices=off" {
			return notAllowed("zfs", a)
		}
		rest = rest[4:]
		if len(rest) == 4 && rest[0] == "-o" && strings.HasPrefix(rest[1], "mountpoint=") {
			mp, rest = strings.TrimPrefix(rest[1], "mountpoint="), rest[2:]
		}
		if len(rest) != 2 || !isSnap(rest[0]) || !isDataset(rest[1]) {
			break
		}
		// A clone of a guest disk's snapshot pins it (Proxmox can no longer
		// remove the disk), and promoting the clone would carry the disk's
		// snapshots away from it.
		src, _, _ := strings.Cut(rest[0], "@")
		h, err := LoadHostView(ctx)
		if err != nil {
			return fmt.Errorf("%w: %v; no se hace nada", ErrHostUnknown, err)
		}
		if kind, why := h.DatasetKind(src); kind == HostGuest {
			return fmt.Errorf("%w: %s", ErrHostStorage, why)
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
			// Promote takes the origin's older snapshots over: the origin is
			// affected as much as the clone.
			origin, err := readOrigin(ctx, a[1])
			if err != nil {
				return err
			}
			if ds, _, ok := strings.Cut(origin, "@"); ok {
				if err := guardHost(ctx, OpDatasetRemove, "", ds); err != nil {
					return err
				}
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
	if err := setuidDevicesOn(prop, value); err != nil {
		return err
	}
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
	if _, ok := propValidators[prop]; !ok || prop == "setuid" || prop == "devices" {
		// Inheriting setuid or devices resets them to the default, or with
		// -S to what a crafted stream carried: "on" either way.
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

// RecvFSArgs / RecvVolArgs — the two receive shapes the gateway runs (see
// its "recv" case), for a filesystem and a volume stream; internal/replication
// builds its receive stage from them. The filesystem form fails on a volume
// stream (setuid "does not apply"), and the gateway checks the stream's own
// type before running either (internal/priv). Both stamp the destination
// easyzfs:replica=on, the mark a later incremental receive requires.
var (
	RecvFSArgs = []string{"-s", "-u", "-o", "setuid=off", "-o", "devices=off", "-o", "exec=off",
		"-x", "mountpoint", "-x", "canmount", "-x", "sharenfs", "-x", "sharesmb", "-o", "easyzfs:replica=on"}
	RecvVolArgs = []string{"-s", "-u", "-o", "volmode=dev", "-o", "easyzfs:replica=on"}
)

// RecvArgsFor — the receive shape for a volume or a filesystem stream.
func RecvArgsFor(volume bool) []string {
	if volume {
		return RecvVolArgs
	}
	return RecvFSArgs
}

// IsRecvVolumeForm — whether a receive argv (after "recv") is the volume form.
func IsRecvVolumeForm(args []string) bool {
	return len(args) == len(RecvVolArgs)+1 && argsEqual(args[:len(args)-1], RecvVolArgs)
}

func argsEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// readReplicaMark — the easyzfs:replica user property; ErrNoSuchDataset when
// the dataset does not exist.
var readReplicaMark = func(ctx context.Context, name string) (string, error) {
	out, err := executil.RunRead(ctx, 10*time.Second, "zfs", "get", "-H", "-o", "value", "easyzfs:replica", name)
	if err != nil {
		return "", classifyReadErr(err)
	}
	return strings.TrimSpace(string(out)), nil
}

// DMU objset types in a send stream's DRR_BEGIN record (sys/dmu.h).
const (
	dmuOstZFS  = 2
	dmuOstZvol = 3
)

// RecvStreamIsVolume reads the stream's first record (DRR_BEGIN) and says
// whether it carries a volume. hdr must hold at least 40 bytes: record type
// and payload length (2×uint32), then drr_magic, versioninfo and creation
// time (3×uint64) and the objset type (uint32). drr_magic tells the byte
// order.
func RecvStreamIsVolume(hdr []byte) (bool, error) {
	if len(hdr) < 40 {
		return false, fmt.Errorf("%w: stream demasiado corto", ErrInvalidInput)
	}
	const magic = 0x2F5bacbac
	le := func(b []byte) uint64 {
		var v uint64
		for i := 7; i >= 0; i-- {
			v = v<<8 | uint64(b[i])
		}
		return v
	}
	be := func(b []byte) uint64 {
		var v uint64
		for i := 0; i < 8; i++ {
			v = v<<8 | uint64(b[i])
		}
		return v
	}
	var typ uint64
	switch {
	case le(hdr[8:16]) == magic:
		if uint32(le(hdr[0:8])) != 0 { // DRR_BEGIN
			return false, fmt.Errorf("%w: el stream no empieza por DRR_BEGIN", ErrInvalidInput)
		}
		typ = le(hdr[32:40]) & 0xffffffff
	case be(hdr[8:16]) == magic:
		if be(hdr[0:8])>>32 != 0 {
			return false, fmt.Errorf("%w: el stream no empieza por DRR_BEGIN", ErrInvalidInput)
		}
		typ = be(hdr[32:40]) >> 32
	default:
		return false, fmt.Errorf("%w: no es un stream de zfs send", ErrInvalidInput)
	}
	switch typ {
	case dmuOstZFS:
		return false, nil
	case dmuOstZvol:
		return true, nil
	}
	return false, fmt.Errorf("%w: tipo de dataset %d en el stream", ErrInvalidInput, typ)
}

// readMounted — the dataset's mounted property ("yes"/"no").
var readMounted = func(ctx context.Context, name string) (string, error) {
	out, err := executil.RunRead(ctx, 10*time.Second, "zfs", "get", "-H", "-o", "value", "mounted", name)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// readOrigin — the snapshot a clone came from ("-" for a dataset that is
// not a clone).
var readOrigin = func(ctx context.Context, name string) (string, error) {
	out, err := executil.RunRead(ctx, 10*time.Second, "zfs", "get", "-H", "-o", "value", "origin", name)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrHostUnknown, err)
	}
	return strings.TrimSpace(string(out)), nil
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
			if err := h.check(OpDatasetRemove, "", d); err != nil {
				return err
			}
			// Listed there is not mounted there: unmounted, the path is a
			// directory of whatever filesystem holds it, and -x would rewrite
			// that one instead.
			if m, err := readMounted(ctx, d); err != nil || m != "yes" {
				return fmt.Errorf("%w: %s no está montado en %s", ErrInvalidInput, d, mp)
			}
			return nil
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
