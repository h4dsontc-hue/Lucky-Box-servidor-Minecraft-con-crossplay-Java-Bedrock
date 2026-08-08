# Lucky Box — servidor Minecraft con crossplay Java + Bedrock

Servidor **Paper 26.2** con Lucky Blocks jugables desde **Java y Bedrock** a la vez.

## Por qué plugin y no el mod

El mod original Lucky Block (Forge/Fabric) **no sirve** aquí. Geyser sí corre sobre
Fabric y NeoForge, pero traduce *protocolo*, no *contenido*. Del FAQ oficial de GeyserMC:

> *"Currently, there is no way for Geyser to translate the features that most mods add
> (blocks, items, etc.). Servers that require mods to be installed clientside are
> unsupportable through Geyser."*

Un bloque del mod no existe en Bedrock, así que esos jugadores no lo verían. La salida
sería mapear a mano cada ítem con `enable-custom-content` más un resource pack de Bedrock:
frágil y se rompe en cada actualización.

**ntdLuckyBlock** evita el problema de raíz: dibuja el lucky block con un diseño de cristal
usando **bloques vanilla**. Su propio lema es *"Say goodbye to the sponge, player head and
resourcepack"*. Sin resource pack, sin mods en el cliente, idéntico en ambas ediciones.

## Componentes

| Componente | Versión | Función |
|---|---|---|
| Paper | 26.2 (build 99) | Servidor. SHA256 verificado en la descarga |
| Geyser-Spigot | 2.11.1 | Traduce el protocolo Bedrock ↔ Java |
| Floodgate | 2.2.5 | Deja entrar a Bedrock sin cuenta de Java |
| ntdLuckyBlock | 2.8.35 | Lucky Blocks, 18 colores, sin resource pack |
| FastAsyncWorldEdit | 2.15.3 | Requerido por 3 drops de tipo schematic |
| Panel | en este repo | Administración por web: consola, jugadores, configuración y backups |

Se eligió ntdLuckyBlock tras comparar todo el catálogo de SpigotMC, Modrinth, Hangar y
CurseForge. Los dos plugins más descargados (219k y 85k) están abandonados en 1.15 y 1.19.
Éste tiene 84k descargas, 4.9★ y build de junio de 2026.

## Uso en local

Con el panel, que es como se administra normalmente:

```bash
cd panel && go run .        # abre http://127.0.0.1:8080
```

O arrancando Paper a pelo, sin panel:

```bash
cd server && ./start.sh
```

Conectar: Java en `localhost:25565`, Bedrock en `localhost` puerto `19132`.

La RAM se ajusta con variables: `MEM_MIN=4G MEM_MAX=8G ./start.sh`
(en el panel, con `-mem-min 4G -mem-max 8G`).

Los dos caminos no se mezclan: el panel solo puede parar y ver la consola del
servidor **que ha arrancado él**. Si dejas uno corriendo con `./start.sh` y luego
abres el panel, éste lo detecta y avisa, pero no podrá controlarlo.

## Panel de administración

Un binario de Go sin dependencias externas que lleva la interfaz web embebida.
Arranca `server/start.sh` como proceso hijo y se queda con su `stdin`, y de ahí
sale todo lo demás: mandar comandos a Paper sin habilitar RCON, leer la consola
en vivo y pararlo con `stop` para que guarde el mundo antes de morir.

| Pestaña | Qué hace |
|---|---|
| Consola | Consola en vivo por SSE, con filtro, y envío de comandos |
| Estado | CPU, RAM y TPS del servidor, disco, y ping real a los puertos Java y Bedrock |
| Jugadores | Conectados ahora, op/deop, kick, ban, whitelist. Marca quién entra por Bedrock |
| Configuración | Editor de los `.yml`/`.properties`/`.json` del servidor, con validación y copia `.bak` |
| Backups | `tar.gz` del mundo en caliente: `save-off` → `save-all flush` → tar → `save-on` |

Opciones (todas tienen un valor por defecto razonable):

```
-addr 127.0.0.1:8080   donde escucha
-server /ruta/server   directorio del servidor (por defecto busca ./server hacia arriba)
-backups /ruta         donde deja los tar.gz (por defecto <raíz>/backups)
-mem-min 2G -mem-max 4G   RAM que se pasa a start.sh
-autostart             arranca Paper al abrir el panel
-mirror                repite la consola de Paper en la salida del panel (activado)
```

Los backups viven **fuera** de `server/` a propósito: `deploy.sh` sincroniza ese
directorio con `--delete` y se los llevaría por delante.

### El panel no tiene contraseña

No hay login. Quien llegue al puerto puede parar el servidor, editar la
configuración y descargarse el mundo. Por eso escucha en `127.0.0.1` y **su
puerto no se abre nunca en el firewall**: al panel del VPS se llega por un túnel
SSH (más abajo). Si lo arrancas en una dirección que no sea local, el panel
avisa por consola pero no te lo impide.

## Despliegue al VPS

```bash
VPS_HOST=tu.ip.del.vps VPS_USER=minecraft ./deploy.sh
```

`deploy.sh` **no** pisa el mundo ni `ops.json` / bans / whitelist del VPS: esos datos viven
allí y sobreviven a cada despliegue. Sincroniza jars y configuración, compila el panel
para Linux y lo sube. Necesita `go` en tu máquina; en un VPS ARM, `VPS_ARCH=arm64 ./deploy.sh`.

El VPS queda con la misma disposición que el repo, y eso importa: el panel busca
`server/start.sh` a partir de su directorio de trabajo y deja los backups en `../backups`,
donde el `rsync --delete` no los alcanza.

```
/opt/lukybox/
├── server/     <- ./server, sincronizado con --delete
├── panel       <- binario compilado desde ./panel (lleva la web dentro)
└── backups/    <- tar.gz del mundo, deploy.sh no los toca
```

### Preparar el VPS la primera vez

```bash
sudo apt update && sudo apt install -y openjdk-25-jre-headless rsync
sudo useradd -r -m -d /opt/lukybox minecraft
sudo mkdir -p /opt/lukybox && sudo chown minecraft:minecraft /opt/lukybox
```

Luego el servicio:

```bash
sudo cp lukybox.service /etc/systemd/system/
sudo systemctl daemon-reload && sudo systemctl enable --now lukybox
sudo journalctl -u lukybox -f
```

El servicio arranca **el panel**, y es el panel quien lanza Paper (`-autostart`).
Con `-mirror` activado, `journalctl -u lukybox` sigue mostrando la consola de Paper
igual que cuando el servicio ejecutaba `start.sh` directamente.

`TimeoutStopSec=150` no es un número al azar: al parar, el panel manda `stop` y le da
90 s a Paper para guardar el mundo, más 30 s tras un `SIGTERM`. Con menos margen systemd
lo mataría a mitad de guardado. Y `KillMode=mixed` hace que el `SIGTERM` llegue sólo al
panel; si no, systemd señalaría también a la JVM y las dos paradas competirían.

Si vienes de un despliegue anterior, con `start.sh` en la raíz de `/opt/lukybox`,
`deploy.sh` se para y te dice los comandos exactos para mover el mundo a `server/`.
No mueve datos por su cuenta.

### Firewall

Los dos puertos del juego son obligatorios. **19132 es UDP**, no TCP — es el fallo
más común:

```bash
sudo ufw allow 25565/tcp   # Java
sudo ufw allow 19132/udp   # Bedrock
```

El puerto del panel **no se abre**. No tiene contraseña: se llega por un túnel SSH,
que además lo deja cifrado y sin exponer nada.

```bash
ssh -N -L 8080:127.0.0.1:8080 minecraft@tu.ip.del.vps
```

Con el túnel abierto, el panel del VPS está en `http://127.0.0.1:8080` de tu navegador.

## Configuración aplicada

En `server.properties`:

| Ajuste | Valor | Motivo |
|---|---|---|
| `enforce-secure-profile` | `false` | **Obligatorio.** Bedrock no soporta el chat firmado de Java 1.19+; con `true` no pueden chatear ni entrar |
| `spawn-protection` | `0` | Por defecto son 16 bloques donde nadie puede romper nada — mataría el juego junto al spawn |
| `difficulty` | `normal` | Los drops hostiles del lucky block tienen sentido |
| `online-mode` | `true` | Se mantiene. Floodgate exceptúa a Bedrock sin abrir el servidor a pirateo |

En `plugins/Geyser-Spigot/config.yml`: `auth-type: floodgate` (venía en `online`, que
habría exigido cuenta de Java a los jugadores de Bedrock).

## Cómo se juega

`/lb` (o `/luckyblock`) es el comando principal. Para darte lucky blocks:

```
/lb give <jugador> <color> <cantidad>
```

Los drops se editan en `server/plugins/ntdLuckyBlock/`.

## Aviso de seguridad

`server/plugins/floodgate/key.pem` es un **secreto compartido**. Si se filtra, cualquiera
puede suplantar jugadores de Bedrock. No lo subas a un repositorio público; `deploy.sh` lo
deja en modo `600` en el VPS.

`server.properties` contiene `management-server-secret`, que Paper genera solo. En el
repo va **vacío** a propósito: Paper lo regenera al arrancar y así no se publica una
credencial. Hoy no se usa (`management-server-enabled=false`), pero lo sería en cuanto
alguien active el management server.

Los ficheros de jugadores (`ops.json`, bans, whitelist, `usercache.json`) están en
`.gitignore`: la copia buena vive en el VPS —`deploy.sh` se niega a pisarlos— y llevan
nombres y UUIDs de gente real.

El panel no lista ni sirve `key.pem`, y su editor sólo abre ficheros de texto de
configuración dentro de `server/`. Pero **no tiene login**: trátalo como acceso de
administrador total y no lo saques de `127.0.0.1` (ver arriba).
