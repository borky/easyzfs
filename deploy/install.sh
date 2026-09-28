#!/usr/bin/env bash
# =============================================================================
# install.sh — Instalador interactivo de EasyZFS (estilo ProxMenux)
#
# Despliega el binario Go de gestión ZFS en cualquier servidor Linux con
# systemd: detecta la distro, instala ZFS + smartmontools, crea la cuenta de
# servicio (o modo root), escribe /etc/easyzfs/env y la unit de systemd.
#
# Uso:
#   bash install.sh [opciones]
#   curl -fsSL https://raw.githubusercontent.com/gnacho/easyzfs/main/deploy/install.sh | bash
#   DRY_RUN=1 bash install.sh --binary ./easyzfs --yes   # ensayo sin cambios
#
# Opciones:
#   --binary <ruta>   Binario local (defecto, desde un checkout: el ./easyzfs
#                     que deja 'make build'; sin él, se para y pide make install)
#   --url <url>       URL de release; acepta {arch} (x86_64|aarch64).
#                     También vía EASYZFS_RELEASE_URL. Solo se descarga si se
#                     indica, salvo con 'curl | bash' (sin checkout), que usa
#                     las releases oficiales de github.com/gnacho/easyzfs.
#   --source <dir>    Compila desde el repo fuente (requiere go y make;
#                     node/npm solo si existe web/ en el fuente)
#   --listen <addr>   Where the web UI listens: 127.0.0.1 (default with --yes), a local IPv4, or all
#   --port <n>        Puerto de escucha (defecto: 8080)
#   --root-mode       El servicio corre como root (sin usuario easyzfs/sudoers)
#   --uninstall       Desinstala unit, binario y sudoers (pregunta por datos)
#   --update          Updates binary and helper from --binary/--source (make update)
#   --yes, -y         No interactivo: acepta todos los valores por defecto
#   --help, -h        Muestra la ayuda
#
# Entorno:
#   DRY_RUN=1              Imprime los comandos sin ejecutarlos (pruebas)
#   EASYZFS_RELEASE_URL     Equivalente a --url
#   NO_COLOR=1             Desactiva los colores
# =============================================================================
set -euo pipefail

# ---- Constantes de despliegue (rutas y nombres fijos de la app) ----
readonly APP="easyzfs"
readonly SCRIPT_VERSION="1.0.0"
readonly INSTALL_BIN="/usr/local/bin/easyzfs"
readonly SYSD_HELPER="/usr/local/libexec/easyzfs-sysd"
readonly APPLY_HELPER="/usr/local/libexec/easyzfs-apply-update"
readonly UNIT_PATH="/etc/systemd/system/easyzfs.service"
readonly SUDOERS_PATH="/etc/sudoers.d/easyzfs"
readonly ENV_DIR="/etc/easyzfs"
readonly ENV_FILE="${ENV_DIR}/env"
readonly DATA_DIR="/var/lib/easyzfs"
readonly SVC_USER="easyzfs"

# URL oficial de releases (one-liner curl|bash). {arch} = x86_64|aarch64.
readonly DEFAULT_RELEASE_URL="https://github.com/gnacho/easyzfs/releases/latest/download/easyzfs-linux-{arch}"

# ---- Opciones (flags / variables de entorno) ----
OPT_BINARY=""
# Empty unless given: a download is only ever the default for the
# curl | bash one-liner, never for a run from a checkout (select_binary_source).
OPT_URL="${EASYZFS_RELEASE_URL:-}"
OPT_SOURCE=""
DOWNLOADED_TAG=""   # release tag the binary was downloaded from (release_tag_of)
BIN_CHANNEL=""      # update channel of the installed binary: local | github
PROBE_HOST="127.0.0.1"   # where verify_service expects the HTTP API
OPT_UPDATE=0        # --update: replace binary and helper of an existing install
# Root-run units that apply upstream releases; never wanted for a local build.
readonly UPDATE_UNITS="easyzfs-update-weekly.timer easyzfs-update-weekly.service easyzfs-update.path easyzfs-update.service"
OPT_PORT="8080"
PORT_FROM_FLAG=0
LISTEN_HOST=""       # 127.0.0.1 | a local IPv4 | all — see choose_listen_host
LISTEN_FROM_FLAG=0
OPT_ROOT_MODE=0
OPT_DEMO=0
OPT_READONLY=0     # --read-only: monitoring only, see write_sudoers
OPT_UNPINNED=0     # --allow-unpinned-sudo: accept unrestricted zpool/zfs on sudo < 1.9.10
OPT_UNINSTALL=0
OPT_YES=0
DRY_RUN="${DRY_RUN:-0}"

# ---- Estado global rellenado por las funciones de detección ----
DISTRO_ID="unknown"
DISTRO_FAMILY="unknown"
DISTRO_PRETTY="desconocida"
ARCH="unknown"
USE_WHIPTAIL=0
SUDO=()
BIN_MODE=""
GENERATED_ADMIN=""

# ---- Colores (solo si stdout es un TTY; NO_COLOR los desactiva) ----
if [ -t 1 ] && [ -z "${NO_COLOR:-}" ]; then
  C_RESET=$'\033[0m'; C_BOLD=$'\033[1m'
  C_RED=$'\033[31m'; C_GREEN=$'\033[32m'; C_YELLOW=$'\033[33m'
  C_BLUE=$'\033[34m'; C_CYAN=$'\033[36m'; C_GRAY=$'\033[90m'
else
  C_RESET=""; C_BOLD=""; C_RED=""; C_GREEN=""; C_YELLOW=""
  C_BLUE=""; C_CYAN=""; C_GRAY=""
fi

# ---- Mensajes con símbolos (${C_*:-} para poder extraer funciones con sed) ----
info() { printf '%s\n' "${C_CYAN:-}»${C_RESET:-} $*"; }
ok()   { printf '%s\n' "${C_GREEN:-}✔${C_RESET:-} $*"; }
warn() { printf '%s\n' "${C_YELLOW:-}⚠${C_RESET:-} $*" >&2; }
err()  { printf '%s\n' "${C_RED:-}✖${C_RESET:-} $*" >&2; }
die()  { err "$*"; exit 1; }
step() { printf '\n%s\n' "${C_BOLD:-}${C_BLUE:-}══ $* ══${C_RESET:-}"; }

# ---- run: ejecuta (o imprime en DRY-RUN) un comando simple ----
run() {
  if [ "$DRY_RUN" = "1" ]; then
    printf '%s\n' "${C_GRAY:-}[DRY-RUN]${C_RESET:-} $*" >&2
    return 0
  fi
  "$@"
}

# ---- write_root_file RUTA MODO: escribe stdin como root con permisos MODO ----
write_root_file() {
  local path="$1" mode="$2"
  if [ "$DRY_RUN" = "1" ]; then
    cat > /dev/null # consume stdin
    printf '%s\n' "${C_GRAY:-}[DRY-RUN]${C_RESET:-} escribir ${path} (modo ${mode})" >&2
    return 0
  fi
  "${SUDO[@]}" install -m "$mode" /dev/stdin "$path"
}

# ---- Banner ASCII ----
banner() {
  printf '%s\n' "${C_BLUE:-}${C_BOLD:-}"
  cat <<'EOF'
███████╗ █████╗ ███████╗██╗   ██╗███████╗███████╗███████╗
██╔════╝██╔══██╗██╔════╝╚██╗ ██╔╝╚══███╔╝██╔════╝██╔════╝
█████╗  ███████║███████╗ ╚████╔╝   ███╔╝ █████╗  ███████╗
██╔══╝  ██╔══██║╚════██║  ╚██╔╝   ███╔╝  ██╔══╝  ╚════██║
███████╗██║  ██║███████║   ██║   ███████╗██║     ███████║
╚══════╝╚═╝  ╚═╝╚══════╝   ╚═╝   ╚══════╝╚═╝     ╚══════╝
EOF
  printf '%s\n' "${C_RESET:-}  Instalador de ${APP} v${SCRIPT_VERSION} — gestión ZFS para tu NAS"
  if [ "$DRY_RUN" = "1" ]; then
    printf '%s\n\n' "${C_YELLOW:-}  *** MODO DRY-RUN: no se aplicará ningún cambio real ***${C_RESET:-}"
  else
    printf '\n'
  fi
}

usage() {
  cat <<'EOF'
Instalador de EasyZFS — despliegue en cualquier servidor Linux con systemd.

Uso:
  bash install.sh [opciones]
  curl -fsSL <url>/install.sh | bash -s -- --yes
  DRY_RUN=1 bash install.sh --binary ./easyzfs --yes   # ensayo sin cambios

Opciones:
  --binary <ruta>   Binario local (defecto, desde un checkout: el ./easyzfs que
                    deja 'make build'; sin él, se para y pide 'make install')
  --url <url>       Solo se descarga si se indica (o con 'curl | bash', sin
                    checkout). URL de release; acepta {arch}. Si la URL
                    no apunta a un fichero, se asume <base>/easyzfs-linux-<arch>.
                    También vía EASYZFS_RELEASE_URL. Defecto: releases gnacho/easyzfs.
  --source <dir>    Compila desde el repo fuente (go + make; node/npm si hay web/)
                    Bajo sudo se rechaza (compilaría como root en el checkout):
                    usa 'make install' / 'make update'.
  --listen <dir>    Dónde escucha la web: 127.0.0.1 (defecto con --yes), una IPv4
                    de este equipo, o all (todas las interfaces)
  --port <n>        Puerto de escucha (defecto: 8080)
  --demo            Arranca en modo demo (DEMO=1: datos de muestra, mutaciones 403)
  --root-mode       El servicio corre como root (sin usuario easyzfs ni sudoers)
  --read-only       Solo monitorización con datos reales: la API rechaza todo
                    cambio de almacenamiento y sudoers solo permite lecturas
                    (smartctl, zpool events/history, zfs diff). Sin helper root
  --allow-unpinned-sudo
                    Con sudo < 1.9.10 (sin regex en sudoers) concede zpool/zfs
                    con cualquier argumento, equivalente a root. Sin esta opción
                    la instalación se detiene (o usa --read-only).
  --uninstall       Desinstala unit, binario y sudoers (pregunta por los datos)
  --update          Actualiza una instalación existente con --binary o --source:
                    cambia binario y helper y reinicia; no toca la config ni
                    los datos, y nunca descarga (lo usa 'make update')
  --yes, -y         No interactivo: todo por defecto
  --help, -h        Muestra esta ayuda

Entorno:
  DRY_RUN=1              Imprime los comandos sin ejecutarlos
  EASYZFS_RELEASE_URL     Equivalente a --url
  NO_COLOR=1             Desactiva los colores
EOF
}

# =============================================================================
# UI: whiptail si hay TTY + whiptail; si no, prompts de texto elegantes.
# En modo pipe (`bash <(curl ...)`) los prompts usan /dev/tty cuando existe.
# =============================================================================

setup_ui() {
  USE_WHIPTAIL=0
  if [ "$OPT_YES" = "0" ] && [ -t 0 ] && [ -t 1 ] \
     && [ "${TERM:-dumb}" != "dumb" ] && command -v whiptail >/dev/null 2>&1; then
    USE_WHIPTAIL=1
  fi
}

# tty_ok — ¿hay terminal de control usable? (-r/-w no bastan: ENXIO sin ctty)
tty_ok() { (exec 3<>/dev/tty) 2>/dev/null; }

# Imprime un prompt (%b: interpreta \n) en /dev/tty si existe; si no, en stderr.
_prompt_out() {
  if tty_ok; then
    printf '%b' "$*" > /dev/tty
  else
    printf '%b' "$*" >&2
  fi
}

# _read_line VAR [secreto:0|1] — lee de /dev/tty (modo pipe) o de stdin.
_read_line() {
  local __var="$1" __secret="${2:-0}" __val=""
  if tty_ok; then
    if [ "$__secret" = "1" ]; then
      IFS= read -rs __val < /dev/tty || true
      printf '\n' > /dev/tty
    else
      IFS= read -r __val < /dev/tty || true
    fi
  else
    if [ "$__secret" = "1" ]; then
      IFS= read -rs __val || true
      printf '\n' >&2
    else
      IFS= read -r __val || true
    fi
  fi
  printf -v "$__var" '%s' "$__val"
}

# confirm "pregunta" [defecto: 0=no, 1=sí] → devuelve 0 (sí) o 1 (no)
confirm() {
  local text="$1" def="${2:-0}" reply="" hint="s/N"
  # def is 1 for "yes" but a shell status of 0 means yes, so returning "$def"
  # directly inverted every answer: Enter at [s/N] said yes (and at the
  # uninstall prompt deleted DATA_DIR and ENV_DIR), and --yes did the same.
  # Only the text path had this; whiptail's own exit status is right.
  if [ "$OPT_YES" = "1" ]; then [ "$def" = "1" ]; return; fi
  if [ "$USE_WHIPTAIL" = "1" ]; then
    if [ "$def" = "1" ]; then
      whiptail --title "$APP" --yesno "$text" 10 68 --defaultyes
    else
      whiptail --title "$APP" --yesno "$text" 10 68
    fi
    return $?
  fi
  [ "$def" = "1" ] && hint="S/n"
  while true; do
    _prompt_out "${text} [${hint}] "
    _read_line reply
    case "${reply,,}" in
      "") [ "$def" = "1" ]; return ;;
      s|si|sí|y|yes) return 0 ;;
      n|no) return 1 ;;
      *) _prompt_out "Responde 's' o 'n'.\n" ;;
    esac
  done
}

# prompt VAR "texto" "defecto" — entrada de texto con valor por defecto.
prompt() {
  local __var="$1" text="$2" def="${3:-}" reply=""
  if [ "$OPT_YES" = "1" ]; then printf -v "$__var" '%s' "$def"; return 0; fi
  if [ "$USE_WHIPTAIL" = "1" ]; then
    reply=$(whiptail --title "$APP" --inputbox "$text" 10 68 "$def" 3>&1 1>&2 2>&3) || reply="$def"
    printf -v "$__var" '%s' "${reply:-$def}"
    return 0
  fi
  _prompt_out "${text} [${def}]: "
  _read_line reply
  printf -v "$__var" '%s' "${reply:-$def}"
}

# prompt_password VAR "texto" — entrada oculta (vacío = cancelar/generar).
prompt_password() {
  local __var="$1" text="$2" reply=""
  if [ "$OPT_YES" = "1" ]; then printf -v "$__var" '%s' ""; return 0; fi
  if [ "$USE_WHIPTAIL" = "1" ]; then
    reply=$(whiptail --title "$APP" --passwordbox "$text" 10 68 3>&1 1>&2 2>&3) || reply=""
  else
    _prompt_out "${text} (oculta; vacío = generar aleatoria): "
    _read_line reply 1
  fi
  printf -v "$__var" '%s' "$reply"
}

# menu VAR "título" "texto" tag1 desc1 tag2 desc2 ... → VAR=tag elegido (""=cancelar)
menu() {
  # OJO: la variable de resultado interna usa prefijo __ para no colisionar
  # (ámbito dinámico de bash) con el nombre que pida el llamador (p. ej. "choice").
  local __var="$1" title="$2" text="$3" __choice=""
  shift 3
  if [ "$USE_WHIPTAIL" = "1" ]; then
    __choice=$(whiptail --title "$title" --menu "$text" 17 72 7 "$@" 3>&1 1>&2 2>&3) || __choice=""
  else
    local i=1 n=""
    local tags=()
    _prompt_out "\n${C_BOLD:-}${text}${C_RESET:-}\n"
    while [ $# -ge 2 ]; do
      tags+=("$1")
      _prompt_out "  ${i}) $1 — $2\n"
      shift 2
      i=$((i + 1))
    done
    _prompt_out "Elige [1-${#tags[@]}] (vacío = cancelar): "
    _read_line n
    if [[ "$n" =~ ^[0-9]+$ ]] && [ "$n" -ge 1 ] && [ "$n" -le "${#tags[@]}" ]; then
      __choice="${tags[$((n - 1))]}"
    fi
  fi
  printf -v "$__var" '%s' "$__choice"
}

# =============================================================================
# Detección del sistema
# =============================================================================

# detect_distro — /etc/os-release → DISTRO_ID / DISTRO_FAMILY / DISTRO_PRETTY.
# Familias: debian | arch | rhel | suse | alpine | unknown.
detect_distro() {
  DISTRO_ID="unknown"; DISTRO_FAMILY="unknown"
  DISTRO_PRETTY="desconocida"
  if [ -r /etc/os-release ]; then
    local ID="" ID_LIKE="" PRETTY_NAME=""
    # shellcheck disable=SC1091  # /etc/os-release es un fichero de datos estándar
    . /etc/os-release
    DISTRO_ID="${ID:-unknown}"
    DISTRO_PRETTY="${PRETTY_NAME:-$DISTRO_ID}"
    local tokens=" ${DISTRO_ID} ${ID_LIKE:-} "
    case "$tokens" in
      *" debian "*|*" ubuntu "*) DISTRO_FAMILY="debian" ;;
      *" arch "*|*" manjaro "*|*" endeavouros "*) DISTRO_FAMILY="arch" ;;
      *" fedora "*|*" rhel "*|*" centos "*|*" almalinux "*|*" rocky "*|*" ol "*) DISTRO_FAMILY="rhel" ;;
      *" suse "*|*" opensuse "*|*" sled "*|*" sles "*) DISTRO_FAMILY="suse" ;;
      *" alpine "*) DISTRO_FAMILY="alpine" ;;
      *) DISTRO_FAMILY="unknown" ;;
    esac
  fi
}

# detect_arch — uname -m → ARCH (x86_64 | aarch64 | unknown) y GOARCH
# (amd64 | arm64). GOARCH es el naming nuevo de los assets (v3+, formato
# {cmd}_{goos}_{goarch}); ARCH el viejo (easyzfs-linux-{arch}).
detect_arch() {
  case "$(uname -m)" in
    x86_64|amd64) ARCH="x86_64"; GOARCH="amd64" ;;
    aarch64|arm64) ARCH="aarch64"; GOARCH="arm64" ;;
    *) ARCH="unknown"; GOARCH="unknown" ;;
  esac
}

# check_root — exige root o sudo; en DRY-RUN solo avisa y simula con 'sudo'.
check_root() {
  SUDO=()
  if [ "$(id -u)" -eq 0 ]; then
    ok "Ejecutando como root."
    return 0
  fi
  if [ "$DRY_RUN" = "1" ]; then
    warn "DRY-RUN sin root: los comandos se mostrarán prefijados con 'sudo'."
    SUDO=(sudo)
    return 0
  fi
  if command -v sudo >/dev/null 2>&1; then
    SUDO=(sudo)
    if sudo -v; then
      ok "Sin root; se usará sudo para los pasos privilegiados."
      return 0
    fi
  fi
  die "Se necesita root (o sudo con contraseña validable). Reejecuta como root o con sudo."
}

# check_systemd — requiere systemd en ejecución (PID 1). Alpine/OpenRC: no soportado.
check_systemd() {
  if [ -d /run/systemd/system ] && command -v systemctl >/dev/null 2>&1; then
    ok "systemd detectado y en ejecución."
    return 0
  fi
  if [ "$DISTRO_FAMILY" = "alpine" ]; then
    if [ "$DRY_RUN" = "1" ]; then
      warn "Alpine usa OpenRC: aún no soportado (DRY-RUN: continúo el ensayo)."
      return 0
    fi
    die "Alpine usa OpenRC y este instalador aún no soporta servicios OpenRC (solo systemd)."
  fi
  if [ "$DRY_RUN" = "1" ]; then
    warn "systemd no está activo en este entorno (DRY-RUN: continúo el ensayo)."
    return 0
  fi
  die "No se detectó systemd en ejecución (PID 1). Este instalador requiere systemd."
}

# check_resources — pre-flight de disco y RAM (bloquea solo si el disco es crítico).
check_resources() {
  local avail=""
  avail="$(df -Pm / 2>/dev/null | awk 'NR==2 {print $4}')"
  if [ -n "$avail" ]; then
    if [ "$avail" -lt 300 ]; then
      die "Espacio en disco insuficiente: ${avail} MB libres (mínimo 300 MB para ZFS + EasyZFS)."
    elif [ "$avail" -lt 600 ]; then
      warn "Poco espacio en disco: ${avail} MB libres (recomendado 600+ MB)."
    else
      ok "Espacio en disco: ${avail} MB libres."
    fi
  fi
  local mem=""
  mem="$(awk '/^MemAvailable:/ {print int($2/1024)}' /proc/meminfo 2>/dev/null)"
  if [ -n "$mem" ] && [ "$mem" -lt 512 ]; then
    warn "RAM disponible baja: ${mem} MB. ZFS rinde mejor con 512+ MB libres."
  fi
}

# port_in_use N — ¿hay algo escuchando en el puerto N? Si no hay herramienta
# de red (ss/netstat), se asume libre.
port_in_use() {
  local p="$1"
  if command -v ss >/dev/null 2>&1; then
    ss -tln 2>/dev/null | awk '{print $4}' | grep -qE "[:.]${p}\$"
  elif command -v netstat >/dev/null 2>&1; then
    netstat -tln 2>/dev/null | awk '{print $4}' | grep -qE "[:.]${p}\$"
  else
    return 1
  fi
}

# next_free_port BASE → imprime el primer puerto libre en (BASE, BASE+20].
next_free_port() {
  local p=$(( $1 + 1 )) end=$(( $1 + 21 ))
  while [ "$p" -le "$end" ]; do
    if ! port_in_use "$p"; then printf '%s' "$p"; return 0; fi
    p=$((p + 1))
  done
  return 1
}

# =============================================================================
# Dependencias: ZFS + herramientas, con mapeo de paquetes por familia
# =============================================================================

# is_pve — Proxmox VE, where ZFS, the kernel and their packages belong to
# Proxmox's own release cycle and must not be touched by this installer.
is_pve() { [ -d /etc/pve ] || command -v pveversion >/dev/null 2>&1; }

# deps_debian — installs only what is missing, and never upgrades what is
# already there. Checked on a Proxmox VE 8.4 VM: the previous unconditional
# 'apt-get install -y zfsutils-linux …' upgraded ZFS 2.2.7 → 2.2.10 (with
# zfs-initramfs, which rebuilds the boot image) as a partial upgrade while the
# kernel module stayed at 2.2.7, and 'apt-get update' exiting 100 on the
# subscription-only enterprise repositories aborted the installer outright.
deps_debian() {
  local missing=() pkg
  if ! zpool version >/dev/null 2>&1; then
    if is_pve; then
      die "ZFS no responde en este Proxmox ('zpool version' falló). Proxmox trae ZFS de serie y este instalador no toca sus paquetes: revisa el sistema antes de instalar EasyZFS."
    fi
    missing+=(zfsutils-linux)
  fi
  command -v smartctl >/dev/null 2>&1 || missing+=(smartmontools)
  # fuser: the recycle bin refuses a dataset something still uses. Proxmox
  # ships it; a minimal Debian may not. Read-only installs never need it.
  [ "$OPT_READONLY" = "1" ] || command -v fuser >/dev/null 2>&1 || missing+=(psmisc)
  command -v lsblk    >/dev/null 2>&1 || missing+=(util-linux)
  command -v curl     >/dev/null 2>&1 || missing+=(curl)
  [ -e /etc/ssl/certs/ca-certificates.crt ] || missing+=(ca-certificates)
  if [ "${#missing[@]}" -eq 0 ]; then
    ok "Dependencias ya presentes: no se instala ni se actualiza ningún paquete."
    return 0
  fi
  info "Faltan: ${missing[*]}. Se instalan solo esos, sin actualizar nada de lo ya instalado."
  apt_refresh
  run "${SUDO[@]}" env DEBIAN_FRONTEND=noninteractive apt-get install -y --no-upgrade "${missing[@]}" \
    || die "apt-get no pudo instalar: ${missing[*]}."
  # Kernel headers are only for building ZFS with DKMS, i.e. only when ZFS
  # itself had to be installed here; never on Proxmox, whose kernels ship ZFS.
  for pkg in "${missing[@]}"; do
    [ "$pkg" = "zfsutils-linux" ] || continue
    if ! run "${SUDO[@]}" env DEBIAN_FRONTEND=noninteractive apt-get install -y --no-upgrade \
        "linux-headers-$(uname -r)"; then
      warn "No se pudieron instalar los headers del kernel ($(uname -r)); normal en LXC/contenedores."
      warn "Si el módulo ZFS no carga, instala los headers correctos y ejecuta: dkms autoinstall"
    fi
  done
}

# apt_refresh — 'apt-get update', tolerating a failing repository: a Proxmox
# host without a subscription gets 401 from the enterprise repositories on
# every refresh, which is its normal state, not a reason to stop.
apt_refresh() {
  if ! run "${SUDO[@]}" apt-get update -qq; then
    warn "apt-get update devolvió error (¿repositorio enterprise sin suscripción?): se usan las listas de paquetes actuales."
  fi
}

# ensure_sudo — the service account elevates through sudo, which Proxmox VE
# does not ship. Installed only when missing and only for that mode; without
# it the sudoers file would be written and silently unusable.
ensure_sudo() {
  if command -v sudo >/dev/null 2>&1 && command -v visudo >/dev/null 2>&1; then
    return 0
  fi
  info "Falta 'sudo' (el servicio lo necesita para los comandos permitidos en sudoers): se instala solo ese paquete."
  case "$DISTRO_FAMILY" in
    debian) apt_refresh
            run "${SUDO[@]}" env DEBIAN_FRONTEND=noninteractive apt-get install -y --no-upgrade sudo ;;
    arch)   run "${SUDO[@]}" pacman -S --needed --noconfirm sudo ;;
    rhel)   run "${SUDO[@]}" dnf install -y sudo || run "${SUDO[@]}" yum install -y sudo ;;
    suse)   run "${SUDO[@]}" zypper --non-interactive install sudo ;;
    alpine) run "${SUDO[@]}" apk add --no-cache sudo ;;
    *)      false ;;
  esac || die "No se pudo instalar 'sudo'. Instálalo a mano, o usa --root-mode."
  if [ "$DRY_RUN" != "1" ] && ! command -v visudo >/dev/null 2>&1; then
    die "'sudo' instalado pero falta 'visudo': no se puede validar el fichero sudoers."
  fi
}

# preflight_report — what this host is, printed before anything changes, so
# the operator sees what the installer is about to run on. Read-only.
preflight_report() {
  step "Inventario del host (solo lectura)"
  if is_pve; then
    info "Proxmox VE: $(pveversion 2>/dev/null || echo 'detectado')"
  fi
  info "Kernel: $(uname -r)"
  info "ZFS: $(zfs version 2>/dev/null | tr '\n' ' ' || echo 'no disponible')"
  info "Arranque: $([ -d /sys/firmware/efi ] && echo UEFI || echo BIOS)$(secure_boot_state)"
  info "Sistema de ficheros raíz: $(findmnt -no SOURCE,FSTYPE / 2>/dev/null || echo '?')"
  info "Pools: $(zpool list -H -o name,health 2>/dev/null | tr '\t\n' ': ' || echo 'ninguno')"
  local esp
  esp="$(lsblk -no NAME,PARTTYPENAME 2>/dev/null | awk '/EFI System/ {print $1}' | tr -d '└├─│ ' | tr '\n' ' ')"
  [ -n "$esp" ] && info "Particiones ESP: ${esp}"
  if [ -e /etc/pve/corosync.conf ]; then
    info "Clúster Proxmox: sí (este nodo forma parte de un clúster)"
  elif is_pve; then
    info "Clúster Proxmox: no"
  fi
  if is_pve; then
    info "En Proxmox el instalador no instala, actualiza ni quita paquetes de ZFS o del kernel."
  fi
}

secure_boot_state() {
  local f
  f="$(ls /sys/firmware/efi/efivars/SecureBoot-* 2>/dev/null | head -1)"
  [ -n "$f" ] || return 0
  if [ "$(od -An -t u1 -j4 -N1 "$f" 2>/dev/null | tr -d ' ')" = "1" ]; then
    printf ', Secure Boot activo'
  else
    printf ', Secure Boot inactivo'
  fi
}

deps_arch() {
  info "Instalando paquetes con pacman…"
  if run "${SUDO[@]}" pacman -Sy --needed --noconfirm zfs-utils smartmontools util-linux curl; then
    return 0
  fi
  warn "pacman no pudo instalar 'zfs-utils': no está en los repos oficiales de Arch."
  cat >&2 <<'EOF'
  Opciones para ZFS en Arch/Manjaro/EndeavourOS:
    1) Repo [archzfs] — añade a /etc/pacman.conf:
         [archzfs]
         Server = https://archzfs.com/$repo/$arch
       Importa y firma la clave:
         pacman-key -r DDF7DB817396A49B2A2723F7403BD972F75D9D76
         pacman-key --lsign-key DDF7DB817396A49B2A2723F7403BD972F75D9D76
       Luego: pacman -Sy zfs-utils
    2) AUR (DKMS): yay -S zfs-dkms zfs-utils   (requiere base-devel y headers)
EOF
  confirm "¿Ya tienes zfs-utils instalado por otra vía y quieres continuar?" 0 \
    || die "Instala ZFS (archzfs o AUR) y reintenta."
}

deps_rhel() {
  local pm="dnf"
  command -v dnf >/dev/null 2>&1 || pm="yum"
  local dist=""
  dist="$(rpm --eval '%{dist}' 2>/dev/null || true)"
  local base="fedora"
  [ "$DISTRO_ID" = "fedora" ] || base="epel"
  if [ -n "$dist" ]; then
    local repo_url="https://zfsonlinux.org/${base}/zfs-release-2-3${dist}.noarch.rpm"
    info "Añadiendo el repo ZFS on Linux: ${repo_url}"
    if ! run "${SUDO[@]}" "$pm" install -y "$repo_url"; then
      warn "No se pudo instalar zfs-release (¿no hay build para ${DISTRO_PRETTY}?)."
      warn "Descarga el RPM correcto de https://zfsonlinux.org/ e instálalo a mano."
    fi
  else
    warn "No se pudo evaluar %%{dist}; instala el repo zfs-release a mano si falla 'zfs'."
  fi
  run "${SUDO[@]}" "$pm" install -y zfs smartmontools util-linux curl \
    || warn "La instalación de paquetes falló; revisa el repo ZFS on Linux."
  # kernel-devel para DKMS (solo aviso si no está)
  run "${SUDO[@]}" "$pm" install -y kernel-devel \
    || warn "kernel-devel no disponible: DKMS podría no compilar el módulo ZFS."
}

deps_suse() {
  info "Instalando paquetes con zypper…"
  if run "${SUDO[@]}" zypper --non-interactive install --no-recommends \
      zfs smartmontools util-linux curl; then
    return 0
  fi
  warn "zypper falló: 'zfs' puede requerir el repo 'filesystems' de openSUSE."
  cat >&2 <<'EOF'
  Añade el repo (sustituye <VERSION>, p. ej. openSUSE_Leap_15.6):
    zypper ar -f https://download.opensuse.org/repositories/filesystems/<VERSION>/ filesystems
    zypper ref && zypper install zfs
EOF
  confirm "¿Continuar asumiendo que ZFS ya está instalado?" 0 \
    || die "Instala ZFS (repo filesystems) y reintenta."
}

deps_alpine() {
  warn "Alpine: asegúrate de tener el repo 'community' habilitado en /etc/apk/repositories."
  run "${SUDO[@]}" apk add zfs smartmontools util-linux curl \
    || die "apk falló (¿está habilitado el repo community?)."
}

# verify_zfs_stack — modprobe + comprobación real de zpool y smartctl.
verify_zfs_stack() {
  info "Cargando el módulo ZFS y verificando herramientas…"
  run "${SUDO[@]}" modprobe zfs || true
  if [ "$DRY_RUN" = "1" ]; then
    info "[DRY-RUN] verificaría: zpool version && smartctl --version"
    return 0
  fi
  if ! zpool version >/dev/null 2>&1; then
    err "El módulo ZFS no está disponible ('zpool version' falló tras modprobe)."
    cat >&2 <<'EOF'
  Pistas:
    • DKMS sin headers del kernel: instala linux-headers / kernel-devel
      y ejecuta: dkms autoinstall && modprobe zfs
    • Secure Boot: un módulo sin firmar no carga; fírmalo con mokutil
      o desactiva Secure Boot en la UEFI
    • Contenedor LXC: el host debe cargar ZFS y pasar el módulo/dispositivos
EOF
    exit 1
  fi
  ok "ZFS operativo: $(zpool version 2>/dev/null | head -1)"
  if smartctl --version >/dev/null 2>&1; then
    ok "smartmontools: $(smartctl --version 2>/dev/null | head -1)"
  else
    warn "smartctl no disponible: las funciones SMART de EasyZFS no funcionarán."
  fi
}

install_dependencies() {
  step "Dependencias del sistema (${DISTRO_FAMILY})"
  case "$DISTRO_FAMILY" in
    debian) deps_debian ;;
    arch)   deps_arch ;;
    rhel)   deps_rhel ;;
    suse)   deps_suse ;;
    alpine) deps_alpine ;;
    *)
      warn "Distribución no reconocida (${DISTRO_PRETTY}): no hay mapeo automático de paquetes."
      warn "Instala manualmente: zfs (utils), smartmontools, util-linux, curl, ca-certificates."
      confirm "¿Continuar en modo manual (asumo dependencias ya instaladas)?" 0 \
        || die "Instalación cancelada. Instala las dependencias y reintenta."
      ;;
  esac
  verify_zfs_stack
}

# =============================================================================
# Origen e instalación del binario EasyZFS
# =============================================================================

# resolve_asset_url — sustituye {arch}; si la URL no apunta a un fichero,
# asume patrón de GitHub releases: <base>/easyzfs-linux-<arch>.
resolve_asset_url() {
  local url="$1"
  if [[ "$url" == *"{arch}"* ]]; then
    url="${url//\{arch\}/${ARCH}}"
  elif [[ "$url" != *.tar.gz && "$url" != */easyzfs* ]]; then
    url="${url%/}/easyzfs-linux-${ARCH}"
  fi
  printf '%s\n' "$url"
}

# script_real_dir — the directory of this script when it runs from a real
# file (a checkout); nothing under 'curl ... | bash', where BASH_SOURCE is
# empty and $0 is "bash", so dirname would give the current directory.
script_real_dir() {
  if [ -n "${BASH_SOURCE[0]:-}" ] && [ -f "${BASH_SOURCE[0]}" ]; then
    (cd "$(dirname "${BASH_SOURCE[0]}")" 2>/dev/null && pwd) || true
  fi
}

# select_binary_source — where the binary comes from, in this order:
# --binary, --source, an explicit --url (or EASYZFS_RELEASE_URL); then, run
# from a checkout, that checkout's own build (./easyzfs next to deploy/, as
# 'make build' leaves it). A checkout with nothing built stops and points at
# 'make install' instead of downloading: this fork installs from its checkout
# only. The download of upstream's latest release remains the default solely
# for the 'curl | bash' one-liner, which has no checkout.
#
# There used to be a menu and a ./easyzfs lookup in the current directory, but
# OPT_URL always carried a default and was checked first, so a run from the
# checkout without --binary downloaded upstream's release and re-enabled its
# auto-update. The current-directory lookup would also have installed any file
# called easyzfs found there, as root.
select_binary_source() {
  if [ -n "$OPT_BINARY" ]; then BIN_MODE="local"; return 0; fi
  if [ -n "$OPT_SOURCE" ]; then BIN_MODE="build"; return 0; fi
  if [ -n "$OPT_URL" ];    then BIN_MODE="download"; return 0; fi
  local dir; dir="$(script_real_dir)"
  if [ -n "$dir" ]; then
    if [ -x "${dir}/../easyzfs" ]; then
      OPT_BINARY="$(cd "${dir}/.." && pwd)/easyzfs"; BIN_MODE="local"
      info "Binario del checkout: ${OPT_BINARY}"
      return 0
    fi
    die "No hay binario compilado en el checkout ($(cd "${dir}/.." && pwd)/easyzfs). Usa 'make install': compila como tu usuario e instala."
  fi
  OPT_URL="$DEFAULT_RELEASE_URL"; BIN_MODE="download"
}

install_binary() {
  step "Instalación del binario (${BIN_MODE})"
  case "$BIN_MODE" in
    local)
      if [ ! -f "$OPT_BINARY" ] && [ "$DRY_RUN" != "1" ]; then
        die "No existe el binario: ${OPT_BINARY}"
      fi
      run "${SUDO[@]}" install -m 0755 "$OPT_BINARY" "$INSTALL_BIN"
      ;;
    download)
      [ "$ARCH" != "unknown" ] || die "Arquitectura '$(uname -m)' no soportada para descarga (x86_64/aarch64)."
      local url=""
      url="$(resolve_asset_url "$OPT_URL")"
      # Naming nuevo (v3+): easyzfs_linux_{GOARCH} + checksums.txt único.
      # Fallback al viejo (releases antiguas): easyzfs-linux-{ARCH} +
      # checksums-{ARCH}.txt. El asset es un binario plano (sin .tar.gz).
      local rel_base="${url%/*}"
      local want_url="" want_sums="" asset_name=""
      if [ -z "$OPT_URL" ] || [[ "$OPT_URL" == *"{arch}"* ]]; then
        # Probamos el naming nuevo primero.
        if curl -fsI --max-time 10 "${rel_base}/easyzfs_linux_${GOARCH}" >/dev/null 2>&1; then
          want_url="${rel_base}/easyzfs_linux_${GOARCH}"
          asset_name="easyzfs_linux_${GOARCH}"
          curl -fsSL --max-time 15 "${rel_base}/checksums.txt" -o /dev/null 2>/dev/null \
            && want_sums="${rel_base}/checksums.txt"
        else
          want_url="${rel_base}/easyzfs-linux-${ARCH}"
          asset_name="easyzfs-linux-${ARCH}"
          curl -fsSL --max-time 15 "${rel_base}/checksums-${ARCH}.txt" -o /dev/null 2>/dev/null \
            && want_sums="${rel_base}/checksums-${ARCH}.txt"
        fi
      else
        # URL explícita de asset: se usa tal cual (sin adivinar).
        want_url="$url"
        asset_name="$(basename "$url")"
        curl -fsSL --max-time 15 "${rel_base}/checksums.txt" -o /dev/null 2>/dev/null \
          && want_sums="${rel_base}/checksums.txt" \
          || curl -fsSL --max-time 15 "${rel_base}/checksums-${ARCH}.txt" -o /dev/null 2>/dev/null \
             && want_sums="${rel_base}/checksums-${ARCH}.txt"
      fi

      info "Descargando: ${want_url}"
      if [ "$DRY_RUN" = "1" ]; then
        info "[DRY-RUN] curl -fsSL '${want_url}' → ${INSTALL_BIN}"
        DOWNLOADED_TAG="$(release_tag_of "$want_url" || true)"
      else
        local tmp="" bin=""
        tmp="$(mktemp -d)"
        curl -fsSL "$want_url" -o "${tmp}/asset" || die "La descarga falló: ${want_url}"
        # Verificación sha256 contra el checksums (único o por arch).
        if [ -n "$want_sums" ]; then
          curl -fsSL "$want_sums" -o "${tmp}/checksums.txt" 2>/dev/null || rm -f "${tmp}/checksums.txt"
        fi
        if [ -s "${tmp}/checksums.txt" ]; then
          local want="" got=""
          want="$(grep " ${asset_name}\$" "${tmp}/checksums.txt" | awk '{print $1}' | head -1)"
          [ -n "$want" ] || want="$(grep " ${asset_name}" "${tmp}/checksums.txt" | awk '{print $1}' | head -1)"
          got="$(sha256sum "${tmp}/asset" | awk '{print $1}')"
          [ -n "$want" ] || die "checksums no lista ${asset_name} (¿release sin asset para esta arch?)."
          [ "$want" = "$got" ] || die "sha256 NO COINCIDE para ${asset_name} — descarga corrupta o manipulada."
          ok "sha256 verificado contra $(basename "$want_sums")."
        else
          warn "La release no publica checksums: descarga SIN verificar."
          # Default no. Under the old inverted confirm() a default of 1 already
          # meant no; now that it means yes, it would let Enter and --yes skip
          # the checksum.
          confirm "¿Continuar sin verificación de integridad?" 0 || die "Instalación cancelada por seguridad."
        fi
        bin="${tmp}/asset"
        # Si el asset es un .tar.gz (releases antiguas comprimidas), se extrae.
        if file "${tmp}/asset" 2>/dev/null | grep -qi 'gzip compressed'; then
          tar -xzf "${tmp}/asset" -C "$tmp" || die "No se pudo descomprimir el asset."
          bin="$(find "$tmp" -type f -name easyzfs -print -quit)"
          [ -n "$bin" ] || die "El asset descargado no contiene un binario 'easyzfs'."
        fi
        "${SUDO[@]}" install -m 0755 "$bin" "$INSTALL_BIN"
        rm -rf "$tmp"
        # The root helper is fetched from this same tag, so it matches the binary.
        DOWNLOADED_TAG="$(release_tag_of "$want_url" || true)"
      fi
      ;;
    build)
      # Under sudo this would run npm ci and go build as root inside the user's
      # checkout, leaving root-owned node_modules/, dist/ and easyzfs behind that
      # break the next build as the user. 'make install' / 'make update' build as
      # the user and only install as root.
      if [ "$(id -u)" = "0" ] && [ -n "${SUDO_USER:-}" ] && [ "$SUDO_USER" != "root" ]; then
        die "--source bajo sudo compilaría como root en tu checkout. Usa 'make install' o 'make update' (sin sudo), o ejecuta el instalador sin sudo."
      fi
      [ -d "$OPT_SOURCE" ] || die "No existe el directorio fuente: ${OPT_SOURCE}"
      if [ "$DRY_RUN" != "1" ]; then
        command -v go   >/dev/null 2>&1 || die "Falta 'go' para compilar (instálalo o usa --binary/--url)."
        command -v make >/dev/null 2>&1 || die "Falta 'make' para compilar."
        if [ -d "${OPT_SOURCE}/web" ] && ! command -v npm >/dev/null 2>&1; then
          die "Existe ${OPT_SOURCE}/web pero falta 'npm' para compilar el front (Node 20+)."
        fi
        info "Compilando con make (web + binario estático)…"
        make -C "$OPT_SOURCE" build || die "La compilación falló."
        [ -f "${OPT_SOURCE}/easyzfs" ] || die "make no produjo ${OPT_SOURCE}/easyzfs."
        "${SUDO[@]}" install -m 0755 "${OPT_SOURCE}/easyzfs" "$INSTALL_BIN"
      else
        info "[DRY-RUN] make -C ${OPT_SOURCE} build && install easyzfs → ${INSTALL_BIN}"
      fi
      ;;
    *)
      die "Modo de instalación del binario desconocido: ${BIN_MODE}"
      ;;
  esac
  ok "Binario instalado en ${INSTALL_BIN}"
}

# release_tag_of <url> — the release tag behind an asset URL of this project's
# GitHub releases, or nothing. A ".../releases/latest/download/..." URL is
# resolved through its first redirect, which names the concrete tag.
release_tag_of() {
  local u="$1" base="https://github.com/gnacho/easyzfs/releases/"
  case "$u" in "$base"*) ;; *) return 1 ;; esac
  case "$u" in
    "${base}latest/download/"*)
      # Headers first, then an awk that reads to the end: an early 'exit' can
      # SIGPIPE the writer, which set -o pipefail turns into a failure.
      local hdr
      hdr="$(curl -fsI --max-time 15 "$u" 2>/dev/null)" || return 1
      u="$(printf '%s\n' "$hdr" | tr -d '\r' \
        | awk 'tolower($1)=="location:" && !seen {print $2; seen=1}')"
      ;;
  esac
  case "$u" in *"/releases/download/"*) ;; *) return 1 ;; esac
  local t="${u#*/releases/download/}"; t="${t%%/*}"
  [[ "$t" =~ ^[A-Za-z0-9._-]+$ ]] || return 1
  printf '%s\n' "$t"
}

# install_sysd_helper — helper root confinado (edición/migración de tareas del
# sistema).
#
# Sources, in order: the checkout this script runs from, the --source tree, the
# directory holding --binary. Only then the network, and only from the release
# tag the binary itself was downloaded from, never from the moving main
# branch, so the helper always matches the binary. If no tag is known (a
# custom --url), the install stops rather than guess.
#
# The script's own directory only counts when the script is a real file. Under
# 'curl ... | bash' BASH_SOURCE is empty and $0 is "bash", so dirname gave the
# current directory: running the one-liner from a directory where someone else
# had left an 'easyzfs-sysd' installed their file as the root helper.
# local_deploy_file <name> — path of deploy/<name> next to this script (only
# when the script is a real file, see install_sysd_helper), in the --source
# tree, or next to --binary; nothing if none has it.
local_deploy_file() {
  local name="$1" script_dir
  script_dir="$(script_real_dir)"
  if [ -n "$script_dir" ] && [ -f "${script_dir}/${name}" ]; then
    printf '%s\n' "${script_dir}/${name}"
  elif [ -n "$OPT_SOURCE" ] && [ -f "${OPT_SOURCE}/deploy/${name}" ]; then
    printf '%s\n' "${OPT_SOURCE}/deploy/${name}"
  elif [ -n "$OPT_BINARY" ] && [ -f "$(dirname "$OPT_BINARY")/${name}" ]; then
    printf '%s\n' "$(dirname "$OPT_BINARY")/${name}"
  fi
}

install_sysd_helper() {
  step "Helper de tareas del sistema (easyzfs-sysd)"
  # Read-only installs get no root helper at all (it edits cron and systemd);
  # one left by an earlier full install is removed.
  if [ "$OPT_READONLY" = "1" ]; then
    if [ -e "$SYSD_HELPER" ]; then
      run "${SUDO[@]}" rm -f "$SYSD_HELPER"; ok "Helper eliminado (modo solo lectura): ${SYSD_HELPER}"
    else
      info "Modo solo lectura: no se instala el helper root."
    fi
    return 0
  fi
  run "${SUDO[@]}" mkdir -p "$(dirname "$SYSD_HELPER")" \
    || die "No se pudo crear $(dirname "$SYSD_HELPER")"
  local src; src="$(local_deploy_file easyzfs-sysd)"
  if [ -n "$src" ]; then
    info "Helper local: ${src}"
    run "${SUDO[@]}" install -m 0755 "$src" "$SYSD_HELPER" \
      || die "No se pudo instalar el helper en $SYSD_HELPER"
  elif [ -n "${DOWNLOADED_TAG:-}" ]; then
    local url="https://raw.githubusercontent.com/gnacho/easyzfs/${DOWNLOADED_TAG}/deploy/easyzfs-sysd"
    info "Descargando helper de la release ${DOWNLOADED_TAG}: $url"
    if [ "$DRY_RUN" = "1" ]; then
      info "[DRY-RUN] curl -fsSL '${url}' → ${SYSD_HELPER}"
    else
      local tmp; tmp="$(mktemp)"
      curl -fsSL "$url" -o "$tmp" || { rm -f "$tmp"; die "No se pudo descargar el helper."; }
      head -1 "$tmp" | grep -q '^#!/usr/bin/env bash' \
        || { rm -f "$tmp"; die "El helper descargado no es el script esperado."; }
      run "${SUDO[@]}" install -m 0755 "$tmp" "$SYSD_HELPER" \
        || { rm -f "$tmp"; die "No se pudo instalar el helper en $SYSD_HELPER"; }
      rm -f "$tmp"
    fi
  else
    die "No hay helper local y no se sabe de qué release vino el binario: ejecuta el instalador desde el repo (bash deploy/install.sh) o usa --source."
  fi
  ok "Helper instalado en ${SYSD_HELPER}"
}

# =============================================================================
# Cuenta de servicio y sudoers limitado
# =============================================================================

setup_user_and_sudoers() {
  step "Cuenta de servicio y privilegios"
  if [ "$OPT_ROOT_MODE" = "0" ] && [ "$OPT_YES" = "0" ]; then
    local choice=""
    menu choice "EasyZFS — privilegios" "¿Con qué usuario debe correr el servicio?" \
      easyzfs "Usuario de sistema 'easyzfs' + sudoers limitado (recomendado)" \
      root "root — administración completa sin sudoers (decisión consciente)"
    [ "$choice" = "root" ] && OPT_ROOT_MODE=1
  fi
  if [ "$OPT_ROOT_MODE" = "1" ]; then
    warn "Modo root: el servicio correrá como root (appliance de administración; sin sudoers)."
    return 0
  fi
  if id "$SVC_USER" >/dev/null 2>&1; then
    ok "El usuario de sistema '${SVC_USER}' ya existe."
  else
    run "${SUDO[@]}" useradd --system --shell /usr/sbin/nologin \
        --home-dir "$DATA_DIR" --comment "EasyZFS service" "$SVC_USER" \
      || run "${SUDO[@]}" useradd -r -s /usr/sbin/nologin -d "$DATA_DIR" "$SVC_USER" \
      || die "No se pudo crear el usuario de sistema '${SVC_USER}'."
    ok "Usuario de sistema '${SVC_USER}' creado."
  fi
  ensure_sudo
  write_sudoers
}

# sudo_has_regex — whole-argument regular expressions (^…$) in sudoers
# arrived in sudo 1.9.10; an older sudo rejects a file that uses them.
sudo_has_regex() {
  [ "$DRY_RUN" = "1" ] && return 0
  local v; v="$(sudo -V 2>/dev/null | awk 'NR==1 {print $3}')"
  [ -n "$v" ] && [ "$(printf '%s\n1.9.10\n' "${v%%p*}" | sort -V | head -1)" = "1.9.10" ]
}

# require_sudo_regex — the read-only file cannot be written any other way.
require_sudo_regex() {
  sudo_has_regex || die "El modo solo lectura necesita sudo ≥ 1.9.10 (hay: $(sudo -V 2>/dev/null | awk 'NR==1 {print $3}'))."
}

# pinned_sudoers — the full-mode grant, one line per command shape the
# service actually runs (internal/actions, internal/replication,
# internal/collectors), each a whole-argument regex. Granting zpool and zfs
# with any arguments made the service account root in all but name:
# 'zfs program' runs Lua in the kernel, 'zfs allow' delegates, 'zpool import
# -d <dir>' imports a crafted pool file, 'zpool create' takes file vdevs,
# '-o altroot' and 'zpool status -c' run or place what the caller chooses.
# None of those shapes appears below.
#
# What stays possible is what the app is for: a compromised service can still
# destroy datasets and pools and receive a stream (zfs recv) into a dataset
# whose mountpoint it can set; that is root-equivalent and inherent. The
# property names mirror propValidators in internal/actions/props.go, which
# internal/actions/sudoers_test.go checks. In sudoers, ',', ':', '=' and '\'
# in arguments are escaped with '\'.
pinned_sudoers() {
  local zpool="$1" zfs="$2" smartctl="$3" lsblk="$4" crontab="$5" hdparm="$6" udisksctl="$7" dd="$8" fuser="$9" cat="${10}"
  local P='[A-Za-z0-9][A-Za-z0-9_.-]*'                  # pool (rePool)
  local D='[A-Za-z0-9][A-Za-z0-9_./-]*'                 # dataset (reDataset)
  local N='[A-Za-z0-9][A-Za-z0-9_.\:-]*'                # snapshot name (reSnapName)
  local S="${D}@${N}"
  local V='(/dev/disk/by-id/)?[A-Za-z0-9][A-Za-z0-9_.\:-]*'  # validNewDev
  local K='/dev/[A-Za-z0-9][A-Za-z0-9_.-]*'             # kernel device path
  local BM="${D}\#ezrepl-last"                          # replication.BookmarkName
  local TOPO='( (mirror|raidz[123]))?'
  local PROPS='(compression|recordsize|atime|relatime|sync|checksum|copies|xattr|acltype|aclinherit|primarycache|secondarycache|logbias|canmount|mountpoint|exec|setuid|devices|readonly|snapdir|quota|reservation|volsize|volblocksize)'
  local VAL='[A-Za-z0-9_./-]+'
  # Read-only listings (the fallback when /dev/zfs refuses a non-root read).
  # No token may be -c: 'zpool status/iostat -c' runs scripts.
  local RD='( (-[A-Zabd-z]+|--json|[A-Za-z0-9_./@][A-Za-z0-9_.\,/@\:-]*))*'
  local u="${SVC_USER} ALL=(root) NOPASSWD:"
  cat <<EOF
# EasyZFS — generated by deploy/install.sh; every argument pinned.
$u ${zpool} ^create (-o ashift\=[0-9]+ )?${P}${TOPO}( ${V})+\$
$u ${zpool} ^add ${P}${TOPO}( ${V})+\$
$u ${zpool} ^(replace|attach) ${P} ${V} ${V}\$
$u ${zpool} ^(offline|online|detach) ${P} ${V}\$
$u ${zpool} ^clear ${P}( ${V})?\$
$u ${zpool} import
$u ${zpool} ^(destroy|trim) ${P}\$
# -N only: a plain 'zpool import <pool>' mounts every dataset at whatever the
# pool records, /etc included, and nothing in the code does that any more.
$u ${zpool} ^import -N ${P}\$
$u ${zpool} ^export (-f )?${P}\$
$u ${zpool} ^scrub (-p |-s )?${P}\$
$u ${zpool} ^checkpoint (-d )?${P}\$
$u ${zpool} ^set autotrim\=(on|off) ${P}\$
$u ${zpool} events -f
$u ${zpool} ^history -i ${P}\$
$u ${zpool} ^(list|get|status|iostat)${RD}\$
$u ${zpool} --version
$u ${zfs} ^create -p -o compression\=(lz4|zstd|off)( -o atime\=(on|off|relatime))?( -o quota\=[0-9]+)?( -V [0-9]+)?( -o encryption\=aes-256-gcm -o keyformat\=passphrase -o keylocation\=prompt)? ${D}\$
$u ${zfs} ^create -o mountpoint\=none -o canmount\=off ${P}/easyzfs-trash\$
$u ${zfs} ^load-key (-n -L prompt )?${D}\$
$u ${zfs} ^unload-key ${D}\$
$u ${zfs} ^change-key -o keyformat\=passphrase ${D}\$
$u ${zfs} ^set ${PROPS}\=${VAL} ${D}\$
$u ${zfs} ^inherit (-S )?${PROPS} ${D}\$
$u ${zfs} ^destroy (-r )?${D}(@${N}|\#ezrepl-last)?\$
$u ${zfs} ^snapshot (-r )?${S}\$
$u ${zfs} ^rollback -r ${S}\$
$u ${zfs} ^diff -FHt ${S} ${S}\$
$u ${zfs} ^clone (-o mountpoint\=${VAL} )?${S} ${D}\$
$u ${zfs} ^rename ${D} ${D}\$
$u ${zfs} ^(promote|mount|unmount|share) ${D}\$
$u ${zfs} ^bookmark ${S} ${BM}\$
$u ${zfs} ^send -v( -w)?( -i ${BM})? ${S}\$
$u ${zfs} ^recv -s ${D}\$
$u ${zfs} ^rewrite -r -x /[A-Za-z0-9_./-]*\$
$u ${zfs} ^(list|get)${RD}\$
$u ${zfs} version
$u ${smartctl} ^-j -a ${K}\$
$u ${smartctl} ^-t (short|long) ${K}\$
$u ${dd} ^if\=${K} of\=/dev/null bs\=1M count\=2048\$
$u ${hdparm} ^-y ${K}\$
$u ${udisksctl} ^power-off -b ${K}\$
$u ${fuser} ^-sm? /[A-Za-z0-9_./-]+\$
$u ${cat} /etc/pve/storage.cfg
$u ${lsblk} ^-J( -[bd])? -o [A-Z\,]+( ${K})?\$
$u ${crontab} -l
$u ${SYSD_HELPER}
EOF
}

# write_sudoers — the service's sudo grant (pinned_sudoers, or the read-only
# set below). Validated with visudo -cf before it is installed.

write_sudoers() {
  local zpool_path zfs_path smartctl_path lsblk_path crontab_path udisksctl_path hdparm_path content
  zpool_path="$(command -v zpool 2>/dev/null || echo /usr/sbin/zpool)"
  zfs_path="$(command -v zfs 2>/dev/null || echo /usr/sbin/zfs)"
  smartctl_path="$(command -v smartctl 2>/dev/null || echo /usr/sbin/smartctl)"
  lsblk_path="$(command -v lsblk 2>/dev/null || echo /usr/bin/lsblk)"
  crontab_path="$(command -v crontab 2>/dev/null || echo /usr/bin/crontab)"
  udisksctl_path="$(command -v udisksctl 2>/dev/null || echo /usr/bin/udisksctl)"
  hdparm_path="$(command -v hdparm 2>/dev/null || echo /usr/sbin/hdparm)"
  local dd_path; dd_path="$(command -v dd 2>/dev/null || echo /usr/bin/dd)"
  # fuser (psmisc): the recycle bin checks nothing is still using a dataset.
  local fuser_path; fuser_path="$(command -v fuser 2>/dev/null || echo /usr/bin/fuser)"
  # cat: /etc/pve/storage.cfg (root:www-data 0640) names the datasets Proxmox
  # uses as storage; pinned to exactly that file.
  local cat_path; cat_path="$(command -v cat 2>/dev/null || echo /usr/bin/cat)"
  if sudo_has_regex; then
    content="$(pinned_sudoers "$zpool_path" "$zfs_path" "$smartctl_path" "$lsblk_path" "$crontab_path" "$hdparm_path" "$udisksctl_path" "$dd_path" "$fuser_path" "$cat_path")"
  elif [ "$OPT_READONLY" = "1" ]; then
    : # require_sudo_regex below stops the install with its own message
  elif [ "$OPT_UNPINNED" = "1" ]; then
    # Only on explicit request: this grant is root-equivalent ('zfs program',
    # 'zpool import -d', altroot…). It used to be a silent fallback, printed
    # as a warning nobody sees under --yes.
    warn "sudo < 1.9.10: se concede zpool/zfs/smartctl sin restringir argumentos (--allow-unpinned-sudo)."
    content="${SVC_USER} ALL=(root) NOPASSWD: ${zpool_path}, ${zfs_path}, ${smartctl_path}, ${lsblk_path}, ${crontab_path} -l, ${hdparm_path} -y /dev/*, ${udisksctl_path} power-off -b /dev/*, ${dd_path} if=/dev/* of=/dev/null bs=1M count=2048, ${SYSD_HELPER}"
  else
    die "sudo < 1.9.10 no permite fijar los argumentos de zpool/zfs: el servicio sería equivalente a root. Actualiza sudo, instala con --read-only, o acepta el riesgo con --allow-unpinned-sudo."
  fi
  if [ "$OPT_READONLY" = "1" ]; then
    # Read-only: the reads that need root, each pinned to the exact arguments
    # the service uses, and nothing else. No zpool or zfs subcommand that can
    # change a pool, no root helper, no disk power commands: even a
    # compromised service cannot change storage. Everything else it reads
    # (zpool list/status/get/iostat, zfs list/get, lsblk) works without root.
    # A whole-argument regex (^…$) needs sudo 1.9.10; ':' is escaped because
    # sudoers treats it as a separator.
    require_sudo_regex
    content="${SVC_USER} ALL=(root) NOPASSWD: ${smartctl_path} ^-j -a /dev/[A-Za-z0-9._-]+\$, ${zpool_path} events -f, ${zpool_path} ^history -i [A-Za-z0-9._-]+\$, ${zfs_path} ^diff -FHt [A-Za-z0-9._/@\\:-]+ [A-Za-z0-9._/@\\:-]+\$, ${crontab_path} -l, ${cat_path} /etc/pve/storage.cfg"
  fi
  # The collectors run smartctl through sudo for every disk on every poll;
  # the app keeps its own audit log, so sudo's per-command log and PAM session
  # would only flood the journal (as deploy/easyzfs.sudoers always did).
  # Refused attempts are still logged.
  content="Defaults:${SVC_USER} !pam_session, !log_allowed"$'\n'"${content}"
  if [ "$DRY_RUN" = "1" ]; then
    info "[DRY-RUN] escribiría ${SUDOERS_PATH} (0440) y validaría con visudo -cf:"
    printf '    %s\n' "$content"
    return 0
  fi
  local tmp=""
  tmp="$(mktemp)"
  printf '%s\n' "$content" > "$tmp"
  # Validation is not optional: ensure_sudo guarantees visudo exists, and a
  # file that cannot be checked is not installed.
  if ! visudo -cf "$tmp" >/dev/null; then
    rm -f "$tmp"
    die "visudo rechazó el fichero sudoers generado (no se instala)."
  fi
  "${SUDO[@]}" install -m 0440 "$tmp" "$SUDOERS_PATH"
  rm -f "$tmp"
  ok "Sudoers limitado instalado: ${SUDOERS_PATH} ($(printf '%s\n' "$content" | grep -c NOPASSWD) reglas)"
}

# =============================================================================
# Directorios, fichero env y unit de systemd
# =============================================================================

setup_dirs() {
  step "Directorios"
  local owner="root" group="root"
  if [ "$OPT_ROOT_MODE" = "0" ]; then owner="$SVC_USER"; group="$SVC_USER"; fi
  run "${SUDO[@]}" install -d -m 0755 -o "$owner" -g "$group" "$DATA_DIR"
  run "${SUDO[@]}" install -d -m 0750 "$ENV_DIR"
  ok "${DATA_DIR} (datos) y ${ENV_DIR} (config) listos."
}

# random_password — contraseña aleatoria de 20 caracteres alfanuméricos.
random_password() {
  if [ "$DRY_RUN" = "1" ]; then printf '%s' "DRYRUN-password-0000"; return 0; fi
  openssl rand -base64 24 | tr -dc 'A-Za-z0-9' | cut -c1-20
}

# configure_env — /etc/easyzfs/env (0600) con las vars exactas que acepta el
# binario (internal/config): LISTEN_ADDR, DB_PATH, SESSION_SECRET, ADMIN_PASSWORD.
# Idempotente: en reinstalaciones reutiliza los secretos y el puerto ya existentes.
# choose_listen_host — where the web interface listens. It can change pools,
# so it is not exposed on every interface unless that is asked for: --yes
# means localhost (reach it with a reverse proxy or 'ssh -L'); interactively
# the choice is localhost, one of this host's addresses, or all interfaces.
choose_listen_host() {
  if [ -z "$LISTEN_HOST" ]; then
    if [ "$OPT_YES" = "1" ]; then
      LISTEN_HOST="127.0.0.1"
    else
      local opts=(127.0.0.1 "Solo este equipo (proxy inverso o túnel 'ssh -L'). Recomendado") ip iface
      while read -r iface ip; do
        opts+=("$ip" "Solo la red de ${iface} (${ip})")
      done < <(ip -4 -o addr show scope global 2>/dev/null | awk '{sub(/\/.*/, "", $4); print $2, $4}')
      opts+=(all "Todas las interfaces (cualquiera que alcance este equipo; cortafuegos a tu cargo)")
      menu LISTEN_HOST "EasyZFS — dirección de escucha" "¿Desde dónde se podrá abrir la interfaz web?" "${opts[@]}"
    fi
  fi
  case "$LISTEN_HOST" in
    all|127.0.0.1) ;;
    *)
      [[ "$LISTEN_HOST" =~ ^[0-9]{1,3}(\.[0-9]{1,3}){3}$ ]] || die "Dirección de escucha inválida: ${LISTEN_HOST} (127.0.0.1, una IPv4 de este equipo o all)."
      if [ "$DRY_RUN" != "1" ] && ! ip -4 -o addr show 2>/dev/null | awk '{sub(/\/.*/, "", $4); print $4}' | grep -qx "$LISTEN_HOST"; then
        die "La dirección ${LISTEN_HOST} no pertenece a este equipo."
      fi
      ;;
  esac
  if [ "$LISTEN_HOST" = "all" ]; then
    warn "La interfaz web escuchará en todas las interfaces: limita el puerto con el cortafuegos."
    PROBE_HOST="127.0.0.1"
  else
    PROBE_HOST="$LISTEN_HOST"
  fi
}

# listen_addr — the LISTEN_ADDR value: ":port" for every interface.
listen_addr() {
  if [ "$LISTEN_HOST" = "all" ]; then printf ':%s' "$OPT_PORT"; else printf '%s:%s' "$LISTEN_HOST" "$OPT_PORT"; fi
}

configure_env() {
  step "Configuración (${ENV_FILE})"

  # Reutilizar secretos y puerto existentes (idempotencia en reinstalaciones)
  local existing_secret="" existing_admin="" existing_port=""
  local existing_vapid_pub="" existing_vapid_priv="" existing_vapid_sub=""
  if [ "$DRY_RUN" != "1" ] && [ -r "$ENV_FILE" ]; then
    existing_secret="$(sed -n 's/^SESSION_SECRET=//p' "$ENV_FILE" | head -1)"
    existing_admin="$(sed -n 's/^ADMIN_PASSWORD=//p' "$ENV_FILE" | head -1)"
    # The whole address, not just the port: reading only 'LISTEN_ADDR=:port'
    # turned an existing 127.0.0.1:8080 into :8080 (every interface) on reinstall.
    local existing_listen
    existing_listen="$(sed -n 's/^LISTEN_ADDR=//p' "$ENV_FILE" | head -1 | tr -d '\r')"
    if [ -n "$existing_listen" ]; then
      existing_port="${existing_listen##*:}"
      if [ "$LISTEN_FROM_FLAG" = "0" ]; then
        case "${existing_listen%:*}" in ""|0.0.0.0|"[::]") LISTEN_HOST="all" ;; *) LISTEN_HOST="${existing_listen%:*}" ;; esac
      fi
    fi
    existing_vapid_pub="$(sed -n 's/^VAPID_PUBLIC_KEY=//p' "$ENV_FILE" | head -1)"
    existing_vapid_priv="$(sed -n 's/^VAPID_PRIVATE_KEY=//p' "$ENV_FILE" | head -1)"
    existing_vapid_sub="$(sed -n 's/^VAPID_SUBJECT=//p' "$ENV_FILE" | head -1)"
    existing_webhook="$(sed -n 's/^WEBHOOK_SECRET=//p' "$ENV_FILE" | head -1)"
    # DEMO is written by the installer but was never read back, so a reinstall
    # without --demo silently switched a demo install to production.
    if [ "$OPT_DEMO" = "0" ] && [ "$(sed -n 's/^DEMO=//p' "$ENV_FILE" | head -1)" = "1" ]; then
      OPT_DEMO=1; info "Se conserva DEMO=1 de ${ENV_FILE}."
    fi
    if [ "$OPT_READONLY" = "0" ] && [ "$(sed -n 's/^EASYZFS_READONLY=//p' "$ENV_FILE" | head -1)" = "1" ]; then
      OPT_READONLY=1; info "Se conserva el modo solo lectura (EASYZFS_READONLY=1)."
    fi
  fi
  # Prioridad del puerto: --port > puerto del env existente > defecto (8080)
  if [ "$PORT_FROM_FLAG" = "0" ] && [ -n "$existing_port" ]; then
    OPT_PORT="$existing_port"
  fi

  choose_listen_host
  if [ "$OPT_YES" = "0" ] && [ "$PORT_FROM_FLAG" = "0" ]; then
    prompt OPT_PORT "Puerto de escucha de la interfaz web" "$OPT_PORT"
  fi
  if ! [[ "$OPT_PORT" =~ ^[0-9]+$ ]] || [ "$OPT_PORT" -lt 1 ] || [ "$OPT_PORT" -gt 65535 ]; then
    die "Puerto inválido: ${OPT_PORT}"
  fi

  # Puerto ocupado: si coincide con el del env existente es NUESTRO propio
  # servicio corriendo (reinstalación) y no hay conflicto. Si es otro proceso:
  # --port explícito aborta; interactivo pregunta (sugiere el siguiente libre);
  # no interactivo usa el siguiente libre con aviso.
  if [ "$DRY_RUN" != "1" ] && [ "$OPT_PORT" != "$existing_port" ] && port_in_use "$OPT_PORT"; then
    if [ "$PORT_FROM_FLAG" = "1" ]; then
      die "El puerto ${OPT_PORT} ya está en uso (se pidió con --port). Elige otro: ss -tlnp | grep :${OPT_PORT}"
    fi
    local next=""
    next="$(next_free_port "$OPT_PORT")" || next=""
    if [ "$OPT_YES" = "0" ] && tty_ok; then
      local elegido=""
      while true; do
        prompt elegido "El puerto ${OPT_PORT} está ocupado. ¿En qué puerto escucha EasyZFS?" "${next:-}"
        if ! [[ "$elegido" =~ ^[0-9]+$ ]] || [ "$elegido" -lt 1 ] || [ "$elegido" -gt 65535 ]; then
          warn "Puerto inválido: ${elegido} (1-65535)."
          continue
        fi
        if port_in_use "$elegido"; then
          warn "El puerto ${elegido} también está en uso."
          continue
        fi
        OPT_PORT="$elegido"
        break
      done
    elif [ -n "$next" ]; then
      warn "El puerto ${OPT_PORT} está en uso por otro proceso; EasyZFS escuchará en ${next}."
      OPT_PORT="$next"
    else
      die "Puerto ${OPT_PORT} ocupado y ninguno libre entre $((OPT_PORT + 1)) y $((OPT_PORT + 21))."
    fi
  fi

  local secret="$existing_secret"
  if [ -z "$secret" ]; then
    if [ "$DRY_RUN" = "1" ]; then
      secret="dry-run-session-secret"
    else
      secret="$(openssl rand -hex 32)"
    fi
  fi

  local admin="$existing_admin"
  if [ -z "$admin" ]; then
    local typed=""
    prompt_password typed "Contraseña del usuario 'admin'"
    if [ -n "$typed" ]; then
      admin="$typed"
    else
      admin="$(random_password)"
      GENERATED_ADMIN="$admin"
    fi
  fi

  # Modo demo: --demo, o pregunta en instalaciones nuevas interactivas.
  if [ "$OPT_DEMO" = "0" ] && [ ! -r "$ENV_FILE" ] && [ "$OPT_YES" = "0" ] && [ "$DRY_RUN" != "1" ]; then
    if confirm "¿Arrancar en MODO DEMO? (pools/discos de muestra para explorar; tus discos no se tocan)" 0; then
      OPT_DEMO=1
    fi
  fi

  # Claves VAPID (notificaciones Web Push): se generan UNA vez con el binario
  # recién instalado (-generate-vapid). Idempotente: si ya existen en el env
  # se conservan — regenerarlas invalidaría todas las suscripciones push.
  local vapid_pub="$existing_vapid_pub" vapid_priv="$existing_vapid_priv"
  local vapid_sub="$existing_vapid_sub"
  [ -z "$vapid_sub" ] && vapid_sub="mailto:easyzfs@localhost"
  if [ -z "$vapid_priv" ]; then
    if [ "$DRY_RUN" = "1" ]; then
      vapid_pub="dry-run-vapid-public"
      vapid_priv="dry-run-vapid-private"
    else
      local vapid_keys=""
      if vapid_keys="$("${SUDO[@]}" "$INSTALL_BIN" -generate-vapid 2>/dev/null)"; then
        vapid_pub="$(printf '%s\n' "$vapid_keys" | sed -n 's/^VAPID_PUBLIC_KEY=//p' | head -1)"
        vapid_priv="$(printf '%s\n' "$vapid_keys" | sed -n 's/^VAPID_PRIVATE_KEY=//p' | head -1)"
      fi
      if [ -z "$vapid_priv" ]; then
        warn "No se pudieron generar las claves VAPID: push desactivado (el servicio arrancará igual; añade VAPID_* a ${ENV_FILE} a mano)."
      fi
    fi
  else
    info "Claves VAPID ya presentes en ${ENV_FILE}: se conservan."
  fi

  # WEBHOOK_SECRET: firma HMAC para webhooks salientes. Generar UNA vez;
  # idempotente: en upgrades se conserva.
  local wbhook="$(sed -n '/^WEBHOOK_SECRET=/p' "$ENV_FILE" | sed 's/^WEBHOOK_SECRET=//' | head -1)"
  if [ -z "$wbhook" ]; then
    if [ "$DRY_RUN" = "1" ]; then
      wbhook="dry-run-webhook-secret"
    else
      wbhook="$(openssl rand -hex 32)"
    fi
  fi

  # OJO: $(...) elimina los \n finales; por eso el contenido se compone con
  # saltos de línea literales y se escribe con un único printf '%s\n'.
  local env_content="LISTEN_ADDR=$(listen_addr)
DB_PATH=${DATA_DIR}/app.db
SESSION_SECRET=${secret}
ADMIN_PASSWORD=${admin}
WEBHOOK_SECRET=${wbhook}"
  if [ -n "$vapid_priv" ]; then
    env_content+="
VAPID_PUBLIC_KEY=${vapid_pub}
VAPID_PRIVATE_KEY=${vapid_priv}
VAPID_SUBJECT=${vapid_sub}"
  fi
  if [ "$OPT_DEMO" = "1" ]; then
    env_content+="
DEMO=1"
  fi
  if [ "$OPT_READONLY" = "1" ]; then
    env_content+="
EASYZFS_READONLY=1"
  fi

  if [ "$DRY_RUN" = "1" ]; then
    info "[DRY-RUN] escribiría ${ENV_FILE} (modo 0600):"
    printf '    %s\n' "LISTEN_ADDR=$(listen_addr)" "DB_PATH=${DATA_DIR}/app.db" \
      "SESSION_SECRET=***" "ADMIN_PASSWORD=***" \
      "WEBHOOK_SECRET=***" \
      "VAPID_PUBLIC_KEY=***" "VAPID_PRIVATE_KEY=***" "VAPID_SUBJECT=${vapid_sub}" \
      "$(if [ "$OPT_DEMO" = "1" ]; then echo 'DEMO=1'; fi)"
  else
    # Keep every line of the previous file whose key the installer does not
    # manage (CSRF_CHECK, COOKIE_SECURE, SMTP_*, comments…). A reinstall used
    # to rewrite the file from scratch and silently drop them.
    local kept="" kept_hdr="# Conservado de la configuración anterior:"
    if [ -r "$ENV_FILE" ]; then
      kept="$(awk -v hdr="$kept_hdr" '
        BEGIN { n = split("LISTEN_ADDR DB_PATH SESSION_SECRET ADMIN_PASSWORD WEBHOOK_SECRET VAPID_PUBLIC_KEY VAPID_PRIVATE_KEY VAPID_SUBJECT DEMO EASYZFS_READONLY", k, " ")
                for (i = 1; i <= n; i++) managed[k[i]] = 1 }
        $0 == hdr { next }
        /^[ \t]*(export[ \t]+)?[A-Za-z_][A-Za-z0-9_]*[ \t]*=/ { key = $0; sub(/^[ \t]*(export[ \t]+)?/, "", key); sub(/[ \t]*=.*/, "", key); if (key in managed) next }
        { print }' "$ENV_FILE" | sed '/[^[:space:]]/,$!d' | sed -e :a -e '/^[[:space:]]*$/{$d;N;ba' -e '}')"
    fi
    if [ -n "$(printf '%s' "$kept" | tr -d '[:space:]')" ]; then
      env_content+="

${kept_hdr}
${kept}"
      info "Se conservan las líneas añadidas a mano en ${ENV_FILE}."
    fi
    printf '%s\n' "$env_content" | write_root_file "$ENV_FILE" 0600
  fi
  ok "Configuración escrita en ${ENV_FILE} (modo 600)."
  if [ "$OPT_DEMO" = "1" ]; then
    info "MODO DEMO activado (DEMO=1): datos de muestra; las mutaciones responden 403 demo_mode."
    info "Para pasar a producción: quita DEMO=1 de ${ENV_FILE} y reinicia el servicio."
  else
    info "Opcionales que puedes añadir: COOKIE_SECURE=1 (tras proxy TLS), RETENTION_DAYS=30, EASYZFS_ZPOOL_INTERVAL=10 (segundos con UI abierta), EASYZFS_ZPOOL_ALERT_INTERVAL=60 (heartbeat cerrada), EASYZFS_ZPOOL_IDLE_INTERVAL=300 (full collect cerrada), DEMO=1, MOCK=1."
  fi
}

# write_unit — unit basada en deploy/easyzfs.service del repo, con el usuario elegido.
write_unit() {
  step "Servicio systemd"
  local user="root" group="root"
  local nota="modo root: administración completa (decisión consciente, ver README)"
  local nnp="NoNewPrivileges=yes"
  if [ "$OPT_ROOT_MODE" = "0" ]; then
    user="$SVC_USER"; group="$SVC_USER"
    nota="zpool/zfs/smartctl/lsblk/crontab vía sudoers limitado: ${SUDOERS_PATH}"
    # NoNewPrivileges=yes bloquea el bit setuid de sudo: solo se puede poner
    # en modo root (sin sudo). En modo usuario+sudoers tiene que ir fuera.
    nnp="# NoNewPrivileges=yes (incompatible con sudo setuid; superficie root limitada por sudoers)"
  fi
  local unit=""
  unit="$(cat <<EOF
[Unit]
Description=EasyZFS — gestión ZFS del NAS (colector + PWA)
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=${user}
Group=${group}
EnvironmentFile=${ENV_FILE}
ExecStart=${INSTALL_BIN}
Restart=on-failure
RestartSec=5

# Huella y longevidad
MemoryMax=256M
LimitNOFILE=4096

# Hardening (${nota})
${nnp}
# No ProtectSystem/ProtectHome/PrivateTmp: each gives the service a private
# mount namespace, and the zfs commands it runs through sudo inherit it. A
# dataset it mounted was then visible only inside, and one it unmounted,
# renamed or destroyed stayed mounted on the host (destroy failed "busy",
# the recycle bin left stale mounts). Found on a Proxmox VE 8.4 VM. The
# namespace never kept a bad mountpoint off the host either: ZFS applies
# the property at the next import. Mountpoints are checked when set
# (internal/actions/props.go); the root surface is the pinned sudoers file.

# Solo si escucha en puerto <1024 (preferir puerto alto + proxy):
# AmbientCapabilities=CAP_NET_BIND_SERVICE

[Install]
WantedBy=multi-user.target
EOF
)"
  if [ "$DRY_RUN" = "1" ]; then
    info "[DRY-RUN] escribiría ${UNIT_PATH} (modo 0644):"
    printf '%s\n' "$unit" | sed 's/^/    /'
  else
    printf '%s\n' "$unit" | write_root_file "$UNIT_PATH" 0644
  fi
  # daemon-reload + enable + restart: reinstalar actualiza el binario y reinicia
  run "${SUDO[@]}" systemctl daemon-reload
  run "${SUDO[@]}" systemctl enable easyzfs.service
  run "${SUDO[@]}" systemctl restart easyzfs.service
  ok "Servicio habilitado y (re)iniciado."

  setup_update_units
}


# detect_bin_channel — asks the installed binary where it is updated from
# (easyzfs -update-channel). A binary that predates the flag fails on it and
# is taken as upstream's ("github"). In a dry run nothing is installed, so the
# answer comes from --binary, or from how the binary would be obtained.
detect_bin_channel() {
  local bin="$INSTALL_BIN"
  if [ "$DRY_RUN" = "1" ]; then
    case "$BIN_MODE" in
      local) bin="$OPT_BINARY" ;;
      build) BIN_CHANNEL="local"; return 0 ;;   # this fork's Makefile default
      *)     BIN_CHANNEL="github"; return 0 ;;
    esac
  fi
  BIN_CHANNEL="$("$bin" -update-channel 2>/dev/null)" || BIN_CHANNEL=""
  [ "$BIN_CHANNEL" = "local" ] || BIN_CHANNEL="github"
  info "Canal de actualización del binario: ${BIN_CHANNEL}"
}

# remove_update_units — stops and deletes every unit that applies upstream
# releases, and /opt/easyzfs. Idempotent: absent units are skipped.
remove_update_units() {
  local u removed=0
  # Stop the two triggers first, so neither fires while the units go.
  if command -v systemctl >/dev/null 2>&1; then
    for u in easyzfs-update-weekly.timer easyzfs-update.path; do
      [ -e "/etc/systemd/system/${u}" ] && { run "${SUDO[@]}" systemctl disable --now "$u" || true; }
    done
  fi
  for u in $UPDATE_UNITS; do
    if [ -e "/etc/systemd/system/${u}" ]; then
      run "${SUDO[@]}" rm -f "/etc/systemd/system/${u}"; ok "Unit eliminada: /etc/systemd/system/${u}"; removed=1
    fi
  done
  if [ -d /opt/easyzfs ]; then
    run "${SUDO[@]}" rm -rf /opt/easyzfs; ok "Eliminado: /opt/easyzfs"; removed=1
  fi
  if [ -e "$APPLY_HELPER" ]; then
    run "${SUDO[@]}" rm -f "$APPLY_HELPER"; ok "Eliminado: ${APPLY_HELPER}"
  fi
  # Whatever the old in-app updater staged for easyzfs-update.path to apply.
  if [ -d "${DATA_DIR}/update" ]; then
    run "${SUDO[@]}" rm -rf "${DATA_DIR}/update"; ok "Eliminado: ${DATA_DIR}/update"
  fi
  if [ "$removed" = "1" ] && command -v systemctl >/dev/null 2>&1; then
    run "${SUDO[@]}" systemctl daemon-reload || true
  fi
}

# setup_update_units — the root units through which a binary gets replaced:
# easyzfs-update.path (applies what the in-app updater stages) and the weekly
# timer. A binary on the "local" channel is updated from its checkout only
# (make update), so it gets neither, and any left by an earlier install of an
# upstream release are removed: nothing can then replace it from GitHub.
setup_update_units() {
  if [ "$BIN_CHANNEL" = "local" ]; then
    info "Binario del checkout local: sin auto-update desde GitHub (se actualiza con 'make update')."
    remove_update_units
    return 0
  fi
  local user="$SVC_USER" group="$SVC_USER"
  if [ "$(service_user)" = "root" ]; then user="root"; group="root"; fi
  # Unit de auto-update (patrón app-auto-update): easyzfs-update.path vigila
  # $DATA_DIR/update/.restart-me; cuando el updater lo toca, el oneshot instala
  # el binario nuevo (easyzfs.new) sobre INSTALL_BIN y reinicia el servicio.
  local upd_path="/etc/systemd/system/easyzfs-update.path"
  local upd_svc="/etc/systemd/system/easyzfs-update.service"
  local upd_dir="${DATA_DIR}/update"
  # The unit used to 'install' whatever the service account left in upd_dir,
  # as root. easyzfs-apply-update re-verifies it against the release's
  # checksums.txt first. It exists only in this fork, never in an upstream
  # release tag, so it comes from the checkout or not at all; without it the
  # .path unit is not installed and an update is applied by hand.
  local apply_src; apply_src="$(local_deploy_file easyzfs-apply-update)"
  if [ -z "$apply_src" ]; then
    info "Sin easyzfs-apply-update junto al instalador: no se instala easyzfs-update.path (las versiones descargadas se aplican a mano)."
    local u
    for u in easyzfs-update.path easyzfs-update.service; do
      if [ -e "/etc/systemd/system/${u}" ]; then
        [ "$u" = easyzfs-update.path ] && { run "${SUDO[@]}" systemctl disable --now "$u" || true; }
        run "${SUDO[@]}" rm -f "/etc/systemd/system/${u}"; ok "Unit eliminada: /etc/systemd/system/${u}"
      fi
    done
  elif [ "$DRY_RUN" = "1" ]; then
    info "[DRY-RUN] escribiría ${upd_path} y ${upd_svc} (auto-update: ${upd_dir}/.restart-me)"
  else
    run "${SUDO[@]}" mkdir -p "$(dirname "$APPLY_HELPER")"
    run "${SUDO[@]}" install -m 0755 -o root -g root "$apply_src" "$APPLY_HELPER" \
      || die "No se pudo instalar ${APPLY_HELPER}"
    # upd_dir sits in DATA_DIR, which the service account owns, so it may
    # have been replaced by a symlink: a plain chown would then hand the
    # link's target (say /etc/systemd/system) to that account. Drop anything
    # that is not a real directory, and never follow a link when chowning.
    if [ -L "$upd_dir" ] || { [ -e "$upd_dir" ] && [ ! -d "$upd_dir" ]; }; then
      "${SUDO[@]}" rm -f "$upd_dir"
    fi
    "${SUDO[@]}" mkdir -p "$upd_dir"
    "${SUDO[@]}" chown -h "${user}:${group}" "$upd_dir"
    write_root_file "$upd_path" 0644 <<EOF
[Unit]
Description=Reinicia EasyZFS cuando el updater prepara una versión nueva

[Path]
PathChanged=${upd_dir}/.restart-me

[Install]
WantedBy=multi-user.target
EOF
    write_root_file "$upd_svc" 0644 <<EOF
[Unit]
Description=Aplica la actualización de EasyZFS (instala el binario nuevo y reinicia)
After=network-online.target

[Service]
Type=oneshot
ExecStart=${APPLY_HELPER} ${upd_dir} ${INSTALL_BIN}
EOF
    run "${SUDO[@]}" systemctl daemon-reload
    run "${SUDO[@]}" systemctl enable --now easyzfs-update.path
    ok "Auto-update: easyzfs-update.path activo (aplica versiones descargadas por /api/update/apply)."

  fi

  # Timer de auto-update semanal (patrón Keynest/Deltos): comprueba releases
  # estables una vez por semana y aplica automáticamente con checksums.
  #
  # Only for a binary that came from a release, and with .release-id set to
  # that release: the weekly job installs the latest upstream release over
  # whatever does not match it. For a local binary or a source build (a fork,
  # a patched build) that would silently replace it every week, so the timer
  # is skipped there. The marker was written from NEW_VERSION, which nothing
  # defined: under set -u that aborted the installer right after the service
  # had started. The copy used a relative path (absent under curl | bash) and
  # no sudo for the root-owned /opt/easyzfs.
  local upd_weekly_svc="/etc/systemd/system/easyzfs-update-weekly.service"
  local upd_weekly_timer="/etc/systemd/system/easyzfs-update-weekly.timer"
  local upd_script="/opt/easyzfs/easyzfs-update-weekly.sh"
  local weekly_src="" weekly_tmp=0
  if [ "$BIN_MODE" != "download" ]; then
    info "Auto-update semanal NO instalado: el binario es local o compilado (${BIN_MODE}), y el timer lo sustituiría por la última release oficial."
  elif [ -z "${DOWNLOADED_TAG:-}" ]; then
    info "Auto-update semanal NO instalado: no se pudo determinar de qué release oficial viene el binario (¿URL propia?)."
  elif [ "$DRY_RUN" = "1" ]; then
    info "[DRY-RUN] instalaría easyzfs-update-weekly.timer + .service + script (release ${DOWNLOADED_TAG})"
  else
    "${SUDO[@]}" mkdir -p /opt/easyzfs
    weekly_src="$(local_deploy_file easyzfs-update-weekly.sh)"
    if [ -z "$weekly_src" ]; then
      weekly_src="$(mktemp)"; weekly_tmp=1
      curl -fsSL "https://raw.githubusercontent.com/gnacho/easyzfs/${DOWNLOADED_TAG}/deploy/easyzfs-update-weekly.sh" \
        -o "$weekly_src" || { rm -f "$weekly_src"; die "No se pudo descargar easyzfs-update-weekly.sh."; }
      head -1 "$weekly_src" | grep -q '^#!/bin/sh' \
        || { rm -f "$weekly_src"; die "easyzfs-update-weekly.sh descargado no es el script esperado."; }
    fi
    "${SUDO[@]}" install -m 0755 "$weekly_src" "$upd_script"
    [ "$weekly_tmp" = "1" ] && rm -f "$weekly_src"
    printf '%s\n' "${DOWNLOADED_TAG#v}" | "${SUDO[@]}" tee /opt/easyzfs/.release-id >/dev/null
    write_root_file "$upd_weekly_svc" 0644 <<EOF
[Unit]
Description=EasyZFS weekly auto-update check and apply
After=network-online.target
Wants=network-online.target

[Service]
Type=oneshot
ExecStart=/opt/easyzfs/easyzfs-update-weekly.sh
User=root
StandardOutput=journal
StandardError=journal
EOF
    write_root_file "$upd_weekly_timer" 0644 <<EOF
[Unit]
Description=EasyZFS weekly auto-update timer
After=network-online.target

[Timer]
OnCalendar=weekly
Persistent=true
RandomizedDelaySec=4h

[Install]
WantedBy=timers.target
EOF
    run "${SUDO[@]}" systemctl daemon-reload
    run "${SUDO[@]}" systemctl enable --now easyzfs-update-weekly.timer
    ok "Auto-update semanal: easyzfs-update-weekly.timer activo (comprueba y aplica releases estables 1 vez/semana)."
  fi
}

# service_user — the User= the service runs as: from --root-mode on a fresh
# install, from the installed unit on an update.
service_user() {
  if [ "$OPT_ROOT_MODE" = "1" ]; then echo root; return 0; fi
  if [ "$OPT_UPDATE" = "1" ] && [ -r "$UNIT_PATH" ]; then
    local u; u="$(sed -n 's/^User=//p' "$UNIT_PATH" | head -1)"
    echo "${u:-root}"; return 0
  fi
  echo "$SVC_USER"
}

# =============================================================================
# Verificación final y resumen
# =============================================================================

verify_service() {
  # strict: no HTTP answer is a failure, not a warning (used by --update, where
  # "updated" must mean "the new binary is serving").
  local strict="${1:-}"
  step "Verificación"
  if [ "$DRY_RUN" = "1" ]; then
    info "[DRY-RUN] comprobaría: systemctl is-active easyzfs y HTTP en ${PROBE_HOST}:${OPT_PORT}"
    return 0
  fi
  local i
  for i in $(seq 1 10); do
    if systemctl is-active --quiet easyzfs; then break; fi
    sleep 1
  done
  if ! systemctl is-active --quiet easyzfs; then
    err "El servicio no arrancó. Revisa: journalctl -u easyzfs -n 50 --no-pager"
    return 1
  fi
  ok "Servicio activo (systemd)."
  # /api/version: 200 si responde, 401 si exige login — ambos prueban que escucha.
  # OJO: curl imprime "000" con -w incluso al fallar la conexión; no concatenar
  # otro "000" con `|| echo 000` (salía "000000"). Reintenta unos segundos:
  # el servicio puede tardar un poco en bindear tras el restart.
  local code="000" i
  for i in $(seq 1 10); do
    code="$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 \
        "http://${PROBE_HOST}:${OPT_PORT}/api/version" 2>/dev/null)" || true
    [ "$code" != "000" ] && break
    sleep 1
  done
  if [ "$code" = "000" ]; then
    warn "Sin respuesta HTTP en ${PROBE_HOST}:${OPT_PORT} (¿firewall o arranque lento?)."
    warn "Comprueba: systemctl status easyzfs && journalctl -u easyzfs -n 50"
    if [ "$strict" = "strict" ]; then return 1; fi
  else
    ok "HTTP escuchando en ${PROBE_HOST}:${OPT_PORT} (/api/version → código ${code})."
  fi
}

summary() {
  local ip=""
  # 'hostname -I' is Debian's; Arch's hostname exits 64 on it, and under
  # set -eo pipefail that ended the installer here, after a good install.
  ip="$(hostname -I 2>/dev/null | awk '{print $1}' || true)"
  ip="${ip:-127.0.0.1}"
  # The URL is only reachable where the service listens.
  case "$LISTEN_HOST" in all|"") ;; *) ip="$LISTEN_HOST" ;; esac
  step "Instalación completada"
  cat <<EOF
  URL:      http://${ip}:${OPT_PORT}
  Usuario:  admin
EOF
  if [ -n "$GENERATED_ADMIN" ]; then
    printf '  Clave:    %s  %s\n' "$GENERATED_ADMIN" \
      "${C_YELLOW:-}(guárdala: solo se muestra esta vez)${C_RESET:-}"
  else
    printf '  Clave:    la de ADMIN_PASSWORD en %s\n' "$ENV_FILE"
  fi
  cat <<EOF

  Comandos útiles:
    systemctl status easyzfs
    journalctl -u easyzfs -f
    systemctl restart easyzfs
  Config: ${ENV_FILE}  ·  Datos: ${DATA_DIR}
EOF
  if [ "$OPT_DEMO" = "1" ]; then
    cat <<EOF
  MODO DEMO activo: pools y discos de muestra; nada se modifica (403 demo_mode).
  Para usar tus discos reales: quita DEMO=1 de ${ENV_FILE} y reinicia.
EOF
  else
    cat <<EOF
  Para explorar primero con datos de muestra: añade DEMO=1 a ${ENV_FILE} y reinicia.
EOF
  fi
  # A checkout install is updated and removed from that checkout; pointing it
  # at upstream's script would undo the point of installing from here.
  if [ "$BIN_CHANNEL" = "local" ]; then
    cat <<EOF
  Actualizar:  desde el checkout, git pull y después make update
  Desinstalar: desde el checkout, sudo bash deploy/install.sh --uninstall
EOF
  else
    cat <<EOF
  Desinstalar: curl -fsSL https://raw.githubusercontent.com/gnacho/easyzfs/main/deploy/install.sh | bash -s -- --uninstall
EOF
  fi
}

# =============================================================================
# Desinstalación
# =============================================================================

# =============================================================================
# Update from a checkout
# =============================================================================

# do_update — updates an install made from a checkout: replaces the binary
# (keeping the previous one as easyzfs.prev) and the root helper, refreshes
# the sudoers file, and restarts. Deliberately does not touch the env file,
# the data, the service account or the unit, and never downloads a binary:
# it takes --binary or --source only.
do_update() {
  step "Actualización de ${APP}"
  [ -e "$INSTALL_BIN" ] && [ -e "$UNIT_PATH" ] \
    || die "No hay una instalación que actualizar (${INSTALL_BIN}, ${UNIT_PATH}). Usa 'make install'."
  if [ -n "$OPT_BINARY" ]; then BIN_MODE="local"
  elif [ -n "$OPT_SOURCE" ]; then BIN_MODE="build"
  else die "--update necesita --binary o --source: nunca descarga."
  fi
  check_root
  # Verify against the address the service really listens on: OPT_PORT is
  # only a fresh install's default, so probing it reported updates on another
  # port as unreachable, or as healthy if something else answered there.
  if [ -r "$ENV_FILE" ]; then
    [ "$(sed -n 's/^EASYZFS_READONLY=//p' "$ENV_FILE" | head -1)" = "1" ] && OPT_READONLY=1
    local la; la="$(sed -n 's/^LISTEN_ADDR=//p' "$ENV_FILE" | head -1 | tr -d '\r')"
    if [ -n "$la" ]; then
      OPT_PORT="${la##*:}"
      case "${la%:*}" in ""|0.0.0.0|"[::]") PROBE_HOST="127.0.0.1" ;; *) PROBE_HOST="${la%:*}" ;; esac
    fi
  else
    warn "No se puede leer ${ENV_FILE}: se comprobará ${PROBE_HOST}:${OPT_PORT}."
  fi
  # A local build gets no update units. Remove them before the swap: the old
  # daemon's updater and easyzfs-update.path are live until then, and a
  # release staged in that window would be installed over the new binary.
  local new_ch="local"
  if [ "$BIN_MODE" = "local" ]; then
    new_ch="$("$OPT_BINARY" -update-channel 2>/dev/null)" || new_ch=""
  fi
  if [ "$new_ch" = "local" ]; then remove_update_units; fi
  run "${SUDO[@]}" cp -p "$INSTALL_BIN" "${INSTALL_BIN}.prev" \
    || die "No se pudo guardar el binario actual en ${INSTALL_BIN}.prev; no se actualiza sin copia."
  ok "Binario anterior guardado en ${INSTALL_BIN}.prev"
  install_binary
  install_sysd_helper
  if [ "$(service_user)" != "root" ]; then write_sudoers; fi
  detect_bin_channel
  setup_update_units
  # Units written before the namespace was dropped (see write_unit): take
  # out just those lines, leaving everything else in the unit as it is.
  if grep -qE '^(ProtectSystem|ProtectHome|PrivateTmp|ReadWritePaths)=' "$UNIT_PATH"; then
    run "${SUDO[@]}" sed -i -E '/^(ProtectSystem|ProtectHome|PrivateTmp|ReadWritePaths)=/d' "$UNIT_PATH"
    run "${SUDO[@]}" systemctl daemon-reload
    ok "Unit: quitado el espacio de montaje privado (los montajes de ZFS ahora son los del host)."
  fi
  run "${SUDO[@]}" systemctl restart easyzfs.service
  verify_service strict || die "El servicio no responde con el binario nuevo. Para volver al anterior: sudo install -m 0755 ${INSTALL_BIN}.prev ${INSTALL_BIN} && sudo systemctl restart easyzfs"
  ok "Actualizado. Config, datos y ${ENV_FILE} sin tocar."
}

do_uninstall() {
  step "Desinstalación de ${APP}"
  local found=0
  [ -e "$UNIT_PATH" ] && found=1
  [ -e "$INSTALL_BIN" ] && found=1
  [ -e "$SUDOERS_PATH" ] && found=1
  [ -d "$ENV_DIR" ] && found=1
  [ -d "$DATA_DIR" ] && found=1
  # The update units run as root on their own schedule; a box where only they
  # are left still has something installed.
  local u
  for u in $UPDATE_UNITS; do [ -e "/etc/systemd/system/${u}" ] && found=1; done
  [ -d /opt/easyzfs ] && found=1
  if [ "$found" = "0" ]; then
    ok "No hay nada instalado de ${APP}; nada que hacer."
    return 0
  fi
  check_root
  if command -v systemctl >/dev/null 2>&1; then
    run "${SUDO[@]}" systemctl stop easyzfs.service || true
    run "${SUDO[@]}" systemctl disable easyzfs.service || true
  fi
  # Uninstall used to leave these behind, and the weekly timer kept running as
  # root afterwards. Removed first, so neither can fire mid-uninstall.
  remove_update_units
  [ -e "$UNIT_PATH" ] && { run "${SUDO[@]}" rm -f "$UNIT_PATH"; ok "Unit eliminada: ${UNIT_PATH}"; }
  if command -v systemctl >/dev/null 2>&1; then
    run "${SUDO[@]}" systemctl daemon-reload || true
  fi
  [ -e "$INSTALL_BIN" ] && { run "${SUDO[@]}" rm -f "$INSTALL_BIN"; ok "Binario eliminado: ${INSTALL_BIN}"; }
  [ -e "${INSTALL_BIN}.prev" ] && { run "${SUDO[@]}" rm -f "${INSTALL_BIN}.prev"; ok "Eliminado: ${INSTALL_BIN}.prev"; }
  [ -e "$SYSD_HELPER" ] && { run "${SUDO[@]}" rm -f "$SYSD_HELPER"; ok "Helper eliminado: ${SYSD_HELPER}"; }
  [ -e "$APPLY_HELPER" ] && { run "${SUDO[@]}" rm -f "$APPLY_HELPER"; ok "Helper eliminado: ${APPLY_HELPER}"; }
  [ -e "$SUDOERS_PATH" ] && { run "${SUDO[@]}" rm -f "$SUDOERS_PATH"; ok "Sudoers eliminado: ${SUDOERS_PATH}"; }

  if [ -d "$DATA_DIR" ] || [ -d "$ENV_DIR" ]; then
    if [ "$OPT_YES" = "1" ]; then
      warn "Se conservan los datos (${DATA_DIR}) y la config (${ENV_DIR})."
      warn "Para borrarlos: rm -rf ${DATA_DIR} ${ENV_DIR}"
    elif confirm "¿Borrar también los datos y la configuración (${DATA_DIR}, ${ENV_DIR})?" 0; then
      run "${SUDO[@]}" rm -rf "$DATA_DIR" "$ENV_DIR"
      ok "Datos y configuración eliminados."
    fi
  fi
  if id "$SVC_USER" >/dev/null 2>&1; then
    if [ "$OPT_YES" = "0" ] && confirm "¿Eliminar también el usuario de sistema '${SVC_USER}'?" 0; then
      run "${SUDO[@]}" userdel "$SVC_USER" || true
      ok "Usuario '${SVC_USER}' eliminado."
    fi
  fi
  ok "Desinstalación completada."
}

# =============================================================================
# Argumentos y flujo principal
# =============================================================================

parse_args() {
  while [ $# -gt 0 ]; do
    case "$1" in
      --binary)   [ $# -ge 2 ] || die "--binary requiere un valor"; OPT_BINARY="$2"; shift 2 ;;
      --binary=*)  OPT_BINARY="${1#*=}"; shift ;;
      --url)      [ $# -ge 2 ] || die "--url requiere un valor"; OPT_URL="$2"; shift 2 ;;
      --url=*)     OPT_URL="${1#*=}"; shift ;;
      --source)   [ $# -ge 2 ] || die "--source requiere un valor"; OPT_SOURCE="$2"; shift 2 ;;
      --source=*)  OPT_SOURCE="${1#*=}"; shift ;;
      --port)     [ $# -ge 2 ] || die "--port requiere un valor"; OPT_PORT="$2"; PORT_FROM_FLAG=1; shift 2 ;;
      --port=*)    OPT_PORT="${1#*=}"; PORT_FROM_FLAG=1; shift ;;
      --listen)   [ $# -ge 2 ] || die "--listen requiere un valor"; LISTEN_HOST="$2"; LISTEN_FROM_FLAG=1; shift 2 ;;
      --listen=*)  LISTEN_HOST="${1#*=}"; LISTEN_FROM_FLAG=1; shift ;;
      --demo)      OPT_DEMO=1; shift ;;
      --read-only) OPT_READONLY=1; shift ;;
      --allow-unpinned-sudo) OPT_UNPINNED=1; shift ;;
      --root-mode) OPT_ROOT_MODE=1; shift ;;
      --uninstall) OPT_UNINSTALL=1; shift ;;
      --update)    OPT_UPDATE=1; shift ;;
      --yes|-y)    OPT_YES=1; shift ;;
      --help|-h)   usage; exit 0 ;;
      *) die "Opción desconocida: $1 (usa --help)" ;;
    esac
  done
}

main() {
  parse_args "$@"
  banner
  setup_ui
  if [ "$DRY_RUN" = "1" ]; then
    warn "MODO DRY-RUN: se imprimirán los comandos sin ejecutarlos."
  fi

  if [ "$OPT_UNINSTALL" = "1" ]; then
    do_uninstall
    exit 0
  fi
  if [ "$OPT_UPDATE" = "1" ]; then
    do_update
    exit 0
  fi

  detect_arch
  if [ "$ARCH" = "unknown" ]; then
    warn "Arquitectura no reconocida ($(uname -m)); solo x86_64 y aarch64 tienen assets de release."
  fi
  detect_distro
  ok "Sistema: ${DISTRO_PRETTY}  [familia=${DISTRO_FAMILY}, arch=${ARCH}]"
  if [ "$DISTRO_FAMILY" = "unknown" ]; then
    warn "Distribución desconocida: se ofrecerá continuar en modo manual en el paso de dependencias."
  fi

  check_root
  check_systemd
  check_resources
  preflight_report
  install_dependencies
  select_binary_source
  install_binary
  detect_bin_channel
  install_sysd_helper
  setup_user_and_sudoers
  setup_dirs
  configure_env
  write_unit
  verify_service
  summary
}

main "$@"
