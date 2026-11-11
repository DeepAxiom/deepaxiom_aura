# AURA

### El sistema operativo de los robots autónomos.

Lo bastante rápido para estar dentro del lazo. Cada decisión trazable hasta lo
que la argumentó. Nada actúa sobre el mundo sin que el nodo lo haya revisado
antes.

[Inicio rápido](QUICKSTART-ES.md) · [Guía completa](GUIDE-ES.md) · [Roadmap](ROADMAP-ES.md) ·
[Estado de los hitos](GUIDE-ES.md#estado-de-los-hitos) · [English](README.md) ·
**v0.4.0 — pre-1.0, pre-producción**

<!-- UI-HERO -->
<p align="center"><img src="docs/media/studio-live.gif" alt="El plano de control: un grafo corriendo en vivo, tokens apareciendo en el nodo que los produce, una arista gateada hacia un Expertise motor" width="900"></p>
<!-- /UI-HERO -->

---

## Qué es, y qué es hoy

Una máquina que actúa por su cuenta tiene tres problemas. Percibir y moverse
tienen buenas respuestas desde hace años — el middleware del robot y sus
controladores. **Decidir** es más nuevo: los modelos son grandes, comparten un
acelerador y se reemplazan cada pocos meses. Y **responder por lo que hizo** es
el que nadie construye primero, lo cual es un error, porque un sistema que no
fue diseñado para emitir esa evidencia no puede hacerlo después sin reconstruir
cómo ejecuta.

AURA es la capa para los dos últimos. Un solo binario que corre los modelos que
deciden, lleva lo que ven y dicen como streams tipados en vivo, y se pone entre
cada decisión y el actuador.

**Lee esto antes que el resto.** Hoy es la capa de decisión, con la evidencia
incorporada. No ha conducido un robot físico en nada de lo que este repositorio
prueba, y el puente al middleware de un robot es el
[primer punto del roadmap](ROADMAP-ES.md#el-robot-en-sí), no algo que exista.
No es un controlador de tiempo real duro ni lo va a ser: el lazo de servo se
queda en el robot. Lo que AURA controla es lo que a ese controlador se le
*pide* hacer, y lo que el registro dice que hizo.

Todo lo de abajo está construido y probado, salvo donde dice lo contrario.

---

## 1 · Rápido

### Un sensor no termina

Las herramientas por lotes disparan un trigger, corren una cadena una vez y la
destruyen. Eso encaja con una sincronización nocturna y se rompe en cuanto el
trabajo es *vivo* — una cámara, una conversación, un modelo respondiendo token
a token.

<p align="center"><img src="docs/media/mechanism-connection.svg" alt="Una ejecución por lotes arranca, corre y se destruye; una conexión se queda abierta y los envelopes fluyen por el grafo conforme se producen" width="820"></p>

Texto, audio, fotogramas y eventos viajan como el mismo envelope tipado sobre
una conexión que se queda abierta. Un nodo sirve **QUIC (WebTransport)** en el
mismo número de puerto que su listener TCP, así que las tres clases de QoS son
tres primitivas de transporte reales: `realtime` es un datagrama que no puede
frenar ni ser frenado, `reliable` es un stream ordenado por arista, `bulk` tiene
el suyo. Un peer que no alcanza UDP conserva el camino WebSocket. **Los
deadlines son absolutos y se heredan** — un salto puede apretar uno, nunca
extenderlo — y una entrega que ya va tarde se descarta en vez de calcularse
para nadie.

### Una sesión lenta solo se frena a sí misma

Todas las sesiones a las que sirve un Expertise comparten su única conexión. El
kernel ruteaba cada envelope en línea, así que una sesión cuyo consumidor se
atrasaba frenaba a todas las demás de ese socket. Ahora cada sesión tiene su
propia cola.

<p align="center"><img src="docs/media/mechanism-mailbox.svg" alt="Con ruteo en línea una sesión bloqueada frena a las que vienen detrás en la misma conexión; con una cola por sesión solo espera la bloqueada" width="820"></p>

Las sesiones nuevas van a la réplica que menos sesiones vivas lleva, contadas
por el propio kernel. Y el `cancel` de un cliente no hace cola: adelanta a cada
token que todavía espera detrás, y el kernel los suprime — aquí la cancelación
es una garantía, no una petición.

### Menos envelopes para las mismas palabras

Un modelo emite un token cada pocos milisegundos, y cada uno era su propio
envelope — un id, dos codificaciones, dos appends al log, una decisión de
ruteo — para cuatro caracteres.

<p align="center"><img src="docs/media/mechanism-coalescing.svg" alt="Muchos envelopes de un token se vuelven unos pocos más grandes con el mismo texto; el primer token nunca se retrasa" width="820"></p>

El primer token sigue saliendo de inmediato; nada espera más de 25 ms. En un
stream rápido eso fue **cerca de 50 envelopes en vez de 120, con texto
idéntico**, en una corrida de punta a punta contra un nodo real.

### Qué está medido, y qué no

| | |
|---|---|
| Ruteo + durabilidad del kernel, un nodo, echo sobre loopback | 1 sesión: 1.504 msg/s, p50 0,58 ms · 200 sesiones: 1.908 msg/s, p50 63 ms (eran 344 msg/s, p50 489 ms antes del group commit). `kernel/cmd/loadgen` lo reproduce. |
| Log causal de eventos | 1,5M eventos/s sobre segmentos append-only contra los 27k de SQLite con durabilidad igualada. |
| Un nodo muerto reemplazado por su standby | 21–118 ms desde el kill hasta volver a contestar, Linux, mismo host. `scripts/failover_e2e.py`. |
| Colas por sesión y agrupado bajo carga | **No medido.** Probado en corrección; el generador de carga existente no tiene consumidor lento y es un envelope por mensaje, así que no muestra ninguno. |
| Percepción → decisión → efecto en un robot | **No medido.** Todavía no hay un robot en el lazo. |

---

## 2 · Cada decisión, trazable

<p align="center"><img src="docs/media/mechanism-gate-ledger.svg" alt="Una entrega hacia un Expertise motor queda retenida en el gate del kernel para una persona con nombre, y luego se sella en un ledger encadenado por hash" width="820"></p>

- **El gate es un invariante del kernel.** En las librerías de agentes la
  interrupción vive en el código que escribiste, así que el código que se
  olvida no tiene gate. Aquí el executor lo aplica en el único punto por el que
  pasa toda entrega, guiado por la policy del nodo. Un grafo puede pedir *más*
  escrutinio del que la policy exige, nunca menos.
- **La entry nombra al humano que lo aprobó, y él lo firmó.** Con una clave que
  el nodo nunca ha tenido, sobre digests de lo que realmente se le mostró — así
  el registro dice que una persona consintió *esto*, no que alguien hizo clic.
  Escrito como [Internet-Draft](spec/proposals/draft-signed-human-approval.md).
- **Cada efecto se sella, no se registra.** Una cadena de hashes, comprometida
  en una cabeza Merkle RFC 6962 que el nodo firma y un tercero puede
  contrafirmar. `aura verify` lo recalcula todo desde el archivo de base de
  datos solo, sin kernel corriendo.
- **Nombra el modelo que lo argumentó.** Motor, modelo, revisión, cuantización,
  parámetros de muestreo, semilla — y ahora los tokens que contó el motor —
  atados a cada efecto que esa salida causó. La sustitución silenciosa de
  modelos es un problema medido [[10]](#refs); aquí rompe hashes ya
  comprometidos en una cadena append-only. Sigue siendo la palabra del
  Expertise: una afirmación atada a sus consecuencias, no una prueba de lo que
  se ejecutó.
- **Se une a la traza que ya tienes.** Un `traceparent` W3C viaja en cada salto
  de la cadena que inicia quien llama — llevado por el kernel, así que
  sobrevive a un Expertise que nunca oyó hablar de él — y `aura up --otlp`
  exporta spans mientras el nodo corre. Se muestrean sesiones enteras, nunca
  árboles parciales, y los payloads nunca se exportan.

<p align="center"><img src="docs/media/mechanism-trace.svg" alt="El contexto de traza de quien llama viaja por cada salto dentro del nodo y de vuelta, así los spans del nodo se unen a su traza" width="820"></p>

Cuando alguien pregunta después, no llega con un hash. Llega con un rango de
fechas:

```bash
aura audit --since 2026-07-01 --out q3.json   # qué actuó, quién lo autorizó, bajo qué policy
aura audit --verify q3.json                   # cualquiera, en cualquier parte, sin nodo corriendo
aura why <session>                            # qué pasó, narrado desde el log causal
aura regress                                  # reproduce sesiones grabadas contra un modelo nuevo; compara los *efectos*
```

**La obligación ya está escrita.** El [Reglamento (UE) 2024/1689](https://artificialintelligenceact.eu/article/12/)
exige el registro automático de eventos (Artículo 12) y la supervisión efectiva
por personas físicas (Artículo 14) a los sistemas de IA de alto riesgo; sus
fechas de aplicación las movió el [Reglamento (UE) 2026/1744](https://eur-lex.europa.eu/eli/reg/2026/1744/oj)
al 2 de diciembre de 2027 para los sistemas del Anexo III y al 2 de agosto de
2028 para los embebidos en productos regulados. Se movió el calendario; el
texto no. ISO/IEC 42001 pide la misma cadena de evidencia hoy.

---

## 3 · Seguro

- **Lo que actúa sobre el mundo es un tipo.** Cada Expertise declara si
  percibe, razona, recuerda, transforma o actúa (`motor`). La policy deniega
  por defecto `motor.*`; la especulación se rechaza al cablear cualquier arista
  hacia uno.
- **Un Expertise no es el operador.** Una credencial acotada a una capability
  puede conectarse, registrarse como esa capability y gastar los recibos que
  reciba — y no puede registrar un grafo, leer el ledger ni inscribir un
  aprobador.
- **Un secreto se libera contra un recibo, no se tiene de forma ambiente.**
  Saltarse el gate deja de ser una forma de evitar el escrutinio y pasa a ser
  una forma de conseguir un 401.
- **Un espectador recibe un ticket para una sesión y nada más.** Firmado, de
  vida corta, válido en una sola ruta.
- **Los Expertises `format: wasm` corren en un sandbox WASI real.** Los
  `format: source` **no**: `--sandbox process` limpia el entorno y encierra el
  directorio de trabajo, y no contiene código hostil. Hasta que llegue la
  frontera de microVM, la posición honesta es *corre tus propios Expertises*,
  no *instala los de un desconocido*.
- **Los pesos en formato pickle se rechazan; las revisiones de modelo se
  fijan** antes de descargar, así que lo atestiguado es lo que se bajó.

Empieza aquí sin adoptar nada más — pon los servidores MCP que un agente ya usa
tras la policy, el gate y el ledger de este nodo:

```bash
aura guard --config claude_desktop_config.json
```

Se sostiene exactamente hasta donde llegue tu control sobre la configuración
del agente; no hay imposición en red. [Detalles](GUIDE-ES.md#proteger-las-tools-de-un-agente).

---

## 4 · Modelos: despliegue y optimización

Un robot es sobre todo un problema de despliegue de modelos con motores
pegados.

- **Un solo contrato para cada modelo.** Lenguaje, voz, visión — cualquier
  modelo que envuelva un Expertise — tras el mismo manifiesto y los mismos
  puertos, en local o en remoto. Cambiar uno cambia la colocación, no el grafo.
- **Un solo juego de pesos para todo el nodo.** Apunta todo Expertise de
  primera parte a un servidor compatible con OpenAI (`AURA_INFERENCE_URL`) y
  comparten sus slots, su batching y su caché de prefijo en vez de cargar cada
  uno una copia.
- **Un prompt cuyo prefijo sobrevive a la conversación.** El historial se
  recorta por bloques, así que una caché KV o la caché de prompts de un
  proveedor sigue siendo válida entre compactaciones en vez de invalidarse en
  cada turno.
- **El costo en tokens como lo contó el motor**, aciertos de caché incluidos,
  en la atestación y en `aura_tokens_total` — no una estimación a bytes/4.
- **Un planner que no puede inventar una capability**: su decodificador queda
  restringido por una gramática construida desde el catálogo vivo.
- **Ajuste antes de descargar.** `model-fit` estima qué puede correr una
  máquina; `model-manager` lo baja con la revisión fijada.

Todavía no: que el nodo sea dueño de ese servidor de inferencia y lo supervise,
cambiar un modelo sin reiniciar, producir un artefacto optimizado para un
dispositivo en vez de elegir entre los descargados, y planificar varios modelos
en un acelerador. El camino del servidor compartido está verificado contra un
endpoint falso, no uno real. [Roadmap](ROADMAP-ES.md#modelos-despliegue-y-optimización).

---

## 5 · Video

[`media/`](media/) es un segundo artefacto, `aura-media`, con su propia línea
de versión — codificar ocupa un núcleo durante minutos y aprobar un efecto no
puede hacer cola detrás de eso nunca.

- **Reproducible mientras codifica.** Los segmentos se publican conforme
  cierran; un activo tiene dirección segundos después de empezar, con progreso
  reportado.
- **Entra en vivo, sale HLS adaptativo.** Una transmisión RTMP se vuelve una
  escalera con una ventana DVR que pasa a ser una grabación normal cuando la
  transmisión termina; un emisor que se cae y se reconecta continúa la misma
  transmisión.
- **Fotogramas para modelos desde la misma decodificación que la
  codificación.**
- **Una audiencia sobre la sesión de al lado.** `GET /v1/watch` conecta
  observadores de solo lectura a una sesión viva — ninguno capaz de frenarla, a
  ninguno se le muestra un gate — con un ticket por espectador y el historial
  reciente reproducido al entrar.

Corrido de punta a punta contra ffmpeg real en Linux. No reproducido en un
reproductor real; ningún encoder por hardware ha codificado un fotograma; la
latencia en vivo está calculada, no medida. Para un robot la pieza que falta es
la otra dirección — una cámara hacia un modelo en decenas de milisegundos, cosa
que HLS nunca será. [Qué está construido y qué no](media/README.md).

---

## 6 · Un nodo que muere se reemplaza

```bash
aura up --data /var/lib/aura                 # el nodo
aura up --data /var/lib/aura --standby       # toma el relevo cuando el primero muere
```

El standby no abre nada que el nodo vivo esté escribiendo. En el mismo host ve
que el proceso del líder ya no existe en vez de esperar un arrendamiento, y
contesta como el *mismo nodo* — misma identidad, mismo ledger — en decenas de
milisegundos. Los Expertises se reconectan solos; un cliente que se reconecta
continúa su sesión desde el log causal. Lo que no sobrevive es perder la
máquina: el standby necesita el directorio de datos.
[Detalles](GUIDE-ES.md#lo-que-un-nodo-no-sobrevive).

---

<!-- UI-SECTION -->
## El plano de control

Viene dentro del binario — `aura up`, y abre el puerto. Cada pantalla de abajo
es la UI real contra un nodo corriendo. El tráfico sale de tres Expertises de
prueba pequeños, etiquetados como pruebas en pantalla — el que emite texto
reproduce un pasaje fijo y no es un modelo. Cómo se capturó
cada una está en [`docs/media/README.md`](docs/media/README.md).

**Studio.** Construye un grafo y míralo correr: los envelopes avanzan por las
aristas conforme se entregan, el texto aparece en el nodo que lo produce, y una
arista gateada hacia un Expertise `motor` se ve gateada.

<p align="center"><picture><source media="(prefers-color-scheme: dark)" srcset="docs/media/studio-gate-dark.png"><source media="(prefers-color-scheme: light)" srcset="docs/media/studio-gate-light.png"><img src="docs/media/studio-gate-dark.png" alt="Studio con una entrega retenida en el gate de la arista hacia un Expertise motor" width="900"></picture></p>

**Pulse.** Lo que el nodo está haciendo ahora: sesiones vivas, tokens de
entrada y salida como los contaron los motores, la tasa de acierto de la caché
de prefijo, una barra por réplica con las sesiones que lleva, colas de
despacho, observadores.

<p align="center"><img src="docs/media/pulse-load.gif" alt="Pulse: sesiones vivas, tasas de tokens, caché de prefijo, carga por réplica" width="900"></p>

**Sesiones como trazas.** Cada sesión es su árbol causal — qué causó qué,
cuándo, en qué puerto, a costa de cuántos tokens — con el id de traza al que se
unió.

<p align="center"><img src="docs/media/session-waterfall.gif" alt="Una sesión mostrada como cascada causal" width="900"></p>

**Evidencia.** El ledger, entry por entry: capability, decisión, quién aprobó,
bajo qué policy, y si la cadena verifica.

<p align="center"><picture><source media="(prefers-color-scheme: dark)" srcset="docs/media/evidence-ledger-dark.png"><source media="(prefers-color-scheme: light)" srcset="docs/media/evidence-ledger-light.png"><img src="docs/media/evidence-ledger-dark.png" alt="El ledger de efectos con su resultado de verificación" width="900"></picture></p>

---

<!-- /UI-SECTION -->

## Dónde está parado, honestamente

| | |
|---|---|
| **Con pruebas** | Envelopes en streaming con QoS por arista sobre WebSocket y QUIC · despacho por sesión y réplicas por carga · el gate como invariante del kernel · identidad firmada del aprobador sobre lo que se le mostró · el ledger y la verificación sin conexión · credenciales con alcance y el broker de credenciales · el log del witness y su monitor · cancelación · reanudación de sesión · relevo por standby · replay determinista y regresión a nivel de efectos · reportes de auditoría por periodo · contexto de traza y exportación de spans en vivo · uso de tokens reportado · observadores de solo lectura con tickets e historial · puertos tipados con gramáticas compiladas · la frontera MCP en ambos sentidos · Expertises Wasm en un sandbox real · CDC de Postgres · respaldo y restauración verificables · rotación de la clave de firma |
| **Verificado a mano** | Voz con barge-in · el planner (`aura do`) · `aura why` · ML-BOM |
| **Probado solo contra dobles** | El servidor de inferencia compartido · la caché de estado KV en proceso · la gramática del planner (no hay llama.cpp en CI) · los encoders de video por hardware (nunca corridos) |
| **Todavía no** | Un puente al middleware de un robot · un paro a nivel de nodo · tramas binarias para datos de sensores · sobrevivir a la pérdida de la máquina · contención para `format: source` · evidencia TEE más allá de `bound` · SDKs e imagen de contenedor publicados · binarios firmados de 0.4 |

Una suite de conformidad de 67 verificaciones ejercita el kernel sobre el
cable, y jobs de CI aparte demuestran que el ledger detecta manipulación
editando una base de datos real a espaldas de un binario real, que un token con
alcance no puede actuar como el operador, y que un nodo arrancado **con su auth
por defecto** se puede conducir de punta a punta. Lee [Modelo de
seguridad](GUIDE-ES.md#modelo-de-seguridad) antes de exponer un puerto, y [el
roadmap](ROADMAP-ES.md) para lo que falta y por qué va en ese orden. Esto es
pre-producción; trátalo como tal.

**Sobre el nombre.** Lo que las versiones anteriores llamaban *skill* es un
**Expertise**: una función tipada, en ejecución, experta en una sola cosa — no
un prompt que se le entrega a un modelo. Todo identificador anterior sigue
funcionando (`skill.yaml`, `/ws/skill`, `/v1/skills`, `Skill`).

---

## Un registry que alojas tú, y Expertises que son tuyos

Los Expertises se distribuyen por un registry **federable** — cualquiera aloja uno
con `aura registry serve`, exactamente como un registry de contenedores. Eso es
lo que hace que la neutralidad del ecosistema sea algo que puedes comprobar en
vez de algo que prometemos.

```bash
aura registry serve                      # aloja un registry, en su propio puerto
aura publish my-skill/                   # zip + firma (Ed25519) + subida
aura add --capability sensorial.ocr      # descubre por capability, no por nombre
aura run acme/vision/invoice-ocr         # arráncalo contra el nodo local
```

Forzado y con pruebas: **versiones inmutables** (republicar una versión con
contenido distinto se rechaza), **trust-on-first-use** (la primera publicación
ata un id de paquete a la clave de su publicador, y una versión posterior firmada
con otra clave no puede secuestrarlo), y una revisión de permisos antes de que
nada aterrice.

**Hay un registry público arriba.** `https://registry.deepaxiom.com` sirve la
misma API `r1` que `aura registry serve`, con el mismo código. Apunta la CLI con
`--registry` o `AURA_REGISTRY`:

```bash
export AURA_REGISTRY=https://registry.deepaxiom.com
aura add acme/vision/invoice-ocr         # verifica hash + firma, revisa permisos, instala
curl -s https://registry.deepaxiom.com/r1/health   # {"api":"r1","ok":true,"registry":"aura"}

# publicar necesita una credencial de publicador; viaja en la URL
AURA_REGISTRY=https://publisher:PASS@registry.deepaxiom.com aura publish mi-skill/
```

Leer es público; escribir no. Todo método mutante va tras basic auth en el
borde, porque el trust-on-first-use del registry ata al *primer* par de llaves
que publica un id de forma permanente, sin rotación ni puerta administrativa —
abierto, cualquiera podría apropiarse de `deepaxiom/*/*` para siempre. La
credencial de publicador se entrega a petición (`info@deepaxiom.com`) hasta que
las cuentas de Studio puedan emitirlas por desarrollador.

**DeepAxiom Studio** ya está en `https://studio.deepaxiom.com` como primer
corte de solo lectura: un frente web para explorar lo que hay en el registry,
descargar una versión y un directorio de quién construye qué — publicar desde
el navegador y las cuentas vienen después. Corre en el mismo VPS que el registry y,
a propósito, fuera de su frontera de confianza: lee el registry solo por la API
HTTP pública de arriba, nunca por disco, que es donde viven los vínculos
trust-on-first-use. No confundir con la **vista Studio** del plano de control del
propio nodo, que es donde construyes y corres grafos; comparten nombre, no cosa.

**El SDK es Apache-2.0**, así que un Expertise que escribas y vendas no carga
obligación de copyleft, nunca. Dado el hueco de aislamiento de arriba, el pitch
honesto de hoy es *publica y aloja los tuyos* y no *instala código de
desconocidos* — la distribución, la firma y el descubrimiento son reales y están
probados; el sandbox que haría seguro un catálogo público todavía no está.

**Los diez Expertises en [`expertise/`](expertise/) son demos.** Existen para mostrar la
forma de un Expertise y darle a un nodo frío algo que correr — no para ser un
catálogo, y no para que dependas de ellos en producción; por eso llevan el org
`example/` en todos sus manifiestos. Copia el más cercano y reemplázalo. Empieza
por [`expertise/echo/`](expertise/echo/), unas 100 líneas.

---

---

## Correrlo al lado de tu app

La integración son seis líneas. El despliegue es un proceso, y esa distinción es
la que decide si esto encaja en tu stack:

```js
import { createNode } from "@deepaxiom/aura";
const aura = createNode({ org: "acme", app: "shop" });

aura.expose("get-order",    ({id}) => db.orders.find(id),   { params: ["id"] });
aura.expose("refund-order", ({id}) => db.orders.refund(id), { params: ["id"], write: true });

await aura.start();
```

`write: true` es la única línea aquí que trata de seguridad, y es una declaración
y no una implementación: el executor gatea esa llamada en toda arista que la
alcance —incluso en un grafo cuyo autor nunca lo pidió— y sella el efecto en el
ledger. CI lo demuestra exactamente así contra un binario real; ver
[`examples/expose-app/`](examples/expose-app/).

```bash
docker compose up      # el kernel, con ledger persistente, al lado de tu app
```

El contenedor es distroless, non-root y sin CGO, y `STOPSIGNAL SIGTERM` con
entrypoint en forma exec no es boilerplate: sin eso la señal nunca llega al
kernel, `docker stop` se convierte en un SIGKILL diez segundos después, y el
orden de cierre del store se salta en cada deploy. El nodo drena en una ventana
acotada y lo dice.

**El directorio de datos no es una caché.** Tiene la identidad del nodo, el
ledger de efectos y los secretos cifrados del broker — y la clave del broker se
*deriva* de la identidad, así que un restore sin `identity/` produce texto
cifrado que nadie puede abrir. Respáldalo como una base de datos.

Dos endpoints que un orquestador necesita:

| | |
|---|---|
| `GET /readyz` | Abierto, porque una probe no tiene credencial. Más estricto que `/healthz`: responde solo cuando el store y el ledger son usables, así un rolling deploy no manda tráfico a un nodo que está arriba pero no funciona. |
| `GET /metrics` | Texto Prometheus, **autenticado** — las series incluyen conteos de sesiones vivas, tamaño del ledger y la cola de aprobaciones pendientes. `aura_store_batch_mean` es la de capacidad: cerca del tope del batch significa que el commit es tu techo y más escritores no ayudarían. |

**Lo que esto no sobrevive es perder la máquina.** A un nodo que muere lo
reemplaza un standby en el mismo host (sección 6); una máquina que muere se
lleva el directorio de datos. Contesta eso con honestidad antes de desplegar:
si esta máquina desaparece, ¿tu app degrada o se detiene? Si se detiene, lee
primero [el roadmap](ROADMAP-ES.md).

**Los SDKs están empaquetados y sin publicar.** `@deepaxiom/aura` y `aura-sdk`
existen, compilan y tienen tests; todavía no hay `npm install` para ellos, así que
hoy se copian desde el repo.

## Conectar lo que ya tienes

```bash
aura connect --openapi ./crm.yaml   # cada operación se vuelve un skill
```

De solo lectura por defecto, con dry-run y promoción por operación antes de que
nada escriba. Los webhooks entran, los Expertises `sensorial` envuelven sistemas que
empujan, y los Expertises de inversión de control marcan *hacia afuera* para correr
detrás de NAT sin puertos de entrada. En las fronteras: un **servidor MCP** (cada
Expertise es una herramienta para Claude Code o Cursor, y `tools/call` hace
streaming), una tarjeta de descubrimiento A2A, y exportación OpenTelemetry del
árbol causal.

Una llamada a herramienta MCP vuelve con los recibos de lo que hizo — capability,
decisión, resultado, y el humano que lo firmó — en `_meta`, así la evidencia viaja
con la acción en vez de estar en un log que alguien tiene que correlacionar
después. Esa forma está escrita como
[una propuesta a MCP](spec/proposals/mcp-effect-receipts.md).

---

## 60 segundos

**0.4 todavía no tiene release firmada** — compílalo desde el código, más
abajo. Los binarios firmados para Linux, macOS y Windows en amd64 y arm64 siguen
siendo los de la
[release v0.3.0](https://github.com/DeepAxiom/deepaxiom_aura/releases/tag/v0.3.0). Verifica antes de correr — la firma
sobre `SHA256SUMS` demuestra quién construyó el archivo, y solo entonces vale la
pena comprobar la suma ([cómo](SECURITY.md#verifying-a-release)):

```bash
curl -LO https://github.com/DeepAxiom/deepaxiom_aura/releases/download/v0.3.0/aura-linux-amd64
curl -LO https://github.com/DeepAxiom/deepaxiom_aura/releases/download/v0.3.0/SHA256SUMS{,.sig,.pem}
cosign verify-blob --certificate-identity-regexp \
  "^https://github.com/DeepAxiom/deepaxiom_aura/.github/workflows/release.yml@.*$" \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  --signature SHA256SUMS.sig --certificate SHA256SUMS.pem SHA256SUMS
sha256sum -c SHA256SUMS --ignore-missing
chmod +x aura-linux-amd64 && ./aura-linux-amd64 up   # kernel, UI, store de estado, ledger, cliente de witness
```

O por el camino largo, desde el código:

```bash
git clone https://github.com/DeepAxiom/deepaxiom_aura && cd deepaxiom_aura/kernel
go build -o aura ./cmd/aura     # Go 1.25+, sin CGO, sin servicios externos
./aura up
```

Un binario, 18,6 MB — la compilación stripped de la página de release y del
contenedor; un `go build` local sin stripping ronda los 26. Sin cuenta, sin
nube, sin Postgres, sin broker, sin clúster. `aura up` es el runtime completo y ningún
Expertise: los Expertises son procesos aparte que se conectan *a* él, así que un nodo
recién arrancado es un kernel funcionando con el catálogo vacío y cuatro grafos
sembrados — `echo`, `chat`, `plan` y `voice` — a los que apuntar uno.

```bash
pip install -r expertise/llm-chat/requirements.txt
cd expertise/llm-chat && PYTHONPATH=../../sdk/python/src python main.py
aura chat "explica la contrapresión"    # responde token por token
```

---

<a id="refs"></a>

## Referencias

Donde este README afirma algo sobre el estado del arte, esto es en lo que se
apoya. Varios de estos describen el mismo problema que nosotros y lo resuelven de
otra forma — ese es justamente el motivo de listarlos.

1. Kumar et al., *Model Context Protocol Threat Modeling and Analyzing Vulnerabilities to Prompt Injection with Tool Poisoning* — [arXiv:2603.22489](https://arxiv.org/abs/2603.22489). STRIDE/DREAD sobre los componentes de MCP; los metadatos de herramientas son la superficie de ataque principal del lado cliente.
2. *Parasites in the Toolchain: A Large-Scale Analysis of Attacks on the MCP Ecosystem* — [arXiv:2509.06572](https://arxiv.org/abs/2509.06572). Por qué `aura guard` trata una herramienta sin anotar como si actuara sobre el mundo.
3. Silvestre et al., *Failure Transparency in Stateful Dataflow Systems* — [arXiv:2407.06738](https://arxiv.org/abs/2407.06738). La propiedad de corrección de la que la reanudación de sesión es un caso.
4. *Oversight Has a Capacity: Calibrating Agent Guards to a Subjective, Fatiguing Human* — [arXiv:2606.08919](https://arxiv.org/abs/2606.08919). El mejor argumento contra gatear de más, y la razón de que decida la policy y no el grafo.
5. *When Agents Handle Secrets: A Survey of Confidential Computing for Agentic AI* — [arXiv:2605.03213](https://arxiv.org/abs/2605.03213).
6. *CapSeal: Capability-Sealed Secret Mediation for Secure Agent Execution* — [arXiv:2604.16762](https://arxiv.org/abs/2604.16762). Convergencia independiente sobre la forma del broker de credenciales.
7. *Right to History: A Sovereignty Kernel for Verifiable AI Agent Execution* — [arXiv:2602.20214](https://arxiv.org/abs/2602.20214). Logs RFC 6962 más fronteras por capability, en un kernel en Rust.
8. *Notarized Agents: Receiver-Attested Confidential Receipts for AI Agent Actions* — [arXiv:2606.04193](https://arxiv.org/abs/2606.04193). Firma del lado receptor y logs contrafirmados por witness; otro corte al mismo problema de evidencia.
9. Syta et al., *Keeping Authorities "Honest or Bust" with Decentralized Witness Cosigning* — [arXiv:1503.08768](https://arxiv.org/abs/1503.08768). El origen del argumento de que un witness al que se puede pillar contradiciéndose no necesita ser creído.
10. Cai et al., *Are You Getting What You Pay For? Auditing Model Substitution in LLM APIs* — [arXiv:2504.04715](https://arxiv.org/abs/2504.04715). Sustituciones silenciosas de modelo, medidas en producción.
11. *Confidential LLM Inference: Performance and Cost Across CPU and GPU TEEs* — [arXiv:2509.18886](https://arxiv.org/abs/2509.18886). De dónde sale la cifra de 4–8%.
12. *Do Agent Benchmarks Measure Capability? Protocol Validity in the Age of Agentic AI* — [arXiv:2607.22368](https://arxiv.org/abs/2607.22368). La forma de audit bundle que implementa `aura bundle`.
13. Chursin et al., *Tidehunter: Large-Value Storage With Minimal Data Relocation* — [arXiv:2602.01873](https://arxiv.org/abs/2602.01873). Tratar el log como almacenamiento permanente; la compactación deja de existir.

Fuera de arXiv, y de carga: [RFC 6962](https://www.rfc-editor.org/rfc/rfc6962)
(Certificate Transparency), [RFC 8032](https://www.rfc-editor.org/rfc/rfc8032)
(Ed25519), el [Reglamento (UE) 2024/1689](https://artificialintelligenceact.eu/article/12/)
(el Reglamento de IA) y el [Reglamento (UE) 2026/1744](https://eur-lex.europa.eu/eli/reg/2026/1744/oj)
(el Digital Omnibus sobre IA, que aplazó las fechas de alto riesgo citadas arriba
y dejó por lo demás intactos los artículos 12 y 14).

---

**Licencia.** La especificación y los SDKs son Apache-2.0 — construye un kernel
conforme, escribe y vende Expertises, sin obligación de copyleft nunca. Eso incluye
las partes que una segunda implementación más necesitaría: el contrato del
ledger, el protocolo de witness, y una suite de conformidad de 67 verificaciones
con la que demostrar el cumplimiento. Un verificador vale menos cuantas menos
cosas puede verificar, y un ancla vale menos cuantas menos partes acuden a ella —
ninguna de las dos es buena cosa para poseer. El kernel y la UI son AGPLv3, con
una licencia comercial disponible en su lugar. Correr `aura` sin modificar — tu
laptop, tus servidores, dentro de tu empresa — no dispara ninguna obligación
AGPL. Ver [LICENSE.md](LICENSE.md) · Contribuciones:
[CONTRIBUTING.md](CONTRIBUTING.md).
