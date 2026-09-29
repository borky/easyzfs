// schedsys.go — colector de tareas del sistema (SOLO LECTURA): timers de
// systemd (`systemctl list-timers --all --output=json`) y cron (crontab del
// usuario del servicio o de root vía sudo, /etc/crontab, /etc/cron.d/* y los
// directorios cron.{hourly,daily,weekly,monthly} como @hourly/@daily/…).
// Tolerante a fallos: sin systemd o sin cron devuelve lista vacía (solo log).
package collectors

import (
	"context"
	"encoding/json"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"easyzfs/internal/executil"
	"easyzfs/internal/model"
)

const schedsysInterval = 5 * time.Minute

// SchedSysCollector — caché de temporizadores del sistema.
type SchedSysCollector struct {
	mu      sync.RWMutex
	timers  []model.SysTimer
	systemd bool // systemd disponible como init (systemctl + /run/systemd/system)
}

// SystemdAvailable — systemd operativo como sistema de init: systemctl en
// PATH Y directorio de runtime /run/systemd/system (ausente en contenedores,
// WSL o inits alternativos aunque systemctl exista).
func SystemdAvailable() bool {
	if _, err := exec.LookPath("systemctl"); err != nil {
		return false
	}
	st, err := os.Stat("/run/systemd/system")
	return err == nil && st.IsDir()
}

// NewSchedSysCollector crea el colector de tareas del sistema.
func NewSchedSysCollector() *SchedSysCollector { return &SchedSysCollector{} }

// Name implementa Collector.
func (c *SchedSysCollector) Name() string { return "schedsys" }

// Run — bucle con ticker (patrón del skill); nunca falla en fatal.
func (c *SchedSysCollector) Run(ctx context.Context) {
	c.collectOnce(ctx)
	t := time.NewTicker(schedsysInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			c.collectOnce(ctx)
		}
	}
}

// SysTimers — caché de temporizadores (copia defensiva).
func (c *SchedSysCollector) SysTimers() []model.SysTimer {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]model.SysTimer, len(c.timers))
	copy(out, c.timers)
	return out
}

// SystemdAvailable implementa SysTimerProvider: si el sistema tiene systemd.
func (c *SchedSysCollector) SystemdAvailable() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.systemd
}

// Refresh — fuerza una pasada inmediata (tras editar/migrar una tarea).
func (c *SchedSysCollector) Refresh(ctx context.Context) { c.collectOnce(ctx) }

// collectOnce refresca la caché; cada fuente es independiente y best-effort.
// Solo se conservan las tareas relacionadas con ZFS (ver isZFSTask): el resto
// del sistema (logrotate, apt, xfs/e2scrub…) es ruido para esta app.
func (c *SchedSysCollector) collectOnce(ctx context.Context) {
	out := []model.SysTimer{}
	for _, t := range append(c.systemdTimers(ctx), c.cronEntries(ctx)...) {
		if isZFSTask(t) {
			out = append(out, t)
		}
	}
	// OnCalendar solo de las tareas ZFS (pocas): 'systemctl show' por unidad.
	for i := range out {
		if out[i].Source == "systemd" && out[i].Schedule == "" {
			out[i].Schedule = timerOnCalendar(ctx, out[i].Name)
		}
	}
	c.mu.Lock()
	c.timers = out
	c.systemd = SystemdAvailable()
	c.mu.Unlock()
}

// timerOnCalendar — OnCalendar actual de un timer ('systemctl show -p OnCalendar').
func timerOnCalendar(ctx context.Context, unit string) string {
	out, err := executil.RunDirect(ctx, 5*time.Second,
		"systemctl", "show", "-p", "OnCalendar", "--value", unit)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// zfsTaskRe — una tarea del sistema "es ZFS" si su unidad, comando u origen
// mencionan zfs/zpool o las herramientas clásicas del ecosistema (sanoid,
// syncoid, znapzend, zrepl, zed, zfs-auto-snapshot). Deliberadamente NO se
// filtra por "scrub"/"snapshot" a secas: xfs_scrub_all y e2scrub_all no son ZFS.
var zfsTaskRe = regexp.MustCompile(`(?i)\b(zfs|zpool|zed|sanoid|syncoid|znapzend|zrepl|zfs-auto-snap[a-z-]*)\b`)

// isZFSTask decide si una tarea del sistema está relacionada con ZFS.
func isZFSTask(t model.SysTimer) bool {
	return zfsTaskRe.MatchString(t.Name) ||
		zfsTaskRe.MatchString(t.Command) ||
		zfsTaskRe.MatchString(t.Schedule)
}

// sysdTimerJSON — subconjunto tolerante de `systemctl list-timers --output=json`
// (systemd ≥ 249; encoding/json casa las claves sin distinguir mayúsculas).
// OJO: systemd nuevo (≥ 256) emite next/left/last/passed como NÚMEROS en µs
// (epoch para next/last, duración para left/passed) en vez de strings.
type sysdTimerJSON struct {
	Next      json.RawMessage `json:"next"`
	Left      json.RawMessage `json:"left"`
	Last      json.RawMessage `json:"last"`
	Passed    json.RawMessage `json:"passed"`
	Unit      string          `json:"unit"`
	Activates string          `json:"activates"`
}

// sysdText convierte un campo temporal de systemd a string legible: acepta
// el string de systemd viejo tal cual y los µs epoch de systemd nuevo.
func sysdText(r json.RawMessage) string {
	if len(r) == 0 || string(r) == "null" {
		return ""
	}
	var s string
	if err := json.Unmarshal(r, &s); err == nil {
		return s
	}
	var usec int64
	if err := json.Unmarshal(r, &usec); err == nil && usec > 0 {
		return time.UnixMicro(usec).Format("2006-01-02 15:04:05")
	}
	return ""
}

// systemdTimers — timers de systemd vía `systemctl list-timers --all
// --output=json`. Si el binario no existe o no soporta JSON, lista vacía + log
// (sin fallback a parsear texto: el formato es localizado y frágil).
func (c *SchedSysCollector) systemdTimers(ctx context.Context) []model.SysTimer {
	out, err := executil.RunDirect(ctx, 10*time.Second,
		"systemctl", "list-timers", "--all", "--output=json")
	if err != nil {
		log.Printf("schedsys: systemctl list-timers: %v", err)
		return nil
	}
	var raw []sysdTimerJSON
	if err := json.Unmarshal(out, &raw); err != nil {
		log.Printf("schedsys: systemctl list-timers JSON not supported: %v", err)
		return nil
	}
	timers := make([]model.SysTimer, 0, len(raw))
	for _, t := range raw {
		if t.Unit == "" {
			continue
		}
		timers = append(timers, model.SysTimer{
			Source:   "systemd",
			Name:     t.Unit,
			NextRun:  sysdText(t.Next),
			LastRun:  sysdText(t.Last),
			Command:  t.Activates,
			Origin:   "systemctl list-timers",
			Editable: strings.HasSuffix(t.Unit, ".timer"),
		})
	}
	return timers
}

// cronEntries — cron del sistema: `crontab -l` (usuario del servicio; si no,
// root vía sudo -n), /etc/crontab, /etc/cron.d/* y los directorios
// cron.{hourly,daily,weekly,monthly} (sin horario exacto: @hourly/@daily/…).
func (c *SchedSysCollector) cronEntries(ctx context.Context) []model.SysTimer {
	out := []model.SysTimer{}

	// crontab del usuario del servicio (directo) o de root (vía sudo -n).
	if data, err := executil.RunDirect(ctx, 5*time.Second, "crontab", "-l"); err == nil {
		out = append(out, parseCrontab(string(data), "crontab", false)...)
	} else if executil.SudoEnabled() {
		if data, err2 := executil.Run(ctx, 5*time.Second, "crontab", "-l"); err2 == nil {
			out = append(out, parseCrontab(string(data), "crontab (root)", false)...)
		}
	}

	// Ficheros de sistema: incluyen campo de usuario entre schedule y comando.
	for _, f := range cronFiles() {
		data, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		out = append(out, parseCrontab(string(data), f, true)...)
	}

	// Directorios de periodicidad fija: no hay horario exacto, solo el periodo.
	for dir, sched := range map[string]string{
		"/etc/cron.hourly":  "@hourly",
		"/etc/cron.daily":   "@daily",
		"/etc/cron.weekly":  "@weekly",
		"/etc/cron.monthly": "@monthly",
	} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue // sin cron instalado o directorio ausente: tolerado
		}
		for _, e := range entries {
			if e.IsDir() || strings.HasPrefix(e.Name(), ".") {
				continue
			}
			out = append(out, model.SysTimer{
				Source:   "cron",
				Name:     e.Name(),
				Schedule: sched,
				Command:  filepath.Join(dir, e.Name()),
				Origin:   dir,
			})
		}
	}
	return out
}

// cronFiles — /etc/crontab + /etc/cron.d/* (ficheros regulares visibles).
func cronFiles() []string {
	files := []string{}
	if st, err := os.Stat("/etc/crontab"); err == nil && st.Mode().IsRegular() {
		files = append(files, "/etc/crontab")
	}
	matches, _ := filepath.Glob("/etc/cron.d/*")
	for _, m := range matches {
		base := filepath.Base(m)
		if strings.HasPrefix(base, ".") || strings.ContainsAny(base, "~#") {
			continue // ocultos y backups de editores
		}
		if st, err := os.Stat(m); err == nil && st.Mode().IsRegular() {
			files = append(files, m)
		}
	}
	return files
}

// parseCrontab extrae las líneas activas de un crontab: ignora comentarios,
// asignaciones de entorno (VAR=valor) y líneas vacías. Con hasUser=true
// (ficheros de /etc) hay un campo de usuario entre el schedule y el comando.
func parseCrontab(content, origin string, hasUser bool) []model.SysTimer {
	out := []model.SysTimer{}
	editable := strings.HasPrefix(origin, "/etc/")
	lineNo := 0
	for _, line := range strings.Split(content, "\n") {
		lineNo++
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "@") {
			// Forma especial: @daily [@reboot…] [user] comando
			fields := strings.Fields(line)
			if len(fields) < 2 {
				continue
			}
			cmd := fields[1:]
			if hasUser && len(cmd) > 1 {
				cmd = cmd[1:]
			}
			command := strings.Join(cmd, " ")
			out = append(out, model.SysTimer{
				Source: "cron", Name: cronName(command, origin), Schedule: fields[0],
				Command: command, Origin: origin, Line: lineNo, Editable: editable,
			})
			continue
		}
		fields := strings.Fields(line)
		need := 6 // 5 campos de schedule + comando
		if hasUser {
			need = 7 // + campo de usuario
		}
		if len(fields) < need {
			continue // asignación de entorno (VAR=valor) o línea malformada
		}
		if strings.Contains(fields[0], "=") {
			continue // asignación de entorno
		}
		cmd := fields[5:]
		if hasUser {
			cmd = fields[6:]
		}
		schedule := strings.Join(fields[:5], " ")
		command := strings.Join(cmd, " ")
		out = append(out, model.SysTimer{
			Source: "cron", Name: cronName(command, origin), Schedule: schedule,
			Command: command, Origin: origin, Line: lineNo, Editable: editable,
		})
	}
	return out
}

// cronName deriva un nombre legible: base del primer ejecutable del comando.
func cronName(command, origin string) string {
	fields := strings.Fields(command)
	if len(fields) == 0 {
		return origin
	}
	first := filepath.Base(fields[0])
	// Comandos envueltos en shell ("if [ … ]; then /ruta/real; fi"): el
	// primer token es ruido sintáctico; mejor el primer path absoluto.
	switch first {
	case "if", "[", "test", "then", "(", "env":
		for _, f := range fields[1:] {
			if strings.HasPrefix(f, "/") {
				return filepath.Base(f)
			}
		}
		return origin
	}
	return first
}
