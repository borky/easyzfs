// props.go — tabla de propiedades editables de datasets (U3, fase P1).
// Whitelist ESTRICTA de propiedades y valores: NUNCA se interpola un nombre
// o valor en el argv de zfs sin pasar la validación (patrón del proyecto).
// Solo propiedades seguras y útiles; las delicadas (dedup, encryption,
// keyformat…) quedan FUERA. `zfs get all` expone muchas más (read-only y
// user properties) — se listan en GET pero no son editables vía PATCH.
package actions

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"fmt"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"easyzfs/internal/executil"
	"easyzfs/internal/model"
)

// ErrNotLocal — la propiedad no es local, no se puede heredar (409).
var ErrNotLocal = errors.New("la propiedad no es local (no se puede heredar)")

// propKind — tipo de validación de una propiedad editable.
type propKind int

const (
	propBool propKind = iota // "on" | "off"
	propEnum                 // pertenencia exacta a una lista
	propSize                 // "none" (si sizeNone) o número con sufijo K/M/G/T…
	propSizePow2             // potencia de 2 entre min/max (recordsize, volblocksize)
	propPath                 // ruta absoluta o none|legacy
)

// propSpec — definición de una propiedad editable.
type propSpec struct {
	kind     propKind
	enum     []string // propEnum
	sizeNone bool     // propSize: admite "none" (quota, reservation)
	minBytes uint64   // propSizePow2: cota inferior
	maxBytes uint64   // propSizePow2: cota superior
	fsOnly   bool     // solo aplica a filesystem (no volume)
	volOnly  bool     // solo aplica a volume (no filesystem)
}

// propValidators — whitelist de propiedades editables. Cada propiedad nueva
// que se quiera exponer DEBE añadirse aquí con su tipo y validación.
var propValidators = map[string]propSpec{
	"compression":     {kind: propEnum, enum: []string{"lz4", "zstd", "zlib", "gzip", "gzip-1", "gzip-2", "gzip-3", "gzip-4", "gzip-5", "gzip-6", "gzip-7", "gzip-8", "gzip-9", "lzjb", "off"}},
	"recordsize":      {kind: propSizePow2, minBytes: 4 << 10, maxBytes: 16 << 20},
	"atime":           {kind: propBool},
	"relatime":        {kind: propBool},
	"sync":            {kind: propEnum, enum: []string{"standard", "always", "disabled"}},
	// No "off" (it disables corruption detection) and no "fletcher2"
	// (deprecated by OpenZFS as too weak): neither has a use worth the risk.
	"checksum":        {kind: propEnum, enum: []string{"on", "fletcher4", "sha256"}},
	"copies":          {kind: propEnum, enum: []string{"1", "2", "3"}},
	"xattr":           {kind: propEnum, enum: []string{"on", "off", "sa"}},
	"acltype":         {kind: propEnum, enum: []string{"off", "posix", "nfsv4"}},
	"aclinherit":      {kind: propEnum, enum: []string{"discard", "noallow", "restricted", "passthrough", "passthrough-x"}},
	"primarycache":    {kind: propEnum, enum: []string{"all", "none", "metadata"}},
	"secondarycache":  {kind: propEnum, enum: []string{"all", "none", "metadata"}},
	"logbias":         {kind: propEnum, enum: []string{"latency", "throughput"}},
	"canmount":        {kind: propEnum, enum: []string{"on", "off", "noauto"}},
	"mountpoint":      {kind: propPath, fsOnly: true},
	"exec":            {kind: propBool},
	"setuid":          {kind: propBool},
	"devices":         {kind: propBool},
	"readonly":        {kind: propBool},
	"snapdir":         {kind: propEnum, enum: []string{"hidden", "visible"}},
	"quota":           {kind: propSize, sizeNone: true, fsOnly: true},
	"reservation":     {kind: propSize, sizeNone: true, fsOnly: true},
	"volsize":         {kind: propSize, volOnly: true},
	"volblocksize":    {kind: propSizePow2, minBytes: 512, maxBytes: 128 << 10, volOnly: true},
}

// reSize — número con sufijo opcional K/M/G/T/P/E (o sin él = bytes),
// con la 'i' y 'B' opcionales que zfs acepta ("500G", "1TiB", "128K").
var reSize = regexp.MustCompile(`^[0-9]+([KMGTPE]i?B?)?$`)

// reMountpoint — ruta absoluta simple (whitelist estricta: sin espacios,
// sin ';' ni metacharacteres; también admite none|legacy).
var reMountpoint = regexp.MustCompile(`^/[a-zA-Z0-9_./\-]+$`)

// systemMountpoints — roots where mounting a dataset shadows binaries,
// credentials, units or root-run scripts, leaving the system recoverable only
// from a console. Both the root itself and anything below it are refused.
// /var/lib/easyzfs and /opt/easyzfs are EasyZFS's own: the root update unit
// installs whatever sits in <datadir>/update, and the optional weekly updater
// runs a script from /opt/easyzfs as root. main adds the configured data dir
// too, since DB_PATH can move it (see ProtectMountpoint).
var systemMountpoints = []string{
	"/etc", "/bin", "/sbin", "/lib", "/lib32", "/lib64", "/libx32", "/usr",
	"/boot", "/dev", "/proc", "/sys", "/run", "/root",
	"/var/spool/cron", "/var/lib/dpkg", "/var/lib/easyzfs", "/opt/easyzfs",
}

// exactMountpoints — refused as a mountpoint themselves, but mounting below
// them is normal (/var/lib/docker, /var/log/archive…), so only the exact path
// is denied. /home, /opt, /srv, /mnt and /tmp stay allowed entirely.
var exactMountpoints = []string{"/var", "/var/lib", "/var/log"}

// ProtectMountpoint adds a path, and everything below it, to the refused
// mountpoints. main registers the configured data directory with it. A
// relative path is made absolute, and when the path resolves through a
// symlink both spellings are protected: mount(2) would follow the link.
func ProtectMountpoint(p string) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return
	}
	add := func(q string) {
		if q = path.Clean(q); strings.HasPrefix(q, "/") && q != "/" {
			systemMountpoints = append(systemMountpoints, q)
		}
	}
	add(abs)
	if r, err := filepath.EvalSymlinks(abs); err == nil && r != abs {
		add(r)
	}
}

// deniedMountpoint reports whether an absolute, cleaned path is refused:
// the root, an exact-only path, a protected root or anything below one, or an
// ancestor of a protected root (mounting on /opt hides /opt/easyzfs just as
// surely as mounting on /opt/easyzfs does).
func deniedMountpoint(p string) bool {
	if p == "/" {
		return true
	}
	for _, e := range exactMountpoints {
		if p == e {
			return true
		}
	}
	for _, sys := range systemMountpoints {
		if p == sys || strings.HasPrefix(p, sys+"/") || strings.HasPrefix(sys, p+"/") {
			return true
		}
	}
	return false
}

// allowedMountRoots — besides the dataset's own pool tree (/<pool> and
// below), the only places a dataset may be mounted explicitly, and only
// strictly below them: /srv/<name>, /mnt/<name>… This is an allowlist on
// purpose. A denylist of dangerous places kept missing some (/var/lib/dpkg,
// ancestors such as /opt, links such as /var/run), and none of these trees is
// where root-run code or credentials live.
var allowedMountRoots = []string{"/mnt", "/media", "/srv", "/home"}

// mountTrustedUID — the only owner a directory on the way to a mountpoint may
// have. A test seam; root in production.
var mountTrustedUID = 0

// poolRootMountpoint returns where the pool's root dataset is mounted, or ""
// if that cannot be read or is not a usable path. A pool is not always at
// /<pool>: its root may carry mountpoint=/data, and its children then live
// under /data. A variable so tests need no zfs.
var poolRootMountpoint = func(ctx context.Context, pool string) string {
	out, err := executil.Run(ctx, 10*time.Second, "zfs", "get", "-H", "-o", "value", "mountpoint", pool)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// poolTrees — the pool's own trees: /<pool>, and the root dataset's actual
// mountpoint when it is elsewhere. A root mounted at "/" (or anything that is
// not a plain absolute path, such as none or legacy) is ignored: it would put
// the whole filesystem on the allowlist.
func poolTrees(ctx context.Context, pool string) []string {
	if pool == "" {
		return nil
	}
	trees := []string{"/" + pool}
	if mp := poolRootMountpoint(ctx, pool); reMountpoint.MatchString(mp) {
		if mp = path.Clean(mp); mp != "/" && mp != "/"+pool {
			trees = append(trees, mp)
		}
	}
	return trees
}

// underAllowedRoot — the lexical allowlist check.
func underAllowedRoot(p string, trees []string) bool {
	for _, t := range trees {
		if p == t || strings.HasPrefix(p, t+"/") {
			return true
		}
	}
	for _, r := range allowedMountRoots {
		if strings.HasPrefix(p, r+"/") {
			return true
		}
	}
	return false
}

// trustedDir — owned by root and writable by nobody else. Anyone else who
// can write to a directory on the way can swap the next component for a
// symlink between this check and the moment root mounts (at set time or at
// the next boot), and mount(2) would follow it. Same rule as sshd's
// StrictModes.
func trustedDir(dir string) error {
	fi, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok || !fi.IsDir() || int(st.Uid) != mountTrustedUID || fi.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("%s no es un directorio de root protegido contra escritura", dir)
	}
	return nil
}

// checkMountpointPath walks p from base down. Every directory it passes
// through must be trusted, and no existing component may be a symlink. It
// stops at the first component that does not exist yet: zfs will create the
// rest as root, and the trusted parent means nobody else can get there first.
// A component that cannot be inspected (EACCES for the service account) is
// refused: root's mount would enter it all the same.
func checkMountpointPath(base, p string) error {
	cur := base
	rel := strings.TrimPrefix(strings.TrimPrefix(p, base), "/")
	for _, part := range strings.Split(rel, "/") {
		if err := trustedDir(cur); err != nil {
			return err
		}
		next := filepath.Join(cur, part)
		fi, err := os.Lstat(next)
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("no se puede inspeccionar %s: %w", next, err)
		}
		if fi.Mode()&fs.ModeSymlink != 0 {
			return fmt.Errorf("%s es un enlace simbólico", next)
		}
		cur = next
	}
	return nil
}

// checkMountpoint — full validation of an explicit mountpoint for a dataset:
// syntax, the allowlist, the system denylist (kept underneath as a second
// layer: a pool may legally be named "etc"), and the path-trust walk. The
// error says why, so the admin is not left guessing.
//
// Known limit: only an explicit mountpoint passes through here. One that is
// inherited, e.g. by a dataset created beneath a root-on-ZFS dataset mounted
// at /, does not.
func checkMountpoint(ctx context.Context, v, dataset string) error {
	if v == "none" || v == "legacy" {
		return nil
	}
	if !reMountpoint.MatchString(v) {
		return fmt.Errorf("%w: mountpoint no válido: %q", ErrInvalidInput, v)
	}
	clean := path.Clean(v)
	pool, _, _ := strings.Cut(dataset, "/")
	trees := poolTrees(ctx, pool)
	if !underAllowedRoot(clean, trees) {
		return fmt.Errorf("%w: mountpoint fuera de las rutas permitidas (%s, /mnt/…, /media/…, /srv/…, /home/…): %s",
			ErrInvalidInput, strings.Join(trees, ", "), clean)
	}
	if deniedMountpoint(clean) {
		return fmt.Errorf("%w: mountpoint en una ruta de sistema: %s", ErrInvalidInput, clean)
	}
	if err := checkMountpointPath("/", clean); err != nil {
		return fmt.Errorf("%w: mountpoint no seguro: %v", ErrInvalidInput, err)
	}
	return nil
}

// valid — comprueba que el valor es admisible para la propiedad.
func (p propSpec) valid(v string) bool {
	switch p.kind {
	case propBool:
		return v == "on" || v == "off"
	case propEnum:
		for _, e := range p.enum {
			if v == e {
				return true
			}
		}
		return false
	case propSize:
		if p.sizeNone && v == "none" {
			return true
		}
		return reSize.MatchString(v)
	case propSizePow2:
		n, ok := parseSize(v)
		if !ok || n < p.minBytes || n > p.maxBytes {
			return false
		}
		return n&(n-1) == 0 // potencia de 2
	case propPath:
		// Syntax only: where it may point depends on the dataset's pool and
		// on the filesystem, so DatasetPropSet calls checkMountpoint as well.
		return v == "none" || v == "legacy" || reMountpoint.MatchString(v)
	}
	return false
}

// appliesTo — la propiedad es aplicable al tipo de dataset.
func (p propSpec) appliesTo(dsType string) bool {
	if p.fsOnly && dsType == "volume" {
		return false
	}
	if p.volOnly && dsType == "fs" {
		return false
	}
	return true
}

// parseSize — convierte un tamaño zfs ("500G", "1TiB", "1024") a bytes.
func parseSize(v string) (uint64, bool) {
	if v == "" {
		return 0, false
	}
	s := v
	// Sufijo opcional "iB" / "B" (p. ej. "1TiB", "500GB").
	for _, suf := range []string{"iB", "B"} {
		if strings.HasSuffix(s, suf) {
			s = strings.TrimSuffix(s, suf)
			break
		}
	}
	mult := uint64(1)
	if len(s) > 0 {
		switch s[len(s)-1] {
		case 'K':
			mult, s = 1<<10, s[:len(s)-1]
		case 'M':
			mult, s = 1<<20, s[:len(s)-1]
		case 'G':
			mult, s = 1<<30, s[:len(s)-1]
		case 'T':
			mult, s = 1<<40, s[:len(s)-1]
		case 'P':
			mult, s = 1<<50, s[:len(s)-1]
		case 'E':
			mult, s = 1<<60, s[:len(s)-1]
		}
	}
	n, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		return 0, false
	}
	return n * mult, true
}

// describeKind — mensaje legible del tipo de valor esperado (para errores).
func (p propSpec) describeKind() string {
	switch p.kind {
	case propBool:
		return "on|off"
	case propEnum:
		return "uno de: " + strings.Join(p.enum, ", ")
	case propSize:
		return "tamaño (none o número con sufijo K/M/G/T)"
	case propSizePow2:
		return fmt.Sprintf("potencia de 2 entre %d y %d bytes", p.minBytes, p.maxBytes)
	case propPath:
		return "none, legacy, o una ruta bajo /<pool>, /mnt, /media, /srv o /home"
	}
	return "valor válido"
}

// DatasetPropsGet — 'zfs get -H -o name,property,value,source all <ds>'.
// Lista TODAS las propiedades (nativas + user). El front agrupa por
// propSource; PATCH solo acepta las de la whitelist.
func (s *Service) DatasetPropsGet(ctx context.Context, name string) ([]model.DatasetProp, error) {
	if !reDataset.MatchString(name) {
		return nil, ErrInvalidName
	}
	out, err := executil.RunRead(ctx, 10*time.Second, "zfs",
		"get", "-H", "-o", "name,property,value,source", "all", name)
	if err != nil {
		return nil, fmt.Errorf("zfs get properties: %w", err)
	}
	props := []model.DatasetProp{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line == "" {
			continue
		}
		f := strings.Split(line, "\t")
		if len(f) < 4 {
			continue
		}
		props = append(props, model.DatasetProp{Name: f[1], Value: f[2], Source: f[3]})
	}
	return props, nil
}

// DatasetPropSet — 'zfs set <property>=<value> <ds>' (admin). Whitelist
// estricta de propiedad y valor; la propiedad debe aplicar al tipo.
func (s *Service) DatasetPropSet(ctx context.Context, actor, name, property, value, dsType string, ackRisk bool) error {
	if !reDataset.MatchString(name) {
		return ErrInvalidName
	}
	spec, ok := propValidators[property]
	if !ok {
		return fmt.Errorf("%w: propiedad no editable (%s)", ErrInvalidInput, property)
	}
	if dsType != "" && !spec.appliesTo(dsType) {
		return fmt.Errorf("%w: %s no aplica a un %s", ErrInvalidInput, property, dsType)
	}
	if !spec.valid(value) {
		return fmt.Errorf("%w: valor inválido para %s (%s)", ErrInvalidInput, property, spec.describeKind())
	}
	if spec.kind == propPath {
		if err := checkMountpoint(ctx, value, name); err != nil {
			return err
		}
	}
	risk := PropRisk(property, value)
	if property == "volsize" && s.volsizeShrinks(ctx, name, value) {
		risk = "volsize_shrink"
	}
	if risk != "" && !ackRisk {
		return fmt.Errorf("%w: %s", ErrRiskAck, riskText[risk])
	}
	s.audit(ctx, actor, "dataset.setprop", name,
		map[string]any{"property": property, "value": value}, false)
	if _, err := executil.Run(ctx, 15*time.Second, "zfs", "set", property+"="+value, name); err != nil {
		return fmt.Errorf("zfs set %s: %w", property, err)
	}
	return nil
}

// DatasetPropInherit — 'zfs inherit <property> <ds>' (admin). Solo para
// propiedades de la whitelist con source == "local" (el handler comprueba
// el source contra la última lectura; si está obsoleta, zfs hace un no-op).
func (s *Service) DatasetPropInherit(ctx context.Context, actor, name, property string, ackRisk bool) error {
	if !reDataset.MatchString(name) {
		return ErrInvalidName
	}
	if _, ok := propValidators[property]; !ok {
		return fmt.Errorf("%w: propiedad no editable (%s)", ErrInvalidInput, property)
	}
	if risk := inheritRisk[property]; risk != "" && !ackRisk {
		return fmt.Errorf("%w: %s", ErrRiskAck, riskText[risk])
	}
	s.audit(ctx, actor, "dataset.inherit", name, map[string]any{"property": property}, false)
	if _, err := executil.Run(ctx, 15*time.Second, "zfs", "inherit", property, name); err != nil {
		return fmt.Errorf("zfs inherit %s: %w", property, err)
	}
	return nil
}

// ErrRiskAck — the change is allowed but can lose data or break things; the
// caller must repeat it with acknowledge_risk. Mapped to 409 risk_ack_required.
// The UI asks first with its own translated text; this makes API clients ask too.
var ErrRiskAck = errors.New("cambio de alto impacto: repite la petición con acknowledge_risk=true si es lo que quieres")

// riskText — what each high-impact change does, in plain words.
var riskText = map[string]string{
	"sync_disabled":  "sync=disabled pierde los últimos segundos de escrituras si se corta la luz; máquinas virtuales y bases de datos pueden quedar corruptas",
	"copies":         "copies solo afecta a lo que se escriba a partir de ahora y no sustituye a la redundancia del pool",
	"readonly_on":    "readonly=on hace fallar a todo lo que escriba en este dataset",
	"canmount_off":   "con canmount=off/noauto el dataset deja de montarse, también al arrancar",
	"mountpoint":     "cambiar el punto de montaje mueve los datos de sitio: lo que los busque en la ruta anterior (aplicaciones, almacenamiento de Proxmox) deja de encontrarlos",
	"acl_semantics":  "acltype/xattr cambian cómo se interpretan los permisos de los ficheros existentes",
	"volsize_shrink": "reducir volsize destruye los datos que queden más allá del nuevo tamaño",
}

// PropRisk — the risk key of setting property=value, or "" when it is
// harmless. volsize is judged against the current size by DatasetPropSet.
func PropRisk(property, value string) string {
	switch property {
	case "sync":
		if value == "disabled" {
			return "sync_disabled"
		}
	case "copies":
		return "copies"
	case "readonly":
		if value == "on" {
			return "readonly_on"
		}
	case "canmount":
		if value == "off" || value == "noauto" {
			return "canmount_off"
		}
	case "mountpoint":
		return "mountpoint"
	case "acltype", "xattr":
		return "acl_semantics"
	}
	return ""
}

// inheritRisk — inheriting these takes the parent's value, whatever it is,
// so it carries the same risk as setting it.
var inheritRisk = map[string]string{
	"sync": "sync_disabled", "copies": "copies", "readonly": "readonly_on",
	"canmount": "canmount_off", "mountpoint": "mountpoint",
	"acltype": "acl_semantics", "xattr": "acl_semantics",
}

// volsizeShrinks reports whether value is smaller than the volume's current
// size, read live. If the size cannot be read it answers true: shrinking is
// the one change here that destroys data, so an unknown means ask.
func (s *Service) volsizeShrinks(ctx context.Context, name, value string) bool {
	want, ok := parseSize(value)
	if !ok {
		return true
	}
	out, err := executil.RunRead(ctx, 10*time.Second, "zfs", "get", "-Hp", "-o", "value", "volsize", name)
	if err != nil {
		return true
	}
	cur, err := strconv.ParseUint(strings.TrimSpace(string(out)), 10, 64)
	return err != nil || want < cur
}

