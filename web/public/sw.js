// ============================================================================
// sw.js — Service worker de EasyZFS (handlers Web Push). JS plano, sin build.
//
// REGLA CRÍTICA: TODO evento "push" termina en showNotification().
//   - Chrome: si no, muestra un aviso genérico al usuario.
//   - Safari: si no, REVOCA el permiso de notificaciones (doc Apple).
// Por eso el handler tiene try/catch con fallback visible.
//
// El servidor envía payload híbrido: campos planos (title/body/url/tag/level)
// para el handler "push" de abajo + bloque "web_push"/"notification"
// (Declarative Web Push) que Safari/iOS 18.4+ procesa SIN ejecutar este SW.
// ============================================================================

const ICON = "/icons/icon-192.png";

// === PUSH: recepción =========================================================
self.addEventListener("push", (event) => {
  event.waitUntil(
    (async () => {
      let datos = {};
      try {
        datos = event.data ? event.data.json() : {};
      } catch (_err) {
        // Payload corrupto o no-JSON: se muestra el fallback igualmente.
        datos = {};
      }

      // Fallback visible: NUNCA salir del handler sin notificación.
      // El texto ya llega FINAL e i18n desde el servidor (ES/EN).
      const titulo = datos.title || "EasyZFS";
      const opciones = {
        // With a broken payload the reader's language is unknown: English,
        // the product's language.
        body: datos.body || "You have a new alert.",
        icon: ICON,
        badge: ICON,
        tag: datos.tag || "easyzfs", // coalescing: mismo tag reemplaza la anterior
        renotify: true,              // que el reemplazo vibre/suene de nuevo
        data: { url: datos.url || "/" },
        // NO usar actions/image/requireInteraction: no soportados en iOS.
      };

      await self.registration.showNotification(titulo, opciones);
    })(),
  );
});

// === PUSH: click en la notificación ==========================================
self.addEventListener("notificationclick", (event) => {
  event.notification.close();
  const url = (event.notification.data && event.notification.data.url) || "/";
  // WindowClient.url es ABSOLUTA y la url del payload puede ser relativa
  // ("/#/pools"): normalizar antes de comparar, si no la rama focus es
  // código muerto.
  const abs = new URL(url, self.location.origin).href;

  event.waitUntil(
    clients
      .matchAll({ type: "window", includeUncontrolled: true })
      .then((lista) => {
        // Si ya hay una ventana con esa URL, enfocarla en vez de abrir otra.
        for (const cliente of lista) {
          if (cliente.url === abs && "focus" in cliente) return cliente.focus();
        }
        return clients.openWindow(url);
      }),
  );
});

// === PUSH: renovación automática de la suscripción ===========================
// El navegador puede rotar la suscripción (p. ej. tras restaurar datos).
// Re-suscribimos con la misma applicationServerKey y re-enviamos al servidor,
// que hace upsert por endpoint. Cobertura irregular entre navegadores: es red
// de seguridad; el mecanismo principal de higiene es el borrado por 404/410
// y la re-sincronización del hook usePush al abrir la app.
self.addEventListener("pushsubscriptionchange", (event) => {
  event.waitUntil(
    self.registration.pushManager
      .subscribe({
        userVisibleOnly: true,
        applicationServerKey:
          event.oldSubscription && event.oldSubscription.options
            ? event.oldSubscription.options.applicationServerKey
            : undefined,
      })
      .then((sub) =>
        fetch("/api/push/subscribe", {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          // La sesión viaja por cookie HttpOnly (mismo origen, SameSite=Lax).
          // El idioma se re-sincroniza al abrir la app (hook usePush).
          body: JSON.stringify(sub),
        }),
      ),
  );
});
