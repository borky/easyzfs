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

// --- the privileged gateway ('easyzfs priv', internal/actions/privgate.go) ---

// privTools — the commands that only ever run with root rights through the
// gateway: sudoers grants the service none of them directly, so its checks
// cannot be skipped by calling the tool itself.
var privTools = map[string]bool{"zfs": true, "zpool": true, "smartctl": true, "dd": true, "hdparm": true, "udisksctl": true}

// PrivBin — the root-owned easyzfs binary sudo runs as 'priv' (main sets it
// when the service is unprivileged; sudoers pins exactly this path). Empty:
// the tools go to sudo directly, as in read-only mode, whose sudoers grants a
// few pinned reads and no gateway.
var PrivBin string

// PrivGate — in root mode (no sudo) the gateway's checks run in-process
// before the command: main sets it to actions.PrivCheck.
var PrivGate func(ctx context.Context, tool string, args []string) error

// PrivError — the gateway refused a command. Code is its wire code;
// RegisterPrivCode maps it back to the domain error, so errors.Is works on
// the service side as if the check had run there.
type PrivError struct{ Code, Message string }

func (e *PrivError) Error() string { return e.Message }
func (e *PrivError) Unwrap() error { return privCodes[e.Code] }

var privCodes = map[string]error{}

// RegisterPrivCode — ties a gateway code to the error it stands for.
func RegisterPrivCode(code string, err error) { privCodes[code] = err }

// PrivRefusalPrefix — how the gateway reports a refusal on stderr:
// "easyzfs-priv: <code>: <message>".
const PrivRefusalPrefix = "easyzfs-priv: "

// command — what to run for name+args, applying the sudo and gateway rules.
func command(ctx context.Context, name string, args []string) (string, []string, error) {
	if privTools[name] {
		switch {
		case useSudo && PrivBin != "":
			return "sudo", append([]string{"-n", PrivBin, "priv", name}, args...), nil
		case !useSudo && PrivGate != nil:
			if err := PrivGate(ctx, name, args); err != nil {
				return "", nil, err
			}
		}
	}
	if useSudo {
		return "sudo", append([]string{"-n", name}, args...), nil
	}
	return name, args, nil
}

// exitErr — the error for a command that failed: the gateway's refusal as a
// PrivError, anything else as before (tool name plus trimmed stderr).
func exitErr(orig string, ee *exec.ExitError) error {
	for _, line := range strings.Split(string(ee.Stderr), "\n") {
		if rest, ok := strings.CutPrefix(line, PrivRefusalPrefix); ok {
			code, msg, _ := strings.Cut(rest, ": ")
			return &PrivError{Code: code, Message: msg}
		}
	}
	return fmt.Errorf("%s: %s", orig, trimErr(ee.Stderr))
}

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
	name, args, err := command(ctx, name, args)
	if err != nil {
		return nil, err
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
			return nil, exitErr(orig, ee)
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
	name, args, err := command(ctx, name, args)
	if err != nil {
		return nil, err
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
		if pe, ok := exitErr(orig, ee).(*PrivError); ok {
			return nil, pe // the gateway refused: nothing ran
		}
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
	name, args, err := command(ctx, name, args)
	if err != nil {
		return nil, err
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
			return nil, exitErr(orig, ee)
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
//
// A gateway refusal (root mode, checked in-process) is carried in cmd.Err, so
// Start fails without running anything; through sudo the gateway itself
// refuses and the command exits non-zero with the reason on stderr.
func NewCommand(ctx context.Context, name string, args ...string) *exec.Cmd {
	cname, cargs, err := command(ctx, name, args)
	if err != nil {
		cmd := exec.CommandContext(ctx, name)
		cmd.Err = err
		return cmd
	}
	return groupCommand(ctx, cname, cargs...)
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
