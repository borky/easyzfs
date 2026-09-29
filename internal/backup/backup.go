// Package backup — copia de seguridad de la BD SQLite: automática por
// frecuencia horaria (colector con ticker), manual ("Forzar ahora"),
// exportación (descarga) e importación (swap + reinicio del proceso).
// La copia usa VACUUM INTO: consistente con la BD en WAL y sin bloquear
// lecturas. La retención purga los ficheros más viejos que N días.
package backup

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"easyzfs/internal/settings"
)

// File — un fichero de respaldo en disco.
type File struct {
	Name  string    `json:"file"`
	TS    time.Time `json:"ts"`
	Bytes int64     `json:"bytes"`
}

// Status — contrato GET /api/backup/status.
type Status struct {
	Enabled       bool       `json:"enabled"`
	FreqHours     int        `json:"freq_hours"`
	RetentionDays int        `json:"retention_days"`
	Running       bool       `json:"running"`
	Last          *File      `json:"last"`
	NextRun       *time.Time `json:"next_run"`
	Dir           string     `json:"dir"`
}

// Store — gestiona los respaldos en <dir>.
type Store struct {
	db       *sql.DB
	dir      string
	settings *settings.Store
	running  bool
}

// New crea el store; dir por defecto = <dir de la BD>/backups.
func New(d *sql.DB, dbPath string, st *settings.Store) *Store {
	dir := filepath.Join(filepath.Dir(dbPath), "backups")
	if v := os.Getenv("BACKUP_DIR"); v != "" {
		dir = v
	}
	return &Store{db: d, dir: dir, settings: st}
}

// Dir devuelve el directorio de respaldos.
func (s *Store) Dir() string { return s.dir }

// list — respaldos app-*.db ordenados por nombre (el timestamp va en el
// nombre: app-20060102-150405.db; orden lexicográfico = cronológico).
func (s *Store) list() ([]File, error) {
	entries, err := os.ReadDir(s.dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []File
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), "app-") || !strings.HasSuffix(e.Name(), ".db") {
			continue
		}
		st, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, File{Name: e.Name(), TS: st.ModTime(), Bytes: st.Size()})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Latest devuelve el respaldo más reciente (nil si no hay ninguno).
func (s *Store) Latest() *File {
	files, err := s.list()
	if err != nil || len(files) == 0 {
		return nil
	}
	f := files[len(files)-1]
	return &f
}

// Status compone el estado para la API (último, próximo, configuración).
func (s *Store) Status(ctx context.Context) Status {
	st, err := s.settings.Load(ctx)
	if err != nil {
		st = settings.Defaults()
	}
	last := s.Latest()
	out := Status{
		Enabled:       st.BackupEnabled,
		FreqHours:     st.BackupFreqHours,
		RetentionDays: st.BackupRetentionDays,
		Running:       s.running,
		Last:          last,
		Dir:           s.dir,
	}
	if st.BackupEnabled && last != nil {
		next := last.TS.Add(time.Duration(st.BackupFreqHours) * time.Hour)
		out.NextRun = &next
	}
	return out
}

// Run ejecuta un respaldo ahora (VACUUM INTO) y purga por retención.
// Devuelve el fichero creado.
func (s *Store) Run(ctx context.Context) (*File, error) {
	if s.running {
		return nil, fmt.Errorf("a backup is already running")
	}
	s.running = true
	defer func() { s.running = false }()

	if err := os.MkdirAll(s.dir, 0o750); err != nil {
		return nil, err
	}
	name := "app-" + time.Now().Format("20060102-150405") + ".db"
	path := filepath.Join(s.dir, name)
	// VACUUM INTO: copia online consistente de la BD (compatible con WAL).
	if _, err := s.db.ExecContext(ctx, `VACUUM INTO ?`, path); err != nil {
		os.Remove(path)
		return nil, fmt.Errorf("vacuum into: %w", err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	f := &File{Name: name, TS: fi.ModTime(), Bytes: fi.Size()}
	log.Printf("backup: created %s (%d bytes)", name, f.Bytes)

	st, err := s.settings.Load(ctx)
	if err != nil {
		st = settings.Defaults()
	}
	if err := s.purge(st.BackupRetentionDays); err != nil {
		log.Printf("backup: purge old backups: %v", err)
	}
	return f, nil
}

// purge borra respaldos más viejos que days días.
func (s *Store) purge(days int) error {
	files, err := s.list()
	if err != nil {
		return err
	}
	cutoff := time.Now().AddDate(0, 0, -days)
	for _, f := range files {
		if f.TS.Before(cutoff) {
			if err := os.Remove(filepath.Join(s.dir, f.Name)); err != nil {
				return err
			}
			log.Printf("backup: purged %s (>%d days)", f.Name, days)
		}
	}
	return nil
}

// RunLoop — colector: comprueba cada minuto si toca respaldo automático
// (ahora - último ≥ frecuencia). Sin estado en memoria: la verdad son los
// ficheros del directorio (sobrevive a reinicios).
func (s *Store) RunLoop(ctx context.Context) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	s.maybeRun(ctx) // por si al arrancar ya toca (p.ej. nunca se hizo uno)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.maybeRun(ctx)
		}
	}
}

func (s *Store) maybeRun(ctx context.Context) {
	st, err := s.settings.Load(ctx)
	if err != nil || !st.BackupEnabled {
		return
	}
	last := s.Latest()
	if last != nil && time.Since(last.TS) < time.Duration(st.BackupFreqHours)*time.Hour {
		return
	}
	if _, err := s.Run(ctx); err != nil {
		log.Printf("backup: automatic backup: %v", err)
	}
}

// CheckSQLite valida que path es una BD SQLite íntegra (magic + quick_check).
func CheckSQLite(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	magic := make([]byte, 16)
	if _, err := io.ReadFull(f, magic); err != nil {
		f.Close()
		return fmt.Errorf("file too small or unreadable")
	}
	f.Close()
	if string(magic) != "SQLite format 3\x00" {
		return fmt.Errorf("not an SQLite database")
	}
	d, err := sql.Open("sqlite", "file:"+path+"?_pragma=query_only(1)")
	if err != nil {
		return err
	}
	defer d.Close()
	var res string
	if err := d.QueryRow("PRAGMA quick_check").Scan(&res); err != nil {
		return fmt.Errorf("quick_check: %w", err)
	}
	if res != "ok" {
		return fmt.Errorf("quick_check: %s", res)
	}
	return nil
}
