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
- **Auditable** — cada efecto autorizado por política, gateado y sellado en un
  ledger encadenado que se verifica sin conexión. En el kernel, no en tu grafo.

Un binario. 23 MB. Sin cuenta, sin nube, sin Postgres, sin broker, sin clúster.

[Guía completa](GUIDE-ES.md) · [English](README.md) · [Roadmap](ROADMAP.md) ·
**v0.3.0 — pre-1.0, pre-producción**

---

## 60 segundos

```bash
git clone https://github.com/deepaxiom/aura && cd aura/kernel
go build -o aura ./cmd/aura     # Go 1.25+, sin CGO, sin servicios externos
./aura up                       # UI, LLM local, store de estado, ledger — todo
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
escrituras agrupadas e índice reconstruido desde el archivo al arrancar; una cola
rota por un corte de energía se detecta y se trunca en vez de leerse como datos.
En la misma carga y con durabilidad igualada sostuvo 1.5M eventos/s contra los
27k de SQLite. La forma es la que argumenta
[Tidehunter](https://arxiv.org/abs/2602.01873): tratar el log como almacenamiento
permanente, y la compactación deja de existir porque nada se reubica.

El ledger de efectos se queda en SQLite a propósito. Un bug en el log de eventos
pierde historial de replay; un bug en el ledger pierde evidencia.

Dos cosas más allá del batching: el registro de sesiones entró al mismo lote, y
la resolución ahora reparte sesiones entre réplicas de un skill — diez copias
dejaban nueve ociosas. Un panic al rutear ahora falla una sesión, no el nodo.

**Lo que esto no es.** Son números por nodo en una máquina; un nodo sigue siendo
un proceso sin failover. Escala Twitch significa decenas de miles de conexiones
por máquina en cientos de máquinas — y las plataformas a esa escala **no** sellan
cada evento en una cadena de hashes. Esto sí, a propósito. Ese es el costo del
pilar 3, y es el que este runtime no va a ceder. `kernel/cmd/loadgen/` reproduce
la tabla.

---

## 3 · Auditable

Nada de esto vive en tu grafo:

- **El gate de aprobación es un invariante del kernel.** En las librerías de
  agentes el interrupt vive en el código que escribiste, así que el código que se
  olvida no tiene gate. Aquí el executor lo aplica en el único punto por el que
  pasa toda entrega, guiado por la política del nodo. Un grafo puede pedir *más*
  supervisión de la que la política exige, nunca menos.
- **Cada efecto se atestigua, no se registra.** Se sella en un registro
  encadenado por hash, comprometido en una cabeza Merkle RFC 6962 que el nodo
  firma y un tercero puede contrafirmar. `aura verify` recalcula cadena, árbol y
  firmas desde el archivo de base de datos solo, sin kernel corriendo.
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

Los nueve skills en [`skills/`](skills/) son **ejemplos de referencia, no un
catálogo** — por eso llevan el org `example/`. Un skill es cualquier proceso que
hable el protocolo de canal y declare un manifiesto: Python, TypeScript, Go,
Rust, Wasm, o un envoltorio sobre software que ya operas. Copia
[`skills/echo/`](skills/echo/), unas 100 líneas. El SDK es Apache-2.0, así que un
skill que escribas y vendas no carga obligación de copyleft, nunca.

---

## Dónde está parado, honestamente

| | |
|---|---|
| **Con pruebas** | Envelopes en streaming con QoS por arista sobre WebSocket y QUIC · el ledger y la verificación sin conexión · el gate como invariante del kernel · la cancelación · reanudación de sesión · replay determinista · puertos tipados con gramáticas de decodificación compiladas · la frontera MCP en ambos sentidos · `aura guard` · skills Wasm en un sandbox real · CDC de Postgres |
| **Verificado a mano** | Voz con barge-in · el planner (`aura do`) · `aura why` · exportación OpenTelemetry · ML-BOM |
| **Todavía no** | Sin vista multidispositivo de una misma sesión viva · sin failover si el nodo muere · **sin aislamiento de procesos para skills `format: source`** — un skill corre con los privilegios de quien lo arrancó |

`internal/` está en 72% de cobertura, `cmd/aura` en 9.5%, la UI no tiene. Una
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
