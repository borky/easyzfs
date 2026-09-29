// push.ts — hook usePush (Web Push) para EasyZFS, adaptado de la skill
// web-push-alerts (assets/push-client.template.ts).
//
// Reglas:
//   * Notification.requestPermission() NUNCA en carga de página: solo tras un
//     gesto del usuario (el botón "Activar alertas" llama a subscribe()).
//   * iOS/iPadOS: push SOLO con la PWA instalada en pantalla de inicio.
//   * Si el permiso es "denied": no re-pedir jamás; la UI da instrucciones.
//   * El texto de las notificaciones se compone server-side (ES/EN); el
//     dispositivo solo guarda su idioma (lang) en la suscripción.
import { useCallback, useEffect, useState } from 'react';
import { getProvider, isDemo } from './index';
import { getCurrentLang } from '../ui/i18n';
import { ApiError } from './types';
import type { PushSubscriptionJSON } from './types';

// --- Helpers de detección (references/ios-y-soporte.md) ----------------------

/** ¿PWA corriendo instalada (standalone)? */
export function isStandalone(): boolean {
  return (
    window.matchMedia('(display-mode: standalone)').matches ||
    (navigator as { standalone?: boolean }).standalone === true // iOS Safari legacy
  );
}

/** ¿Es iOS/iPadOS? (incluye iPadOS identificándose como Mac) */
export function isIOS(): boolean {
  return (
    /iPad|iPhone|iPod/.test(navigator.userAgent) ||
    (navigator.platform === 'MacIntel' && navigator.maxTouchPoints > 1)
  );
}

/** ¿Soporta Web Push ESTE contexto? */
export function supportsPush(): boolean {
  return 'serviceWorker' in navigator && 'PushManager' in window && 'Notification' in window;
}

/**
 * Re-sincronización silenciosa de la suscripción con el servidor (upsert por
 * endpoint) con el idioma y origin actuales. Se llama tras login/arranque
 * autenticado (NO en demo): cubre rotaciones de pushsubscriptionchange
 * perdidas y cambios de idioma. Nunca lanza ni pide permiso.
 */
export async function syncPushSubscription(): Promise<void> {
  if (isDemo() || !supportsPush()) return;
  if (Notification.permission !== 'granted') return;
  try {
    const reg = await navigator.serviceWorker.ready;
    const sub = await reg.pushManager.getSubscription();
    if (sub) {
      await getProvider().pushSubscribe(sub.toJSON() as PushSubscriptionJSON, getCurrentLang());
    }
  } catch { /* silenciosa: se reintenta en el próximo arranque */ }
}

// --- Helper base64url → Uint8Array (para applicationServerKey) ---------------
export function urlBase64ToUint8Array(base64: string): Uint8Array {
  const padding = '='.repeat((4 - (base64.length % 4)) % 4);
  const b64 = (base64 + padding).replace(/-/g, '+').replace(/_/g, '/');
  const raw = atob(b64);
  const salida = new Uint8Array(raw.length);
  for (let i = 0; i < raw.length; i++) salida[i] = raw.charCodeAt(i);
  return salida;
}

// --- Tipos del hook ------------------------------------------------------------

export type PushState =
  | 'unknown'            // detectando soporte/estado
  | 'unsupported'        // este navegador/contexto no soporta Web Push
  | 'needs-ios-install'  // iOS sin la PWA instalada en pantalla de inicio
  | 'demo'               // sesión demo: sin push real (regla demo de la skill)
  | 'not-configured'     // servidor sin claves VAPID (503 push_not_configured)
  | 'idle'               // soportado y configurado, sin suscribir
  | 'subscribing'        // gesto en curso (prompt nativo + subscribe)
  | 'subscribed'         // alertas activadas en este dispositivo
  | 'denied'             // permiso denegado: NO re-pedir, instrucciones manuales
  | 'error';

export function usePush(): {
  state: PushState;
  error: string | null;
  subscribe: () => Promise<boolean>;
  unsubscribe: () => Promise<void>;
} {
  const [state, setState] = useState<PushState>('unknown');
  const [error, setError] = useState<string | null>(null);

  // Detección inicial: soporte, demo, configuración del servidor y posible
  // suscripción existente (re-sincronizada con el servidor: cubre rotaciones
  // de pushsubscriptionchange perdidas).
  useEffect(() => {
    let cancelado = false;
    (async () => {
      if (isDemo()) {
        if (!cancelado) setState('demo');
        return;
      }
      if (!supportsPush()) {
        if (!cancelado) setState(isIOS() && !isStandalone() ? 'needs-ios-install' : 'unsupported');
        return;
      }
      if (isIOS() && !isStandalone()) {
        if (!cancelado) setState('needs-ios-install');
        return;
      }
      try {
        await getProvider().getPushVapidKey(); // 503 = sin claves VAPID
      } catch (e) {
        if (!cancelado) setState(e instanceof ApiError && e.status === 503 ? 'not-configured' : 'error');
        return;
      }
      let sub: PushSubscription | null = null;
      try {
        const reg = await navigator.serviceWorker.ready;
        sub = await reg.pushManager.getSubscription();
        if (sub) {
          // Re-sincronización silenciosa (upsert por endpoint en el servidor).
          await getProvider()
            .pushSubscribe(sub.toJSON() as PushSubscriptionJSON, getCurrentLang())
            .catch(() => {});
        }
      } catch { /* SW aún no listo: se reintenta en la próxima carga */ }
      if (!cancelado) {
        if (sub) setState('subscribed');
        else if (Notification.permission === 'denied') setState('denied');
        else setState('idle');
      }
    })();
    return () => { cancelado = true; };
  }, []);

  /**
   * Activa las alertas. LLAMAR SOLO DESDE UN GESTO DEL USUARIO (onClick),
   * tras la tarjeta propia que explica qué alertas llegarán.
   */
  const subscribe = useCallback(async (): Promise<boolean> => {
    if (state !== 'idle' && state !== 'error') return false;
    setState('subscribing');
    setError(null);
    try {
      // 1) Permiso nativo (tras el gesto). Si denied: no se puede re-pedir.
      const permiso = await Notification.requestPermission();
      if (permiso !== 'granted') {
        setState(permiso === 'denied' ? 'denied' : 'idle');
        return false;
      }
      // 2) Clave pública VAPID del servidor.
      const { publicKey } = await getProvider().getPushVapidKey();
      if (!publicKey) throw new Error('push_not_configured');
      // 3) Suscripción (el SW debe estar activo: usar siempre .ready).
      const reg = await navigator.serviceWorker.ready;
      const sub = await reg.pushManager.subscribe({
        userVisibleOnly: true, // contrato: todo push visible (regla crítica)
        applicationServerKey: urlBase64ToUint8Array(publicKey) as BufferSource,
      });
      // 4) Enviar al servidor (upsert por endpoint) con el idioma actual.
      await getProvider().pushSubscribe(sub.toJSON() as PushSubscriptionJSON, getCurrentLang());
      setState('subscribed');
      return true;
    } catch (err) {
      setError(err instanceof Error ? err.message : 'unknown_error');
      setState('error');
      return false;
    }
  }, [state]);

  /** Desactiva las alertas en este dispositivo (gesto del usuario). */
  const unsubscribe = useCallback(async (): Promise<void> => {
    setError(null);
    try {
      const reg = await navigator.serviceWorker.ready;
      const sub = await reg.pushManager.getSubscription();
      if (sub) {
        await getProvider().pushUnsubscribe(sub.endpoint).catch(() => {});
        await sub.unsubscribe();
      }
      setState('idle');
    } catch (err) {
      setError(err instanceof Error ? err.message : 'unknown_error');
      setState('error');
    }
  }, []);

  return { state, error, subscribe, unsubscribe };
}
