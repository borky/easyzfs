// Package httpapi — handlers REST + SSE. Los handlers leen la CACHÉ de los
// colectores, nunca ejecutan comandos del sistema directamente.
// Errores: {"error":"código","message":"texto legible"} con HTTP 4xx/5xx.
package httpapi

import (
	"database/sql"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"

	"easyzfs/internal/actions"
	"easyzfs/internal/alerts"
	"easyzfs/internal/apikeys"
	"easyzfs/internal/auth"
	"easyzfs/internal/backup"
	"easyzfs/internal/channels"
	"easyzfs/internal/collectors"
	"easyzfs/internal/config"
	"easyzfs/internal/hub"
	"easyzfs/internal/longops"
	"easyzfs/internal/notifier"
	"easyzfs/internal/push"
	"easyzfs/internal/replication"
	"easyzfs/internal/scheduler"
	"easyzfs/internal/settings"
	"easyzfs/internal/updater"
	"easyzfs/internal/users"
)

// Server — dependencias inyectadas desde main (sin framework de DI).
type Server struct {
	cfg          *config.Config
	db           *sql.DB
	auth         *auth.Manager
	users        *users.Store
	apiKeys      *apikeys.Store
	alerter      *alerts.Alerter
	settings     *settings.Store
	pools        collectors.PoolProvider
	disks        collectors.DiskProvider
	sysTimers    collectors.SysTimerProvider
	perf         collectors.PerfProvider
	caps         collectors.CapProvider
	diskUse      collectors.DiskUseProvider
	host         collectors.HostStorageProvider
	act          *actions.Service
	sched        *scheduler.Scheduler
	jstore       *scheduler.Store
	h            *hub.Hub
	push         *push.Sender
	channels     *channels.Client
	channelStore *channels.Store
	mailer       *notifier.Mailer
	backup       *backup.Store
	longOps      *longops.Manager
	repl         *replication.Runner
	updater      *updater.Updater
	started      time.Time
	version      string
	build        string
	zfsVersion   string

	// updateChannel — "local" when the binary is updated from its checkout
	// and has no updater; the UI then hides the update check.
	updateChannel string

	loginLimiter *loginLimiter    // rate limit de /api/login (IP+usuario)
	rateBucket   *rateGuardBucket // rateGuard: mutations per IP per minute
}

// Deps — parámetros del constructor.
type Deps struct {
	Cfg          *config.Config
	DB           *sql.DB
	Auth         *auth.Manager
	Users        *users.Store
	APIKeys      *apikeys.Store
	Alerter      *alerts.Alerter
	Settings     *settings.Store
	Pools        collectors.PoolProvider
	Disks        collectors.DiskProvider
	SysTimers    collectors.SysTimerProvider
	Perf         collectors.PerfProvider
	Caps         collectors.CapProvider
	DiskUse      collectors.DiskUseProvider
	Host         collectors.HostStorageProvider
	Actions      *actions.Service
	Sched        *scheduler.Scheduler
	Jobs         *scheduler.Store
	Hub          *hub.Hub
	Push         *push.Sender
	Channels     *channels.Client
	ChannelStore *channels.Store
	Mailer       *notifier.Mailer
	Backup       *backup.Store
	LongOps      *longops.Manager
	Repl         *replication.Runner
	Updater      *updater.Updater
	Version      string
	Build        string
	ZFSVersion   string

	UpdateChannel string
}

// NewServer crea el servidor del API.
func NewServer(d Deps) *Server {
	return &Server{
		cfg: d.Cfg, db: d.DB, auth: d.Auth, users: d.Users, apiKeys: d.APIKeys,
		alerter: d.Alerter, settings: d.Settings,
		pools: d.Pools, disks: d.Disks, sysTimers: d.SysTimers,
		perf: d.Perf, caps: d.Caps, diskUse: d.DiskUse, host: d.Host,
		act: d.Actions, sched: d.Sched, jstore: d.Jobs, h: d.Hub, push: d.Push,
		channels: d.Channels, channelStore: d.ChannelStore,
		mailer: d.Mailer,
		backup: d.Backup, longOps: d.LongOps, repl: d.Repl,
		updater: d.Updater,
		started: time.Now(), version: d.Version, build: d.Build, zfsVersion: d.ZFSVersion, updateChannel: d.UpdateChannel,
		loginLimiter: newLoginLimiter(),
	}
}

// Handler monta el árbol de rutas: /api/login público, resto tras auth,
// mutaciones bloqueadas en modo demo.
func (s *Server) Handler() http.Handler {
	root := http.NewServeMux()
	root.HandleFunc("POST /api/login", s.login)
	root.HandleFunc("POST /api/login/2fa", s.login2FA)
	// Público (sin sesión): el login consulta si el modo demo está habilitado.
	root.HandleFunc("GET /api/public/demo", s.publicDemo)

	a := http.NewServeMux()
	// sesión
	a.HandleFunc("POST /api/logout", s.logout)
	a.HandleFunc("GET /api/me", s.me)
	a.HandleFunc("PUT /api/me/language", s.putMyLanguage)
	a.HandleFunc("PUT /api/me/profile", s.putMyProfile)
	a.HandleFunc("PUT /api/me/avatar", s.putMyAvatar)
	a.HandleFunc("DELETE /api/me/avatar", s.deleteMyAvatar)
	a.HandleFunc("GET /api/avatars/{name}", s.getAvatar)
	a.HandleFunc("POST /api/me/password", s.changeMyPassword)
	// 2FA del propio usuario (#84)
	a.HandleFunc("GET /api/me/2fa", s.my2FAStatus)
	a.HandleFunc("POST /api/me/2fa/setup", s.my2FASetup)
	a.HandleFunc("POST /api/me/2fa/confirm", s.my2FAConfirm)
	a.HandleFunc("POST /api/me/2fa/disable", s.my2FADisable)
	// POST, not GET: it destroys the stored recovery codes and issues new ones.
	// As a GET it was exempt from the CSRF, rate and demo guards, and SameSite=Lax
	// still sends the session cookie on a cross-site top-level navigation.
	a.HandleFunc("POST /api/me/2fa/recovery", s.my2FARecovery)
	// usuarios (admin)
	a.HandleFunc("GET /api/users", s.auth.RequireAdmin(s.listUsers))
	// A new admin account is a way past every later confirmation: a hijacked
	// session would answer them with a password it chose.
	a.HandleFunc("POST /api/users", s.auth.RequireAdmin(s.requireReauth(s.createUser)))
	// Irreversible operations below go through requireReauth (reauth.go):
	// the password again, and the TOTP code when 2FA is on.
	a.HandleFunc("DELETE /api/users/{name}", s.auth.RequireAdmin(s.requireReauth(s.deleteUser)))
	a.HandleFunc("POST /api/users/{name}/password", s.auth.RequireAdmin(s.requireReauth(s.setUserPassword)))
	a.HandleFunc("PUT /api/users/{name}/language", s.auth.RequireAdmin(s.setUserLanguage))
	a.HandleFunc("DELETE /api/users/{name}/2fa", s.auth.RequireAdmin(s.requireReauth(s.admin2FADisable)))
	// API keys de solo lectura (admin, #87)
	a.HandleFunc("GET /api/keys", s.auth.RequireAdmin(s.listAPIKeys))
	a.HandleFunc("POST /api/keys", s.auth.RequireAdmin(s.requireReauth(s.createAPIKey)))
	a.HandleFunc("DELETE /api/keys/{id}", s.auth.RequireAdmin(s.deleteAPIKey))
	// sistema
	a.HandleFunc("GET /api/version", s.getVersion)
	a.HandleFunc("GET /api/settings", s.getSettings)
	a.HandleFunc("PUT /api/settings", s.auth.RequireAdmin(s.putSettings))
	a.HandleFunc("GET /api/activity", s.listActivity)
	a.HandleFunc("GET /api/alerts", s.listAlerts)
	a.HandleFunc("POST /api/alerts/{id}/ack", s.ackAlert)
	a.HandleFunc("GET /api/overview", s.getOverview)
	// canales de alerta (#134): estado (sin secretos), configuración y prueba
	a.HandleFunc("GET /api/channels", s.auth.RequireAdmin(s.getChannels))
	a.HandleFunc("PUT /api/channels/{name}", s.auth.RequireAdmin(s.putChannel))
	a.HandleFunc("DELETE /api/channels/{name}", s.auth.RequireAdmin(s.deleteChannel))
	a.HandleFunc("POST /api/channels/{name}/test", s.auth.RequireAdmin(s.testChannel))
	a.HandleFunc("GET /api/system-timers", s.listSystemTimers)
	a.HandleFunc("POST /api/system-timers/schedule", s.auth.RequireAdmin(s.sysTimerSchedule))
	a.HandleFunc("POST /api/system-timers/migrate", s.auth.RequireAdmin(s.sysTimerMigrate))
	// pools (mutaciones: admin — son potencialmente destructivas)
	a.HandleFunc("GET /api/pools", s.listPools)
	a.HandleFunc("GET /api/pools/missing", s.missingPools)
	a.HandleFunc("POST /api/pools", s.auth.RequireAdmin(s.requireReauth(s.createPool)))
	a.HandleFunc("POST /api/pools/import", s.auth.RequireAdmin(s.importPool))
	a.HandleFunc("POST /api/pools/{name}/scrub", s.auth.RequireAdmin(s.scrubPool))
	a.HandleFunc("POST /api/pools/{name}/export", s.auth.RequireAdmin(s.requireReauth(s.exportPool)))
	a.HandleFunc("POST /api/pools/{name}/vdev", s.auth.RequireAdmin(s.requireReauth(s.addVdev)))
	a.HandleFunc("POST /api/pools/{name}/vdev/action", s.auth.RequireAdmin(s.requireReauth(s.vdevAction)))
	a.HandleFunc("POST /api/pools/{name}/replace", s.auth.RequireAdmin(s.requireReauth(s.replaceDisk)))
	a.HandleFunc("POST /api/pools/{name}/autotrim", s.auth.RequireAdmin(s.setAutotrim))
	a.HandleFunc("POST /api/pools/{name}/checkpoint", s.auth.RequireAdmin(s.requireReauth(s.poolCheckpoint)))
	a.HandleFunc("POST /api/pools/{name}/expand", s.auth.RequireAdmin(s.requireReauth(s.expandPool)))
	a.HandleFunc("POST /api/pools/{name}/clear", s.auth.RequireAdmin(s.clearPool))
	a.HandleFunc("GET /api/pools/{name}/history", s.poolHistory)
	a.HandleFunc("GET /api/performance", s.getPerformance)
	// series históricas (U2): rangos con downsampling LTTB
	a.HandleFunc("GET /api/series", s.getSeries)
	// datasets
	a.HandleFunc("GET /api/datasets", s.listDatasets)
	a.HandleFunc("POST /api/datasets", s.auth.RequireAdmin(s.createDataset))
	a.HandleFunc("PATCH /api/datasets/{name}", s.auth.RequireAdmin(s.patchDataset))
	a.HandleFunc("PATCH /api/datasets/{name}/properties", s.auth.RequireAdmin(s.requireReauthIf(volsizeChange, s.patchDatasetProps)))
	a.HandleFunc("GET /api/datasets/{name}/properties", s.listDatasetProps)
	a.HandleFunc("PATCH /api/datasets/{name}/rename", s.auth.RequireAdmin(s.renameDataset))
	a.HandleFunc("DELETE /api/datasets/{name}", s.auth.RequireAdmin(s.requireReauth(s.deleteDataset)))
	a.HandleFunc("GET /api/trash", s.listTrash)
	a.HandleFunc("POST /api/trash/{id}/restore", s.auth.RequireAdmin(s.restoreTrash))
	a.HandleFunc("DELETE /api/trash/{id}", s.auth.RequireAdmin(s.requireReauth(s.purgeTrash)))
	a.HandleFunc("POST /api/datasets/{name}/promote", s.auth.RequireAdmin(s.promoteDataset))
	a.HandleFunc("POST /api/datasets/{name}/mount", s.auth.RequireAdmin(s.mountDataset))
	a.HandleFunc("POST /api/datasets/{name}/unmount", s.auth.RequireAdmin(s.unmountDataset))
	a.HandleFunc("POST /api/datasets/{name}/rewrite", s.auth.RequireAdmin(s.rewriteDataset))
	a.HandleFunc("POST /api/datasets/{name}/unlock", s.auth.RequireAdmin(s.unlockDataset))
	a.HandleFunc("POST /api/datasets/{name}/lock", s.auth.RequireAdmin(s.lockDataset))
	a.HandleFunc("POST /api/datasets/{name}/change-key", s.auth.RequireAdmin(s.requireReauth(s.changeKeyDataset)))
	a.HandleFunc("POST /api/datasets/{name}/properties/{prop}/inherit", s.auth.RequireAdmin(s.inheritDatasetProp))
	// operaciones largas (runner longops: rewrite, futura replicación)
	a.HandleFunc("GET /api/longops", s.listLongOps)
	a.HandleFunc("POST /api/longops/{id}/cancel", s.auth.RequireAdmin(s.cancelLongOp))
	// snapshots
	a.HandleFunc("GET /api/snapshots", s.listSnapshots)
	a.HandleFunc("GET /api/snapshots/diff", s.diffSnapshots)
	a.HandleFunc("POST /api/snapshots", s.auth.RequireAdmin(s.createSnapshot))
	a.HandleFunc("POST /api/snapshots/{full}/clone", s.auth.RequireAdmin(s.cloneSnapshot))
	a.HandleFunc("DELETE /api/snapshots/{full}", s.auth.RequireAdmin(s.requireReauth(s.deleteSnapshot)))
	a.HandleFunc("POST /api/snapshots/{full}/rollback", s.auth.RequireAdmin(s.requireReauth(s.rollbackSnapshot)))
	// jobs
	a.HandleFunc("GET /api/jobs", s.listJobs)
	a.HandleFunc("POST /api/jobs", s.auth.RequireAdmin(s.createJob))
	a.HandleFunc("GET /api/jobs/history", s.jobsHistory)
	a.HandleFunc("PATCH /api/jobs/{id}", s.auth.RequireAdmin(s.patchJob))
	a.HandleFunc("DELETE /api/jobs/{id}", s.auth.RequireAdmin(s.deleteJob))
	a.HandleFunc("POST /api/jobs/{id}/run", s.auth.RequireAdmin(s.runJob))
	// replicación ZFS send/recv (mutaciones: admin)
	a.HandleFunc("GET /api/replication", s.listReplication)
	a.HandleFunc("POST /api/replication", s.auth.RequireAdmin(s.requireReauthIf(forceFull, s.createReplication)))
	a.HandleFunc("PATCH /api/replication/{id}", s.auth.RequireAdmin(s.requireReauthIf(forceFull, s.patchReplication)))
	a.HandleFunc("DELETE /api/replication/{id}", s.auth.RequireAdmin(s.deleteReplication))
	a.HandleFunc("POST /api/replication/{id}/run", s.auth.RequireAdmin(s.runReplication))
	a.HandleFunc("GET /api/replication/sshkey", s.getReplicationSSHKey)
	a.HandleFunc("POST /api/replication/test", s.auth.RequireAdmin(s.testReplication))
	// discos
	a.HandleFunc("GET /api/disks", s.listDisks)
	a.HandleFunc("GET /api/recommendations", s.listRecommendations)
	a.HandleFunc("POST /api/disks/{dev}/smart-test", s.auth.RequireAdmin(s.smartTest))
	a.HandleFunc("GET /api/disks/{dev}/smart", s.diskSmart)
	a.HandleFunc("GET /api/disks/{dev}/smart-log", s.diskSmartLog)
	a.HandleFunc("POST /api/disks/{dev}/poweroff", s.auth.RequireAdmin(s.powerOff))
	a.HandleFunc("POST /api/disks/{dev}/identify", s.auth.RequireAdmin(s.identifyDisk))
	// notificaciones push (Web Push; 503 push_not_configured sin claves VAPID)
	a.HandleFunc("GET /api/push/vapid-public-key", s.getPushVapidKey)
	a.HandleFunc("POST /api/push/subscribe", s.postPushSubscribe)
	a.HandleFunc("DELETE /api/push/unsubscribe", s.deletePushUnsubscribe)
	// preferencias de notificación (cada usuario gestiona las suyas)
	a.HandleFunc("GET /api/push/preferences", s.getPushPreferences)
	a.HandleFunc("PUT /api/push/preferences", s.putPushPreferences)
	a.HandleFunc("GET /api/push/quiet-hours", s.getPushQuietHours)
	a.HandleFunc("PUT /api/push/quiet-hours", s.putPushQuietHours)

	// Copia de seguridad de la BD (solo admin)
	a.HandleFunc("GET /api/backup/status", s.auth.RequireAdmin(s.backupStatus))
	a.HandleFunc("POST /api/backup/run", s.auth.RequireAdmin(s.backupRun))
	a.HandleFunc("GET /api/backup/download", s.auth.RequireAdmin(s.backupDownload))
	a.HandleFunc("POST /api/backup/import", s.auth.RequireAdmin(s.requireReauth(s.backupImport)))
	// Actualizaciones (solo admin; wireUpdater registra las rutas si hay updater)
	s.wireUpdater(a)
	// SSE (con el usuario de la sesión para la regla no-duplicar push/SSE)
	a.Handle("GET /api/events", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.h.ServeSSE(w, r, actor(r))
	}))

	root.Handle("/api/", s.auth.Middleware(s.rateGuard(s.csrfGuard(s.demoGuard(s.refreshAfterMutation(a))))))
	return root
}

// rateGuard — limita mutaciones a 30 por minuto por IP. Solo lectura (GET/HEAD)
// sin límite. El bucket es en memoria (sin persistencia, aceptable para LAN).
type rateGuardBucket struct {
	mu   sync.Mutex
	hits map[string][]time.Time
}

func (b *rateGuardBucket) allow(ip string, now time.Time, max int, window time.Duration) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	list := b.hits[ip]
	cutoff := now.Add(-window)
	j := 0
	for ; j < len(list) && list[j].Before(cutoff); j++ {
	}
	list = list[j:]
	if len(list) >= max {
		b.hits[ip] = list
		return false
	}
	list = append(list, now)
	b.hits[ip] = list
	return true
}

// Each Server has its own bucket (s.rateBucket). It used to be a package
// global, shared by every Server in the process, so tests had to empty it by
// hand to avoid 429s leaking from one test into the next.

func (s *Server) rateGuard(next http.Handler) http.Handler {
	if s.rateBucket == nil {
		s.rateBucket = &rateGuardBucket{hits: map[string][]time.Time{}}
	}
	bucket := s.rateBucket
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet || r.Method == http.MethodHead {
			next.ServeHTTP(w, r)
			return
		}
		ip := r.RemoteAddr
		if idx := strings.LastIndex(ip, ":"); idx != -1 {
			ip = ip[:idx]
		}
		if !bucket.allow(ip, time.Now(), 30, time.Minute) {
			writeErr(w, http.StatusTooManyRequests, "rate_limited",
				"demasiadas peticiones; inténtalo de nuevo en unos segundos")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// csrfGuard — validates Origin/Referer against Host on mutations
// (POST/PUT/PATCH/DELETE). On by default: this API can destroy pools, and
// SameSite=Lax alone was the only defence. CSRF_CHECK=0 turns it off, for a
// reverse proxy that rewrites the Host header instead of passing it through.
// A request with neither header is let through: browsers send Origin on every
// cross-site mutation, so its absence means a non-browser client.
func (s *Server) csrfGuard(next http.Handler) http.Handler {
	switch strings.ToLower(os.Getenv("CSRF_CHECK")) {
	case "0", "false", "no", "off":
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet || r.Method == http.MethodHead {
			next.ServeHTTP(w, r)
			return
		}
		origin := r.Header.Get("Origin")
		if origin == "" {
			origin = r.Header.Get("Referer")
		}
		if origin != "" {
			host := r.Host
			if strings.HasPrefix(origin, "https://") {
				origin = origin[8:]
			} else if strings.HasPrefix(origin, "http://") {
				origin = origin[7:]
			}
			if i := strings.Index(origin, "/"); i != -1 {
				origin = origin[:i]
			}
			if i := strings.Index(origin, ":"); i != -1 {
				origin = origin[:i]
			}
			if i := strings.Index(host, ":"); i != -1 {
				host = host[:i]
			}
			if origin != host {
				writeErr(w, http.StatusForbidden, "csrf",
					"petición rechazada: origen '"+origin+"' no coincide con el host '"+host+"'")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// demoGuard — en DEMO=1 las mutaciones devuelven 403 demo_mode
// (excepto logout y ack de alertas, inofensivas y necesarias para la demo).
func (s *Server) demoGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.cfg.Demo && r.Method != http.MethodGet && r.Method != http.MethodHead {
			allowed := r.URL.Path == "/api/logout" ||
				(strings.HasPrefix(r.URL.Path, "/api/alerts/") && strings.HasSuffix(r.URL.Path, "/ack"))
			if !allowed {
				writeErr(w, http.StatusForbidden, "demo_mode", "modo demo: las mutaciones están desactivadas")
				return
			}
		}
		if s.cfg.ReadOnly && r.Method != http.MethodGet && r.Method != http.MethodHead && storageRoute(r.URL.Path) {
			writeErr(w, http.StatusForbidden, "read_only", "modo solo lectura: EasyZFS no modifica el almacenamiento")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// refreshAfterMutation — after a successful change on a storage route, ask
// the pool collector for an immediate pass and drop the cached disk view.
// Only a few handlers did this, so a pool destroyed through the API kept
// showing as its disks' owner until the next idle tick, minutes later.
// GET and HEAD pass straight through, so the SSE stream is never wrapped.
func (s *Server) refreshAfterMutation(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet || r.Method == http.MethodHead {
			next.ServeHTTP(w, r)
			return
		}
		sw := &statusWriter{ResponseWriter: w, code: http.StatusOK}
		next.ServeHTTP(sw, r)
		if sw.code < 400 && storageRoute(r.URL.Path) {
			if rc, ok := s.pools.(interface{ RefreshSoon() }); ok {
				rc.RefreshSoon()
			}
			if rc, ok := s.diskUse.(interface{ RefreshSoon() }); ok {
				rc.RefreshSoon()
			}
			if rc, ok := s.host.(interface{ RefreshSoon() }); ok {
				rc.RefreshSoon()
			}
		}
	})
}

// statusWriter records the status a handler wrote.
type statusWriter struct {
	http.ResponseWriter
	code int
}

func (w *statusWriter) WriteHeader(code int) {
	w.code = code
	w.ResponseWriter.WriteHeader(code)
}

// storageRoute — the routes whose mutations reach pools, datasets, disks or
// the host (jobs and replication run zfs commands later, system timers edit
// cron and systemd). In read-only mode their mutations are refused; the app's
// own settings (alerts, users, channels, push, backups of its database) stay
// editable, since they change nothing on the host's storage.
func storageRoute(p string) bool {
	for _, prefix := range []string{
		"/api/pools", "/api/datasets", "/api/snapshots", "/api/jobs",
		"/api/replication", "/api/disks", "/api/system-timers", "/api/longops", "/api/trash",
		"/api/update",
	} {
		if p == prefix || strings.HasPrefix(p, prefix+"/") || strings.HasPrefix(p, prefix+"s/") {
			return true
		}
	}
	return false
}

// --- helpers ---

// writeJSON serializa v con el código dado.
func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	if v == nil {
		return
	}
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("httpapi: encode: %v", err)
	}
}

// writeErr — formato de error del contrato.
func writeErr(w http.ResponseWriter, code int, errCode, msg string) {
	writeJSON(w, code, map[string]string{"error": errCode, "message": msg})
}

// decodeJSON decodifica el body; false = ya se escribió el error 400.
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	if err := dec.Decode(dst); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_json", "body JSON inválido: "+err.Error())
		return false
	}
	return true
}

// requireConfirm valida {"confirm":"<target>"} en destructivas (lección 6).
func requireConfirm(w http.ResponseWriter, confirm, target string) bool {
	if confirm != target || target == "" {
		writeErr(w, http.StatusBadRequest, "confirm_required",
			"se requiere {\"confirm\":\""+target+"\"} para confirmar la operación")
		return false
	}
	return true
}

// actor — usuario autenticado para el audit_log.
func actor(r *http.Request) string {
	return auth.UserFromContext(r.Context())
}

// actionErr traduce errores de actions/scheduler a respuestas HTTP.
func actionErr(w http.ResponseWriter, err error) {
	switch {
	case err == nil:
		return
	case errors.Is(err, actions.ErrSnapshotNotFound), errors.Is(err, actions.ErrNotFound):
		writeErr(w, http.StatusNotFound, "not_found", err.Error())
	case errors.Is(err, actions.ErrHostStorage):
		writeErr(w, http.StatusForbidden, "host_storage", err.Error())
	case errors.Is(err, actions.ErrNotAllowed):
		// The privileged gateway refused a shape outside its grammar: a bug
		// or a tampered request, never something the UI offers.
		writeErr(w, http.StatusForbidden, "not_allowed", err.Error())
	case errors.Is(err, actions.ErrHostUnknown):
		writeErr(w, http.StatusConflict, "host_unknown", err.Error())
	case errors.Is(err, actions.ErrConflict):
		writeErr(w, http.StatusConflict, "conflict", err.Error())
	case errors.Is(err, actions.ErrRiskAck):
		writeErr(w, http.StatusConflict, "risk_ack_required", err.Error())
	case errors.Is(err, actions.ErrDiskInUse):
		writeErr(w, http.StatusConflict, "dev_in_use", err.Error())
	case errors.Is(err, actions.ErrWrongKey):
		// 403, not 401: the frontend treats any 401 as an expired session.
		writeErr(w, http.StatusForbidden, "wrong_key", err.Error())
	case strings.Contains(err.Error(), "inválid"):
		writeErr(w, http.StatusBadRequest, "invalid_input", err.Error())
	default:
		writeErr(w, http.StatusInternalServerError, "exec_error", err.Error())
	}
}

// memRSS — RSS aproximado del proceso vía runtime.ReadMemStats.
func memRSS() uint64 {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return m.Sys
}
