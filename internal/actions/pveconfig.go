// pveconfig.go — what Proxmox's own configuration says about storage and
// guest disks (remediation spec P2, P6).
//
// Guest disks were recognised by Proxmox's naming only; a disk Proxmox
// references under any other name (a volume moved or renamed by hand, an
// imported one) looked like ordinary data. The guest configs — every node's
// /etc/pve/nodes/<node>/{qemu-server,lxc}/<id>.conf, running guests and
// stopped ones, snapshot sections included — name each volume they use as
// "<storage>:<volume>", and storage.cfg maps a zfspool storage to its
// dataset, so together they give the datasets Proxmox is using regardless of
// their names.
//
// The files are root:www-data 0640. As root they are read directly; the
// unprivileged service asks the privileged gateway ('easyzfs priv pvecfg'),
// which reads exactly these paths and nothing else. A read that fails on a
// Proxmox host leaves the storage unknown, which the policy treats as
// "everything top-level may be Proxmox storage" rather than as "nothing is".
package actions

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"easyzfs/internal/executil"
)

// PVEConfig — storage.cfg and the volumes every guest config references, as
// read from /etc/pve. Only the references leave the gateway: a guest config
// also holds cloud-init password hashes, SSH keys and raw 'args', none of
// which the service account needs.
type PVEConfig struct {
	StorageCfg string              `json:"storage_cfg"`
	Refs       map[string][]string `json:"refs"` // guestVolumeRefs' result
}

// reGuestConf — a guest config path relative to /etc/pve/nodes/<node>/.
var reGuestConf = regexp.MustCompile(`^(qemu-server|lxc)/[0-9]+\.conf$`)

// ReadPVEConfig reads /etc/pve as root (the gateway's 'pvecfg' query, or the
// service itself in root mode). A guest config that cannot be read fails the
// whole read: a partial list would call an unlisted guest's disk data.
func ReadPVEConfig(dir string) (*PVEConfig, error) {
	b, err := os.ReadFile(filepath.Join(dir, "storage.cfg"))
	if err != nil {
		return nil, err
	}
	c := &PVEConfig{StorageCfg: string(b), Refs: map[string][]string{}}
	guests := map[string]string{}
	nodes, err := os.ReadDir(filepath.Join(dir, "nodes"))
	if err != nil {
		if os.IsNotExist(err) {
			return c, nil // no guests anywhere (a fresh install)
		}
		return nil, err
	}
	for _, n := range nodes {
		for _, kind := range []string{"qemu-server", "lxc"} {
			files, err := os.ReadDir(filepath.Join(dir, "nodes", n.Name(), kind))
			if err != nil {
				if os.IsNotExist(err) {
					continue
				}
				return nil, err
			}
			for _, f := range files {
				rel := kind + "/" + f.Name()
				if !reGuestConf.MatchString(rel) {
					continue
				}
				b, err := os.ReadFile(filepath.Join(dir, "nodes", n.Name(), kind, f.Name()))
				if err != nil {
					return nil, err
				}
				guests[strings.TrimSuffix(rel, ".conf")] = string(b)
			}
		}
	}
	c.Refs = guestVolumeRefs(guests)
	// A bind mount through a symlink (/data → /tank/data) would match no
	// mountpoint as written: add the resolved path too, read here as root
	// where every path resolves. Paths through a legacy-mounted dataset
	// still match nothing (its mountpoint property is not a path).
	for _, ref := range sortedRefKeys(c.Refs) {
		if !strings.HasPrefix(ref, "/") || strings.HasPrefix(ref, "/dev/") {
			continue
		}
		if r, err := filepath.EvalSymlinks(ref); err == nil && r != ref && r != "/" {
			for _, w := range c.Refs[ref] {
				if !contains(c.Refs[r], w) {
					c.Refs[r] = append(c.Refs[r], w)
				}
			}
		}
	}
	return c, nil
}

// readPVEConfig — test seam. As root, read /etc/pve directly; unprivileged,
// through the gateway; in read-only mode (no gateway) only storage.cfg,
// through its pinned sudo rule, and no guest configs.
var readPVEConfig = func(ctx context.Context) (*PVEConfig, error) {
	if os.Geteuid() == 0 {
		return ReadPVEConfig(pveDir)
	}
	if executil.PrivBin != "" {
		out, err := executil.RunDirect(ctx, 20*time.Second, "sudo", "-n", executil.PrivBin, "priv", "pvecfg")
		if err != nil {
			return nil, err
		}
		var c PVEConfig
		if err := json.Unmarshal(out, &c); err != nil {
			return nil, fmt.Errorf("pvecfg: %w", err)
		}
		return &c, nil
	}
	b, err := readStorageCfg(ctx)
	if err != nil {
		return nil, err
	}
	return &PVEConfig{StorageCfg: string(b)}, nil
}

// guestVolumeRefs — what the guest configs use → the guests that use it:
// disk keys (scsi0, virtio1, efidisk0, tpmstate0, rootfs, mp0, dev0…), unused
// disks, saved state and raw lxc.mount.entry binds, in the current config
// and in every snapshot section. A reference is either "<storage>:<volume>"
// (only zfspool storages resolve to datasets; the caller maps them) or an
// absolute path: a bind mount (mp0: /tank/media,mp=/media) or a raw device
// (scsi1: /dev/zvol/tank/vol) — a guest using a dataset by path rather than
// through a storage, which no naming convention would reveal.
func guestVolumeRefs(guests map[string]string) map[string][]string {
	refs := map[string][]string{}
	keys := make([]string, 0, len(guests))
	for k := range guests {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, g := range keys {
		kind, id, _ := strings.Cut(g, "/")
		who := "VM " + id
		if kind == "lxc" {
			who = "CT " + id
		}
		for _, line := range strings.Split(guests[g], "\n") {
			k, v, ok := strings.Cut(line, ":")
			if !ok || strings.HasPrefix(strings.TrimSpace(line), "#") {
				continue
			}
			k, v = strings.TrimSpace(k), strings.TrimSpace(v)
			var ref string
			switch {
			case k == "lxc.mount.entry":
				// "<source> <target> none bind[,…] 0 0"
				if f := strings.Fields(v); len(f) > 0 && strings.HasPrefix(f[0], "/") {
					ref = path.Clean(f[0])
				}
			case reDiskKey.MatchString(k):
				vol, _, _ := strings.Cut(v, ",")
				if strings.HasPrefix(vol, "/") {
					ref = path.Clean(vol)
				} else if s, name, ok := strings.Cut(vol, ":"); ok && s != "" && name != "" {
					// A linked clone is "<base>/<clone>": the clone is the
					// dataset, as Proxmox's ZFSPoolPlugin reads it.
					ref = s + ":" + name[strings.LastIndex(name, "/")+1:]
				}
			}
			if ref != "" && ref != "/" && !contains(refs[ref], who) {
				refs[ref] = append(refs[ref], who)
			}
		}
	}
	return refs
}

func sortedRefKeys(m map[string][]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// reDiskKey — the config keys whose value is a volume.
var reDiskKey = regexp.MustCompile(`^(ide|sata|scsi|virtio|efidisk|tpmstate|unused|mp|dev)[0-9]+$|^(rootfs|vmstate)$`)

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}
