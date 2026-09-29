// Package push — sender Web Push (VAPID + webpush-go) con catálogo i18n ES/EN
// compuesto server-side. Reglas (skill web-push-alerts):
//   - El texto final (ES/EN) se compone aquí según push_subscriptions.lang del
//     dispositivo; el service worker solo pinta lo que llega.
//   - 404/410 del push service = suscripción muerta: DELETE en el mismo envío.
//     429/5xx: solo log (se conserva la fila).
//   - El endpoint es un secreto (capability URL): nunca se loguea completo,
//     solo una huella (hash truncado).
//   - Payload < 4 KB, sin datos sensibles; urgency "high" solo críticas.
//   - Modo demo: NUNCA se envía push real; solo log.
package push

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"log"
	"math/rand"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	webpush "github.com/SherClockHolmes/webpush-go"

	"easyzfs/internal/config"
	"easyzfs/internal/i18n"
)

// Hub — lo mínimo que el sender necesita del hub SSE: saber si un usuario
// tiene la app ABIERTA (conexión SSE activa) para no duplicar avisos.
type Hub interface {
	UserActive(userID string) bool
}

// Alert — datos de la alerta a notificar (kind estructurado + params).
type Alert struct {
	Level  string         // "info" | "warn" | "crit"
	Source string         // "pool.tank", "disk.sda"…
	Target string         // "pools:tank", "disks:sda", "tasks", "settings"…
	Kind   string         // pool_capacity | pool_status | scrub_errors | disk_temp | smart_status
	Params map[string]any // parámetros de interpolación del catálogo i18n
}

// subscription — fila de push_subscriptions (endpoint = secreto).
type subscription struct {
	id       int64
	userID   string
	endpoint string
	p256dh   string
	auth     string
	lang     string
	origin   string // origin del navegador al suscribir (para URLs absolutas)
}

// Sender envía notificaciones Web Push. Best-effort: Notify nunca devuelve
// error al llamador (la alerta ya está en BD y emitida por SSE).
type Sender struct {
	cfg    *config.Config
	db     *sql.DB
	hub    Hub
	client *http.Client                               // inyectable en tests
	sleep  func(ctx context.Context, d time.Duration) // espera entre reintentos (inyectable en tests)
}

// New crea el sender. Si !cfg.PushEnabled() queda inerte (Notify no hace nada).
func New(cfg *config.Config, db *sql.DB, h Hub) *Sender {
	return &Sender{
		cfg: cfg, db: db, hub: h,
		client: &http.Client{Timeout: 15 * time.Second},
		sleep:  esperaReal,
	}
}

// esperaReal — espera por defecto entre reintentos, cancelable con ctx.
func esperaReal(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

// --- operaciones de suscripción (usadas por los endpoints HTTP) ---

// Subscribe — upsert por endpoint: re-suscripciones y rotaciones actualizan
// la fila; también reasigna user_id (dos usuarios que comparten navegador).
// lang u origin vacíos (p. ej. el re-POST espontáneo de pushsubscriptionchange,
// que no conoce el idioma ni el origin) NO pisan lo guardado: se conserva lo de
// la fila.
func (s *Sender) Subscribe(ctx context.Context, userID, endpoint, p256dh, auth, lang, origin, userAgent string) error {
	if lang == "" && origin == "" {
		// Re-POST espontáneo (pushsubscriptionchange del SW): no tocar lang ni
		// origin — upsert conserva los existentes. A new row gets English, the
		// product's language: the column's own default (v1) is 'es'.
		_, err := s.db.ExecContext(ctx, `
			INSERT INTO push_subscriptions(user_id, endpoint, p256dh, auth, lang, user_agent, updated_at)
			VALUES (?,?,?,?,'en',?,datetime('now'))
			ON CONFLICT(endpoint) DO UPDATE SET
			  user_id=excluded.user_id, p256dh=excluded.p256dh, auth=excluded.auth,
			  user_agent=excluded.user_agent, updated_at=datetime('now')`,
			userID, endpoint, p256dh, auth, userAgent)
		return err
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO push_subscriptions(user_id, endpoint, p256dh, auth, lang, origin, user_agent, updated_at)
		VALUES (?,?,?,?,COALESCE(NULLIF(?,''),'en'),?,?,datetime('now'))
		ON CONFLICT(endpoint) DO UPDATE SET
		  user_id=excluded.user_id, p256dh=excluded.p256dh, auth=excluded.auth,
		  lang=COALESCE(NULLIF(excluded.lang,''), push_subscriptions.lang),
		  origin=COALESCE(NULLIF(excluded.origin,''), push_subscriptions.origin),
		  user_agent=excluded.user_agent, updated_at=datetime('now')`,
		userID, endpoint, p256dh, auth, lang, origin, userAgent)
	return err
}

// Unsubscribe borra la suscripción SOLO si pertenece al usuario de la sesión.
func (s *Sender) Unsubscribe(ctx context.Context, userID, endpoint string) error {
	_, err := s.db.ExecContext(ctx,
		"DELETE FROM push_subscriptions WHERE endpoint=? AND user_id=?", endpoint, userID)
	return err
}

// --- envío ---

// Notify envía la alerta a las suscripciones que proceda. Filtros por usuario:
//   - Preferencias: tipo deshabilitado en notification_preferences → se omite.
//   - Quiet hours: warn/info dentro de la ventana → se encola en
//     notification_queue (la vacía RunQueue al terminar la ventana);
//     crit atraviesa el silencio SIEMPRE (decisión deliberada).
//   - No duplicar: push SOLO a usuarios SIN conexión SSE activa, EXCEPTO
//     level=crit que se envía siempre.
//
// Nunca bloquea ni propaga errores al llamador.
func (s *Sender) Notify(ctx context.Context, a Alert) {
	if s.cfg.Demo {
		// Modo demo: sin push real, jamás. La alerta ya llega in-app por SSE.
		log.Printf("push: demo: alert %s/%s not sent (log only)", a.Kind, a.Source)
		return
	}
	if !s.cfg.PushEnabled() {
		return // push desactivado (aviso ya emitido al arrancar)
	}
	subs, err := s.list(ctx)
	if err != nil {
		log.Printf("push: list subscriptions: %v", err)
		return
	}
	// Preferencias y quiet hours se evalúan UNA vez por usuario (un usuario
	// puede tener N dispositivos suscritos).
	type decision struct{ omitir, encolado bool }
	decisiones := map[string]decision{}
	for _, sub := range subs {
		d, ok := decisiones[sub.userID]
		if !ok {
			// Las críticas (level=crit) atraviesan SIEMPRE las preferencias
			// por tipo: el preset "Ninguna" no puede silenciar un pool
			// FAULTED o errores de datos.
			if a.Level != "crit" && !s.prefEnabled(ctx, sub.userID, a.Kind) {
				d.omitir = true
			} else if a.Level != "crit" && s.inQuietHours(ctx, sub.userID, time.Now()) {
				if err := s.enqueue(ctx, sub.userID, a); err != nil {
					log.Printf("push: queue alert %s for %s: %v", a.Kind, sub.userID, err)
				}
				d.encolado = true
			}
			decisiones[sub.userID] = d
		}
		if d.omitir || d.encolado {
			continue
		}
		if a.Level != "crit" && s.hub != nil && s.hub.UserActive(sub.userID) {
			continue // la app está abierta y ya lo ve por SSE: no duplicar
		}
		s.sendTo(ctx, sub, a)
	}
}

// list carga todas las suscripciones (una por dispositivo/navegador).
func (s *Sender) list(ctx context.Context) ([]subscription, error) {
	// The user's language wins over the device's: the subscription keeps
	// the one its device had when it subscribed, and a user who switches
	// the UI to English must get English on every device (i18n.Resolve).
	return s.query(ctx, "")
}

// query — subscriptions with the language to write to each in, resolved
// from its user (see list); userID "" = all users.
func (s *Sender) query(ctx context.Context, userID string) ([]subscription, error) {
	q := `
		SELECT s.id, s.user_id, s.endpoint, s.p256dh, s.auth, s.lang, s.origin,
		       COALESCE(u.language, ''), COALESCE(u.ui_lang, '')
		FROM push_subscriptions s LEFT JOIN users u ON u.user = s.user_id`
	var args []any
	if userID != "" {
		q += " WHERE s.user_id=?"
		args = append(args, userID)
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []subscription
	for rows.Next() {
		var sub subscription
		var language, uiLang string
		if err := rows.Scan(&sub.id, &sub.userID, &sub.endpoint, &sub.p256dh, &sub.auth, &sub.lang, &sub.origin, &language, &uiLang); err != nil {
			return nil, err
		}
		sub.lang = i18n.Resolve(language, uiLang, sub.lang)
		out = append(out, sub)
	}
	return out, rows.Err()
}

// maxIntentos — intentos totales por envío (1 inicial + 2 reintentos) para
// errores transitorios (429/5xx y fallos de red).
const maxIntentos = 3

// sendTo compone el payload en el idioma del dispositivo y lo envía cifrado.
// Ciclo de vida (skill web-push-alerts):
//   - 404/410 → DELETE inmediato, sin reintento: la suscripción no resucita.
//   - 429/5xx y errores de red → reintento con backoff exponencial + jitter
//     (máx. maxIntentos); Retry-After del servicio se respeta como espera mínima.
//   - 400/401/403/413 → bug nuestro (VAPID/payload): log sin endpoint, sin retry.
func (s *Sender) sendTo(ctx context.Context, sub subscription, a Alert) {
	payload, err := json.Marshal(composePayload(sub.lang, sub.origin, a))
	if err != nil {
		log.Printf("push: marshal payload (sub %s): %v", fp(sub.endpoint), err)
		return
	}
	opts := &webpush.Options{
		HTTPClient:      s.client,
		Subscriber:      s.cfg.VAPIDSubject,
		VAPIDPublicKey:  s.cfg.VAPIDPublicKey,
		VAPIDPrivateKey: s.cfg.VAPIDPrivateKey,
		Topic:           a.Kind, // coalescing en tránsito (dispositivo offline)
		TTL:             ttlFor(a.Level),
	}
	if a.Level == "crit" {
		opts.Urgency = webpush.UrgencyHigh // 'high' SOLO críticas
	}
	ws := &webpush.Subscription{
		Endpoint: sub.endpoint,
		Keys:     webpush.Keys{P256dh: sub.p256dh, Auth: sub.auth},
	}
	backoff := 1 * time.Second
	for intento := 1; ; intento++ {
		resp, err := webpush.SendNotificationWithContext(ctx, payload, ws, opts)
		if err != nil {
			// Fallo de red/timeout (el http.Client ya tiene timeout): transitorio.
			if intento < maxIntentos {
				s.sleep(ctx, jitter(backoff))
				backoff *= 2
				continue
			}
			log.Printf("push: sending to sub %s failed %d times: %v", fp(sub.endpoint), intento, err)
			return
		}
		status := resp.StatusCode
		var retryAfter time.Duration
		if status == http.StatusTooManyRequests || status >= 500 {
			retryAfter = parseRetryAfter(resp.Header.Get("Retry-After"))
		}
		resp.Body.Close()
		switch {
		case status == http.StatusNotFound || status == http.StatusGone:
			// Suscripción muerta: no resucita jamás → borrar en el mismo envío.
			if _, err := s.db.ExecContext(ctx,
				"DELETE FROM push_subscriptions WHERE id=?", sub.id); err != nil {
				log.Printf("push: delete dead subscription %s: %v", fp(sub.endpoint), err)
			}
			return
		case status == http.StatusTooManyRequests || status >= 500:
			if intento < maxIntentos {
				// Retry-After manda como espera mínima sobre el backoff.
				espera := max(jitter(backoff), retryAfter)
				s.sleep(ctx, espera)
				backoff *= 2
				continue
			}
			log.Printf("push: service answers HTTP %d after %d attempts (sub %s): subscription kept",
				status, intento, fp(sub.endpoint))
			return
		case status >= 400:
			// 400/401/403/413: probablemente bug nuestro (VAPID/payload). Sin endpoint.
			log.Printf("push: HTTP %d on send (sub %s)", status, fp(sub.endpoint))
			return
		default:
			return // 2xx: entregado al push service
		}
	}
}

// jitter — entre el 50% y el 100% de d (evita reintentos sincronizados).
func jitter(d time.Duration) time.Duration {
	return d/2 + time.Duration(rand.Int63n(int64(d/2)+1))
}

// parseRetryAfter — cabecera Retry-After en segundos o fecha HTTP; 0 si ausente
// o inválida.
func parseRetryAfter(h string) time.Duration {
	h = strings.TrimSpace(h)
	if h == "" {
		return 0
	}
	if n, err := strconv.Atoi(h); err == nil {
		if n > 0 {
			return time.Duration(n) * time.Second
		}
		return 0
	}
	if t, err := http.ParseTime(h); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}

// fp — huella corta del endpoint para logs (el endpoint completo es secreto).
func fp(endpoint string) string {
	sum := sha256.Sum256([]byte(endpoint))
	return hex.EncodeToString(sum[:])[:12]
}

// ttlFor — TTL acorde a la vigencia (tabla de la skill): 1 h para críticas
// (una alerta de "caída" con horas de retraso ya no sirve) y 6 h para el resto.
func ttlFor(level string) int {
	if level == "crit" {
		return 3600
	}
	return 21600
}

// urlFor — destino navegable derivado de alert.target (router por hash de la SPA).
func urlFor(target string) string {
	base, _, _ := strings.Cut(target, ":")
	switch base {
	case "pools", "disks", "tasks", "settings":
		return "/#/" + base
	default:
		return "/"
	}
}

// absoluteURL — convierte la URL relativa de destino en absoluta con el origin
// guardado del dispositivo (Declarative Web Push exige notification.navigate
// absoluta; fallback a relativa si el origin está vacío o es inválido).
func absoluteURL(origin, rel string) string {
	if origin == "" {
		return rel
	}
	u, err := url.Parse(origin)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return rel
	}
	return strings.TrimSuffix(origin, "/") + rel
}

// payload — híbrido: campos planos (title/body/url/tag) para el handler push
// del SW + bloque Declarative Web Push (web_push/notification) que Safari/iOS
// 18.4+ procesa sin ejecutar el SW. < 4 KB y sin datos sensibles.
type payload struct {
	Title        string         `json:"title"`
	Body         string         `json:"body"`
	Tag          string         `json:"tag"` // coalescing en el dispositivo (renotify)
	URL          string         `json:"url"`
	Level        string         `json:"level"`
	WebPush      int            `json:"web_push"` // magic number Declarative Web Push
	Notification map[string]any `json:"notification"`
}

// composePayload — texto final ES/EN según el idioma del dispositivo. url y
// notification.navigate van absolutas cuando la suscripción guardó su origin.
func composePayload(lang, origin string, a Alert) payload {
	title, body := catalog(lang, a.Kind, a.Params)
	url := absoluteURL(origin, urlFor(a.Target))
	return payload{
		Title:   title,
		Body:    body,
		Tag:     a.Kind + ":" + a.Source,
		URL:     url,
		Level:   a.Level,
		WebPush: 8030,
		Notification: map[string]any{
			"title":    title,
			"body":     body,
			"navigate": url,
		},
	}
}
