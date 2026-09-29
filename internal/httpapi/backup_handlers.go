// backup_handlers.go — copia de seguridad de la BD (solo admin):
// estado, forzar ahora, exportar (descarga) e importar (swap + reinicio).
package httpapi

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"easyzfs/internal/backup"
)

// backupStatus — GET /api/backup/status → Status (admin).
func (s *Server) backupStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.backup.Status(r.Context()))
}

// backupRun — POST /api/backup/run → 201 + File (admin). "Forzar ahora".
func (s *Server) backupRun(w http.ResponseWriter, r *http.Request) {
	f, err := s.backup.Run(r.Context())
	if err != nil {
		writeErr(w, http.StatusConflict, "backup_error", err.Error())
		return
	}
	s.act.AuditOnly(r.Context(), actor(r), "backup.run", f.Name, map[string]any{"bytes": f.Bytes})
	writeJSON(w, http.StatusCreated, f)
}

// backupDownload — GET /api/backup/download → fichero .db (admin).
// Hace la copia en un temporal único y lo sirve; se borra al terminar.
func (s *Server) backupDownload(w http.ResponseWriter, r *http.Request) {
	tmp, err := os.CreateTemp("", "easyzfs-export-*.db")
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "backup_error", err.Error())
		return
	}
	tmpPath := tmp.Name()
	tmp.Close()
	defer os.Remove(tmpPath)

	if _, err := s.db.ExecContext(r.Context(), `VACUUM INTO ?`, tmpPath); err != nil {
		writeErr(w, http.StatusInternalServerError, "backup_error", fmt.Sprintf("vacuum into: %v", err))
		return
	}
	fi, err := os.Stat(tmpPath)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "backup_error", err.Error())
		return
	}
	name := "easyzfs-backup-" + time.Now().Format("20060102-150405") + ".db"
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, name))
	w.Header().Set("Content-Type", "application/octet-stream")
	http.ServeContent(w, r, name, fi.ModTime(), mustOpen(tmpPath))
}

func mustOpen(path string) *os.File {
	f, err := os.Open(path)
	if err != nil {
		// ServeContent con fichero nulo fallaría feo; mejor panic controlado
		// (no debería ocurrir: acabamos de escribirlo)
		panic(err)
	}
	return f
}

// backupImport — POST /api/backup/import con el .db en el body (admin).
// Verifica que es SQLite válido (magic + quick_check), lo deja preparado y
// responde 202; una goroutine cierra la BD, hace el swap atómico y sale del
// proceso (systemd lo relanza con la BD importada). La BD actual queda en
// app.db.pre-import.
func (s *Server) backupImport(w http.ResponseWriter, r *http.Request) {
	const maxBytes = 4 << 30 // 4 GiB
	r.Body = http.MaxBytesReader(w, r.Body, maxBytes)

	dbPath := s.cfg.DBPath
	tmpPath := dbPath + ".import"
	out, err := os.Create(tmpPath)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "import_error", err.Error())
		return
	}
	_, copyErr := io.Copy(out, r.Body)
	out.Close()
	if copyErr != nil {
		os.Remove(tmpPath)
		writeErr(w, http.StatusRequestEntityTooLarge, "import_error", copyErr.Error())
		return
	}

	// Verificación: magic SQLite + quick_check con una apertura read-only.
	if err := backup.CheckSQLite(tmpPath); err != nil {
		os.Remove(tmpPath)
		writeErr(w, http.StatusBadRequest, "invalid_backup", err.Error())
		return
	}

	s.act.AuditOnly(r.Context(), actor(r), "backup.import", filepath.Base(dbPath), nil)
	w.WriteHeader(http.StatusAccepted)

	// Swap diferido: dar tiempo a que la respuesta salga, cerrar la BD,
	// renombrar y salir (systemd Restart=always relanza el proceso).
	go func() {
		time.Sleep(500 * time.Millisecond)
		log.Println("backup: importing — closing the database and swapping")
		_ = s.db.Close()
		preImport := dbPath + ".pre-import"
		_ = os.Remove(preImport)
		if err := os.Rename(dbPath, preImport); err != nil {
			log.Fatalf("backup: import: could not preserve the current database: %v", err)
		}
		// Limpiar restos WAL/SHM de la BD anterior (pertenecen al fichero viejo)
		_ = os.Remove(dbPath + "-wal")
		_ = os.Remove(dbPath + "-shm")
		if err := os.Rename(tmpPath, dbPath); err != nil {
			// Intentar restaurar la original antes de morir
			_ = os.Rename(preImport, dbPath)
			log.Fatalf("backup: import: could not install the imported database (the previous one was restored): %v", err)
		}
		log.Println("backup: imported database installed; restarting the process")
		os.Exit(0)
	}()
}
