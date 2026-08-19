# Deep Axiom

### Despliega asistentes y automatizaciones que corren en vivo — y puede demostrar qué hicieron.

[Inicio rápido](QUICKSTART-ES.md) · [Guía completa](GUIDE-ES.md) · [English](README.md) ·
[Estado de los hitos](GUIDE-ES.md#estado-de-los-hitos) · [Roadmap](ROADMAP-ES.md) ·
**v0.3.0 — pre-1.0, pre-producción**

---

## Empieza sin adoptar nada

Tu agente ya llama a servidores MCP. Ponlos detrás de un checkpoint con un
comando — sin runtime que levantar, sin puerto que elegir, sin reescribir nada:

```bash
aura guard --config claude_desktop_config.json
```

```
  no node on port 9080 — running an embedded kernel
  ledger    every guarded call is sealed here

  TOOL               CAPABILITY                   ON CALL
  probe/read_thing   motor.mcp.probe.read_thing   human approval + sealed
  probe/write_thing  motor.mcp.probe.write_thing  human approval + sealed

  2 of 2 act on the world and are gated; the rest are read-only.
```

Cada llamada a herramienta que haga el agente pasa ahora por una policy que tú
controlas, se detiene ante un humano si actúa sobre el mundo, y aterriza en un
ledger encadenado por hash que se verifica sin conexión. Una herramienta está
gateada salvo que su servidor demuestre que solo lee **y** tú decidas creerle —
el default es estricto porque `readOnlyHint` es una afirmación del mismo servidor
del que trata la llamada, y los metadatos de herramientas son la superficie de
ataque documentada [[1]](#refs)[[2]](#refs): un análisis a gran escala del
ecosistema MCP encuentra que el envenenamiento a nivel de descriptor es la
vulnerabilidad de cliente más frecuente, y que la mayoría de los clientes la
validan de forma insuficiente.

La trampa, dicha de entrada: esto se sostiene exactamente hasta donde llegue tu
control sobre la configuración del agente. No hay imposición en red.
[Detalles](GUIDE-ES.md#proteger-las-tools-de-un-agente).

---

## 1 · Despliega en tiempo real

Cuando quieras más que un checkpoint, el mismo binario es un runtime. Describe lo
que necesitas; lee el catálogo vivo, elige skills, compila un grafo y lo deja
corriendo:

```bash
aura do "vigila la tabla de órdenes y mándame un mensaje cuando entre un reembolso mayor a $500"
```

La escritura queda gateada porque actúa sobre el mundo. El grafo se queda arriba:
Postgres empuja los cambios de fila conforme se confirman, y nada sondea.

**La conexión es la unidad de trabajo, no la ejecución.** n8n, Zapier y Make
disparan un trigger, corren una cadena una vez, y terminan. Eso encaja con una
sincronización nocturna y se rompe en cuanto el trabajo es *vivo* — una
conversación, un feed de video, una base de datos cambiando debajo, un modelo
respondiendo token a token.

|  | Herramientas por lotes | Deep Axiom |
|---|---|---|
| Unidad de trabajo | Una ejecución: arranca, corre, se destruye | Una **conexión** que se queda abierta |
| Obtener datos | Sondear cada 5 minutos | La fuente **empuja**, conforme ocurre |
| Un LLM respondiendo | Esperar la respuesta completa | Token a token; lo de abajo reacciona a media frase |
| Audio / video | No soportado de verdad | PCM ritmado, transcripciones parciales, barge-in real |
| Un paquete perdido | Frena todo lo que viene detrás (TCP) | Frena solo su propio carril (QUIC) |
| Cancelar | Al mejor esfuerzo | Garantía del kernel — la salida de una cadena cancelada no llega a ningún lado |

Texto, audio, documentos y eventos viajan como el mismo envelope tipado. Un nodo
sirve **QUIC (WebTransport)** en el mismo número de puerto que su listener TCP,
así que las tres clases de QoS se vuelven tres primitivas de transporte reales:
`realtime` es un datagrama que no puede frenar ni ser frenado, `reliable` es un
stream ordenado por arista, `bulk` tiene el suyo propio. Un peer que no alcanza
UDP conserva el camino WebSocket sin cambios. **Los deadlines son absolutos y se
heredan** — un salto puede apretarlos, nunca extenderlos.

Un socket caído se reanuda: la ventana de deduplicación, los índices causales,
los gates pendientes y los contadores por salto se reconstruyen desde el log de
eventos, haya muerto el cliente o el kernel. Esa es la propiedad que la
literatura de dataflow con estado llama transparencia ante fallos [[3]](#refs), y
es la que un asistente *vivo* no puede no tener — no existe "vuelve a correr el
job" para una conversación.

### Vivo significa concurrente, y la concurrencia tiene que ayudar

Cada envelope se registra de forma duradera antes de ser confirmado. Eso
significaba una transacción por evento encolada en un único lock de escritura —
200 sesiones concurrentes hacían *menos* trabajo total que una. El group commit
(el trato que PostgreSQL y RocksDB hicieron hace décadas) mantiene a cada
llamador esperando su propia durabilidad mientras todos los que ya esperaban se
suman a la misma transacción.

| Sesiones concurrentes | Antes | Después |
|---|---|---|
| 1 | 1.556 msg/s · p50 0,47 ms | 1.504 msg/s · p50 0,58 ms |
| 50 | 365 msg/s · p50 96 ms | 1.631 msg/s · p50 19 ms |
| 200 | 344 msg/s · p50 489 ms | **1.908 msg/s · p50 63 ms** |
| 1.000 *(8 réplicas)* | *no conectaba* | **28.202 msg/s · p50 0,51 ms** |

Lee la tabla por la forma, no por los números absolutos, y sabiendo qué mide: un
skill echo sobre loopback, o sea el costo de ruteo y durabilidad del kernel sin
trabajo real en el lazo. El throughput agregado divide round-trips entre tiempo
de pared *incluyendo* el tiempo de conexión, lo cual favorece a las filas de alta
concurrencia. La afirmación honesta es la de la fila del medio: con 200 sesiones
el mismo nodo pasó de 344 msg/s a 1.908 y el p50 de 489 ms a 63, sin debilitar la
durabilidad. `kernel/cmd/loadgen/` lo reproduce.

Un profile de CPU encontró el resto, y no donde nadie suponía: la codificación
JSON era el 1% del CPU mientras que **la E/S de archivos de SQLite era el 54%**.
El log causal de eventos salió de SQLite hacia archivos de segmentos append-only
con CRC por registro y escrituras agrupadas — 1,5M eventos/s contra los 27k de
SQLite con durabilidad igualada, la forma que argumenta
[Tidehunter](https://arxiv.org/abs/2602.01873) [[13]](#refs). Rota a los 128 MiB;
`--event-log-max` reclama segmentos enteros del más viejo al más nuevo. El ledger
de efectos se queda en SQLite a propósito: un bug en el log de eventos pierde
historial de replay, un bug en el ledger pierde evidencia.

---

## 2 · Auditable — porque ya está especificado

La obligación está escrita y fechada. La fecha se movió; el texto no.

**El [Reglamento (UE) 2024/1689](https://artificialintelligenceact.eu/article/12/)
— el Reglamento de IA — exige esto a los sistemas de IA de alto riesgo desde el
2 de diciembre de 2027.** Esa fecha era el 2 de agosto de 2026 hasta que el
[Reglamento (UE) 2026/1744](https://eur-lex.europa.eu/eli/reg/2026/1744/oj), el
Digital Omnibus sobre IA, la aplazó — los sistemas autónomos del Anexo III al 2
de diciembre de 2027, los sistemas embebidos del Anexo I al 2 de agosto de 2028.
En vigor desde el 27 de julio de 2026.

**Lo que el Omnibus movió fue el calendario, no el requisito.** Los artículos 12
y 14 sobreviven a la enmienda con su contenido intacto, y hablan directamente de
lo que un runtime tiene que emitir:

- El **Artículo 12** exige el registro *automático* de eventos durante todo el
  ciclo de vida del sistema, al servicio de la identificación de riesgos
  (Art. 79), la vigilancia poscomercialización (Art. 72) y la supervisión por
  parte del responsable del despliegue (Art. 26(5)). Los responsables del
  despliegue deben conservar esos logs al menos seis meses.
- El **Artículo 14** exige que el sistema pueda ser supervisado de forma efectiva
  por **personas físicas** mientras está en uso.

Así que este README no te va a decir que el mundo se acaba en quince días. El
argumento para construir la vía de evidencia ahora es más estrecho y, creemos,
mejor: **un sistema que no fue diseñado para emitir esta evidencia no puede
hacerlo después sin reconstruir cómo ejecuta.** Un rastro de auditoría es una
propiedad de la ruta de ejecución, no una función atornillada a un costado — que
es justamente la razón por la que el gate de más abajo es un invariante del
kernel y no una llamada de librería. Meter eso a posteriori en un producto en
marcha es la versión cara de este trabajo, y el aplazamiento es la ventana en la
que la versión barata sigue disponible.

**Y un reloj no se movió.** **ISO/IEC 42001** (cláusula 9.2) pide la misma cadena
de evidencia para auditoría interna, está en vigor hoy, es certificable ya, y
cada vez más es una condición de compra antes que una exigencia regulatoria. Las
normas armonizadas que operacionalizarán el Artículo 12 — prEN 18229-1, ISO/IEC
DIS 24970 — siguen en borrador, lo que significa que la forma de la evidencia se
está decidiendo durante el aplazamiento, no antes de él.

Esto es lo que produce, y nada de ello vive en tu grafo:

- **El gate de aprobación es un invariante del kernel.** En las librerías de
  agentes la interrupción vive en el código que escribiste, así que el código que
  se olvida no tiene gate. Aquí el executor lo aplica en el único punto por el
  que pasa toda entrega, guiado por la policy del nodo. Un grafo puede pedir
  *más* escrutinio del que la policy exige, nunca menos — y donde la policy sí
  deja que un grafo waivee un gate, la entry registra que fue el *grafo* quien lo
  excusó y no el nodo, porque un grafo es un JSON que puede publicar cualquiera
  que alcance la superficie de control.
- **La entry nombra al humano que lo aprobó, y él lo firmó.** El Artículo 14 pide
  supervisión de una persona física; un log que dice "un humano aprobó" no la
  evidencia. El operador firma una declaración atada a esa entrega concreta con
  una clave que el nodo nunca ha tenido, así que el aprobador no puede negarlo
  después y el nodo no puede fabricarla. Esa es la mitad de *"quién autorizó
  esto"* que todo audit trail se salta — incluido el propio
  [draft de audit trail para agentes](https://datatracker.ietf.org/doc/draft-sharif-agent-audit-trail/)
  de la IETF, que registra un id de operador pseudónimo sin firma y firma los
  registros con la clave del *agente*. Escribimos el arreglo como un
  [Internet-Draft](spec/proposals/draft-signed-human-approval.md).
  Una advertencia que conviene leer antes de confiar en cualquier gate, el
  nuestro incluido: el revisor no es un oráculo infinitamente disponible, y
  calibrar *qué* acciones detener frente a un humano subjetivo y que se fatiga es
  un problema abierto [[4]](#refs). Un gate que salta demasiado es un gate que
  acaba sellándose sin leer.
- **Un skill no es el operador.** `aura token issue --capability motor.erp.write`
  acuña una credencial que puede conectarse, registrarse como *esa* capability y
  gastar los recibos que reciba — y no puede registrar un grafo, leer el ledger
  ni inscribir un aprobador. Una sola tabla ordenada decide qué alcanza cada
  scope, denegando por defecto.
- **La credencial de un skill se libera contra un recibo, no se tiene de forma
  ambiente.** `aura secret set` pone la credencial en el kernel; se libera solo
  contra el recibo de un efecto que acaba de pasar el checkpoint. Saltarse el
  gate deja de ser una forma de evitar el escrutinio y pasa a ser una forma de
  conseguir un 401. Esa misma forma sellada por capability es a la que converge
  la literatura de computación confidencial para agentes [[5]](#refs)[[6]](#refs)
  — un agente comprometido o un prompt filtrado nunca deberían ver una clave en
  claro.
- **Cada efecto se atestigua, no se registra.** Se sella en un registro
  encadenado por hash, comprometido en una cabeza Merkle RFC 6962 que el nodo
  firma y un tercero puede contrafirmar. `aura verify` recalcula cadena, árbol y
  firmas desde el archivo de base de datos solo, sin kernel corriendo. Los discos
  se llenan: por defecto un efecto que el nodo no pudo sellar se entrega igual y
  el hueco se registra a gritos, y un nodo donde el ledger tiene que estar
  completo pone `on_seal_failure: refuse`. El banner de arranque dice cuál rige.
  Aplicar la construcción de Certificate Transparency a la ejecución de agentes
  es una idea convergente, no solo nuestra [[7]](#refs)[[8]](#refs).
- **Y ese tercero también rinde cuentas.** Un witness publica su propio log
  append-only, firma su cabeza, y firma su respuesta a *hasta dónde has
  respaldado a este nodo* — así que decirle una cosa a uno y otra a otro pasa a
  ser dos declaraciones firmadas que no pueden ser ambas ciertas. El argumento de
  fondo — que una autoridad a la que se puede pillar contradiciéndose no necesita
  ser creída — tiene una década y sigue siendo el correcto [[9]](#refs).
- **Cita el modelo que argumentó a favor, y dice cuánto creerle.** Un skill
  atestigua engine, modelo, revisión, cuantización, parámetros de muestreo y
  semilla, atados a cada efecto que esa salida causó. La sustitución silenciosa
  de modelos es un problema documentado y medido en APIs de LLM desplegadas
  [[10]](#refs), y esto hace que rompa hashes ya comprometidos en una cadena
  append-only. Pero sigue siendo la palabra del skill. Donde el hardware puede
  estrecharlo, el nonce del quote TEE **es** el hash de esa declaración concreta
  — así que la evidencia es sobre *este* registro y no al lado, y editar la
  declaración después lo rompe. Un verificador reporta `none`, `bound` o
  `verified`, nunca un booleano. El costo ya es tolerable — 4–8% de throughput en
  H100 con confidential compute, decreciendo con el batch [[11]](#refs) — que es
  por qué el campo se mueve y por qué conviene leer sus propias revisiones antes
  de creerle a cualquier vendor.
- **`aura undo`** revierte un efecto por un puerto de compensación declarado. El
  undo está gateado y sellado a su vez.

**Cuando llega el auditor**, no trae un hash de efecto ni un id de sesión. Trae un
rango de fechas:

```bash
aura audit --since 2026-07-01 --out q3.json   # qué actuó, quién lo autorizó, bajo qué policy
aura audit --verify q3.json                   # cualquiera, en cualquier parte, sin nodo corriendo
```

El reporte lleva los conteos, los documentos de policy distintos que estuvieron en
vigor, un desglose por capability y por operador firmante, y un recibo portable
por cada efecto gateado. `aura bundle` exporta una sesión como los materiales
retenidos que un segundo lector necesita para re-derivar una atribución en vez de
aceptarla — trayectoria, procedencia de artefactos con hashes, configuración del
modelo — la forma de cuatro partes que argumenta un análisis de protocolos de
evaluación de agentes tras encontrar que la mayoría de las trazas no sostienen
las conclusiones que se extraen de ellas [[12]](#refs). `aura bom` emite un ML-BOM
CycloneDX 1.6 de los modelos y skills que realmente corrieron, construido desde
el ledger y no desde la configuración.

**¿Vas a cambiar de modelo?** `aura regress` reproduce tus sesiones grabadas
contra él y diffea los *efectos*, no las transcripciones — así "cambió la
redacción" y "dejó de emitir el reembolso" dejan de ser el mismo resultado.

---

## 3 · Dónde está parado, honestamente

Lee esto antes de la siguiente sección, porque la siguiente sección te invita a
correr código de otras personas.

| | |
|---|---|
| **Con pruebas** | Envelopes en streaming con QoS por arista sobre WebSocket y QUIC · el ledger y la verificación sin conexión · el gate como invariante del kernel · **identidad firmada del aprobador sellada en la entry** · **credenciales de skill con alcance** · **el broker de credenciales** · **el log propio del witness, y un monitor que atrapa a uno reescribiéndolo** · cancelación · reanudación de sesión · replay determinista · **regresión a nivel de efectos** · **reportes de auditoría por periodo que se verifican solos** · puertos tipados con gramáticas compiladas · la frontera MCP en ambos sentidos · `aura guard` · skills Wasm en un sandbox real · CDC de Postgres · rotación y recuperación del log de eventos |
| **Verificado a mano** | Voz con barge-in · el planner (`aura do`) · `aura why` · exportación OpenTelemetry · ML-BOM |
| **Todavía no** | Sin vista multidispositivo de una misma sesión viva · **sin failover si el nodo muere** — un proceso, y el estado de ruteo vivo se va con él · la evidencia TEE llega a `bound`, nunca a `verified` — la verificación de cadena del fabricante está declarada y rechazada, no stubbeada · los SDKs están empaquetados pero sin publicar · sin procedimiento documentado de backup/restore, ni de rotación de la clave del nodo |

**El hueco que más importa para lo que sigue: un skill `format: source` no está
contenido.** `--sandbox process` limpia su entorno, encierra su directorio de
trabajo y comprueba el egreso declarado antes de arrancar, lo cual frena fugas
accidentales de credenciales y paseos casuales por el sistema de archivos. No
frena código hostil en absoluto. `format: wasm` **sí** está genuinamente
aislado, en un sandbox WASI real. Una frontera de verdad para skills source
significa un microVM, que está declarado y rechazado al arrancar en vez de
degradado en silencio.

`internal/` está en 72% de cobertura, `cmd/aura` en 7,5%, y la UI está probada en
su capa de modelo — el modelo de grafos del lienzo contra los propios vectores de
conformidad C2 del kernel — pero no en sus componentes. Una
suite de conformidad de 59 verificaciones ejercita el kernel sobre el cable, y
jobs de CI aparte demuestran que el ledger detecta manipulación editando una base
de datos real a espaldas de un binario real, que un token con alcance no puede
actuar como el operador, que el contenedor llega a healthy sobre un volumen
vacío y drena con SIGTERM, y que un nodo arrancado **con su auth por defecto** se
puede conducir de punta a punta — ese último existe porque todos los demás
carriles arrancan con `--no-auth`, y por ese hueco se colaron tres bugs de
credenciales. Lee [Modelo de
seguridad](GUIDE-ES.md#modelo-de-seguridad) antes de exponer un puerto, y [el roadmap](ROADMAP-ES.md) para lo que falta y a
quién le bloquea. Esto es pre-producción; trátalo como tal.

---

## 4 · Un registry que alojas tú, y skills que son tuyas

Los skills se distribuyen por un registry **federable** — cualquiera aloja uno
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

**El SDK es Apache-2.0**, así que un skill que escribas y vendas no carga
obligación de copyleft, nunca. Dado el hueco de aislamiento de arriba, el pitch
honesto de hoy es *publica y aloja los tuyos* y no *instala código de
desconocidos* — la distribución, la firma y el descubrimiento son reales y están
probados; el sandbox que haría seguro un catálogo público todavía no está.

**Los diez skills en [`skills/`](skills/) son demos.** Existen para mostrar la
forma de un skill y darle a un nodo frío algo que correr — no para ser un
catálogo, y no para que dependas de ellos en producción; por eso llevan el org
`example/` en todos sus manifiestos. Copia el más cercano y reemplázalo. Empieza
por [`skills/echo/`](skills/echo/), unas 100 líneas.

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

**Lo que esto no sobrevive es que el nodo muera.** Un proceso, sin failover; el
log de eventos sobrevive, el estado de ruteo vivo no. Contesta eso con honestidad
antes de desplegar: si AURA se cae, ¿tu app degrada o se detiene? Si degrada, esto
es desplegable hoy. Si se detiene, lee primero [el roadmap](ROADMAP-ES.md).

**Los SDKs están empaquetados y sin publicar.** `@deepaxiom/aura` y `aura-sdk`
existen, compilan y tienen tests; todavía no hay `npm install` para ellos, así que
hoy se copian desde el repo.

## Conectar lo que ya tienes

```bash
aura connect --openapi ./crm.yaml   # cada operación se vuelve un skill
```

De solo lectura por defecto, con dry-run y promoción por operación antes de que
nada escriba. Los webhooks entran, los skills `sensorial` envuelven sistemas que
empujan, y los skills de inversión de control marcan *hacia afuera* para correr
detrás de NAT sin puertos de entrada. En las fronteras: un **servidor MCP** (cada
skill es una herramienta para Claude Code o Cursor, y `tools/call` hace
streaming), una tarjeta de descubrimiento A2A, y exportación OpenTelemetry del
árbol causal.

Una llamada a herramienta MCP vuelve con los recibos de lo que hizo — capability,
decisión, resultado, y el humano que lo firmó — en `_meta`, así la evidencia viaja
con la acción en vez de estar en un log que alguien tiene que correlacionar
después. Esa forma está escrita como
[una propuesta a MCP](spec/proposals/mcp-effect-receipts.md).

---

## 60 segundos, por el camino largo

```bash
git clone https://github.com/deepaxiom/aura && cd aura/kernel
go build -o aura ./cmd/aura     # Go 1.25+, sin CGO, sin servicios externos
./aura up                       # kernel, UI, store de estado, ledger, cliente de witness
```

Un binario, 18,6 MB — la compilación Linux stripped que viaja en el contenedor;
un `go build` local sin stripping ronda los 26. Sin cuenta, sin nube, sin
Postgres, sin broker, sin clúster. `aura up` es el runtime completo y ningún
skill: los skills son procesos aparte que se conectan *a* él, así que un nodo
recién arrancado es un kernel funcionando con el catálogo vacío y cuatro grafos
sembrados — `echo`, `chat`, `plan` y `voice` — a los que apuntar uno.

```bash
pip install -r skills/llm-chat/requirements.txt
cd skills/llm-chat && PYTHONPATH=../../sdk/python/src python main.py
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
conforme, escribe y vende skills, sin obligación de copyleft nunca. Eso incluye
las partes que una segunda implementación más necesitaría: el contrato del
ledger, el protocolo de witness, y una suite de conformidad de 59 verificaciones
con la que demostrar el cumplimiento. Un verificador vale menos cuantas menos
cosas puede verificar, y un ancla vale menos cuantas menos partes acuden a ella —
ninguna de las dos es buena cosa para poseer. El kernel y la UI son AGPLv3, con
una licencia comercial disponible en su lugar. Correr `aura` sin modificar — tu
laptop, tus servidores, dentro de tu empresa — no dispara ninguna obligación
AGPL. Ver [LICENSE.md](LICENSE.md) · Contribuciones:
[CONTRIBUTING.md](CONTRIBUTING.md).
