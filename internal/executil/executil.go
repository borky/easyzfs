// Package executil — ejecución defensiva de comandos del sistema.
// Reglas del skill: CommandContext con timeout, args separados (NUNCA shell),
// un comando que falla degrada la métrica, jamás tumba el proceso.
package executil

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

// ErrTimeout indica que el comando superó su timeout.
var ErrTimeout = errors.New("timeout ejecutando comando")

// useSudo decide si los comandos privilegiados (zpool, zfs, smartctl, lsblk, crontab)
// se ejecutan vía `sudo -n`:
//   - EASYZFS_SUDO=1/true/yes → siempre sudo; =0/false/no → nunca.
//   - Sin override (auto): sudo solo si el proceso NO corre como root (euid != 0).
//
// NoNewPrivileges=yes en systemd rompería sudo; el unit lo evita (ver deploy/).
var useSudo = detectSudo()

func detectSudo() bool {
	switch v := strings.ToLower(os.Getenv("EASYZFS_SUDO")); v {
	case "1", "true", "yes":
		return true
	case "0", "false", "no":
		return false
	}
	return os.Geteuid() != 0
}

// SudoEnabled expone la decisión (para logs/tests).
func SudoEnabled() bool { return useSudo }

// SetSudoForTest fuerza la decisión de sudo (tests que lanzan procesos reales
// sin privilegios, p.ej. el runner longops con sleep/printf).
func SetSudoForTest(v bool) { useSudo = v }

// RunRead runs a read-only command without sudo, and falls back to sudo only
// if the kernel refuses it ("permission denied"). zpool list/get/status/
// iostat, zfs list/get and lsblk all work unprivileged on Linux (/dev/zfs is
// world-accessible for reads), so the service can monitor with a sudoers file
// that grants no zpool or zfs subcommand that changes anything.
func RunRead(ctx context.Context, timeout time.Duration, name string, args ...string) ([]byte, error) {
	out, err := RunDirect(ctx, timeout, name, args...)
	if err != nil && useSudo && strings.Contains(strings.ToLower(err.Error()), "permission denied") {
		return Run(ctx, timeout, name, args...)
	}
	return out, err
}

// RunDirect ejecuta name sin anteponer sudo NUNCA (para comandos que no
// necesitan root aunque el proceso corra sin privilegios, p. ej.
// `systemctl list-timers` o `crontab -l` del propio usuario).
func RunDirect(ctx context.Context, timeout time.Duration, name string, args ...string) ([]byte, error) {
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, name, args...)
	out, err := cmd.Output()
	if cctx.Err() == context.DeadlineExceeded {
		return nil, fmt.Errorf("%s: %w tras %s", name, ErrTimeout, timeout)
	}
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && len(ee.Stderr) > 0 {
			return nil, fmt.Errorf("%s: %s", name, trimErr(ee.Stderr))
		}
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	return out, nil
}

// Run ejecuta name con args y devuelve stdout. Nunca interpolar en shell.
// Si el proceso no es root (o EASYZFS_SUDO=1), antepone `sudo -n`.
func Run(ctx context.Context, timeout time.Duration, name string, args ...string) ([]byte, error) {
	orig := name // para mensajes de error legibles
	if useSudo {
		args = append([]string{"-n", name}, args...)
		name = "sudo"
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, name, args...)
	out, err := cmd.Output()
	if cctx.Err() == context.DeadlineExceeded {
		return nil, fmt.Errorf("%s: %w tras %s", orig, ErrTimeout, timeout)
	}
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && len(ee.Stderr) > 0 {
			return nil, fmt.Errorf("%s: %s", orig, trimErr(ee.Stderr))
		}
		return nil, fmt.Errorf("%s: %w", orig, err)
	}
	return out, nil
}

// RunTolerant ejecuta como Run pero, si el comando sale con código != 0,
// devuelve IGUALMENTE el stdout capturado junto al error. Pensado para
// smartctl, que usa el exit status como bitfield de avisos del disco
// (p. ej. 192 = self-test log con errores) y aun así emite JSON válido:
// descartar la salida convertiría un disco ENFERMO en "sin datos", que es
// exactamente lo contrario de lo que debe hacer un monitor.
func RunTolerant(ctx context.Context, timeout time.Duration, name string, args ...string) ([]byte, error) {
	orig := name
	if useSudo {
		args = append([]string{"-n", name}, args...)
		name = "sudo"
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, name, args...)
	out, err := cmd.Output()
	if cctx.Err() == context.DeadlineExceeded {
		return nil, fmt.Errorf("%s: %w tras %s", orig, ErrTimeout, timeout)
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return out, err // stdout válido pese al exit != 0
	}
	if err != nil {
		return nil, fmt.Errorf("%s: %w", orig, err)
	}
	return out, nil
}

// RunStdin ejecuta name con args pasando stdin al proceso (p. ej. la
// passphrase de 'zfs load-key'/'zfs create -o keyformat=passphrase': NUNCA
// por argv, que es visible en ps / audit logs). El buffer NO se copia; el
// llamador debe limpiarlo tras la llamada (Zero). Misma política sudo/timeout
// que Run.
func RunStdin(ctx context.Context, timeout time.Duration, stdin []byte, name string, args ...string) ([]byte, error) {
	orig := name
	if useSudo {
		args = append([]string{"-n", name}, args...)
		name = "sudo"
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, name, args...)
	cmd.Stdin = bytes.NewReader(stdin)
	out, err := cmd.Output()
	if cctx.Err() == context.DeadlineExceeded {
		return nil, fmt.Errorf("%s: %w tras %s", orig, ErrTimeout, timeout)
	}
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && len(ee.Stderr) > 0 {
			return nil, fmt.Errorf("%s: %s", orig, trimErr(ee.Stderr))
		}
		return nil, fmt.Errorf("%s: %w", orig, err)
	}
	return out, nil
}

// Zero sobrescribe un buffer sensible (passphrase) antes de soltarlo.
// runtime no expone utilidad de borrado; bucle manual que el compilador no
// elimina (escribe memoria viva referenciada tras la llamada).
func Zero(b []byte) {
	for i := range b {
		b[i] = 0
	}
}

// NewCommand construye un *exec.Cmd sobre ctx aplicando la misma política de
// sudo que Run (sin timeout propio: lo gestiona el ctx del llamador). Pensado
// para procesos largos o persistentes (zpool events -f, zfs rewrite…) que no
// caben en el patrón Run+timeout.
func NewCommand(ctx context.Context, name string, args ...string) *exec.Cmd {
	if useSudo {
		args = append([]string{"-n", name}, args...)
		name = "sudo"
	}
	return groupCommand(ctx, name, args...)
}

// NewCommandDirect — NewCommand without sudo, for a pipeline stage that must
// run as the service user (ssh, which authenticates with the service's own
// key and has no business running as root).
func NewCommandDirect(ctx context.Context, name string, args ...string) *exec.Cmd {
	return groupCommand(ctx, name, args...)
}

// TermGrace — how long a long-lived command gets to exit after SIGTERM before
// it is killed outright.
const TermGrace = 10 * time.Second

// groupCommand — a command leading its own process group, stopped with
// SIGTERM to the group when ctx ends. exec.CommandContext's default is
// SIGKILL to the leader, and when the leader is sudo that kills sudo alone:
// the command it ran keeps root's uid, so the service may not signal it, and
// it went on running orphaned (reproduced on Proxmox VE 8.4 with sudo
// 1.9.13). sudo relays SIGTERM to its command and exits with it. After
// TermGrace, Go kills the leader and stops waiting.
func groupCommand(ctx context.Context, name string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM) }
	cmd.WaitDelay = TermGrace
	return cmd
}

// trimErr recorta stderr para mensajes de error legibles (máx. 200 chars).
func trimErr(b []byte) string {
	s := string(b)
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == '\r' || s[len(s)-1] == ' ') {
		s = s[:len(s)-1]
	}
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}
