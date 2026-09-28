package httpapi

import (
	"strings"
	"testing"

	"easyzfs/internal/model"
)

func TestVdevActionRisk(t *testing.T) {
	disk := func(dev, role, group, status string) model.Vdev {
		return model.Vdev{Dev: dev, Role: role, Group: group, Status: status}
	}
	// Two 2-way mirrors plus a mirrored log: the layout a pool-wide count
	// of mirror disks got wrong.
	mirrors := model.Pool{Name: "tank", Status: "ONLINE", Topo: "mirror", Vdevs: []model.Vdev{
		disk("sda", "mirror", "mirror-0", "ONLINE"), disk("sdb", "mirror", "mirror-0", "ONLINE"),
		disk("sdc", "mirror", "mirror-1", "ONLINE"), disk("sdd", "mirror", "mirror-1", "ONLINE"),
		disk("sde", "mirror", "mirror-2", "ONLINE"), disk("sdf", "mirror", "mirror-2", "ONLINE")}}
	threeWay := model.Pool{Name: "tank", Status: "ONLINE", Topo: "mirror", Vdevs: []model.Vdev{
		disk("sda", "mirror", "mirror-0", "ONLINE"), disk("sdb", "mirror", "mirror-0", "ONLINE"), disk("sdc", "mirror", "mirror-0", "ONLINE")}}
	raidz1 := model.Pool{Name: "tank", Status: "ONLINE", Topo: "raidz1", Vdevs: []model.Vdev{
		disk("sda", "raidz1", "raidz1-0", "ONLINE"), disk("sdb", "raidz1", "raidz1-0", "ONLINE"), disk("sdc", "raidz1", "raidz1-0", "ONLINE")}}
	raidz2 := model.Pool{Name: "tank", Status: "ONLINE", Topo: "raidz2", Vdevs: []model.Vdev{
		disk("sda", "raidz2", "raidz2-0", "ONLINE"), disk("sdb", "raidz2", "raidz2-0", "ONLINE"),
		disk("sdc", "raidz2", "raidz2-0", "ONLINE"), disk("sdd", "raidz2", "raidz2-0", "ONLINE")}}
	degraded := model.Pool{Name: "tank", Status: "DEGRADED", Vdevs: raidz1.Vdevs}
	resilver := raidz2
	resilver.Scrub = model.ScrubInfo{State: "running", Kind: "resilver"}
	scrubbing := raidz2
	scrubbing.Scrub = model.ScrubInfo{State: "running", Kind: "scrub"}
	replacing := model.Pool{Name: "tank", Status: "ONLINE", Vdevs: []model.Vdev{
		disk("sda", "raidz2", "raidz2-0", "ONLINE"), {Dev: "sdx", Role: "raidz2", Group: "raidz2-0", Status: "ONLINE", Replacing: true}}}
	for _, c := range []struct {
		name        string
		pool        model.Pool
		action, dev string
		want        string
	}{
		{"detach from a 2-way mirror beside others", mirrors, "detach", "sdc", "mirror-1"},
		{"offline from a 2-way mirror", mirrors, "offline", "sda", "mirror-0"},
		{"detach from the mirrored log", mirrors, "detach", "sdf", "mirror-2"},
		{"detach from a 3-way mirror", threeWay, "detach", "sda", ""},
		{"online", mirrors, "online", "sda", ""},
		{"offline from a healthy raidz1", raidz1, "offline", "sdb", "sin paridad"},
		{"offline from a healthy raidz2", raidz2, "offline", "sdb", ""},
		{"offline on a degraded pool", degraded, "offline", "sdb", "DEGRADED"},
		{"offline during resilver", resilver, "offline", "sdb", "resilver"},
		{"offline during scrub", scrubbing, "offline", "sdb", ""},
		{"offline while replacing", replacing, "offline", "sda", "sustitución"},
		{"unknown disk", raidz2, "offline", "sdz", ""},
	} {
		got := vdevActionRisk(c.pool, c.action, c.dev)
		if (c.want == "") != (got == "") || !strings.Contains(got, c.want) {
			t.Errorf("%s: %q, want one mentioning %q", c.name, got, c.want)
		}
	}
}
