// main.go — entrypoint de EasyZFS.
// Wiring: config → db (migraciones) → settings → usuarios (bootstrap) →
// hub SSE → alerter → colectores → acciones → scheduler → HTTP.
// Graceful shutdown: drenar SSE → srv.Shutdown → cerrar SQLite.
package main

import (
	"context"
	"embed"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	webpush "github.com/SherClockHolmes/webpush-go"

	"easyzfs/internal/actions"
	"easyzfs/internal/alerts"
	"easyzfs/internal/apikeys"
	"easyzfs/internal/auth"
	"easyzfs/internal/backup"
	"easyzfs/internal/channels"
	"easyzfs/internal/collectors"
	"easyzfs/internal/config"
	"easyzfs/internal/db"
	"easyzfs/internal/executil"
	"easyzfs/internal/httpapi"
	"easyzfs/internal/hub"
	"easyzfs/internal/longops"
	"easyzfs/internal/notifier"
	"easyzfs/internal/priv"
	"easyzfs/internal/push"
	"easyzfs/internal/replication"
	"easyzfs/internal/scheduler"
	"easyzfs/internal/security"
	"easyzfs/internal/settings"
	"easyzfs/internal/updater"
	"easyzfs/internal/users"
	"easyzfs/internal/webhook"
)

// Inyectadas por ldflags (-X main.version=... -X main.build=...).
var (
	version = "dev"
	build   = ""
	// updateChannel — where this binary may be updated from. "local" (set by
	// this fork's Makefile) means from the checkout it was built from, by
	// rebuilding and reinstalling: the in-app updater is not created at all,
	// so the binary never contacts GitHub and exposes no update routes, and
	// an upstream release can never replace it from the UI. Anything else
	// keeps upstream's behaviour: releases of gnacho/easyzfs.
	updateChannel = "github"
)

//go:embed dist
var distFS embed.FS

func main() {
	// 'easyzfs priv …' — the privileged gateway, run as root by sudo. It
	// shares the binary so its checks are the service's own code, and it
	// must be dispatched before anything else: no config, no database.
	if len(os.Args) > 1 && os.Args[1] == "priv" {
		os.Exit(priv.Main(os.Args[2:]))
	}
	// Run as root through sudo, this binary is only ever the gateway. sudo
	// matches its 'priv *' rule against the space-joined arguments, so a
	// single argument "priv x" passes the rule without being "priv" here,
	// and would otherwise start the whole daemon as root.
	if os.Geteuid() == 0 && os.Getenv("SUDO_USER") != "" {
		fmt.Fprintln(os.Stderr, "easyzfs: bajo sudo solo se admite 'easyzfs priv …'")
		os.Exit(3)
	}
	// -generate-vapid: imprime un par de claves VAPID para /etc/easyzfs/env
	// (lo usa deploy/install.sh) y sale 0. No toca BD ni configuración.
	genVapid := flag.Bool("generate-vapid", false, "genera un par de claves VAPID (Web Push) y sale")
	// -update-channel: prints the update channel and exits. deploy/install.sh
	// asks the binary it installs, and skips the root update units for a
	// binary that is updated from its checkout instead.
	printChannel := flag.Bool("update-channel", false, "imprime el canal de actualización (local|github) y sale")
	flag.Parse()
	if *printChannel {
		fmt.Println(updateChannel)
		return
	}
	if *genVapid {
		priv, pub, err := webpush.GenerateVAPIDKeys()
		if err != nil {
			log.Fatalf("generate-vapid: %v", err)
		}
		fmt.Printf("VAPID_PUBLIC_KEY=%s\nVAPID_PRIVATE_KEY=%s\n", pub, priv)
		return
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg := config.Load()

	// Storage tools run through the privileged gateway (internal/priv):
	// unprivileged, via sudo to this binary; as root, with the same checks
	// in-process. Read-only mode keeps sudo's few pinned reads instead: its
	// sudoers file grants no gateway.
	if executil.SudoEnabled() {
		if !cfg.ReadOnly {
			if bin, err := os.Executable(); err == nil {
				if r, err := filepath.EvalSymlinks(bin); err == nil {
					bin = r
				}
				executil.PrivBin = bin
			}
		}
	} else {
		executil.PrivGate = actions.PrivCheck
	}

	database, err := db.Open(cfg.DBPath)
	if err != nil {
		log.Fatalf("sqlite: %v", err)
	}
	if err := db.Migrate(ctx, database); err != nil {
		log.Fatalf("migraciones: %v", err)
	}

	stStore, err := settings.NewStore(database)
	if err != nil {
		log.Fatalf("settings: %v", err)
	}

	userStore := users.NewStore(database)
	if err := userStore.Bootstrap(ctx, cfg.AdminPassword); err != nil {
		log.Fatalf("bootstrap usuarios: %v", err)
	}

	h := hub.NewHub()
	alerter := alerts.New(database, h, stStore)
	alerter.SetPoolMissingAfter(cfg.PoolMissingAfter)

	// Webhook saliente (issue #18): worker async con cola acotada + DLQ. La URL
	// se resuelve de settings en cada envío (dinámica, editable por UI); secret,
	// timeout y retries vienen de env leídos una vez al arranque (bootstrap).
	webhookNotifier := webhook.NewNotifier(webhook.Config{
		Secret:     cfg.WebhookSecret,
		Timeout:    cfg.WebhookTimeout,
		Retries:    cfg.WebhookRetries,
		RetryDelay: time.Second,
	}, database, func() string {
		st, err := stStore.Load(context.Background())
		if err != nil {
			return ""
		}
		return st.Webhook
	})
	alerter.SetWebhook(webhookNotifier)

	// Sender Web Push: inerte si faltan claves VAPID; en demo nunca envía.
	pushSender := push.New(cfg, database, h)
	alerter.SetPush(pushSender)
	// Ticker de la cola de quiet hours (60 s): entrega diferida al terminar
	// la ventana de silencio. En demo o sin VAPID queda inerte.
	go pushSender.RunQueue(ctx)

	// Canales ntfy/gotify/telegram/syslog/email (#86, #134): la config vive en
	// BD (editable desde Ajustes sin reiniciar); el entorno solo la siembra la
	// primera vez (compatibilidad con la config previa por env).
	channelStore := channels.NewStore(database)
	channelCfg, ok, err := channelStore.Load(ctx)
	if err != nil {
		log.Printf("aviso: no se pudo leer la config de canales: %v", err)
	}
	if !ok {
		channelCfg = channels.Config{
			NtfyURL:          cfg.NtfyURL,
			NtfyToken:        cfg.NtfyToken,
			GotifyURL:        cfg.GotifyURL,
			GotifyToken:      cfg.GotifyToken,
			TelegramBotToken: cfg.TelegramBotToken,
			TelegramChatID:   cfg.TelegramChatID,
			SyslogHost:       cfg.SyslogHost,
			SyslogPort:       cfg.SyslogPort,
			SyslogProto:      cfg.SyslogProto,
			SyslogFacility:   cfg.SyslogFacility,
			SMTPHost:         cfg.SMTPHost,
			SMTPPort:         cfg.SMTPPort,
			SMTPUser:         cfg.SMTPUser,
			SMTPPass:         cfg.SMTPPass,
			SMTPFrom:         cfg.SMTPFrom,
			SMTPEncryption:   cfg.SMTPEncryption,
		}
		if err := channelStore.Save(ctx, channelCfg); err != nil {
			log.Printf("aviso: no se pudo sembrar la config de canales: %v", err)
		}
	}
	channelsClient := channels.New(channelCfg)
	// El alerter SIEMPRE recibe el cliente: al configurar un canal desde la UI
	// entra en vigor sin reiniciar (el cliente decide por config en cada evento).
	alerter.SetChannels(channelsClient)
	log.Printf("canales de alerta: ntfy=%v gotify=%v telegram=%v syslog=%v email=%v",
		channelsClient.Configured("ntfy"), channelsClient.Configured("gotify"),
		channelsClient.Configured("telegram"), channelsClient.Configured("syslog"),
		channelsClient.Configured("email"))

	// Canal email (SMTP): mismo origen de config que los canales; se recrea en
	// caliente al guardar desde Ajustes.
	var emailNotifier *notifier.Mailer
	if m := httpapi.MailerFromConfig(channelCfg); m != nil {
		emailNotifier = m
		alerter.SetEmail(m)
	}

	// Colectores (reales o mock) + providers para los handlers.
	providers, cols := collectors.Build(cfg, database, h, alerter)

	// Never let a dataset be mounted over the data dir: the root update unit
	// installs whatever is in its update/ subdirectory.
	actions.ProtectMountpoint(cfg.DataDir())
	act := actions.NewService(database)
	jobStore := scheduler.NewStore(database)
	sched := scheduler.New(jobStore, act, h, providers.Disks.Disks)

	// Replicación ZFS send/recv (lote C): store propio + ejecución vía longops.
	longOps := longops.New(h)
	replRunner := replication.NewRunner(replication.NewStore(database), longOps, h, jobStore, cfg.DataDir(), cfg.Mock)
	if !cfg.ReadOnly {
		go replRunner.Run(ctx)
	}

	// Copia de seguridad de la BD: colector por frecuencia horaria + handlers.
	backupStore := backup.New(database, cfg.DBPath, stStore)
	go backupStore.RunLoop(ctx)

	// Updater: detecta releases semver y prepara el apply (el swap lo hace la
	// unit easyzfs-update.path). Inerte si version=dev o sin DATA_DIR escribible.
	var updaterSvc *updater.Updater
	if updateChannel != "local" {
		updaterSvc = updater.New(version, cfg.DataDir(), os.Getenv("GITHUB_TOKEN"))
	}
	// Chequeo inicial + ticker de 24 h: el estado se cachea y /api/update/status
	// lo lee sin tocar GitHub (evita el rate-limit de la API, patrón NetPulse).
	if updaterSvc == nil {
		log.Println("actualizaciones: desde el checkout local (sin updater, sin contacto con GitHub)")
	} else if cfg.NoUpdateCheck {
		log.Println("comprobación automática de actualizaciones desactivada (EASYZFS_NO_UPDATE_CHECK)")
	} else {
		updaterSvc.Start(ctx)
	}

	// Versión de OpenZFS del host (una vez, al arranque).
	zfsVersion := "mock"
	if !cfg.Mock {
		detCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		zfsVersion = collectors.DetectZFSVersion(detCtx)
		cancel()
	}

	// API keys de solo lectura (#87): el middleware de auth las valida.
	keyStore := apikeys.NewStore(database)
	authManager := auth.NewManager(database, cfg.SessionSecret, cfg.CookieSecure)
	authManager.SetAPIKeys(keyStore)

	srv := httpapi.NewServer(httpapi.Deps{
		Cfg: cfg, DB: database, Auth: authManager,
		Users: userStore, APIKeys: keyStore, Alerter: alerter, Settings: stStore,
		Pools: providers.Pools, Disks: providers.Disks, SysTimers: providers.SysTimers,
		Perf: providers.Perf, Caps: providers.Caps, DiskUse: providers.DiskUse, Host: providers.Host,
		Actions: act, Sched: sched, Jobs: jobStore, Hub: h, Push: pushSender,
		Backup: backupStore, LongOps: longOps, Repl: replRunner, Updater: updaterSvc,
		Channels: channelsClient, ChannelStore: channelStore, Mailer: emailNotifier,
		Version: version, Build: build, ZFSVersion: zfsVersion, UpdateChannel: updateChannel,
	})

	mux := http.NewServeMux()
	mux.Handle("/api/", srv.Handler())

	// SPA embebida: estáticos + fallback a index.html.
	webFS, err := fs.Sub(distFS, "dist")
	if err != nil {
		log.Fatalf("embed dist: %v", err)
	}
	mux.Handle("/", spaHandler(http.FS(webFS)))

	httpSrv := &http.Server{
		Addr: cfg.ListenAddr,
		// Cabeceras de seguridad HTTP en TODAS las respuestas (auditoría P3).
		Handler:           security.Middleware(mux),
		ReadHeaderTimeout: 10 * time.Second,
		// Sin WriteTimeout global: mataría las conexiones SSE.
	}

	// Arranque de colectores y scheduler (goroutines con ctx cancelable).
	for _, c := range cols {
		go c.Run(ctx)
	}
	// Read-only: no scheduled job (snapshots, scrubs, SMART tests) and no
	// replication runs; nothing changes storage unless a person does it.
	if cfg.ReadOnly {
		log.Println("modo solo lectura: tareas programadas y replicación desactivadas")
	} else {
		go sched.Run(ctx)
		// The recycle bin's purge destroys datasets on its own schedule; a
		// mock or demo deployment has nothing real to purge.
		if !cfg.Mock && !cfg.Demo {
			go act.RunTrashPurger(ctx)
		}
	}

	go func() {
		log.Printf("EasyZFS %s escuchando en %s (mock=%v demo=%v)", version, cfg.ListenAddr, cfg.Mock, cfg.Demo)
		if err := httpSrv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("http: %v", err)
		}
	}()

	<-ctx.Done()
	log.Println("apagando: drenando conexiones SSE y HTTP…")
	h.Close() // cierra clientes SSE con evento 'bye'
	shCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(shCtx); err != nil {
		log.Printf("shutdown: %v", err)
	}
	// Para el worker del webhook ANTES de cerrar SQLite (la DLQ escribe en BD).
	webhookNotifier.Close()
	if emailNotifier != nil {
		if err := emailNotifier.Close(); err != nil {
			log.Printf("email close: %v", err)
		}
	}
	if err := database.Close(); err != nil {
		log.Printf("sqlite close: %v", err)
	}
	log.Println("apagado limpio")
}

// spaHandler sirve la SPA embebida: fichero si existe, index.html si no
// (fallback de rutas del cliente). Cache-Control: index.html y sw.js siempre
// se revalidan (no-cache) para que un despliegue nuevo se vea al recargar;
// /assets/* es inmutable (Vite les pone hash en el nombre — si el hash cambia,
// es un fichero distinto); el resto (iconos, manifest) con caché corta.
func spaHandler(fsys http.FileSystem) http.Handler {
	fileSrv := http.FileServer(fsys)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		switch {
		case path == "/" || path == "/index.html" || path == "/sw.js":
			w.Header().Set("Cache-Control", "no-cache")
		case strings.HasPrefix(path, "/assets/"):
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		default:
			w.Header().Set("Cache-Control", "public, max-age=3600")
		}
		if path == "/" {
			path = "/index.html"
		}
		f, err := fsys.Open(path)
		if err != nil {
			// fallback SPA: cualquier ruta no encontrada devuelve index.html
			w.Header().Set("Cache-Control", "no-cache")
			r.URL.Path = "/"
			fileSrv.ServeHTTP(w, r)
			return
		}
		f.Close()
		fileSrv.ServeHTTP(w, r)
	})
}
