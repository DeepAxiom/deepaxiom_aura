# Inicio rápido

De cero a un nodo corriendo con un skill conectado, en tres terminales. Cada
comando de aquí se ejecutó de punta a punta en una máquina limpia.

[English version](QUICKSTART.md) · [Guía completa](GUIDE-ES.md) · [README](README-ES.md)

**Necesitas:** Go 1.25+ y Python 3.11+. Sin CGO, sin Docker, sin base de datos,
sin cuenta.

---

## 1 · Compila y arranca el nodo

```bash
cd kernel
go build -o aura ./cmd/aura
./aura up
```

Imprime un banner. Dos líneas importan:

```
  ui        http://localhost:9080
  open      http://localhost:9080/#token=FM-cEnU-zwwENsS0iBb3Xy2SeQ...
```

**Abre la URL de `open` en el navegador.** Ese es el plano de control — el
lienzo, la referencia, el chat, las sesiones. El token viaja en el fragmento
porque los navegadores nunca envían el fragmento al servidor, así que no acaba en
los logs de acceso; la página lo guarda y lo borra de la barra de direcciones.

El nodo ata `127.0.0.1` y escribe su token en `~/.aura/node.token`. Deja esta
terminal corriendo.

---

## 2 · Conecta un skill

**Un nodo recién arrancado tiene el catálogo vacío.** `aura up` es el kernel y
ningún skill: los skills son procesos aparte que se conectan *a* él. No va a
correr nada hasta que uno lo haga.

```bash
cd skills/echo
PYTHONPATH=../../sdk/python/src python main.py
```

Deberías ver `registered:` en la salida. Sin configurar ningún token — el SDK lee
`~/.aura/node.token` igual que el CLI.

> Ejecútalo desde el directorio del propio skill. El SDK carga `skill.yaml` desde
> el directorio de trabajo, así que arrancarlo desde la raíz del repo no
> encuentra el manifiesto.

Deja esta terminal corriendo también.

---

## 3 · Háblale

```bash
./kernel/aura status
```

```
skills connected: 1
  · example/logical/echo                     logical    logical.echo
```

```bash
./kernel/aura chat --graph echo "hola aura"
```

```
hola aura
```

Ese viaje de ida y vuelta fue cliente → kernel → executor de grafos → skill →
vuelta, sobre un WebSocket, con cada envelope registrado de forma durable antes
de ser reconocido.

---

## Qué hacer después

| | |
|---|---|
| **Dibujar un grafo** | Abre la vista **Lienzo**. Arrastra un skill de la paleta, cablea `client.text_out` a su entrada y regístralo. |
| **Leer la referencia** | La vista **Referencia** tiene todos los comandos, los cinco contratos y todos los esquemas — offline, sin red. |
| **Poner tu app detrás** | [`examples/expose-app/`](examples/expose-app/) son seis líneas: expone dos funciones que ya existen, marca una con `write: true`, y el kernel la gatea y la sella. |
| **Proteger un agente que ya corres** | `aura guard --config claude_desktop_config.json` pone tus servidores MCP detrás de un checkpoint. Sin levantar ningún runtime. |
| **Ver qué se selló** | `aura verify` recalcula la cadena de hashes y las firmas del ledger solo desde el archivo de base de datos, sin kernel corriendo. |

---

## Cosas que te van a morder

**Los flags van antes del mensaje.**

```bash
./kernel/aura chat --graph echo "hola"    # sí
./kernel/aura chat "hola" --graph echo    # el flag se ignora
```

**`aura chat` sin `--graph` usa el grafo `chat`, que necesita un LLM.** Sin uno
falla con `no connected skill provides capability "cognitive.llm.chat"`. Para eso
arranca [`skills/llm-chat/`](skills/llm-chat/) — descarga un modelo. El grafo
`echo` es el que funciona en frío.

**En PowerShell de Windows** las variables de entorno tienen otra sintaxis:

```powershell
$env:PYTHONPATH = "../../sdk/python/src"; python main.py
```

**No pasa nada / `skills connected: 0`.** Mira la terminal del skill. Si dice
`401`, no encontró credencial — ver abajo. Si dice `ConnectionRefused`, el nodo
no está arriba.

---

## Credenciales, en un párrafo

Un nodo genera un token bearer y todas las rutas están detrás de él, `/ws/skill`
incluida. El CLI y ambos SDKs miran en los mismos dos sitios, en orden:
`AURA_TOKEN` y luego `~/.aura/node.token`. Mismo usuario, misma máquina,
directorio de datos por defecto — no hay nada que configurar.

Solo tienes que pensarlo cuando alguna de esas cosas no se cumple:

```bash
# el nodo guarda sus datos en otro sitio
./aura up --data /srv/aura
export AURA_TOKEN=$(cat /srv/aura/node.token)

# o, para una máquina de un solo usuario en la que no quieres pensarlo
./aura up --no-auth
```

`--no-auth` es solo loopback e imprime una advertencia. Está bien para
desarrollo local y mal para cualquier otra cosa — ver [Modelo de
seguridad](GUIDE-ES.md#modelo-de-seguridad).

---

## Correrlo como servicio

```bash
docker compose up
```

El contenedor es distroless, non-root y sin CGO. Dos endpoints que un
orquestador quiere: `GET /readyz` (abierto, responde solo cuando el store y el
ledger son usables) y `GET /metrics` (Prometheus, autenticado).

**El directorio de datos no es una caché.** Contiene la identidad del nodo, el
ledger de efectos y los secretos cifrados del broker — y la clave del broker se
*deriva* de la identidad, así que una restauración sin `identity/` produce texto
cifrado que nadie puede abrir. Respáldalo como una base de datos.

**Esto es pre-1.0.** Un proceso, sin failover: si el nodo muere, el estado de
ruteo vivo muere con él mientras el log de eventos sobrevive. Contesta eso antes
de desplegar — si tu app se degrada, esto es desplegable hoy; si se detiene, lee
primero [el roadmap](ROADMAP-ES.md).
