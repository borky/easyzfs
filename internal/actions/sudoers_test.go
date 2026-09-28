// sudoers_test.go — deploy/install.sh pins every zfs/zpool argument shape in
// sudoers (pinned_sudoers). A property added to propValidators but not to the
// installer's list would be refused by sudo at runtime, with nothing in CI to
// say so; this test is that something.
package actions

import (
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"testing"
)

func TestSudoersPropsMatchValidators(t *testing.T) {
	b, err := os.ReadFile("../../deploy/install.sh")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?m)^\s*local PROPS='\(([a-z|]+)\)'$`).FindSubmatch(b)
	if m == nil {
		t.Fatal("PROPS not found in pinned_sudoers")
	}
	inSudoers := strings.Split(string(m[1]), "|")
	var inGo []string
	for k := range propValidators {
		inGo = append(inGo, k)
	}
	sort.Strings(inSudoers)
	sort.Strings(inGo)
	if strings.Join(inSudoers, ",") != strings.Join(inGo, ",") {
		t.Errorf("sudoers PROPS %v != propValidators %v", inSudoers, inGo)
	}
}

// The static deploy/easyzfs.sudoers (manual installs) must grant exactly what
// the installer grants.
func TestStaticSudoersMatchesInstaller(t *testing.T) {
	script := `eval "$(sed -n '/^pinned_sudoers() {/,/^}/p' ../../deploy/install.sh)"
SVC_USER=easyzfs SYSD_HELPER=/usr/local/libexec/easyzfs-sysd
pinned_sudoers /usr/sbin/zpool /usr/sbin/zfs /usr/sbin/smartctl /usr/bin/lsblk /usr/bin/crontab /usr/sbin/hdparm /usr/bin/udisksctl /usr/bin/dd /usr/bin/fuser /usr/bin/cat`
	gen, err := exec.Command("bash", "-c", script).Output()
	if err != nil {
		t.Fatal(err)
	}
	static, err := os.ReadFile("../../deploy/easyzfs.sudoers")
	if err != nil {
		t.Fatal(err)
	}
	rules := func(b []byte) string {
		var out []string
		for _, l := range strings.Split(string(b), "\n") {
			if strings.Contains(l, "NOPASSWD:") {
				out = append(out, l)
			}
		}
		return strings.Join(out, "\n")
	}
	if g, s := rules(gen), rules(static); g == "" || g != s {
		t.Errorf("deploy/easyzfs.sudoers differs from pinned_sudoers; regenerate it.\ninstaller:\n%s\nstatic:\n%s", g, s)
	}
}
