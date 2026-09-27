// capabilities.go — detección de la versión de OpenZFS y capacidades derivadas
// (feature-gating). Se sondea al arranque y cada hora por si el sistema se
// actualiza (la UI consulta las capacidades vía GET /api/version).
package collectors

import (
	"context"
	"log"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"easyzfs/internal/executil"
	"easyzfs/internal/model"
)

const capsInterval = time.Hour

// versionRe extrae X.Y.Z de líneas como 'zfs-2.3.2-2' o 'zfs-kmod-2.3.2-2'.
var versionRe = regexp.MustCompile(`(\d+)\.(\d+)\.(\d+)`)

// parseVersion extrae (major, minor, patch) de una salida de 'zfs version'
// (puede traer varias líneas; vale la primera coincidencia).
func parseVersion(out string) (int, int, int, bool) {
	m := versionRe.FindStringSubmatch(out)
	if m == nil {
		return 0, 0, 0, false
	}
	maj, _ := strconv.Atoi(m[1])
	min, _ := strconv.Atoi(m[2])
	pat, _ := strconv.Atoi(m[3])
	return maj, min, pat, true
}

// atLeast compara (maj,min,pat) con un mínimo semver.
func atLeast(maj, min, pat, rmaj, rmin, rpat int) bool {
	if maj != rmaj {
		return maj > rmaj
	}
	if min != rmin {
		return min > rmin
	}
	return pat >= rpat
}

// capsFromVersion deriva las capacidades conocidas de OpenZFS:
//   - rewrite (zfs rewrite): Linux ≥ 2.3.4
//   - raidz_expansion / --json en zpool status / zpool wait -t raidz_expand: ≥ 2.3
//   - scrub -a (todos los pools) y scrub -S/-E (rango): ≥ 2.4
//   - zarcsummary/zarcstat (nombres nuevos): ≥ 2.4
func capsFromVersion(maj, min, pat int, version string) model.Capabilities {
	return model.Capabilities{
		Rewrite:        atLeast(maj, min, pat, 2, 3, 4),
		RaidzExpansion: atLeast(maj, min, pat, 2, 3, 0),
		ScrubAll:       atLeast(maj, min, pat, 2, 4, 0),
		ScrubRange:     atLeast(maj, min, pat, 2, 4, 0),
		ZarcNames:      atLeast(maj, min, pat, 2, 4, 0),
		JSONOutput:     atLeast(maj, min, pat, 2, 3, 0),
		Version:        version,
	}
}

// CapabilitiesFromOutput — pura, testeable: de la salida de 'zfs version'.
func CapabilitiesFromOutput(out string) model.Capabilities {
	if maj, min, pat, ok := parseVersion(out); ok {
		ver := mjoin(maj, min, pat)
		return capsFromVersion(maj, min, pat, ver)
	}
	return model.Capabilities{Version: "desconocida"}
}

// mjoin formatea "2.3.4".
func mjoin(maj, min, pat int) string {
	return strconv.Itoa(maj) + "." + strconv.Itoa(min) + "." + strconv.Itoa(pat)
}

// CapsCollector — sondeo horario de la versión de OpenZFS del host.
type CapsCollector struct {
	mu   sync.RWMutex
	caps model.Capabilities
}

// NewCapsCollector crea el colector (primera pasada diferida a Run/Refresh).
func NewCapsCollector() *CapsCollector { return &CapsCollector{} }

// Name implementa Collector.
func (c *CapsCollector) Name() string { return "caps" }

// Run — pasada inmediata y luego cada capsInterval.
func (c *CapsCollector) Run(ctx context.Context) {
	c.Refresh(ctx)
	t := time.NewTicker(capsInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			c.Refresh(ctx)
		}
	}
}

// Refresh ejecuta el sondeo ahora (también tras arranque del proceso).
func (c *CapsCollector) Refresh(ctx context.Context) {
	out, err := executil.RunRead(ctx, 5*time.Second, "zfs", "version")
	if err != nil {
		// Algunas distros solo responden a 'zpool --version'.
		out, err = executil.RunRead(ctx, 5*time.Second, "zpool", "--version")
		if err != nil {
			log.Printf("caps: %v", err)
			return
		}
	}
	caps := CapabilitiesFromOutput(string(out))
	c.mu.Lock()
	c.caps = caps
	c.mu.Unlock()
}

// Capabilities implementa CapProvider (copia defensiva).
func (c *CapsCollector) Capabilities() model.Capabilities {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.caps
}

// DetectZFSVersion — versión de OpenZFS del host para /api/version.
func DetectZFSVersion(ctx context.Context) string {
	out, err := executil.RunRead(ctx, 5*time.Second, "zpool", "--version")
	if err != nil {
		return "desconocida"
	}
	// formato: 'zfs-2.2.6-1\nzfs-kmod-2.2.6-1'
	first, _, _ := strings.Cut(strings.TrimSpace(string(out)), "\n")
	return strings.TrimPrefix(first, "zfs-")
}
