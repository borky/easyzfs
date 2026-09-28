// unit_test.go — the service unit's hardening (remediation spec P5), in the
// static deploy/easyzfs.service and in the one install.sh writes. Each
// forbidden directive was tried on a Proxmox VE 8.4 VM (systemd 252) in a
// unit running as the service account: it either implies NoNewPrivileges,
// and sudo then refuses to run, or gives the service a private mount
// namespace, and ZFS mounts stop reaching the host.
package actions

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

var unitForbidden = regexp.MustCompile(`(?m)^(NoNewPrivileges|ProtectSystem|ProtectHome|PrivateTmp|PrivateDevices|PrivateMounts|ReadWritePaths|ReadOnlyPaths|InaccessiblePaths|ProtectProc|ProcSubset|` +
	`ProtectKernelTunables|ProtectKernelModules|ProtectKernelLogs|ProtectControlGroups|ProtectHostname|ProtectClock|` +
	`LockPersonality|RestrictRealtime|SystemCallArchitectures|SystemCallFilter|RestrictSUIDSGID|RestrictNamespaces|` +
	`MemoryDenyWriteExecute|RestrictAddressFamilies|CapabilityBoundingSet|DevicePolicy|DeviceAllow|UMask)=`)

func TestUnitHardening(t *testing.T) {
	static, err := os.ReadFile("../../deploy/easyzfs.service")
	if err != nil {
		t.Fatal(err)
	}
	inst, err := os.ReadFile("../../deploy/install.sh")
	if err != nil {
		t.Fatal(err)
	}
	// The template write_unit renders: from 'unit="$(cat <<EOF' to its EOF.
	s := string(inst)
	i := strings.Index(s, `unit="$(cat <<EOF`)
	if i < 0 {
		t.Fatal("write_unit's template not found in install.sh")
	}
	tmpl := s[i:]
	tmpl = tmpl[:strings.Index(tmpl, "\nEOF\n")]
	// ${nnp} is NoNewPrivileges=yes only in root mode, where no sudo runs.
	tmpl = strings.ReplaceAll(tmpl, "${nnp}", "")
	for name, unit := range map[string]string{"deploy/easyzfs.service": string(static), "install.sh write_unit": tmpl} {
		for _, want := range []string{"LimitCORE=0", "RemoveIPC=yes", "KeyringMode=private"} {
			if !regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(want) + `$`).MatchString(unit) {
				t.Errorf("%s: missing %s", name, want)
			}
		}
		if m := unitForbidden.FindAllString(unit, -1); len(m) > 0 {
			t.Errorf("%s: sets %v, which breaks sudo or the host's mount namespace", name, m)
		}
	}
}
