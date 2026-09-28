package httpapi

import (
	"testing"

	"easyzfs/internal/actions"
)

// The UI shows a banner when a Proxmox host's storage.cfg cannot be read;
// a non-Proxmox host never raises it.
func TestHostStorageJSON(t *testing.T) {
	bad := hostStorageJSON(&actions.HostView{PVE: true, StorageUnknown: true, OSPools: map[string]bool{"rpool": true}})
	if bad["storage_cfg_unreadable"] != true || len(bad["os_pools"].([]string)) != 1 {
		t.Fatalf("unreadable: %v", bad)
	}
	if ok := hostStorageJSON(&actions.HostView{PVE: false, StorageUnknown: true}); ok["storage_cfg_unreadable"] != false {
		t.Fatalf("not Proxmox: %v", ok)
	}
}
