// Selector de provider: el provider HTTP es el único camino para sesiones
// reales. El mock solo se activa con una sesión demo explícita (botón
// "Entrar en modo demo" del login), persistida en localStorage.
import type { DataProvider } from './provider';
import { HttpProvider } from './http';
import { MockProvider } from './mock';
import { wrapDemoProvider } from './demoI18n';
import { connectSSE, disconnectSSE } from './events';

const DEMO_KEY = 'zfc-demo';
const DEMO_USER = 'demo';

let current: DataProvider | null = null;
let demo = false;

export function getProvider(): DataProvider {
  if (!current) throw new Error('Provider no inicializado');
  return current;
}

export function isDemo(): boolean {
  return demo;
}

// ¿Hay una sesión demo guardada? (sobrevive a refresh)
export function hasDemoSession(): boolean {
  return localStorage.getItem(DEMO_KEY) === '1';
}

function setProvider(p: DataProvider, isDemoMode: boolean) {
  if (current instanceof MockProvider) current.dispose();
  disconnectSSE();
  current = p;
  demo = isDemoMode;
  if (!isDemoMode) connectSSE();
}

async function startMock(): Promise<void> {
  const m = new MockProvider();
  setProvider(wrapDemoProvider(m), true);
  // Sesión local "demo" (no toca el backend). Permite elegir el usuario
  // demo (p.ej. 'maria' para probar el rol no-admin) vía localStorage.
  await m.login(localStorage.getItem('zfc-demo-user') || DEMO_USER, '');
}

// Inicialización al arrancar la app
export async function initProvider(): Promise<{ demo: boolean }> {
  if (hasDemoSession()) {
    await startMock();
    return { demo: true };
  }
  setProvider(new HttpProvider(), false);
  return { demo: false };
}

// Entrar en modo demo: sesión local, sin llamar al backend
export async function enterDemoSession(): Promise<void> {
  localStorage.setItem(DEMO_KEY, '1');
  await startMock();
}

// Salir del modo demo: vuelve al provider HTTP (habrá que hacer login real)
export function exitDemoSession(): void {
  localStorage.removeItem(DEMO_KEY);
  setProvider(new HttpProvider(), false);
}
