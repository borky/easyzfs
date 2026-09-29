// demoI18n.ts — Spanish for the demo provider (mock.ts).
//
// The real server writes English and translates what it writes when the UI
// asks for Spanish (internal/i18n). The demo has no server: its sample data,
// activity feed, alerts and errors are English literals in mock.ts, so the
// same job is done here, at the provider's boundary (wrapDemoProvider), with
// the same rule: names, paths and values are never touched, only prose.
import { getLang } from '../ui/i18n';
import { ApiError } from './types';

// Exact texts: English → Spanish.
const EXACT: Record<string, string> = {
  'running': 'en curso',
  '0 errors (4h 12m)': '0 errores (4h 12m)',
  '0 errors · 4h 12m': '0 errores · 4h 12m',
  '0 errors': '0 errores',
  'completed without errors': 'completado sin errores',
  'cancelled by the user at 31%': 'cancelado por el usuario al 31%',
  'test started': 'test iniciado',
  'disk powered off': 'disco apagado',
  'Scrub of ssd running (62%)': 'Scrub de ssd en curso (62%)',
  'The nightly backup finished with warnings · check /var/log/backup.log': 'El backup nocturno terminó con avisos · revisa /var/log/backup.log',
  'smartd: nvme1n1 at a sustained 48 °C · check the cooling': 'smartd: nvme1n1 a 48 °C de forma sostenida · revisar ventilación',
  'Scrub of ssd completed · 0 errors': 'Scrub de ssd completado · 0 errores',
  'Checksum errors on nvme1n1 (ZFS event, pool tank)': 'Errores de checksum en nvme1n1 (evento ZFS, pool tank)',
  'Automatic snapshot created': 'Snapshot automático creado',
  'Manual snapshot created': 'Snapshot manual creado',
  'Scrub started on ssd': 'Scrub iniciado en ssd',
  'fortnightly schedule': 'programación quincenal',
  'Sign-in': 'Inicio de sesión',
  'Automatic backups': 'Respaldos automáticos',
  'every 24 h · 3-day retention': 'cada 24 h · retención 3 días',
  'Scrub completed on tank': 'Scrub completado en tank',
  'Scrub completed on ssd': 'Scrub completado en ssd',
  'User created': 'Usuario creado',
  'maria · user role': 'maria · rol usuario',
  'Password changed': 'Contraseña cambiada',
  'Manual database backup': 'Respaldo manual de la base de datos',
  'Automatic database backup': 'Respaldo automático de la base de datos',
  'Dataset created': 'Dataset creado',
  'Dataset unlocked': 'Dataset desbloqueado',
  'Dataset locked': 'Dataset bloqueado',
  'Encryption key changed': 'Clave de cifrado cambiada',
  'RAID-Z expansion completed': 'Expansión RAID-Z completada',
  'Data rewrite completed': 'Reescritura de datos completada',
  'Rewrite completed': 'Reescritura completada',
  'Cancelled by the user': 'Cancelada por el usuario',
  'Replication completed': 'Replicación completada',
  "ssh: Permission denied (publickey) — install the server's public key on the destination": 'ssh: Permission denied (publickey) — instala la clave pública del servidor en el destino',
  'Add this key to ~/.ssh/authorized_keys of the destination user. To avoid root: zfs allow -u <user> snapshot,send,receive,destroy,hold,bookmark <pool>': 'Añade esta clave a ~/.ssh/authorized_keys del usuario destino. Para no usar root: zfs allow -u <usuario> snapshot,send,receive,destroy,hold,bookmark <pool>',
  "authentication failed (Permission denied): install the server's public key in the destination user's authorized_keys": 'autenticación fallida (Permission denied): instala la clave pública del servidor en el authorized_keys del usuario destino',
  'not available': 'no disponible',
  'Not signed in': 'Sesión no iniciada',
  'Two-step verification is not available in demo mode': 'En modo demo la verificación en dos pasos no está disponible',
  'Image too large (max. 512 KB)': 'Imagen demasiado grande (máx. 512 KB)',
  'The user already exists': 'El usuario ya existe',
  'Wrong confirmation': 'Confirmación incorrecta',
  'You cannot delete yourself': 'No puedes eliminarte a ti mismo',
  'Pool not found': 'Pool no encontrado',
  'Vdev not found': 'Vdev no encontrado',
  'Disk not found': 'Disco no encontrado',
  'it is a two-disk mirror: removing one leaves the pool without redundancy': 'es un mirror de dos discos: al retirar uno, el pool se queda sin redundancia',
  'The passphrase must be at least 8 characters long': 'La passphrase debe tener al menos 8 caracteres',
  'The new passphrase must be at least 8 characters long': 'La passphrase nueva debe tener al menos 8 caracteres',
  'The dataset is not encrypted': 'El dataset no está cifrado',
  'The passphrase is required': 'Se requiere la passphrase',
  'The current passphrase is required': 'Se requiere la passphrase actual',
  'RAID-Z expansion needs OpenZFS ≥ 2.3': 'RAID-Z expansion requiere OpenZFS ≥ 2.3',
  'zfs rewrite needs OpenZFS ≥ 2.3.4': 'zfs rewrite requiere OpenZFS ≥ 2.3.4',
  'it is not in the recycle bin': 'no existe en la papelera',
  'Operation not found': 'Operación no encontrada',
  'The operation is no longer running': 'La operación ya no está en curso',
  'The dataset does not exist, is not a filesystem, or is not mounted': 'Dataset inexistente, no es filesystem o no está montado',
  'Replication job not found': 'Job de replicación no encontrado',
  'A replication is already running for this job': 'Ya hay una replicación en curso para este job',
  'task not found or not editable': 'tarea no encontrada o no editable',
  'task not found or cannot be migrated': 'tarea no encontrada o no migrable',
  'the disk has mounted partitions or active swap': 'el disco tiene particiones montadas o swap activo',
};

// Texts with values in them.
const PATTERNS: [RegExp, (...m: string[]) => string][] = [
  [/^Type "(.*)" to confirm$/, (v) => `Escribe "${v}" para confirmar`],
  [/^the pool is (\S+): removing another disk now can leave it without redundancy$/, (s) => `el pool está ${s}: quitar otro disco ahora puede dejarlo sin redundancia`],
  [/^(.+) is left without parity: without redundancy, one more failure loses the data$/, (g) => `${g} se queda sin paridad: sin redundancia, un fallo más pierde los datos`],
  [/^Vdev '(.*)' is not a raidz of the pool$/, (v) => `El vdev '${v}' no es un raidz del pool`],
  [/^Disk '(.*)' is not free$/, (d) => `El disco '${d}' no está libre`],
  [/^(.+) has child datasets; mark it recursive$/, (n) => `${n} tiene datasets hijos; márcalo como recursivo`],
  [/^(.+) already exists$/, (n) => `ya existe ${n}`],
  [/^An operation is already running on (.+)$/, (n) => `Ya hay una operación en curso sobre ${n}`],
  [/^Rewriting the blocks of (.+)…$/, (p) => `Reescribiendo bloques de ${p}…`],
  [/^(\d+) % of the blocks processed…$/, (n) => `Procesados ${n} % de los bloques…`],
  [/^the disk belongs to pool '(.*)'$/, (p) => `el disco pertenece al pool '${p}'`],
];

// The fields that carry prose, as on the server (internal/i18n/json.go):
// only these are translated. Names, paths, commands and schedules are data,
// and must come back exactly as they are (an edit form writes them back).
const PROSE_KEYS = new Set([
  'message', 'error', 'warnings', 'reason', 'detail', 'text', 'title', 'summary',
  'instructions', 'lines', 'version', 'remote_version', 'applyRefused',
]);

function proseKey(k: string): boolean {
  return PROSE_KEYS.has(k) || k.endsWith('_reason') || k.endsWith('_error') ||
    k.endsWith('_detail') || k.endsWith('_result');
}

export function demoSpanish(s: string): string {
  const exact = EXACT[s];
  if (exact !== undefined) return exact;
  for (const [re, fn] of PATTERNS) {
    const m = re.exec(s);
    if (m) return fn(...m.slice(1));
  }
  return s;
}

function deep<T>(v: T, key = ''): T {
  if (typeof v === 'string') return (proseKey(key) ? demoSpanish(v) : v) as T;
  if (Array.isArray(v)) return v.map((x) => deep(x, key)) as T;
  if (v && typeof v === 'object' && Object.getPrototypeOf(v) === Object.prototype) {
    const out: Record<string, unknown> = {};
    for (const [k, x] of Object.entries(v as Record<string, unknown>)) out[k] = deep(x, k);
    return out as T;
  }
  return v;
}

// demoText — a value the demo emits by itself (an SSE event), in the UI's
// language.
export function demoText<T>(v: T): T {
  return getLang() === 'es' ? deep(v) : v;
}

function translateError(e: unknown): unknown {
  if (getLang() === 'es' && e instanceof ApiError) {
    return new ApiError(e.status, e.code, demoSpanish(e.message));
  }
  return e;
}

// wrapDemoProvider — every method of the demo provider answers, and fails,
// in the UI's language. Results are copied, never changed in place: the mock
// keeps its own state in English.
export function wrapDemoProvider<P extends object>(p: P): P {
  return new Proxy(p, {
    get(target, prop, receiver) {
      const v = Reflect.get(target, prop, receiver);
      if (typeof v !== 'function') return v;
      return (...args: unknown[]) => {
        let out: unknown;
        try {
          out = (v as (...a: unknown[]) => unknown).apply(target, args);
        } catch (e) {
          throw translateError(e);
        }
        if (out instanceof Promise) {
          return out.then((r) => demoText(r), (e) => { throw translateError(e); });
        }
        return demoText(out);
      };
    },
  });
}
