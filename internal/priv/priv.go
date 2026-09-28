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
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
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
	if tool == "zfs" && len(rest) > 0 && rest[0] == "recv" {
		return recv(bin, rest, env)
	}
	err = syscall.Exec(bin, append([]string{tool}, rest...), env)
	refuse("invalid_input", fmt.Sprintf("ejecutar %s: %v", tool, err))
	return 3
}

// recv — 'zfs recv' after checking the stream is what the form says: the
// filesystem form (setuid, devices and exec off) for a filesystem stream,
// the volume form for a volume stream. volmode is ignored for a filesystem,
// so a filesystem stream must never go through the volume form, and only
// the stream itself says which it is. Its first record is read here and fed
// back to zfs ahead of the rest, so zfs runs as a child rather than by exec;
// SIGTERM and SIGINT are passed on, and its exit status is returned.
func recv(bin string, args, env []string) int {
	hdr := make([]byte, 40)
	n, err := io.ReadFull(os.Stdin, hdr)
	if err != nil {
		refuse("invalid_input", fmt.Sprintf("leer el stream: %v", err))
		return 3
	}
	volume, err := actions.RecvStreamIsVolume(hdr[:n])
	if err != nil {
		refuse(actions.PrivCode(err), err.Error())
		return 3
	}
	if volume != actions.IsRecvVolumeForm(args[1:]) {
		refuse("not_allowed", "el tipo del stream no corresponde a la forma de zfs recv pedida")
		return 3
	}
	cmd := exec.Command(bin, args...)
	cmd.Args[0] = "zfs"
	cmd.Env = env
	cmd.Stdin = io.MultiReader(bytes.NewReader(hdr[:n]), os.Stdin)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	sig := make(chan os.Signal, 2)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
	if err := cmd.Start(); err != nil {
		refuse("invalid_input", fmt.Sprintf("ejecutar zfs recv: %v", err))
		return 3
	}
	go func() {
		for s := range sig {
			_ = cmd.Process.Signal(s)
		}
	}()
	err = cmd.Wait()
	signal.Stop(sig)
	if ee, ok := err.(*exec.ExitError); ok {
		return ee.ExitCode()
	}
	if err != nil {
		return 3
	}
	return 0
}

// refuse — one line on stderr in the form executil turns back into a
// PrivError.
func refuse(code, msg string) {
	fmt.Fprintf(os.Stderr, "%s%s: %s\n", executil.PrivRefusalPrefix, code, strings.ReplaceAll(msg, "\n", " "))
}
