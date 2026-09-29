// mountpoint.go — the effective mountpoint check (§1 of the security review).
//
// checkMountpoint in props.go guards a mountpoint the app is asked to *set*.
// A dataset also gets one without anybody setting it: inherited from its
// parent, carried in a received stream, or derived from the pool's name. That
// path is what ZFS mounts on — at creation, and at every import afterwards —
// and until this file existed nothing looked at it. On the Proxmox VE test VM
// (root dataset rpool/ROOT/pve-1 mounted at /) creating a dataset whose
// inherited mountpoint resolved into /etc left the host with no network after
// its next reboot. Since 2145b2c the service has no private mount namespace
// either, so such a mount reaches the host immediately as well.
//
// The rules, applied to the path ZFS would actually use:
//
//   - a system path is refused, always (deniedMountpoint);
//   - a path that cannot be reached safely is refused, always
//     (checkMountpointPath with trustDirs=false: no existing component on the
//     way may be a symlink, and one that cannot be inspected is refused too).
//     Root ownership of every directory on the way is *not* demanded, unlike
//     for a mountpoint somebody asks for: see checkMountpointPath. What that
//     gives up is the race — somebody who can write to a directory on the way
//     can still swap the next component for a symlink between this check and
//     root's mount;
//   - a *derived* path must additionally sit inside a tree somebody already
//     chose — the pool's own tree, /mnt, /media, /srv, /home, or the
//     mountpoint of the nearest ancestor that has one recorded — and may not be
//     /mnt, /media, /srv or /home exactly, which would hide everything below
//     (derivedExactMountpoints; a pool named "home" lands there, and rePool
//     allows the name). Placing a dataset somewhere is the app's own decision,
//     so the app answers for it.
//
// A path *recorded* on the dataset itself (source local or received) skips
// that last rule: it is already what ZFS will mount at, EasyZFS running or
// not, and refusing to be the one to trigger it would not un-record it. What
// would shadow the system is still refused. "Recorded" and "the allowlist does
// not apply" are the same bit throughout: checkTarget takes it as one argument
// and every caller derives it from mountTarget.recorded.
//
// An ancestor mounted at "/" grants nothing, exactly as poolTrees ignores a
// pool root at "/": it would put the whole filesystem inside the allowlist,
// which is how /etc became a legal place for a dataset in the first place.
//
// The source strings are ZFS's own; internal/actions/testdata holds real
// 'zfs get' output from the VM for each of them.
package actions

import (
	"context"
	"errors"
	"fmt"
	"log"
	"path"
	"sort"
	"strings"
	"time"

	"easyzfs/internal/executil"
)

// srcInheritedFrom — the prefix ZFS uses for an inherited property's source
// ("inherited from rpool/data/ezsrc").
const srcInheritedFrom = "inherited from "

// recordedSource — the mountpoint is set on the dataset itself rather than
// derived from somewhere else. "received" counts: a received value overrides
// inheritance and persists exactly like a local one.
func recordedSource(src string) bool { return src == "local" || src == "received" }

// mountsNothing — a mountpoint value that puts nothing on the filesystem.
// "-" is what volumes and snapshots report.
func mountsNothing(v string) bool {
	return v == "" || v == "-" || v == "none" || v == "legacy"
}

// ErrNoSuchDataset — zfs says the dataset is not there. Told apart from every
// other read failure on purpose: resolving a dataset that does not exist yet
// means walking up to its nearest existing ancestor, and a read that failed
// for any other reason must not be mistaken for that. Substituting the
// inherited path for the recorded one is the "intended state instead of
// observed state" trap, and here it would hand a mount over /etc a pass.
var ErrNoSuchDataset = errors.New("the dataset does not exist")

// readMountpointProp — mountpoint value and source of one dataset, read live.
// RunRead needs no sudo: 'zfs get' works unprivileged on Debian/Proxmox, and
// read-only mode grants no sudo for it. A test seam.
var readMountpointProp = func(ctx context.Context, dataset string) (value, source string, err error) {
	out, err := executil.RunRead(ctx, 10*time.Second, "zfs", "get", "-H", "-o", "value,source", "mountpoint", dataset)
	if err != nil {
		return "", "", classifyReadErr(err)
	}
	// value first, source last, tab-separated. Split on the *last* tab: the
	// source never contains one, a mountpoint legally can.
	line := strings.TrimRight(string(out), "\n")
	i := strings.LastIndex(line, "\t")
	if i < 0 {
		return "", "", fmt.Errorf("zfs get mountpoint %s: unexpected answer", dataset)
	}
	return line[:i], line[i+1:], nil
}

// classifyReadErr marks the one failure that means "look one level up": zfs
// saying the dataset is not there. Its wording reaches us on stderr, which
// executil puts into the error: "zfs: cannot open 'tank/x': dataset does not
// exist" (checked on the VM). Nothing else zfs or sudo says contains that
// phrase, and a timeout says "tras 10s". If OpenZFS ever rewords it, every
// resolve of a dataset that does not exist yet fails closed — a refused create
// with a puzzling message, never a mount that should not have happened.
func classifyReadErr(err error) error {
	if err != nil && zfsNoDataset(err) {
		return fmt.Errorf("%w: %v", ErrNoSuchDataset, err)
	}
	return err
}

// zfsNoDataset — zfs's own "cannot open 'x': dataset does not exist", told
// apart from any message of ours: now that those are English too, "the
// dataset does not exist" (ErrNoSuchDataset) or a gateway refusal quoting it
// must not read as "zfs says it is gone" — the purge would then drop a
// recycle-bin entry that was never destroyed.
func zfsNoDataset(err error) bool {
	return err != nil && strings.Contains(err.Error(), "': dataset does not exist")
}

// parentDataset — the dataset one level up, and false at a pool's root.
func parentDataset(name string) (string, bool) {
	i := strings.LastIndex(name, "/")
	if i <= 0 {
		return "", false
	}
	return name[:i], true
}

// mountTarget — where ZFS would put a dataset, and what allows that place.
type mountTarget struct {
	// path — the effective mountpoint, cleaned; "" when nothing is mounted
	// (a volume, none, legacy).
	path string
	// recorded — the path is set on the dataset itself (local or received),
	// not derived from an ancestor or from the pool's name.
	recorded bool
	// grant — the mountpoint of the nearest ancestor that has one recorded,
	// i.e. the tree this path was derived from. "" means there is no such
	// ancestor, so the value comes from the pool's name and poolTrees covers
	// it; it is also "" when recorded is true, where it has no meaning.
	grant string
}

// inheritFrom — the target a child gets by inheriting from base: base's path
// with the child's relative name appended, which is what ZFS inheritance
// produces. Base's own place becomes the tree the child was derived from.
func inheritFrom(base mountTarget, rel string) mountTarget {
	grant := base.grant
	if base.recorded {
		grant = base.path
	}
	return mountTarget{path: path.Join(base.path, rel), grant: grant}
}

// mountResolver resolves effective mountpoints, remembering what it read.
// A whole tree usually inherits from the same two or three ancestors, and a
// pool import would otherwise read every dataset's ancestors again per
// dataset. Create one per operation and throw it away: the cache must never
// outlive the mutation it is checking, or the next check reports the state
// before it.
type mountResolver struct {
	props map[string][2]string // dataset → {value, source}; {} = does not exist
	trees map[string][]string  // pool → poolTrees
}

func newMountResolver() *mountResolver {
	return &mountResolver{props: map[string][2]string{}, trees: map[string][]string{}}
}

// seed records a value and source already read, so a tree walk that read them
// all at once does not read them again.
func (r *mountResolver) seed(dataset, value, source string) {
	r.props[dataset] = [2]string{value, source}
}

// read — the dataset's mountpoint value and source, memoised. Only a dataset
// zfs says is absent is remembered as absent: resolving a tree walks up
// through the same missing names repeatedly. Any other failure is returned as
// it came and not cached, so a retry can still get the real answer.
func (r *mountResolver) read(ctx context.Context, dataset string) (value, source string, err error) {
	if p, ok := r.props[dataset]; ok {
		if p[1] == "" {
			return "", "", fmt.Errorf("%w: %s", ErrNoSuchDataset, dataset)
		}
		return p[0], p[1], nil
	}
	value, source, err = readMountpointProp(ctx, dataset)
	if err != nil {
		if errors.Is(err, ErrNoSuchDataset) {
			r.props[dataset] = [2]string{}
		}
		return "", "", err
	}
	if source == "" {
		// A line with no source is not an answer about where this mounts.
		return "", "", fmt.Errorf("zfs get mountpoint %s: no source", dataset)
	}
	r.props[dataset] = [2]string{value, source}
	return value, source, nil
}

// pool trees, memoised (poolTrees asks zfs where the pool's root is).
func (r *mountResolver) poolTreesOf(ctx context.Context, pool string) []string {
	if t, ok := r.trees[pool]; ok {
		return t
	}
	t := poolTrees(ctx, pool)
	r.trees[pool] = t
	return t
}

// targetFrom builds the target of a dataset whose value and source are known.
func (r *mountResolver) targetFrom(ctx context.Context, dataset, value, source string) mountTarget {
	t := mountTarget{path: path.Clean(value), recorded: recordedSource(source)}
	if t.recorded {
		return t
	}
	// "inherited from X" names the dataset where the value is actually set,
	// so X's own value is the tree this one was derived from, and one read is
	// enough — ZFS never reports "inherited from X" for an X that is itself
	// inheriting or defaulting (the PVE fixture shows rpool/ROOT as "default",
	// not as inherited from rpool). If X turns out not to be recorded anyway,
	// the grant is dropped, which only makes the check stricter.
	// "default" names nothing: the value comes from the pool's name.
	from := strings.TrimPrefix(source, srcInheritedFrom)
	if from == source || from == "" {
		return t
	}
	v, s, err := r.read(ctx, from)
	if err != nil || !recordedSource(s) || mountsNothing(v) || !strings.HasPrefix(v, "/") {
		return t // no usable grant: only the pool's own trees will do
	}
	t.grant = path.Clean(v)
	return t
}

// target — where ZFS would mount dataset. The dataset need not exist: one
// about to be created, cloned or renamed into place lands at the nearest
// existing ancestor's mountpoint with its relative name appended, which is
// exactly what inheritance gives it. A volume, none or legacy comes back with
// an empty path: nothing is mounted, so there is nothing to check.
func (r *mountResolver) target(ctx context.Context, dataset string) (mountTarget, error) {
	cur := dataset
	var value, source string
	for {
		v, s, err := r.read(ctx, cur)
		if err == nil {
			value, source = v, s
			break
		}
		parent, ok := parentDataset(cur)
		// Only "does not exist" means look one level up. Anything else — a
		// timeout, a suspended pool, sudo refusing — leaves us not knowing
		// where this dataset would mount, and an unknown is not a pass.
		if !errors.Is(err, ErrNoSuchDataset) || !ok {
			return mountTarget{}, fmt.Errorf("%w: cannot read the mountpoint of %s: %v", ErrInvalidInput, cur, err)
		}
		cur = parent
	}
	if mountsNothing(value) {
		return mountTarget{}, nil
	}
	if !strings.HasPrefix(value, "/") {
		return mountTarget{}, fmt.Errorf("%w: unreadable mountpoint of %s (%q)", ErrInvalidInput, cur, value)
	}
	t := r.targetFrom(ctx, cur, value, source)
	if cur != dataset {
		t = inheritFrom(t, dataset[len(cur)+1:])
	}
	return t, nil
}

// inherited — where dataset would land if it had no mountpoint of its own,
// which is what 'zfs inherit mountpoint' leaves it with (and zfs remounts it
// there straight away). A pool's root dataset inherits nothing.
func (r *mountResolver) inherited(ctx context.Context, dataset string) (mountTarget, error) {
	parent, ok := parentDataset(dataset)
	if !ok {
		// A pool's root dataset has no parent, but inheriting is not a no-op
		// there either: it resets the value to the default /<pool>. rePool
		// allows "etc" and "home", and an imported foreign pool can be called
		// either, so this is checked like any other derived path.
		return mountTarget{path: "/" + dataset}, nil
	}
	t, err := r.target(ctx, parent)
	if err != nil || t.path == "" {
		return mountTarget{}, err
	}
	return inheritFrom(t, dataset[len(parent)+1:]), nil
}

// checkTarget applies the rules to a resolved target. allowlist=false drops the
// "inside a tree somebody chose" half and keeps the two that always hold, for a
// recorded path or an operation that does not choose where the dataset goes.
func (r *mountResolver) checkTarget(ctx context.Context, dataset string, t mountTarget, allowlist bool) error {
	if t.path == "" {
		return nil
	}
	if deniedMountpoint(t.path) {
		return fmt.Errorf("%w: %s would be mounted on a system path (%s); that leaves the machine unable to boot or without network",
			ErrInvalidInput, dataset, t.path)
	}
	if allowlist {
		for _, e := range derivedExactMountpoints {
			if t.path == e {
				return fmt.Errorf("%w: %s would be mounted over %s, hiding everything underneath",
					ErrInvalidInput, dataset, t.path)
			}
		}
		pool, _, _ := strings.Cut(dataset, "/")
		trees := r.poolTreesOf(ctx, pool)
		// An ancestor at "/" grants nothing: see the file comment.
		if t.grant != "" && t.grant != "/" {
			trees = append(append([]string(nil), trees...), t.grant)
		}
		if !underAllowedRoot(t.path, trees) {
			return fmt.Errorf("%w: %s would inherit the mountpoint %s, outside the allowed paths (%s, /mnt/…, /media/…, /srv/…, /home/…); set an explicit mountpoint or choose another parent",
				ErrInvalidInput, dataset, t.path, strings.Join(trees, ", "))
		}
	}
	// Symlinks only, never root ownership: the directories on the way here are
	// the pool's own dataset mountpoints, and a share directory chowned to its
	// users or left group-writable is the ordinary setup. See
	// checkMountpointPath for what that gives up.
	if err := checkMountpointPath("/", t.path, false); err != nil {
		return fmt.Errorf("%w: %s cannot be mounted at %s: %v", ErrInvalidInput, dataset, t.path, err)
	}
	return nil
}

// check — the full rules for where ZFS would mount dataset.
func (r *mountResolver) check(ctx context.Context, dataset string) error {
	t, err := r.target(ctx, dataset)
	if err != nil {
		return err
	}
	return r.checkTarget(ctx, dataset, t, !t.recorded)
}

// checkEffectiveMountpoint — the single-dataset entry point: refuse the place
// ZFS would mount dataset if it shadows the system, cannot be reached safely,
// or (for a place the app is deriving) falls outside the allowed trees.
// dataset may be one that does not exist yet.
func checkEffectiveMountpoint(ctx context.Context, dataset string) error {
	return newMountResolver().check(ctx, dataset)
}

// checkEffectiveMountpointTree — the same check for a dataset about to be
// created with 'zfs create -p', which also creates every missing ancestor and
// mounts it. The ancestors' paths are above the leaf's, and deniedMountpoint is
// not monotone (it refuses an *ancestor* of a protected root too), so a leaf at
// /var/lib/b can pass while the /var/lib that -p creates on the way shadows
// /var/lib/dpkg and EasyZFS's own data directory.
func checkEffectiveMountpointTree(ctx context.Context, dataset string) error {
	r := newMountResolver()
	// Find where the existing part ends; everything below it is about to be
	// created. The resolver's cache makes this one read per level at most.
	var missing []string
	for cur := dataset; ; {
		if _, _, err := r.read(ctx, cur); err == nil {
			break
		} else if !errors.Is(err, ErrNoSuchDataset) {
			return fmt.Errorf("%w: cannot read the mountpoint of %s: %v", ErrInvalidInput, cur, err)
		}
		missing = append(missing, cur)
		parent, ok := parentDataset(cur)
		if !ok {
			break
		}
		cur = parent
	}
	if len(missing) == 0 {
		return r.check(ctx, dataset)
	}
	for _, ds := range missing {
		if err := r.check(ctx, ds); err != nil {
			return err
		}
	}
	return nil
}

// CheckEffectiveMountpoint — the same check for callers outside this package.
// internal/replication uses it on a local receive destination before it starts
// a stream: a received stream carries mountpoints.
func CheckEffectiveMountpoint(ctx context.Context, dataset string) error {
	return checkEffectiveMountpoint(ctx, dataset)
}

// checkMountDanger — only the rules that always hold, for an operation that
// mounts nothing itself and does not choose the place, but would make the
// dataset's arrangement permanent (promote). A dataset already sitting on a
// path that a later import would mount over /etc is still refused.
func checkMountDanger(ctx context.Context, dataset string) error {
	r := newMountResolver()
	t, err := r.target(ctx, dataset)
	if err != nil {
		return err
	}
	return r.checkTarget(ctx, dataset, t, false)
}

// checkInheritedMountpoint — where dataset lands once its own mountpoint is
// dropped ('zfs inherit mountpoint'). Dropping it is the app choosing a new
// place, so the allowlist applies.
func checkInheritedMountpoint(ctx context.Context, dataset string) error {
	r := newMountResolver()
	t, err := r.inherited(ctx, dataset)
	if err != nil {
		return err
	}
	return r.checkTarget(ctx, dataset, t, true)
}

// checkRenameMount — where the dataset ends up after a rename. ZFS remounts
// on rename, and a mountpoint recorded on the dataset travels with it
// unchanged, while a derived one becomes the new parent's path plus the new
// relative name. Descendants with their own recorded mountpoints are not
// touched by the rename; descendants that inherit land below the path checked
// here.
func checkRenameMount(ctx context.Context, oldName, newName string) error {
	r := newMountResolver()
	t, err := r.target(ctx, oldName)
	if err != nil {
		return err
	}
	if t.path == "" {
		return nil // nothing is mounted either side of the rename
	}
	if t.recorded {
		return r.checkTarget(ctx, newName, t, false)
	}
	nt, err := r.target(ctx, newName)
	if err != nil {
		return err
	}
	return r.checkTarget(ctx, newName, nt, true)
}

// mountTree mounts everything under root that an import would have mounted,
// and refuses to mount what the rules above reject. It is the work 'zpool
// import -N' leaves undone and the work restoring from the recycle bin has to
// redo, so both go through here. The returned strings are what it would not
// or could not mount; the caller decides whether that is an error.
func (s *Service) mountTree(ctx context.Context, root string) []string {
	// -t filesystem alone fails outright on a volume ("not applicable");
	// volumes come back with mountpoint "-" and are skipped below.
	// sharenfs/sharesmb: 'zpool import' mounts *and* shares (libzfs
	// zpool_enable_datasets), while 'zfs mount' only mounts, so -N would
	// silently drop a NAS's exports until the next boot.
	rows, err := s.zfsGetRows(ctx, "-r", "-t", "filesystem,volume",
		"canmount,mountpoint,keystatus,sharenfs,sharesmb", root)
	if err != nil {
		return []string{fmt.Sprintf("could not read the tree to mount it: %v", err)}
	}
	byName := map[string]map[string]string{}
	source := map[string]string{}
	var names []string
	for _, w := range rows {
		if byName[w.name] == nil {
			byName[w.name] = map[string]string{}
			names = append(names, w.name)
		}
		byName[w.name][w.prop] = w.value
		if w.prop == "mountpoint" {
			source[w.name] = w.source
		}
	}
	r := newMountResolver()
	for n, p := range byName {
		if src := source[n]; src != "" {
			r.seed(n, p["mountpoint"], src)
		}
	}
	// Order by mountpoint, which is what 'zpool import' does (libzfs sorts
	// zfs_foreach_mountpoint by mountpoint, not by dataset name) and what
	// nesting actually depends on: tank/a at /mnt/d and tank/bbbb at /mnt/d/s
	// nest the other way round from their names, and mounting the child first
	// leaves it hidden under the parent.
	sort.Slice(names, func(i, j int) bool {
		return byName[names[i]]["mountpoint"] < byName[names[j]]["mountpoint"]
	})
	var warnings []string
	for _, n := range names {
		p := byName[n]
		if p["canmount"] != "on" || mountsNothing(p["mountpoint"]) || p["keystatus"] == "unavailable" {
			continue
		}
		if err := r.check(ctx, n); err != nil {
			// Logged as well as returned: the HTTP response is read once, and
			// a dataset deliberately left unmounted is a state somebody will
			// have to explain days later.
			log.Printf("mountpoint: %s is not mounted: %v", n, err)
			warnings = append(warnings, fmt.Sprintf("%s is not mounted: %v", n, err))
			continue
		}
		if _, err := runZFS(ctx, 60*time.Second, "mount", n); err != nil && !strings.Contains(err.Error(), "already mounted") {
			warnings = append(warnings, fmt.Sprintf("mount %s: %v", n, err))
			continue
		}
		// Put back what 'zfs mount' does not: the NFS/SMB export the dataset
		// asks for. Only a failure to share a dataset that wants to be shared
		// is worth reporting.
		if shared(p["sharenfs"]) || shared(p["sharesmb"]) {
			if _, err := runZFS(ctx, 30*time.Second, "share", n); err != nil &&
				!strings.Contains(err.Error(), "already shared") {
				warnings = append(warnings, "compartir "+n+": "+err.Error())
			}
		}
	}
	return warnings
}

// shared — the dataset asks to be exported over NFS or SMB. "off" and "-" do
// not; anything else is a share definition ("on", "rw=…", "name=…").
func shared(v string) bool { return v != "" && v != "off" && v != "-" }
