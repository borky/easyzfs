#!/bin/sh
# easyzfs-update-weekly.sh — chequeo y aplicación semanal de actualizaciones.
#
# Ejecutado por easyzfs-update-weekly.timer (systemd, cadencia semanal).
# Downloads the latest STABLE release from GitHub and hands it to
# easyzfs-apply-update, which verifies the signature and sha256, keeps the
# current binary as .prev, installs the new one and restarts the service.
#
# A diferencia del apply in-app (POST /api/update/apply → .restart-me flag →
# easyzfs-update.path → easyzfs-update.service), este script es AUTÓNOMO:
# no depende de que el servicio esté corriendo ni de un admin que pulse un botón.
set -eu

APP=easyzfs
REPO=gnacho/easyzfs
INSTALL_BIN=/usr/local/bin/easyzfs
MARKER=/opt/easyzfs/.release-id
APPLY_HELPER=/usr/local/libexec/easyzfs-apply-update
TMP_DIR=$(mktemp -d)
trap 'rm -rf "$TMP_DIR"' EXIT INT TERM

log() { logger -t "$APP-update-weekly" "$@"; }

# 1. Detectar última release estable
echo "STEP:detect"
VER=$(curl -fsSL --max-time 20 "https://api.github.com/repos/$REPO/releases/latest" \
  | sed -n 's/.*"tag_name": *"\(v\?[0-9][^"]*\)".*/\1/p' | head -n1)
[ -n "$VER" ] || { log "no se pudo resolver la última release estable"; exit 4; }
VER_NO_V=$(printf '%s' "$VER" | sed 's/^v//')

# ¿Ya instalado?
if [ -f "$MARKER" ] && [ "$(cat "$MARKER" 2>/dev/null || true)" = "$VER_NO_V" ]; then
  log "al día ($VER_NO_V)"; exit 0
fi

# 2. Download the binary only. Verifying and installing it is the apply
# helper's job, the same one the in-app update goes through: it checks the
# minisign signature of checksums.txt against the embedded key before the
# sha256, which a checksum fetched next to the binary cannot replace. A
# second, weaker install path here would be the one an attacker uses.
echo "STEP:download"
[ -x "$APPLY_HELPER" ] || { log "falta $APPLY_HELPER: no se instala nada"; exit 5; }
ARCH=$(uname -m | sed 's/x86_64/amd64/;s/aarch64/arm64/')
BIN="${APP}_linux_${ARCH}"
BASE="https://github.com/$REPO/releases/download/$VER"
curl -fL --proto '=https' --max-time 120 "$BASE/$BIN" -o "$TMP_DIR/$APP.new"
printf '%s\n' "$VER" > "$TMP_DIR/$APP.new.tag"

# 3. Verify, install and restart. The helper keeps the replaced binary as
# ${INSTALL_BIN}.prev; a dated copy made here, before anything was verified,
# piled up every week a release was refused.
echo "STEP:install"
if ! "$APPLY_HELPER" "$TMP_DIR" "$INSTALL_BIN"; then
  log "la release $VER no pasó la verificación (firma/sha256): no se instala"
  exit 5
fi
printf '%s\n' "$VER_NO_V" > "$MARKER"

log "actualizado a $VER_NO_V"
echo "OK:$VER_NO_V"
