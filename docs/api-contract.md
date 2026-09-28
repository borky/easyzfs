# EasyZFS — Contrato API (front ↔ back)

Base: `/api`. Auth: cookie de sesión HttpOnly (`easyzfs_session`), login devuelve el usuario.
Todas las respuestas de error: `{"error":"código","message":"texto legible"}` con HTTP 4xx/5xx.
Acciones destructivas exigen `{"confirm":"<nombre exacto del objetivo>"}` en el body → si falta o no coincide: 400 `confirm_required`.

**Modo demo**: es una sesión mock 100% en cliente (botón "Entrar en modo demo" en el login, sin llamada al backend). Las credenciales reales autenticadas muestran SIEMPRE datos reales: no existe fallback a mock. Opcionalmente, el backend acepta `DEMO=1` para desplegar una instancia pública de demostración completa (colectores mock + mutaciones 403 `{"error":"demo_mode"}`), pero es un modo de despliegue, no de sesión.
Números: bytes en enteros (el front formatea a TiB/GiB con coma es-ES). Fechas: RFC3339 UTC.

**Reautenticación** (acciones irreversibles): crear/exportar/destruir pool, añadir vdev, offline/online/detach de vdev, replace, expand, checkpoint, borrar dataset o snapshot, rollback, change-key, crear o borrar usuario, crear API key, resetear la contraseña o el 2FA de un usuario, importar backup, cambiar `volsize` (reducirlo destruye datos), y crear/editar un job de replicación con `force_full:true`. Además del `confirm`, el body lleva `reauth_password` (y `reauth_code`, el TOTP, si el usuario tiene 2FA; los recovery codes no valen aquí). Respuestas, siempre 403 y nunca 401 (un 401 cerraría la sesión en el front): `reauth_required` (falta la contraseña), `reauth_code_required` (falta el código), `reauth_failed` (incorrecto). Los fallos cuentan para el limitador del login (429 `rate_limited`); los aciertos no. `POST /api/backup/import` no es JSON: lleva la respuesta en las cabeceras `X-Reauth-Password` / `X-Reauth-Code`, codificadas con `encodeURIComponent`, y sin ellas el servidor responde `reauth_required` sin leer el fichero.

**Almacenamiento del host** (§19.6/§21 del análisis): se lee del sistema en vivo, nunca de lo que EasyZFS haya creado (en Proxmox los pools existen antes de instalarlo). `GET /api/pools` añade a cada pool `host` (`"system"`: un dataset suyo está montado en `/` o `/boot`; `"storage"`: contiene almacenamiento de Proxmox o discos de VM/CT, o no se pudo leer `storage.cfg` en un host Proxmox) y `host_reason`. `GET /api/datasets` añade `host` (`"system"`: la raíz de un pool del sistema, su contenedor de arranque `…/ROOT` y lo que cuelga, lo montado fuera del árbol del pool como `/var/lib/vz` y sus antecesores; `"guest"`: un volumen que referencia la configuración de una VM o CT de cualquier nodo (`/etc/pve/nodes/*/{qemu-server,lxc}/*.conf`, incluidas secciones de snapshot y discos `unused`; `host_reason` nombra la VM/CT), o un nombre de volumen de Proxmox `(vm|base|subvol|basevol)-<id>-…`; `"storage"`: un `zfspool` de `/etc/pve/storage.cfg`, el dataset montado en la ruta de un almacenamiento `dir`, un dataset que contiene discos de VM/CT, o —si `storage.cfg` no se puede leer— cualquier dataset de primer nivel) y `host_reason`. Reglas, que las acciones comprueban leyendo el host otra vez: pool `system` → sin exportar/destruir, añadir, sustituir, offline/detach, expandir ni crear o descartar checkpoints (scrub, trim, autotrim, clear, SMART y snapshots sí); pool `storage` → sin exportar/destruir (la gestión de discos sigue disponible); dataset `system` o `guest` → sin borrar, papelera, renombrar, promover, desmontar, rewrite, cambiar propiedades, bloquear o cambiar la clave, ni rollback; `storage` → sin borrar, renombrar, promover, desmontar, rewrite, bloquear ni cambiar la clave, y solo admite `compression`, `atime`, `relatime`, `recordsize`, `primarycache`, `secondarycache`, `logbias` y `snapdir` (el resto lo heredan sus VMs y contenedores); `guest` → sin crear ni borrar snapshots; un snapshot recursivo manual que incluiría discos `guest` se rechaza, y el de una tarea programada los deja fuera (un único `zfs snapshot a@x b@x …`); crear, clonar o renombrar no admite un nombre de disco de Proxmox ni un destino dentro de almacenamiento `system`; un destino local de replicación no puede ser del host. Rechazo → 403 `host_storage` con el motivo. Si el host no se puede leer → 409 `host_unknown` y no se hace nada (para trabajar con los discos de un pool basta con leer los montajes).

**Modo solo lectura** (`EASYZFS_READONLY=1`, `install.sh --read-only`): toda mutación de almacenamiento → 403 `read_only`. `GET /api/version` lo indica en `read_only`.

## Auth y sesión
- `POST /api/login` `{user, password}` → `{user:"admin", role:"admin"}` + cookie. 401 si credenciales mal. 429 `rate_limited` si se supera el límite (5 intentos/min por IP+usuario; bloqueo 15 min tras 10 fallos consecutivos), o con `Retry-After: 1` si ya hay 32 logins en curso o esperando. Un `user` de más de 64 bytes (ninguna cuenta puede tenerlo) → 401 sin crear estado.
- `POST /api/logout` → 204. Invalida la sesión.
- `GET /api/me` → `{user, role}` o 401.
- `POST /api/me/password` `{current, new}` → 204. Cierra el resto de sesiones del usuario. `current` incorrecta → 403 `bad_credentials` (no 401, que cerraría la sesión en el front); los fallos cuentan para el limitador del login (429 `rate_limited`).

## 2FA (TOTP) del propio usuario (#84)
- `GET /api/me/2fa` → `{enabled:bool, recovery_remaining:int}`. Solo lectura: **nunca** regenera nada. Los recovery codes se guardan hasheados, así que no pueden volver a mostrarse; `recovery_remaining` es cuántos quedan sin gastar.
- `POST /api/me/2fa/setup` → `{secret, otpauth, qr}`. Genera un secreto nuevo provisional (invalida cualquier setup sin confirmar).
- `POST /api/me/2fa/confirm` `{code}` → `{codes:[…10]}`. Activa 2FA y entrega los recovery codes (única vez que se ven).
- `POST /api/me/2fa/disable` `{code}` → 204.
- `POST /api/me/2fa/recovery` → `{codes:[…10]}`. **Regenera**: borra los códigos anteriores y emite diez nuevos. Es POST precisamente porque muta: como GET quedaba fuera de los guards de CSRF/rate/demo y una navegación cross-site bastaba para dejar al usuario sin códigos.

## Usuarios (solo admin)
- `GET /api/users` → `[{user, role:"admin"|"user", last_login, sessions}]`
- `POST /api/users` `{user, password, role}` → 201. 409 si existe.
- `DELETE /api/users/{name}` `{confirm}` → 204. No puede borrarse a sí mismo ni al último admin.
- `POST /api/users/{name}/password` `{new, close_sessions?}` → 204 (admin). `close_sessions` por defecto `true` si no se envía.

## Sistema
- `GET /api/version` → `{name:"EasyZFS", version, build, go, os_arch, uptime_sec, rss_bytes, db_bytes, db_path, zfs_version, demo, capabilities:{rewrite, raidz_expansion, scrub_all, scrub_range, zarc_names, json_output, version}, read_only, update_channel, host_storage?}`. `host_storage` (ausente hasta la primera lectura del host): `{pve, storage_cfg_unreadable, os_pools:[…]}`; con `storage_cfg_unreadable:true` (Proxmox sin poder leer `/etc/pve/storage.cfg`, casi siempre por falta de la regla de sudoers) todos los pools y datasets de primer nivel se tratan como almacenamiento de Proxmox, y la UI muestra un aviso con lo que implica y cómo arreglarlo. `update_channel`: `"local"` (build del checkout: sin updater ni rutas `/api/update/*`) o `"github"`.
- `GET /api/update/status` (solo canal `github`) → `{current, latest, available, inProgress?, progress?, restartConfigured, releaseNotes?, releaseUrl?, applyRefused?}`. `applyRefused`: why the root helper refused the last staged update (no trusted signing key, missing or bad `checksums.txt.minisig`, checksum mismatch…); the binary in use is then still the previous one. Cleared by a successful install or a new `POST /api/update/apply`.
  - `capabilities` — feature-gating derivado de la versión de OpenZFS del host (sondeo al arranque y cada hora): `rewrite` (zfs rewrite, Linux ≥ 2.3.4), `raidz_expansion` (≥ 2.3), `scrub_all`/`scrub_range` (scrub -a y -S/-E, ≥ 2.4), `zarc_names` (zarcsummary/zarcstat, ≥ 2.4; si no, arc_summary/arcstat), `json_output` (--json, ≥ 2.3).
- `GET /api/performance` → `{arc:{size_bytes, hit_pct}|null, pools:[{name, read_bps, write_bps}]}`
  - Caché del colector `perf` (tick 60 s). ARC de `/proc/spl/kstat/zfs/arcstats` (respaldo: zarcsummary/arc_summary); `arc:null` = sin fuente en el sistema (la UI oculta la tarjeta). `read_bps`/`write_bps` = bytes/s de `zpool iostat -Hpy 1 1` (muestra de 1 s).
- `GET /api/settings` → `{lang:"auto"|"es"|"en", cap_warn_pct, cap_crit_pct, disk_temp_c, webhook, notify_scrub_errors, notify_smart_change}`
  - `webhook` llega vacío para quien no sea admin (usuario normal o API key de solo lectura): suele ser una URL con el secreto dentro.
- `PUT /api/settings` (admin) mismo body → 204. 400 `invalid_input` si `cap_warn_pct`/`cap_crit_pct` fuera de 1-100, `warn >= crit` o `disk_temp_c` fuera de 20-90.
- `GET /api/alerts` → `[{id, ts, level:"info"|"warn"|"crit", source, target, message, acked}]`
  - `target` — destino navegable en la UI según la fuente de la alerta: `"pools:<pool>"` (capacidad, DEGRADED/FAULTED, scrub con errores), `"disks:<dev>"` (temperatura, SMART), `"tasks"`, `"settings"`; `""` = sin destino.
- `POST /api/alerts/{id}/ack` → 204.

## Tareas del sistema (cron + systemd timers, solo lectura)
- `GET /api/system-timers` → `{timers:[{source:"systemd"|"cron", name, schedule, next_run, last_run, command, origin}], systemd_available:bool}`
  - Lo que YA existe en el sistema (colector `schedsys`, refresco 5 min; sin systemd/cron devuelve `[]`, nunca error). `systemd_available` = systemctl en PATH + `/run/systemd/system` (la UI oculta el cambio cron→systemd si es false; `POST /api/system-timers/migrate` responde 400 `systemd_unavailable`).
  - systemd: `name` = unidad `.timer`, `command` = unidad activada, `next_run`/`last_run` tal como los devuelve `systemctl list-timers`, `schedule` = `""` (list-timers no expone OnCalendar), `origin` = `"systemctl list-timers"`.
  - cron: `schedule` = expresión cron (`"30 3 * * *"`) o `"@daily"`/`"@hourly"`/… (entradas de `/etc/cron.{hourly,daily,…}`), `command` = comando, `next_run`/`last_run` = `""`, `origin` = `"crontab"`, `"crontab (root)"`, `"/etc/crontab"`, `"/etc/cron.d/<fichero>"`, `"/etc/cron.daily"`…

## Overview (dashboard)
- `GET /api/overview` → `{pools_total, pools_online, cap_used_bytes, cap_total_bytes, snapshots_total, jobs_active, last_scrub:{pool, ts, errors}, alerts:[…últimas 3], activity:[{ts, text, detail}…últimas 10]}`

## Pools
- `GET /api/pools` → `[{name, status:"ONLINE"|"DEGRADED"|"FAULTED", topo, used_bytes, total_bytes, frag_pct, comp_ratio, autotrim, checkpoint, scrub:{state:"none"|"running"|"done", kind:"scrub"|"resilver"|"trim", pct, eta_sec, ts, errors, bytes_done?, bytes_total?}, vdevs:[{dev, role, status, temp_c}]}]`
  - `scrub` (lote B) unifica scrub/resilver/**trim**: el progreso de trim se obtiene con `zpool status -t` cada tick (un scrub/resilver en curso tiene prioridad en la representación unificada). `bytes_done`/`bytes_total` son mejor esfuerzo (`scan_stats.examined/to_examine` en `--json`; `scanned`/`issued`/`trimmed` en texto). Cadencia = tick del colector (~30 s): la barra de progreso de la UI salta en cada tick (aceptable, documentado).
  - Cada vdev hoja lleva `group`: el vdev redundante al que pertenece (`"mirror-0"`, `"raidz2-1"`), ausente para un disco sin redundancia propia.
  - `autotrim` — propiedad autotrim del pool (TRIM continuo en SSD). `checkpoint` — el pool tiene un checkpoint activo.
- `POST /api/pools` `{name, topo:"stripe"|"mirror"|"raidz1"|"raidz2"|"raidz3", disks:[…], confirm}` → 202 (job). `confirm` = nombre del pool. El dataset raíz se monta en `/<name>` en cuanto el pool existe, así que un nombre que dé una ruta de sistema (`etc`, `usr`, `home`, `srv`, `var`…) → 400 `invalid_input`.
- `POST /api/pools/import` `{name?}` → sin `name`, 200 `{importable:[…]}`. Con `name`, importa y devuelve 202 `{warnings:[…]}`: el pool queda importado, y `warnings` lleva un texto por cada dataset que se ha quedado **sin montar**. La importación es `zpool import -N` (sin montar nada) seguida de un `zfs mount` por dataset que el pool montaría (`canmount=on`, mountpoint real, clave cargada) y que pasa la comprobación de punto de montaje efectivo (ver **Punto de montaje efectivo**): un pool no es un documento de confianza y puede traer `mountpoint=/etc` grabado.
- `POST /api/pools/{name}/scrub` `{action:"start"|"pause"|"stop"}` → 202
- `POST /api/pools/{name}/export` `{confirm, force, destroy}` → 202
- `POST /api/pools/{name}/vdev` `{topo, disks:[…], confirm, checkpoint?}` → 202 (añadir vdev). `confirm` = nombre del pool. Con `checkpoint:true` (opcional, `false` por defecto) se crea antes un checkpoint del pool para poder deshacer el añadido (`zpool import --rewind-to-checkpoint`); si ya hay uno → 409 `conflict` (volvería a otra fecha); si no se puede crear, no se añade nada. No es automático porque, mientras exista, ZFS rechaza `zpool replace` y la activación de hot spares.
- `POST /api/pools/{name}/vdev/action` (admin, reautenticación) `{dev, action:"offline"|"online"|"detach", confirm?, acknowledge_risk?}` → 202. `detach` exige `confirm` = nombre del pool. `offline`/`detach` con el pool no ONLINE, con un resilver/expansión o una sustitución en curso, o cuando dejaría su vdev sin redundancia (un mirror con un solo disco sano, un raidzN sin paridad; se decide por `group`) → 409 `risk_ack_required` con el motivo en `message`, hasta que la petición lleve `acknowledge_risk:true` (lo decide la caché del colector; ZFS sigue rechazando por su cuenta lo que perdería datos).
- `POST /api/pools/{name}/replace` `{old_dev, new_dev, confirm}` → 202. `confirm` = nombre del pool.
- `POST /api/pools/{name}/autotrim` (admin) `{enabled:bool}` → 204 (`zpool set autotrim=on|off`).
- `POST /api/pools/{name}/checkpoint` (admin) `{action:"create"|"discard", confirm}` → 202. `confirm` = nombre del pool (operación delicada: el checkpoint bloquea remove/attach/detach y retiene espacio; revertir con `zpool import --rewind-to-checkpoint`).
- `GET /api/pools/{name}/history` → `[{ts, command, duration_sec?}]` más reciente primero (últimas ~100 líneas de `zpool history -i`, cacheadas por el colector zpool).
- `POST /api/pools/{name}/expand` (admin) `{vdev:"raidz2-0", disk:"sdX", confirm:"<pool>"}` → 202. **RAID-Z expansion** (lote D; `zpool attach <pool> <vdev> <disco>`, OpenZFS ≥ 2.3): añade UN disco a un vdev raidz. Errores: 400 `not_supported` (sin capability `raidz_expansion`), 400 `confirm_required`, 400 `invalid_input` (vdev no raidz del pool), 404 `not_found` (pool), 409 `dev_in_use` (disco no libre o ya en un pool). Audit `pool.expand`.
  - `pools[].raidz_vdevs` (`["raidz2-0"…]`) — vdevs raidz detectados en `zpool status`; objetivo válido del endpoint. El progreso de la expansión aparece en `pools[].scrub` con `kind:"expand"` (pct/eta_sec/bytes_done) y en el SSE `scrub.progress {kind:"expand"}`.

## Datasets
- `GET /api/datasets` → `[{name, type:"fs"|"volume", compression, used_bytes, avail_bytes, quota_bytes, mountpoint, encryption, keystatus}]`
  - `encryption` (lote D) — valor efectivo de la propiedad (`"off"` sin cifrado; `"aes-256-gcm"`… cifrado, propio o heredado). `keystatus` — `"available"` (clave cargada, desbloqueado) | `"unavailable"` (bloqueado) | `"-"` (sin cifrado).
- `POST /api/datasets` `{pool, name, type:"fs"|"volume", compression:"lz4"|"zstd"|"off", quota_bytes, volsize_bytes?, encryption?, passphrase?}` → 201. Con `encryption:true` crea con cifrado nativo AES-256-GCM (`-o encryption=aes-256-gcm -o keyformat=passphrase -o keylocation=prompt`); la passphrase (mín. 8) viaja SOLO en el body y se pasa a zfs por stdin — jamás en URL, argv, logs ni audit_log.
- `PATCH /api/datasets/{name}` `{quota_bytes?, compression?}` → 204
- **Propiedades (U3, fase P1)**: tabla completa + edición con whitelist estricta.
  - `GET /api/datasets/{name}/properties` → `{name, properties:[{name, value, source}]}` (admin y viewer).
    - `source` ∈ `local`/`default`/`inherited`/`received`/`temporary`/`-`. Lista TODAS (nativas + user props); el front agrupa por editabilidad.
    - Lectura bajo demanda con caché TTL 30 s por dataset (excepción puntual a la caché de collectors, documentada en `docs/specs-p1-v2.5.md`). 404 `not_found`.
  - `PATCH /api/datasets/{name}/properties` (admin) `{property, value, acknowledge_risk?}` → 204.
    - `setuid=on` y `devices=on` → 400 `invalid_input`, y heredar `setuid`/`devices` tampoco se permite: una réplica recibida llega con ambos a `off` (lo que traiga el stream puede incluir ficheros setuid-root o de dispositivo) y EasyZFS no los vuelve a activar.
    - Valores de alto impacto (`sync=disabled`, `copies`, `readonly=on`, `canmount`, `mountpoint`, `exec`/`setuid`/`devices=off`, reducir `volsize`) → 409 `risk_ack_required` con la explicación en `message`, hasta que la petición lleve `acknowledge_risk:true`. `checksum` solo admite `on`, `fletcher4`, `sha256`.
    - Whitelist estricta en `internal/actions/props.go` (compression, recordsize, atime, relatime, sync, checksum, copies, xattr, acltype, aclinherit, primarycache, secondarycache, logbias, canmount, mountpoint, exec, setuid, devices, readonly, snapdir, quota, reservation, volsize, volblocksize). 400 `invalid_property` / 400 `invalid_value` (nunca llega a zfs). Propiedades no aplicables al tipo (mountpoint en volume…) → 400.
    - `mountpoint`: `none`, `legacy`, o una ruta absoluta simple **dentro de la lista permitida**: el árbol del propio pool (`/<pool>` y lo que cuelga, y también el mountpoint real del dataset raíz del pool si es otro, p. ej. `/data`; nunca `/`) o estrictamente por debajo de `/mnt`, `/media`, `/srv` o `/home`. Además, como segunda capa, se rechazan las rutas de sistema (`/etc`, `/usr`, `/root`, `/var/lib/dpkg`, el directorio de datos de EasyZFS, `/opt/easyzfs`…, y cualquier ruta que las contenga): un pool puede llamarse `etc`. Por último se recorre la ruta: ningún componente existente puede ser un enlace simbólico, y cada directorio por el que pasa debe ser de root y no escribible por grupo ni otros (si no, alguien podría cambiar el siguiente tramo por un enlace antes de que root monte). Un tramo que el servicio no pueda inspeccionar también se rechaza. Error → 400 `invalid_input` con el motivo. Esto cubre el valor que se **pide**; el que un dataset acaba teniendo sin que nadie lo pida va por la comprobación de **punto de montaje efectivo** (más abajo).
  - `POST /api/datasets/{name}/properties/{prop}/inherit` (admin) `{acknowledge_risk?}` (body opcional) → 204. Heredar una propiedad de alto impacto → 409 `risk_ack_required`, como arriba. Solo propiedades de la whitelist con `source == "local"`; 400 `invalid_property`, 409 `not_local`. Audit `dataset.setprop`/`dataset.inherit`. Con `prop=mountpoint` o `prop=canmount`, además, la comprobación de **punto de montaje efectivo** va **antes** del 409: heredar `mountpoint` le da al dataset la ruta del padre (y zfs lo remonta allí al momento), y heredar `canmount` lo deja en su valor por omisión, `on`. Si el resultado es una ruta de sistema → 400 `invalid_input`, sin pedir `acknowledge_risk`: no hay nada que aceptar.
- `DELETE /api/datasets/{name}` `{confirm, recursive, permanent?}` → 202. **Por defecto va a la papelera**: `zfs rename` a `<pool>/easyzfs-trash/<nombre>-<fecha>` (dataset con `mountpoint=none`, `canmount=off`), con los mountpoints locales del árbol puestos a `none` y `readonly=on`; se destruye a los 7 días. Sin `recursive`, un dataset con hijos → 400 `invalid_input` (como `zfs destroy`). No se puede con el dataset raíz del pool. Un dataset que hereda el cifrado de su padre no puede salir de su raíz de cifrado → 409 `conflict` (usar `permanent`). `permanent:true`, o un dataset que ya está en la papelera → `zfs destroy [-r]` inmediato.
- `PATCH /api/datasets/{name}/rename` (admin) `{new_name}` → 204 (`zfs rename`). Renombra el dataset y sus hijos.
- `POST /api/datasets/{name}/promote` (admin) → 204 (`zfs promote`): el clon pasa a ser el origen. Irreversible, no destructivo.
- `POST /api/datasets/{name}/mount` / `.../unmount` (admin) → 204 (`zfs mount` / `zfs unmount`, sin `-f`). `mount` es idempotente; `unmount` devuelve el error legible de zfs si el dataset está ocupado.
- **Punto de montaje efectivo** (§1 del análisis de seguridad): un dataset también obtiene un punto de montaje sin que nadie lo fije — heredado del padre, recibido en un stream, o derivado del nombre del pool — y eso es lo que ZFS monta, al crearlo y en cada import posterior. En la VM de pruebas de Proxmox (raíz en `rpool/ROOT/pve-1`, montada en `/`) crear un dataset cuyo mountpoint heredado caía en `/etc` dejó el host sin red tras reiniciar. Antes de que ZFS pueda montar, se resuelve la ruta real (`zfs get mountpoint`, sin sudo) y se aplican:
  - **siempre**, sea quien sea quien lo puso ahí: se rechaza una ruta de sistema (la misma lista que la propiedad `mountpoint`) y una ruta que no se pueda alcanzar con seguridad (ningún componente existente puede ser un enlace simbólico, y un tramo que no se pueda inspeccionar también se rechaza);
  - **además, si la ruta es derivada** (`source` = `default` o `inherited from …`): tiene que caer dentro del árbol del propio pool, de `/mnt`, `/media`, `/srv`, `/home`, o del mountpoint del ancestro más cercano que tenga uno grabado, y no puede ser `/mnt`, `/media`, `/srv` ni `/home` **exactamente** (montar ahí esconde todo lo que hay debajo, y un pool llamado `home` caería justo ahí). Colocar un dataset en un sitio nuevo es una decisión de la app, así que la app responde por ella. Un ancestro montado en `/` no da permiso para nada.
  - Una ruta **grabada en el dataset** (`source` = `local` o `received`) se salta esa última regla: es ya lo que ZFS va a montar, con EasyZFS o sin él, y negarse a ser quien lo dispare no la borraría. Por eso `rpool/var-lib-vz` (montado en `/var/lib/vz` por Proxmox) sigue funcionando.
  - `none`, `legacy` y los volúmenes no montan nada, así que no hay nada que comprobar. Si el valor no se puede **leer**, se rechaza: un desconocido no es un permiso, y "el dataset no existe" (lo único que hace subir al padre para resolver uno que aún no está creado) se distingue de cualquier otro fallo de lectura.
  - El recorrido de la ruta **no** exige aquí que cada directorio por el que pasa sea de root y no escribible por otros, a diferencia de un `mountpoint` que se pide explícitamente. El directorio de un recurso compartido suele estar `chown`-eado a sus usuarios (`chown -R nas:nas /tank/media`) o dejado escribible por el grupo, y exigirlo rechazaría cualquier dataset creado, importado o restaurado debajo de uno — rutas que antes no pasaban por ninguna comprobación.
  - **Límites que quedan:** para una ruta grabada en el dataset, la lista de rutas de sistema es toda la defensa, y una lista de denegación siempre se deja algo (`/tmp` se admite a propósito). Y sin la exigencia de dueño queda la carrera: quien pueda escribir en un directorio del camino puede cambiar el siguiente tramo por un enlace simbólico entre la comprobación y el montaje de root.
  - Pasan por aquí: crear un dataset, clonar sin `mountpoint`, renombrar, `mount`, promocionar (solo las reglas "siempre"), heredar `mountpoint`, importar un pool, crear un pool (`/<name>`), restaurar de la papelera y el destino local de una replicación. Error → 400 `invalid_input` con el motivo; en import y restore, un aviso por dataset y el resto sí se monta.
- **Papelera**:
  - `GET /api/trash` → `{items:[{id, pool, original, trashed, trashed_at, purge_at, actor, used_bytes|null}], days}`. `used_bytes` sale de la caché de datasets (`null` si aún no lo conoce).
  - `POST /api/trash/{id}/restore` (admin) → 200 `{warnings:[…]}`. Renombra de vuelta, repone mountpoints y `readonly`, y monta el árbol. 409 `conflict` si ya existe un dataset con el nombre original; 404 `not_found`. `warnings` lleva lo que no ha vuelto tal cual, incluidos los datasets que se han quedado sin montar porque su punto de montaje efectivo no pasa las comprobaciones (el dataset ya está restaurado).
  - `DELETE /api/trash/{id}` (admin, reautenticación) `{confirm:"<nombre original>"}` → 204. `zfs destroy -r` del elemento; solo actúa dentro de `<pool>/easyzfs-trash`.
  - Las vistas ocultan `<pool>/easyzfs-trash` y sus snapshots de las listas normales.
- **Cifrado nativo** (lote D; admin; audit sin claves NUNCA):
  - `POST /api/datasets/{name}/unlock` `{key}` → 204 (`zfs load-key`, clave por stdin). 400 `invalid_input` (sin clave o dataset sin cifrar), 404 `not_found`. **No monta**: comprobado en la VM, el dataset queda `keystatus=available` y `mounted=no`; montarlo es el endpoint `mount`.
  - `POST /api/datasets/{name}/lock` → 204 (`zfs unload-key`, sin `-f`: si el dataset está ocupado se devuelve el error legible de zfs).
  - `POST /api/datasets/{name}/change-key` `{current_key, new_key}` → 204 (`zfs change-key -o keyformat=passphrase`; la nueva va por stdin dos veces). Con keyformat=passphrase y la clave cargada, zfs no pide la actual, así que `current_key` se verifica antes con un dry-run `zfs load-key -n -L prompt <raíz de cifrado>` (por stdin). Clave incorrecta → 403 `wrong_key` y no se toca nada (auditado como `dataset.change_key.denied`, sin claves). Cualquier otro fallo de la verificación también aborta el cambio, pero con el error real, no como `wrong_key`. `new_key` mín. 8. Pensado para datasets `keyformat=passphrase` (los únicos que crea la UI): con `keyformat=raw` la verificación falla y el cambio se rechaza (`hex` sí funciona).

## Snapshots
- `GET /api/snapshots?dataset=` → agrupado: `[{dataset, snaps:[{name, full, ts, used_bytes, kind:"auto"|"manual"}]}]`
- `POST /api/snapshots` `{dataset, name, recursive}` → 201
- `POST /api/snapshots/{full}/clone` (admin) `{target, mountpoint?}` → 201 `{name}`. `mountpoint` opcional, con la misma validación que la propiedad `mountpoint` (ver Datasets); fuera de lo permitido → 400 `invalid_input` con el motivo. Sin `mountpoint`, el clon no hereda el del origen sino el que le da su propia posición, y esa ruta pasa por la comprobación de **punto de montaje efectivo**. El clon se crea siempre con `setuid=off` y `devices=off` (los heredaría de su nuevo padre, no del origen); no se admite clonar un snapshot de un disco de VM/CT.
- `DELETE /api/snapshots/{full}` `{confirm}` → 204 (`full` = `tank/docs@snap`, URL-encoded)
- `POST /api/snapshots/{full}/rollback` `{confirm}` → 202

## Jobs (tareas programadas)
- `GET /api/jobs` → `[{id, tipo:"snapshot"|"scrub"|"trim"|"smart_short"|"smart_long", target, schedule, retention, enabled, last_run, last_result, next_run}]`
  - `schedule` formato propio: `hourly@:15`, `daily@06:00`, `weekly:sun@03:00`, `monthly:1@02:00`
- `POST /api/jobs` `{tipo, target, schedule, retention?}` → 201
- `PATCH /api/jobs/{id}` `{enabled?, schedule?, retention?}` → 204
- `DELETE /api/jobs/{id}` `{confirm}` → 204
- `POST /api/jobs/{id}/run` → 202
- `GET /api/jobs/history` → `[{ts, tipo, target, ok, detail}]`

## Discos
- `GET /api/disks` → `[{dev, model, serial, size_bytes, temp_c:number|null, smart:"ok"|"warn"|"crit"|"unknown", smart_detail, pool, hours, in_use, in_use_reason}]`
  - `in_use_reason`: por qué un disco que no está en ningún pool no está libre (montado, swap, LVM, LUKS, mdadm, Ceph, partición EFI o BIOS boot, etiqueta ZFS, sistema de ficheros, retenido por device-mapper). Caché de 15 s solo para la vista: crear pool, añadir vdev, replace, expand y apagar vuelven a leer el disco en el momento y responden 409 `dev_in_use` con el motivo.
  - Solo dispositivos físicos: whitelist `sd[a-z]+`, `hd[a-z]+`, `vd[a-z]+`, `xvd[a-z]+`, `nvmeNnM`, `mmcblkN`. Excluidos siempre: `zd*` (zvols), `loop*`, `ram*`, `dm-*`, `sr*`, `fd*`, `mmcblk*boot*`, `mmcblk*rpmb`.
  - `temp_c: null` = sin lectura (eMMC, USB sin SAT, smartctl no disponible); `null` no es lo mismo que `0`. El front muestra "—".
  - `smart:"unknown"` + `smart_detail:"no disponible"` cuando el disco no habla smartctl: no es un error.
- `POST /api/disks/{dev}/smart-test` `{type:"short"|"long"}` → 202
- **SMART drill-down (U1, fase P1)** — datos de la caché del colector (hasta 10 min de antigüedad; NUNCA smartctl bajo demanda).
  - `GET /api/disks/{dev}/smart` → `{dev, model, serial, smart, smart_detail, hours, attributes:[{id, name, value, worst, thresh, raw, when_failed}]}` (admin y viewer). 404 `not_found`. Discos `unknown` (sin smartctl: eMMC, USB sin SAT) → 200 con `attributes:[]`.
  - `GET /api/disks/{dev}/smart-log` → `{dev, selftests:[{type, status, lifetime_hours, percent}], error_log:{count, entries:[{error_type, detail}]}}`. Listas vacías si el disco no expone logs.
  - El detalle se parsea en la pasada de 10 min del colector del mismo `smartctl -j -a`; protocolo ATA/NVMe detectado automáticamente.

## Notificaciones push (Web Push)
Los endpoints de suscripción devuelven 503 `push_not_configured` si el servidor no tiene claves VAPID. El texto de las notificaciones se compone server-side (ES/EN) según el `lang` guardado en la suscripción.
- `GET /api/push/vapid-public-key` → `{publicKey}` para `pushManager.subscribe()`.
- `POST /api/push/subscribe` `{endpoint, keys:{p256dh, auth}, lang?, origin?}` → 204. Upsert por `endpoint` (re-suscripciones y rotaciones actualizan la fila y reasignan `user_id`).
  - `lang` — `"es"|"en"`; ausente/desconocido no pisa el guardado.
  - `origin` — `window.location.origin` del navegador (p. ej. `https://zfs.example.com`). El servidor compone con él `url` y `notification.navigate` ABSOLUTAS en el payload (Declarative Web Push las exige); vacío = fallback a relativa. El upsert lo actualiza.
  - 400 `invalid_endpoint` (no https://), 400 `invalid_keys` (p256dh debe ser base64url de 65 bytes y auth base64url de 16 bytes), 400 `invalid_origin` (no http(s)://).
- `DELETE /api/push/unsubscribe` `{endpoint}` → 204. Solo borra suscripciones del propio usuario.
- `GET /api/push/preferences` → `{preferences:[{tipo, enabled}]}`. Los 5 tipos del catálogo (`pool_capacity`, `pool_status`, `scrub_errors`, `disk_temp`, `smart_status`), siempre presentes; `enabled` por defecto `true` si no hay fila guardada.
- `PUT /api/push/preferences` `{tipo, enabled}` → 204. Upsert por `(usuario, tipo)`. 400 `invalid_tipo` si el tipo es desconocido.
- `GET /api/push/quiet-hours` → `{enabled, start, end, tz}`. `start`/`end` son `null` cuando `enabled:false`. `tz` fija de momento: `Europe/Madrid`.
- `PUT /api/push/quiet-hours` `{enabled, start, end}` → 204. `start`/`end`: hora local 0-23; la ventana puede cruzar medianoche (22 → 8). 400 `invalid_hours` si fuera de 0-23 o `start == end`.
  - Efecto en el envío: las críticas atraviesan el horario silencioso SIEMPRE; warn/info dentro de la ventana se encolan (`notification_queue`) y se entregan al terminar (ticker de 60 s).

## SSE (tiempo real)
- `GET /api/events` — stream `text/event-stream`, heartbeat `:ping` cada 25 s, cabecera `X-Accel-Buffering: no`.
  Eventos (`event:` / `data:` JSON):
  - `pool.status` `{name, status}` · `scrub.progress` `{pool, pct, eta_sec}`
  - `disk.temp` `{dev, temp_c}` · `alert.new` `{alert}` · `job.finished` `{id, ok, detail}`
  - `longop.update` `{op}` (lote B: ciclo de vida de operaciones largas)
  - `replication.finished` `{id, ok, detail}` (lote C: fin de una replicación)
  - `overview` (cambios agregados para refrescar KPIs)

## Operaciones largas (lote B; runner `internal/longops`)
Procesos desacoplados monitorizados (hoy `zfs rewrite`; el runner es genérico
para reusarlo en la replicación del lote C: `Start(tipo, target, cmd, args…)`
con contexto propio, NO el de la request). El registro es **solo memoria**
(decisión deliberada): si el daemon reinicia, las ops en curso quedan
huérfanas (el proceso sigue en el SO pero EasyZFS ya no las ve). Las
terminadas se conservan 1 h (TTL). Cada cambio de estado publica
`longop.update` `{op}` por SSE.

`LongOp = { id, type ("rewrite"|"replication"), target, pid, started, ended?, status ("running"|"done"|"error"|"canceled"), error?, lines[] }` — `lines` es un anillo con las últimas 50 líneas de salida combinada.

- `GET /api/longops` → `[LongOp]` (más reciente primero).
- `POST /api/longops/{id}/cancel` (admin) → 204 · 404 `not_found` · 409 `not_running`.
- `POST /api/datasets/{name}/rewrite` (admin) `{confirm:"<dataset>"}` → 202 `{op_id}`. Errores: 400 `not_supported` (sin capability `rewrite`, OpenZFS ≥ 2.3.4), 400 `confirm_required`, 400 `invalid_input` (dataset inexistente, no filesystem o no montado), 409 `already_running`. Lanza `zfs rewrite -r -x <mountpoint>` y registra `dataset.rewrite` en audit_log.

## Replicación ZFS send/recv (lote C; `internal/replication`)
- **Destino local**: se recibe con `zfs recv -s -u` endurecido (sistema de ficheros: `setuid`, `devices` y `exec` a `off`, sin montar, ignorando el `mountpoint`/`canmount`/`share*` del stream; zvol: `volmode=dev`) y queda marcado `easyzfs:replica=on`. Una recepción incremental solo entra en un destino con esa marca. El origen no puede ser un dataset del sistema operativo (`host:"system"`). Una réplica local queda sin montar; montarla pasa por la comprobación de punto de montaje.
Copia incremental de datasets, local o vía SSH. Tabla propia
`replication_jobs` (migración v12). Modelo: cada ejecución crea un snapshot
`ezrepl-YYYYMMDD-HHMMSS` en el origen; la primera vez envía completo y las
siguientes `zfs send -i <origen>#ezrepl-last`; tras el éxito el bookmark
`ezrepl-last` pasa al snapshot nuevo y se podan los snapshots `ezrepl-*`
(se conservan los 2 últimos). Si el incremental falla (divergencia): con
`force_full` se destruye el destino y se reintenta completo UNA vez; sin él,
`last_error` lo explica y no se toca el destino. La ejecución va por el runner
longops (`type:"replication"`, pipeline `bash -c 'set -o pipefail; zfs send -v
… | [ssh …] zfs recv -s …'`) y el resultado queda en el job (`last_run`,
`last_ok`, `last_error`, `last_bookmark`) y en `job_history` (tipo
`replication`). Planificación con el mismo formato `schedule` y tick de 30 s
que los jobs. **Todo** lo interpolado en el pipeline pasa whitelists estrictas
(dataset `[a-zA-Z0-9_.\-/]+`, usuario `[a-z_][a-z0-9_-]*`, host
`[a-zA-Z0-9.\-]+`, puerto 1-65535).

El `send` no lleva `-p` ni `-R`, así que el stream **no arrastra propiedades** y
el destino no puede heredar el punto de montaje del origen: comprobado en la VM,
un `zfs send | zfs recv` pelado deja el destino en su propio `default`. Lo que un
destino sí puede tener ya grabado es un `mountpoint` con `source=received`
puesto por el stream de otro (los nuestros no ponen ninguno), y eso es lo que
merece atajarse: con `dest_type:"local"` el destino pasa por la comprobación de
**punto de montaje efectivo** antes de empezar, así que un destino montado sobre
una ruta de sistema sale a la luz en vez de repetirse (400 `invalid_input`, en
`last_error`). Con `dest_type:"ssh"` la ruta cae en el otro host y desde aquí no
se puede comprobar.

`ReplicationJob = { id, source, dest_type ("local"|"ssh"), dest_dataset, host, user, port, raw, force_full, schedule, enabled, last_bookmark, last_run, last_ok, last_error, next_run? }`

- `GET /api/replication` → `[ReplicationJob]` (con `next_run` calculado).
- `POST /api/replication` (admin) `{source, dest_type, dest_dataset, host?, user?, port?, raw?, force_full?, schedule}` → 201 `{id}` · 400 `invalid_input` / `invalid_schedule`.
- `PATCH /api/replication/{id}` (admin) `{enabled?, schedule?, force_full?, raw?}` → 204 · 404 `not_found`.
- `DELETE /api/replication/{id}` (admin) `{confirm:"<source>"}` → 204 (solo la definición: no toca snapshots, bookmark ni destino).
- `POST /api/replication/{id}/run` (admin) → 202 · 404 `not_found` · 409 `already_running` (una ejecución por job).
- `GET /api/replication/sshkey` → `{public_key, instructions}`: clave pública ed25519 del daemon (se genera al primer uso en `<datadir>/ssh/id_ed25519`, 0600; la privada jamás sale del servidor). Las conexiones usan `BatchMode=yes`, `StrictHostKeyChecking=accept-new` y `UserKnownHostsFile=<datadir>/ssh/known_hosts`.
- `POST /api/replication/test` (admin) `{host, user, port}` → 200 `{ok:true, remote_version}` (salida de `zfs version` remoto) o 200 `{ok:false, error}` con mensaje legible clasificado (autenticación / red / zfs ausente).

Evento SSE nuevo: `replication.finished` `{id, ok, detail}` (además de
`job.finished` para refrescar el historial). En MOCK=1: los jobs locales
completan con progreso simulado y los SSH fallan con error de autenticación
legible.

## Alertas en tiempo real (eventos ZFS; lote B)
Colector `events`: proceso persistente `zpool events -f` (sin timeout;
reconexión con backoff 5 s→2 min; si no está disponible —permisos, zfs
ausente— se desactiva solo tras log y el polling queda como red de seguridad).
Mapea bloques a alertas con `source = "zed.<class>"` (distinto del polling,
para que el dedupe source+message no colisione):

- `ereport.fs.zfs.io|checksum|data` → **crit**, target `disks:<dev>` (o `pools:<pool>`)
- `ereport.fs.zfs.deadman|delay` → **warn**
- `sysevent.fs.zfs.resilver_start|resilver_finish` → **info**
- `sysevent.fs.zfs.scrub_finish` con errores > 0 → **warn**
- `sysevent.fs.zfs.vdev_statechange` a FAULTED/DEGRADED → **crit**
- `config_sync`, `trim_start/finish`, `scrub_start` → sin alerta (solo log debug)

Kinds nuevos en el catálogo push (ES/EN): `zfs_io_error`, `zfs_checksum_error`,
`zfs_data_error`, `zfs_deadman`, `zfs_io_delay`, `vdev_state`,
`resilver_start`, `resilver_finish`.
