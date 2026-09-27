// events.go — alertas en tiempo real desde los eventos ZFS: proceso persistente
// 'zpool events -f' (contexto largo, SIN timeout) cuya salida se parsea por
// bloques. Self-contained: no toca la configuración de zed del sistema.
//
// Reconexión con backoff si el proceso muere; si 'zpool events' no está
// disponible (permisos, zfs ausente, dos arranques que mueren al instante) el
// colector se desactiva con un log y el resto del sistema sigue igual (el
// polling de los colectores queda como red de seguridad).
package collectors

import (
	"bufio"
	"context"
	"io"
	"log"
	"regexp"
	"strconv"
	"strings"
	"time"

	"easyzfs/internal/alerts"
	"easyzfs/internal/executil"
)

const (
	eventsMinBackoff = 5 * time.Second
	eventsMaxBackoff = 2 * time.Minute
	// Si el proceso muere antes de este tiempo de vida se considera fallo de
	// arranque (zpool events no disponible); tras 2 seguidos se desactiva.
	eventsMinAlive = 3 * time.Second
)

// kvRe — líneas 'clave = "valor"' o 'clave = 0x…' de los bloques de eventos.
var kvRe = regexp.MustCompile(`^\s*([a-zA-Z0-9_.]+)\s*=\s*(?:"([^"]*)"|(\S+))\s*$`)

// parseEventBlock extrae los pares clave=valor de un bloque de evento
// (tolerante: primera aparición de cada clave; cabeceras TIME/fecha no casan).
func parseEventBlock(block string) map[string]string {
	out := map[string]string{}
	for _, line := range strings.Split(block, "\n") {
		m := kvRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		key := m[1]
		val := m[2]
		if m[3] != "" {
			val = m[3]
		}
		if _, dup := out[key]; !dup {
			out[key] = val
		}
	}
	return out
}

// devShort — 'vdev' del evento a nombre corto de disco para el target de la
// UI: '/dev/sdb1'→'sdb', 'nvme0n1p2'→'nvme0n1', 'ata-XXX-part1'→'ata-XXX'.
func devShort(v string) string {
	v = strings.TrimPrefix(v, "/dev/")
	v = strings.TrimSuffix(v, ".eli")
	if i := strings.LastIndex(v, "-part"); i > 0 && allDigitsStr(v[i+5:]) {
		return v[:i]
	}
	if i := strings.LastIndex(v, "p"); i > 0 && allDigitsStr(v[i+1:]) && !allDigitsStr(v[:i]) {
		return v[:i]
	}
	for _, pre := range []string{"xvd", "sd", "vd", "hd"} {
		if strings.HasPrefix(v, pre) {
			rest := v[len(pre):]
			j := len(rest)
			for j > 0 && rest[j-1] >= '0' && rest[j-1] <= '9' {
				j--
			}
			if j < len(rest) && j > 0 {
				return pre + rest[:j]
			}
			return v
		}
	}
	return v
}

func allDigitsStr(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// EventsCollector — sigue 'zpool events -f' y convierte eventos en alertas.
type EventsCollector struct {
	al *alerts.Alerter
}

// NewEventsCollector crea el colector de eventos ZFS.
func NewEventsCollector(al *alerts.Alerter) *EventsCollector {
	return &EventsCollector{al: al}
}

// Name implementa Collector.
func (c *EventsCollector) Name() string { return "events" }

// Run — bucle de reconexión con backoff exponencial; se desactiva tras 2
// arranques fallidos seguidos (zpool events no disponible en este host).
func (c *EventsCollector) Run(ctx context.Context) {
	backoff := eventsMinBackoff
	badStarts := 0
	for {
		started := time.Now()
		err := c.follow(ctx)
		if ctx.Err() != nil {
			return
		}
		alive := time.Since(started)
		if alive < eventsMinAlive {
			badStarts++
			if badStarts >= 2 {
				log.Printf("events: 'zpool events' no disponible (%v); colector desactivado (capability off)", err)
				return
			}
		} else {
			badStarts = 0
		}
		log.Printf("events: proceso terminado (%v); reconexión en %s", err, backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(2*backoff, eventsMaxBackoff)
	}
}

// follow lanza 'zpool events -f' y consume bloques hasta que muera el proceso
// o se cancele el contexto.
func (c *EventsCollector) follow(ctx context.Context) error {
	cmd := executil.NewCommand(ctx, "zpool", "events", "-f")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() {
		scanEvents(stdout, func(block string) {
			c.dispatch(ctx, parseEventBlock(block))
		})
		done <- cmd.Wait()
	}()
	select {
	case <-ctx.Done():
		// The context's end already sent SIGTERM to the process group
		// (executil), which sudo relays to the root zpool. A SIGKILL here used
		// to race it: sudo died first, the root child survived holding the
		// pipe open, the scanner never saw EOF and shutdown hung until
		// systemd's stop timeout. If the group has not gone after the grace
		// period, close our end so the reader, and so Wait, can return.
		select {
		case <-done:
		case <-time.After(executil.TermGrace):
			_ = stdout.Close()
			<-done
		}
		return ctx.Err()
	case err := <-done:
		return err
	}
}

// scanEvents lee bloques separados por líneas en blanco y los entrega a fn.
func scanEvents(r io.Reader, fn func(string)) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	var b strings.Builder
	flush := func() {
		if b.Len() > 0 {
			fn(b.String())
			b.Reset()
		}
	}
	for sc.Scan() {
		line := sc.Text()
		if strings.TrimSpace(line) == "" {
			flush()
			continue
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	flush()
}

// dispatch mapea un evento ZFS a alerta (o lo ignora: config_sync, trim_*,
// scrub_start y cualquier clase desconocida son ruido → solo log debug).
// Source "zed.<class>": distinto del polling ("pool.<name>", "scrub.<name>")
// para que el dedupe por source+message no colisione entre vías.
func (c *EventsCollector) dispatch(ctx context.Context, ev map[string]string) {
	class := ev["class"]
	if class == "" {
		return
	}
	pool := ev["pool"]
	vdev := ev["vdev"]
	poolTarget := ""
	if pool != "" {
		poolTarget = "pools:" + pool
	}
	// target: disco si hay vdev, pool si no
	target := poolTarget
	if vdev != "" {
		target = "disks:" + devShort(vdev)
	}
	// Para los textos push: si no hay vdev, muestra el pool como "dispositivo".
	devLabel := vdev
	if devLabel == "" {
		devLabel = pool
	}
	params := map[string]any{"pool": pool, "vdev": devLabel}

	switch class {
	case "ereport.fs.zfs.io", "ereport.fs.zfs.checksum", "ereport.fs.zfs.data":
		kind := map[string]string{
			"ereport.fs.zfs.io":       "zfs_io_error",
			"ereport.fs.zfs.checksum": "zfs_checksum_error",
			"ereport.fs.zfs.data":     "zfs_data_error",
		}[class]
		what := "E/S"
		if class == "ereport.fs.zfs.checksum" {
			what = "checksum"
		} else if class == "ereport.fs.zfs.data" {
			what = "datos"
		}
		where := vdev
		if where == "" {
			where = pool
		}
		c.al.RaiseKind(ctx, "crit", "zed."+class, target,
			"Errores de "+what+" en "+where+" (evento ZFS, pool "+pool+")",
			kind, params)
	case "ereport.fs.zfs.deadman", "ereport.fs.zfs.delay":
		kind := "zfs_io_delay"
		what := "E/S lenta (delay)"
		if class == "ereport.fs.zfs.deadman" {
			kind = "zfs_deadman"
			what = "E/S colgada (deadman)"
		}
		where := vdev
		if where == "" {
			where = pool
		}
		c.al.RaiseKind(ctx, "warn", "zed."+class, target,
			what+" en "+where+" (evento ZFS, pool "+pool+")",
			kind, params)
	case "sysevent.fs.zfs.resilver_start":
		c.al.RaiseKind(ctx, "info", "zed."+class, poolTarget,
			"Resilver iniciado en el pool "+pool,
			"resilver_start", params)
	case "sysevent.fs.zfs.resilver_finish":
		c.al.RaiseKind(ctx, "info", "zed."+class, poolTarget,
			"Resilver del pool "+pool+" terminado",
			"resilver_finish", params)
	case "sysevent.fs.zfs.scrub_finish":
		if n, _ := strconv.ParseInt(ev["errors"], 10, 64); n > 0 {
			c.al.RaiseKind(ctx, "warn", "zed."+class, poolTarget,
				"Scrub de "+pool+" terminó con "+strconv.FormatInt(n, 10)+" errores (evento ZFS)",
				"scrub_errors", map[string]any{"pool": pool, "errors": n})
		}
	case "sysevent.fs.zfs.vdev_statechange":
		state := ev["vdev_state"]
		if state == "" {
			state = ev["state"]
		}
		if state == "FAULTED" || state == "DEGRADED" {
			where := vdev
			if where == "" {
				where = pool
			}
			c.al.RaiseKind(ctx, "crit", "zed."+class, target,
				"El vdev "+where+" pasó a "+state+" (pool "+pool+")",
				"vdev_state", map[string]any{"pool": pool, "vdev": vdev, "state": state})
		}
	default:
		// config_sync, trim_start/finish, scrub_start, desconocidos: ruido.
		log.Printf("events: %s pool=%s (sin alerta)", class, pool)
	}
}
