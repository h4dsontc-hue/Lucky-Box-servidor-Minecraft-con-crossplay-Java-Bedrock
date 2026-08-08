#!/usr/bin/env bash
# Despliegue de Lucky Box (servidor + panel) al VPS.
#
#   VPS_HOST=1.2.3.4 VPS_USER=minecraft ./deploy.sh
#
# Sincroniza ./server y compila ./panel para el VPS. Es idempotente: puedes
# relanzarlo tras cada cambio.
#
# En el VPS queda la misma disposicion que en el repo, y eso importa: el panel
# busca server/start.sh a partir de su directorio de trabajo y deja los backups
# en ../backups, fuera del alcance del rsync --delete.
#
#   /opt/lukybox/
#   ├── server/     <- ./server, sincronizado con --delete
#   ├── panel       <- binario compilado desde ./panel (lleva la web dentro)
#   └── backups/    <- tar.gz del mundo, NO se toca desde aqui
set -euo pipefail

VPS_HOST="${VPS_HOST:?Define VPS_HOST (IP o dominio del VPS)}"
VPS_USER="${VPS_USER:-minecraft}"
VPS_PATH="${VPS_PATH:-/opt/lukybox}"
SSH_PORT="${SSH_PORT:-22}"
# Casi todos los VPS son x86_64. En uno ARM (Oracle Ampere, Hetzner CAX...)
# lanza el script con VPS_ARCH=arm64.
VPS_ARCH="${VPS_ARCH:-amd64}"

REPO="$(cd "$(dirname "$0")" && pwd)"
LOCAL_DIR="$REPO/server"

SSH=(ssh -p "$SSH_PORT" "$VPS_USER@$VPS_HOST")

echo "==> Compilando el panel para linux/$VPS_ARCH"
if ! command -v go >/dev/null 2>&1; then
  echo "ERROR: no encuentro 'go' en el PATH y hace falta para compilar el panel." >&2
  echo "       Instalalo (https://go.dev/dl/) o exporta PATH=\$PATH:/usr/local/go/bin" >&2
  exit 1
fi
PANEL_BIN="$(mktemp -t lukybox-panel.XXXXXX)"
trap 'rm -f "$PANEL_BIN"' EXIT
# CGO_ENABLED=0 para que el binario no dependa de la glibc del VPS: se compila
# aqui y corre alla sea Debian, Ubuntu o Alpine.
( cd "$REPO/panel" && CGO_ENABLED=0 GOOS=linux GOARCH="$VPS_ARCH" \
    go build -trimpath -ldflags='-s -w' -o "$PANEL_BIN" . )
echo "    $(du -h "$PANEL_BIN" | cut -f1) listo"

echo "==> Comprobando el VPS"
# El despliegue antiguo dejaba start.sh directamente en $VPS_PATH. Si seguimos
# adelante sin mover el mundo, el panel arrancaria un servidor con un mundo
# nuevo y vacio mientras el de verdad se queda al lado: mejor parar y avisar.
"${SSH[@]}" VPS_PATH="$VPS_PATH" bash -s <<'REMOTE'
set -euo pipefail
if [ -f "$VPS_PATH/start.sh" ]; then
  echo "ERROR: $VPS_PATH/start.sh es de la disposicion anterior." >&2
  echo "       Ahora el servidor vive en $VPS_PATH/server/ y lo arranca el panel." >&2
  echo "       Mueve los datos UNA vez (con el servicio parado) y repite el deploy:" >&2
  echo >&2
  echo "         sudo systemctl stop lukybox" >&2
  echo "         sudo mkdir -p $VPS_PATH/server" >&2
  echo "         cd $VPS_PATH" >&2
  echo "         sudo mv world world_nether world_the_end server/ 2>/dev/null || true" >&2
  echo "         sudo mv ops.json banned-players.json banned-ips.json \\" >&2
  echo "                 whitelist.json usercache.json server/ 2>/dev/null || true" >&2
  echo "         sudo mv plugins server/ 2>/dev/null || true" >&2
  echo "         sudo rm -f start.sh paper.jar" >&2
  echo "         sudo chown -R minecraft:minecraft $VPS_PATH" >&2
  echo >&2
  echo "       El mundo, los ops y los bans se conservan; el resto lo repone el rsync." >&2
  exit 1
fi
mkdir -p "$VPS_PATH/server" "$VPS_PATH/backups"
REMOTE

echo "==> Desplegando $LOCAL_DIR  ->  $VPS_USER@$VPS_HOST:$VPS_PATH/server"

# --delete mantiene el VPS igual que el local, PERO preservamos el mundo y los
# datos que solo existen alla arriba (jugadores reales, bans, ops). Al apuntar
# a server/ el borrado no puede alcanzar al binario del panel ni a los backups.
rsync -avz --progress \
  -e "ssh -p $SSH_PORT" \
  --delete \
  --exclude 'logs/' \
  --exclude 'cache/' \
  --exclude 'crash-reports/' \
  --exclude '*.log' \
  --exclude 'libraries/' \
  --exclude 'versions/' \
  --exclude 'world/' \
  --exclude 'world_nether/' \
  --exclude 'world_the_end/' \
  --exclude 'ops.json' \
  --exclude 'banned-players.json' \
  --exclude 'banned-ips.json' \
  --exclude 'whitelist.json' \
  --exclude 'usercache.json' \
  "$LOCAL_DIR/" "$VPS_USER@$VPS_HOST:$VPS_PATH/server/"

echo "==> Subiendo el panel"
# rsync escribe en un temporal y renombra, asi que sustituir el binario no
# molesta al panel que este corriendo: seguira con el inodo viejo hasta que
# systemctl restart lo relance.
rsync -avz -e "ssh -p $SSH_PORT" \
  "$PANEL_BIN" "$VPS_USER@$VPS_HOST:$VPS_PATH/panel"

echo "==> Ajustando permisos y reiniciando el servicio"
"${SSH[@]}" VPS_PATH="$VPS_PATH" bash -s <<'REMOTE'
set -euo pipefail
chmod +x "$VPS_PATH/server/start.sh" "$VPS_PATH/panel"
# key.pem de Floodgate es un secreto compartido: solo el dueño debe leerlo.
# En un if y no con "&&": bajo set -e un && que falla aborta el script.
if [ -f "$VPS_PATH/server/plugins/floodgate/key.pem" ]; then
  chmod 600 "$VPS_PATH/server/plugins/floodgate/key.pem"
fi
if systemctl list-unit-files | grep -q '^lukybox.service'; then
  sudo systemctl restart lukybox
  sleep 5
  systemctl --no-pager status lukybox | head -15
else
  echo "AVISO: el servicio systemd 'lukybox' no existe todavia."
  echo "Copia lukybox.service a /etc/systemd/system/ y ejecuta:"
  echo "  sudo systemctl daemon-reload && sudo systemctl enable --now lukybox"
fi
REMOTE

echo "==> Listo."
echo "    Java    -> $VPS_HOST:25565"
echo "    Bedrock -> $VPS_HOST:19132 (UDP)"
echo "    Panel   -> tunel SSH, el puerto NO se abre en el firewall:"
echo "               ssh -p $SSH_PORT -N -L 8080:127.0.0.1:8080 $VPS_USER@$VPS_HOST"
echo "               y abre http://127.0.0.1:8080"
