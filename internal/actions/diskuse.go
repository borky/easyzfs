// diskuse.go — whether a disk is safe to hand to ZFS or to power off, read
// live at the moment of the request rather than from the collector's cache.
//
// On a Proxmox VE 8.4 test VM the disk view called an LVM physical volume and
// a disk with an EFI system partition "free", and pool creation had no check
// of its own: only ZFS refusing without -f stood between a click and those
// disks. The cached inventory also showed a destroyed pool for minutes.
package actions

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"easyzfs/internal/executil"
)

// ErrDiskInUse — the disk holds something the operation would destroy or
// disturb. Mapped to 409 dev_in_use.
var ErrDiskInUse = errors.New("disco en uso")

// Test seams.
var (
	sysBlockDir = "/sys/block"
	devByIDDir  = "/dev/disk/by-id"
	lsblkJSON   = func(ctx context.Context, args ...string) ([]byte, error) {
		return executil.RunRead(ctx, 10*time.Second, "lsblk", args...)
	}
	// importedPools — the pools imported right now, read live.
	importedPools = func(ctx context.Context) (map[string]bool, error) {
		out, err := executil.RunRead(ctx, 10*time.Second, "zpool", "list", "-H", "-o", "name")
		if err != nil {
			return nil, err
		}
		m := map[string]bool{}
		for _, l := range strings.Fields(string(out)) {
			m[l] = true
		}
		return m, nil
	}
)

// Partition type GUIDs that mean "this disk boots something".
const (
	partTypeESP      = "c12a7328-f81f-11d2-ba4b-00a0c93ec93b"
	partTypeBIOSBoot = "21686148-6449-6e6f-744e-656564454649"
)

type lsblkNode struct {
	Name        string      `json:"name"`
	Type        string      `json:"type"`
	FSType      *string     `json:"fstype"`
	Label       *string     `json:"label"` // for zfs_member: the pool's name
	PartType    *string     `json:"parttype"`
	Mountpoints []*string   `json:"mountpoints"`
	Mountpoint  *string     `json:"mountpoint"` // util-linux < 2.37 has no MOUNTPOINTS
	Children    []lsblkNode `json:"children"`
}

// kernelName resolves what the API accepts for a disk — "sdb", "nvme0n1", a
// by-id name, or "/dev/disk/by-id/<name>" — to the kernel's name ("sdb").
func kernelName(dev string) string {
	switch {
	case strings.HasPrefix(dev, "/dev/disk/by-id/"):
		// Always the literal by-id path the API accepts; resolved under
		// devByIDDir, which tests point elsewhere.
		dev = filepath.Join(devByIDDir, strings.TrimPrefix(dev, "/dev/disk/by-id/"))
	case strings.HasPrefix(dev, "/dev/"):
		return strings.TrimPrefix(dev, "/dev/")
	case !strings.Contains(dev, "/"):
		if _, err := os.Stat(filepath.Join(sysBlockDir, dev)); err == nil {
			return dev
		}
		dev = filepath.Join(devByIDDir, dev)
	}
	if real, err := filepath.EvalSymlinks(dev); err == nil {
		return filepath.Base(real)
	}
	return filepath.Base(dev)
}

// diskUseReason classifies one lsblk tree for a takeover (a disk ZFS is about
// to write labels on): anything it holds is a reason. The first found, or "".
func diskUseReason(n lsblkNode) string { return classify(n, nil, "") }

// classify — with imported == nil, the takeover rules. Otherwise the power-off
// rules, which refuse only what is in use right now: an exported pool's disk
// or an unmounted filesystem is exactly what someone powers off to pull
// (the takeover rules refused both, so the removal flow never worked); a
// member of an imported pool, a mount, swap, LVM/LUKS/RAID/Ceph and boot
// partitions still refuse.
//
// ownPool (takeover rules only) is the pool a disk is being replaced in: its
// own ZFS label is then no reason, since it is that pool's old disk reseated.
func classify(n lsblkNode, imported map[string]bool, ownPool string) string {
	powerOff := imported != nil
	for _, mp := range append(n.Mountpoints, n.Mountpoint) {
		if mp != nil && *mp != "" {
			if *mp == "[SWAP]" {
				return fmt.Sprintf("%s es swap activo", n.Name)
			}
			return fmt.Sprintf("%s está montado en %s", n.Name, *mp)
		}
	}
	if n.PartType != nil {
		switch strings.ToLower(*n.PartType) {
		case partTypeESP:
			return fmt.Sprintf("%s es una partición EFI de arranque", n.Name)
		case partTypeBIOSBoot:
			return fmt.Sprintf("%s es una partición BIOS boot", n.Name)
		}
	}
	if n.FSType != nil && *n.FSType != "" {
		switch *n.FSType {
		case "zfs_member":
			if powerOff {
				label := ""
				if n.Label != nil {
					label = *n.Label
				}
				if label == "" {
					return fmt.Sprintf("%s tiene una etiqueta ZFS sin nombre de pool: no se puede saber si está en uso", n.Name)
				}
				if !imported[label] {
					break // an exported or stale pool: safe to power off
				}
				return fmt.Sprintf("%s es miembro del pool importado '%s'", n.Name, label)
			}
			if ownPool != "" && n.Label != nil && *n.Label == ownPool {
				break
			}
			return fmt.Sprintf("%s tiene una etiqueta ZFS (miembro de un pool, activo, exportado o antiguo)", n.Name)
		case "LVM2_member":
			return fmt.Sprintf("%s es un volumen físico LVM", n.Name)
		case "crypto_LUKS":
			return fmt.Sprintf("%s es un volumen cifrado LUKS", n.Name)
		case "linux_raid_member":
			return fmt.Sprintf("%s es miembro de un RAID mdadm", n.Name)
		case "ceph_bluestore":
			return fmt.Sprintf("%s es un OSD de Ceph", n.Name)
		default:
			if !powerOff { // unmounted (mounts were checked above)
				return fmt.Sprintf("%s contiene un sistema de ficheros %s", n.Name, *n.FSType)
			}
		}
	}
	switch n.Type {
	case "lvm", "crypt", "dm", "raid0", "raid1", "raid4", "raid5", "raid6", "raid10", "mpath":
		return fmt.Sprintf("%s lo usa %s", n.Name, n.Type)
	}
	for _, c := range n.Children {
		if r := classify(c, imported, ownPool); r != "" {
			return r
		}
	}
	return ""
}

// holdersReason — device-mapper (LVM, dm-crypt, multipath) holding the disk
// or one of its partitions, which lsblk can miss for inactive mappings.
func holdersReason(name string) string {
	check := func(dir, who string) string {
		if ents, err := os.ReadDir(filepath.Join(dir, "holders")); err == nil && len(ents) > 0 {
			return fmt.Sprintf("%s lo retiene %s", who, ents[0].Name())
		}
		return ""
	}
	base := filepath.Join(sysBlockDir, name)
	if r := check(base, name); r != "" {
		return r
	}
	ents, _ := os.ReadDir(base)
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), name) {
			if r := check(filepath.Join(base, e.Name()), e.Name()); r != "" {
				return r
			}
		}
	}
	return ""
}

// DiskUse — why the disk cannot be given to ZFS or powered off, read live;
// "" when it holds nothing. An error means it could not be read, which the
// callers treat as "do not proceed".
func DiskUse(ctx context.Context, dev string) (string, error) {
	return diskUse(ctx, dev, nil, "")
}

// DiskActiveUse — why the disk cannot be powered off right now, read live
// (see classify); "" when nothing on it is in use.
func DiskActiveUse(ctx context.Context, dev string) (string, error) {
	imported, err := importedPools(ctx)
	if err != nil {
		return "", fmt.Errorf("leer los pools importados: %w", err)
	}
	return diskUse(ctx, dev, imported, "")
}

func diskUse(ctx context.Context, dev string, imported map[string]bool, ownPool string) (string, error) {
	name := kernelName(dev)
	if !reDev.MatchString(name) {
		return "", ErrInvalidDev
	}
	out, err := lsblkJSON(ctx, "-J", "-o", "NAME,TYPE,FSTYPE,LABEL,PARTTYPE,MOUNTPOINTS", "/dev/"+name)
	if err != nil {
		// util-linux < 2.37 (Debian 11, PVE 7) has no MOUNTPOINTS column.
		out, err = lsblkJSON(ctx, "-J", "-o", "NAME,TYPE,FSTYPE,LABEL,PARTTYPE,MOUNTPOINT", "/dev/"+name)
	}
	if err != nil {
		return "", fmt.Errorf("leer el estado de %s: %w", name, err)
	}
	var tree struct {
		Devices []lsblkNode `json:"blockdevices"`
	}
	if err := json.Unmarshal(out, &tree); err != nil || len(tree.Devices) == 0 {
		return "", fmt.Errorf("leer el estado de %s: salida de lsblk no válida", name)
	}
	if r := classify(tree.Devices[0], imported, ownPool); r != "" {
		return r, nil
	}
	return holdersReason(name), nil
}

// requireFreeDisk — refuses a disk that holds anything, just before the
// operation that would take it over.
func requireFreeDisk(ctx context.Context, dev string) error {
	return requireFreeDiskFor(ctx, dev, "")
}

// requireFreeDiskFor — requireFreeDisk, but pool's own ZFS label is allowed
// (a disk replaced by itself after being wiped or reseated).
func requireFreeDiskFor(ctx context.Context, dev, pool string) error {
	reason, err := diskUse(ctx, dev, nil, pool)
	if err != nil {
		return err
	}
	if reason != "" {
		return fmt.Errorf("%w: %s. Si de verdad quieres reutilizarlo, bórralo antes a mano (wipefs / zpool labelclear)", ErrDiskInUse, reason)
	}
	return nil
}

// AllDiskUse — the same classification for every disk in one lsblk call, for
// the disk view. Keyed by kernel name.
func AllDiskUse(ctx context.Context) (map[string]string, error) {
	out, err := lsblkJSON(ctx, "-J", "-d", "-o", "NAME")
	if err != nil {
		return nil, err
	}
	var top struct {
		Devices []lsblkNode `json:"blockdevices"`
	}
	if err := json.Unmarshal(out, &top); err != nil {
		return nil, err
	}
	res := map[string]string{}
	for _, d := range top.Devices {
		if r, err := DiskUse(ctx, d.Name); err == nil && r != "" {
			res[d.Name] = r
		}
	}
	return res, nil
}
