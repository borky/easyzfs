// sshkey.go — clave SSH ed25519 del daemon para la replicación remota.
// Vive en <datadir>/ssh/ (0600 la privada): NUNCA el ~/.ssh del sistema y la
// privada jamás sale del servidor ni se registra en logs.
package replication

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"

	"easyzfs/internal/executil"
)

// sshDir — <datadir>/ssh (id_ed25519 + known_hosts).
func (r *Runner) sshDir() string { return filepath.Join(r.dataDir, "ssh") }

func (r *Runner) keyPath() string { return filepath.Join(r.sshDir(), "id_ed25519") }
func (r *Runner) knownHosts() string {
	return filepath.Join(r.sshDir(), "known_hosts")
}

// sshArgs — opciones comunes y seguras para los ssh de replicación:
// BatchMode (jamás interactivo), accept-new contra known_hosts propio,
// timeout de conexión acotado. El puerto se añade tras validar (1-65535).
func (r *Runner) sshArgs(j *Job) []string {
	return []string{
		"-i", r.keyPath(),
		"-o", "BatchMode=yes",
		"-o", "ConnectTimeout=10",
		"-o", "StrictHostKeyChecking=accept-new",
		"-o", "UserKnownHostsFile=" + r.knownHosts(),
		"-p", fmt.Sprintf("%d", j.Port),
	}
}

// EnsureSSHKey — genera el par ed25519 al primer uso y devuelve la clave
// pública en formato authorized_keys. Idempotente.
func (r *Runner) EnsureSSHKey() (string, error) {
	dir := r.sshDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	privPath := r.keyPath()
	pubPath := privPath + ".pub"
	if _, err := os.Stat(privPath); errors.Is(err, os.ErrNotExist) {
		pub, priv, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			return "", fmt.Errorf("generar clave ed25519: %w", err)
		}
		sshPub, err := ssh.NewPublicKey(pub)
		if err != nil {
			return "", err
		}
		blk, err := ssh.MarshalPrivateKey(priv, "easyzfs-replication")
		if err != nil {
			return "", err
		}
		// Escritura atómica + permisos estrictos ANTES de que exista el fichero.
		tmp := privPath + ".tmp"
		if err := os.WriteFile(tmp, pem.EncodeToMemory(blk), 0o600); err != nil {
			return "", err
		}
		if err := os.Rename(tmp, privPath); err != nil {
			return "", err
		}
		pubLine := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sshPub))) + " easyzfs-replication\n"
		if err := os.WriteFile(pubPath, []byte(pubLine), 0o644); err != nil {
			return "", err
		}
	}
	b, err := os.ReadFile(pubPath)
	if err != nil {
		return "", fmt.Errorf("leer clave pública: %w", err)
	}
	return strings.TrimSpace(string(b)), nil
}

// TestConnection — POST /api/replication/test: 'ssh … zfs version' contra el
// destino. Devuelve la versión remota o un error legible clasificado
// (autenticación / red / zfs ausente).
func (r *Runner) TestConnection(ctx context.Context, host, user string, port int) (string, error) {
	if err := ValidateSSHHost(host); err != nil {
		return "", err
	}
	if err := ValidateSSHUser(user); err != nil {
		return "", err
	}
	if err := ValidateSSHPort(port); err != nil {
		return "", err
	}
	if r.mock {
		return "", fmt.Errorf("autenticación fallida: Permission denied (publickey) — instala la clave pública del servidor en el destino")
	}
	if _, err := r.EnsureSSHKey(); err != nil {
		return "", err
	}
	j := &Job{Host: host, User: user, Port: port}
	args := append(r.sshArgs(j), user+"@"+host, "zfs", "version")
	// As the service user: its key lives in the data dir; sudo does not grant ssh.
	out, err := executil.RunDirect(ctx, 20*time.Second, "ssh", args...)
	if err != nil {
		return "", classifySSHError(err)
	}
	// 'zfs version' imprime 2 líneas (zfs-x.y.z / zfs-kmod-…); nos quedamos
	// con la primera como versión remota legible.
	line := strings.TrimSpace(strings.SplitN(string(out), "\n", 2)[0])
	if line == "" {
		line = "zfs (versión desconocida)"
	}
	return line, nil
}

// classifySSHError — traduce fallos de ssh a mensajes accionables.
func classifySSHError(err error) error {
	s := err.Error()
	switch {
	case strings.Contains(s, "Permission denied"):
		return fmt.Errorf("autenticación fallida (Permission denied): instala la clave pública del servidor en el authorized_keys del usuario destino (GET /api/replication/sshkey)")
	case strings.Contains(s, "command not found"), strings.Contains(s, "no such file"):
		return fmt.Errorf("el destino no tiene ZFS instalado o no está en el PATH del usuario remoto: %s", s)
	case strings.Contains(s, "Connection refused"):
		return fmt.Errorf("conexión rechazada: ¿sshd escuchando en ese puerto? (%s)", s)
	case strings.Contains(s, "Could not resolve"), strings.Contains(s, "Name or service not known"):
		return fmt.Errorf("no se puede resolver el host: %s", s)
	case strings.Contains(s, "timed out"), strings.Contains(s, "timeout"):
		return fmt.Errorf("timeout de conexión (host inalcanzable o cortafuegos): %s", s)
	case strings.Contains(s, "Host key verification failed"):
		return fmt.Errorf("la clave del host cambió o no es de confianza; revisa %s: %s", "known_hosts", s)
	}
	return fmt.Errorf("ssh: %s", s)
}
