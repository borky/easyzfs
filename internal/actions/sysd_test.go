// sysd_test.go — deploy/easyzfs-sysd cron-to-timer, run against a temp root
// (EASYZFS_SYSD_ROOT). The generated units were checked against real systemd
// on a Proxmox test VM (output identical to /bin/sh running the cron line);
// these tests pin that text so a regression shows up in CI.
package actions

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// sysdConvert writes cronFile as /etc/cron.d/ezjob in a temp root, converts
// line n, and returns the unit's text or the helper's error output.
func sysdConvert(t *testing.T, cronFile string, n string) (unit string, errOut string) {
	t.Helper()
	root := t.TempDir()
	for _, d := range []string{"etc/cron.d", "etc/systemd/system"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "etc/cron.d/ezjob"), []byte(cronFile), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", "../../deploy/easyzfs-sysd", "cron-to-timer", "/etc/cron.d/ezjob", n, "ezjob")
	cmd.Env = append(os.Environ(), "EASYZFS_SYSD_ROOT="+root)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", string(out)
	}
	b, err := os.ReadFile(filepath.Join(root, "etc/systemd/system/easyzfs-ezjob.service"))
	if err != nil {
		t.Fatal(err)
	}
	return string(b), ""
}

func TestCronToTimerRunsThroughTheShell(t *testing.T) {
	unit, errOut := sysdConvert(t,
		"0  3 * * *   root   echo \"a  b\" | tr a-z A-Z > /tmp/o; echo \"$HOME x\\%y\" >> /tmp/o\n", "1")
	if errOut != "" {
		t.Fatalf("conversion failed: %s", errOut)
	}
	for _, want := range []string{
		"User=root\n",          // always written: systemd then sets HOME/LOGNAME as cron does
		"WorkingDirectory=~\n", // cron runs jobs in the user's home, systemd in /
		"Environment=\"PATH=/usr/bin:/bin\"\n",
		`ExecStart=/bin/sh -c "echo \"a  b\" | tr a-z A-Z > /tmp/o; echo \"$$HOME x%%y\" >> /tmp/o"` + "\n",
	} {
		if !strings.Contains(unit, want) {
			t.Errorf("unit lacks %q:\n%s", want, unit)
		}
	}
}

func TestCronToTimerCarriesShellAndPath(t *testing.T) {
	unit, errOut := sysdConvert(t,
		"SHELL=/bin/sh\nPATH=/usr/local/bin:/usr/bin:/bin\n\n17 * * * * nobody echo hi\n", "4")
	if errOut != "" {
		t.Fatalf("conversion failed: %s", errOut)
	}
	for _, want := range []string{"User=nobody\n", "Environment=\"PATH=/usr/local/bin:/usr/bin:/bin\"\n", "ExecStart=/bin/sh -c \"echo hi\"\n"} {
		if !strings.Contains(unit, want) {
			t.Errorf("unit lacks %q:\n%s", want, unit)
		}
	}
}

func TestCronToTimerRefusesWhatItCannotConvert(t *testing.T) {
	for name, c := range map[string]struct{ file, line string }{
		"unescaped %": {"0 4 * * * root date +%s > /tmp/x\n", "1"},
		"odd SHELL":   {"SHELL=/bin/bash;rm\n0 4 * * * root echo hi\n", "2"},
		"odd PATH":    {"PATH=/bin $(id)\n0 4 * * * root echo hi\n", "2"},
		"odd user":    {"0 4 * * * r;oot echo hi\n", "1"},
	} {
		if unit, _ := sysdConvert(t, c.file, c.line); unit != "" {
			t.Errorf("%s: converted, want a refusal:\n%s", name, unit)
		}
	}
}
