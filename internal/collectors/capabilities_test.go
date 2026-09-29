// capabilities_test.go — parseo de versiones OpenZFS y feature-gating.
package collectors

import (
	"testing"

	"easyzfs/internal/model"
)

func TestCapabilitiesFromOutput(t *testing.T) {
	cases := []struct {
		name string
		out  string
		want model.Capabilities
	}{
		{
			name: "2.1.x: sin nada moderno",
			out:  "zfs-2.1.11-1\nzfs-kmod-2.1.11-1",
			want: model.Capabilities{Version: "2.1.11"},
		},
		{
			name: "2.2.x: json viejo (no ≥2.3), sin raidz expansion",
			out:  "zfs-2.2.6-1\nzfs-kmod-2.2.6-1",
			want: model.Capabilities{Version: "2.2.6"},
		},
		{
			name: "2.3.2: raidz expansion + json, sin rewrite ni 2.4",
			out:  "zfs-2.3.2-2\nzfs-kmod-2.3.2-2",
			want: model.Capabilities{RaidzExpansion: true, JSONOutput: true, Version: "2.3.2"},
		},
		{
			name: "2.3.4: rewrite sí, scrub -a no",
			out:  "zfs-2.3.4-1\nzfs-kmod-2.3.4-1",
			want: model.Capabilities{Rewrite: true, RaidzExpansion: true, JSONOutput: true, Version: "2.3.4"},
		},
		{
			name: "2.4.0: todo (incl. zarc names y scrub -a/-S/-E)",
			out:  "zfs-2.4.0~rc1-1\nzfs-kmod-2.4.0~rc1-1",
			want: model.Capabilities{Rewrite: true, RaidzExpansion: true, ScrubAll: true,
				ScrubRange: true, ZarcNames: true, JSONOutput: true, Version: "2.4.0"},
		},
		{
			name: "salida sin versión reconocible",
			out:  "no zfs here",
			want: model.Capabilities{Version: "unknown"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := CapabilitiesFromOutput(c.out); got != c.want {
				t.Errorf("CapabilitiesFromOutput(%q) = %+v, esperaba %+v", c.out, got, c.want)
			}
		})
	}
}

func TestParseVersion(t *testing.T) {
	for _, tc := range []struct {
		out           string
		maj, min, pat int
		ok            bool
	}{
		{"zfs-2.3.2-2", 2, 3, 2, true},
		{"zfs-kmod-2.4.0", 2, 4, 0, true},
		{"garbage", 0, 0, 0, false},
		{"", 0, 0, 0, false},
	} {
		maj, min, pat, ok := parseVersion(tc.out)
		if maj != tc.maj || min != tc.min || pat != tc.pat || ok != tc.ok {
			t.Errorf("parseVersion(%q) = (%d,%d,%d,%v), esperaba (%d,%d,%d,%v)",
				tc.out, maj, min, pat, ok, tc.maj, tc.min, tc.pat, tc.ok)
		}
	}
}
