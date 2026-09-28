// hoststorage.go — recognise the storage the host itself runs on, and keep
// EasyZFS's hands off it (analysis §19.6, §21).
//
// On a Proxmox host the pools exist before EasyZFS is installed: the
// installer made rpool with the running OS in rpool/ROOT/pve-1, and the
// admin made the data pools, registered as Proxmox storage holding VM and
// container disks. Nothing here depends on what EasyZFS created or has
// recorded; every decision is read from the live system each time:
//
//   - the host pool: the pool whose dataset is mounted at / (mountinfo);
//   - system datasets: in that pool, its root dataset, the root filesystem's
//     boot-environment container (rpool/ROOT) and everything in it, and any
//     dataset mounted outside the pool's own tree (rpool/var-lib-vz at
//     /var/lib/vz);
//   - guest disks: Proxmox's own names (vm-100-disk-0, base-…, subvol-…,
//     vm-…-state-…, vm-…-cloudinit), on any pool;
//   - Proxmox storage: the datasets /etc/pve/storage.cfg names as zfspool
//     storage, and any dataset holding guest disks.
//
// The rules: the host pool is view-only apart from maintenance (scrub, trim,
// clear, SMART, snapshots); a system dataset or a guest disk cannot be
// destroyed, renamed, unmounted, rolled back or changed; Proxmox storage
// cannot be destroyed, renamed, unmounted or have its mountpoint changed,
// and a pool holding it cannot be exported or destroyed. Everything else,
// including replacing a failed disk in a data pool, stays available. When
// the host cannot be read, the guarded operations are refused: an unknown
// host is not a data pool.
package actions

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"regexp"
	"strings"
	"time"

	"easyzfs/internal/executil"
)

// ErrHostStorage — the target is storage the host runs on or Proxmox
// manages. Mapped to 403 host_storage.
var ErrHostStorage = errors.New("almacenamiento del host")

// Host kinds, as the API reports them (model.Pool.Host, model.Dataset.Host).
const (
	HostSystem  = "system"  // part of the running operating system
	HostGuest   = "guest"   // a Proxmox VM or container disk
	HostStorage = "storage" // Proxmox storage, or a pool holding it
)

// reGuestDisk — the names Proxmox gives the disks it manages
// (PVE::Storage::ZFSPoolPlugin): images, templates, container subvolumes,
// saved RAM state and cloud-init drives.
var reGuestDisk = regexp.MustCompile(`^(vm|base)-\d+-disk-\d+$|^subvol-\d+-disk-\d+$|^vm-\d+-state-.+$|^vm-\d+-cloudinit$`)

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
	RootDataset string // mounted at /; "" when the root filesystem is not ZFS
	HostPool    string // the pool of RootDataset
	PVE         bool
	// StorageRoots — dataset → Proxmox storage id (zfspool entries).
	StorageRoots map[string]string
	// StorageUnknown — a Proxmox host whose storage.cfg could not be read:
	// only the guest-disk names protect its storage then.
	StorageUnknown bool
	datasets       map[string]dsEntry
	guests         []string
}

type dsEntry struct{ typ, mountpoint string }

// LoadHostView reads the host now.
func LoadHostView(ctx context.Context) (*HostView, error) {
	h := &HostView{StorageRoots: map[string]string{}, datasets: map[string]dsEntry{}}
	root, err := zfsRootDataset()
	if err != nil {
		return nil, fmt.Errorf("leer los montajes del host: %w", err)
	}
	h.RootDataset = root
	h.HostPool, _, _ = strings.Cut(root, "/")
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
	if st, err := os.Stat(pveDir); err == nil && st.IsDir() {
		h.PVE = true
		if cfg, err := readStorageCfg(ctx); err == nil {
			h.StorageRoots = parseStorageCfg(string(cfg))
		} else {
			h.StorageUnknown = true
		}
	}
	return h, nil
}

// zfsRootDataset — the ZFS dataset mounted at /, from mountinfo (field 5 is
// the mount point, and after the " - " separator come fstype and source).
func zfsRootDataset() (string, error) {
	f, err := os.Open(mountinfoPath)
	if err != nil {
		return "", err
	}
	defer f.Close()
	root := ""
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	for sc.Scan() {
		pre, post, ok := strings.Cut(sc.Text(), " - ")
		if !ok {
			continue
		}
		fields, tail := strings.Fields(pre), strings.Fields(post)
		if len(fields) < 5 || len(tail) < 2 || fields[4] != "/" {
			continue
		}
		// The last mount at / wins, as it is the one on top.
		if tail[0] == "zfs" {
			root = tail[1]
		} else {
			root = ""
		}
	}
	return root, sc.Err()
}

// parseStorageCfg — the zfspool entries of a Proxmox storage.cfg: dataset →
// storage id.
func parseStorageCfg(cfg string) map[string]string {
	roots := map[string]string{}
	id := ""
	for _, line := range strings.Split(cfg, "\n") {
		if line == "" {
			continue
		}
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			// A commented-out section's options must not attach to the
			// section before it.
			if line[0] == '#' {
				id = ""
			}
			continue
		}
		if line[0] != ' ' && line[0] != '\t' {
			id = ""
			if typ, name, ok := strings.Cut(line, ":"); ok && strings.TrimSpace(typ) == "zfspool" {
				id = strings.TrimSpace(name)
			}
			continue
		}
		if id == "" {
			continue
		}
		if k, v, ok := strings.Cut(strings.TrimSpace(line), " "); ok && k == "pool" {
			roots[strings.TrimSpace(v)] = id
		}
	}
	return roots
}

func under(name, anc string) bool { return name == anc || strings.HasPrefix(name, anc+"/") }

// PoolKind — HostSystem for the pool the OS runs on, HostStorage for a pool
// holding Proxmox storage or guest disks, "" otherwise, with the reason.
func (h *HostView) PoolKind(pool string) (string, string) {
	if pool == "" {
		return "", ""
	}
	if pool == h.HostPool {
		return HostSystem, fmt.Sprintf("el pool %s contiene el sistema operativo en marcha (%s montado en /)", pool, h.RootDataset)
	}
	for ds, id := range h.StorageRoots {
		if under(ds, pool) {
			return HostStorage, fmt.Sprintf("el pool %s contiene el almacenamiento de Proxmox «%s» (%s)", pool, id, ds)
		}
	}
	for _, g := range h.guests {
		if under(g, pool) {
			return HostStorage, fmt.Sprintf("el pool %s contiene discos de máquinas virtuales o contenedores de Proxmox (%s…)", pool, g)
		}
	}
	return "", ""
}

// DatasetKind — how name (a dataset, or a snapshot "ds@snap") belongs to the
// host, with the reason; "" when it is ordinary data.
func (h *HostView) DatasetKind(name string) (string, string) {
	name, _, _ = strings.Cut(name, "@")
	pool, _, _ := strings.Cut(name, "/")
	if reGuestDisk.MatchString(path.Base(name)) {
		return HostGuest, fmt.Sprintf("%s es un disco de una máquina virtual o contenedor de Proxmox: gestiónalo desde Proxmox", name)
	}
	if h.HostPool != "" && pool == h.HostPool {
		if name == pool {
			return HostSystem, fmt.Sprintf("%s es el dataset raíz del pool del sistema: lo que se cambie en él lo heredan el sistema operativo y todo lo demás", name)
		}
		// The boot-environment container (rpool/ROOT) and all it holds,
		// and the root filesystem's own ancestors.
		if bootEnv := path.Dir(h.RootDataset); bootEnv != "." && bootEnv != pool && under(name, bootEnv) {
			return HostSystem, fmt.Sprintf("%s forma parte del sistema operativo en marcha (%s)", name, h.RootDataset)
		}
		if under(h.RootDataset, name) || under(name, h.RootDataset) {
			return HostSystem, fmt.Sprintf("%s forma parte del sistema operativo en marcha (%s)", name, h.RootDataset)
		}
		// Mounted outside the pool's own tree: host storage such as
		// /var/lib/vz. Checked on the dataset and each ancestor.
		for d := name; d != pool && d != "."; d = path.Dir(d) {
			if e, ok := h.datasets[d]; ok && outsidePoolTree(e.mountpoint, pool) {
				return HostSystem, fmt.Sprintf("%s está montado en %s, fuera del pool: es almacenamiento del sistema", d, e.mountpoint)
			}
		}
	}
	if id, ok := h.StorageRoots[name]; ok {
		return HostStorage, fmt.Sprintf("%s es el almacenamiento de Proxmox «%s»: los discos de las VMs y contenedores viven aquí", name, id)
	}
	for ds, id := range h.StorageRoots {
		if under(ds, name) {
			return HostStorage, fmt.Sprintf("%s contiene el almacenamiento de Proxmox «%s» (%s)", name, id, ds)
		}
	}
	for _, g := range h.guests {
		if strings.HasPrefix(g, name+"/") {
			return HostStorage, fmt.Sprintf("%s contiene discos de máquinas virtuales o contenedores de Proxmox (%s…)", name, g)
		}
	}
	return "", ""
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
	OpPoolLayout                  // add, replace, detach, offline, expand, create a checkpoint
	OpDatasetRemove               // destroy, trash, rename, promote, rewrite
	OpDatasetUnmount
	OpDatasetChange   // properties, quota, compression, keys
	OpDatasetMountCfg // mountpoint, canmount, readonly
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
	case OpPoolRemove, OpDatasetRemove, OpDatasetUnmount, OpDatasetMountCfg:
		return false
	case OpPoolLayout:
		// The OS pool's disks carry boot partitions that 'zpool replace' or
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
	h, err := LoadHostView(ctx)
	if err != nil {
		return fmt.Errorf("%w: no se pudo comprobar si es almacenamiento del host (%v); no se hace nada", ErrHostStorage, err)
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
// This is also what a scheduled snapshot job on such a tree runs into.
func guardSnapshotCreate(ctx context.Context, dataset string, recursive bool) error {
	h, err := LoadHostView(ctx)
	if err != nil {
		return fmt.Errorf("%w: no se pudo comprobar si es almacenamiento del host (%v); no se hace nada", ErrHostStorage, err)
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

// guardRenameTarget — a rename must not put a dataset into the running OS's
// tree or under host storage mounted outside its pool, nor give it a name
// Proxmox would take for one of its disks.
func guardRenameTarget(ctx context.Context, newName string) error {
	h, err := LoadHostView(ctx)
	if err != nil {
		return fmt.Errorf("%w: no se pudo comprobar si es almacenamiento del host (%v); no se hace nada", ErrHostStorage, err)
	}
	if kind, why := h.DatasetKind(newName); kind == HostSystem || kind == HostGuest {
		return fmt.Errorf("%w: el destino %s no es válido: %s", ErrHostStorage, newName, why)
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
