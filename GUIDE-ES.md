# Deep Axiom — un runtime de streaming para skills de IA

**Estado: v0.3.0 — pre-1.0, pre-producción, Fase 1 de 2 hacia la beta abierta.** · **Construido sobre la arquitectura de kernel AURA · [Apache-2.0 (spec y SDK) · AGPLv3 o comercial (kernel) — ver LICENSE.md](LICENSE.md)** · [English version](GUIDE.md) · [← README](README-ES.md)

Deep Axiom es un runtime de código abierto para componer skills — LLMs, voz,
bases de datos, APIs de negocio — en grafos que corren como streams tipados y vivos.
Un único binario contiene el kernel, la UI del plano de control, el almacén de
estado, el bus de mensajes y el ledger de efectos; no necesita cuenta, ni nube,
ni base de datos externa. Los grafos se escriben a mano o los genera un planner
a partir de un objetivo en lenguaje natural, y cada sesión guarda un log de
eventos causal append-only que puedes explicar o reproducir después.

Cuatro decisiones de diseño lo separan de las herramientas de workflows (n8n,
Zapier, Make) y de los frameworks de agentes:

- **La unidad de trabajo es la conexión, no la ejecución.** Los mensajes son
  envelopes sobre un stream tipado, ordenado, causal y con contrapresión, sobre
  un socket pensado para quedarse abierto — no pasos de un lote que arranca,
  corre y se destruye.
- **Dos propiedades de seguridad viven en el kernel, no en userland.** Toda
  arista hacia un skill que actúa sobre el mundo (`motor.*`) lleva la puerta que
  decidió la política del nodo — no el autor del grafo —, aplicada por el
  executor, así que un grafo escrito a mano no puede saltársela; y el kernel
  suprime lo que llegue de una cadena cancelada, así que un skill que ignore el
  cancel produce salida que no va a ninguna parte.
- **Cada efecto se sella en un ledger encadenado y firmado — no se registra
  como log, se atestigua.** Un nodo autoriza un efecto por política, lo
  registra en una cadena donde alterar una entrada vieja rompe todos los
  hashes posteriores, se compromete a una cabeza Merkle RFC 6962, y la firma.
  Un **witness** externo puede contrafirmarla, que es lo que descarta que el
  propio operador del nodo reescriba la historia. Cualquier efecto suelto se
  exporta como **recibo portátil** que cualquiera verifica sin conexión — sin
  base de datos, sin nodo, sin red.
- **El ledger registra qué *argumentó* un acto, no solo quién lo autorizó.**
  Un skill que corre un modelo lo atestigua — motor, modelo, revisión de
  Hugging Face, cuantización, parámetros de muestreo, semilla — y el kernel
  liga ese registro a cada efecto al que la salida llevó causalmente. Así una
  entrada sellada responde "sobre qué base pasó esto", y un cambio silencioso
  de modelo altera hashes ya comprometidos en una cadena de solo anexado. Es
  una *afirmación* ligada de forma infalsificable a sus consecuencias, no una
  prueba de qué se ejecutó; ver
  [Modelo de seguridad](#modelo-de-seguridad), que lo explica en detalle.

**Lo que todavía no es.** No hay vista multidispositivo de una sesión viva, ni
failover si muere el nodo propietario — el estado de una sesión sobrevive a un
socket caído, pero no a que desaparezca el proceso del nodo. Un nodo se autentica
— loopback por defecto, token bearer, comprobación de origen del WebSocket, TLS
opcional — pero todos los llamadores comparten un solo token de nodo, así que un
skill comprometido tiene la misma credencial que el operador. Y un skill
`format: source` no está contenido: `--sandbox process` limpia su entorno y
encierra su directorio de trabajo, `format: wasm` sí está genuinamente aislado,
pero ninguno de los dos contiene código hostil. Lee [Modelo de
seguridad](#modelo-de-seguridad) antes de exponer un puerto; [Estado de los
hitos](#estado-de-los-hitos) y [Diseñado, aún no construido](#diseñado-aún-no-construido)
lo detallan todo.

Para agregar una capacidad, escribe un skill y regístralo — no hay cola de
revisión, porque el registry lo alojas tú. Mira [`skills/`](skills/) para
patrones ([`skills/echo/`](skills/echo/) es el mínimo,
[`skills/postgres-cdc/`](skills/postgres-cdc/) un skill `sensorial` que envuelve
un sistema externo, [`skills/model-manager/`](skills/model-manager/) uno
`motor` que actúa sobre el mundo) y [`CONTRIBUTING.md`](CONTRIBUTING.md) para
el kernel/SDK/spec en sí.

---

## Índice

1. [Qué problema resuelve](#qué-problema-resuelve)
2. [Cómo se compara](#cómo-se-compara)
3. [Funcionalidades destacadas](#funcionalidades-destacadas)
4. [Casos de uso](#casos-de-uso)
5. [Conceptos fundamentales](#conceptos-fundamentales)
6. [Arquitectura](#arquitectura)
7. [Instalación y arranque rápido](#instalación-y-arranque-rápido)
8. [Referencia de comandos](#referencia-de-comandos)
9. [Ejemplos completos](#ejemplos-completos)
10. [La UI del plano de control](#la-ui-del-plano-de-control)
11. [Escribir un skill (el SDK)](#escribir-un-skill-el-sdk)
12. [Configuración de skills en tiempo de ejecución](#configuración-de-skills-en-tiempo-de-ejecución)
13. [Conectar software existente](#conectar-software-existente)
14. [Hablarle](#hablarle)
15. [Operación en lenguaje natural](#operación-en-lenguaje-natural)
16. [Puertos tipados, forzados](#puertos-tipados-forzados)
17. [Scheduling: especulación, deadlines, presupuestos](#scheduling-especulación-deadlines-presupuestos)
18. [Drivers de modelos y admisión de recursos](#drivers-de-modelos-y-admisión-de-recursos)
19. [Audit bundles](#audit-bundles)
20. [Un entorno OpenEnv](#un-entorno-openenv)
21. [Aprobación firmada — quién lo permitió](#aprobación-firmada--quién-lo-permitió)
22. [El broker de credenciales](#el-broker-de-credenciales)
23. [Regresión contra el ledger](#regresión-contra-el-ledger)
24. [Aislamiento de skills](#aislamiento-de-skills)
25. [Witnessing abierto](#witnessing-abierto)
26. [El marketplace](#el-marketplace)
27. [Explicabilidad y replay](#explicabilidad-y-replay)
28. [Federar nodos](#federar-nodos)
29. [Estándares en las fronteras](#estándares-en-las-fronteras)
30. [Proteger las tools de un agente](#proteger-las-tools-de-un-agente)
31. [Los cinco contratos](#los-cinco-contratos)
32. [Modelo de seguridad](#modelo-de-seguridad)
33. [API HTTP y WebSocket](#api-http-y-websocket)
34. [Estructura del repositorio](#estructura-del-repositorio)
35. [Compilación y release](#compilación-y-release)
36. [Estado de los hitos](#estado-de-los-hitos)
37. [Diseñado, aún no construido](#diseñado-aún-no-construido)
38. [Licencia y gobernanza](#licencia-y-gobernanza)

---

## Qué problema resuelve

Las plataformas de automatización de workflows — n8n, Zapier, Make —
resolvieron "conecta mis herramientas existentes" para trabajo *por lotes*: se
dispara un trigger, corre una cadena de pasos una vez, y la ejecución termina.
Ese modelo encaja bien con una sincronización nocturna y con un webhook. Encaja
mal con una conversación de voz o una sesión multiagente larga, porque ahí el
estado que importa vive *entre* mensajes, no dentro de una ejecución.

Hacer de la conexión la unidad de trabajo no es una idea nueva — la
infraestructura de chat y videojuegos lleva años funcionando así, y Temporal y
los frameworks de voz en tiempo real resuelven cada uno una parte. Lo poco
habitual aquí es aplicarlo a trabajo de IA *conectado a legacy*: un runtime que
instalas junto a los sistemas existentes y que les habla por el mismo protocolo
de streaming que usa para modelos y clientes. De ahí salen tres consecuencias, y
de ellas trata buena parte de esta guía:

- **Legacy primero.** El primer comando útil es *conecta lo que ya tienes*, no
  "crea un proyecto". Apúntalo a una especificación OpenAPI y sus operaciones se
  convierten en skills — de solo lectura por defecto, con aprobación humana
  obligatoria antes de cualquier escritura. Ver [Conectar software
  existente](#conectar-software-existente).
- **Una sola interfaz para cada modelo.** LLMs, ASR, TTS — y cualquier modelo
  que un skill envuelva — todos tras el mismo contrato de skill, en local o
  remoto. Mover un skill a otra máquina cambia la colocación, no el grafo.
- **Distribución federable.** La especificación, el kernel y los SDKs son
  abiertos y el registry es federable, así que el catálogo del que instalas
  puedes alojarlo tú. Ver [El marketplace](#el-marketplace).

---

## Cómo se compara

Casi todo lo que hace este runtime ya lo hace algo más — normalmente con más
madurez, más integraciones, o ambas. Un mapa honesto de dónde encaja:

| Si necesitas | Usa | Posición de Deep Axiom |
|---|---|---|
| Cientos de integraciones SaaS listas | **n8n, Zapier, Make** | No compite *en catálogo*. El suyo *es* el producto; los diez skills de aquí son [ejemplos de referencia](skills/), no un inventario — la historia de integración es `aura connect` (cualquier spec OpenAPI se vuelve skills) y `aura guard` (cualquier servidor MCP), no un estante que surtimos. |
| Workflows largos, duraderos y reproducibles | **Temporal** | No compite en durabilidad. Temporal sobrevive a la muerte del proceso a mitad de workflow; esto todavía no resume una sesión caída. |
| Agentes de voz en tiempo real en producción | **LiveKit Agents, Pipecat, OpenAI Realtime** | Mucha menos madurez. El primer sonido de una respuesta llega a ~0,41 s en una máquina de desarrollo con modelo local — nunca medido contra estos lado a lado, así que léelo como "usable", no como "competitivo". Elige esos salvo que necesites la voz sobre el *mismo* runtime que el resto. |
| Una librería de agentes dentro de tu app | **LangGraph, CrewAI, AutoGen** | Forma distinta. Esas son librerías con las que construyes; esto es un proceso que instalas al lado de sistemas existentes. |
| Descubrimiento de herramientas para un modelo | **MCP** | No es competencia — esto trae un servidor MCP para que sus skills sean herramientas MCP. |
| Una capa de seguridad sobre el agente que ya corres | **nada que exista hoy** | `aura guard` pone por delante los servidores MCP que un agente ya usa, así cada tool call queda autorizado por la policy del nodo, gateado si actúa sobre el mundo, y sellado en un ledger verificable — sin reescribir el agente. Los productos de audit log se sientan *al lado* de un runtime y registran; esto se sienta *delante* de la llamada y puede rechazarla. La limitación es real y está dicha de entrada: vale hasta donde llega tu control sobre la configuración del agente. |

Lo que sí es genuinamente distinto, y la razón para mirar esto en vez de lo de
arriba:

- **La puerta de aprobación humana es un invariante del kernel, no código del
  grafo.** En las librerías de agentes el interrupt vive en el grafo que
  escribiste, así que un grafo que se olvide no tiene puerta. Aquí la aplica el
  executor en el único punto por el que pasa todo grafo, y el modo `published`
  *rechaza* una arista `motor.*` sin puerta en vez de repararla en silencio.
  Ver [Modelo de seguridad](#modelo-de-seguridad).
- **La cancelación es una garantía, no una petición.** El kernel suprime todo lo
  que pertenezca a una cadena cancelada, así que un skill que ignore el cancel
  no puede entregar igualmente — y direcciona a cada skill con el `cause_id` que
  *ese* skill reconoce, a cualquier profundidad.
- **Un solo IR para grafos escritos a mano y generados por el planner.** Un
  executor, un modelo de permisos, una ruta de replay, un depurador — en vez de
  un camino aparte para "esto lo decidió el agente".
- **Un binario, sin servicios externos.** ~22 MB con la UI embebida, y sin
  Postgres, sin Redis, sin broker, sin clúster que levantar antes del primer
  mensaje.

Los contratos sobre los que descansa esto están congelados y especificados en
[`spec/`](spec/), con una suite de conformidad con la que una implementación
demuestra que cumple.

---

## Funcionalidades destacadas

Un mapa, no un resumen — cada línea nombra la sección que explica bien la cosa,
porque una lista de funcionalidades que reformula treinta secciones es una
segunda copia de la guía que se desincroniza de la primera.

| | |
|---|---|
| **Un solo binario** — kernel, UI, almacén de estado y bus de mensajes, sin servicios externos | [Arquitectura](#arquitectura) |
| **Streams como primitiva** — canales tipados, ordenados, causales, idempotentes y con contrapresión que transportan por igual texto, audio, documentos y eventos | [Conceptos fundamentales](#conceptos-fundamentales) |
| **Un solo formato de grafo** para lo escrito a mano y lo generado por el planner — un depurador, un modelo de permisos, una vía de replay | [Operación en lenguaje natural](#operación-en-lenguaje-natural) |
| **Cualquier modelo tras una interfaz** — LLM, ASR, TTS, local o remoto, intercambiable sin tocar el grafo; más un presupuesto de memoria contra el que el nodo admite | [Drivers de modelos y admisión de recursos](#drivers-de-modelos-y-admisión-de-recursos) |
| **Skills ajustables en caliente** — defaults del manifiesto, un archivo `--config`, overrides en vivo aplicados sobre la propia conexión del skill | [Config de skills en runtime](#configuración-de-skills-en-tiempo-de-ejecución) |
| **Legacy primero** — una especificación OpenAPI se vuelve skills, de solo lectura por defecto, con dry-run y promoción por operación | [Conectar software existente](#conectar-software-existente) |
| **Fronteras estándar** — un servidor MCP cuyo `tools/call` hace streaming, una tarjeta A2A, exportación OpenTelemetry | [Estándares en las fronteras](#estándares-en-las-fronteras) |
| **`aura guard`** — los servidores MCP que tu agente ya usa, puestos tras la policy, el gate y el ledger de este nodo, sin reescribir nada | [Proteger las herramientas de un agente](#proteger-las-tools-de-un-agente) |
| **El gate como invariante del kernel** — lo aplica el executor, así que un grafo que se olvide de él igual lo tiene | [Modelo de seguridad](#modelo-de-seguridad) |
| **Un skill no es el operador** — una credencial acotada a una capability, que no puede registrar un grafo ni leer el ledger | [Credenciales con alcance](#credenciales-con-alcance) |
| **Quién aprobó, firmado** — la firma de un operador inscrito sellada en la entry, hecha con una clave que el nodo nunca tuvo | [Aprobación firmada](#aprobación-firmada--quién-lo-permitió) |
| **Una credencial solo contra un recibo** — el secreto vive en el kernel y se libera contra un efecto sellado, así que saltarse el gate da un 401 | [El broker de credenciales](#el-broker-de-credenciales) |
| **El ledger de efectos** — encadenado por hash, comprometido en Merkle, firmado, verificable sin conexión desde el archivo de base de datos y sin kernel corriendo | [Modelo de seguridad](#modelo-de-seguridad) |
| **Recibos portables y audit bundles** — la evidencia de un efecto o de una sesión, verificable por cualquiera, sin revelar nada más | [Audit bundles](#audit-bundles) |
| **Terceros que rinden cuentas** — ancla en un witness que publica su propio log y firma hasta dónde ha respaldado | [Witnessing abierto](#witnessing-abierto) |
| **Regresión a nivel de efectos** — reproduce sesiones grabadas contra un modelo nuevo y diffea los actos, no las transcripciones | [Tests de regresión](#regresión-contra-el-ledger) |
| **Explicar y reproducir** — `aura why` narra un fallo desde el log causal | [Explicabilidad y replay](#explicabilidad-y-replay) |
| **Marketplace firmado y federable, y federación de nodos** | [El marketplace](#el-marketplace), [Federar nodos](#federar-nodos) |

Todo lo anterior existe y funciona. Lo que **no** está — aislamiento de procesos
para skills `format: source`, el modo `site`, sesiones multidispositivo, failover
de nodo — aparece con la misma claridad en [Modelo de
seguridad](#modelo-de-seguridad) y [Estado de los hitos](#estado-de-los-hitos),
junto con qué partes detectaría realmente una regresión.

---

## Casos de uso

Formas concretas que adopta el mismo runtime. En todas ellas, la IA se añade
*sobre* lo que existe, no en su lugar.

- **IA sobre un ERP/CRM legacy.** `aura connect` a una OpenAPI interna, y el
  personal opera el sistema en lenguaje natural — "factura este pedido y luego
  envía un email al cliente" — con cada escritura requiriendo un OK humano. Sin
  cambios en el ERP.
- **Asistente de voz en tiempo real.** Compón `sensorial.asr.transcribe` →
  `cognitive.llm.chat` → `motor.tts.speak` en un grafo. Voz de entrada,
  razonamiento, voz de salida, en streaming — las piezas son skills
  intercambiables.
- **Captura de cambios hacia el razonamiento.** `skills/postgres-cdc` convierte
  el propio stream de replicación de una base legacy en eventos causales; un LLM
  clasifica cada cambio de fila; una API proyectada archiva el resultado. Los
  datos sensibles pueden fijarse a nodos on-premise para que nunca salgan del
  edificio.
- **División edge + nube.** Ejecuta la captura y la inferencia sensible a la
  privacidad en un nodo edge (un Jetson en la planta de fábrica); federa el
  razonamiento pesado a un nodo en la nube. Un solo grafo lógico, con la
  colocación decidida por skill; la planta sigue funcionando si el enlace cae.
- **Back office multiagente.** Skills especialistas — un investigador, un
  redactor, un aprobador — encadenados con puertas humanas entre ellos, y
  `aura why` explica cualquier ejecución que se torció.
- **Hub de interoperabilidad para herramientas de IA existentes.** Expón los
  skills de tu organización como tools MCP para que Claude Code / Cursor /
  cualquier cliente MCP pueda llamarlos, y exporta cada sesión a tu stack
  OpenTelemetry existente para observabilidad.
- **Un marketplace de skills.** Un equipo publica un skill firmado y versionado
  (un OCR de dominio, un verificador de compliance); otros equipos lo instalan
  por capacidad y lo ejecutan en sus propios nodos, o lo llaman en remoto — el
  registro lo alojas tú, no un proveedor.
- **Plataforma para desarrolladores.** Distribuye Deep Axiom embebido en tu producto
  para que *tus* clientes añadan IA a *su* stack, con tu propio registro
  federado como catálogo.

---

## Conceptos fundamentales

Solo necesitas los dos primeros el primer día. El resto se descubren cuando
duelen.

| Concepto | Qué es |
|---|---|
| **Skill** | La unidad atómica de función — algo que el sistema *sabe hacer*. Tiene identidad, puertos tipados y un manifiesto. Un skill puede ser lógica, un modelo o una proyección de un sistema existente. |
| **Channel** | Un stream tipado entre dos puertos. Ordenado (FIFO), causal (cada mensaje nombra su causa), idempotente (at-least-once con claves de deduplicación), con back-pressure y medible. |
| **Graph** | Skills cableados mediante channels. Escrito por un humano en unas pocas líneas, o generado por un planner en tiempo de ejecución — ambos compilan a la *misma* representación intermedia y corren por el *mismo* ejecutor. |
| **Kernel** | Un único binario con cuatro primitivas: identidad, channels, un registro vivo de skills y un ejecutor de grafos. SQLite embebido para el estado, bus de mensajes embebido, cero servicios externos. |
| **Node** | Un proceso kernel en una máquina. Los nodos funcionan de forma autónoma y pueden federarse. |
| **Session** | Una instancia viva de un grafo, propiedad de exactamente un nodo, con su propio log de eventos causal append-only. |
| **Projection** | Un sistema existente (una API, una base de datos, un servidor MCP) expuesto *como skills* sin cambiar su código. |
| **Registry** | Un host de paquetes federable donde los skills se publican (firmados) y se instalan, resolubles por capacidad. |

### Los cinco tipos de skill

Cada skill declara un tipo, que además prefija su capacidad
(`sensorial.asr.transcribe`, `motor.tts.speak`, …). El tipo no es cosmético: el
runtime lo usa para aplicar políticas — por ejemplo, las aristas hacia skills
`motor.*` siempre reciben una puerta de aprobación humana en los grafos
generados por el planner o por MCP.

| Tipo | Rol | Ejemplos |
|---|---|---|
| `sensorial` | Percibe — convierte el mundo en datos | ASR, OCR, cámaras, lectores de archivos |
| `cognitive` | Razona — decide, planifica, genera | chat LLM, planner, clasificadores |
| `motor` | Actúa — produce efectos en el mundo | enviar un mensaje, escribir en un ERP, hablar |
| `memory` | Recuerda — persiste y recupera contexto | historial, memoria semántica |
| `logical` | Transforma/valida — datos → datos determinista | parsers, validadores, puentes de formato |

---

## Arquitectura

Tres planos, limpiamente separados, para que las partes que cambian rápido nunca
desestabilicen las partes que deben permanecer estables durante años.

```
╔══════════ ESPECIFICACIÓN (neutral, versionada, con suite de conformidad) ═══════╗
║   C1 Manifiesto        C2 IR de Grafo        C3 Protocolo de Channel            ║
╚═════════════════════════════════════════════════════════════════════════════════╝
                                    ▲ implementada por
┌────────────────────────────── KERNEL (un binario) ──────────────────────────────┐
│  Identidad   ·   Channels   ·   Registro de skills   ·   Ejecutor de grafos     │
│  SQLite embebido (estado + log causal)   ·   bus embebido   ·   admisión        │
└──────────────────────────────────────────────────────────────────────────────────┘
                                    ▲ todo lo de abajo es USERLAND (reemplazable)
┌──────────────────┬──────────────────┬──────────────────┬───────────────────────┐
│ COGNICIÓN        │ DRIVERS MODELOS  │ PROYECCIONES     │ FRONTERAS ESTÁNDAR    │
│ planner, chat,   │ llama.cpp, ONNX, │ OpenAPI → skills │ servidor MCP, A2A,    │
│ memoria          │ whisper, TTS     │ (legacy-first)   │ export OpenTelemetry  │
└──────────────────┴──────────────────┴──────────────────┴───────────────────────┘
                                    ▲ distribuido vía
┌───────────────── REGISTRO FEDERABLE ──────────────┐   ┌─────── FEDERACIÓN ──────┐
│ local (~/.aura) → sitio → público, un protocolo   │   │ nodo ⇄ nodo, hoja-nodo  │
└───────────────────────────────────────────────────┘   └─────────────────────────┘
```

**La idea estructural:** el kernel no sabe nada de LLMs, modelos, APIs ni del
marketplace. La cognición (el planner, el chat), los drivers de modelos, las
proyecciones legacy, el servidor MCP e incluso la federación son todos
*userland* — se conectan al kernel por el mismo protocolo público que usa
cualquier tercero, sin privilegios especiales. El test de pureza: si una
funcionalidad necesita un LLM para funcionar, no pertenece al kernel.

- **Plano de control** — el kernel: quién existe, qué ofrece, quién puede hablar
  con quién, dónde corre el trabajo. Señaliza y retransmite; no incorpora
  inteligencia.
- **Plano de datos** — los channels: streams tipados y causales entre skills.
  Hoy se retransmiten a través del kernel; el protocolo ya transporta lo que
  necesitará el transporte peer-to-peer negociado (LAN, QUIC/WebRTC).
- **Plano de modelos** — los drivers: inferencia nativa (llama.cpp, ONNX
  Runtime, whisper, voces del SO) tras una interfaz de skill uniforme. Un modelo
  es simplemente un skill.

---

## Instalación y arranque rápido

**Requisitos:** Go 1.25+ para compilar el kernel; Python 3.11+ para ejecutar los
skills de primera parte; Node 20+ solo si quieres reconstruir la UI. Hardware de
consumo (8 GB de RAM) es suficiente.

```powershell
# 1. Compila el kernel (la UI va embebida — no hace falta Node en runtime)
cd kernel
go build -o aura.exe ./cmd/aura
.\aura.exe up
#   → UI del plano de control en http://localhost:9080
#   → endpoint MCP en           http://localhost:9080/mcp
```

`aura up` arranca un nodo plenamente funcional — kernel, UI y cuatro grafos
sembrados de fábrica (`echo`, `chat`, `plan` y `voice`). En una segunda terminal,
comprueba que todo el camino funciona sin modelo y sin ninguna descarga:

```powershell
# 2. El skill más pequeño posible — sin modelo, sin credenciales, sin red
pip install -r skills\echo\requirements.txt
$env:PYTHONPATH="sdk\python\src"
cd skills\echo; python main.py

# después, desde la raíz del repo:
.\kernel\aura.exe chat --graph echo "test"    # → test
```

Después pon en línea un LLM local y háblale de verdad:

```powershell
# 3. El skill de LLM local (desde la raíz del repo)
pip install -r skills\llm-chat\requirements.txt
#   wheel CPU de llama-cpp-python:
#   pip install llama-cpp-python --extra-index-url https://abetlen.github.io/llama-cpp-python/whl/cpu
cd skills\llm-chat
$env:PYTHONPATH="..\..\sdk\python\src"; python main.py
#   descarga Qwen2.5-1.5B (~1,1 GB) con el primer mensaje
```

```powershell
.\kernel\aura.exe chat            # REPL interactivo
.\kernel\aura.exe chat "hola"     # una sola pregunta
```

> **Los flags van antes del mensaje.** `aura chat --graph echo "test"` funciona;
> `aura chat "test" --graph echo` manda el texto literal `test --graph echo` al
> grafo por defecto. Lo mismo aplica a `aura do`.

Ese es el camino del primer día: un solo binario, dos conceptos (skill y
channel) y un modelo local respondiendo a través del pipeline completo. Para la
voz, ver [Hablarle](#hablarle); para comprobar el nodo contra los contratos
congelados, ver [Compilación y release](#compilación-y-release).

---

## Referencia de comandos

El binario `aura` es el kernel, el cliente, el registro y la herramienta de
flota.

### Ejecutar un nodo

| Comando | Propósito |
|---|---|
| `aura up [--port 9080] [--data <dir>] [--mode local\|site\|published] [--memory-budget 8Gi] [--config <archivo>]` | Arranca un nodo. `--memory-budget` activa la admisión de recursos; `--config` fija valores por defecto de skills (ver [Configuración de skills en tiempo de ejecución](#configuración-de-skills-en-tiempo-de-ejecución)). |
| ↳ `[--with-examples] [--examples-dir skills]` | Arranca también los skills de ejemplo de `skills/`. Opt-in, nunca por defecto: los skills de un nodo normalmente los elige un operador, y el kernel es un binario Go que no debería depender de un toolchain de Python. Cada uno se lanza y se observa — el que muere devuelve su propia última línea de stderr, que ante una dependencia ausente es la línea de pip que necesitas. |
| `aura status [--port 9080]` | Salud de un nodo en ejecución más sus skills conectados. |
| `aura ready [--port 9080] [--quiet] [--timeout 3s]` | Readiness como código de salida, para un healthcheck de contenedor. Lee `/readyz` — abierto, para que una sonda no necesite credencial — y sale 0 solo cuando el store, el ledger y el registro son usables. La imagen distroless no tiene shell ni curl, así que su `HEALTHCHECK` es este comando. |
| `aura verify [--data <dir>]` | Recalcula la cadena de hashes y el árbol Merkle del ledger de efectos, y comprueba cada firma de checkpoint y cada contrafirma de witness — sin conexión, sin necesitar un kernel corriendo. Sale con código distinto de cero si algo no verifica. Ver [Modelo de seguridad](#modelo-de-seguridad). |
| `aura version` | Versión y los majors de protocolo/IR que habla este binario. |

### Evidencia

Todo esto sirve para probar, después, qué hizo un nodo — ver
[Modelo de seguridad](#modelo-de-seguridad).

| Comando | Propósito |
|---|---|
| `aura witness <url-witness> [--port 9080]` | Ancla el ledger de este nodo con un tercero: presenta la cabeza firmada más una prueba de consistencia, y registra la contrafirma. Cualquier nodo puede actuar como witness. |
| `aura receipt <hash-efecto> [--out <archivo>] [--data <dir>]` | Construye el documento de evidencia portátil de un efecto sellado — entrada, prueba de inclusión, cabeza firmada, contrafirmas, atestaciones citadas. |
| `aura receipt --verify <archivo>` | Comprueba un recibo sin base de datos, sin nodo y sin red. Lo que ejecuta un tercero. |
| `aura bundle <sesión> [--out <archivo>] [--data <dir>]` | Arma el audit bundle de una sesión: trayectoria, un recibo verificable por efecto sellado, y las configuraciones de modelo detrás. |
| `aura bundle --verify <archivo>` | Comprueba un bundle sin base de datos, sin nodo y sin red. |
| `aura bom [sesión] [--out <archivo>] [--data <dir>]` | ML-BOM CycloneDX 1.6 de los modelos y skills que realmente corrieron, construido desde el ledger y no desde la configuración. |

### Hablar con grafos

Los flags deben ir antes del mensaje posicional — `aura chat --graph echo "test"`,
no `aura chat "test" --graph echo` (esto último manda los flags como parte del texto).

| Comando | Propósito |
|---|---|
| `aura chat [--graph chat] [--port 9080] ["mensaje"]` | REPL o conversación de un solo turno con un grafo, con respuestas en streaming. |
| `aura do [--yes] [--port 9080] "objetivo"` | Objetivo en lenguaje natural → plan → grafo registrado → ejecución con puertas. |

### Conectar software existente

| Comando | Propósito |
|---|---|
| `aura connect --openapi <url\|archivo> [--name x] [--base-url y] [--header "K: V"]` | Introspecciona una API y proyecta sus operaciones como skills. |
| `aura observe --target <url> [--port 8080] [--out observed.jsonl]` | Hace de proxy de un sistema en marcha y graba la forma de su tráfico. |
| `aura generate connector --from <grabación> --name <nombre> [--base-url y]` | Convierte una grabación en un spec de conector, para revisar. |
| `aura projections [--port 9080]` | Lista las proyecciones y el modo de cada operación. |
| `aura promote <proyección> <op> --mode dry-run\|live\|disabled` | Cambia el modo de seguridad de una operación. |

### El marketplace

| Comando | Propósito |
|---|---|
| `aura registry serve [--port 9091] [--data <dir>]` | Aloja un registro de paquetes federable. |
| `aura publish <dir-skill> [--registry <url>]` | Firma (Ed25519, keygen automático) y sube un skill. |
| `aura add <org/cat/nombre>[@versión] \| --capability <cap> [--registry <url>] [--yes]` | Descarga, verifica hash + firma, muestra permisos, instala. |
| `aura run <org/cat/nombre> [--port 9080]` | Arranca un skill instalado contra el nodo local — lanza un proceso para `format: source`, o lo aloja dentro del sandbox wazero del propio kernel para `format: wasm` (ver [Skills Wasm](#skills-wasm)). |

### Depuración y observabilidad

| Comando | Propósito |
|---|---|
| `aura why [sesión] [--no-explain] [--port 9080]` | Recorre la cadena causal de una sesión hasta la causa raíz; la narra con el LLM local. |
| `aura replay <sesión> [--graph <id>] [--deny-gates] [--port 9080]` | Re-ejecuta entradas reales grabadas sobre el grafo actual y compara las salidas, y además compara las entradas selladas del ledger de ambas sesiones entre sí — misma capability, decisión y resultado, no solo la misma transcripción (ver [C4](spec/c4-ledger.md)). |
| `aura trace <sesión> [--otlp <url>] [--out <archivo>] [--port 9080]` | Exporta el log causal como trazas OpenTelemetry. |
| `aura undo <sesión\|recibo> [--yes] [--port 9080]` | Revierte un efecto, o todos los efectos reversibles de una sesión (orden causal inverso), reenviando su payload original al puerto `compensates` declarado por el skill. Cada undo es a su vez un efecto gateado y sellado — ver [C4](spec/c4-ledger.md). |

### Flota

| Comando | Propósito |
|---|---|
| `aura federate <url-remota> [--capability <cap>] [--port 9080]` | Proyecta los skills de un nodo remoto dentro del nodo local. |

### Proteger un agente

| Comando | Propósito |
|---|---|
| `aura guard --config <mcp-servers.json> [--dry-run] [--trust-annotations] [--port 9080]` | Pone por delante los servidores MCP que un agente ya usa, de modo que cada tool call pase por la policy, el gate y el ledger de este nodo. `--dry-run` informa qué se registraría y no cambia nada. Ver [Proteger las tools de un agente](#proteger-las-tools-de-un-agente). |
| `aura approvals [--json] [--port 9080]` | Lista las llamadas que están esperando a un humano. |
| `aura approve <id> [--deny] [--port 9080]` | Responde una de ellas. |

---

## Ejemplos completos

Recorridos de extremo a extremo que encadenan los comandos en algo real. Cada
uno es autocontenido; ejecútalos desde la raíz del repo con el kernel compilado.

### Ejemplo 1 — Un bucle de voz por ficheros (WAV → razonamiento → WAV)

Compón tres skills en un grafo para que una frase grabada vuelva como habla
grabada. Esta es la forma *batch*, a propósito: usa los puertos de fichero
completo (`ears.audio_in`, `mouth.audio_out`), así que es fácil de conducir con
`curl` y fácil de leer. La versión viva, en streaming, es el grafo `voice`
sembrado — añade un cuarto skill y usa los puertos de chunks; ver
[Hablarle](#hablarle).

```powershell
# Arranca el nodo, luego ejecuta los tres skills driver (cada uno en su terminal):
.\kernel\aura.exe up
$env:PYTHONPATH="sdk\python\src"
cd skills\asr;  python main.py        # sensorial.asr.transcribe
cd skills\llm-chat; python main.py    # cognitive.llm.chat
cd skills\tts;  python main.py        # motor.tts.speak
```

Registra un grafo que los cablea y condúcelo (WAV de entrada → WAV de salida):

```json
POST /v1/graphs
{
  "ir": "1", "graph_id": "voice-file", "origin": { "kind": "declared" },
  "nodes": [
    { "ref": "ears",  "resolve": "sensorial.asr.transcribe" },
    { "ref": "brain", "resolve": "cognitive.llm.chat" },
    { "ref": "mouth", "resolve": "motor.tts.speak" }
  ],
  "edges": [
    { "from": "client.audio_out", "to": "ears.audio_in" },
    { "from": "ears.text_out",    "to": "brain.text_in" },
    { "from": "brain.text_out",   "to": "mouth.text_in", "gate": "none" },
    { "from": "mouth.audio_out",  "to": "client.audio_in" }
  ]
}
```

Usa un `graph_id` propio — registrar uno llamado `voice` sobrescribiría el
grafo de streaming sembrado. El `"gate": "none"` no es decoración opcional:
hablar es `motor.*`, así que sin él el kernel pone puerta a esa arista y la
respuesta se queda esperando un clic humano ([Modelo de
seguridad](#modelo-de-seguridad)).

Los dos saltos de modelo van en streaming por dentro, pero el audio en cada
extremo de *este* grafo es un fichero completo: la respuesta se habla una vez
sintetizada la cláusula, no mientras se escribe. El intercambio queda grabado
en cualquier caso: `aura why sess-…` lo narra, `aura trace` lo envía a tu stack
de observabilidad.

### Ejemplo 2 — Hacer operable por IA una API legacy

Lleva una API existente de cero a "manéjala hablándole" en tres comandos.

```powershell
.\kernel\aura.exe connect --openapi https://crm.internal/api/openapi.json --name crm
#  proyección "crm" registrada: 40 operaciones — lecturas activas, escrituras deshabilitadas

.\kernel\aura.exe promote crm create-user --mode dry-run     # inspecciona primero
# y cuando te fíes:
.\kernel\aura.exe promote crm create-user --mode live

# con el skill planner en ejecución:
.\kernel\aura.exe do "crea un usuario llamado Grace Hopper en el crm"
#  plan › Usar motor.api.crm.create_user …
#  gate › ¿Aprobar la entrega a s1.request_in?  [y/N]: y
#  result ‹ { "ok": true, "status": 201, "body": { "id": 99, "name": "Grace Hopper" } }
```

El código del CRM nunca se tocó; la escritura ocurrió solo después de que un
humano la aprobara.

### Ejemplo 3 — Construir, publicar e instalar un skill

Escribe un skill, publícalo firmado e instálalo por capacidad en otro sitio.

```powershell
# esqueleto: un directorio con skill.yaml + main.py (ver "Escribir un skill")
.\kernel\aura.exe registry serve                       # aloja un registro (puerto 9091)
.\kernel\aura.exe publish .\my-skills\invoice-ocr\     # zip + firma (Ed25519) + subida
#  publicado acme/vision/invoice-ocr@1.0.0 — firmado con la clave QMJ…

# en otra máquina / nodo:
.\kernel\aura.exe add --capability sensorial.ocr --registry http://registry.internal:9091
#  capacidad "sensorial.ocr" resuelta → acme/vision/invoice-ocr
#  permisos: egress_http []  filesystem none  → ¿instalar? [y/N]: y
.\kernel\aura.exe run acme/vision/invoice-ocr
```

Republicar la misma versión con bytes distintos se rechaza; una clave diferente
no puede secuestrar el id del paquete.

### Ejemplo 4 — Edge + nube con federación

Mantén la inferencia privada en local y toma prestado el razonamiento pesado de
un nodo más grande.

```powershell
# En la caja edge (tiene el skill de OCR, mantiene los documentos on-premise):
edge> .\aura.exe up --port 9080

# En el nodo de la nube (tiene un LLM grande):
cloud> .\aura.exe up --port 9080

# Desde la caja edge, toma prestada la capacidad de razonamiento de la nube:
edge> .\aura.exe federate http://cloud-node:9080 --capability cognitive
#  → el nodo edge ahora resuelve cognitive.llm.chat, ejecutado en el nodo de la
#    nube, mientras el OCR sigue en local. Si el enlace cae, el edge sigue
#    sirviendo lo que tiene.
```

### Ejemplo 5 — Exponer todo a Claude Code / Cursor

Cada skill conectado se convierte en una tool MCP con una línea.

```powershell
claude mcp add --transport http aura http://localhost:9080/mcp
#  En tu cliente MCP, tools/list ahora muestra logical_echo, cognitive_llm_chat,
#  sensorial_api_crm_list_users, …  — las tools de lectura corren directamente;
#  las tools de acción (motor.*) se rechazan a la espera de aprobación humana,
#  por diseño.
```

**Estos cinco ilustran la forma, no son recetas para copiar y pegar** — asumen
una API, una cámara o un registro que tú aportas. Para un camino que corre tal
cual, ver [Instalación y arranque rápido](#instalación-y-arranque-rápido);
para skills que puedes leer y copiar hoy, ver [`skills/`](skills/) —
[`skills/echo/`](skills/echo/) es el mínimo, [`skills/postgres-cdc/`](skills/postgres-cdc/)
un skill `sensorial` que envuelve un sistema externo, y
[`skills/model-manager/`](skills/model-manager/) uno `motor` que actúa sobre el mundo.

---

## La UI del plano de control

El frontend ([`ui/`](ui/), React 19 + Vite + TypeScript, internacionalizado con
react-i18next — base en inglés, español incluido) está **integrado en el
binario** vía `go:embed`. `aura up` lo sirve en `http://localhost:9080` sin
proceso Node en runtime. Seis vistas, abriendo en el Estudio:

- **Estudio** — la pantalla de inicio, y la única superficie para construir y
  ejecutar. Una paleta de skills vivos, un lienzo donde arrastras y cableas, un
  inspector para cada campo C2 de una arista, deshacer/rehacer, selección
  múltiple y en caja, copiar/pegar/duplicar, controles de zoom, ajustar,
  ordenar, un minimapa y un cajón de IR que hace round-trip en ambos sentidos.
  O escribe un objetivo y el planner compila un grafo sobre el mismo lienzo.

  Reemplazó dos vistas. Lienzo podía dibujar un grafo pero nunca mostraba uno
  funcionando; Operate podía ejecutarlo pero como una lista al lado de un
  resultado. El grafo que editas es el grafo que se enciende, en las mismas
  coordenadas.

  La edición está activa mientras nada corre y pasa a solo lectura en cuanto
  arranca una sesión — un nodo que se movió nunca debe ser ambiguo entre "lo
  arrastré yo" y "pasó algo". La planificación no está en vivo (el planner
  emite un solo `std/plan@1`, así que el grafo aparece entero); la ejecución
  sí, porque cada envelope nombra su nodo. No le cuesta nada al nodo: esos
  envelopes ya llegan por el socket de la sesión, y la actividad se agrupa a un
  render por frame.

  Su columna derecha tiene pestañas: el **inspector** de lo que esté
  seleccionado, la **actividad** de la sesión que corre, o **conversar** — que
  es donde fueron a parar las pantallas de Chat y Voz. Escribir y hablar nunca
  fueron dos funciones (ambas abren una sesión, meten entrada por un puerto de
  cliente y renderizan lo que vuelve), así que son un solo panel con un
  selector de transporte:

  - *Texto* corre el grafo registrado que elijas, normalmente `chat`, y
    responde a las puertas de aprobación en línea con Aprobar/Denegar.
  - *Voz* corre el grafo `voice` — cuatro skills a la vez, seis puertos de
    cliente. Tus palabras aparecen mientras las dices, la respuesta se habla
    mientras se escribe, y hablarle encima la para.

  Siguen siendo dos grafos y no un grafo con un ajuste, porque eso es lo que
  son; el selector cambia la sesión. Volver a texto, o salir del panel, suelta
  el micrófono.
- **Skills** — el catálogo vivo de capacidades, con colores por tipo, con los
  puertos, esquemas y descripción de cada skill.
- **Projections** — conecta una especificación OpenAPI pegándola, y cambia cada
  operación entre `disabled` / `dry-run` / `live`.
- **Graphs** — los grafos registrados y su IR.
- **Sessions** — todas las sesiones con insignias de error; haz clic en una para
  inspeccionar su log causal de eventos en línea (la materia prima detrás de
  `aura why`).
- **Referencia** — todos los comandos, los cinco contratos completos, todos los
  esquemas JSON y todas las enumeraciones. Ver más abajo.

### El lienzo

Una paleta de skills alimentada por el catálogo vivo, nodos arrastrables, cableado
puerto a puerto que solo ilumina destinos con esquema compatible, pan y zoom, y un
inspector que expone cada campo C2 que lleva una arista — `gate`, `qos`,
`speculative`, `deadline_ms`, `priority` — con lo que hace cada uno, no solo su
nombre. Un cajón de IR hace round-trip de texto en ambas direcciones, así que un
grafo se puede pegar, dibujar encima y copiar de vuelta.

**Edición.** Deshacer/rehacer, selección múltiple, selección en caja con
Shift, copiar/pegar/duplicar, borrar, zoom con lectura, ajustar y ordenar.
Además de eso:

- **Clic derecho en cualquier cosa.** El lienzo, un nodo, una arista y una nota
  tienen su propio menú, construido para ese objetivo en vez de uno solo con
  casi todo deshabilitado.
- **Doble clic en el lienzo** para añadir un skill *donde hiciste clic*,
  escribiendo. Las flechas mueven, Enter elige; la paleta de la izquierda sigue
  siendo lo que hojeas.
- **Doble clic en una arista** — o *Insertar skill aquí* — para meter un nodo
  dentro de una conexión existente. La mitad de arriba conserva el
  `deadline_ms` y la `priority` de ese salto, porque describían ese salto y
  siguen haciendo, mientras que el gate de la regla 5 se va a la mitad que
  ahora termina en un skill motor.
- **Doble clic en un nodo** para renombrarlo en el sitio. Una ref que C2
  rechazaría, o una ya usada, se rechaza en vez de corregirse en silencio.
- **Alinear y repartir** una selección múltiple, desde su menú.
- **Notas adhesivas** (`N`), arrastrables, redimensionables, cinco colores.
  Viven en el almacenamiento local junto a las posiciones y deliberadamente
  *no* en el IR: una nota es un hecho sobre lo que entiende una persona, y dos
  grafos que solo difieren en su prosa tienen que seguir siendo idénticos byte
  a byte en el cable.
- **`Ctrl+Shift+C` / `Ctrl+Shift+V`** copian y pegan el grafo entero como IR por
  el portapapeles del sistema — un flujo que puedes pegar desde un mensaje de
  chat, que es el truco de interoperabilidad que vale la pena robar.
- **Apagar un nodo** (`D`). Se queda en el lienzo, atenuado y tachado, y
  desaparece del IR: cada camino que pasaba por él se reconecta rodeándolo,
  uniendo cada entrada con cada salida. Apagar un conversor puede dejar dos
  puertos cableados que no encajan, y el validador lo dice — contra el nodo
  que apagaste, porque la arista que lo muestra es sintetizada y no tiene caja
  donde hacer clic.
- **Marcos** (`G`), para decir "estos cuatro son el camino de reintento". Un
  marco arrastra lo que contiene, y la pertenencia es geométrica y no una
  lista guardada: arrastra un nodo dentro y entra, sácalo y sale, sin nada que
  quede obsoleto cuando un nodo se renombra o se borra.
- **Pinear la salida de un nodo**, para trabajar el resto del grafo sin
  ejecutar la parte lenta, cara o irreversible. Ver abajo.
- **Historial de versiones** (`H`) — todas las versiones de este grafo que el
  nodo ha tenido.
- **`?`** lista cada atajo, desde la misma tabla de la que los menús sacan sus
  pistas, para que no puedan separarse.

Las notas, los marcos y el estado encendido/apagado viven en el almacenamiento
local junto a las posiciones, por una razón: C2 no tiene campo para ninguno y
no debe ganarlo. Dos grafos que se ejecutan igual tienen que seguir siendo
idénticos byte a byte en el cable, o `aura verify` empieza a comparar prosa y
maquetación.

### Salidas pineadas

Pinea un nodo y el kernel deja de despacharle: lo que pineaste pasa a ser su
salida, y el skill no se ejecuta. Itera sobre lo que consume la respuesta de un
modelo sin pagar el modelo; construye la rama que maneja la respuesta de una API
sin llamar a la API.

Toda herramienta de flujos tiene alguna versión de esto. Lo que aquí es
distinto se deriva de lo que este runtime afirma de sí mismo — que su registro
de lo que pasó es verdadero — y cuesta tres reglas:

1. **Rechazado en modo `published`.** Un pin es una comodidad de desarrollo. En
   una red pública, una sesión cuyos resultados eligió quien la abrió no es
   evidencia de nada, y la respuesta honesta es rechazar, no anotar.
2. **Anunciado en la cadena.** Cada entrega pineada emite un `status` que nombra
   el nodo antes del payload que lo sustituye, y ese status va al log causal, así
   que `aura why` lo enseña. Alguien que nunca haya oído hablar de pinear sigue
   sin poder confundir una salida pineada con el trabajo del skill.
3. **Validado contra el manifiesto.** El puerto tiene que ser uno que el nodo
   realmente tenga, y el esquema el que ese puerto declara. Un pin que no podría
   haber salido de ese nodo es una mentira que todo lo de aguas abajo se creería.

```
$ aura why <sesión>
  status  pinned  node=eco port=text_out
          output supplied by the caller; eco was not run
  data    eco.text_out  {"text":"PINNED ANSWER"}
```

Los pins viajan en `config_update`, que C3 ya tiene, y solo antes del primer
envelope de datos: cambiar lo que produce un nodo a mitad dejaría un mismo id
de sesión describiendo dos grafos distintos.

### Historial de versiones

`SaveGraph` añade una revisión cada vez que un registro cambia el documento,
indexada por el **SHA-256 del IR**. Ese digest es lo que hace de una versión un
hecho y no una marca de tiempo: dos revisiones con el mismo digest son el mismo
grafo. Volver a registrar algo sin cambios no añade nada, lo que importa porque
`aura up` resiembra sus grafos de ejemplo en cada arranque.

```
GET /v1/graphs/{id}/revisions       todas las versiones, la más nueva primero
GET /v1/graphs/{id}/revisions/{n}   el IR de una versión
```

Cargar una versión antigua la pone en el lienzo y la deja ahí, sin registrar.
No revierte el nodo — eso haría del historial algo reescribible, y la única
propiedad que vale la pena tener es que solo crece. Registra el grafo
restaurado y la lista gana una entrada cuyo digest coincide con el antiguo, que
dice exactamente lo que pasó: volviste atrás, a propósito, en un momento
conocido.

**Las réplicas se muestran una vez.** El catálogo guarda una entrada por
*conexión* viva, así que dos procesos sirviendo un skill son dos entradas. La
paleta las agrupa en una fila con una insignia `×2`, y "cuántos skills
satisfacen esta capacidad" cuenta skills distintos — decir dos mandaría a un
autor a buscar una segunda implementación que no existe. Ver
[Los skills sobreviven a su nodo](#los-skills-sobreviven-a-su-nodo).

**El IR que emite es puro según la especificación.** Sin coordenadas ni claves del
editor: un grafo dibujado aquí es comparable byte a byte con uno escrito a mano.
Las posiciones viven en el almacenamiento local del navegador indexadas por id de
grafo, y un grafo abierto en una máquina que nunca lo ha visto se dispone a partir
de su topología.

La validación corre las reglas C2 que correrá el executor, mientras dibujas, y
nunca lo sustituye. La regla 5 (el invariante del gate motor) se reporta según el
modo del nodo, porque `local` repara un gate omitido y `published` rechaza la
sesión. La regla 6 (especulación hacia un skill motor, o junto a un gate
human-approval) y la regla 2 (compatibilidad de esquemas entre puertos) bloquean
el registro directamente.

**El rail en vivo** de la izquierda es lo que el nodo está corriendo, lo haya
puesto esta UI o no. Un grafo registrado por `aura do`, traído por un peer
federado o instanciado por una ruta de webhook aparece ahí con su número de
sesiones vivas, de eventos y de errores. Con *Siguiendo* activo, el lienzo abre un
grafo que apareció por su cuenta — con la guarda de que nunca descarta un dibujo
sin registrar que tengas en curso.

### La referencia

Todos los comandos del CLI agrupados como los agrupa esta guía, los cinco
contratos congelados completos, todos los esquemas JSON y todas las enumeraciones
de los contratos — con búsqueda, y servidos por el propio nodo, así que funciona
sin red.

Nada de eso está escrito a mano. `scripts/gen_ui_reference.py` lo genera desde las
tablas de comandos de esta guía, el propio bloque de uso del binario y `spec/`, y
CI falla cuando el archivo generado está obsoleto **o cuando el binario ofrece un
comando que esta guía nunca documentó**. Así que "la referencia está completa" es
una afirmación comprobada, no una intención.

Para desarrollar la UI contra un kernel en ejecución:

```powershell
cd ui
npm install
npm run dev      # servidor de desarrollo Vite en :3000, con proxy de API + WS al kernel
npm test         # modelo del grafo + parser Markdown (node:test, requiere node 22.6+)
npm run build    # emite en kernel/internal/gateway/ui/dist (re-embeber: recompila el kernel)
```

Regenera la referencia tras cambiar una tabla de comandos, un contrato o un esquema:

```powershell
python scripts/gen_ui_reference.py          # escribe
python scripts/gen_ui_reference.py --check  # lo que corre CI
```

---

## Escribir un skill (el SDK)

Un skill es la unidad publicable más pequeña. Es un directorio con un manifiesto
(`skill.yaml`), el código y sus dependencias. El SDK de Python
([`sdk/python/`](sdk/python/), el paquete `aura`) gestiona la conexión, el
registro, la causalidad y la idempotencia — tú escribes handlers.

**`skill.yaml`** (contrato C1) declara identidad, tipo, capacidad y puertos
tipados:

```yaml
id: "acme/logical/uppercase"       # org/categoría/nombre — una identidad global
version: "1.0.0"
protocol: "1"

name: "Uppercase"
description: "Pone en mayúsculas el texto que recibe."   # el planner LEE esto
capability: "logical.uppercase"                          # <tipo>.<función> — resoluble
type: logical

format: source
runtime: { language: python, version: ">=3.11" }

ports:
  ingress:
    - name: text_in
      schema: "std/text@1"                        # todo puerto DEBE declarar un esquema
  egress:
    - name: text_out
      schema: "std/text@1"

permissions:                                      # basados en capacidades; lo no listado = denegado
  egress_http: []
  filesystem: none
  channels: declared-only
```

**`main.py`** — conectar, registrar, manejar:

```python
from aura import Context, Skill

skill = Skill()                    # lee skill.yaml del directorio de trabajo

@skill.on("text_in")
async def handle(ctx: Context) -> None:
    text = (ctx.payload or {}).get("text", "")
    await ctx.emit("text_out", {"text": text.upper(), "final": True})

if __name__ == "__main__":
    skill.run()                    # se conecta al kernel, reconecta para siempre
```

Ejecútalo desde su propio directorio — el SDK lee `skill.yaml` del directorio de
trabajo — con `python main.py`. Apúntalo a otro sitio con
`AURA_WS_URL=ws://host:9080/ws/skill`, o instálalo y ejecútalo desde el registro
con `aura run acme/logical/uppercase`.

**La credencial se encuentra, no se configura.** Un nodo ata loopback y genera un
token bearer por defecto, y `/ws/skill` está detrás de él como cualquier otra
ruta. Ambos SDKs lo resuelven exactamente donde lo hace el CLI — `AURA_TOKEN`, y
luego `~/.aura/node.token` — así que un skill arrancado por el mismo usuario en la
misma máquina no necesita nada. Pásalo explícitamente con `Skill(token=...)` en
Python o `createNode({ token })` en TypeScript cuando el nodo guarde sus datos en
otro sitio; un nodo arrancado con `--no-auth` no tiene archivo de token y no pide
credencial, y una cadena vacía significa "no envíes nada". Un skill rechazado por
falta de credencial registra el 401 y qué hacer al respecto, en vez de reintentar
en silencio.

**Puntos clave de diseño.** Los skills se conectan *hacia fuera* al kernel
(inversión de control), así que atraviesan NAT y firewalls corporativos sin
puertos de entrada. El `Context` te da `emit` (una respuesta enlazada
causalmente), `status`, `error` y `done`; cada mensaje que envía queda
automáticamente encadenado al que lo causó y recibe una clave de idempotencia.
Los handlers deben ser idempotentes — la entrega es at-least-once por contrato.

### Los skills sobreviven a su nodo

La otra mitad de "los skills se conectan *hacia fuera*": reconectan para
siempre por contrato, así que parar un nodo no los para. Siguen reintentando y
se enganchan al nodo que venga después — que es lo que permite reiniciar un
nodo sin que un operador reinicie diez procesos detrás, y también es cómo
acabas con dos de todo.

La vía habitual: `Ctrl+C` es limpio (`aura up` para los hijos que lanzó), pero
un `kill -9`, una terminal que se cae o un nodo que mata el sistema operativo no
lo son, y el siguiente `aura up --with-examples` arranca un segundo juego al
lado de los supervivientes. Los síntomas son un catálogo duplicado y sesiones
cayendo en un proceso que nadie recuerda haber arrancado.

**Correr N réplicas de un skill está soportado y es deliberado** — el registry
rota entre ellas, así que N copias sirven de verdad N veces las sesiones. Por
eso el nodo no rechaza una segunda conexión. Lo dice, una vez, en el momento en
que pasa:

```
level=WARN msg="skill has more than one connection"
  skill=example/cognitive/llm-chat instances=2
  note="deliberate for replicas; otherwise a process outlived an earlier node"
```

En el evento de registro y no al arrancar, porque es el único sitio donde se
sabe: un huérfano que reconecta puede llegar diez segundos después de que el
nodo termine de arrancar, mucho después de que cualquier comprobación de
arranque haya corrido y dado el visto bueno. `aura up --with-examples` además
se salta lanzar un skill que ya esté conectado cuando mira, y lo reporta como
`already up`.

Si no fue deliberado, para los procesos de más (`pkill -f skills/` en Unix,
`Get-Process python3.13 | Stop-Process` en Windows) y vuélvelos a arrancar.

### Esquemas estándar

Los puertos referencian esquemas versionados. El espacio de nombres `std`
incluye estos:

| Esquema | Forma |
|---|---|
| `std/text@1` | `{ "text": string, "final": bool? }` |
| `std/status@1` | `{ "state": "working"\|"done"\|"error", "detail": string? }` |
| `std/document@1` | `{ "mime": string, "bytes_b64": string, "uri": string? }` |
| `std/transcript@1` | `{ "text": string, "final": bool, "replace": bool?, "utterance": string?, "confidence": number? }` |
| `std/audio-chunk@1` | `{ "pcm_b64": string, "sample_rate": int, "channels": int?, "final": bool?, "seq": int?, "ref": string? }` |
| `std/api-request@1` | `{ "params": obj?, "query": obj?, "headers": obj?, "body": any? }` |
| `std/api-response@1` | `{ "ok": bool, "status": int, "body": any?, "dry_run": bool?, "error": string? }` |
| `std/confirmation@1` | `{ "question": string, "options": [string], "held": string, "to_ref": string?, "to_port": string? }` |
| `std/plan@1` | `{ "reasoning": string, "graph": <IR C2>, "inputs": [...] }` |

Son JSON Schemas ejecutables en [`spec/schemas/std/`](spec/schemas/std/) que la
suite de conformidad comprueba — no solo prosa.

**`text` y `transcript` no son intercambiables.** En `std/text@1`, `text` es un
*delta* — un skill en streaming emite uno por token y el consumidor concatena.
En `std/transcript@1` *reemplaza* la hipótesis anterior, porque un reconocedor
de voz redecodifica todo su buffer y la hipótesis 3 no es la 2 más un sufijo.
Llevar parciales de voz como `std/text@1` haría que todo consumidor que
concatena produjera basura, y por eso son schemas distintos en puertos
distintos.

### Skills Wasm

`format: wasm` (Fase 3, [kernel/internal/wasmrt](kernel/internal/wasmrt)) es
la otra forma de escribir un skill, para un caso específico: una
transformación síncrona `logical`/`motor` donde quieres que
`permissions.filesystem` sea un sandbox que el kernel de verdad aplica, no
una línea que `aura add` solo imprime antes de instalar. Compila a un
binario `.wasm` (`GOOS=wasip1 GOARCH=wasm go build`, o cualquier otro
toolchain que apunte a WASI — TinyGo, Rust, C) y se aloja **dentro del
propio proceso del kernel**, nunca como uno aparte: `aura run` detecta
`format: wasm` en el manifiesto instalado y hace `POST` del módulo
compilado al kernel corriendo (`POST /v1/skills/wasm`) en lugar de lanzar
nada.

El contrato del guest es deliberadamente angosto — un módulo WASI
**command** (el mismo modelo que un script CGI: una instanciación por
entrega, no un proceso de larga vida), exactamente un puerto de ingreso y
uno de egreso. El kernel escribe el payload entregado en el stdin del guest
y lo cierra; el guest escribe su respuesta en stdout y sale con 0, o
escribe un error en stderr y sale con código distinto de cero:

```yaml
format: wasm                    # en vez de source
ports:
  ingress: [{ name: text_in, schema: "std/text@1" }]   # exactamente uno de cada
  egress:  [{ name: text_out, schema: "std/text@1" }]

permissions:
  filesystem: "read:/data/lookup"     # o write:<path>, u omitir para ninguno — un
                                       # preopen de directorio WASI, aplicado no declarado
  egress_http: ["api.example.com"]    # solo hostnames exactos; omitir o [] para ninguno
```

Los dos permisos se aplican de verdad, no solo se le muestran a un humano
al instalar (`aura add`). `filesystem` es un preopen de directorio WASI —
sin concesión el guest no puede abrir ni un archivo; con una, queda
encerrado exactamente a ese directorio. `egress_http` es un host import
propio (`env.http_fetch`, porque WASI preview1 no tiene sockets en
absoluto): el guest es dueño de sus propios buffers de request y respuesta,
y el kernel solo completa un fetch cuando el hostname de la URL coincide
**exactamente** con la lista — sin prefijo ni sufijo. El permiso viaja en
el `context.Context` de cada entrega, así que dos skills con distintas
concesiones corriendo al mismo tiempo nunca ven la del otro — ver
[kernel/internal/wasmrt](kernel/internal/wasmrt).

Los skills `sensorial`/`cognitive` que necesitan streaming siguen siendo
`format: source` — un módulo WASI command corre una vez y sale, así que
todavía no hay contrato de guest para un skill que emite más de una
respuesta por entrega.

---

## Configuración de skills en tiempo de ejecución

Cualquier skill puede declarar parámetros ajustables — no solo perillas de
LLM como `temperature`/`max_tokens`, sino cualquier cosa: un timeout, un
umbral de confianza, una ruta de almacenamiento. Un bloque `config` en
`skill.yaml` (C1, aditivo) declara qué existe; **no** declara el valor
actual, porque un manifiesto publicado es inmutable (regla 1 de C1) — un
valor en vivo no puede vivir dentro de él.

```yaml
config:
  - key: "temperature"
    type: float                 # string | int | float | bool | enum
    default: 0.7
    min: 0.0
    max: 2.0
    description: "Temperatura de muestreo."
    restart_required: false     # true = solo aplica en la próxima conexión del skill
```

Los valores efectivos se resuelven fuera del manifiesto, en prioridad
ascendente: **`default` declarado** → **un archivo `--config` dado a
`aura up`** (git-friendly, releído en cada arranque del kernel) →
**un override en vivo** puesto vía la UI o `PUT /v1/skills/config?id=<id>`
(persistido por el kernel, sobrevive reinicios). La pestaña Skills de la UI
del plano de control renderiza un formulario a partir del esquema declarado
para cualquier skill conectado que tenga uno — sin código que escribir para
tener un editor; y los mismos valores efectivos son lo que un archivo
`--config` expresa como YAML plano para defaults reproducibles:

```yaml
# aura.config.yaml
skills:
  "deepaxiom/cognitive/llm-chat":
    temperature: 0.3
    max_tokens: 2048
  "deepaxiom/memory/context-window":
    max_context_tokens: 6000
    trim_strategy: "summarize"
```

```powershell
.\kernel\aura.exe up --config aura.config.yaml
```

Un skill lee sus valores iniciales del ack de registro del kernel
(`skill.config`) y recibe un empuje en vivo — el mismo dict, in-place — si
un valor cambia mientras está conectado, salvo que todas las claves
cambiadas sean `restart_required`, en cuyo caso aplica en la siguiente
reconexión del skill. Un skill que nunca declara `config` no se ve
afectado — esto es opt-in y totalmente retrocompatible con cualquier skill
escrito antes de que existiera. `skills/llm-chat` (`temperature`,
`max_tokens`, `system_prompt`, `context_window`) y `skills/memory-context`
(abajo) son los ejemplos trabajados para copiar.

---

## Conectar software existente

No *escribes* un adaptador para un sistema legacy — lo *generas* por
introspección.

```powershell
.\kernel\aura.exe connect --openapi https://my-erp.com/api/spec.json
#  proyección "my-erp" registrada: 214 operaciones
#    lecturas ACTIVAS (solo lectura)  ·  escrituras DESHABILITADAS
#    habilita una:  aura promote my-erp create-invoice --mode dry-run
```

Cada operación de la especificación se convierte en un skill individual: las
operaciones de lectura reciben la capacidad `sensorial.api.*` y se activan de
inmediato; las de escritura reciben `motor.api.*` y empiezan
**deshabilitadas**. Los parámetros de path/query y los cuerpos de petición se
mapean sobre `std/api-request@1`, y la descripción de cada operación se redacta
para que un planner pueda leerla y saber cómo llamarla.

Cuatro salvaguardas protegen al sistema objetivo, siempre:

1. **Solo lectura por defecto** — nada escribe hasta que tú lo digas.
2. **Promoción explícita por operación** — habilitas una escritura cada vez.
3. **Modo dry-run** — `--mode dry-run` devuelve la petición exacta que
   *enviaría* sin enviarla.
4. **Aprobación humana** — toda arista hacia una escritura queda gateada por el
   propio kernel (ver [el invariante de la puerta motor](#modelo-de-seguridad)).

```powershell
.\kernel\aura.exe projections                                # ver los modos
.\kernel\aura.exe promote my-erp create-invoice --mode dry-run
.\kernel\aura.exe promote my-erp create-invoice --mode live  # escrituras reales
```

El host de proyecciones corre dentro del binario pero habla con el kernel como
un cliente ordinario — no tiene privilegios especiales, y el código del sistema
objetivo nunca se toca.

### Cuando no hay documento OpenAPI

La introspección necesita algo que introspeccionar. El caso común es el
contrario: un endpoint suelto de un backend que nadie documentó. Escribir un
OpenAPI completo de una API de la que solo necesitas tres llamadas no es un
precio razonable, y escribir un skill a mano tampoco.

Un conector declarativo — un skill chico que lee unas pocas líneas de YAML y
registra cada operación como un skill ordinario — hace el mismo trabajo que la
proyección OpenAPI sin un documento OpenAPI. No es parte de los skills de
primera parte de este repo, pero la forma es apenas un poco de glue
`Skill`/`Context` (ver [Escribir un skill](#escribir-un-skill-el-sdk)) que lee
un spec como:

```yaml
name: legacy-erp
base_url: http://erp.internal
headers:
  Authorization: "Bearer ${ERP_TOKEN}"      # del entorno, nunca del archivo
operations:
  - { op_id: get-order,    method: GET,  path: /orders/{id}, params: [{name: id, in: path}] }
  - { op_id: create-order, method: POST, path: /orders }
```

y registra `sensorial.api.legacy_erp.get_order` (activo) y
`motor.api.legacy_erp.create_order` (deshabilitado hasta promoverse) contra
él — las mismas cuatro salvaguardas, las mismas capacidades, la misma puerta
que el camino OpenAPI. La diferencia está en dónde corre: esto es un
**skill**, no código del kernel, así que se instala desde el registro como
cualquier otra cosa y nunca engorda el binario que va a un dispositivo de
borde.

La promoción tampoco necesita mecanismo nuevo — el `mode` por operación es
[configuración en tiempo de ejecución](#configuración-de-skills-en-tiempo-de-ejecución)
declarada, así que el kernel la valida, la persiste entre reinicios y la empuja
al conector en marcha:

```powershell
# o simplemente cámbialo en la pestaña Skills de la UI
curl -X PUT "localhost:9080/v1/skills/config?id=connector/motor/legacy-erp-create-order" `
     -H "Content-Type: application/json" -d '{\"mode\":\"dry-run\"}'
```

Las credenciales se leen del entorno (`${VAR}`), nunca se escriben en el spec, y
la API nunca las devuelve — `GET /v1/projections` redacta los valores de las
cabeceras conservando sus nombres, de modo que un operador puede ver que hay
auth configurada sin que el plano de control reparta la clave.

### Sin escribir tampoco el spec

Si el sistema ya está corriendo y ya se usa, puedes saltarte también el YAML.
Pones un proxy delante, usas la app con normalidad, y la forma de sus llamadas
reales se convierte en el conector:

```powershell
.\kernel\aura.exe observe --port 8080 --target http://localhost:3000
#   apunta tu app a :8080 y úsala unos minutos

.\kernel\aura.exe generate connector --from observed.jsonl --name mi-app
#   6 call(s) observed → 2 operation(s): 1 read, 1 write
```

Derivarlo del tráfico le gana a escribirlo a mano por la misma razón por la que
existe `aura replay`: el tráfico grabado es lo que el sistema *hace*, no lo que
alguien creía que hacía. Las llamadas que solo difieren en un id se pliegan en
una sola operación con un `{param}`.

**Lo que contiene una grabación es estrecho por construcción.** Solo se anotan
el método, la ruta, los *nombres* de los parámetros de query, la *forma* de los
cuerpos (nombres de clave y tipos JSON) y los códigos de estado. Nunca el valor
de una cabecera, ni el de un parámetro de query, ni el de un cuerpo — así que
una grabación normalmente se puede pegar en un issue sin limpiarla. Una
salvedad que queda de tu lado: la ruta se anota literal, así que una API que
mete un token *en la ruta* lo mete en la grabación.

El archivo generado se abre para revisión y nunca se ejecuta. Todo lo que
contiene es una inferencia sobre el sistema de otro, así que un humano lo
confirma antes de que pueda actuar — el mismo principio que hace que las
escrituras empiecen deshabilitadas.

### Cuando la app es tuya

Los dos caminos anteriores envuelven un sistema desde fuera. Cuando el código es
tuyo, la integración más barata es que la app se anuncie a sí misma — sin spec
que escribir y sin adaptador que mantener sincronizado con la lógica que
envuelve:

```ts
import { createNode } from "@deepaxiom/aura";

const aura = createNode({ org: "acme", app: "shop" });

aura.expose("get-order",    ({ id }) => db.orders.find(id), { params: ["id"] });
aura.expose("create-order", (input) => db.orders.create(input), { write: true });

await aura.start();
```

Esa es toda la integración. Esas dos funciones ya son
`sensorial.api.shop.get_order` y `motor.api.shop.create_order`: resolubles en
grafos, legibles por el planner, invocables como herramientas MCP y —porque la
segunda declara `write: true`— gateadas con aprobación humana en toda arista que
llegue a ellas, incluso en grafos cuyo autor olvidó pedirlo.

El paquete ([`sdk/node/`](sdk/node/), `@deepaxiom/aura`) **no tiene dependencias
de runtime** —usa el WebSocket que ya traen Node 20+ y el navegador— y sus tipos
se generan desde `spec/schemas/`, así que no pueden divergir de los contratos
congelados. También incluye un cliente de sesión tipado, que es lo que usa un
frontend en vez de copiar el `useSession.ts` del plano de control:

```ts
const session = await openSession("chat", { onEnvelope: render });
const id = session.sendText("hola");
session.cancel(id);            // lo abandonas; no llega nada más
```

Un error lanzado se convierte en `{ ok: false, status: 500, error }` en vez de en
un skill muerto: quien preguntó recibe una respuesta sobre la que puede actuar.

### Eventos que vienen en sentido contrario (webhooks)

Los skills marcan *hacia afuera* al kernel, que es lo que les permite correr
detrás de NAT sin puertos de entrada. Quien manda un webhook hace lo contrario:
Stripe o GitHub marcan hacia adentro, y no pueden alcanzar a un skill que no
tiene dirección. Así que el HTTP entrante es la única pieza de conectividad que
sí tiene que vivir en el kernel — es transporte, no integración.

Declaras una ruta y el kernel te da la URL que le pasas al emisor:

```powershell
$env:STRIPE_SECRET = "whsec_..."          # el kernel lo lee, nunca lo guarda

curl -X POST localhost:9080/v1/ingress -H "Content-Type: application/json" `
     -d '{\"name\":\"stripe\",\"graph\":\"billing\",\"secret_env\":\"STRIPE_SECRET\"}'
#  → { "url": "/hooks/stripe", ... }
```

Cada entrega se verifica con HMAC-SHA256 contra ese secreto, comparado en tiempo
constante, y abre **su propia sesión** en el grafo destino — así que un webhook
obtiene el mismo log causal de eventos que cualquier otra cosa, y
`aura why <sesión>` explica qué provocó ese evento. Una firma válida reenviada
sobre un cuerpo modificado se rechaza, que es el ataque que importa.

La verificación está activa por defecto: una ruta sin `secret_env` se rechaza al
declararla, en vez de convertirse en silencio en un endpoint público sin
autenticar que inyecta en un grafo. Aceptar entregas sin firmar es posible pero
hay que pedirlo (`"unsigned": true`). Las rutas se pueden listar y revocar
(`DELETE /v1/ingress/{name}`) — una URL de entrada que no puedes revocar es un
problema en sí misma — y el listado nunca devuelve el secreto, solo el nombre de
la variable de la que sale.

### Un cambio de fila se vuelve un evento causal (CDC de Postgres)

Los sistemas legacy sin API son un caso común como para nombrarlo aparte:
[`skills/postgres-cdc`](skills/postgres-cdc) convierte el propio stream de
replicación lógica de Postgres en eventos `sensorial.postgres.cdc` — un
skill, como cualquier conector de arriba, no una funcionalidad del kernel.

```bash
ALTER SYSTEM SET wal_level = logical;   # una vez, en la base origen; después reiniciarla

PG_CDC_DSN="host=... dbname=... user=... password=..." \
    cd skills/postgres-cdc && PYTHONPATH=../../sdk/python/src python main.py
```

Usa `test_decoding` — incluido en el núcleo de Postgres desde la 9.4, así
que no hay nada que instalar en la base destino. Un mensaje `watch_in`
arranca el stream; sigue emitiendo eventos `std/db-change@1` (`{ table, op,
columns, lsn }`, uno por fila cambiada) hasta que la sesión lo cancela.
`config.tables` lo acota a `schema.tabla`s específicas; por defecto es toda
la base. El DSN se lee de `PG_CDC_DSN`, no de un campo `config` —
los valores de `config` se pueden leer vía `GET /v1/skills/config`, y una
cadena de conexión lleva una contraseña.

---

## Hablarle

`aura up` siembra un grafo `voice`, y la UI del plano de control lo maneja desde
la pestaña de conversación del Estudio (transporte: *Voz*). Arranca los cuatro
skills que resuelve, pulsa empezar y habla:

```powershell
$env:PYTHONPATH = "sdk\python\src"
cd skills\asr;              python main.py    # sensorial.asr.transcribe
cd skills\llm-chat;         python main.py    # cognitive.llm.chat
cd skills\sentence-chunker; python main.py    # logical.text.sentence_chunk
cd skills\tts;              python main.py    # motor.tts.speak
```

Tus palabras aparecen mientras las dices, la respuesta se habla mientras se
escribe, y hablarle encima la para. Nada de eso necesita una cuenta en la nube.

**Cinco puertos de cliente en un socket, a la vez** — audio de subida, audio de
bajada, transcripciones parciales, texto de la respuesta, estado (este último
alimentado por dos aristas, la del reconocedor y la del sintetizador). Eso es
lo que significa "multicanal" aquí, y el kernel siempre lo soportó; el grafo de
voz es lo primero que lo usa. Cada arista declara lo que necesita:

```jsonc
{ "from": "client.audio_out",      "to": "ears.audio_chunk_in" },              // reliable: perder audio de entrada degrada el reconocimiento en silencio
{ "from": "clause.text_out",       "to": "mouth.text_in",     "gate": "none" }, // hablar es motor.*; dilo en voz alta para que no pida clic
{ "from": "mouth.audio_chunk_out", "to": "client.audio_in",   "qos": "realtime" } // un frame rancio no vale nada
```

**La interrupción es la parte difícil, y es la razón de ser del resto.** Cuando
hablas encima del asistente, el navegador para la reproducción localmente
primero —al instante, con un fundido de 20 ms para que el corte no haga un clic
lo bastante fuerte como para retriggerear el micrófono— y solo después le dice
al kernel que abandone la cadena. El kernel deja de rutear todo lo que le
pertenezca, a cualquier profundidad, así que un skill que ignore el cancel y
siga generando produce salida que no llega a ningún sitio. Medido: una pregunta
nueva desplaza a la anterior en ~0,2s.

Cosas que conviene saber antes de enchufar un micrófono:

- **El micrófono necesita un contexto seguro.** `localhost` vale; una dirección
  de LAN por `http://` plano no, y falla con un error de permisos confuso.
- **Usa auriculares, o marca "silenciar mientras habla".** La cancelación de eco
  del navegador es buena pero no perfecta, y un asistente que se oye a sí mismo
  se interrumpe solo.
- **Fija el idioma del ASR** si lo sabes. La autodetección sobre una frase corta
  es medible-mente peor — la misma frase se transcribe limpia con pista y como
  casi galimatías sin ella.
- **piper es de facto necesario para una voz que se sienta viva.** La voz del SO
  funciona y se trocea igual, pero solo sabe sintetizar archivos enteros, así
  que el sonido empieza más tarde.

---

## Operación en lenguaje natural

Con el skill planner en ejecución (`skills/planner/`), `aura do` cierra el bucle
desde un objetivo expresado en lenguaje natural hasta un resultado ejecutado con
puertas de aprobación:

```
> aura do "crea un usuario llamado Grace Hopper en el crm"
  plan › Usar motor.api.crm.create_user para crear el usuario.
  step › s1 → motor.api.crm.create_user
  gate › ¿Aprobar la entrega a s1.request_in?  [y/N]: y
  result ‹ { "ok": true, "status": 201, "body": { "id": 99, "name": "Grace Hopper" } }
  objetivo completado
```

La división del trabajo es deliberada y es lo que lo hace fiable incluso con un
modelo local pequeño: **el LLM solo elige y extrae** (qué skills, qué
parámetros — la parte difusa), y **el código compila el plan** en un grafo
válido (la parte exacta). La política de seguridad vive en el código, no en el
prompt: cada arista hacia un skill `motor.*` recibe automáticamente una puerta
`human-approval`. El planner además repara alucinaciones menores (un prefijo de
capacidad erróneo con una única coincidencia real) y normaliza parámetros mal
colocados contra la forma declarada de cada proyección.

Los backends del planner forman una cadena de degradación declarada: `openai` →
`local` (cualquier GGUF vía llama.cpp) → `keyword` (un fallback determinista sin
modelo). El último significa que la planificación funciona incluso sin ningún
modelo instalado.

---

## Puertos tipados, forzados

C1 obliga a que cada puerto declare un schema, y la regla 2 de C2 se niega a
cablear dos puertos cuyos schemas no coinciden. Así que el runtime conoce,
*estáticamente*, la forma exacta de todo lo que un skill puede emitir. De ahí
salen dos cosas que ningún otro runtime de agentes está en posición de ofrecer,
porque ninguno tiene puertos con schema obligatorio.

**Una gramática de decodificación derivada del tipo del puerto.** El kernel
compila el JSON Schema de cada puerto a una gramática GBNF y se la entrega al
skill al registrarse. Un skill que genera bajo ella *no puede* emitir una forma
que el puerto rechace — no "rara vez lo hace", no puede:

```python
grammar = skill.grammar_for("plan_out")     # la empuja el kernel al registrarse
out = llm.create_completion(prompt, grammar=grammar)
```

```powershell
curl localhost:9080/v1/grammars/std/status@1
#  root-state-1 ::= "\"working\"" | "\"done\"" | "\"error\""
```

La decodificación restringida no es nueva — XGrammar, llguidance y Outlines lo
hacen bien, y todo stack de serving trae uno. Lo inusual es *de dónde sale la
gramática*. En todos los demás sale de un schema que el autor escribió a mano y
pasó a la llamada de inferencia, así que solo es tan correcta como su disciplina
y se desvía de lo que consume la salida. Aquí sale del tipo del puerto al que va
la salida — lo que efectivamente la va a rechazar.

El orden de propiedades sigue el orden de declaración del schema y no el
alfabético, deliberadamente: `std/transcript@1` declara `text` antes que
`final`, y un modelo obligado a comprometerse con `final` antes de escribir el
texto que describe genera peor — sobre todo los modelos locales pequeños a los
que apunta este runtime.

**Validación para todo lo que no genera.** El mismo schema comprueba los
payloads al pasar, así que un skill que arma un dict a mano, una API proyectada
que devuelve una sorpresa o un cliente que postea cualquier cosa también quedan
sujetos al tipo del puerto. La gramática vuelve inalcanzable una violación; el
validador la vuelve rechazada. Hacen falta las dos mitades para que "canal
tipado" sea una afirmación y no una descripción.

El compilador cubre el subconjunto que usa el namespace `std` y **rechaza
cualquier cosa fuera de él** en vez de emitir una gramática permisiva — una
gramática que permite más que el schema es peor que ninguna, porque parece una
garantía.

---

## Scheduling: especulación, deadlines, presupuestos

### Ejecución especulativa de grafo

El coste dominante en un grafo de agentes es la espera secuencial: un modelo
transmite dos segundos y solo entonces empieza el siguiente skill. Arrancar ese
skill temprano sobre el prefijo es una ganancia obvia y muy estudiada — PASTE,
SPORK, SpecBox y toda una literatura de 2026. Cada uno de esos sistemas gasta
la mayor parte de su esfuerzo en la misma pregunta: **¿qué pasos son seguros de
correr antes de estar seguros?** Correr temprano una herramienta que manda un
email o cobra una tarjeta no es una optimización de latencia, es un bug con
cronómetro. Responden con heurísticas, listas blancas, o el criterio de un LLM.

Este runtime no tiene que preguntar. Los cinco tipos de skill de C1 son un
**sistema de tipos de efectos** — `motor` es precisamente "actúa sobre el
mundo" — así que la respuesta ya está en el manifiesto, estáticamente, para
cada skill del grafo:

```json
{ "from": "brain.text_out", "to": "summariser.text_in", "speculative": true }
```

```
edge brain.text_out -> writer.text_in declares speculative but
"acme/motor/erp-writer" is a motor skill; work that acts on the world is never
run ahead of certainty, because a discarded effect is not discarded
```

Ese rechazo ocurre al cablear, antes de que exista una sesión. Es la invariante
del motor-gate vista desde otro ángulo: el tipo de skill que debe esperar a un
humano es el tipo que nunca debe correr por delante de la certeza.

La reconciliación reutiliza maquinaria que ya existe. El kernel pliega los
parciales en un valor acumulado según la semántica del propio schema
(concatenando en `std/text@1`, reemplazando en `std/transcript@1` — la
distinción que C1 ya traza), lo entrega marcado como completo, y cuando el
productor termina o suprime la entrega redundante o abandona la cadena
especulativa por el camino normal de `cancel`. Un skill que maneja bien cancel
maneja bien un fallo de especulación gratis.

Se reportan aciertos y fallos, porque una funcionalidad que gasta cómputo en
adivinar debería tener que enseñar su historial. Un nodo puede rechazar la
especulación entera con `speculation: deny` en su policy.

### Deadlines que se propagan

```json
{ "from": "client.text_out", "to": "brain.text_in", "deadline_ms": 800 }
```

Se convierte a un instante absoluto en el hop que lo declara y se hereda hacia
abajo, así que una cadena de tres hops no puede concederse en silencio tres
veces el presupuesto añadiendo hops. Un hop puede apretarlo, nunca extenderlo.
Se espera que el skill receptor **degrade** — menos beams, un modelo más chico,
una cuantización más gruesa — en vez de abortar: una respuesta peor a tiempo
gana a una mejor cuando ya nadie escucha.

La propagación de deadlines es estándar en RPC desde hace una década y está
ausente en todo runtime de agentes, que es por qué allí una herramienta lenta
degrada en un cuelgue en vez de en una respuesta más barata.

### Qué modelo respondió, decidido por policy

```yaml
routes:
  - match: "cognitive.llm.*"
    prefer: ["deepaxiom/cognitive/llm-chat-small"]
    reason: "el modelo chico atiende el grafo por defecto; escalar por grafo, no por suerte"
```

La elección de modelo es una cuestión de gobernanza además de de ingeniería —
"qué modelo respondió esto" es algo que un auditor pregunta — así que pertenece
al mismo documento firmado que dice qué puede actuar sobre el mundo, legible por
inspección.

Esto **no** es un router aprendido, deliberadamente. Los clasificadores estilo
RouteLLM y el semantic router de vLLM eligen por consulta y son genuinamente
mejores en coste y calidad; también son inauditables por inspección, que es la
propiedad que aquí se cambia. Los dos componen: pon un router aprendido detrás
de una capacidad `cognitive.*` y rutea hacia él.

### Un presupuesto de contexto que el kernel hace cumplir

```json
{ "ir": "1", "graph_id": "long-session", "context_budget": 8000, "nodes": [] }
```

Un presupuesto que hace cumplir el skill es un presupuesto que solo vale para
los skills que se acordaron de implementarlo, y el modo de fallo difiere en cada
uno — una truncación en uno, un 413 en otro, pérdida silenciosa de datos en un
tercero. Un solo lugar, una sola regla, un solo mensaje de error.

El conteo de tokens es explícitamente una **estimación** (bytes/4, la regla
habitual para BPE). El kernel no tiene tokenizador y no debería adquirir uno,
porque lo acoplaría a una familia de modelos. Un presupuesto presentado como
exacto se usaría para planificación de capacidad que no puede sostener.

---

## Drivers de modelos y admisión de recursos

La voz llega como skills de primera parte — cada uno un driver fino
sobre un runtime nativo, cada uno con una cadena de degradación declarada:

| Skill | Capacidad | Motor |
|---|---|---|
| [`skills/llm-chat`](skills/llm-chat/) | `cognitive.llm.chat` | llama.cpp (cualquier GGUF) por defecto; cualquier API compatible con OpenAI (OpenAI, Gemini, …) si `OPENAI_API_KEY` está definida. Historial por sesión. |
| [`skills/asr`](skills/asr/) | `sensorial.asr.transcribe` | faster-whisper (CPU int8). Acepta un WAV completo en `audio_in`, o un stream PCM en vivo en `audio_chunk_in` con transcripciones parciales mientras la persona todavía habla. Termina una utterance por señal del cliente, por silencio final, o por un tope duro. |
| [`skills/tts`](skills/tts/) | `motor.tts.speak` | piper (opcional) → voces del SO (SAPI/espeak). Emite PCM en trozos de ~200ms para un oyente en vivo, sea cual sea el backend, más la cláusula entera como WAV. |
| [`skills/sentence-chunker`](skills/sentence-chunker/) | `logical.text.sentence_chunk` | Agrupa un stream de tokens en cláusulas. Ponlo entre un LLM en streaming y cualquier cosa que trabaje con frases, o el sintetizador se dispara una vez por token. |
| [`skills/model-manager`](skills/model-manager/) | `motor.models.manage` | listar / catálogo / búsqueda en HF / descargar / borrar / set-active |
| [`skills/model-fit`](skills/model-fit/) | `sensorial.hardware.modelfit` | Lee RAM, CPU y GPU/VRAM y ordena el catálogo por lo que realmente corre aquí, con tokens/s estimados y las suposiciones detrás. Solo librería estándar, así que responde antes de instalar nada. |
| [`skills/memory-context`](skills/memory-context/) | `memory.context.window` | SQLite (embebido, archivo local) — persiste turnos por sesión a través de reinicios, `recall` devuelve una ventana recortada a un presupuesto de tokens configurable (drop-oldest, o resumen vía `aura.llm.ChatBackend`). El historial propio de llm-chat es en proceso y sin límite (ver su `main.py`); este es la alternativa durable y consciente del presupuesto — se cablea a un grafo explícitamente, no se conecta solo. |
| [`skills/postgres-cdc`](skills/postgres-cdc/) | `sensorial.postgres.cdc` | Convierte el stream de replicación lógica propio de Postgres (`test_decoding`, nada que instalar) en eventos `std/db-change@1`, uno por fila cambiada. |

Como todos comparten la interfaz de puertos/esquemas, a un grafo que pide
`sensorial.asr.transcribe` nunca le importa qué motor responde — ese es el plano
de modelos en la práctica. Si quieres algo que corra sin nada en absoluto para
envolver un sistema externo nuevo, empieza por [`skills/echo/`](skills/echo/)
— sin modelo, sin credenciales, sin red.

**Admisión de recursos (hardware finito).** Un nodo arrancado con un presupuesto
de memoria se niega a admitir skills que no caben, con una explicación en lugar
de una muerte silenciosa por falta de memoria:

```powershell
.\kernel\aura.exe up --memory-budget 1536Mi
#   un skill que declara requirements.memory: 1Gi es admitido;
#   un segundo skill de 1Gi es rechazado:
#   "admission rejected: … declares 1.0Gi but only 512Mi of the 1.5Gi budget remains …"
#   la reserva se libera cuando el skill se desconecta; el uso se muestra en /healthz
```

---


### Qué modelo usa cada skill, y cómo cambiarlo

Dos skills cargan un modelo: **llm-chat** (la pestaña de conversación del Estudio) y **planner** (la
vista Operate, y `aura do`). Son procesos separados, así que cada uno responde
la pregunta por su cuenta — y ambos la responden igual, en este orden:

| | Dónde mira | Qué significa |
|---|---|---|
| 1 | `AURA_MODEL_PATH` | Un override explícito, se honra tal cual. |
| 2 | la clave de config `model` de ese skill | **La elección de ese skill.** Pónla para planificar con un modelo y chatear con otro. |
| 3 | `~/.aura/models/active.json` | El default de todo el nodo, escrito por `model-manager set-active`. Dejar la clave vacía es la forma de decir "sigue al nodo". |
| 4 | el default incorporado | `qwen2.5-1.5b-instruct-q4_k_m.gguf`, descargado en el primer uso. |

Cada paso se salta en vez de fallar, así que un ajuste que nombra un archivo
borrado degrada al siguiente en lugar de dejar al nodo sin skill de chat.

**Para que ambos usen el mismo modelo** — el caso común — descarga uno y
márcalo como activo, dejando ambas claves vacías:

```bash
aura do "rank which models fit on this machine" --yes   # qué corre aquí
#   {"action": "download",   "model_id": "gemma-3-4b-it"}
#   {"action": "set-active", "model": "gemma-3-4B-it-Q4_K_M.gguf"}
```

**Para darles modelos distintos**, pon la clave `model` en uno de ellos — desde
la vista **Skills** del plano de control, o:

```bash
curl -X PUT "http://localhost:9080/v1/skills/config?id=example/cognitive/planner"   -H "Authorization: Bearer $(cat ~/.aura/node.token)"   -H 'Content-Type: application/json'   -d '{"model": "gemma-3-4B-it-Q4_K_M.gguf"}'
```

**Para ver qué cargó cada uno**, lee su línea de arranque — ambos registran el
archivo y cuál de las cuatro reglas lo eligió:

```
model: /home/tu/.aura/models/gemma-3-4B-it-Q4_K_M.gguf (via model-manager active.json)
```

`model` es `restart_required` en ambos: cargar otro GGUF significa descargar el
que está en memoria, y hacerlo a mitad de una generación no es una
actualización en caliente.

**Cada uno carga su propia copia.** Dos procesos, dos instancias de llama.cpp —
en una máquina de prueba, 1,9 GB residentes cada uno para un modelo de 1,1 GB.
Ambos además descargan todas las capas a GPU por defecto (`n_gpu_layers: -1`),
así que en una tarjeta pequeña el segundo en arrancar acaba en CPU sin avisar.
`model-fit` dimensiona *un* modelo contra la máquina entera; no sabe cuántos
skills piensan cargar uno. Compartir una sola instancia cargada significaría que
el planner razone a través de `cognitive.llm.chat` por el grafo en vez de
importar llama.cpp — ver [el roadmap](ROADMAP-ES.md).

## Audit bundles

Un estudio de 2026 sobre protocolos de benchmarks de agentes
([arXiv 2607.22368](https://arxiv.org/abs/2607.22368)) examinó trazas
publicadas y encontró que **el 67% contenía "protocol exposures"** — caminos
por los que se puede ganar una puntuación sin que la capacidad medida
intervenga. Su conclusión no fue que hagan falta mejores tareas, sino que los
informes deben incluir la evidencia necesaria para interpretarlos. Y nombra lo
que un runtime tiene que emitir para que eso sea posible:

| Lo que pide el estudio | Este runtime ya lo tenía, por otras razones |
|---|---|
| Logs de trayectoria completos — llamadas, orden, timestamps | El log causal (C3 regla 7) |
| Procedencia de artefactos, con hashes | El ledger de efectos (C4) |
| Configuración del modelo, replayable | La atestación de inferencia (C5) |
| Baselines de comparación de runs pareados | `aura replay` |

Lo que faltaba era un solo documento que llevara las cuatro y verificara solo.

```powershell
.\kernel\aura.exe bundle sess-8f3a --out caso.json
#  session    sess-8f3a
#  steps      412
#  effects    3 sealed · 2 delivered · 1 denied

.\kernel\aura.exe bundle --verify caso.json     # cualquiera, en cualquier parte
```

Un **recibo** prueba un efecto. Un **bundle** explica una sesión: la
trayectoria, un recibo autónomo por efecto sellado, y las configuraciones de
modelo que los argumentaron. Editar, quitar o reordenar un solo paso hace
fallar la verificación — que es justo la edición que alguien haría para ocultar
cómo se ganó una puntuación.

La truncación se declara, nunca se esconde: una sesión más larga que el tope
produce un bundle que dice que es un prefijo.

---

## Un entorno OpenEnv

[OpenEnv](https://github.com/huggingface/OpenEnv) es el contrato de Hugging
Face para entornos de RL agéntico: `reset` / `step` / `state` estilo Gymnasium
sobre HTTP, consumido por TRL, torchforge y SkyRL. Cada grafo registrado en un
nodo es uno:

```powershell
curl localhost:9080/openenv/spec
curl -X POST localhost:9080/openenv/reset -d '{"environment":"chat"}'
curl -X POST localhost:9080/openenv/step  -d '{"episode_id":"ep-…","action":{"text":"hola"}}'
```

Es un **border**, como el servidor MCP y la tarjeta A2A: habla el protocolo de
otro en el borde, sin privilegios dentro del kernel.

Dos decisiones que conviene declarar, porque las dos son negativas:

**El reward es siempre `null`.** Un runtime no puede saber qué cuenta como
éxito en una tarea, y un número inventado es exactamente la "puntuación sin
protocolo detrás" de la que trata el estudio. El campo existe para que un
wrapper lo rellene.

**El audit bundle del episodio está en `/openenv/bundle?episode=<id>`.** Esa es
la razón de tener este border y no solo cumplirlo: un entorno que devuelve
evidencia a prueba de manipulación junto a la observación permite que un
benchmark reporte los supuestos detrás de una puntuación, no solo la
puntuación.

---

## Aprobación firmada — quién lo permitió

El modelo de seguridad siempre pudo demostrar *que* un efecto fue gateado y
respondido. No podía demostrar **quién respondió**, y ese hueco es más grande de
lo que parece: una revisión de compliance no pregunta si un humano aprobó,
pregunta cuál. "Un humano aprobó" sin nombre es un log, no un audit trail.

Y no estaba simplemente ausente: era indemostrable en principio. El nodo escribe
su propio ledger, así que un nodo que quisiera afirmar que hubo una aprobación
podía escribir una. Toda garantía aquí se apoya en una firma que el nodo no
puede falsificar *en nombre de otro* — y la aprobación, el único campo que
describe una decisión humana, no tenía ninguna.

```powershell
.\kernel\aura.exe operator enroll grace --name "Grace Hopper"
#  enrolled grace
#  public key   MCowBQYDK2VwAyEA…
#  private key  C:\Users\tu\.aura\operator\grace\ed25519.key

.\kernel\aura.exe approvals                    # qué está esperando
.\kernel\aura.exe approve <id> --as grace      # fírmalo
#  approved 01J9ZK… — signed as grace
```

La entry sellada ahora lleva la respuesta:

```json
"approver": {
  "operator": "grace",
  "pubkey":   "MCowBQYDK2VwAyEA…",
  "envelope": "01J9ZK2M1P…",
  "decision": "approve",
  "ts":       1754083200000,
  "sig":      "base64…"
}
```

**El nodo nunca tiene la clave privada.** Vive en el home del propio operador y
solo se enrola la mitad pública. Eso es lo que convierte una aprobación en algo
que el nodo no puede fabricar sobre sí mismo, que es todo el punto: la parte
auditada no debe poder producir su propia evidencia. En una instalación de una
sola máquina la misma persona posee ambos directorios, así que ahí la separación
es una convención y no una frontera; se vuelve real en cuanto el operador aprueba
desde su portátil contra un nodo que está en otro sitio.

Todo lo que hay en la declaración firmada está atado, así que una firma válida no
se puede mover: ni a otro nodo, ni a otra sesión, ni a otra entrega, ni a otra
decisión, ni a otro momento. Corren dos chequeos, deliberadamente separados:

| | Qué pregunta | Contra qué | Cuándo |
|---|---|---|---|
| **Autoría** | ¿esta clave firmó *esta* resolución? | los bytes sellados | siempre, offline |
| **Enrolamiento** | ¿ese operador puede aprobar aquí, con esa clave? | el roster vivo | una vez, en el gate |

Unirlos haría que la historia dependiera del presente. **Revocar a un operador no
invalida las aprobaciones que ya dio** — un audit trail que cambia cuando cambia
el organigrama no es un audit trail. `aura operator revoke` lo dice explícitamente.

Tres rechazos que conviene conocer, cada uno un ataque real:

- Una clave que el nodo nunca enroló, firmando como `grace`, produce una
  declaración criptográficamente válida. Se rechaza — el nombre es una
  afirmación hasta que el roster confirma la clave detrás.
- Un `deny` firmado, reenviado con `"approve": true` al lado, se rechaza en vez
  de que gane uno de los dos. La firma cubre la decisión precisamente para que
  voltear un booleano no autenticado no pueda cambiar la respuesta.
- Una aprobación de una entrega, reproducida contra otra de la misma sesión, se
  rechaza. Dentro de una misma conversación, esa es la diferencia entre aprobar
  un pago y aprobar el siguiente.

Para hacerlas obligatorias:

```yaml
# aura.policy.yaml
policy: 1
default_effect: gate
require_signed_approval: true
```

Entonces una respuesta sin firma a un gate es una **denegación**, no un
fallback — un enforcement que puedes saltarte omitiendo un campo no hace
enforcement de nada. Un nodo con esto activado y sin nadie enrolado **se niega a
arrancar**, en vez de levantarse sano y denegar su primera escritura minutos
después con la causa a varias capas del síntoma.

`aura verify` chequea las firmas de aprobador en el mismo recorrido offline que
usa para la cadena. Una aprobación que ya no verifica deja el ledger
**unsound**: es evidencia directa de que una entry fue editada tras sellarse, o
de que se escribió una afirmando que un humano dijo que sí cuando ninguno lo hizo.

---

## El broker de credenciales

`aura guard` y la política de nodo comparten una limitación que esta guía siempre
dijo con todas las letras: valen exactamente hasta donde llegue tu control sobre
la configuración del agente. Nada impide que un proceso se salte el gate — llama
al ERP directo. Eso hace del checkpoint una excelente barandilla de seguridad y
una frontera de seguridad débil, y ninguna cantidad de política lo arregla,
porque la política se aplica en el lugar que el llamante decidió visitar.

La respuesta habitual es enforcement en la red: un proxy, eBPF, un sidecar.
Funcionan, necesitan infraestructura que este runtime promete que no vas a
necesitar, y ninguno corre en una Raspberry Pi.

La otra respuesta es dejar de intentar hacer imposible el bypass y hacerlo
**inútil**. Un agente que se salta el gate llega al ERP y no tiene credencial
para él, porque la credencial nunca estuvo en su entorno — está en el kernel, y
la única forma de obtenerla es presentar el recibo de un efecto que acaba de
pasar el checkpoint.

```powershell
.\kernel\aura.exe secret set erp_token      # lee stdin: sin historial de shell, sin tabla de procesos
#  stored erp_token — reference it as ${secret:erp_token}

.\kernel\aura.exe secret ls
#  ${secret:erp_token}
```

Referéncialo donde antes estaba la credencial:

```yaml
name: legacy-erp
base_url: http://erp.internal
headers:
  Authorization: "Bearer ${secret:erp_token}"    # se resuelve al llamar, nunca se guarda expandido
```

Qué demuestra un recibo, sin que el broker confíe en el llamante en absoluto:

- **el efecto existió y fue autorizado** — la entry está en la cadena, y a la
  cadena solo se llega pasando el checkpoint;
- **fue permitido o aprobado** — un efecto denegado no compra nada;
- **es *este* efecto** — la entry nombra la capability, así que un skill no puede
  gastar la aprobación de otro en el token de pagos;
- **es reciente** — 90 segundos, canjeable tres veces. Un recibo no es un bearer
  token con vida útil útil; el margen existe para reintentar tras un error de
  conexión, no para fan-out.

Un recibo falsificado no está en el ledger. Uno robado está atado a la capability
de otro. Uno reproducido está caducado. **Un token de nodo filtrado no se
convierte en una credencial de ERP filtrada** — tenerlo te deja poner un secreto
y ver que existe, y no hay ninguna ruta, en ningún sitio, que devuelva un valor.

Los secretos se cifran en reposo con una clave derivada de la propia clave de
identidad del nodo, y el nombre se autentica junto al valor, así que un
ciphertext no puede moverse de la fila de un secreto a la de otro con un editor
de texto. Un directorio de datos copiado sin su subdirectorio `identity/` produce
ciphertext que nadie puede abrir, que es el resultado correcto para un archivo de
base de datos robado.

Un skill que corre como proceso propio usa la misma puerta:

```powershell
curl -X POST localhost:9080/v1/secrets/resolve `
  -H "Authorization: Bearer $token" `
  -d '{"receipt":"sha256:…","capability":"motor.api.erp.create_invoice","names":["erp_token"]}'
```

**Lo que esto no hace**, dicho claro: no impide que un skill que recibió
legítimamente una credencial se la quede. Una vez que un valor llega a un
proceso, ese proceso lo tiene, y nada salvo no revelarlo nunca — un oráculo de
firma, un proxy de egress — cambia eso. Lo que sí elimina es la credencial
*ambiente y permanente*: el token en el entorno de un agente de larga vida,
disponible para cada llamada que haga, gateada o no. La ventana se reduce a un
efecto autorizado.

---

## Regresión contra el ledger

`aura replay` contesta "¿esta sesión reprodujo lo que registró?". Esa es la
unidad correcta para depurar y la equivocada para la pregunta que los equipos se
hacen de verdad cada semana:

> vamos a cambiar el modelo. ¿Qué le hace eso a los efectos?

Nada en el mercado la contesta con evidencia. Las suites de evals puntúan salidas
contra una rúbrica — lo cual mide si el texto mejoró y no dice nada sobre si el
runtime pasó a cobrar una tarjeta distinta. La observabilidad registra las
consecuencias. Ninguna puede decir *"339 de 345 sesiones sellaron exactamente los
mismos efectos; aquí están las 6 que no, y aquí la revisión del modelo de cada
lado"*.

```powershell
.\kernel\aura.exe regress --sessions 200
#    ── regression report ──────────────────────────────────
#
#    sessions      200 replayed · 194 reproduced · 6 changed · 0 could not run
#    effects       412 sealed before · 406 after   ← 6 fewer effects happened
#
#    models cited
#      before  Qwen/Qwen2.5-1.5B-Instruct-GGUF @f1d2d2f (Q4_K_M)
#      after   Qwen/Qwen2.5-3B-Instruct-GGUF @a7c31e0 (Q4_K_M)
#
#    what changed, by capability
#      motor.payments.refund                    6 session(s)  [count×6]
#
#    verdict: 6/200 sessions changed behaviour
```

La línea que importa es **"6 fewer effects happened"**, y es el hallazgo que un
diff de salidas estructuralmente *no puede* hacer: un acto que dejó de ocurrir no
deja texto detrás contra el cual comparar, así que se lee como silencio. Solo
algo que cuente los efectos sellados puede verlo.

Esto funciona aquí y en ningún otro lado por una razón que no es ingenio — la
agregación es un fold sobre el diff por sesión que ya existía. Es que C5 ya ata
revisión del modelo, cuantización y parámetros de muestreo a cada acto, así que
la comparación es entre dos configuraciones *nombradas y fijadas* en vez de entre
"antes" y "después", y una divergencia llega con las dos atestaciones que la
produjeron.

Deliberadamente no es un gate de pass/fail por defecto: un grafo con modelo
detrás redacta legítimamente distinto entre corridas, y una herramienta que grita
regresión ante cada reformulación queda silenciada en una semana — que es como
deja de leerse justo en el momento que importa. `--fail-on-regression` sale con
código distinto de cero para un job de CI que haya decidido que sus grafos son lo
bastante deterministas, y `--json` emite el informe completo.

Dos límites honestos:

- Una sesión que **no se pudo reproducir** (su grafo ya no está, un skill que
  necesita está caído) se cuenta aparte y **no** ensucia la corrida. Convertir
  en silencio "no se pudo probar" en "la prueba falló" enseña a la gente a
  ignorar los resultados.
- **Los gates se autorresponden sin firma durante el replay**, así que un nodo
  con `require_signed_approval` los verá rechazados. Eso es correcto y no
  desafortunado: un replay capaz de acuñar una aprobación firmada significaría
  que la maquinaria para fabricar una existe, y el valor de una aprobación
  sellada es precisamente que no se puede producir sin la clave de un humano.

---

## Aislamiento de skills

Esta ha sido la mayor brecha de seguridad abierta desde la primera release, y
el README lo decía. Esto la estrecha; no la cierra, y la diferencia merece
precisión.

```powershell
.\kernel\aura.exe run acme/vision/invoice-ocr --sandbox process --env PG_CDC_DSN
#  sandbox: process — scrubbed environment, cwd jail, no inherited handles.
#    It does NOT contain hostile code — use `format: wasm` for that.
```

| Backend | Aislamiento | Portátil | Estado |
|---|---|---|---|
| `none` | Ninguno — el comportamiento previo, ahora explícito | sí | entregado |
| `process` | Entorno depurado, jaula de cwd, sin handles heredados | sí | entregado |
| `wasm` | WASI — `permissions` forzado de verdad por wazero | sí | entregado |
| `microvm` | Una frontera real | Linux+KVM | **solo interfaz, rechazado al arrancar** |

**Qué compra `process`.** El entorno pasa a ser una allowlist en vez de una
herencia: un skill instalado desde un registry ya no recibe el
`AWS_SECRET_ACCESS_KEY`, el `GITHUB_TOKEN` o el `SSH_AUTH_SOCK` del operador
solo por arrancarse. Un skill que sí necesita una credencial la nombra con
`--env`. Eso cierra un fallo real y común. **No** es una frontera contra código
hostil, que todavía puede llegar a la red y al filesystem por syscalls que
ningún runtime de Go portable intercepta.

**Por qué `microvm` se rechaza en vez de simularse.** El consenso de 2026 es
inequívoco: los contenedores comparten kernel y no son una frontera de
aislamiento; lo que usa producción son microVMs (Firecracker, Cloud Hypervisor,
Kata) o gVisor. Ese consenso es correcto. Implementarlo requiere Linux con KVM,
y un backend que degradara en silencio a algo más débil sería peor que ninguno,
porque un operador correría código de terceros creyendo en un aislamiento que
el runtime le fabricó. Pedirlo produce un error que explica qué usar en su
lugar.

---

## Witnessing abierto

`aura witness` ancla un ledger con un tercero. Hasta ahora el endpoint estaba
tras el token del nodo, lo que significaba que el witnessing solo funcionaba
entre partes que ya habían intercambiado una credencial — un witness dentro del
mismo dominio de confianza que el log que avala, que es justo lo que el modelo
de Certificate Transparency necesita que no ocurra.

```powershell
.\kernel\aura.exe up --open-witness
#  WARN open witness enabled — any node may anchor its ledger here without a token
#       max_nodes=10000 per_node_per_hour=12 retention_days=90
```

Lo que lo hizo seguro de ofrecer no fue el cambio de ruta sino los límites. Un
witness abierto es una superficie de escritura, y cualquiera puede generar un
keypair e inventarse un node id:

- **Tasa**: 12 presentaciones por nodo por hora. Un nodo con checkpoints
  normales presenta muy por debajo.
- **Capacidad**: 10.000 nodos distintos. En el tope, los nodos ya avalados
  siguen funcionando y los nuevos se rechazan — el modo de fallo útil, porque
  el valor del witnessing está en la continuidad de lo ya prometido.
- **Retención**: 90 días. Olvidar también es honesto: una contrafirma cuya
  línea base el witness descartó es una que ya no puede contradecir.

El statement se autoautentica —lleva la clave pública del nodo que lo presenta
y una firma sobre la cabeza— así que un witness abierto verifica antes de
recordar nada, y rechaza antes de hacer criptografía para quien excede su
límite.

---

## El marketplace

Los skills se distribuyen mediante un registro **federable** — cualquiera puede
alojar uno con `aura registry serve`, exactamente igual que un registro de
contenedores, que es lo que hace creíble la neutralidad del ecosistema.

```powershell
.\kernel\aura.exe registry serve                 # aloja un registro (su propio puerto)

.\kernel\aura.exe publish my-skill\              # zip + firma (Ed25519) + subida
.\kernel\aura.exe add acme/vision/invoice-ocr    # verificar + revisar permisos + instalar
.\kernel\aura.exe add --capability sensorial.ocr # descubrir por capacidad, no por nombre
.\kernel\aura.exe run acme/vision/invoice-ocr    # arrancarlo contra el nodo local
```

Instalar te muestra exactamente en qué estás confiando *antes* de que aterrice —
la clave del publicador y los permisos declarados del skill ("puede llamar a
`api.acme.com` y a nada más") — y entonces pide confirmación.

Garantías del registro, todas aplicadas y testeadas:

- **Versiones inmutables** — republicar una versión con contenido distinto se
  rechaza; hay que subir la versión. Republicar contenido idéntico es
  idempotente.
- **Trust-on-first-use** — la primera publicación liga un id de paquete a la
  clave de su publicador; una versión posterior firmada con una clave
  *diferente* no puede secuestrar el id, aunque esa firma sea válida por lo
  demás.
- **Acoplamiento a prueba de manipulación** — la firma cubre el hash del
  manifiesto *y* el hash del artefacto juntos, de modo que ninguno puede
  sustituirse por separado.
- **Extracción segura** — una entrada del paquete que escapa de la ruta se
  rechaza al instalar.

---

## Explicabilidad y replay

Cada sesión persiste un log de eventos causal append-only — cada mensaje nombra
al mensaje que lo causó. Dos herramientas lo leen.

**`aura why`** recorre la cadena hacia atrás desde el fallo (o desde la última
actividad) hasta la raíz y la narra con el LLM local:

```
> aura why sess-90615a146927
  FALLÓ — cadena causal desde la raíz hasta el fallo:
  │ 1. [data]             client.text_out    {"text":"envía el informe confidencial"}
  │ 2. [data]             eco.text_in        {"text":"envía el informe confidencial"}
  │ 4. [confirm_request]  client.confirm_in  {"question":"¿Aprobar la entrega…"}
  │ 5. [confirm_response] client             {"approve":false}
  6. [error]            client.text_in     {"detail":"delivery … denied by user"}

  why › La causa raíz es que el usuario denegó la entrega del informe confidencial.
```

Sin skill de LLM conectado degrada limpiamente a la cadena determinista anotada.
Llamado sin id de sesión, explica la más reciente.

**`aura replay`** convierte tráfico real grabado en una suite de evaluación:
reenvía las entradas originales del cliente de una sesión a una sesión *nueva*
del grafo actual y compara las salidas:

```powershell
.\kernel\aura.exe replay sess-c0884792d2b4          # veredicto: 3/3 salidas idénticas
.\kernel\aura.exe replay sess-… --graph chat-v2     # A/B contra otro grafo
.\kernel\aura.exe replay sess-… --deny-gates        # ejercitar la vía de denegación
```

---

## Federar nodos

Cada nodo funciona de forma autónoma (autonomía hoja-nodo). La federación
*amplía* lo que un nodo puede resolver proyectando los skills de otro nodo como
si fueran locales.

```powershell
# En una caja edge (nodo B): ejecuta, por ejemplo, el skill de OCR.
# En tu portátil (nodo A):
.\kernel\aura.exe federate http://edge-box:9080
#   federando http://edge-box:9080 → 1 skill(s) proyectado(s) en el nodo local (route: lan)
#   el nodo A ahora resuelve sensorial.ocr.image — el trabajo corre físicamente
#   en B, y las respuestas vuelven en streaming con la causalidad intacta.
.\kernel\aura.exe federate http://edge-box:9080 --capability sensorial   # filtrar
```

El puente es puro userland: es simultáneamente un cliente-skill del nodo local
(registrando skills proxy) y un cliente de streams del nodo remoto (conduciendo
grafos allí), retransmitiendo envelopes entre ambos. No requirió ningún cambio
en el kernel, lo que es algo de evidencia de que la frontera del micro-kernel
está trazada en un sitio útil. También tiene tests (`fed`, 87%), incluido que
`cancel` e `idem` sobreviven al salto de nodo.

**Transporte negociado (regla 5 de C3).** El puente mantiene una conexión
persistente por capability federada, reutilizada entre relays — no
redialeada por envelope, que era lo que hacía antes (confirmado en el propio
cable, no asumido: un intercambio de cinco mensajes significaba cinco
handshakes de WebSocket y cinco sesiones remotas desconectadas entre sí).
Cancelar un relay envía un envelope `cancel` dirigido sobre esa conexión
compartida en vez de cerrarla — cerrarla abortaría cualquier otro relay que
la esté usando — y `aura federate` imprime qué ruta midió hacia el remoto
(`same-host` / `lan` / `relay`, cronometrada contra `/healthz`). Acotado a lo
que de verdad se puede verificar sin dos redes genuinamente separadas:
QUIC/WebRTC y NAT traversal quedan para más adelante.

---

## Estándares en las fronteras

Deep Axiom habla los protocolos del ecosistema en sus bordes, de modo que
interopera con ellos en lugar de competir.

**Servidor MCP.** Cada skill conectado es automáticamente una tool. Conecta
Deep Axiom a Claude Code, Cursor o cualquier cliente MCP:

```powershell
claude mcp add --transport http aura http://localhost:9080/mcp
#   tools/list → logical_echo, cognitive_llm_chat, sensorial_api_crm_list_users, …
#   tools/call ejecuta el skill por la vía normal de cliente
```

Los nombres de las tools provienen de las capacidades; los esquemas de entrada
se derivan del esquema de ingreso de cada skill.

**Hace streaming.** Un `tools/call` de un cliente que envía
`Accept: text/event-stream` se responde como tal: cada fragmento que produce un
skill se convierte en un `notifications/progress` de MCP a medida que se
produce, y la respuesta JSON-RPC cierra el stream. Un cliente que no pide
stream sigue recibiendo el cuerpo JSON completo. Esta frontera era solo
request/response, lo que la hacía contradecir el argumento sobre el que
descansa el resto del runtime: un skill que emite token a token no debería
aplanarse en una única respuesta justo en el borde que un agente externo
realmente usa.

**Sus gates se pueden responder.** Un efecto invocado por MCP se gatea igual
que cualquier otro, pero el cliente de ese socket es un programa, así que no
hay humano a quien preguntar. La pregunta se estaciona en la cola de
aprobaciones del nodo y se responde desde la UI, `aura approve` o
`POST /v1/approvals/{id}`; la llamada espera hasta entonces y se deniega sola
si nadie contesta. Antes se resolvía como un rechazo automático con un puntero
a la UI — seguro, pero significaba que un agente externo podía *leer* a través
de este nodo y nunca *actuar* a través de él. Ver
[Proteger las tools de un agente](#proteger-las-tools-de-un-agente), que es lo
que eso desbloqueó.

**Tarjeta de descubrimiento A2A** en `/.well-known/agent.json`, construida desde
el catálogo vivo, para que otros agentes puedan descubrir este nodo y sus
skills. Solo descubrimiento — el protocolo de *mensajes* A2A no está
implementado, y "soporte A2A" aquí debe leerse como exactamente ese endpoint.

**Exportación OpenTelemetry.** El log causal se convierte en trazas OTLP — un
span por envelope, con `parentSpanId` apuntando al padre causal — de modo que
cualquier backend OTel (Jaeger, Grafana Tempo, …) renderiza el árbol causal de
una sesión:

```powershell
.\kernel\aura.exe trace sess-… --otlp http://localhost:4318   # a un collector
.\kernel\aura.exe trace sess-… --out trace.json               # o a un archivo
```

---

## Proteger las tools de un agente

Todo lo anterior asume que estás construyendo *sobre* este runtime. Esto no.

Un agente — Claude Code, Cursor, OpenClaw, cualquier cosa que hable MCP — llama
a sus servidores de tools directamente. Lo que una tool haga, lo hace; nada
decide si debería, y nada registra que lo hizo. `aura guard` pone el checkpoint
de este nodo en medio de ese camino sin pedirle a nadie que reescriba un agente
ni que adopte un runtime:

```
agente ──▶ aura /mcp ──▶ executor ──▶ guard ──▶ el servidor MCP real
                            │
                    policy · gate · ledger
```

Apúntalo al `mcpServers` que ya tienes — el formato que Claude Desktop, Cursor y
el resto ya escriben, sin modificar:

```powershell
.\kernel\aura.exe guard --config .\claude_desktop_config.json --dry-run
```

```
  aura guard — would guard 2 tool(s) on node node-7d6794d2

  TOOL                CAPABILITY                    ON CALL
  ------------------  ----------------------------  ------------------------
  github/delete_repo  motor.mcp.github.delete_repo  human approval + sealed
  github/list_repos   motor.mcp.github.list_repos   human approval + sealed

  2 of 2 act on the world and are gated; the rest are read-only.
```

Quita `--dry-run` para proteger de verdad, y después apunta el agente a este
nodo en vez de a los servidores directamente:

```powershell
claude mcp add --transport http aura http://localhost:9080/mcp
```

Desde ahí, una llamada a una tool que actúa sobre el mundo se detiene y espera:

```powershell
.\kernel\aura.exe approvals
#   01M051C3ZTJQ…
#       Approve delivery to s.call?
#       tool     motor_mcp_github_delete_repo (via mcp)
#       args     {"body":{"repo":"prod-database"}}
.\kernel\aura.exe approve 01M051C3ZTJQ…      # o --deny
```

y el efecto queda sellado en el ledger como cualquier otro, con un receipt que
se verifica sin conexión.

**Cómo está construido importa más que lo que hace.** Guard no contiene código
de policy, ni de gate, ni de ledger. Se conecta a `/ws/skill` — el mismo socket
de inversión de control que usa cualquier skill fuera de proceso, sin
privilegios de kernel — y registra un skill por cada tool upstream, tipado
`motor` o `sensorial`. Desde el lado del executor no tienen nada de especial,
así que heredan gratis el invariante de aprobación humana, el ledger, la
cancelación y el replay. Un guard que tomara sus propias decisiones sería un
segundo checkpoint, y el argumento de todo este runtime es que hay exactamente
uno.

**Una tool está gateada salvo que demuestre lo contrario.** MCP permite que un
servidor anote una tool como `readOnlyHint`. Guard registra todo como `motor`
—gateado—, incluidas todas las tools de un servidor que no anota nada.
`--trust-annotations` honra el hint y deja esas tools sin gate. El default es
estricto en esa dirección a propósito: `readOnlyHint` es una afirmación hecha
por el mismo servidor sobre el que trata la llamada, exactamente igual que una
atestación C5 es una afirmación del skill que la hizo. Creerle para *saltarse*
un gate permitiría que un servidor desarmara el guard mintiendo sobre sí mismo;
creerle para *exigir* uno cuesta un prompt en una tool que solo lee.

**Lo que esto no hace, dicho sin vueltas.** Un guard que se puede saltear es
teatro. Nada aquí impide que un agente hable con el servidor MCP original
directamente — la garantía vale exactamente hasta donde llega tu control sobre
la configuración del agente. Eso lo hace útil en una máquina o un equipo donde
esa config está gestionada, y decorativo donde no. No hay enforcement de red,
ni control de egress, ni intento de detectar a un agente que lo esquiva.

Dos límites menores que conviene saber: un `tools/call` en vuelo no se puede
cancelar, porque MCP no tiene cancelación para uno — el kernel suprime lo que
la cadena abandonada hubiera producido, pero el servidor upstream sigue
trabajando. Y una llamada gateada mantiene abierta la request del agente
mientras espera, cosa que algunos agentes van a timeoutear antes de que llegue
un humano.

---

## Desplegar y operar un nodo

Todo lo anterior trata de lo que hace un nodo. Esto trata de correr uno, que es
otro conjunto de preguntas y hasta hace poco tenía otras respuestas.

### El cierre era lo primero que había que arreglar

`aura up` maneja SIGINT y SIGTERM: deja de aceptar conexiones, da a las peticiones
en vuelo una ventana acotada (10 s, dentro de los periodos de gracia por defecto
de Docker y de Kubernetes), y solo entonces cierra los escritores y la base — en
ese orden, que es el que documenta `store.Close`.

Esa secuencia es la razón de que cada `Close()` de este codebase signifique algo.
Antes de que existiera, `ListenAndServe` bloqueaba para siempre, la limpieza
diferida nunca corría, y SIGTERM —lo que mandan `docker stop`, el borrado de un
pod y `systemctl stop`— mataba el proceso de golpe. El orden de cierre del store,
el writer bufferizado de 1 MiB de seglog y el drenado del escritor de SQLite eran
inalcanzables en la práctica, en cada deploy. Con un runtime de contenedores el
kill sigue a la señal tras un periodo de gracia fijo, así que un cierre ordenado
no era improbable: era imposible.

Un cierre limpio imprime `stopped cleanly` y deja el ledger y el log de eventos
consistentes. CI verifica las dos cosas —que el drenado ocurrió, y que
`aura verify` reporta SOUND después— porque un log de aspecto limpio sobre una
cadena rota sería el peor de los dos resultados.

### Dos endpoints que un orquestador necesita

```
GET /readyz    abierto     ¿vale la pena mandarle tráfico a este nodo?
GET /metrics   con token   texto Prometheus
```

`/readyz` es deliberadamente más estricto que `/healthz` y deliberadamente
abierto. Una probe de liveness responde en cuanto el listener está arriba; la de
readiness responde solo cuando el store y el ledger son usables. Confundir las dos
es la razón de que un rolling deploy mande tráfico a un pod nuevo que está
corriendo y no funcionando. Está abierto porque la probe de un runtime de
contenedores no tiene credencial — un chequeo de readiness que responde 401 deja
a un nodo sano fuera de rotación para siempre.

`/metrics` **no** está abierto. Las series incluyen conteos de sesiones vivas,
tamaño del ledger y la cola de aprobaciones pendientes, que es una foto útil de lo
que este nodo está haciendo para alguien que no debería tenerla. Un scraper puede
llevar un bearer token.

La métrica que conviene mirar primero es `aura_store_batch_mean`. Cerca del tope
del batch significa que el commit es tu techo y añadir escritores sobre un archivo
SQLite lo empeoraría; cerca de 1 bajo carga significa que el límite está en otro
sitio. `aura_store_commit_fallbacks_total` por encima de cero significa que alguna
escritura está fallando una constraint y cada una cuesta un commit lento.
`aura_event_log_damaged_segments` por encima de cero significa que un archivo
sellado cambió después de cerrarse — investiga en vez de reiniciar.

### El contenedor

```bash
docker compose up
```

Distroless, non-root, sin CGO. Dos líneas del Dockerfile cargan peso en vez de
ser boilerplate: `STOPSIGNAL SIGTERM` con entrypoint en **forma exec**, para que
la señal llegue a PID 1 y PID 1 sea el kernel. Con un entrypoint en forma de
shell, el shell es PID 1, no reenvía señales, y cada parada se convierte en un
SIGKILL — que es exactamente el fallo que describe la sección de arriba.

`docker build` todavía no corre en CI, así que la imagen está escrita y no
probada. Ver [ROADMAP-ES.md](ROADMAP-ES.md).

### El directorio de datos no es una caché

Tiene tres cosas que no se pueden regenerar:

- **`identity/`** — el par de claves Ed25519 del nodo. Cada checkpoint y cada
  declaración de witness verifica contra él.
- **`kernel.db`** — el ledger de efectos. Esto es la evidencia.
- **los secretos cifrados** — y la clave de cifrado del broker se *deriva* de la
  clave de identidad, así que un directorio de datos restaurado **sin**
  `identity/` produce texto cifrado que nadie puede abrir. Ese es el resultado
  correcto para un archivo de base de datos robado y catastrófico para un backup
  parcial.

Respáldalo como una base de datos, no como una caché. Todavía no hay
procedimiento documentado de backup/restore, ni rotación de claves; los dos están
en [ROADMAP-ES.md](ROADMAP-ES.md) entre lo que bloquea producción.

### Lo que un nodo no sobrevive

Un proceso, sin failover. Un nodo que muere se lleva su estado de ruteo vivo — el
log de eventos sobrevive, así que la historia y el replay quedan intactos, pero
las sesiones que estaban abiertas se fueron. Contesta esto antes de desplegar: si
AURA se cae, ¿tu aplicación degrada o se detiene? Si degrada, esto es desplegable
hoy como servicio auxiliar. Si se detiene, el failover va primero.

---

## Los cinco contratos

Todo lo anterior descansa sobre cinco contratos pequeños, formalmente
especificados y con versión congelada, en [`spec/`](spec/). Son lo *único* que
AURA inventa; una suite de conformidad
([`spec/conformance/`](spec/conformance/), 59 comprobaciones, caja negra sobre
el protocolo crudo) es cómo una implementación demuestra que cumple C1-C3 — las
garantías propias de C4 y C5 las comprueba, por separado, un job de CI
adversarial (ver [Modelo de seguridad](#modelo-de-seguridad)).

- **C1 — Manifiesto** ([spec/c1-manifest.md](spec/c1-manifest.md)): qué es un
  skill — identidad, tipo, capacidad, puertos tipados con esquemas obligatorios,
  permisos y necesidades de recursos declarados, metadata de compensación
  opcional, firma.
- **C2 — IR de Grafo** ([spec/c2-graph-ir.md](spec/c2-graph-ir.md)): el formato
  único de grafo al que compilan tanto el YAML humano como la salida del
  planner — nodos que declaran una *demanda de capacidad*, aristas con puertas
  opcionales de aprobación humana, olas paralelas.
- **C3 — Channel** ([spec/c3-channel.md](spec/c3-channel.md)): el envelope de
  mensaje y su semántica normativa — orden FIFO, entrega at-least-once con
  idempotencia, back-pressure obligatorio, `cause_id` causal, cancelación,
  QoS declarable, transporte negociado, medición, un recibo de efecto
  opcional, y una atestación de inferencia opcional.
- **C4 — Ledger de Efectos y Política** ([spec/c4-ledger.md](spec/c4-ledger.md)):
  el Effect Checkpoint por el que pasa toda entrega `motor.*` — autorizar por
  política de nodo, atestiguar en una entrada encadenada por hash, comprometer
  una cabeza Merkle RFC 6962, firmarla periódicamente, y opcionalmente hacer
  que un tercero la contrafirme — y la verificación sin conexión que una
  implementación conforme debe soportar.
- **C5 — Atestación de Inferencia** ([spec/c5-attestation.md](spec/c5-attestation.md)):
  lo que un skill afirma sobre cómo produjo una salida — motor, modelo,
  revisión, cuantización, parámetros de muestreo, semilla — direccionado por
  contenido y citado por cada efecto que esa salida causó. C4 responde *bajo
  qué autoridad*; C5 responde *sobre qué base*. Es cuidadoso con sus propios
  límites: una atestación es una afirmación ligada de forma infalsificable a
  sus consecuencias, no una prueba de qué se ejecutó.

**La interrupción es una garantía, no una petición.** Un cliente puede abandonar
una cadena a media ejecución — el "para, cambié de idea" de una conversación en
vivo. El kernel marca esa cadena como cancelada y no rutea nada más que le
pertenezca, así que un skill que ignore el cancel y siga generando produce
salida que no llega a ningún sitio. Además *pide* a cada skill que trabaja en la
cadena que pare, a cualquier profundidad, y a cada uno lo direcciona con el
`cause_id` que ese skill reconoce — un skill a tres saltos nunca vio el id que
nombró el cliente. La cancelación está acotada a esa cadena: una segunda
frase, o un stream de visión que comparta la sesión, quedan intactos.

Congelado significa solo-aditivo dentro de una versión mayor; un cambio de
ruptura requiere un nuevo major y una justificación escrita.

---

## Modelo de seguridad

La seguridad está *diseñada* para ser **progresiva** — la fricción aparece
exactamente donde aparece el riesgo, y en ningún otro sitio. Esa es la
intención; lo que sigue es cuánto de ella aplica el código hoy, porque la
distancia importa más que la intención.

> **Autenticación.** `aura up` sin flags ata **loopback**, genera un **token
> bearer** al primer arranque (0600, impreso una vez, reutilizado al reiniciar),
> lo exige en `/v1/*`, `/ws/*`, `/mcp` y la tarjeta A2A, y comprueba el
> **Origin** del WebSocket contra una allowlist. `--listen 0.0.0.0` es una
> renuncia explícita que imprime un aviso; `--tls-cert`/`--tls-key` terminan
> TLS; `--no-auth` desactiva el token y lo dice en voz alta al arrancar.
>
> Tres rutas quedan abiertas sin token, cada una por un motivo declarado:
> `/healthz` (un orquestador no tiene credencial), `/hooks/*` (el emisor se
> autentica con su propio HMAC) y los assets estáticos de la UI (el navegador
> tiene que cargar la página antes de poder presentar nada). El catálogo de
> capacidades **no** está entre ellas: nombra cada efecto que el nodo puede
> producir.
>
> **Autorización.** Una política de nodo (`--policy`) decide qué puede actuar
> sobre el mundo: deny-by-default para `motor.*`, primera regla que casa gana,
> sin lenguaje de reglas. **Un grafo puede ser más estricto que la política,
> nunca más laxo**: un `deny` es absoluto, y `"gate": "none"` pasa a ser una
> *petición* que solo se honra si el nodo concedió esa autoridad — que es lo
> que cierra el agujero por el que cualquiera capaz de hacer POST de un grafo
> podía renunciar a la mejor garantía del kernel.
>
> **Lo que sigue sin aplicarse:** el aislamiento de procesos. Un skill corre con
> los privilegios de quien lo arrancó, y `permissions` es una declaración, no un
> sandbox. Hacerlo real necesita el executor Wasm, que es trabajo posterior a la
> beta en [Diseñado, aún no construido](#diseñado-aún-no-construido).

**Un waiver se registra como tal.** Donde una policy sí concede esa autoridad a
los grafos, la entry sellada para el efecto lleva `waived: true` — porque
`decision: allow` por sí solo no distingue "la policy del operador autorizó esto"
de "quien registró este grafo lo autorizó", y un grafo es un JSON que puede
publicar cualquiera con acceso a la superficie de control. El broker de
credenciales se niega a gastar un recibo que lo lleve, así que un waiver sigue
siendo una forma de bajar fricción y no se convierte en una forma de acuñar la
autorización que compra un secreto. Ver [C4](spec/c4-ledger.md) y [El broker de
credenciales](#el-broker-de-credenciales).

**Cuando el ledger no se puede escribir.** Sellar escribe a un disco, y los
discos se llenan. `on_seal_failure` decide cuál de dos malos resultados prefiere
este nodo:

```yaml
on_seal_failure: deliver   # por defecto: el efecto pasa, el hueco se registra
on_seal_failure: refuse    # el efecto se detiene en vez de quedar sin atestiguar
```

No hay una tercera opción donde el nodo siga arriba y el ledger quede completo,
así que la decisión es del operador y no del kernel. `deliver` es el default
porque un runtime cuya premisa es que nunca se detiene no debería convertir un
disco lleno en una caída; un nodo que sella pagos quiere `refuse`. El banner de
arranque imprime cuál está en vigor, y un efecto que no se pudo sellar no lleva
recibo — así que nada río abajo puede confundirlo con atestiguado.

| Modo | Pensado para | Qué cambia hoy |
|---|---|---|
| `local` | localhost, un usuario | El default. Token, comprobación de origen y política se aplican. |
| `site` | varios nodos de una organización | **Nada todavía.** La identidad automática, los canales cifrados y los permisos evaluados están diseñados, no implementados — el modo se acepta y se comporta exactamente como `local`. |
| `published` | código de terceros / red pública | Una sola cosa: el executor *rechaza* un grafo con una arista `motor.*` sin puerta en vez de añadirla. En todo lo demás es idéntico a `local`. |

La firma de paquetes está deliberadamente fuera de esa tabla, porque no depende
del modo: `aura publish` firma siempre (Ed25519) y `aura add` verifica siempre
el hash del artefacto y la firma antes de instalar, corra el nodo en el modo
que corra. Esa parte del modelo de confianza es real — ver
[el marketplace](#el-marketplace).

### Qué ya lo exige

El resto de esta sección describe mecanismos. Esta parte dice qué los obliga, que
es algo concreto, superada ya la etapa de borrador y —desde julio de 2026— más
tardío de lo que era.

**El [Reglamento (UE) 2024/1689](https://artificialintelligenceact.eu/) — el
Reglamento de IA — exige esto a los sistemas de alto riesgo desde el 2 de
diciembre de 2027.** La fecha era el 2 de agosto de 2026 hasta que el
[Reglamento (UE) 2026/1744](https://eur-lex.europa.eu/eli/reg/2026/1744/oj), el
Digital Omnibus sobre IA (en vigor el 27 de julio de 2026), aplazó el régimen de
alto riesgo: los sistemas autónomos del Anexo III al **2 de diciembre de 2027**,
los sistemas embebidos del Anexo I al **2 de agosto de 2028**. Lo que la enmienda
movió es la fecha de aplicación. El contenido de los artículos 12 y 14 no cambió,
y ambos hablan directamente de lo que un runtime debe emitir, no de lo que una
organización debe prometer:

| | Exigencia | Qué la contesta aquí |
|---|---|---|
| **Art. 12** — Conservación de registros | Registro *automático* de eventos durante todo el ciclo de vida, al servicio de la identificación de riesgos (Art. 79), la vigilancia poscomercialización (Art. 72) y la supervisión del responsable del despliegue (Art. 26(5)). Los responsables conservan los logs **al menos seis meses**. | El log causal de eventos (C3 regla 7) y el ledger de efectos (C4). Automático porque los escribe el executor, no el autor del grafo. La retención es decisión del operador: el log guarda todo por defecto y `--event-log-max` lo acota — ponlo por encima de tu obligación de retención, no por debajo. |
| **Art. 14** — Supervisión humana | El sistema puede ser supervisado de forma efectiva por **personas físicas** mientras está en uso. | El gate de aprobación como invariante del kernel, y el **aprobador firmado** — un log que registra que "un humano aprobó" evidencia la supervisión de nadie en particular. Ver [Aprobación firmada](#aprobación-firmada--quién-lo-permitió). |

**Sobre el aplazamiento, sin rodeos:** elimina la urgencia, no el requisito. Quien
esté decidiendo qué construir este trimestre debería pesar dos cosas que el
aplazamiento no cambia. Primera: **ISO/IEC 42001** cláusula 9.2 exige auditoría
interna contra tus propias políticas de IA con una cadena documentada desde el
hallazgo hasta la corrección — que es `aura audit` más `aura why` — y está en
vigor hoy, es certificable ya, y aparece cada vez más en cuestionarios de compra
que no esperan a Bruselas. Segunda: un rastro de auditoría es una propiedad de la
ruta de ejecución, no un módulo a su lado; un sistema que no fue construido para
emitir esta evidencia se reconstruye, no se extiende, cuando le toca. Eso es un
argumento para usar los dieciséis meses extra, no para gastarlos.

Las **normas armonizadas** que operacionalizarán el Artículo 12 — prEN 18229-1,
ISO/IEC DIS 24970 — siguen en borrador, lo cual conviene saber por dos motivos:
todavía nadie puede reclamar conformidad con ellas, y la forma de la evidencia
exigida se está decidiendo *durante* el aplazamiento, no antes de él. Eso corta
en ambos sentidos — el objetivo todavía puede moverse, y hay una ventana
inusualmente larga para influir en dónde aterriza.

Una advertencia que corresponde a un modelo de seguridad y no a marketing: **nada
de lo anterior vuelve conforme a un despliegue.** El Artículo 12 es una
obligación entre muchas, este kernel emite registros y no realiza una evaluación
de conformidad, y ninguna herramienta puede hacer esa parte por ti. Lo que quita
es el fallo habitual: descubrir en la auditoría que los registros existen solo
como logs de aplicación que nadie puede demostrar que no fueron editados.

### Credenciales con alcance

Un nodo tenía exactamente una credencial. La CLI, la UI del navegador, un bridge
de federación y cada proceso skill presentaban el mismo bearer token, así que
"puede alcanzar este nodo" y "es el operador de este nodo" eran la misma
afirmación.

Eso hacía que la garantía del broker de credenciales fuera más débil de lo que se
lee. Su argumento es que saltarse el Effect Checkpoint te da un 401 — cierto
contra un proceso fuera del nodo, y falso contra un skill, que tenía el token del
operador y por tanto podía registrar un grafo que waiveara su propio gate, sellar
un efecto y canjear el recibo que acababa de acuñar.

```powershell
.\kernel\aura.exe token issue --capability motor.erp.write --label "erp writer"
#  tok-2sNkcJvt  scoped to motor.erp.write
#
#  mGN2mnCbyz6_fkh-Jd9bXcqBib2NSuHPll0swrQqtPE
#
#  Es la unica vez que se muestra: solo se guarda su hash.
```

Una credencial `skill` puede hacer tres cosas: conectarse a `/ws/skill`,
registrarse como la capability para la que fue emitida, y gastar un recibo por
las credenciales que ese efecto autorizó. No puede registrar un grafo, leer el
ledger, guardar un secreto, inscribir un aprobador ni hospedar un módulo Wasm.

Dos propiedades son las que lo hacen real y no nominal:

- **El registro queda atado a la capability emitida**, exacta y nunca por
  prefijo. Registrarse es cómo un proceso declara qué *es* ante el executor, y de
  ahí cuelga todo — qué regla de policy aplica, si una arista hacia él es un
  efecto, qué recibos puede gastar. Un token estrecho libre de registrarse como
  cualquier cosa sería uno completo con un nombre más pequeño.
- **Una sola tabla ordenada decide qué alcanza cada scope**, denegando por
  defecto, en `internal/gateway/scope.go`. El mismo argumento que hace el motor
  de policy sobre las reglas: un modelo de autorización repartido en cuarenta
  handlers existe solo como la suma de cuarenta decisiones, y una ruta añadida el
  año que viene toma por defecto lo que su autor recordara. Aquí una ruta nueva
  está cerrada a los skills hasta que alguien ensanche la tabla en un diff que un
  revisor puede ver.

`aura token ls` lista lo emitido; `aura token revoke <id>` detiene una en su
siguiente request. La revocación se registra en vez de borrarse, por la misma
razón que la de un operador: un efecto sellado mientras el token era válido sigue
siendo explicable después.

El token del propio operador no cambia — sigue siendo `node.token` en el
directorio de datos, y un nodo que no emite ninguno se comporta igual que antes.

**Lo que esto no hace.** No contiene a un skill al que ya se le entregó una
credencial que pidió legítimamente, ni aísla el proceso — ver [Aislamiento de
skills](#aislamiento-de-skills). Lo que quita es la autoridad de operador
permanente que cada skill cargaba solo por haber sido arrancado.

### Atestación — el ledger de efectos ([C4](spec/c4-ledger.md))

La autorización decide si un efecto ocurre. La atestación es la pregunta
separada de si alguien puede *probar*, después, que ocurrió — y bajo qué
autoridad. Cada efecto que la política de un nodo autoriza —entregado sin más,
o con puerta y luego aprobado o denegado— se sella en una entrada de ledger
append-only:

```json
{ "seq": 42, "prev": "sha256:9f2a…", "actor": "acme/motor/erp-writer@1.2.0",
  "capability": "motor.erp.invoice.create", "decision": "gate", "outcome": "delivered",
  "policy": "sha256:7c1e…", "payload_sha256": "b5b3…" }
```

Cinco cosas hacen que esto sea evidencia y no logging:

- **Encadenado por hash.** Cada entrada cita el hash de la anterior (`prev`),
  calculado sobre el contenido propio de la entrada. Alterar el contenido de
  una entrada vieja cambia su hash, lo que rompe todas las entradas selladas
  después — la cadena o recalcula limpia de punta a punta, o visiblemente no.
- **Comprometido a una cabeza Merkle.** Junto a la cadena lineal, las entradas
  forman un árbol RFC 6962 — la construcción de Certificate Transparency,
  elegida porque da los dos tipos de prueba de abajo sobre una sola forma y
  cierra el ataque de segunda preimagen que tiene un árbol ingenuo. Cada
  checkpoint se compromete a la cabeza del árbol además de a la de la cadena.
- **Firmado periódicamente.** Cada ~100 entradas o 60 segundos, lo que ocurra
  primero, el nodo firma su cabeza actual con su propia clave Ed25519
  (`identity.Node.Keys`, generada al primer arranque como cualquier otro
  keypair de este runtime). Esto es lo que una cadena de hashes por sí sola no
  puede dar: un atacante con acceso directo a la base de datos podría editar
  una entrada vieja *y* reparar cada puntero `prev` posterior, dejando una
  cadena que sigue recalculando limpia — pero no puede forjar una firma sobre
  la nueva cabeza sin la clave privada del nodo.
- **Contrafirmado opcionalmente por un tercero.** Ver
  [anclaje externo](#anclaje-externo) — lo que la autofirma no puede hacer.
- **Verificable sin conexión.** `aura verify [--data <dir>]` recalcula toda la
  cadena *y* el árbol, y comprueba cada firma de checkpoint directamente
  contra el archivo SQLite — sin ningún proceso del kernel involucrado, y sin
  tocar nunca la clave privada, solo la pública. `GET /v1/ledger/verify` corre
  la misma comprobación contra un nodo corriendo, por conveniencia; las dos
  nunca pueden discrepar en silencio, porque es la misma función. Manipular la
  cadena de cualquiera de las dos formas —editar una entrada, o editarla y
  reparar la cadena después— hace que ambas reporten fallo; esto lo ejercita
  un job de CI que sella un efecto real a través de un nodo corriendo, edita
  `kernel.db` directamente, y comprueba que ambos caminos lo detectan.

### Anclaje externo

Que un nodo firme su propia historia establece que **nadie la alteró sin la
clave**. No establece que quien tiene la clave no lo hizo: un operador puede
reescribir entradas y refirmar el resultado, y la cadena recalcula, cada firma
verifica, y nada en disco registra que hace una hora decía otra cosa. Versiones
anteriores de este documento describían el ledger como evidencia que "un
auditor puede comprobar sin confiar en el proceso que la produjo", lo cual lo
sobrevendía — el auditor todavía tenía que confiar en quien tuviera la clave.

Un **witness** cierra eso con el mecanismo de Certificate Transparency: antes de
contrafirmar, verifica una *prueba de consistencia* RFC 6962 — las entradas que
ya avalé siguen siendo, sin cambios y en el mismo orden, un prefijo de estas. Un
nodo que reescribió la entrada 3 no puede producir una; no hay nada que forjar,
o la prueba existe porque la historia realmente es una extensión, o no existe. La
propiedad resultante es que **un nodo todavía puede mentir, pero no de forma
consistente a dos partes a lo largo del tiempo.**

[Witnessing abierto](#witnessing-abierto) cubre el mecanismo, el ancla pública y
`--open-witness`, incluido qué mantiene honesto al witness mismo. Lo que
corresponde aquí es la parte que trata
sobre la confiabilidad de este nodo.

Un nodo cuya propia clave se usó para reescribir la historia sigue pasando sus
*auto*comprobaciones —cadena intacta, checkpoint válido— y `aura verify` ahora
reporta esa combinación por lo que es:

```
  7 entries · chain intact · 1/1 checkpoint(s) valid · key 2b850090b4d4
  0/1 witness countersignature(s) verify

  NOT SOUND.
  → 1 of 1 witness countersignatures no longer match.
     Note: the chain and every checkpoint verify. That combination —
     internally perfect, externally contradicted — is what a rewrite by the
     holder of this node's own key looks like. Ask the witness directly.
```

Y el límite que sobrevive a todo lo demás: **las contrafirmas que guarda un nodo
las guarda ese nodo**, que puede descartar las incómodas. "0 witnesses" no prueba
que no se emitiera ninguna — la copia autoritativa es el log publicado del propio
witness. La herramienta dice "sin atestiguar", nunca "sin atestiguar, por lo
tanto bien".

### Recibos portátiles

Probar algo sobre un efecto significaba entregar el archivo SQLite entero: cada
efecto no relacionado venía con él, y quien lo recibía necesitaba una
herramienta que hablara el formato de almacenamiento. En la práctica la
evidencia nunca se compartía, lo que vuelve teórico lo de "verificable".

```powershell
.\kernel\aura.exe receipt sha256:9f2a… --out factura-99.json
.\kernel\aura.exe receipt --verify factura-99.json      # cualquiera, en cualquier parte
```

Un recibo es un solo documento JSON autocontenido: la entrada sellada, una
prueba de inclusión que la ubica en un árbol de tamaño declarado, el checkpoint
firmado sobre ese árbol, las contrafirmas de witnesses, y las atestaciones C5
que la entrada cita. `--verify` no abre ninguna base de datos, no contacta
ningún nodo, y no necesita material de clave más allá de lo que el documento
lleva. Una prueba de inclusión son ~log₂(n) hashes hermanos, así que un recibo
revela el efecto del que trata y absolutamente nada sobre las demás entradas.

### Atestación de la inferencia misma ([C5](spec/c5-attestation.md))

C4 registra *qué actuó y quién lo autorizó*. C5 registra **qué lo argumentó**:
qué modelo, qué revisión, qué cuantización, qué parámetros de muestreo, qué
semilla.

```json
{ "engine": "llama.cpp", "model": "Qwen/Qwen2.5-1.5B-Instruct-GGUF",
  "model_revision": "f1d2d2f9…", "quantization": "Q4_K_M",
  "params": { "temperature": 0.7, "seed": 42 },
  "prompt_sha256": "b221…", "energy": { "millijoules": 4120.5, "source": "nvml" } }
```

Un skill adjunta una a cualquier envelope que emite (`ctx.emit(..., attest=...)`
en el SDK de Python). El kernel la direcciona por contenido, la guarda una vez,
y cita su hash en la entrada del ledger de **cada efecto al que esa salida
llevó causalmente** — así una entrada responde "sobre qué base pasó esto", y un
cambio silencioso de modelo altera hashes ya sellados en una cadena de solo
anexado.

**Qué prueba esto, dicho sin rodeos, porque la exageración es tentadora:** una
atestación es una *afirmación del skill*, ligada de forma infalsificable a lo
que causó y al momento en que se hizo. **No** es prueba de que el skill dijo la
verdad. Un skill que miente sobre su modelo produce un registro inalterable de
una mentira. Lo garantizado es que la afirmación no puede editarse después, no
puede desligarse de sus consecuencias, y no puede antedatarse. Cerrar el resto
requiere atestación por hardware (un quote de TEE); el campo `tee` está
reservado para eso y está vacío en todos los nodos de hoy.

Vienen con ello dos seguridades relacionadas. Las descargas de pesos resuelven
y **fijan la revisión de Hugging Face antes de descargar**, así `model_revision`
nombra los bytes que realmente se trajeron y no lo que `main` apuntara después.
Y los formatos de pesos basados en pickle (`.bin`, `.pt`, `.ckpt`) se
**rechazan** en vez de advertirse — ejecutan código arbitrario al cargarse, y
los dos formatos que este runtime usa de verdad (GGUF, safetensors) son datos
puros.

### Lista de materiales

```powershell
.\kernel\aura.exe bom sess-8f3a --out inventario.json
#  ML-BOM CycloneDX 1.6 · 2 componente(s) de skill, 1 componente(s) de modelo
```

Toda herramienta de AI-SBOM construye su inventario desde un manifiesto o un
lockfile — una declaración de lo que *se suponía* que corriera. `aura bom` lo
construye desde el ledger: lo que corrió de verdad, citado por qué efectos,
bajo qué política. Un modelo configurado pero nunca invocado no aparece; uno
intercambiado en tiempo de ejecución sí. La salida es CycloneDX 1.6 con
componentes `machine-learning-model`, así que encaja en herramientas que ya
existen — relevante para las obligaciones de registro del Reglamento de IA de
la UE, que aplican a sistemas de alto riesgo desde el 2 de diciembre de 2027
(ver [Modelo de seguridad](#modelo-de-seguridad) para el aplazamiento del
Omnibus). Hereda exactamente el estatus de C5: un registro fiel de lo que se
*afirmó* y de lo que causó.

El payload mismo nunca se guarda — solo `payload_sha256` — así que el ledger
se mantiene pequeño (~300–400 bytes/entrada) y un payload con datos personales
no se vuelve permanentemente indeleble. `GET /v1/ledger` lista entradas
recientes (paginado, detrás del token como todo lo demás); `/healthz` incluye
un resumen de una línea (número de entradas, número de checkpoints, clave
pública del nodo) lo bastante barato como para calcularlo en cada probe.

Los principios a los que apunta el diseño: permisos **basados en capacidades**,
**humano en el bucle para las acciones**, **auditable por construcción**, y que
**el registry sea quien aplica las reglas** al publicar/instalar, para que el
runtime local nunca estorbe la experimentación. Tres se sostienen hoy: el log
causal de eventos hace una sesión auditable a posteriori, la puerta motor de
abajo es un invariante del kernel, y el ledger de efectos hace esa auditoría
verificable criptográficamente. El cuarto todavía no: `permissions` es una
declaración que el registry te enseña al instalar, no un sandbox, y un skill
corre con los privilegios de quien lo arrancó.

**El invariante de la puerta motor.** Toda arista que entrega a un skill
`motor.*` lleva la puerta que decidió la política del nodo. El ejecutor la
aplica al instanciar la sesión, no la herramienta que produjo el grafo, así que
es igual de cierto para un grafo declarado a mano, para la salida del planner,
para un grafo generado por MCP, o para un editor de grafos que todavía no
existe — implementado en
[`kernel/internal/executor/session.go`](kernel/internal/executor/session.go) y
[`policy.go`](kernel/internal/executor/policy.go), y cubierto por tests,
incluido un job de CI adversarial que corre un binario real con los flags por
defecto y comprueba que rechaza lo que debe.

---

## API HTTP y WebSocket

Un nodo sirve todo en un solo puerto (9080 por defecto).

**Kernel — HTTP**

| Método y ruta | Propósito |
|---|---|
| `GET /` | La UI del plano de control. |
| `GET /healthz` | Identidad del nodo, modo, majors de protocolo/IR, uso de admisión. |
| `GET /v1/skills` | El catálogo vivo de capacidades. |
| `GET /v1/skills/config?id=<id>` | El schema de config declarado por un skill más sus valores efectivos actuales. |
| `PUT /v1/skills/config?id=<id>` | Fija overrides de config en runtime; validados, persistidos y empujados al skill vivo. |
| `GET /v1/graphs` · `GET /v1/graphs/{id}` | Listar / obtener grafos registrados. |
| `POST /v1/graphs` | Registrar un documento IR C2. |
| `GET /v1/sessions` · `GET /v1/sessions/{id}` | Listar / resumir sesiones. |
| `GET /v1/sessions/{id}/events` | El log causal de eventos de una sesión (con timestamps). |
| `POST /v1/projections` · `GET /v1/projections` | Conectar / listar proyecciones. |
| `POST /v1/projections/{name}/promote` | Cambiar el modo de una operación. |
| `POST /v1/ingress` · `GET /v1/ingress` | Declarar / listar rutas de webhook entrantes. |
| `DELETE /v1/ingress/{name}` | Revocar una ruta entrante. |
| `GET /v1/approvals` | Gates de aprobación humana esperando, con la tool y los argumentos que los levantaron. |
| `POST /v1/approvals/{id}` | Responder uno: `{"approve": true\|false}`. El campo es obligatorio — una decisión omitida es un `400`, nunca una suposición. |
| `GET /v1/schemas` | Cada referencia de schema que este nodo puede compilar. |
| `GET /v1/schemas/{ref}` | Un documento de schema (`/v1/schemas/std/text@1`). |
| `GET /v1/grammars/{ref}` | La gramática GBNF de ese schema, como `text/plain`. |
| `GET /v1/ledger?from_seq=&limit=` | Entradas paginadas del ledger de efectos ([C4](spec/c4-ledger.md)). |
| `GET /v1/ledger/verify` | Recalcula la cadena, el árbol Merkle y cada firma; `200` si es sólida, `409` si no. |
| `GET /v1/ledger/head` | Posición actual: efectos sellados, cabeza de cadena, cabeza Merkle. |
| `GET /v1/ledger/statement?from_seq=` | Lo que este nodo presenta a un witness: cabeza firmada + prueba de consistencia. |
| `POST /v1/ledger/witness` | Este nodo actuando como witness de otro. Devuelve una contrafirma, o `409` con el motivo del rechazo. |
| `GET /v1/ledger/witness/last-seen?node=` | Hasta dónde ha avalado ya este nodo a otro. |
| `POST /v1/ledger/witness/record` | Registra una contrafirma sobre el ledger propio (se reverifica antes de guardarla). |
| `GET /v1/sessions/{id}/bundle` | El audit bundle de la sesión. |
| `GET /openenv/spec` · `POST /openenv/reset` · `POST /openenv/step` | Superficie OpenEnv — cada grafo registrado es un entorno. |
| `GET /openenv/state` · `GET /openenv/bundle?episode=` | Metadatos del episodio, y su audit bundle. |
| `GET /v1/ledger/receipt/{hash}` | El documento de evidencia portátil de un efecto sellado. |
| `GET /v1/ledger/attestations/{hash}` | Una atestación de inferencia C5, reverificada contra su dirección de contenido. |
| `POST /hooks/{name}` | Recibir un webhook: verificado con HMAC, abre su propia sesión. |
| `GET /.well-known/agent.json` | Tarjeta de descubrimiento A2A. |
| `POST /mcp` | Endpoint MCP Streamable HTTP (JSON-RPC). |

**Kernel — WebSocket**

| Ruta | Propósito |
|---|---|
| `GET /ws/skill` | Un skill se conecta aquí (primer frame: un envelope `register` C3). |
| `GET /v1/stream?graph=<id>[&session=<id>]` | Un cliente abre una sesión contra un grafo. |

Los clientes pueden enviar un envelope C3 completo, o el atajo `{"text": "…"}`
— práctico para `curl` o un navegador sin el SDK.

**Registro — HTTP** (servido por `aura registry serve`, puerto 9091 por defecto)

| Método y ruta | Propósito |
|---|---|
| `GET /r1/health` | Vitalidad del registro. |
| `POST /r1/packages` | Publicar un paquete firmado. |
| `GET /r1/packages[?capability=<cap>]` | Listar / descubrir paquetes. |
| `GET /r1/packages/{org}/{cat}/{name}[/{version}]` | Metadatos del paquete. |
| `GET /r1/packages/{org}/{cat}/{name}/{version}/artifact` | Descargar el artefacto. |

---

## Estructura del repositorio

```
README.md           La puerta de entrada corta, en inglés.
GUIDE.md            La referencia completa, en inglés.
README-ES.md        La puerta de entrada corta, en español.
GUIDE-ES.md         Este documento — la referencia completa.
CONTRIBUTING.md     Cómo contribuir al kernel, SDK y spec (en inglés) —
                    para agregar un skill, copia skills/echo/ (el ejemplo
                    mínimo trabajado) o cualquier otro bajo skills/.
LICENSE.md          Mapa de licencias por componente, licenciamiento comercial.
LICENSE-APACHE-2.0.txt   Texto completo de Apache-2.0 (spec, SDK, docs).
LICENSE-AGPL-3.0.txt     Texto completo de AGPLv3 (kernel, UI, skills de primera parte).
spec/               Los cinco contratos congelados + JSON Schemas + suite de conformidad.
  c1-manifest.md      C1 — manifiesto de skill.
  c2-graph-ir.md      C2 — la IR única de grafo.
  c3-channel.md       C3 — el protocolo de channel.
  c4-ledger.md        C4 — ledger de efectos, cabeza Merkle, witnessing, recibos.
  c5-attestation.md   C5 — atestación de inferencia, y qué NO prueba.
  enums.yaml          Fuente única de verdad de cada enum que usan C1-C5;
                      genera las constantes Go/Python/TypeScript (scripts/gen_ssot.py).
  schemas/            JSON Schemas ejecutables para C1-C3 y la atestación C5.
  conformance/        Suite de caja negra (runner.py) + vectores de test reutilizables.
kernel/             El kernel en Go — el binario único `aura`.
  cmd/aura/           Comandos CLI (up, chat, do, connect, publish, why, federate,
                      verify, witness, receipt, bom, guard, approve, …).
  internal/           identity · channel · registry · executor · store · gateway
                      · ledger (ledger de efectos C4 + árbol Merkle, witnessing,
                      recibos portátiles, atestaciones C5, ML-BOM) · projection · signing
                      · hub (registro) · mcpsrv (skills como tools MCP) · mcpcli
                      (cliente MCP: los servidores que corre otro) · guard (las
                      tool calls de un agente, puestas detrás del checkpoint) ·
                      approvals (donde un gate encuentra a un humano) · fed
                      (federación) · spec (constantes generadas) · config (la
                      capa de archivo --config, ver "Configuración de skills en
                      tiempo de ejecución").
ui/                 Frontend del plano de control (React + Vite + TypeScript, i18n).
                      · graph/ (el modelo C2 del lienzo: round-trip del IR,
                      disposición, las reglas que comprueba pronto) · markdown/
                      (un parser pequeño y su renderer, para los contratos) ·
                      reference/ (generado — ver scripts/gen_ui_reference.py) ·
                      hooks/ (useLiveGraphs: lo que el nodo está corriendo) ·
                      test/ (el modelo de grafos contra los vectores de
                      conformidad de spec/).
sdk/python/         El SDK `aura` para escribir skills (incluye aura.llm.ChatBackend).
sdk/node/           @deepaxiom/aura — expone las funciones de una app como
                    skills, o maneja un grafo desde TypeScript. Tipos
                    generados desde spec/schemas.
skills/             Skills de referencia — ejemplos trabajados que llevan el
                    org `example/`, no un catálogo. Uno por forma: echo (el
                    mínimo), llm-chat, asr, tts, sentence-chunker, planner,
                    model-manager, model-fit, memory-context, postgres-cdc. Ver
                    skills/README.md.
scripts/            release.ps1 — construye un zip distribuible. gen_ssot.py
                    (spec/ → constantes generadas), gen_ui_reference.py
                    (GUIDE.md + spec/ + el uso del binario → la referencia
                    del plano de control), check_links.py, adversarial.sh y
                    ledger_adversarial.py (CI: un binario real rechaza lo que debe).
aura-landing/       El sitio de marketing (Astro) — independiente del runtime.
```

---

## Compilación y release

```powershell
# Recompila el kernel tras cambios en Go:
cd kernel; go build -o aura.exe ./cmd/aura

# Reconstruye la UI y re-embébela recompilando el kernel:
cd ui; npm run build; cd ..\kernel; go build -o aura.exe ./cmd/aura

# Ejecuta la suite de conformidad contra un nodo en marcha:
pip install websockets jsonschema
python spec\conformance\runner.py --port 9080

# Empaqueta una release distribuible (binario con UI embebida + SDK + skills + spec):
.\scripts\release.ps1 -Version 0.1.0
#   → release\aura-0.1.0-windows-amd64.zip  (~10 MB)  →  descomprimir y .\aura\aura.exe up
```

---

## Estado de los hitos

**Esto es pre-1.0.** Ver [ROADMAP-ES.md](ROADMAP-ES.md) para lo que falta y a quién le bloquea, y [Estado de los hitos](#estado-de-los-hitos) para qué está construido y
qué no — los cimientos, la conectividad, la voz multicanal en streaming, la
seguridad del nodo y el ledger de efectos están todos, cliente de navegador
incluido. Los dieciocho hitos de abajo corren de extremo a extremo; la suite de
conformidad (59 comprobaciones, `spec/conformance/`) cubre C1-C3, no los hitos,
y las garantías propias del ledger de efectos las comprueba, por separado, un
job de CI adversarial (ver [Modelo de seguridad](#modelo-de-seguridad)). Existe
una suite `go test` que cubre *parte* del kernel — ver [cobertura de
tests](#cobertura-parcial) abajo para saber qué parte — y el CI bloquea
cualquier merge que rompa la compilación, los tests, el detector de carreras,
el formato, el bundle embebido de la UI, la suite de conformidad, la
comprobación de SSOT, o un enlace de la documentación.

"Con tests" abajo significa cubierto por tests automatizados en CI. "Verificado
a mano" significa que se ejercitó manualmente y funciona, pero nada impide una
regresión.

| Hito | Qué entregó | Verificación |
|---|---|---|
| Kernel de binario único + SDK + distro | `aura up` → UI + chat con LLM local, sin servicios externos | Con tests |
| Spec + conformidad | Contratos congelados — C1 v1.5, C2 v1.2, C3 v1.6, C4 v1.2, C5 v1.0; suite de caja negra de 59 comprobaciones | Con tests |
| Voz multicanal en streaming | Grafo `voice` + cliente de navegador: transcripciones parciales, respuestas habladas, barge-in funcionando | Kernel con tests; cliente verificado a mano |
| Conectividad sin OpenAPI | Patrón de conector declarativo, SDK TypeScript, ingreso de webhooks, observación de tráfico | Ingreso y generador con tests; patrón de conector verificado a mano (no se incluye ningún skill de conector de primera parte) |
| Drivers de modelos + admisión | Skills de ASR / TTS; `--memory-budget` | Admisión y ASR/chunker con tests; drivers a mano |
| Planner + `aura do` | Objetivo en lenguaje natural → plan → ejecución con puertas | Verificado a mano |
| Explicabilidad + replay | `aura why` (narrado por LLM), `aura replay` | Verificado a mano |
| Proyección OpenAPI legacy | `aura connect` → operaciones como skills, cuatro salvaguardas | Con tests (`projection` 61%) |
| Registro federable | `aura publish/add/run`, Ed25519 + trust-on-first-use | Con tests (`signing` 90%, `hub` 81%) |
| Fronteras estándar | Servidor MCP, tarjeta A2A, exportación OpenTelemetry | MCP con tests (63%); OTel a mano |
| Federación de nodos | `aura federate`, resolución entre nodos | Con tests (`fed` 87%) |
| Transporte negociado, LAN/mismo-host (Fase 3) | Una conexión pooled por capability federada en vez de un dial por envelope; cancel por envelope, no por cierre de socket; clasificación de ruta medida (`aura federate` la imprime) | Con tests — reuso de conexión, demux de relays concurrentes, cancel-no-afecta-a-otro, auto-recuperación tras una caída |
| Ledger de efectos (C4) | Atestación encadenada por hash y firmada de cada efecto; `aura verify`, `GET /v1/ledger[/verify]` | Con tests (`ledger` 89%); detección de manipulación probada por un job de CI con un binario real |
| Cabeza Merkle + recibos portátiles (C4 v1.2, Fase 4) | Árbol RFC 6962 sobre cada entrada, comprometido en cada checkpoint; `aura receipt` exporta la evidencia de un efecto y `--verify` la comprueba sin base de datos, nodo ni red | Con tests — pruebas de inclusión en cada posición para árboles de tamaño 1-64, de consistencia para cada par (m,n) hasta 48, y casos negativos incluyendo una entrada reescrita con el hash reparado |
| Anclaje externo (C4 v1.2, Fase 4) | `aura witness <peer>`: un tercero verifica una prueba de consistencia antes de contrafirmar, así una historia reescrita no puede colarse ante quien ya vio la anterior. Cada nodo es un witness | Con tests — extensión honesta aceptada, historia bifurcada rechazada, contrafirma ligada a su nodo, handshake HTTP entre dos nodos |
| Atestación de inferencia (C5, Fase 4) | Un skill declara motor/modelo/revisión/cuantización/muestreo/semilla; el kernel la direcciona por contenido y la cita en cada efecto que la salida causó. Revisiones de HF fijadas, formatos de pesos pickle rechazados, energía reportada con su fuente | Con tests — de extremo a extremo por el camino real del ejecutor (entregado, denegado, malformado y sin inferencia), más ida y vuelta del recibo |
| ML-BOM (Fase 4) | `aura bom` emite CycloneDX 1.6 desde el ledger — modelos y skills que corrieron de verdad, no los configurados | Verificado a mano |
| Reversibilidad (`aura undo`, Fase 2) | Deshace un efecto vía su puerto `compensates` declarado — a su vez un efecto gateado y sellado; se rechaza antes de construir la sesión si ya fue deshecho, nunca se entregó, o es irreversible | Con tests (adversariales: doble undo, sin compensación, gate denegado) |
| Resume de sesión (Fase 2) | La ventana de dedup, los índices causal/en-vuelo, los gates pendientes y los contadores `Seq` por hop de una sesión se reconstruyen desde el log causal al reconectar — da igual si el cliente se cayó o si el propio proceso del kernel reinició, ambos casos toman el mismo camino | Con tests (adversariales: un gate pendiente sobrevive, un cancel sigue alcanzando una cadena multi-hop en vuelo, `Seq` continúa en vez de reiniciarse) |
| Replay determinista (Fase 2) | `aura replay` compara las entradas selladas del ledger de la sesión original y la reproducida, no solo la transcripción visible al cliente — cierra la cuarta propiedad de la tesis (Reproducible) | Con tests (`ledger.Diff`, puro, 8 casos tabulares) |
| Concurrencia: group commit, sesiones en lote, reparto entre réplicas | Cada envelope se sigue registrando de forma durable antes de reconocerse; lo que cambió es que quienes ya esperaban se suman a una transacción. El registro de sesiones entró al mismo lote, y la resolución ahora rota entre réplicas de un paquete en vez de responder siempre desde la conexión más reciente | Medido, no afirmado: 200 sesiones concurrentes pasaron de 344 msg/s con p50 489 ms a 1,908 con p50 63 ms, y 1,000 sesiones (16 réplicas) alcanzan 4,215 msg/s con p50 65 ms. El throughput ahora sube con la carga en vez de derrumbarse. Reproducible con `kernel/cmd/loadgen/` |
| Aislamiento de fallos por sesión | Un panic al rutear falla esa sesión y se registra con su stack, en vez de desenrollarse hasta el tope de la goroutine y llevarse el nodo — todas las demás sesiones, conexiones y el estado en memoria de la cadena del ledger — con él | Probado — un skill que revienta en la entrega falla su propia sesión, el error se reporta en vez de tragarse, y el nodo sigue arrancando y ruteando sesiones nuevas |
| Transporte QUIC / WebTransport (QoS de C3, hecha real) | Las tres clases de QoS se vuelven tres primitivas de transporte: `realtime` un datagrama QUIC (o su propio stream si excede el tamaño), `reliable` un stream ordenado, `bulk` un stream por transferencia. Se sirve en el mismo número de puerto que TCP; un peer que no alcanza UDP conserva la ruta WebSocket | Probado contra un cliente QUIC real, no simulado — FIFO en el carril reliable, un frame realtime llegando como datagrama de verdad, fallback por tamaño, `bulk` que no retrasa un envelope terminal, un productor que nunca se bloquea, y un skill registrándose por WebTransport en el gateway real (`wtsrv` 78.5%) |
| Skills Wasm, sandbox real (Fase 3) | `format: wasm` sobre wazero — `filesystem` (preopen WASI) y `egress_http` (host import `env.http_fetch` propio, allowlist de hostname exacto) ambos aplicados, no declaraciones impresas; alojado dentro del proceso, nunca un OS process aparte | Probado contra guests reales compilados `GOOS=wasip1`, no simulado (`wasmrt` + `gateway` de punta a punta; scoping de permisos probado bajo concurrencia) |
| CDC de Postgres (Fase 3) | `skills/postgres-cdc` — replicación lógica (`test_decoding`, sin instalar extensión) se convierte en eventos causales `std/db-change@1`, uno por fila cambiada | Probado contra un `postgres:16` real de Docker, no simulado — INSERT/UPDATE/DELETE, NULL, filtro de tablas |
| Cliente MCP + frontera con streaming | `internal/mcpcli` — un cliente MCP sobre stdio y Streamable HTTP; y `tools/call` del lado servidor ahora responde un event stream, así la salida de un skill llega al agente a medida que se produce | Probado — stdio contra un subproceso real (demux concurrente, cancelación, muerte del servidor), desempaquetado SSE, paginación, y la frontera con streaming de punta a punta (`mcpcli` 89.6%, `mcpsrv` 83.2%) |
| Gates respondibles | Un gate de aprobación humana levantado por un cliente programático se estaciona en una cola del nodo y se responde desde la UI, `aura approve` o `POST /v1/approvals/{id}`; sin respuesta, se deniega solo | Probado — aprobar, denegar, expiración-deniega-en-vez-de-colgar, doble resolución, y el fallback sin cola que igual rechaza (`approvals` 100%) |
| `aura guard` (tools MCP detrás del checkpoint) | Los servidores MCP que un agente ya usa se registran como skills — `motor` salvo que el operador opte por confiar en `readOnlyHint` — así cada tool call pasa por policy, gate y ledger sin que guard implemente ninguno | Probado de punta a punta contra un gateway y un executor reales: el servidor upstream **no** se llama antes de la aprobación, **sí** después, una denegación nunca lo alcanza, y las anotaciones no confiadas igual gatean (`guard` 80.9%) |
| fsync del log de eventos en la ruta de entrega | `AppendEvent` es una transacción por envelope en la ruta caliente; el `synchronous=FULL` por defecto de SQLite hacía que cada token en streaming pagara un fsync. Ahora `NORMAL` (override con `AURA_SQLITE_SYNCHRONOUS`) | Medido, no afirmado: 82 → 1.584 round-trips de envelope/seg por el gateway y el executor reales, p50 12,0 ms → 0,42 ms en una máquina de desarrollo. El cambio además expuso un bug de frontera latente en la ventana de dedup de ingress, corregido junto con él |

### Qué significa hoy "streaming y persistente" — y qué no

Ambas palabras son ciertas para una parte de la pila y no para el resto. Dónde
cae la línea:

**Ya sostiene hoy:**
- Cada sesión mantiene un log de eventos causal, duradero y append-only en
  SQLite embebido — el historial sobrevive a un reinicio, y `aura why` /
  `aura replay` lo leen de vuelta. Esto es persistencia de auditoría: el
  registro de lo que pasó sobrevive, la sesión viva no.
- La primitiva de cable es un stream tipado, causal y con back-pressure (C3),
  no una llamada petición/respuesta — texto/audio/eventos fluyen como el mismo
  tipo de envelope, ordenado e idempotente.
- **Las conexiones se mantienen honestas.** Los dos sockets de larga vida
  (`/ws/skill`, `/v1/stream`) hacen ping cada 20s y descartan al peer tras 60s
  de silencio, así que una conexión TCP medio muerta — un portátil que se
  durmió, un wifi que se cayó, un proceso matado con SIGKILL — libera su
  sesión, su reserva de admisión y su entrada en el registro, en vez de
  filtrarlas durante toda la vida del nodo.
- **El QoS por arista se fuerza, no solo se declara.** Una arista `reliable`
  bloquea a su productor hasta que el receptor se pone al día; una `realtime`
  no bloquea nunca y descarta el frame *más viejo*, porque en un stream en
  vivo el frame rancio es el que no vale. Los envelopes terminales (`done`,
  `error`, una petición de aprobación) viajan siempre de forma fiable, diga lo
  que diga la arista. En una arista `realtime` el log de eventos conserva el
  envelope y sus enlaces causales pero sustituye el payload por una
  descripción de sí mismo — un minuto de habla son megabytes de base64 y una
  grabación permanente de alguien hablando, y ni `aura why` ni `aura replay`
  necesitan las muestras para hacer su trabajo.
- **Resume de sesión.** Reconectar un cliente con el mismo id de sesión
  reconstruye la ventana de dedup, los índices causal/en-vuelo, las puertas
  pendientes y los contadores `Seq` por hop desde el log causal de eventos —
  da igual si el cliente se cayó o si el propio proceso del kernel reinició,
  ambos casos toman el mismo camino. Ver [Estado de los hitos](#estado-de-los-hitos).

**Todavía no cierto:**
- **El QoS `bulk` sigue sin definir.** `reliable` y `realtime` están
  especificados y forzados (ver arriba); `bulk` es un nombre en el enum sin
  comportamiento detrás, y se trata como `reliable` para no perder nada en
  silencio contra una regla no especificada.
- **Una sesión, un socket, un nodo.** No hay vista multi-dispositivo de la
  misma sesión viva (la forma "un guild, muchos clientes conectados" que tiene
  Discord), y no hay failover si el nodo dueño cae a media sesión — solo el
  log de eventos sobrevive a eso; el estado de ruteo vivo no.
- <a id="cobertura-parcial"></a>**Cobertura de tests parcial, concentrada en el
  núcleo.** Medida con `go test ./... -cover`:

  | Paquete | Cobertura | | Paquete | Cobertura |
  |---|---|---|---|---|
  | `approvals` | 100,0% | | `guard` | 80,9% |
  | `config` | 100,0% | | `wtsrv` | 79,7% |
  | `channel` | 97,5% | | `registry` | 78,8% |
  | `signing` | 90,4% | | `identity` | 78,3% |
  | `approver` | 90,0% | | `mcpsrv` | 75,6% |
  | `sandbox` | 89,7% | | `ledger` | 73,7% |
  | `mcpcli` | 89,6% | | `projection` | 58,6% |
  | `fed` | 86,4% | | `gateway` | 56,4% |
  | `broker` | 86,2% | | `store` | 56,0% |
  | `wasmrt` | 85,6% | | `grammar` | 52,3% |
  | `executor` | 83,7% | | `cmd/aura` | **7,5%** |
  | `seglog` | 83,7% | | | |
  | `hub` | 81,2% | | | |

  **Ningún paquete está en cero.** `internal/` está en **72,3%**; el agregado
  del módulo entero (`go test ./...`, todos los paquetes) es **54,9%**, y la
  diferencia es enteramente `cmd/aura`: unas 3.200 líneas de CLI al 9,5%, que
  se prueban levantando nodos de verdad y no con tests unitarios — la lógica
  central de `aura verify` es la excepción, probada directamente (ver [Modelo
  de seguridad](#modelo-de-seguridad)).

  Los números de arriba están medidos, no recordados, y dos son más bajos de lo
  que una release anterior afirmaba: `registry` está en 77,4% donde esta tabla
  decía 92,6%, y `gateway` en 58,7% donde decía 67,6%. Ambos habían crecido sin
  que sus tests crecieran con ellos. `ledger` y `store` están por debajo de sus
  cifras viejas por la misma razón — ambos ganaron superficie considerable
  (árbol Merkle, witnessing, recibos, atestaciones, ML-BOM) y los tests nuevos,
  aunque exhaustivos con la criptografía, no cubren los caminos de error de
  cada accesor nuevo. Las garantías están cubiertas; la fontanería alrededor
  está más floja de lo que los números viejos sugerían, y citar los números
  viejos sería la mentira más cómoda.

  La UI sigue sin suite de tests. La suite de conformidad ejercita el kernel de
  punta a punta sobre el cable; la garantía de detección de manipulación del
  ledger de efectos se ejercita por separado, contra un binario real, con un
  job de CI adversarial dedicado. Trátalo como pre-producción.

---

## Diseñado, aún no construido

Alcance honesto. Todas estas piezas descansan sobre los mismos contratos
congelados — ninguna cambia C1, C2, C3, C4 ni C5 — pero aún no están
implementadas. El sandboxing de skills es la que impide que esto sea seguro
fuera de una red de confianza:

- ~~**Autenticación del nodo y loopback por defecto**~~ — **hecho.** Un nodo ata
  loopback, genera un token bearer, comprueba el origen del WebSocket y puede
  terminar TLS; una política de nodo decide qué puede actuar sobre el mundo y un
  grafo no puede renunciar a ella. Ver [Modelo de
  seguridad](#modelo-de-seguridad) y [Estado de los hitos](#estado-de-los-hitos).
- **Un sandbox microVM para skills `format: source`** — la mayor brecha de
  seguridad que queda, hoy estrechada en vez de abierta. `--sandbox process`
  convierte el entorno en una lista de permitidos en lugar de una herencia y
  enjaula el directorio de trabajo, lo que cierra la fuga accidental de
  credenciales; `format: wasm` sí está genuinamente aislado. Ninguno de los dos
  contiene código hostil. Una frontera real significa una microVM (Firecracker,
  Cloud Hypervisor, Kata) o gVisor, que necesita Linux con KVM — el backend está
  declarado y **se rechaza al arranque** en vez de simularse, porque una degradación
  silenciosa sería peor que una ausencia. Ver [Aislamiento de
  skills](#aislamiento-de-skills).
- **Lo que los modos `site` y `published` deberían hacer** — identidad
  automática del nodo, canales cifrados entre nodos y permisos evaluados en
  tiempo de ejecución. Hoy la cadena del modo cambia dos comportamientos en
  `published` — una arista `motor.*` sin gate se rechaza en vez de repararse, y
  una renuncia a nivel de grafo nunca se honra — mientras que `site` sigue
  comportándose exactamente como `local`. La tabla del [Modelo de
  seguridad](#modelo-de-seguridad) así lo dice.
- **Atestación por hardware (TEE)** — la brecha que mantiene una atestación C5
  como *afirmación* en vez de prueba. Un quote de Intel TDX, AMD SEV-SNP o
  NVIDIA Confidential Computing, ligando una medición del proceso que
  realmente corrió el modelo, la cerraría. El registro ya tiene un campo
  `tee`, así que aterrizarlo es aditivo; lo que falta es la integración y el
  hardware para probarla. Hasta entonces la documentación dice "afirmado",
  nunca "probado" — ver [Modelo de seguridad](#modelo-de-seguridad).
- **Librería embebible (`libaura`)** — el mismo cliente de channels como
  librería enlazable en C/Rust con bindings Kotlin/Swift/JS, para que una app de
  TV/móvil/reloj capture y reproduzca streams y ejecute lógica ligera sin un
  nodo completo. Es un esfuerzo de empaquetado; el protocolo ya es el contrato.
- **Protocolo de periféricos** — un puente MQTT/BLE mínimo que proyecta sensores
  y controles (que no ejecutan lógica) como puertos de grafo — estructuralmente
  idéntico al host de proyecciones OpenAPI.
- **Más fronteras** — importar servidores MCP externos como skills, el protocolo
  de mensajes A2A completo, y AG-UI hacia los frontends.
- **Nube gestionada** — un despliegue alojado y multi-tenant. Deliberadamente lo
  último: la nube es solo otro nodo, y el runtime abierto debe estar completo
  primero.

---

## Licencia y gobernanza

**Deep Axiom es la marca paraguas; AURA nombra la arquitectura de kernel** — los
contratos y el modelo de ejecución que implementa `kernel/`. Ambos se licencian
de forma distinta a propósito, en detalle en [`LICENSE.md`](LICENSE.md):

- **Apache-2.0** para `spec/`, `sdk/`, `aura-landing/`, `scripts/`
  y esta documentación — neutrales y abiertos para siempre. Cualquiera puede
  construir un kernel, SDK o skill conforme al estándar con cero fricción y
  cero obligación; el ecosistema tiene que seguir siendo seguro para autores
  comerciales de skills para que el marketplace tenga sentido.
- **AGPLv3, o una licencia comercial,** para `kernel/`, `ui/` y los `skills/`
  de primera parte — el producto en sí. Autohospedar, modificar y auditar no
  tienen restricción; ofrecer una copia modificada como servicio de red exige
  publicar tus modificaciones (AGPLv3) o un acuerdo comercial. Las
  organizaciones sin fines de lucro y las fundaciones pueden solicitar la
  licencia comercial sin costo o a tarifa reducida — ver
  [`LICENSE.md`](LICENSE.md) para el procedimiento.

Como el registro es federable y los contratos tienen suite de conformidad,
ningún administrador puede capturar el *estándar* — hacer un fork de la spec y
los SDKs es siempre trivial. El copyleft del kernel existe para el problema más
acotado y distinto de que un proveedor de nube hospede el producto en sí sin
contribuir nunca de vuelta.

*Todo lo descrito en este README se ha ejecutado; nada de aquí es un plan
disfrazado de funcionalidad — lo que solo está diseñado vive en su propia
sección. Es pre-1.0: `cmd/aura` tiene cobertura delgada de tests unitarios (se
ejercita levantando nodos reales en su lugar), la cobertura propia de la UI se
limita a su modelo de grafos y al parser de Markdown,
y el catálogo de integraciones es pequeño.
[Estado de los hitos](#estado-de-los-hitos) es el resumen exacto; si ese
apartado y este documento se contradicen alguna vez, el que tiene razón es
Estado de los hitos.*

---

## Referencias

En qué se apoyan las afirmaciones de esta guía. Varios de estos describen el
mismo problema que este runtime y lo resuelven de otra forma; son los más útiles
de leer, porque son los que muestran dónde una decisión de diseño de aquí fue una
elección y no la única opción.

**La amenaza a la que esto responde**

1. Kumar et al., *Model Context Protocol Threat Modeling and Analyzing Vulnerabilities to Prompt Injection with Tool Poisoning* — [arXiv:2603.22489](https://arxiv.org/abs/2603.22489). STRIDE/DREAD sobre los seis componentes de MCP; los metadatos de herramientas son la superficie de ataque principal del lado cliente, y la mayoría de los clientes los validan de forma insuficiente. Por eso `aura guard` tipa una herramienta sin anotar como `motor` y se niega a creerle a `readOnlyHint` sin que un operador lo diga.
2. *Parasites in the Toolchain: A Large-Scale Analysis of Attacks on the MCP Ecosystem* — [arXiv:2509.06572](https://arxiv.org/abs/2509.06572).
3. Cai et al., *Are You Getting What You Pay For? Auditing Model Substitution in LLM APIs* — [arXiv:2504.04715](https://arxiv.org/abs/2504.04715). Sustitución silenciosa de modelos medida en APIs desplegadas — el fallo concreto que el vínculo de atestación de C5 vuelve detectable.

**Evidencia, logs de transparencia y witnessing**

4. Syta et al., *Keeping Authorities "Honest or Bust" with Decentralized Witness Cosigning* — [arXiv:1503.08768](https://arxiv.org/abs/1503.08768). El origen del argumento sobre el que descansa el protocolo de witness: una autoridad a la que se puede pillar contradiciéndose no tiene por qué ser creída.
5. *Right to History: A Sovereignty Kernel for Verifiable AI Agent Execution* — [arXiv:2602.20214](https://arxiv.org/abs/2602.20214). Logs RFC 6962 y fronteras por capability en un kernel en Rust — el vecino arquitectónico más cercano a este.
6. *Notarized Agents: Receiver-Attested Confidential Receipts for AI Agent Actions* — [arXiv:2606.04193](https://arxiv.org/abs/2606.04193). Firma del lado receptor más logs contrafirmados por witness; un corte genuinamente distinto al mismo problema de evidencia, y que vale la pena comparar contra el recibo de C4.
7. *Context Lineage Assurance for Non-Human Identities in Critical Multi-Agent Systems* — [arXiv:2509.18415](https://arxiv.org/abs/2509.18415).

**Supervisión humana**

8. *Oversight Has a Capacity: Calibrating Agent Guards to a Subjective, Fatiguing Human* — [arXiv:2606.08919](https://arxiv.org/abs/2606.08919). El mejor argumento disponible contra gatear de más, y la razón de que sea la policy del nodo la que decide qué se gatea y no el grafo: un gate que salta con todo es un gate que acaba sellándose sin leer.

**Credenciales y computación confidencial**

9. *When Agents Handle Secrets: A Survey of Confidential Computing for Agentic AI* — [arXiv:2605.03213](https://arxiv.org/abs/2605.03213).
10. *CapSeal: Capability-Sealed Secret Mediation for Secure Agent Execution* — [arXiv:2604.16762](https://arxiv.org/abs/2604.16762). Convergencia independiente sobre la forma del broker de credenciales.
11. *Confidential LLM Inference: Performance and Cost Across CPU and GPU TEEs* — [arXiv:2509.18886](https://arxiv.org/abs/2509.18886). De dónde sale la cifra de 4–8% en H100, y por qué la inferencia respaldada por TEE dejó de ser teórica.

**Evaluación y el audit bundle**

12. *Do Agent Benchmarks Measure Capability? Protocol Validity in the Age of Agentic AI* — [arXiv:2607.22368](https://arxiv.org/abs/2607.22368). La forma de materiales retenidos en cuatro partes que implementa `aura bundle`.

**Almacenamiento**

13. Chursin, Kokoris-Kogias, Orlov, Sonnino, Zablotchi, *Tidehunter: Large-Value Storage With Minimal Data Relocation* — [arXiv:2602.01873](https://arxiv.org/abs/2602.01873). Tratar el log como almacenamiento permanente en vez de como buffer de recuperación, y la compactación deja de existir porque nada se reubica. La forma que sigue `internal/seglog`.
14. Silvestre et al., *Failure Transparency in Stateful Dataflow Systems* — [arXiv:2407.06738](https://arxiv.org/abs/2407.06738). La propiedad de corrección de la que la reanudación de sesión es un caso.

**Estándares y normativa, fuera de arXiv y de carga**

- [RFC 6962](https://www.rfc-editor.org/rfc/rfc6962) — Certificate Transparency. La construcción Merkle y las pruebas de inclusión y consistencia que C4 v1.2 usa literalmente.
- [RFC 8032](https://www.rfc-editor.org/rfc/rfc8032) — Ed25519. Todas las firmas de este sistema.
- [RFC 8446](https://www.rfc-editor.org/rfc/rfc8446), [RFC 9000](https://www.rfc-editor.org/rfc/rfc9000) — TLS 1.3 y QUIC, bajo WebTransport.
- [Reglamento (UE) 2024/1689](https://artificialintelligenceact.eu/) — el Reglamento de IA. Artículos 12 y 14 en particular; ver [Modelo de seguridad](#modelo-de-seguridad).
- [Reglamento (UE) 2026/1744](https://eur-lex.europa.eu/eli/reg/2026/1744/oj) — el Digital Omnibus sobre IA (en vigor el 27 de julio de 2026). Aplazó el régimen de alto riesgo al 2 de diciembre de 2027 (Anexo III) y al 2 de agosto de 2028 (Anexo I); el contenido de los artículos 12 y 14 no cambió.
- [draft-sharif-agent-audit-trail](https://datatracker.ietf.org/doc/draft-sharif-agent-audit-trail/) — un Internet-Draft individual, no respaldado por la IETF, que define un registro JSON de auditoría de agentes. Nuestro [draft de aprobación firmada](spec/proposals/draft-signed-human-approval.md) está diseñado para encajar dentro de su miembro `human_override`.
- ISO/IEC 42001 (sistemas de gestión de IA, cláusula 9.2 auditoría interna); prEN 18229-1 e ISO/IEC DIS 24970, ambos todavía en borrador.

---

## Referencia

La forma de los [Audit bundles](#audit-bundles) — y la razón de que este
runtime emita uno — viene de:

> **Do Agent Benchmarks Measure Capability? Protocol Validity in the Age of
> Agentic AI** · [arXiv:2607.22368](https://arxiv.org/abs/2607.22368) (2026)
>
> Examinó trazas publicadas de agentes y encontró que el 67% contenía
> *protocol exposures* — caminos por los que se puede ganar una puntuación sin
> la capacidad medida. Concluye que los informes deben incluir la evidencia
> necesaria para interpretarlos, y nombra los cuatro materiales que un runtime
> debe emitir: logs de trayectoria completos, procedencia de artefactos con
> hashes, configuración de modelo replayable, y baselines de comparación.

`aura bundle` emite esos cuatro. La correspondencia se encontró después —el log
causal, el ledger de efectos y la atestación de inferencia se construyeron cada
uno por razones ajenas—, que es por qué el bundle es un ensamblaje de piezas
existentes y no un mecanismo nuevo.
