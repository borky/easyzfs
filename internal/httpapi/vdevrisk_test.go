package httpapi

import (
	"strings"
	"testing"

	"easyzfs/internal/model"
)

func TestVdevActionRisk(t *testing.T) {
	mirror2 := model.Pool{Name: "tank", Status: "ONLINE", Topo: "mirror",
		Vdevs: []model.Vdev{{Dev: "sda", Role: "mirror"}, {Dev: "sdb", Role: "mirror"}}}
	mirror3 := mirror2
	mirror3.Vdevs = append(append([]model.Vdev{}, mirror2.Vdevs...), model.Vdev{Dev: "sdc", Role: "mirror"})
	degraded := model.Pool{Name: "tank", Status: "DEGRADED", Topo: "raidz1", Vdevs: []model.Vdev{{Dev: "sda", Role: "raidz1"}}}
	resilver := model.Pool{Name: "tank", Status: "ONLINE", Topo: "raidz1", Scrub: model.ScrubInfo{State: "running", Kind: "resilver"}}
	scrubbing := model.Pool{Name: "tank", Status: "ONLINE", Topo: "raidz1", Scrub: model.ScrubInfo{State: "running", Kind: "scrub"}}
	replacing := model.Pool{Name: "tank", Status: "ONLINE", Topo: "raidz1", Vdevs: []model.Vdev{{Dev: "sdd", Role: "raidz1", Replacing: true}}}
	for _, c := range []struct {
		name   string
		pool   model.Pool
		action string
		want   string
	}{
		{"detach two-way mirror", mirror2, "detach", "sin redundancia"},
		{"offline two-way mirror", mirror2, "offline", ""},
		{"detach three-way mirror", mirror3, "detach", ""},
		{"offline degraded", degraded, "offline", "DEGRADED"},
		{"online degraded", degraded, "online", ""},
		{"offline during resilver", resilver, "offline", "resilver"},
		{"offline during scrub", scrubbing, "offline", ""},
		{"offline while replacing", replacing, "offline", "sustitución"},
	} {
		got := vdevActionRisk(c.pool, c.action)
		if (c.want == "") != (got == "") || !strings.Contains(got, c.want) {
			t.Errorf("%s: %q, want one mentioning %q", c.name, got, c.want)
		}
	}
}
