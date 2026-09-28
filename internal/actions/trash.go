// trash.go — deleting a dataset from the UI moves it to a recycle bin
// (<pool>/easyzfs-trash) instead of destroying it, and it is destroyed for
// good only after TrashDays, or when someone empties it with the password.
// A wrong click or the wrong row is then an undo, not a restore from backup.
//
// The dataset keeps its data, children and snapshots, so it keeps using pool
// space until purged; the UI shows how much. Mountpoints set on the dataset
// or its children (locally or received) are recorded and set to none, and
// the bin has mountpoint=none, so the host stops showing it. Nothing is made
// readonly: a zvol still open by a running VM, or a subvolume a container
// has bind-mounted, would start failing writes, and trashing must never do
// more harm than the 'busy' refusal destroy used to give.
package actions

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"easyzfs/internal/executil"
)

// TrashDays — how long a trashed dataset is kept before it is destroyed.
const TrashDays = 7

// TrashDir — the name of the recycle bin under each pool's root dataset.
const TrashDir = "easyzfs-trash"

var (
	// ErrNotFound — no such dataset or trash entry. Mapped to 404.
	ErrNotFound = errors.New("no existe")
	// ErrConflict — the operation cannot proceed in the current state.
	// Mapped to 409.
	ErrConflict = errors.New("conflicto")
)

// inUse — whether any process has path open: a zvol's device (mount=false)
// or a mounted filesystem (mount=true, 'fuser -m'). fuser reports processes
// of every user only as root, hence sudo. Exit 0 = in use, exit 1 with
// nothing on stderr = free; anything else (sudo refusing, no fuser) is an
// error, which the caller treats as "cannot tell". Test seam.
var inUse = func(ctx context.Context, path string, mount bool) (bool, error) {
	flag := "-s"
	if mount {
		flag = "-sm"
	}
	_, err := executil.RunTolerant(ctx, 15*time.Second, "fuser", flag, path)
	if err == nil {
		return true, nil
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) && ee.ExitCode() == 1 && len(strings.TrimSpace(string(ee.Stderr))) == 0 {
		return false, nil
	}
	return false, err
}

// zvolDir — test seam.
var zvolDir = "/dev/zvol"

// runZFS — test seam over the privileged zfs runner.
var runZFS = func(ctx context.Context, timeout time.Duration, args ...string) ([]byte, error) {
	return executil.Run(ctx, timeout, "zfs", args...)
}

// TrashRoot — the recycle bin dataset of pool.
func TrashRoot(pool string) string { return pool + "/" + TrashDir }

// InTrash — whether dataset (or snapshot) name lives in a recycle bin.
func InTrash(name string) bool {
	pool, rest, ok := strings.Cut(name, "/")
	return ok && pool != "" && (rest == TrashDir || strings.HasPrefix(rest, TrashDir+"/") || strings.HasPrefix(rest, TrashDir+"@"))
}

// TrashEntry — one trashed dataset (GET /api/trash).
type TrashEntry struct {
	ID        int64     `json:"id"`
	Pool      string    `json:"pool"`
	Original  string    `json:"original"`
	Trashed   string    `json:"trashed"`
	TrashedAt time.Time `json:"trashed_at"`
	PurgeAt   time.Time `json:"purge_at"`
	Actor     string    `json:"actor"`
	// LastError — why the last automatic purge failed (a clone elsewhere
	// depends on a snapshot in it, a hold…); "" when it has not failed.
	LastError string `json:"last_error"`
}

// trashState — what restoring needs to put back.
type trashState struct {
	// Mountpoints — dataset path relative to the trashed one ("" = itself) →
	// the mountpoint that was set on it.
	Mountpoints map[string]string `json:"mountpoints"`
	// Received — the paths in Mountpoints whose value came from a received
	// stream rather than being set locally; restore reverts them with
	// 'zfs inherit -S', so they stay received.
	Received map[string]bool `json:"received,omitempty"`
}

// propRow — one line of 'zfs get -H -o name,property,value,source'.
type propRow struct{ name, prop, value, source string }

func (s *Service) zfsGetRows(ctx context.Context, args ...string) ([]propRow, error) {
	out, err := runZFS(ctx, 15*time.Second, append([]string{"get", "-H", "-o", "name,property,value,source"}, args...)...)
	if err != nil {
		return nil, err
	}
	var rows []propRow
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		f := strings.Split(line, "\t")
		if len(f) >= 4 {
			rows = append(rows, propRow{f[0], f[1], f[2], f[3]})
		}
	}
	return rows, nil
}

// setMountpoints puts each row's recorded mountpoint back, parents first
// (a child mounted before its parent would be hidden under it).
func setMountpoints(ctx context.Context, rows []propRow, received map[string]bool) []string {
	sort.Slice(rows, func(i, j int) bool { return len(rows[i].name) < len(rows[j].name) })
	var problems []string
	for _, r := range rows {
		var err error
		if received[r.name] {
			_, err = runZFS(ctx, 60*time.Second, "inherit", "-S", "mountpoint", r.name)
		} else {
			_, err = runZFS(ctx, 60*time.Second, "set", "mountpoint="+r.value, r.name)
		}
		if err != nil {
			problems = append(problems, "mountpoint de "+r.name+": "+err.Error())
			log.Printf("papelera: mountpoint de %s: %v", r.name, err)
		}
	}
	return problems
}

// DatasetTrash — moves name (and, with recursive, its children) to the pool's
// recycle bin. Without recursive a dataset with children is refused, as
// 'zfs destroy' would refuse it.
func (s *Service) DatasetTrash(ctx context.Context, actor, name string, recursive bool) error {
	if !reDataset.MatchString(name) {
		return ErrInvalidName
	}
	pool, _, ok := strings.Cut(name, "/")
	if !ok {
		return fmt.Errorf("%w: el dataset raíz de un pool no se puede mover a la papelera", ErrInvalidInput)
	}
	if InTrash(name) {
		return fmt.Errorf("%w: ya está en la papelera; bórralo desde allí", ErrInvalidInput)
	}
	s.trashMu.Lock()
	defer s.trashMu.Unlock()

	// Every dataset in the tree, with where its mountpoint comes from, and
	// whether it is mounted or a volume (for the in-use check).
	all, err := s.zfsGetRows(ctx, "-r", "-t", "filesystem,volume", "mountpoint,mounted,type", name)
	if err != nil {
		return fmt.Errorf("leer %s: %w", name, err)
	}
	var rows []propRow
	info := map[string]map[string]string{}
	for _, r := range all {
		if info[r.name] == nil {
			info[r.name] = map[string]string{}
		}
		info[r.name][r.prop] = r.value
		if r.prop == "mountpoint" {
			rows = append(rows, r)
		}
	}
	if len(rows) == 0 {
		return fmt.Errorf("%w: %s", ErrNotFound, name)
	}
	if len(rows) > 1 && !recursive {
		return fmt.Errorf("%w: %s tiene datasets hijos; márcalo como recursivo para moverlos también", ErrInvalidInput, name)
	}
	// Something still using it (a VM on a zvol, a container on a subvolume)
	// is refused, as destroy refused it with "busy": moved to the bin it
	// would keep running on a disk the purge destroys once it stops.
	for _, r := range rows {
		path, mount := "", false
		switch {
		case info[r.name]["type"] == "volume":
			dev, err := filepath.EvalSymlinks(filepath.Join(zvolDir, r.name))
			if err != nil {
				continue // no device node (volmode=none, or not created yet)
			}
			path = dev
		case info[r.name]["mounted"] == "yes":
			path, mount = r.value, true
		default:
			continue
		}
		busy, err := inUse(ctx, path, mount)
		if err != nil {
			return fmt.Errorf("%w: no se pudo comprobar si %s está en uso (%v); no se mueve a la papelera", ErrConflict, r.name, err)
		}
		if busy {
			return fmt.Errorf("%w: %s está en uso (¿una VM o un contenedor en marcha?); detenlo antes de borrarlo", ErrConflict, r.name)
		}
	}
	st := trashState{Mountpoints: map[string]string{}, Received: map[string]bool{}}
	var own []propRow // mountpoints that would survive the move: local or received
	receivedAbs := map[string]bool{}
	for _, r := range rows {
		if (r.source == "local" || r.source == "received") && r.value != "none" && r.value != "legacy" && r.value != "-" {
			own = append(own, r)
			rel := strings.TrimPrefix(r.name, name)
			st.Mountpoints[rel] = r.value
			if r.source == "received" {
				st.Received[rel] = true
				receivedAbs[r.name] = true
			}
		}
	}

	if err := s.ensureTrashRoot(ctx, pool); err != nil {
		return err
	}
	target, err := freeTrashName(ctx, pool, name)
	if err != nil {
		return err
	}
	s.audit(ctx, actor, "dataset.trash", name, map[string]any{"recursive": recursive, "to": target}, true)

	// Deepest first, so no child is left mounted inside an unmounted parent.
	sort.Slice(own, func(i, j int) bool { return len(own[i].name) > len(own[j].name) })
	var done []propRow
	undo := func() { setMountpoints(ctx, done, receivedAbs) }
	for _, r := range own {
		if _, err := runZFS(ctx, 60*time.Second, "set", "mountpoint=none", r.name); err != nil {
			undo()
			return fmt.Errorf("desmontar %s (¿en uso?): %w", r.name, err)
		}
		done = append(done, r)
	}
	if _, err := runZFS(ctx, 60*time.Second, "rename", name, target); err != nil {
		undo()
		if strings.Contains(err.Error(), "encryption root") {
			return fmt.Errorf("%w: %s hereda el cifrado de su padre y no puede salir de él; bórralo de forma permanente", ErrConflict, name)
		}
		return fmt.Errorf("mover %s a la papelera: %w", name, err)
	}
	state, _ := json.Marshal(st)
	if _, err := s.db.ExecContext(ctx,
		"INSERT INTO trash(pool, original, trashed, trashed_at, state, actor) VALUES (?,?,?,?,?,?)",
		pool, name, target, time.Now().UTC().Format(time.RFC3339), string(state), actor); err != nil {
		// Without the row the views hide it and nothing would ever restore
		// or purge it: put it back where it was instead.
		if _, rerr := runZFS(ctx, 60*time.Second, "rename", target, name); rerr != nil {
			return fmt.Errorf("%s quedó en %s sin registrar (%v) y no se pudo devolver: %w", name, target, err, rerr)
		}
		undo()
		return fmt.Errorf("no se pudo registrar en la papelera; %s sigue donde estaba: %w", name, err)
	}
	return nil
}

// freeTrashName — <bin>/<name with / as _>-<UTC stamp>, with a -2, -3…
// suffix when that is taken ('tank/a_b' and 'tank/a/b' trashed in the same
// second map to the same name).
func freeTrashName(ctx context.Context, pool, name string) (string, error) {
	base := TrashRoot(pool) + "/" + strings.ReplaceAll(strings.TrimPrefix(name, pool+"/"), "/", "_") +
		"-" + time.Now().UTC().Format("20060102T150405Z")
	for i := 1; i <= 20; i++ {
		target := base
		if i > 1 {
			target += "-" + strconv.Itoa(i)
		}
		if !reDataset.MatchString(target) {
			return "", fmt.Errorf("%w: nombre de papelera no válido: %s", ErrInvalidInput, target)
		}
		if _, err := runZFS(ctx, 15*time.Second, "get", "-H", "-o", "value", "type", target); err != nil {
			return target, nil
		}
	}
	return "", fmt.Errorf("%w: no hay un nombre libre en la papelera para %s", ErrConflict, name)
}

// ownTrashRoot — whether root is a bin this app made: unmountable, with
// mountpoint=none. A dataset someone created with that name by hand is
// neither used nor ever destroyed.
func ownTrashRoot(ctx context.Context, root string) (exists, ours bool) {
	out, err := runZFS(ctx, 15*time.Second, "get", "-H", "-o", "property,value,source", "canmount,mountpoint", root)
	if err != nil {
		return false, false
	}
	canmountOff, mpNone := false, false
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		f := strings.Split(line, "\t")
		if len(f) < 3 {
			continue
		}
		switch {
		case f[0] == "canmount" && f[1] == "off":
			canmountOff = true
		case f[0] == "mountpoint" && f[1] == "none" && f[2] == "local":
			mpNone = true
		}
	}
	return true, canmountOff && mpNone
}

// ensureTrashRoot creates <pool>/easyzfs-trash, unmountable, if missing.
func (s *Service) ensureTrashRoot(ctx context.Context, pool string) error {
	root := TrashRoot(pool)
	exists, ours := ownTrashRoot(ctx, root)
	if exists && !ours {
		return fmt.Errorf("%w: %s existe pero no es la papelera de EasyZFS (no tiene canmount=off y mountpoint=none); renómbrala o bórrala permanentemente", ErrConflict, root)
	}
	if exists {
		return nil
	}
	if _, err := runZFS(ctx, 30*time.Second, "create", "-o", "mountpoint=none", "-o", "canmount=off", root); err != nil {
		return fmt.Errorf("crear la papelera %s: %w", root, err)
	}
	return nil
}

// TrashList — every trashed dataset, newest first.
func (s *Service) TrashList(ctx context.Context) ([]TrashEntry, error) {
	rows, err := s.db.QueryContext(ctx,
		"SELECT id, pool, original, trashed, trashed_at, actor, last_error FROM trash ORDER BY id DESC")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []TrashEntry{}
	for rows.Next() {
		var e TrashEntry
		var at string
		if err := rows.Scan(&e.ID, &e.Pool, &e.Original, &e.Trashed, &at, &e.Actor, &e.LastError); err != nil {
			return nil, err
		}
		e.TrashedAt, _ = time.Parse(time.RFC3339, at)
		e.PurgeAt = e.TrashedAt.Add(TrashDays * 24 * time.Hour)
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *Service) trashEntry(ctx context.Context, id int64) (TrashEntry, trashState, error) {
	var e TrashEntry
	var at, state string
	err := s.db.QueryRowContext(ctx,
		"SELECT id, pool, original, trashed, trashed_at, actor, last_error, state FROM trash WHERE id = ?", id).
		Scan(&e.ID, &e.Pool, &e.Original, &e.Trashed, &at, &e.Actor, &e.LastError, &state)
	if errors.Is(err, sql.ErrNoRows) {
		return e, trashState{}, ErrNotFound
	}
	if err != nil {
		return e, trashState{}, err
	}
	e.TrashedAt, _ = time.Parse(time.RFC3339, at)
	e.PurgeAt = e.TrashedAt.Add(TrashDays * 24 * time.Hour)
	var st trashState
	_ = json.Unmarshal([]byte(state), &st)
	return e, st, nil
}

// TrashRestore — puts a trashed dataset back under its original name, with
// the mountpoints it had, and mounts it again. Warnings are what did not
// come back as it was; the dataset itself is restored when err is nil.
func (s *Service) TrashRestore(ctx context.Context, actor string, id int64) (warnings []string, err error) {
	s.trashMu.Lock()
	defer s.trashMu.Unlock()
	e, st, err := s.trashEntry(ctx, id)
	if err != nil {
		return nil, err
	}
	if !reDataset.MatchString(e.Original) || !reDataset.MatchString(e.Trashed) || !InTrash(e.Trashed) {
		return nil, ErrInvalidName
	}
	if _, err := runZFS(ctx, 15*time.Second, "get", "-H", "-o", "value", "type", e.Original); err == nil {
		return nil, fmt.Errorf("%w: ya existe un dataset llamado %s; renómbralo antes de restaurar", ErrConflict, e.Original)
	}
	s.audit(ctx, actor, "dataset.restore", e.Original, map[string]any{"from": e.Trashed}, false)
	if _, err := runZFS(ctx, 60*time.Second, "rename", e.Trashed, e.Original); err != nil {
		return nil, fmt.Errorf("restaurar %s: %w", e.Original, err)
	}
	// The row goes first: from here on the dataset is back, whatever else
	// fails, and a purge must never reach it under its old trash name.
	if _, err := s.db.ExecContext(ctx, "DELETE FROM trash WHERE id = ?", id); err != nil {
		log.Printf("papelera: borrar fila %d: %v", id, err)
	}
	s.dropEmptyTrashRoot(ctx, e.Pool)

	var rows []propRow
	received := map[string]bool{}
	res := newMountResolver()
	for rel, mp := range st.Mountpoints {
		ds := e.Original + rel
		if !reDataset.MatchString(ds) || (!st.Received[rel] && !reMountpoint.MatchString(mp)) {
			warnings = append(warnings, "mountpoint de "+ds+" no válido; no se restaura")
			continue
		}
		// Putting the value back *mounts* the dataset there and then: zfs
		// remounts a filesystem whose mountpoint was none as soon as it
		// becomes a path (checked on the VM, mounted no → yes). So the check
		// has to happen here, before the set, not in the mountTree pass below
		// — that one would find it already mounted and report otherwise.
		// Both branches: 'zfs inherit -S' puts a received value back just as
		// 'zfs set' puts a local one back. The value stored here is the
		// effective one and it is recorded on the dataset, hence the danger
		// rules only.
		if strings.HasPrefix(mp, "/") {
			t := mountTarget{path: path.Clean(mp), recorded: true}
			if err := res.checkTarget(ctx, ds, t, false); err != nil {
				log.Printf("papelera: mountpoint de %s no se restaura: %v", ds, err)
				warnings = append(warnings, "mountpoint de "+ds+" no se restaura: "+err.Error())
				continue
			}
		}
		rows = append(rows, propRow{name: ds, value: mp})
		received[ds] = st.Received[rel]
	}
	warnings = append(warnings, setMountpoints(ctx, rows, received)...)

	// Datasets that inherit their mountpoint come back unmounted (the ones
	// with a recorded value are already mounted by the loop above): mount what
	// the pool would mount at import (canmount=on, a real mountpoint, key
	// loaded), minus anything the effective-mountpoint rules refuse, which
	// mountTree reports instead of mounting (§1).
	return append(warnings, s.mountTree(ctx, e.Original)...), nil
}

// TrashPurge — destroys one trashed dataset for good.
func (s *Service) TrashPurge(ctx context.Context, actor string, id int64) error {
	s.trashMu.Lock()
	defer s.trashMu.Unlock()
	e, _, err := s.trashEntry(ctx, id)
	if err != nil {
		return err
	}
	return s.purge(ctx, actor, e)
}

// purge — call with trashMu held.
func (s *Service) purge(ctx context.Context, actor string, e TrashEntry) error {
	// Belt and braces: only ever destroy something inside a recycle bin.
	if !reDataset.MatchString(e.Trashed) || !InTrash(e.Trashed) || e.Trashed == TrashRoot(e.Pool) {
		return fmt.Errorf("%w: %s no está en la papelera; no se destruye", ErrInvalidInput, e.Trashed)
	}
	s.audit(ctx, actor, "dataset.purge", e.Original, map[string]any{"trashed": e.Trashed}, true)
	if _, err := runZFS(ctx, 120*time.Second, "destroy", "-r", e.Trashed); err != nil &&
		!strings.Contains(err.Error(), "does not exist") {
		// Shown on the entry: an hourly retry that keeps failing in silence
		// would leave a purge date in the past and the space still used.
		_, _ = s.db.ExecContext(ctx, "UPDATE trash SET last_error = ? WHERE id = ?", err.Error(), e.ID)
		return fmt.Errorf("vaciar %s: %w", e.Trashed, err)
	}
	if _, err := s.db.ExecContext(ctx, "DELETE FROM trash WHERE id = ?", e.ID); err != nil {
		return err
	}
	s.dropEmptyTrashRoot(ctx, e.Pool)
	return nil
}

// dropEmptyTrashRoot removes <pool>/easyzfs-trash once nothing is in it, so
// the bin leaves no trace on the pool (a Proxmox host's rpool included).
// Only a bin this app made (ownTrashRoot) is ever destroyed.
func (s *Service) dropEmptyTrashRoot(ctx context.Context, pool string) {
	root := TrashRoot(pool)
	if _, ours := ownTrashRoot(ctx, root); !ours {
		return
	}
	out, err := runZFS(ctx, 15*time.Second, "list", "-H", "-o", "name", "-r", "-d", "1", root)
	if err != nil || strings.TrimSpace(string(out)) != root {
		return // unreadable, or not empty
	}
	if _, err := runZFS(ctx, 30*time.Second, "destroy", root); err != nil {
		log.Printf("papelera: quitar %s vacía: %v", root, err)
	}
}

// PurgeExpired destroys entries older than TrashDays. A failure (a clone
// elsewhere depends on a trashed snapshot, a hold) keeps the entry, with the
// reason, for the next pass.
func (s *Service) PurgeExpired(ctx context.Context, now time.Time) {
	s.trashMu.Lock()
	defer s.trashMu.Unlock()
	list, err := s.TrashList(ctx)
	if err != nil {
		log.Printf("papelera: listar: %v", err)
		return
	}
	for _, e := range list {
		if now.Before(e.PurgeAt) {
			// Destroyed or renamed behind the app's back (from the CLI): the
			// row could only mislead, so it goes now rather than in a week.
			if _, err := runZFS(ctx, 15*time.Second, "get", "-H", "-o", "value", "type", e.Trashed); err != nil &&
				strings.Contains(err.Error(), "does not exist") {
				log.Printf("papelera: %s ya no existe; se quita de la lista", e.Trashed)
				_, _ = s.db.ExecContext(ctx, "DELETE FROM trash WHERE id = ?", e.ID)
				s.dropEmptyTrashRoot(ctx, e.Pool)
			}
			continue
		}
		if err := s.purge(ctx, "system", e); err != nil {
			log.Printf("papelera: %v", err)
		}
	}
}

// RunTrashPurger purges expired entries hourly until ctx ends.
func (s *Service) RunTrashPurger(ctx context.Context) {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		s.PurgeExpired(ctx, time.Now())
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
