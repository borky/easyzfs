// Package priv — 'easyzfs priv <tool> <args…>', the privileged gateway.
//
// The service runs unprivileged; sudoers grants it this subcommand of the
// root-owned binary and none of the storage tools themselves. Run as root, it
// checks the command against the gateway's policy (actions.PrivCheck: a closed
// grammar plus the same host/guest, mountpoint and disk checks the actions
// make) and, only if it passes, replaces itself with the tool. Replacing
// (exec) rather than running it as a child keeps the process tree the service
// expects: sudo relays SIGTERM straight to zfs, and zfs's exit status and
// output are the command's own.
package priv

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"easyzfs/internal/actions"
	"easyzfs/internal/executil"
)

// securePath — what sudo's secure_path gives too: the tools are looked up
// here only, never in a PATH the caller chose.
const securePath = "/usr/sbin:/usr/bin:/sbin:/bin"

// Main runs the gateway and returns an exit status; on success it never
// returns (the tool replaces the process).
func Main(args []string) int {
	if os.Geteuid() != 0 {
		refuse("not_allowed", "el gateway privilegiado solo corre como root (vía sudo)")
		return 3
	}
	if len(args) == 0 {
		refuse("not_allowed", "uso: easyzfs priv <herramienta> <argumentos…>")
		return 2
	}
	os.Setenv("PATH", securePath)
	tool, rest := args[0], args[1:]
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	err := actions.PrivCheck(ctx, tool, rest)
	cancel()
	if err != nil {
		refuse(actions.PrivCode(err), err.Error())
		return 3
	}
	bin, err := exec.LookPath(tool)
	if err != nil {
		refuse("invalid_input", fmt.Sprintf("%s no encontrado: %v", tool, err))
		return 3
	}
	// LC_ALL=C: the service reads ZFS's English messages ("does not exist").
	env := []string{"PATH=" + securePath, "LC_ALL=C"}
	err = syscall.Exec(bin, append([]string{tool}, rest...), env)
	refuse("invalid_input", fmt.Sprintf("ejecutar %s: %v", tool, err))
	return 3
}

// refuse — one line on stderr in the form executil turns back into a
// PrivError.
func refuse(code, msg string) {
	fmt.Fprintf(os.Stderr, "%s%s: %s\n", executil.PrivRefusalPrefix, code, strings.ReplaceAll(msg, "\n", " "))
}
