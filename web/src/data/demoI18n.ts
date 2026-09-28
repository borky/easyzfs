// demoI18n.ts — English for the demo provider (mock.ts).
//
// The real server translates what it writes when the UI asks for English
// (internal/i18n). The demo has no server: its sample data, activity feed,
// alerts and errors are Spanish literals in mock.ts, so the same job is done
// here, at the provider's boundary (wrapDemoProvider), with the same rule:
// names, paths and values are never touched, only prose.
import { getLang } from '../ui/i18n';
import { ApiError } from './types';

// Exact texts.
const EXACT: Record<string, string> = {
  // jobs, history, system timers
  'semanal': 'weekly',
  'en curso': 'running',
  '0 errores (4h 12m)': '0 errors (4h 12m)',
  '0 errores · 4h 12m': '0 errors · 4h 12m',
  '0 errores': '0 errors',
  'completado sin errores': 'completed without errors',
  'cancelado por el usuario al 31%': 'cancelled by the user at 31%',
  'logrotate.timer · diario': 'logrotate.timer · daily',
  'man-db.timer · diario': 'man-db.timer · daily',
  'Backup nocturno (crontab de root)': 'Nightly backup (root crontab)',
  'Trim semanal (zfsutils)': 'Weekly trim (zfsutils)',
  'test iniciado': 'test started',
  'disco apagado': 'disk powered off',
  // alerts
  'Scrub de ssd en curso (62%)': 'Scrub of ssd running (62%)',
  'El backup nocturno terminó con avisos · revisa /var/log/backup.log': 'The nightly backup finished with warnings · check /var/log/backup.log',
  'smartd: nvme1n1 a 48 °C de forma sostenida · revisar ventilación': 'smartd: nvme1n1 at a sustained 48 °C · check the cooling',
  'Scrub de ssd completado · 0 errores': 'Scrub of ssd completed · 0 errors',
  'Errores de checksum en nvme1n1 (evento ZFS, pool tank)': 'Checksum errors on nvme1n1 (ZFS event, pool tank)',
  // activity feed
  'Snapshot automático creado': 'Automatic snapshot created',
  'Snapshot manual creado': 'Manual snapshot created',
  'Scrub iniciado en ssd': 'Scrub started on ssd',
  'programación quincenal': 'fortnightly schedule',
  'Inicio de sesión': 'Sign-in',
  'Respaldos automáticos': 'Automatic backups',
  'cada 24 h · retención 3 días': 'every 24 h · 3-day retention',
  'Scrub completado en tank': 'Scrub completed on tank',
  'Scrub completado en ssd': 'Scrub completed on ssd',
  'Usuario creado': 'User created',
  'maria · rol usuario': 'maria · user role',
  'Contraseña cambiada': 'Password changed',
  'Respaldo manual de la base de datos': 'Manual database backup',
  'Respaldo automático de la base de datos': 'Automatic database backup',
  'Dataset creado': 'Dataset created',
  'Dataset desbloqueado': 'Dataset unlocked',
  'Dataset bloqueado': 'Dataset locked',
  'Clave de cifrado cambiada': 'Encryption key changed',
  'Expansión RAID-Z completada': 'RAID-Z expansion completed',
  'Reescritura de datos completada': 'Data rewrite completed',
  // long operations and replication
  'Reescritura completada': 'Rewrite completed',
  'Cancelada por el usuario': 'Cancelled by the user',
  'Replicación completada': 'Replication completed',
  'ssh: Permission denied (publickey) — instala la clave pública del servidor en el destino': 'ssh: Permission denied (publickey) — install the server\'s public key on the destination',
  'Añade esta clave a ~/.ssh/authorized_keys del usuario destino. Para no usar root: zfs allow -u <usuario> snapshot,send,receive,destroy,hold,bookmark <pool>': 'Add this key to ~/.ssh/authorized_keys of the destination user. To avoid root: zfs allow -u <user> snapshot,send,receive,destroy,hold,bookmark <pool>',
  'autenticación fallida (Permission denied): instala la clave pública del servidor en el authorized_keys del usuario destino': 'authentication failed (Permission denied): install the server\'s public key in the destination user\'s authorized_keys',
  // disks
  'no disponible': 'not available',
  // errors
  'Sesión no iniciada': 'Not signed in',
  'En modo demo la verificación en dos pasos no está disponible': 'Two-step verification is not available in demo mode',
  'Imagen demasiado grande (máx. 512 KB)': 'Image too large (max. 512 KB)',
  'El usuario ya existe': 'The user already exists',
  'Confirmación incorrecta': 'Wrong confirmation',
  'No puedes eliminarte a ti mismo': 'You cannot delete yourself',
  'Pool no encontrado': 'Pool not found',
  'Vdev no encontrado': 'Vdev not found',
  'Disco no encontrado': 'Disk not found',
  'es un mirror de dos discos: al retirar uno, el pool se queda sin redundancia': 'it is a two-disk mirror: removing one leaves the pool without redundancy',
  'La passphrase debe tener al menos 8 caracteres': 'The passphrase must be at least 8 characters long',
  'La passphrase nueva debe tener al menos 8 caracteres': 'The new passphrase must be at least 8 characters long',
  'El dataset no está cifrado': 'The dataset is not encrypted',
  'Se requiere la passphrase': 'The passphrase is required',
  'Se requiere la passphrase actual': 'The current passphrase is required',
  'RAID-Z expansion requiere OpenZFS ≥ 2.3': 'RAID-Z expansion needs OpenZFS ≥ 2.3',
  'zfs rewrite requiere OpenZFS ≥ 2.3.4': 'zfs rewrite needs OpenZFS ≥ 2.3.4',
  'no existe en la papelera': 'it is not in the recycle bin',
  'Operación no encontrada': 'Operation not found',
  'La operación ya no está en curso': 'The operation is no longer running',
  'Dataset inexistente, no es filesystem o no está montado': 'The dataset does not exist, is not a filesystem, or is not mounted',
  'Job de replicación no encontrado': 'Replication job not found',
  'Ya hay una replicación en curso para este job': 'A replication is already running for this job',
  'tarea no encontrada o no editable': 'task not found or not editable',
  'tarea no encontrada o no migrable': 'task not found or cannot be migrated',
  'el disco tiene particiones montadas o swap activo': 'the disk has mounted partitions or active swap',
};

// Texts with values in them.
const PATTERNS: [RegExp, (...m: string[]) => string][] = [
  [/^Escribe "(.*)" para confirmar$/, (v) => `Type "${v}" to confirm`],
  [/^el pool está (\S+): quitar otro disco ahora puede dejarlo sin redundancia$/, (s) => `the pool is ${s}: removing another disk now can leave it without redundancy`],
  [/^(.+) se queda sin paridad: sin redundancia, un fallo más pierde los datos$/, (g) => `${g} is left without parity: without redundancy, one more failure loses the data`],
  [/^El vdev '(.*)' no es un raidz del pool$/, (v) => `Vdev '${v}' is not a raidz of the pool`],
  [/^El disco '(.*)' no está libre$/, (d) => `Disk '${d}' is not free`],
  [/^(.+) tiene datasets hijos; márcalo como recursivo$/, (n) => `${n} has child datasets; mark it recursive`],
  [/^ya existe (.+)$/, (n) => `${n} already exists`],
  [/^Ya hay una operación en curso sobre (.+)$/, (n) => `An operation is already running on ${n}`],
  [/^Reescribiendo bloques de (.+)…$/, (p) => `Rewriting the blocks of ${p}…`],
  [/^Procesados (\d+) % de los bloques…$/, (n) => `${n} % of the blocks processed…`],
  [/^el disco pertenece al pool '(.*)'$/, (p) => `the disk belongs to pool '${p}'`],
];

// Fields that hold names, paths and values: never translated.
const DATA_KEYS = new Set([
  'pool', 'dataset', 'dev', 'path', 'mountpoint', 'value', 'target', 'serial',
  'model', 'full', 'origin', 'vdev', 'command', 'user', 'snapshot', 'original',
  'trashed', 'by_id', 'group', 'error', 'code', 'status', 'state', 'kind', 'level',
]);

export function demoEnglish(s: string): string {
  const exact = EXACT[s];
  if (exact !== undefined) return exact;
  for (const [re, fn] of PATTERNS) {
    const m = re.exec(s);
    if (m) return fn(...m.slice(1));
  }
  return s;
}

function deep<T>(v: T, key = ''): T {
  if (typeof v === 'string') return (DATA_KEYS.has(key) ? v : demoEnglish(v)) as T;
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
  return getLang() === 'en' ? deep(v) : v;
}

function translateError(e: unknown): unknown {
  if (getLang() === 'en' && e instanceof ApiError) {
    return new ApiError(e.status, e.code, demoEnglish(e.message));
  }
  return e;
}

// wrapDemoProvider — every method of the demo provider answers, and fails,
// in the UI's language. Results are copied, never changed in place: the mock
// keeps its own state in Spanish.
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
