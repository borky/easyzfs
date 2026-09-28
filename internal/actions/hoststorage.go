// hoststorage.go — recognise the storage the host itself runs on, and keep
// EasyZFS's hands off it (analysis §19.6, §21).
//
// On a Proxmox host the pools exist before EasyZFS is installed: the
// installer made rpool with the running OS in rpool/ROOT/pve-1, and the
// admin made the data pools, registered as Proxmox storage holding VM and
// container disks. Nothing here depends on what EasyZFS created or has
// recorded; every decision is read from the live system each time:
//
//   - OS pools: pools with a ZFS dataset mounted at / or /boot (mountinfo);
//     rpool on Proxmox, rpool and bpool on Ubuntu;
//   - system datasets: in an OS pool, its root dataset, the root
//     filesystem's boot-environment container (rpool/ROOT) and everything in
//     it, anything mounted outside the pool's own tree (rpool/var-lib-vz at
//     /var/lib/vz) and every ancestor of such a mount;
//   - guest disks: Proxmox's volume names, (vm|base|subvol|basevol)-<id>-…,
//     on any pool;
//   - Proxmox storage: the datasets /etc/pve/storage.cfg names as zfspool
//     storage, those mounted at a dir storage's path, and any dataset holding
//     guest disks.
//
// The rules: an OS pool is view-only apart from maintenance (scrub, trim,
// clear, SMART, snapshots); a system dataset or a guest disk cannot be
// destroyed, renamed, unmounted, rolled back or changed; Proxmox storage
// cannot be destroyed, renamed, unmounted or given a property its guests
// would inherit (only compression, atime, recordsize, caching, logbias,
// snapdir), and a pool holding it cannot be exported or destroyed.
// Everything else, including replacing a failed disk in a data pool, stays
// available. When the host cannot be read, the guarded operations are
// refused: an unknown host is not a data pool.
package actions

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"easyzfs/internal/executil"
)

var (
	// ErrHostStorage — the target is storage the host runs on or Proxmox
	// manages. Mapped to 403 host_storage.
	ErrHostStorage = errors.New("almacenamiento del host")
	// ErrHostUnknown — the host could not be read, so whether the target is
	// its storage is unknown. Mapped to 409 host_unknown: it is not a claim
	// that the target is host storage.
	ErrHostUnknown = errors.New("no se pudo leer el host")
)

// Host kinds, as the API reports them (model.Pool.Host, model.Dataset.Host).
const (
	HostSystem  = "system"  // part of the running operating system
	HostGuest   = "guest"   // a Proxmox VM or container disk
	HostStorage = "storage" // Proxmox storage, or a pool holding it
)

// reGuestDisk — Proxmox's volume names (PVE::Storage::ZFSPoolPlugin parses
// them as (vm|base|subvol|basevol)-<vmid>-<rest>): VM and CT disks and
// templates, state files, cloud-init drives, fleecing images, and names
// given by hand to 'pvesm alloc'.
var reGuestDisk = regexp.MustCompile(`^(vm|base|subvol|basevol)-\d+-\S+$`)

// Test seams.
var (
	mountinfoPath   = "/proc/self/mountinfo"
	pveDir          = "/etc/pve"
	listAllDatasets = func(ctx context.Context) ([]byte, error) {
		return executil.RunRead(ctx, 15*time.Second, "zfs", "list", "-H", "-o", "name,type,mountpoint", "-t", "filesystem,volume")
	}
	// readStorageCfg — /etc/pve/storage.cfg is root:www-data 0640, so it is
	// read through sudo (pinned to exactly this file).
	readStorageCfg = func(ctx context.Context) ([]byte, error) {
		return executil.Run(ctx, 10*time.Second, "cat", "/etc/pve/storage.cfg")
	}
)

// HostView — what the host runs on, read at one moment.
type HostView struct {
	RootDataset string          // mounted at /; "" when the root filesystem is not ZFS
	OSPools     map[string]bool // pools with a dataset mounted at / or /boot
	PVE         bool
	// StorageRoots — dataset → Proxmox storage id (zfspool entries, and
	// datasets mounted at a dir storage's path).
	StorageRoots map[string]string
	// StorageUnknown — a Proxmox host whose storage.cfg could not be read.
	// Every top-level dataset is then treated as storage: guessing that one
	// is not would be the unsafe answer.
	StorageUnknown bool
	datasets       map[string]dsEntry
	guests         []string
}

type dsEntry struct{ typ, mountpoint string }

// LoadHostView reads the host now.
func LoadHostView(ctx context.Context) (*HostView, error) {
	root, osPools, err := osPoolsFromMountinfo()
	if err != nil {
		return nil, fmt.Errorf("leer los montajes del host: %w", err)
	}
	h := &HostView{RootDataset: root, OSPools: osPools, StorageRoots: map[string]string{}, datasets: map[string]dsEntry{}}
	out, err := listAllDatasets(ctx)
	if err != nil {
		return nil, fmt.Errorf("listar datasets: %w", err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		f := strings.Split(line, "\t")
		if len(f) < 3 {
			continue
		}
		h.datasets[f[0]] = dsEntry{typ: f[1], mountpoint: f[2]}
		if reGuestDisk.MatchString(path.Base(f[0])) {
			h.guests = append(h.guests, f[0])
		}
	}
	sort.Strings(h.guests)
	if st, err := os.Stat(pveDir); err == nil && st.IsDir() {
		h.PVE = true
		if cfg, err := readStorageCfg(ctx); err == nil {
			zfspools, dirs := parseStorageCfg(string(cfg))
			for ds, id := range zfspools {
				h.StorageRoots[ds] = id
			}
			for name, e := range h.datasets {
				if id, ok := dirs[path.Clean(e.mountpoint)]; ok && strings.HasPrefix(e.mountpoint, "/") {
					h.StorageRoots[name] = id
				}
			}
		} else {
			h.StorageUnknown = true
		}
	}
	return h, nil
}

// osPoolsFromMountinfo — the ZFS dataset mounted at /, and the pools with a
// ZFS dataset mounted at / or /boot. mountinfo: field 5 is the mount point;
// after the " - " separator come fstype and source.
func osPoolsFromMountinfo() (root string, pools map[string]bool, err error) {
	f, err := os.Open(mountinfoPath)
	if err != nil {
		return "", nil, err
	}
	defer f.Close()
	top := map[string]string{} // mount point → ZFS source on top ("" = not ZFS)
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	for sc.Scan() {
		pre, post, ok := strings.Cut(sc.Text(), " - ")
		if !ok {
			continue
		}
		fields, tail := strings.Fields(pre), strings.Fields(post)
		if len(fields) < 5 || len(tail) < 2 || (fields[4] != "/" && fields[4] != "/boot") {
			continue
		}
		// The last mount at a point wins, as it is the one on top.
		if tail[0] == "zfs" {
			top[fields[4]] = tail[1]
		} else {
			top[fields[4]] = ""
		}
	}
	if err := sc.Err(); err != nil {
		return "", nil, err
	}
	pools = map[string]bool{}
	for _, src := range top {
		if src != "" {
			p, _, _ := strings.Cut(src, "/")
			pools[p] = true
		}
	}
	return top["/"], pools, nil
}

// parseStorageCfg — a Proxmox storage.cfg: zfspool dataset → storage id, and
// dir storage path → storage id. Options are "key value" separated by any
// whitespace, as Proxmox's own parser reads them.
func parseStorageCfg(cfg string) (zfspools, dirs map[string]string) {
	zfspools, dirs = map[string]string{}, map[string]string{}
	typ, id := "", ""
	for _, line := range strings.Split(cfg, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			// A commented-out section's options must not attach to the
			// section before it.
			if line[0] == '#' {
				typ, id = "", ""
			}
			continue
		}
		if line[0] != ' ' && line[0] != '\t' {
			typ, id = "", ""
			if t, name, ok := strings.Cut(line, ":"); ok {
				typ, id = strings.TrimSpace(t), strings.TrimSpace(name)
			}
			continue
		}
		f := strings.Fields(line)
		if len(f) < 2 || id == "" {
			continue
		}
		switch {
		case typ == "zfspool" && f[0] == "pool":
			zfspools[f[1]] = id
		case typ == "dir" && f[0] == "path":
			dirs[path.Clean(f[1])] = id
		}
	}
	return zfspools, dirs
}

func under(name, anc string) bool { return name == anc || strings.HasPrefix(name, anc+"/") }

// PoolKind — HostSystem for a pool the OS runs or boots from, HostStorage
// for a pool holding Proxmox storage or guest disks, "" otherwise, with the
// reason.
func (h *HostView) PoolKind(pool string) (string, string) {
	if pool == "" {
		return "", ""
	}
	if h.OSPools[pool] {
		if h.RootDataset != "" && under(h.RootDataset, pool) {
			return HostSystem, fmt.Sprintf("el pool %s contiene el sistema operativo en marcha (%s montado en /)", pool, h.RootDataset)
		}
		return HostSystem, fmt.Sprintf("el pool %s contiene el arranque del sistema (montado en /boot)", pool)
	}
	for _, ds := range sortedKeys(h.StorageRoots) {
		if under(ds, pool) {
			return HostStorage, fmt.Sprintf("el pool %s contiene el almacenamiento de Proxmox «%s» (%s)", pool, h.StorageRoots[ds], ds)
		}
	}
	for _, g := range h.guests {
		if under(g, pool) {
			return HostStorage, fmt.Sprintf("el pool %s contiene discos de máquinas virtuales o contenedores de Proxmox (%s…)", pool, g)
		}
	}
	if h.PVE && h.StorageUnknown {
		return HostStorage, fmt.Sprintf("no se pudo leer /etc/pve/storage.cfg: el pool %s podría ser almacenamiento de Proxmox", pool)
	}
	return "", ""
}

// DatasetKind — how name (a dataset, or a snapshot "ds@snap") belongs to the
// host, with the reason; "" when it is ordinary data. It works on names
// that do not exist yet too (the target of a create, clone or rename).
func (h *HostView) DatasetKind(name string) (string, string) {
	name, _, _ = strings.Cut(name, "@")
	pool, _, _ := strings.Cut(name, "/")
	if reGuestDisk.MatchString(path.Base(name)) {
		return HostGuest, fmt.Sprintf("%s es un disco de una máquina virtual o contenedor de Proxmox", name)
	}
	if h.OSPools[pool] {
		if name == pool {
			return HostSystem, fmt.Sprintf("%s es el dataset raíz de un pool del sistema: lo que se cambie en él lo heredan el sistema operativo y todo lo demás", name)
		}
		// The boot-environment container (rpool/ROOT) and all it holds,
		// and the root filesystem's own ancestors.
		if h.RootDataset != "" && under(h.RootDataset, pool) {
			if bootEnv := path.Dir(h.RootDataset); bootEnv != pool && under(name, bootEnv) {
				return HostSystem, fmt.Sprintf("%s forma parte del sistema operativo en marcha (%s)", name, h.RootDataset)
			}
			if under(h.RootDataset, name) || under(name, h.RootDataset) {
				return HostSystem, fmt.Sprintf("%s forma parte del sistema operativo en marcha (%s)", name, h.RootDataset)
			}
		}
		// Mounted outside the pool's own tree (host storage such as
		// /var/lib/vz): the dataset, what is below it, and its ancestors,
		// which a recursive destroy or a rename would take along.
		for d := name; d != pool && d != "."; d = path.Dir(d) {
			if e, ok := h.datasets[d]; ok && outsidePoolTree(e.mountpoint, pool) {
				return HostSystem, fmt.Sprintf("%s está montado en %s, fuera del pool: es almacenamiento del sistema", d, e.mountpoint)
			}
		}
		for _, d := range sortedKeys(h.datasets) {
			if strings.HasPrefix(d, name+"/") && outsidePoolTree(h.datasets[d].mountpoint, pool) {
				return HostSystem, fmt.Sprintf("%s contiene %s, montado en %s: es almacenamiento del sistema", name, d, h.datasets[d].mountpoint)
			}
		}
	}
	if id, ok := h.StorageRoots[name]; ok {
		return HostStorage, fmt.Sprintf("%s es el almacenamiento de Proxmox «%s»", name, id)
	}
	for _, ds := range sortedKeys(h.StorageRoots) {
		if strings.HasPrefix(ds, name+"/") {
			return HostStorage, fmt.Sprintf("%s contiene el almacenamiento de Proxmox «%s» (%s)", name, h.StorageRoots[ds], ds)
		}
	}
	for _, g := range h.guests {
		if strings.HasPrefix(g, name+"/") {
			return HostStorage, fmt.Sprintf("%s contiene discos de máquinas virtuales o contenedores de Proxmox (%s…)", name, g)
		}
	}
	if h.PVE && h.StorageUnknown && strings.Count(name, "/") == 1 {
		return HostStorage, fmt.Sprintf("no se pudo leer /etc/pve/storage.cfg: %s podría ser almacenamiento de Proxmox", name)
	}
	return "", ""
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// outsidePoolTree — a real mountpoint that is not /<pool> or below it.
func outsidePoolTree(mp, pool string) bool {
	if !strings.HasPrefix(mp, "/") {
		return false // none, legacy, - (volumes)
	}
	mp = path.Clean(mp)
	return mp != "/"+pool && !strings.HasPrefix(mp, "/"+pool+"/")
}

// HostOp — what an action is about to do, for guardHost.
type HostOp int

const (
	OpPoolRemove    HostOp = iota // export or destroy the pool
	OpPoolLayout                  // add, replace, detach, offline, expand, checkpoints
	OpDatasetRemove               // destroy, trash, rename, promote, rewrite
	OpDatasetUnmount
	// OpDatasetChange — a property with no effect on what runs on the data:
	// compression, atime, recordsize, caching, logbias, snapdir.
	OpDatasetChange
	// OpDatasetSensitive — anything a guest would feel through inheritance
	// (exec, devices, setuid, sync, quota, acltype, mountpoint, readonly…)
	// and encryption keys.
	OpDatasetSensitive
	OpRollback
	OpSnapshotDestroy
	OpSnapshotCreate
)

// allowed — whether op may touch something of kind.
func allowed(op HostOp, kind string) bool {
	if kind == "" {
		return true
	}
	switch op {
	case OpPoolRemove, OpDatasetRemove, OpDatasetUnmount, OpDatasetSensitive:
		return false
	case OpPoolLayout:
		// An OS pool's disks carry boot partitions that 'zpool replace' or
		// 'add' do not set up (proxmox-boot-tool does): a data pool that
		// merely holds Proxmox storage keeps full disk management.
		return kind != HostSystem
	case OpDatasetChange, OpRollback:
		return kind == HostStorage
	case OpSnapshotDestroy, OpSnapshotCreate:
		// Proxmox keeps its own snapshot list for guest disks, and 'qm
		// rollback' refuses when a newer, unknown snapshot exists.
		return kind != HostGuest
	}
	return false
}

// guardHost refuses op on a pool (pool != "") or dataset that belongs to the
// host. It reads the host now; if it cannot, it refuses.
func guardHost(ctx context.Context, op HostOp, pool, dataset string) error {
	if op == OpPoolLayout && dataset == "" {
		// Disk work on a pool is refused only on an OS pool, which mountinfo
		// alone decides: replacing a failed disk in a data pool must not
		// depend on listing every dataset of every pool succeeding.
		root, osPools, err := osPoolsFromMountinfo()
		if err != nil {
			return fmt.Errorf("%w: %v; no se hace nada", ErrHostUnknown, err)
		}
		return (&HostView{RootDataset: root, OSPools: osPools}).check(op, pool, "")
	}
	h, err := LoadHostView(ctx)
	if err != nil {
		return fmt.Errorf("%w: %v; no se hace nada", ErrHostUnknown, err)
	}
	return h.check(op, pool, dataset)
}

func (h *HostView) check(op HostOp, pool, dataset string) error {
	if pool != "" {
		if kind, why := h.PoolKind(pool); !allowed(op, kind) {
			return fmt.Errorf("%w: %s. %s", ErrHostStorage, why, hostHint(kind))
		}
	}
	if dataset != "" {
		if kind, why := h.DatasetKind(dataset); !allowed(op, kind) {
			return fmt.Errorf("%w: %s. %s", ErrHostStorage, why, hostHint(kind))
		}
	}
	return nil
}

func hostHint(kind string) string {
	switch kind {
	case HostSystem:
		return "EasyZFS solo lo muestra y hace mantenimiento (scrub, SMART, snapshots); estos cambios, desde la consola de Proxmox"
	case HostGuest, HostStorage:
		return "Hazlo desde la interfaz de Proxmox, que sabe qué VM o contenedor lo usa"
	}
	return ""
}

// guardSnapshotCreate — no snapshot of a guest disk, and no recursive one that
// would take guest disks with it: Proxmox tracks its own snapshots of them,
// and 'qm rollback' refuses while a newer snapshot it does not know exists.
// Scheduled jobs use SnapshotTreeSkipping instead.
func guardSnapshotCreate(ctx context.Context, dataset string, recursive bool) error {
	h, err := LoadHostView(ctx)
	if err != nil {
		return fmt.Errorf("%w: %v; no se hace nada", ErrHostUnknown, err)
	}
	if err := h.check(OpSnapshotCreate, "", dataset); err != nil {
		return err
	}
	if recursive {
		for _, g := range h.guests {
			if strings.HasPrefix(g, dataset+"/") {
				return fmt.Errorf("%w: el snapshot recursivo de %s incluiría discos de Proxmox (%s…), y Proxmox no podría volver a sus propios snapshots de esas VMs. Haz snapshots de los datasets de datos, o de las VMs desde Proxmox", ErrHostStorage, dataset, g)
			}
		}
	}
	return nil
}

// guardNewName — the name a create, clone or rename would give a dataset
// must not put it into the running OS's tree or under host storage mounted
// outside its pool, nor be a name Proxmox would take for one of its disks
// (it would adopt the dataset, and "remove unused disks" would destroy it).
func guardNewName(ctx context.Context, newName string) error {
	h, err := LoadHostView(ctx)
	if err != nil {
		return fmt.Errorf("%w: %v; no se hace nada", ErrHostUnknown, err)
	}
	if reGuestDisk.MatchString(path.Base(newName)) {
		return fmt.Errorf("%w: %s tiene nombre de disco de Proxmox (vm-/base-/subvol-/basevol-<id>-…); Proxmox lo tomaría por suyo", ErrHostStorage, newName)
	}
	pool, _, _ := strings.Cut(newName, "/")
	if h.OSPools[pool] && newName != pool {
		// Everything below the new name's parent that is system: the boot
		// environments, or a dataset mounted outside the pool.
		if kind, why := h.DatasetKind(path.Dir(newName)); kind == HostSystem && path.Dir(newName) != pool {
			return fmt.Errorf("%w: el destino %s está dentro de almacenamiento del sistema: %s", ErrHostStorage, newName, why)
		}
	}
	return nil
}

// CheckHostDataset — for callers outside this package that act on a dataset
// themselves: the rewrite handler (it launches 'zfs rewrite' as a long
// operation) and the replication runner (a destination it may destroy with
// force_full). Same rules and same fail-closed read as the actions.
func CheckHostDataset(ctx context.Context, op HostOp, dataset string) error {
	return guardHost(ctx, op, "", dataset)
}

// LogIfStorageUnknown — said once per process, so the admin learns why every
// top-level dataset of a Proxmox host shows as storage.
var storageUnknownOnce sync.Once

func (h *HostView) LogIfStorageUnknown() {
	if h != nil && h.PVE && h.StorageUnknown {
		storageUnknownOnce.Do(func() {
			log.Printf("hoststorage: no se pudo leer /etc/pve/storage.cfg (¿falta la regla de sudoers? ejecuta 'make update'); todos los datasets de primer nivel se tratan como almacenamiento de Proxmox")
		})
	}
}

// SnapshotTree — what a scheduled snapshot job takes: dataset and everything
// below it, as 'zfs snapshot -r' would, minus Proxmox guest disks, in one
// atomic 'zfs snapshot a@x b@x …'. A job over a Proxmox storage pool used
// to sweep up the VM disks (blocking qm rollback); refusing it outright
// would instead stop the admin's own datasets being snapshotted at all.
// Returns the guest disks left out.
func (s *Service) SnapshotTree(ctx context.Context, actor, dataset, name string) (skipped []string, err error) {
	if !reDataset.MatchString(dataset) || !reSnapName.MatchString(name) {
		return nil, ErrInvalidName
	}
	h, err := LoadHostView(ctx)
	if err != nil {
		return nil, fmt.Errorf("%w: %v; no se hace nada", ErrHostUnknown, err)
	}
	if err := h.check(OpSnapshotCreate, "", dataset); err != nil {
		return nil, err
	}
	var targets []string
	for _, d := range sortedKeys(h.datasets) {
		if !under(d, dataset) {
			continue
		}
		guest := false
		for _, g := range h.guests {
			if under(d, g) {
				guest = true
				break
			}
		}
		if guest {
			skipped = append(skipped, d)
			continue
		}
		targets = append(targets, d+"@"+name)
	}
	args := []string{"snapshot", "-r", dataset + "@" + name}
	if len(skipped) > 0 {
		if len(targets) == 0 {
			return skipped, nil
		}
		args = append([]string{"snapshot"}, targets...)
	}
	s.audit(ctx, actor, "snapshot.create", dataset+"@"+name,
		map[string]any{"recursive": true, "skipped_guest_disks": skipped}, false)
	if err := runZFSSnap(ctx, args...); err != nil {
		return skipped, fmt.Errorf("crear snapshot: %w", err)
	}
	return skipped, nil
}

// runZFSSnap — test seam for SnapshotTree's one command.
var runZFSSnap = func(ctx context.Context, args ...string) error {
	_, err := executil.Run(ctx, 60*time.Second, "zfs", args...)
	return err
}
