// trash.go — deleting a dataset from the UI moves it to a recycle bin
// (<pool>/easyzfs-trash) instead of destroying it, and it is destroyed for
// good only after TrashDays, or when someone empties it with the password.
// A wrong click, the wrong row, or a VM disk that turned out to be in use is
// then an undo, not a restore from backup.
//
// The dataset keeps its data, children and snapshots, so it keeps using pool
// space until purged; the UI shows how much. Mountpoints set on the dataset
// or its children are recorded and set to none, and the trash root has
// mountpoint=none, so nothing trashed stays mounted.
package actions

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sort"
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
}

// trashState — what restoring needs to put back.
type trashState struct {
	// Mountpoints — dataset path relative to the trashed one ("" = itself) →
	// the mountpoint that was set locally on it.
	Mountpoints map[string]string `json:"mountpoints"`
	// Readonly — the readonly value set locally on the dataset, or "" when
	// it was inherited.
	Readonly string `json:"readonly"`
}

// propRow — one line of 'zfs get -H -o name,value,source'.
type propRow struct{ name, value, source string }

func (s *Service) zfsGetRows(ctx context.Context, args ...string) ([]propRow, error) {
	out, err := runZFS(ctx, 15*time.Second, append([]string{"get", "-H", "-o", "name,value,source"}, args...)...)
	if err != nil {
		return nil, err
	}
	var rows []propRow
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		f := strings.Split(line, "\t")
		if len(f) >= 3 {
			rows = append(rows, propRow{f[0], f[1], f[2]})
		}
	}
	return rows, nil
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

	// Every filesystem in the tree, with where its mountpoint comes from.
	rows, err := s.zfsGetRows(ctx, "-r", "-t", "filesystem,volume", "mountpoint", name)
	if err != nil {
		return fmt.Errorf("leer %s: %w", name, err)
	}
	if len(rows) == 0 {
		return fmt.Errorf("%w: %s", ErrNotFound, name)
	}
	if len(rows) > 1 && !recursive {
		return fmt.Errorf("%w: %s tiene datasets hijos; márcalo como recursivo para moverlos también", ErrInvalidInput, name)
	}
	st := trashState{Mountpoints: map[string]string{}}
	var local []propRow
	for _, r := range rows {
		if r.source == "local" && r.value != "none" && r.value != "legacy" && r.value != "-" {
			local = append(local, r)
			st.Mountpoints[strings.TrimPrefix(r.name, name)] = r.value
		}
	}
	ro, err := s.zfsGetRows(ctx, "readonly", name)
	if err != nil {
		return fmt.Errorf("leer %s: %w", name, err)
	}
	if len(ro) == 1 && ro[0].source == "local" {
		st.Readonly = ro[0].value
	}

	if err := s.ensureTrashRoot(ctx, pool); err != nil {
		return err
	}
	stamp := time.Now().UTC()
	target := TrashRoot(pool) + "/" + strings.ReplaceAll(strings.TrimPrefix(name, pool+"/"), "/", "_") +
		"-" + stamp.Format("20060102T150405Z")
	if !reDataset.MatchString(target) {
		return fmt.Errorf("%w: nombre de papelera no válido: %s", ErrInvalidInput, target)
	}
	s.audit(ctx, actor, "dataset.trash", name, map[string]any{"recursive": recursive, "to": target}, true)

	// Deepest first, so no child is left mounted inside an unmounted parent.
	sort.Slice(local, func(i, j int) bool { return len(local[i].name) > len(local[j].name) })
	var done []propRow
	undo := func() {
		for _, r := range done {
			if _, err := runZFS(ctx, 30*time.Second, "set", "mountpoint="+r.value, r.name); err != nil {
				log.Printf("papelera: no se pudo restaurar mountpoint de %s: %v", r.name, err)
			}
		}
	}
	for _, r := range local {
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
	if _, err := runZFS(ctx, 30*time.Second, "set", "readonly=on", target); err != nil {
		log.Printf("papelera: readonly=on en %s: %v", target, err) // it is trashed either way
	}
	state, _ := json.Marshal(st)
	if _, err := s.db.ExecContext(ctx,
		"INSERT INTO trash(pool, original, trashed, trashed_at, state, actor) VALUES (?,?,?,?,?,?)",
		pool, name, target, stamp.Format(time.RFC3339), string(state), actor); err != nil {
		// The dataset is safe in the bin, but without the row nothing would
		// ever purge or restore it: say so loudly rather than pretend.
		return fmt.Errorf("%s está en %s, pero no se pudo registrar: %w", name, target, err)
	}
	return nil
}

// ensureTrashRoot creates <pool>/easyzfs-trash, unmountable, if missing.
func (s *Service) ensureTrashRoot(ctx context.Context, pool string) error {
	root := TrashRoot(pool)
	if _, err := runZFS(ctx, 15*time.Second, "get", "-H", "-o", "value", "type", root); err == nil {
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
		"SELECT id, pool, original, trashed, trashed_at, actor FROM trash ORDER BY id DESC")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []TrashEntry{}
	for rows.Next() {
		var e TrashEntry
		var at string
		if err := rows.Scan(&e.ID, &e.Pool, &e.Original, &e.Trashed, &at, &e.Actor); err != nil {
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
		"SELECT id, pool, original, trashed, trashed_at, actor, state FROM trash WHERE id = ?", id).
		Scan(&e.ID, &e.Pool, &e.Original, &e.Trashed, &at, &e.Actor, &state)
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
// the mountpoints and readonly value it had, and mounts it again.
func (s *Service) TrashRestore(ctx context.Context, actor string, id int64) error {
	e, st, err := s.trashEntry(ctx, id)
	if err != nil {
		return err
	}
	if !reDataset.MatchString(e.Original) || !reDataset.MatchString(e.Trashed) || !InTrash(e.Trashed) {
		return ErrInvalidName
	}
	if _, err := runZFS(ctx, 15*time.Second, "get", "-H", "-o", "value", "type", e.Original); err == nil {
		return fmt.Errorf("%w: ya existe un dataset llamado %s; renómbralo antes de restaurar", ErrConflict, e.Original)
	}
	s.audit(ctx, actor, "dataset.restore", e.Original, map[string]any{"from": e.Trashed}, false)
	if _, err := runZFS(ctx, 60*time.Second, "rename", e.Trashed, e.Original); err != nil {
		return fmt.Errorf("restaurar %s: %w", e.Original, err)
	}
	// The row goes first: from here on the dataset is back, whatever else
	// fails, and a purge must never reach it under its old trash name.
	if _, err := s.db.ExecContext(ctx, "DELETE FROM trash WHERE id = ?", id); err != nil {
		log.Printf("papelera: borrar fila %d: %v", id, err)
	}
	s.dropEmptyTrashRoot(ctx, e.Pool)
	var problems []string
	if st.Readonly != "" {
		if _, err := runZFS(ctx, 30*time.Second, "set", "readonly="+st.Readonly, e.Original); err != nil {
			problems = append(problems, "readonly: "+err.Error())
		}
	} else if _, err := runZFS(ctx, 30*time.Second, "inherit", "readonly", e.Original); err != nil {
		problems = append(problems, "readonly: "+err.Error())
	}
	// Shallowest first: a parent is mounted before its children.
	rels := make([]string, 0, len(st.Mountpoints))
	for rel := range st.Mountpoints {
		rels = append(rels, rel)
	}
	sort.Slice(rels, func(i, j int) bool { return len(rels[i]) < len(rels[j]) })
	for _, rel := range rels {
		ds := e.Original + rel
		if !reDataset.MatchString(ds) || !reMountpoint.MatchString(st.Mountpoints[rel]) {
			problems = append(problems, "mountpoint de "+ds+" no válido; no se restaura")
			continue
		}
		if _, err := runZFS(ctx, 60*time.Second, "set", "mountpoint="+st.Mountpoints[rel], ds); err != nil {
			problems = append(problems, "mountpoint de "+ds+": "+err.Error())
		}
	}
	// Datasets that inherit their mountpoint come back unmounted: mount the
	// tree the way the pool would at import. This puts back what existed
	// before the dataset was trashed; like DatasetMount it does not check
	// the effective mountpoint (the open §1 item in FORK.md).
	rows, err := s.zfsGetRows(ctx, "-r", "-t", "filesystem", "canmount", e.Original)
	if err == nil {
		for _, r := range rows {
			if r.value == "on" {
				if _, err := runZFS(ctx, 60*time.Second, "mount", r.name); err != nil && !strings.Contains(err.Error(), "already mounted") {
					problems = append(problems, "montar "+r.name+": "+err.Error())
				}
			}
		}
	}
	if len(problems) > 0 {
		return fmt.Errorf("%s restaurado, pero con avisos: %s", e.Original, strings.Join(problems, "; "))
	}
	return nil
}

// TrashPurge — destroys one trashed dataset for good.
func (s *Service) TrashPurge(ctx context.Context, actor string, id int64) error {
	e, _, err := s.trashEntry(ctx, id)
	if err != nil {
		return err
	}
	return s.purge(ctx, actor, e)
}

func (s *Service) purge(ctx context.Context, actor string, e TrashEntry) error {
	// Belt and braces: only ever destroy something inside a recycle bin.
	if !reDataset.MatchString(e.Trashed) || !InTrash(e.Trashed) || e.Trashed == TrashRoot(e.Pool) {
		return fmt.Errorf("%w: %s no está en la papelera; no se destruye", ErrInvalidInput, e.Trashed)
	}
	s.audit(ctx, actor, "dataset.purge", e.Original, map[string]any{"trashed": e.Trashed}, true)
	if _, err := runZFS(ctx, 120*time.Second, "destroy", "-r", e.Trashed); err != nil &&
		!strings.Contains(err.Error(), "does not exist") {
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
func (s *Service) dropEmptyTrashRoot(ctx context.Context, pool string) {
	root := TrashRoot(pool)
	out, err := runZFS(ctx, 15*time.Second, "list", "-H", "-o", "name", "-r", "-d", "1", root)
	if err != nil || strings.Count(strings.TrimSpace(string(out)), "\n") > 0 {
		return // missing, unreadable, or not empty
	}
	if strings.TrimSpace(string(out)) != root {
		return
	}
	if _, err := runZFS(ctx, 30*time.Second, "destroy", root); err != nil {
		log.Printf("papelera: quitar %s vacía: %v", root, err)
	}
}

// PurgeExpired destroys entries older than TrashDays. A failure (a clone
// elsewhere depends on a trashed snapshot, a hold) keeps the entry, logged,
// for the next pass.
func (s *Service) PurgeExpired(ctx context.Context, now time.Time) {
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
