// Bus de eventos en tiempo real.
// En modo HTTP: EventSource a /api/events (SSE).
// En modo mock: el propio MockProvider emite eventos sintéticos por aquí.
import type { AppEvent } from './types';
import { getLang, onLangChange } from '../ui/i18n';

type Listener = (ev: AppEvent) => void;

const listeners = new Set<Listener>();
let es: EventSource | null = null;

// Evento global de "sesión expirada": lo emite http.ts ante un 401 y este
// módulo tras 3 fallos 401 consecutivos del stream SSE. El store lo escucha
// y fuerza logout + vuelta al login.
export const AUTH_EXPIRED_EVENT = 'zfsctl:auth-expired';

export function notifyAuthExpired(): void {
  try { window.dispatchEvent(new CustomEvent(AUTH_EXPIRED_EVENT)); } catch { /* sin window (SSR/tests) */ }
}

export function subscribeEvents(fn: Listener): () => void {
  listeners.add(fn);
  return () => listeners.delete(fn);
}

export function emitEvent(ev: AppEvent): void {
  listeners.forEach((fn) => {
    try { fn(ev); } catch { /* listener defectuoso no rompe el bus */ }
  });
}

// ---------- SSE con reconexión manual ----------
// EventSource solo reconecta solo si la conexión llegó a abrirse; ante un
// error HTTP (401/5xx) queda CLOSED para siempre. Por eso gestionamos la
// reconexión nosotros: backoff exponencial 1s→2s→…→30s (reset al conectar
// OK). En cada error se sondea /api/me para conocer el estado HTTP: tras 3
// respuestas 401 consecutivas se fuerza el logout (sesión caducada).
const RETRY_MIN_MS = 1000;
const RETRY_MAX_MS = 30000;
const MAX_AUTH_FAILS = 3;

let retryMs = RETRY_MIN_MS;
let retryTimer: ReturnType<typeof setTimeout> | null = null;
let authFails = 0;
let stopped = true;

const SSE_TYPES: AppEvent['type'][] = [
  'pool.status', 'scrub.progress', 'disk.temp', 'disk.smart', 'alert.new', 'job.finished', 'longop.update', 'overview',
];

function clearRetry(): void {
  if (retryTimer !== null) { clearTimeout(retryTimer); retryTimer = null; }
}

function openStream(): void {
  if (stopped) return;
  try {
    // An EventSource cannot set headers: the language goes in the URL.
    es = new EventSource(`/api/events?lang=${getLang()}`);
  } catch {
    es = null;
    scheduleRetry();
    return;
  }
  es.onopen = () => {
    // Conexión OK: reset de backoff y de fallos de autenticación
    retryMs = RETRY_MIN_MS;
    authFails = 0;
  };
  es.onerror = () => {
    if (es) { es.close(); es = null; }
    void probeAndRetry();
  };
  for (const t of SSE_TYPES) {
    es.addEventListener(t, (m) => {
      try {
        const data = JSON.parse((m as MessageEvent).data) as Record<string, unknown>;
        emitEvent({ type: t, ...data } as AppEvent);
      } catch { /* payload inválido: ignorar */ }
    });
  }
}

function scheduleRetry(): void {
  if (stopped || retryTimer !== null) return;
  retryTimer = setTimeout(() => {
    retryTimer = null;
    openStream();
  }, retryMs);
  retryMs = Math.min(retryMs * 2, RETRY_MAX_MS);
}

// Sondea el estado HTTP de la sesión para distinguir 401 de errores de red/5xx
async function probeAndRetry(): Promise<void> {
  if (stopped) return;
  let status = 0;
  try {
    const res = await fetch('/api/me', { credentials: 'same-origin' });
    status = res.status;
    if (res.ok) authFails = 0; // el servidor responde y la sesión sigue viva
  } catch {
    status = 0; // red caída
  }
  if (stopped) return;
  if (status === 401) {
    authFails += 1;
    if (authFails >= MAX_AUTH_FAILS) {
      disconnectSSE();
      notifyAuthExpired();
      return;
    }
  }
  scheduleRetry();
}

// The stream's language is fixed when it opens: reopen it on a change, so
// the events that follow arrive in the new language.
onLangChange(() => {
  if (stopped || !es) return;
  es.close();
  es = null;
  openStream();
});

// Conecta al stream SSE real. Devuelve función de desconexión.
export function connectSSE(): () => void {
  disconnectSSE();
  stopped = false;
  retryMs = RETRY_MIN_MS;
  authFails = 0;
  openStream();
  return disconnectSSE;
}

export function disconnectSSE(): void {
  stopped = true;
  clearRetry();
  if (es) { es.close(); es = null; }
}
