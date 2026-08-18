# Deep Axiom

### Automatización que nunca deja de correr — y puede probar lo que hizo.

n8n, Zapier y Make disparan un trigger, ejecutan una cadena de pasos una vez y
terminan. Eso sirve para una sincronización nocturna. Se cae en el momento en que
el trabajo es *en vivo*: una conversación, un flujo de video, una base de datos
cambiando debajo de ti, un modelo respondiendo token por token.

Deep Axiom mantiene la conexión abierta. Los skills —LLMs, voz, bases de datos,
APIs de negocio— se conectan en grafos que corren como streams tipados, causales
y con contrapresión. Tres propiedades lo sostienen:

- **Streaming** — la unidad de trabajo es la conexión, no la ejecución. 0.42 ms por salto.
- **Concurrencia** — el throughput *sube* con la carga. 1,000 sesiones concurrentes, medidas.
- **Auditable** — cada efecto autorizado por política, firmado por un humano con
  nombre, y sellado en un ledger encadenado que se verifica sin conexión, anclado
  donde un tercero ya puede estar mirando. En el kernel, no en tu grafo.

Un binario. 23 MB. Sin cuenta, sin nube, sin Postgres, sin broker, sin clúster.

[Guía completa](GUIDE-ES.md) · [English](README.md) ·
[Estado de los hitos](GUIDE-ES.md#estado-de-los-hitos) ·
**v0.3.0 — pre-1.0, pre-producción**

---

## 60 segundos

```bash
git clone https://github.com/deepaxiom/aura && cd aura/kernel
go build -o aura ./cmd/aura     # Go 1.25+, sin CGO, sin servicios externos
./aura up                       # kernel, UI, store de estado, ledger, cliente de witness
```

`aura up` es el runtime completo y ningún skill: un proceso, un puerto, nada que
instalar al lado. Los skills son procesos aparte que se conectan *a* él, así que
un nodo recién arrancado es un kernel funcionando con el catálogo vacío. Dale
algo que correr:

```bash
pip install -r skills/llm-chat/requirements.txt
cd skills/llm-chat && PYTHONPATH=../../sdk/python/src python main.py
# el primer arranque descarga un GGUF local; con OPENAI_API_KEY usa uno en la nube
```

```bash
aura chat "explica la contrapresión"   # responde token por token
aura do "vigila la tabla de órdenes y mándame un mensaje cuando entre un reembolso mayor a $500"
```

Ese segundo comando lee el catálogo vivo, elige skills, compila un grafo y lo
ejecuta — con la escritura gateada porque actúa sobre el mundo. El grafo se queda
arriba: Postgres empuja los cambios de fila conforme se confirman, y nada sondea.

---

## 1 · Streaming

|  | Herramientas por lotes | Deep Axiom |
|---|---|---|
| Unidad de trabajo | Una ejecución: arranca, corre, se destruye | Una **conexión** que permanece abierta |
| Obtener datos | Sondear cada 5 minutos | La fuente **empuja**, conforme sucede |
| Un LLM respondiendo | Esperar la respuesta completa | Token por token; lo de abajo reacciona a media frase |
| Audio y video | Prácticamente no soportado | PCM ritmado, transcripciones parciales, barge-in funcional |
| Un paquete perdido | Frena todo lo que viene detrás (TCP) | Solo frena su propio carril (QUIC) |
| Cancelar | Al mejor esfuerzo | Garantía del kernel — la salida de una cadena cancelada no llega a ningún lado |

Texto, audio, documentos y eventos viajan como el mismo envelope tipado. Un nodo
sirve **QUIC (WebTransport)** en el mismo número de puerto que su listener TCP, y
las tres clases de QoS de C3 se vuelven tres primitivas reales de transporte:

| Clase | En el cable |
|---|---|
| `realtime` | Datagrama QUIC — no puede frenar, ni ser frenado por, otro frame |
| `reliable` | Un stream ordenado — FIFO por arista |
| `bulk` | Su propio stream por transferencia |

Eso cerró un hueco que el runtime cargaba en silencio: descartar el frame más
viejo de una cola responde a un *consumidor lento* y no hace nada ante una *red
con pérdida*, porque TCP reentrega en orden por debajo. También le da sentido a
`bulk` por primera vez. Un peer que no alcanza UDP conserva la ruta WebSocket.

**Los deadlines son absolutos y se heredan** — un salto puede apretarlos, nunca
extenderlos. **La ejecución especulativa** corre trabajo río abajo sobre salida
parcial y se rechaza al cablear cualquier arista hacia un skill `motor`.

---

## 2 · Concurrencia

Cada envelope se registra de forma durable antes de reconocerse. Eso significaba
una transacción por evento y una fila ante un solo lock de escritura: 200
sesiones concurrentes hacían *menos* trabajo total que una. El group commit —el
trato que PostgreSQL y RocksDB hacen desde hace décadas— mantiene que cada
llamador espere su propia durabilidad, pero quienes ya esperaban se suman a la
misma transacción.

Medido de punta a punta en una máquina de desarrollo, con cliente y skill en Go
para que el arnés no sea el límite:

| Sesiones concurrentes | Antes | Después |
|---|---|---|
| 1 | 1,556 msg/s · p50 0.47 ms | 1,504 msg/s · p50 0.58 ms |
| 50 | 365 msg/s · p50 96 ms | 1,631 msg/s · p50 19 ms |
| 200 | 344 msg/s · p50 489 ms | **1,908 msg/s · p50 63 ms** |
| 400 *(8 réplicas)* | — | **11,458 msg/s · p50 0.0 ms** |
| 1,000 *(8 réplicas)* | *no conectaban* | **28,202 msg/s · p50 0.51 ms** |

Un perfil de CPU encontró el resto, y no estaba donde nadie suponía: la
codificación JSON era el 1% del CPU, mientras que **el I/O de archivos de SQLite
era el 54%**. De ahí salieron dos correcciones. Subir el umbral de checkpoint del
WAL bajó `FlushFileBuffers` de 38% del CPU total a 13%. Después el log causal
salió de SQLite por completo.

**El log de eventos no es una tabla.** Se escribe una vez por envelope y nunca se
actualiza ni se borra, y se lee de vuelta como una sesión en orden — el mejor
caso para un log y el peor para un B-tree que paga por actualizaciones que nunca
ocurren. Ahora es un archivo de segmentos append-only con CRC por registro,
escrituras agrupadas e índice reconstruido desde ellos al arrancar; una cola
rota por un corte de energía se detecta y se trunca en vez de leerse como datos.
En la misma carga y con durabilidad igualada sostuvo 1.5M eventos/s contra los
27k de SQLite. La forma es la que argumenta
[Tidehunter](https://arxiv.org/abs/2602.01873): tratar el log como almacenamiento
permanente, y la compactación deja de existir porque nada se reubica.

Segmentar es lo que hace eso sostenible en un runtime que se supone no se detiene
nunca. Un solo archivo que solo crece no tiene forma de reclamar espacio que no
sea la compactación que este diseño existe para evitar, así que el log rota a los
128 MiB y `--event-log-max` reclama borrando segmentos enteros, del más viejo al
más nuevo — apagado por defecto, porque ese historial es lo que leen `aura why` y
`aura replay`. De la misma forma salen dos propiedades más: los registros se
enmarcan por segmento, así que un segmento alterado después de sellado cuesta su
propia cola y no todo lo que viene después; y el índice de locators está acotado,
con las sesiones más frías cayendo a escanear solo los segmentos donde aparecen,
así que la memoria la limita un flag y no cuánto lleva el nodo arriba. El banner
de arranque imprime el tamaño.

El ledger de efectos se queda en SQLite a propósito. Un bug en el log de eventos
pierde historial de replay; un bug en el ledger pierde evidencia.

Dos cosas más allá del batching: el registro de sesiones entró al mismo lote, y
la resolución ahora reparte sesiones entre réplicas de un skill — diez copias
dejaban nueve ociosas. Un panic al rutear ahora falla una sesión, no el nodo.

**Lo que esto no es.** Son números por nodo en una máquina; un nodo sigue siendo
un proceso sin failover. Escala Twitch significa decenas de miles de conexiones
por máquina en cientos de máquinas — y las plataformas a esa escala **no** sellan
cada evento en una cadena de hashes. Esto sí, a propósito. Ese es el costo del
pilar 3, y es el que este runtime no va a ceder.

Lee la tabla por la forma, no por los números absolutos, y sabiendo qué mide: un
skill echo sobre loopback, o sea el costo de ruteo y durabilidad del kernel sin
trabajo real en ninguna parte del lazo. El throughput agregado divide los
round-trips totales entre el tiempo de pared *incluyendo* lo que tarda en abrir
cada sesión, lo cual favorece a las filas de alta concurrencia — una corrida de
1,000 sesiones amortiza su setup de conexión sobre medio millón de mensajes, una
de 1 sesión sobre quinientos. La afirmación honesta es la que hacen solas las
filas del medio: con 200 sesiones el mismo nodo pasó de 344 msg/s a 1,908, y el
p50 de 489 ms a 63, sin debilitar la durabilidad. `kernel/cmd/loadgen/` reproduce
la tabla.

---

## 3 · Auditable

Nada de esto vive en tu grafo:

- **El gate de aprobación es un invariante del kernel.** En las librerías de
  agentes el interrupt vive en el código que escribiste, así que el código que se
  olvida no tiene gate. Aquí el executor lo aplica en el único punto por el que
  pasa toda entrega, guiado por la política del nodo. Un grafo puede pedir *más*
  supervisión de la que la política exige, nunca menos.
- **Un skill no es el operador.** `aura token issue --capability motor.erp.write`
  acuña una credencial que puede conectarse, registrarse como *esa* capability y
  gastar los recibos que reciba — y no puede registrar un grafo, leer el ledger
  ni inscribir un aprobador. Antes, cada proceso skill tenía el token del
  operador, lo que hacía del checkpoint una valla y no una frontera: un skill
  podía registrar un grafo que waiveara su propio gate y acuñar el recibo que
  compra una credencial. Una sola tabla ordenada decide qué alcanza cada scope,
  denegando por defecto, y el registro queda atado a la capability emitida.
- **La entry nombra al humano que lo aprobó, y él lo firmó.** No "un humano
  aprobó" — *cuál* humano, y demostrable. El operador firma una declaración
  atada a esa entrega concreta con una clave que el nodo nunca ha tenido, así
  que el aprobador no puede negarlo después y el nodo no puede fabricar una.
  Esa es la mitad de *"quién autorizó esto"* que todo audit trail se salta,
  porque la respuesta habitual — la palabra del propio nodo — no vale nada
  cuando el nodo es justamente lo que se está auditando.
- **Cada efecto se atestigua, no se registra.** Se sella en un registro
  encadenado por hash, comprometido en una cabeza Merkle RFC 6962 que el nodo
  firma y un tercero puede contrafirmar. `aura verify` recalcula cadena, árbol y
  firmas desde el archivo de base de datos solo, sin kernel corriendo. Sellar
  escribe a un disco, y los discos se llenan: por defecto un efecto que el nodo
  no pudo sellar se entrega igual y el hueco se registra a gritos, para que un
  disco lleno no sea una caída. Un nodo donde el ledger tiene que estar completo
  pone `on_seal_failure: refuse` y el efecto se detiene en su lugar. No hay una
  tercera opción, así que la elección es del operador y el banner de arranque
  dice cuál está en vigor.
- **Y ese tercero también rinde cuentas.** Un witness publica su propio log
  append-only, firma su cabeza, y firma su respuesta a *hasta dónde has
  respaldado a este nodo* — así que decirle una cosa a uno y otra a otro deja de
  ser indetectable y pasa a ser dos declaraciones firmadas que no pueden ser
  ambas ciertas. `aura witness audit` lo sigue y lo rechaza si reescribió algo.
  Los nodos se anclan por defecto en un witness público y gratuito, y con un
  flag pueden apuntar a cualquier otro; un recibo vale lo que valga para quien
  ya esté siguiendo el mismo ancla.
- **Cita el modelo que lo argumentó.** Un skill que corre inferencia atestigua
  motor, modelo, revisión, cuantización, parámetros de muestreo y semilla —
  atados a cada efecto que esa salida causó. Un cambio silencioso de modelo
  altera hashes ya comprometidos en una cadena append-only.
- **`aura undo`** revierte un efecto por un puerto de compensación declarado. El
  undo es a su vez gateado y sellado.

**¿Ya tienes agentes corriendo?** `aura guard` pone los servidores MCP que tu
agente ya llama detrás de este mismo checkpoint — una línea en la configuración
que ya tienes, sin reescribir nada. Una herramienta queda gateada salvo que su
servidor demuestre que solo lee *y* tú hayas decidido creerle. Vale exactamente
hasta donde llegue tu control sobre la configuración del agente; no hay
enforcement de red. [Detalles](GUIDE-ES.md#proteger-las-tools-de-un-agente).

**¿Y cuando controlar la config no alcanza?** Deja de intentar hacer imposible
el bypass y hazlo inútil. `aura secret set` guarda la credencial en el kernel en
vez de en el entorno del agente, y solo se libera contra el recibo de un efecto
que acaba de pasar el checkpoint — la capability correcta, entregado y no
denegado, de hace segundos. Saltarse el gate deja de ser una forma de evitar
escrutinio y pasa a ser una forma de conseguir un 401. Lo que no hace: impedir
que un skill que recibió legítimamente una credencial se la quede. Lo que sí
elimina: la credencial ambiente y permanente que estaba disponible para cada
llamada que el agente hiciera, gateada o no.
[Detalles](GUIDE-ES.md#el-broker-de-credenciales).

**¿Un auditor preguntando por un periodo?** `aura audit --since 2026-07-01 --out q3.json`
contesta la pregunta con la que realmente llega — qué actuó sobre el mundo, quién
autorizó cada cosa, bajo qué documento de policy, y si el registro fue editado —
con un recibo portable por cada efecto gateado. Los recibos prueban un efecto y
los bundles explican una sesión; ambos parten de algo que un ingeniero ya tiene.
Un rango de fechas es lo que nombra una obligación de cumplimiento, y
`aura audit --verify q3.json` verifica el documento entero sin base de datos, sin
nodo y sin red.

**¿Vas a cambiar de modelo?** `aura regress` reproduce tus sesiones grabadas
contra él y hace diff de los *efectos*, no de las transcripciones — así "cambió
la redacción" y "dejó de emitir el reembolso" dejan de ser el mismo resultado.
Una suite de evals puntúa salidas contra una rúbrica y no puede ver un acto que
dejó de ocurrir; esto sí, porque C5 ya ata la revisión del modelo al acto.
[Detalles](GUIDE-ES.md#regresión-contra-el-ledger).

---

## Conectar lo que ya tienes

```bash
aura connect --openapi ./crm.yaml   # cada operación se vuelve un skill
```

Solo lectura por defecto, con dry-run y promoción por operación antes de que algo
escriba. Los webhooks entran, los skills `sensorial` envuelven sistemas que
empujan, y los skills con inversión de control marcan *hacia fuera* para correr
detrás de NAT sin puertos de entrada. En las fronteras: un **servidor MCP** (cada
skill es una herramienta para Claude Code o Cursor, y `tools/call` hace
streaming), una tarjeta de descubrimiento A2A, y exportación OpenTelemetry del
árbol causal.

El aprobador firmado está escrito de la misma forma, como un
[Internet-Draft](spec/proposals/draft-signed-human-approval.md). El draft de
audit trail para agentes que ya existe en la IETF registra *que* un humano
intervino, con un id pseudónimo y sin firma, y firma los registros con la clave
del **agente** — así que la única evidencia de que un humano aprobó es la
palabra del sistema que está siendo auditado. Esa es justo la afirmación sobre
la que una auditoría no puede descansar, y el arreglo es pequeño: una clave que
el sistema que registra nunca tiene, un payload atado a una sola acción, y una
separación estricta entre "¿firmó esto?" (permanente) y "¿puede aprobar ahora?"
(mutable). El draft es independiente del transporte y del formato, y encaja
dentro del campo que ese draft ya reserva.

**Los nueve skills en [`skills/`](skills/) son demos.** Existen para mostrar la
forma de un skill y para darle a un nodo frío algo que correr — no para ser un
catálogo, y no para que dependas de ellos en producción; por eso llevan el org
`example/` en todos sus manifiestos. Léelos como la implementación de referencia
del manifiesto C1 y el protocolo de canal, copia el más cercano a lo que
necesitas, y reemplázalo. `postgres-cdc` es un lector CDC que funciona y aun así
es una demo: no tiene política de reintentos, ni rotación de credenciales, ni
manejo de cambios de esquema, porque esas son decisiones que toma tu despliegue y
un ejemplo no puede tomarlas por ti.

Un skill es cualquier proceso que hable el protocolo de canal y declare un
manifiesto: Python, TypeScript, Go, Rust, Wasm, o un envoltorio sobre software
que ya operas. Empieza por [`skills/echo/`](skills/echo/), unas 100 líneas. El
SDK es Apache-2.0, así que un skill que escribas y vendas no carga obligación de
copyleft, nunca.

---

## Dónde está parado, honestamente

| | |
|---|---|
| **Con pruebas** | Envelopes en streaming con QoS por arista sobre WebSocket y QUIC · el ledger y la verificación sin conexión · el gate como invariante del kernel · **identidad firmada del aprobador sellada en la entry** · **el broker de credenciales (un secreto solo contra un recibo válido, y nunca contra el waiver de un grafo)** · **el log propio del witness, y un monitor que atrapa a uno reescribiéndolo** · la cancelación · reanudación de sesión · replay determinista · **regresión a nivel de efectos entre sesiones** · puertos tipados con gramáticas de decodificación compiladas · la frontera MCP en ambos sentidos · `aura guard` · skills Wasm en un sandbox real · CDC de Postgres · **rotación, retención y recuperación de un segmento corrupto del log de eventos** |
| **Verificado a mano** | Voz con barge-in · el planner (`aura do`) · `aura why` · exportación OpenTelemetry · ML-BOM |
| **Todavía no** | Sin vista multidispositivo de una misma sesión viva · sin failover si el nodo muere · **sin aislamiento de procesos para skills `format: source`** — un skill corre con los privilegios de quien lo arrancó |

`internal/` está en ~72% de cobertura, `cmd/aura` en 7.5%, la UI no tiene. Una
suite de conformidad de 59 verificaciones ejercita el kernel sobre el cable, y un
job de CI aparte demuestra que el ledger detecta manipulación editando una base
de datos real a espaldas de un binario real. Lee [Modelo de
seguridad](GUIDE-ES.md#modelo-de-seguridad) antes de exponer un puerto, y [Estado
de los hitos](GUIDE-ES.md#estado-de-los-hitos) para la línea completa entre lo
probado y lo verificado a mano. Esto es pre-producción; trátalo como tal.

---

**Licencia.** El spec y los SDKs son Apache-2.0 — construye un kernel conforme,
escribe y vende skills, sin obligación de copyleft nunca. El kernel y la UI son
AGPLv3, con licencia comercial disponible en su lugar. Correr `aura` sin
modificar —tu laptop, tus servidores, dentro de tu empresa— no dispara ninguna
obligación AGPL. Ver [LICENSE.md](LICENSE.md) · Contribuciones:
[CONTRIBUTING.md](CONTRIBUTING.md).
