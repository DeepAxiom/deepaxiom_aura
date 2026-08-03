# Deep Axiom — un runtime de streaming para skills de IA

**Estado: v0.3.0 — pre-1.0, pre-producción, Fase 1 de 2 hacia la beta abierta.** · **Construido sobre la arquitectura de kernel AURA · [Apache-2.0 (spec y SDK) · AGPLv3 o comercial (kernel) — ver LICENSE.md](LICENSE.md)** · [English version](README.md)

Deep Axiom es un runtime de código abierto para componer skills — LLMs, visión,
voz, OCR, APIs de negocio — en grafos que corren como streams tipados y vivos.
Un único binario contiene el kernel, la UI del plano de control, el almacén de
estado, el bus de mensajes y el ledger de efectos; no necesita cuenta, ni nube,
ni base de datos externa. Los grafos se escriben a mano o los genera un planner
a partir de un objetivo en lenguaje natural, y cada sesión guarda un log de
eventos causal append-only que puedes explicar o reproducir después.

Tres decisiones de diseño lo separan de las herramientas de workflows (n8n,
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
  hashes posteriores, y firma periódicamente la cabeza de la cadena con su
  propia clave. `aura verify` recalcula todo desde el archivo SQLite, sin el
  kernel corriendo — evidencia que un auditor puede comprobar sin confiar en
  el proceso que la produjo. Ver [Modelo de seguridad](#modelo-de-seguridad).

**Lo que todavía no es.** No hay resume de sesión: un socket caído pierde las
puertas pendientes y el estado en vuelo, y reconectar arranca una sesión nueva.
No hay vista multidispositivo de una sesión viva, ni failover si muere el nodo
propietario. Un nodo se autentica — loopback por defecto, token bearer,
comprobación de origen del WebSocket, TLS opcional — pero el aislamiento de
procesos para skills sigue sin existir, así que un skill corre con los
privilegios de quien lo arrancó. Lee [Modelo de
seguridad](#modelo-de-seguridad) antes de exponer un puerto.
[Estado de los hitos](#estado-de-los-hitos) lo detalla todo, y
[ROADMAP.md](ROADMAP.md) dice qué está construido y qué no. Léelos antes de
decidir si esto encaja con tu problema.

Para agregar una capacidad, escribe un skill y regístralo — no hay cola de
revisión, porque el registry lo alojas tú. Mira [`skills/`](skills/) para
patrones ([`skills/echo/`](skills/echo/) es el mínimo,
[`skills/tuya-status/`](skills/tuya-status/) un wrapper de API de solo lectura,
[`skills/tuya-command/`](skills/tuya-command/) uno que actúa sobre el mundo) y
[`CONTRIBUTING.md`](CONTRIBUTING.md) para el kernel/SDK/spec en sí.

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
16. [Drivers de modelos y admisión de recursos](#drivers-de-modelos-y-admisión-de-recursos)
17. [El marketplace](#el-marketplace)
18. [Explicabilidad y replay](#explicabilidad-y-replay)
19. [Federar nodos](#federar-nodos)
20. [Estándares en las fronteras](#estándares-en-las-fronteras)
21. [Los tres contratos](#los-tres-contratos)
22. [Modelo de seguridad](#modelo-de-seguridad)
23. [API HTTP y WebSocket](#api-http-y-websocket)
24. [Estructura del repositorio](#estructura-del-repositorio)
25. [Compilación y release](#compilación-y-release)
26. [Estado de los hitos](#estado-de-los-hitos)
27. [Diseñado, aún no construido](#diseñado-aún-no-construido)
28. [Licencia y gobernanza](#licencia-y-gobernanza)

---

## Qué problema resuelve

Las plataformas de automatización de workflows — n8n, Zapier, Make —
resolvieron "conecta mis herramientas existentes" para trabajo *por lotes*: se
dispara un trigger, corre una cadena de pasos una vez, y la ejecución termina.
Ese modelo encaja bien con una sincronización nocturna y con un webhook. Encaja
mal con una conversación de voz o una sesión multiagente larga, porque ahí el
estado que importa vive *entre* mensajes, no dentro de una ejecución.

Deep Axiom hace de la conexión la unidad de trabajo. No es una idea nueva — la
infraestructura de chat y videojuegos lleva años funcionando así, y Temporal y
los frameworks de voz en tiempo real resuelven cada uno una parte. Lo poco
habitual aquí es aplicarlo a trabajo de IA *conectado a legacy*: un runtime que
instalas junto a los sistemas existentes y que les habla por el mismo protocolo
de streaming que usa para modelos y clientes.

De esa decisión se derivan cuatro cosas:

- **Streams, no ejecuciones.** La unidad de comunicación es un stream de
  envelopes tipado, causal y con contrapresión sobre una conexión pensada para
  quedarse abierta. El texto fluye token a token, y cada sesión mantiene un log
  causal duradero en vez de descartar su estado al terminar.
- **Legacy primero.** El primer comando útil es *conecta lo que ya tienes*, no
  "crea un proyecto". Apúntalo a una especificación OpenAPI y sus operaciones
  se convierten en skills — de solo lectura por defecto, con aprobación humana
  obligatoria antes de cualquier escritura.
- **Una sola interfaz para cada modelo.** LLMs, visión, ASR, OCR, TTS,
  embeddings — todos tras el mismo contrato de skill, en local o remoto. Mover
  un skill a otra máquina cambia la colocación, no el grafo.
- **Distribución federable.** La especificación, el kernel y los SDKs son
  abiertos; el registry es federable, así que el catálogo del que instalas
  puedes alojarlo tú.

Esto es **pre-1.0**, y la historia de persistencia está a medias. Lo que se
sostiene hoy: el log de eventos duradero, el protocolo de envelopes en
streaming, la liveness de conexión y el forzado de QoS por arista. Lo que no:
resume de conexión, sesiones multidispositivo y failover de nodo.
[Estado de los hitos](#estado-de-los-hitos) traza esa línea con precisión.

---

## Cómo se compara

Casi todo lo que hace este runtime ya lo hace algo más — normalmente con más
madurez, más integraciones, o ambas. Un mapa honesto de dónde encaja:

| Si necesitas | Usa | Posición de Deep Axiom |
|---|---|---|
| Cientos de integraciones SaaS listas | **n8n, Zapier, Make** | No compite. Su catálogo *es* el producto; aquí hay ~15 skills de primera parte. |
| Workflows largos, duraderos y reproducibles | **Temporal** | No compite en durabilidad. Temporal sobrevive a la muerte del proceso a mitad de workflow; esto todavía no resume una sesión caída. |
| Agentes de voz en tiempo real en producción | **LiveKit Agents, Pipecat, OpenAI Realtime** | Mucha menos madurez. El primer sonido de una respuesta llega a ~0,41 s en una máquina de desarrollo con modelo local — nunca medido contra estos lado a lado, así que léelo como "usable", no como "competitivo". Elige esos salvo que necesites la voz sobre el *mismo* runtime que el resto. |
| Una librería de agentes dentro de tu app | **LangGraph, CrewAI, AutoGen** | Forma distinta. Esas son librerías con las que construyes; esto es un proceso que instalas al lado de sistemas existentes. |
| Descubrimiento de herramientas para un modelo | **MCP** | No es competencia — esto trae un servidor MCP para que sus skills sean herramientas MCP. |

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
- **Un binario, sin servicios externos.** ~18 MB con la UI embebida, y sin
  Postgres, sin Redis, sin broker, sin clúster que levantar antes del primer
  mensaje.

Los contratos sobre los que descansa esto están congelados y especificados en
[`spec/`](spec/), con una suite de conformidad con la que una implementación
demuestra que cumple.

---

## Funcionalidades destacadas

**Runtime y modelos**
- Un único binario autocontenido — kernel, UI del plano de control, almacén de
  estado y bus de mensajes en un solo archivo. Sin cuenta, sin nube, sin Docker,
  sin base de datos externa.
- Streams como primitiva universal: canales tipados, ordenados, causales,
  idempotentes, con back-pressure y medibles que transportan por igual texto,
  audio, documentos y eventos.
- Un solo formato de grafo para todo — la misma representación intermedia tanto
  si el grafo lo escribió un humano como si lo generó un planner, de modo que un
  solo depurador, un solo modelo de permisos y una sola vía de replay cubren
  ambos casos.
- Cualquier modelo tras una misma interfaz: LLMs (llama.cpp), ASR (whisper),
  OCR (ONNX Runtime), TTS (voces nativas), embeddings — locales o vía API,
  intercambiables sin tocar el grafo.
- Admisión de recursos: un presupuesto de memoria que el nodo hace cumplir,
  rechazando lo que no cabe con una explicación en lugar de un crash.
- Configuración de skills en tiempo de ejecución: cualquier skill puede
  declarar parámetros ajustables (temperatura, un timeout, una ruta de
  almacenamiento, ...) en su manifiesto; los valores efectivos se resuelven
  desde el default declarado → un archivo `--config` opcional → un override
  en vivo puesto desde la UI del plano de control o la API HTTP, aplicado en
  caliente sobre la propia conexión del skill.
- Memoria persistente y consciente del presupuesto:
  `skills/memory-context` — un skill tipo `memory` que sobrevive reinicios y
  recorta a un presupuesto de tokens configurable, a diferencia del
  historial en proceso y sin límite de llm-chat.

**Integración e interoperabilidad**
- Proyecciones legacy-first: convierte una especificación OpenAPI en skills
  usables por agentes en minutos, de solo lectura por defecto, con dry-run y
  promoción por operación.
- Fronteras estándar: un servidor MCP (cada skill es una tool para Claude Code,
  Cursor, etc.), una tarjeta de descubrimiento A2A y exportación de trazas
  OpenTelemetry — todo integrado.
- Skills con inversión de control que llaman *hacia fuera* al kernel, de modo
  que funcionan tras NAT y firewalls corporativos sin puertos de entrada.

**Autonomía y seguridad**
- Operación en lenguaje natural: un objetivo se convierte en un plan, un grafo y
  una ejecución con puertas de aprobación, donde el LLM elige y el código
  compila.
- Humano en el bucle por invariante del kernel: toda arista hacia un skill de
  acción (`motor.*`) lleva una puerta de aprobación humana, aplicada por el
  propio ejecutor — así que vale igual para grafos escritos a mano, para la
  salida del planner y para cualquier editor de grafos futuro. La regla de
  seguridad vive en el kernel, no en un prompt ni en cada herramienta que
  genere un grafo.
- Seguridad progresiva, en parte: la firma de paquetes es real y siempre está
  activa — cada publicación se firma (Ed25519) y cada instalación verifica hash
  y firma. La autenticación es real: loopback por defecto, token bearer,
  allowlist de origen y TLS opcional. La autorización también: una política de
  nodo decide qué puede actuar sobre el mundo, y un grafo no puede eximirse.
  Lo que sigue faltando es el resto de la escalera — el modo `site` hoy no
  cambia nada, y el aislamiento de procesos para skills no existe: un skill
  corre con los privilegios de quien lo arrancó, y `permissions` es una
  declaración que el registry enseña al instalar, no un sandbox que el kernel
  fuerce en tiempo de ejecución. [Modelo de
  seguridad](#modelo-de-seguridad) traza la línea con precisión.

**Distribución y operación**
- Un marketplace federable y firmado: publica con un comando, instala por nombre
  o por capacidad, con versiones inmutables y trust-on-first-use.
- Federación de nodos: un comando amplía un nodo para resolver capacidades que
  viven físicamente en otra máquina, con las respuestas fluyendo de vuelta con
  la causalidad intacta.
- Explicar y reproducir: `aura why` narra la causa raíz de un fallo a partir del
  log causal; `aura replay` convierte tráfico real grabado en una suite de
  evaluación.
- UI del plano de control internacionalizada (inglés + español; más idiomas
  añadiendo un archivo), embebida en el binario.

> El marketplace, la federación, el servidor MCP y el host de proyecciones
> OpenAPI funcionan pero **no tienen ningún test automatizado** — ver
> [cobertura de tests](#cobertura-parcial). Todo lo de esta lista existe y se
> ejecutó a mano; esa tabla dice qué partes detectarían una regresión.

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
- **Inteligencia documental.** Un skill de OCR lee facturas/documentos de
  identidad escaneados; un LLM extrae campos estructurados; una API proyectada
  archiva el resultado. Los documentos sensibles pueden fijarse a nodos
  on-premise para que sus datos nunca salgan del edificio.
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
| `aura status [--port 9080]` | Salud de un nodo en ejecución más sus skills conectados. |
| `aura verify [--data <dir>]` | Recalcula la cadena de hashes del ledger de efectos y comprueba cada firma de checkpoint — sin conexión, sin necesitar un kernel corriendo. Sale con código distinto de cero si algo no verifica. Ver [Modelo de seguridad](#modelo-de-seguridad). |
| `aura version` | Versión y los majors de protocolo/IR que habla este binario. |

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
[`skills/echo/`](skills/echo/) es el mínimo, [`skills/tuya-status/`](skills/tuya-status/)
un envoltorio de API de solo lectura, y [`skills/tuya-command/`](skills/tuya-command/)
uno que actúa sobre el mundo.

---

## La UI del plano de control

El frontend ([`ui/`](ui/), React 19 + Vite + TypeScript, internacionalizado con
react-i18next — base en inglés, español incluido) está **integrado en el
binario** vía `go:embed`. `aura up` lo sirve en `http://localhost:9080` sin
proceso Node en runtime. Siete vistas:

- **Chat** — elige un grafo, transmite una conversación y responde a las puertas
  de aprobación humana en línea con botones Aprobar/Denegar.
- **Voz** — habla con el grafo `voice`. Tus palabras aparecen mientras las
  dices, la respuesta se habla mientras se escribe, y hablarle encima la para.
- **Operate** — el flujo de `aura do` con UI: escribe un objetivo, observa el
  razonamiento y los pasos del planner, y luego la ejecución con puertas y el
  resultado.
- **Skills** — el catálogo vivo de capacidades, con colores por tipo, con los
  puertos, esquemas y descripción de cada skill.
- **Projections** — conecta una especificación OpenAPI pegándola, y cambia cada
  operación entre `disabled` / `dry-run` / `live`.
- **Graphs** — los grafos registrados y su IR.
- **Sessions** — todas las sesiones con insignias de error; haz clic en una para
  inspeccionar su log causal de eventos en línea (la materia prima detrás de
  `aura why`).

Para desarrollar la UI contra un kernel en ejecución:

```powershell
cd ui
npm install
npm run dev      # servidor de desarrollo Vite en :3000, con proxy de API + WS al kernel
npm run build    # emite en kernel/internal/gateway/ui/dist (re-embeber: recompila el kernel)
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

Ejecútalo contra un nodo con
`AURA_WS_URL=ws://localhost:9080/ws/skill python main.py`, o instálalo y
ejecútalo desde el registro con `aura run acme/logical/uppercase`.

**Puntos clave de diseño.** Los skills se conectan *hacia fuera* al kernel
(inversión de control), así que atraviesan NAT y firewalls corporativos sin
puertos de entrada. El `Context` te da `emit` (una respuesta enlazada
causalmente), `status`, `error` y `done`; cada mensaje que envía queda
automáticamente encadenado al que lo causó y recibe una clave de idempotencia.
Los handlers deben ser idempotentes — la entrega es at-least-once por contrato.

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
transformación síncrona `logical`/`motor` donde querés que
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

[`skills/connector`](skills/connector/) lee unas pocas líneas de YAML en su
lugar, y registra cada operación como un skill ordinario:

```yaml
name: legacy-erp
base_url: http://erp.internal
headers:
  Authorization: "Bearer ${ERP_TOKEN}"      # del entorno, nunca del archivo
operations:
  - { op_id: get-order,    method: GET,  path: /orders/{id}, params: [{name: id, in: path}] }
  - { op_id: create-order, method: POST, path: /orders }
```

```powershell
$env:PYTHONPATH = "sdk\python\src"
python skills\connector\main.py mi-conector.yaml
#  connector 'legacy-erp' -> 2 operation(s) against http://erp.internal
#    sensorial.api.legacy_erp.get_order      mode=live
#    motor.api.legacy_erp.create_order       mode=disabled
```

Las mismas cuatro salvaguardas, las mismas capacidades, la misma puerta. La
diferencia está en dónde corre: esto es un **skill**, no código del kernel. Los
conectores se instalan desde el registro como cualquier otra cosa, así que
agregar uno nunca engorda el binario que va a un dispositivo de borde ni espera
a un release del kernel.

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
    PYTHONPATH=sdk/python/src python skills/postgres-cdc/main.py
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

`aura up` siembra un grafo `voice`, y la UI del plano de control tiene una vista
**Voz** que lo maneja. Arranca los cuatro skills que resuelve, pulsa empezar y
habla:

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

## Drivers de modelos y admisión de recursos

La voz y la visión llegan como skills de primera parte — cada uno un driver fino
sobre un runtime nativo, cada uno con una cadena de degradación declarada:

| Skill | Capacidad | Motor |
|---|---|---|
| [`skills/llm-chat`](skills/llm-chat/) | `cognitive.llm.chat` | llama.cpp (cualquier GGUF) por defecto; cualquier API compatible con OpenAI (OpenAI, Gemini, …) si `OPENAI_API_KEY` está definida. Historial por sesión. |
| [`skills/asr`](skills/asr/) | `sensorial.asr.transcribe` | faster-whisper (CPU int8). Acepta un WAV completo en `audio_in`, o un stream PCM en vivo en `audio_chunk_in` con transcripciones parciales mientras la persona todavía habla. Termina una utterance por señal del cliente, por silencio final, o por un tope duro. |
| [`skills/tts`](skills/tts/) | `motor.tts.speak` | piper (opcional) → voces del SO (SAPI/espeak). Emite PCM en trozos de ~200ms para un oyente en vivo, sea cual sea el backend, más la cláusula entera como WAV. |
| [`skills/sentence-chunker`](skills/sentence-chunker/) | `logical.text.sentence_chunk` | Agrupa un stream de tokens en cláusulas. Ponlo entre un LLM en streaming y cualquier cosa que trabaje con frases, o el sintetizador se dispara una vez por token. |
| [`skills/ocr`](skills/ocr/) | `sensorial.ocr.image` | RapidOCR sobre ONNX Runtime |
| [`skills/model-manager`](skills/model-manager/) | `motor.models.manage` | listar / catálogo / búsqueda en HF / descargar / borrar |
| [`skills/memory-context`](skills/memory-context/) | `memory.context.window` | SQLite (embebido, archivo local) — persiste turnos por sesión a través de reinicios, `recall` devuelve una ventana recortada a un presupuesto de tokens configurable (drop-oldest, o resumen vía `aura.llm.ChatBackend`). El historial propio de llm-chat es en proceso y sin límite (ver su `main.py`); este es la alternativa durable y consciente del presupuesto — se cablea a un grafo explícitamente, no se conecta solo. |

Como todos comparten la interfaz de puertos/esquemas, a un grafo que pide
`sensorial.asr.transcribe` nunca le importa qué motor responde — ese es el plano
de modelos en la práctica.

**Skills de integración.** Estos son los ejemplos trabajados y legibles de
conectar con un sistema externo. El patrón detrás de ellos (mock primero,
`_recover_text()` para las rarezas del modelo pequeño) vale la pena copiarlo si
construyes el tuyo propio — léelo directo de los propios skills:

| Skill | Capacidad | Qué hace |
|---|---|---|
| [`skills/tuya-status`](skills/tuya-status/) | `sensorial.api.tuya.status` | Lee el estado de un dispositivo conectado a Tuya vía la Cloud API de Tuya (firma HMAC-SHA256 propia, sin dependencia de SDK). Solo lectura, siempre activo. |
| [`skills/tuya-command`](skills/tuya-command/) | `motor.api.tuya.command` | Enciende/apaga un dispositivo conectado a Tuya. Es `motor.*`, así que todo grafo generado por el planner o por MCP que llegue a él lleva una puerta de aprobación humana (ver [Modelo de seguridad](#modelo-de-seguridad) para el alcance exacto de esa regla). |
| [`skills/vision-reasoner`](skills/vision-reasoner/) | `cognitive.vision.reasoner` | Narra un evento de detección de objetos (p. ej. de Frigate) y propone una alerta gateada cuando parece accionable. Mismo backend local-o-nube que `llm-chat`. |
| [`skills/notify-alert`](skills/notify-alert/) | `motor.notify.alert` | Entrega una alerta ya aprobada — la escribe en disco, opcionalmente llama a un servicio de notificación de Home Assistant. |
| [`skills/vision-ask`](skills/vision-ask/) | `cognitive.vision.ask` | Preguntas y respuestas de visión bajo demanda contra un snapshot de cámara en vivo. Sin fallback local — necesita `OPENAI_API_KEY` apuntando a un modelo con capacidad de visión. |

Cada uno sigue necesitando aquello que envuelve para hacer algo útil de verdad:
un proyecto de Tuya IoT Platform, una cámara, una clave de API con capacidad de
visión. Si quieres algo que corra sin nada en absoluto, empieza por
[`skills/echo/`](skills/echo/) — sin modelo, sin credenciales, sin red.

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
se derivan del esquema de ingreso de cada skill. Los skills que actúan
(`motor.*`) están gateados: invocados por MCP se rechazan con un puntero a la
UI/CLI, porque un humano debe aprobar una acción — la política de seguridad
sobrevive a la frontera del protocolo.

**Tarjeta de descubrimiento A2A** en `/.well-known/agent.json`, construida desde
el catálogo vivo, para que otros agentes puedan descubrir este nodo y sus
skills.

**Exportación OpenTelemetry.** El log causal se convierte en trazas OTLP — un
span por envelope, con `parentSpanId` apuntando al padre causal — de modo que
cualquier backend OTel (Jaeger, Grafana Tempo, …) renderiza el árbol causal de
una sesión:

```powershell
.\kernel\aura.exe trace sess-… --otlp http://localhost:4318   # a un collector
.\kernel\aura.exe trace sess-… --out trace.json               # o a un archivo
```

---

## Los cuatro contratos

Todo lo anterior descansa sobre cuatro contratos pequeños, formalmente
especificados y con versión congelada, en [`spec/`](spec/). Son lo *único* que
AURA inventa; una suite de conformidad
([`spec/conformance/`](spec/conformance/), 59 comprobaciones, caja negra sobre
el protocolo crudo) es cómo una implementación demuestra que cumple C1-C3 — las
garantías propias de C4 las comprueba, por separado, un job de CI adversarial
(ver [Modelo de seguridad](#modelo-de-seguridad)).

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
  QoS declarable, transporte negociado, medición, y un recibo de efecto
  opcional.
- **C4 — Ledger de Efectos y Política** ([spec/c4-ledger.md](spec/c4-ledger.md)):
  el Effect Checkpoint por el que pasa toda entrega `motor.*` — autorizar por
  política de nodo, atestiguar en una entrada encadenada por hash, firmar
  periódicamente la cabeza de la cadena — y la verificación sin conexión que
  una implementación conforme debe soportar.

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
> beta en [`ROADMAP.md`](ROADMAP.md).

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

Tres cosas hacen que esto sea evidencia y no logging:

- **Encadenado por hash.** Cada entrada cita el hash de la anterior (`prev`),
  calculado sobre el contenido propio de la entrada. Alterar el contenido de
  una entrada vieja cambia su hash, lo que rompe todas las entradas selladas
  después — la cadena o recalcula limpia de punta a punta, o visiblemente no.
- **Firmado periódicamente.** Cada ~100 entradas o 60 segundos, lo que ocurra
  primero, el nodo firma su cabeza actual con su propia clave Ed25519
  (`identity.Node.Keys`, generada al primer arranque como cualquier otro
  keypair de este runtime). Esto es lo que una cadena de hashes por sí sola no
  puede dar: un atacante con acceso directo a la base de datos podría editar
  una entrada vieja *y* reparar cada puntero `prev` posterior, dejando una
  cadena que sigue recalculando limpia — pero no puede forjar una firma sobre
  la nueva cabeza sin la clave privada del nodo.
- **Verificable sin conexión.** `aura verify [--data <dir>]` recalcula toda la
  cadena y comprueba cada firma de checkpoint directamente contra el archivo
  SQLite — sin ningún proceso del kernel involucrado, y sin tocar nunca la
  clave privada, solo la pública. `GET /v1/ledger/verify` corre la misma
  comprobación contra un nodo corriendo, por conveniencia; las dos nunca
  pueden discrepar en silencio, porque es la misma función. Manipular la
  cadena de cualquiera de las dos formas —editar una entrada, o editarla y
  reparar la cadena después— hace que ambas reporten fallo; esto lo ejercita
  un job de CI que sella un efecto real a través de un nodo corriendo, edita
  `kernel.db` directamente, y comprueba que ambos caminos lo detectan.

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
| `GET /v1/ledger?from_seq=&limit=` | Entradas paginadas del ledger de efectos ([C4](spec/c4-ledger.md)). |
| `GET /v1/ledger/verify` | Recalcula la cadena y comprueba cada firma de checkpoint; `200` si es sólida, `409` si no. |
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
README.md           La guía práctica, con instalación y primer arranque (en inglés).
README-ES.md        Esta guía, en español.
ROADMAP.md          Qué se construye a continuación, y por qué.
CONTRIBUTING.md     Cómo contribuir al kernel, SDK y spec (en inglés) —
                    para agregar un skill, copia skills/echo/ (el ejemplo
                    mínimo trabajado) o cualquier otro bajo skills/.
LICENSE.md          Mapa de licencias por componente, licenciamiento comercial.
LICENSE-APACHE-2.0.txt   Texto completo de Apache-2.0 (spec, SDK, docs).
LICENSE-AGPL-3.0.txt     Texto completo de AGPLv3 (kernel, UI, skills de primera parte).
spec/               Los cuatro contratos congelados + JSON Schemas + suite de conformidad.
  c1-manifest.md      C1 — manifiesto de skill.
  c2-graph-ir.md      C2 — la IR única de grafo.
  c3-channel.md       C3 — el protocolo de channel.
  c4-ledger.md        C4 — ledger de efectos y política.
  enums.yaml          Fuente única de verdad de cada enum que usan C1-C4;
                      genera las constantes Go/Python/TypeScript (scripts/gen_ssot.py).
  schemas/            JSON Schemas ejecutables para C1-C3.
  conformance/        Suite de caja negra (runner.py) + vectores de test reutilizables.
kernel/             El kernel en Go — el binario único `aura`.
  cmd/aura/           Comandos CLI (up, chat, do, connect, publish, why, federate,
                      verify, …).
  internal/           identity · channel · registry · executor · store · gateway
                      · ledger (el ledger de efectos, C4) · projection · signing
                      · hub (registro) · mcpsrv · fed (federación) · spec
                      (constantes generadas) · config (la capa de archivo
                      --config, ver "Configuración de skills en tiempo de ejecución").
ui/                 Frontend del plano de control (React + Vite + TypeScript, i18n).
sdk/python/         El SDK `aura` para escribir skills (incluye aura.llm.ChatBackend).
sdk/node/           @deepaxiom/aura — expone las funciones de una app como
                    skills, o maneja un grafo desde TypeScript. Tipos
                    generados desde spec/schemas.
skills/             Skills de primera parte — los únicos que este repo considera
                    "publicados": echo (el ejemplo mínimo trabajado), connector
                    (integraciones declarativas), llm-chat, planner, asr, tts,
                    ocr, sentence-chunker, model-manager, memory-context, y los skills de
                    integración: tuya-status, tuya-command,
                    vision-reasoner, notify-alert, vision-ask.
scripts/            release.ps1 — construye un zip distribuible.
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

**Esto es pre-1.0.** Ver [`ROADMAP.md`](ROADMAP.md) para qué está construido y
qué no — los cimientos, la conectividad, la voz multicanal en streaming, la
seguridad del nodo y el ledger de efectos están todos, cliente de navegador
incluido. Los doce hitos de abajo corren de extremo a extremo; la suite de
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
| Spec + conformidad | Contratos congelados — C1 v1.5, C2 v1.1, C3 v1.4, C4 v1.1; suite de caja negra de 59 comprobaciones | Con tests |
| Voz multicanal en streaming | Grafo `voice` + cliente de navegador: transcripciones parciales, respuestas habladas, barge-in funcionando | Kernel con tests; cliente verificado a mano |
| Conectividad sin OpenAPI | Conector declarativo, SDK TypeScript, ingreso de webhooks, observación de tráfico | Ingreso y generador con tests; conector a mano |
| Drivers de modelos + admisión | Skills de ASR / TTS / OCR; `--memory-budget` | Admisión y ASR/chunker con tests; drivers a mano |
| Planner + `aura do` | Objetivo en lenguaje natural → plan → ejecución con puertas | Verificado a mano |
| Explicabilidad + replay | `aura why` (narrado por LLM), `aura replay` | Verificado a mano |
| Proyección OpenAPI legacy | `aura connect` → operaciones como skills, cuatro salvaguardas | Con tests (`projection` 61%) |
| Registro federable | `aura publish/add/run`, Ed25519 + trust-on-first-use | Con tests (`signing` 90%, `hub` 81%) |
| Fronteras estándar | Servidor MCP, tarjeta A2A, exportación OpenTelemetry | MCP con tests (63%); OTel a mano |
| Federación de nodos | `aura federate`, resolución entre nodos | Con tests (`fed` 87%) |
| Transporte negociado, LAN/mismo-host (Fase 3) | Una conexión pooled por capability federada en vez de un dial por envelope; cancel por envelope, no por cierre de socket; clasificación de ruta medida (`aura federate` la imprime) | Con tests — reuso de conexión, demux de relays concurrentes, cancel-no-afecta-a-otro, auto-recuperación tras una caída |
| Ledger de efectos (C4) | Atestación encadenada por hash y firmada de cada efecto; `aura verify`, `GET /v1/ledger[/verify]` | Con tests (`ledger` 89%); detección de manipulación probada por un job de CI con un binario real |
| Reversibilidad (`aura undo`, Fase 2) | Deshace un efecto vía su puerto `compensates` declarado — a su vez un efecto gateado y sellado; se rechaza antes de construir la sesión si ya fue deshecho, nunca se entregó, o es irreversible | Con tests (adversariales: doble undo, sin compensación, gate denegado) |
| Resume de sesión (Fase 2) | La ventana de dedup, los índices causal/en-vuelo, los gates pendientes y los contadores `Seq` por hop de una sesión se reconstruyen desde el log causal al reconectar — da igual si el cliente se cayó o si el propio proceso del kernel reinició, ambos casos toman el mismo camino | Con tests (adversariales: un gate pendiente sobrevive, un cancel sigue alcanzando una cadena multi-hop en vuelo, `Seq` continúa en vez de reiniciarse) |
| Replay determinista (Fase 2) | `aura replay` compara las entradas selladas del ledger de la sesión original y la reproducida, no solo la transcripción visible al cliente — cierra la cuarta propiedad de la tesis (Reproducible) | Con tests (`ledger.Diff`, puro, 8 casos tabulares) |
| Skills Wasm, sandbox real (Fase 3) | `format: wasm` sobre wazero — `filesystem` (preopen WASI) y `egress_http` (host import `env.http_fetch` propio, allowlist de hostname exacto) ambos aplicados, no declaraciones impresas; alojado dentro del proceso, nunca un OS process aparte | Probado contra guests reales compilados `GOOS=wasip1`, no simulado (`wasmrt` + `gateway` de punta a punta; scoping de permisos probado bajo concurrencia) |
| CDC de Postgres (Fase 3) | `skills/postgres-cdc` — replicación lógica (`test_decoding`, sin instalar extensión) se convierte en eventos causales `std/db-change@1`, uno por fila cambiada | Probado contra un `postgres:16` real de Docker, no simulado — INSERT/UPDATE/DELETE, NULL, filtro de tablas |

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

**Todavía no cierto:**
- **No hay resume de conexión.** Reconectar un cliente con el mismo id de
  sesión arranca una sesión en memoria completamente nueva; las puertas de
  aprobación humana pendientes, el tracking de cancelación en vuelo, y la
  ventana de dedup no sobreviven a un socket caído. No existe un handshake
  de Resume. Este es el hueco más grande: sin él, "persistente" se refiere al
  log, no a la sesión.
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
  | `config` | 100,0% | | `signing` | 90,4% |
  | `channel` | 97,5% | | `registry` | 92,6% |
  | `ledger` | 89,2% | | `fed` | 87,1% |
  | `executor` | 88,0% | | `store` | 81,7% |
  | `hub` | 81,2% | | `identity` | 78,3% |
  | `gateway` | 68,5% | | `mcpsrv` | 62,7% |
  | `projection` | 61,0% | | `cmd/aura` | **13,3%** |

  **Ningún paquete está en cero.** `internal/` está en **77,4%**; el agregado
  del módulo entero (`go test ./...`, todos los paquetes) es **55,6%**, y la
  diferencia es enteramente `cmd/aura`: unas 2.700 líneas de CLI al 13,3%, que
  se prueban levantando nodos de verdad y no con tests unitarios — la lógica
  central de `aura verify` es la excepción, probada directamente (ver [Modelo
  de seguridad](#modelo-de-seguridad)). La UI sigue sin suite de tests. La
  suite de conformidad ejercita el kernel de punta a punta sobre el cable; la
  garantía de detección de manipulación del ledger de efectos se ejercita por
  separado, contra un binario real, con un job de CI adversarial dedicado.
  Trátalo como pre-producción.

---

## Diseñado, aún no construido

Alcance honesto. Todas estas piezas descansan sobre los mismos contratos
congelados — ninguna cambia C1, C2, C3 ni C4 — pero aún no están implementadas.
Las dos primeras son las que impiden que esto sea seguro fuera de una red de
confianza:

- ~~**Autenticación del nodo y loopback por defecto**~~ — hecho en la Fase 0
  (ver [ROADMAP.md](ROADMAP.md)). El texto anterior decía: hoy `aura up` escucha en
  todas las interfaces sin token, sin TLS y sin comprobación de origen ([Modelo
  de seguridad](#modelo-de-seguridad)). Atar loopback por defecto con una
  renuncia explícita, restringir la comprobación de origen del WebSocket y un
  token bearer para la superficie HTTP y WS son el cambio más pequeño que hace
  seguro correr un nodo en una red compartida. Esto va primero, antes que todo
  lo demás.
- **Lo que los modos `site` y `published` deberían hacer** — identidad
  automática del nodo, canales cifrados entre nodos y permisos evaluados en
  tiempo de ejecución. Hoy la cadena del modo cambia exactamente un
  comportamiento (rechazar una arista `motor.*` sin puerta en `published`); el
  resto del diseño de seguridad progresiva no está implementado, y la tabla del
  [Modelo de seguridad](#modelo-de-seguridad) así lo dice.
- **Resume de sesión** — reconstruir las puertas pendientes, el tracking en
  vuelo y la ventana de dedup de una sesión caída, en vez de arrancar una en
  blanco. Este es el hueco más grande entre dónde está el runtime hoy y la
  persistencia que declara; [`ROADMAP.md`](ROADMAP.md) lo agenda junto con la
  compensación, porque las dos son el mismo problema de reconstruir estado
  desde el log.
- **`aura undo`** — un skill ya puede declarar cómo se revierte uno de sus
  efectos (`compensates`, C1) y el ledger ya guarda esa metadata en cada
  entrada sellada (C4), pero nada recorre la cadena hacia atrás y la invoca
  todavía. Delimitado deliberadamente al runtime, no al contrato de cable —
  ver [c4-ledger.md](spec/c4-ledger.md#what-this-contract-deliberately-does-not-specify).
- **Un editor visual de grafos** — hoy un grafo se escribe como IR C2 crudo en
  JSON o lo genera el planner; la vista de Grafos de la UI del plano de
  control es un visor de JSON de solo lectura, no un canvas de arrastrar y
  soltar.

- **Librería embebible (`libaura`)** — el mismo cliente de channels como
  librería enlazable en C/Rust con bindings Kotlin/Swift/JS, para que una app de
  TV/móvil/reloj capture y reproduzca streams y ejecute lógica ligera sin un
  nodo completo. Es un esfuerzo de empaquetado; el protocolo ya es el contrato.
- **Skills Wasm** — un ejecutor `format: wasm` (wazero) para lógica con sandbox
  duro corriendo in-process. El manifiesto ya transporta `format`; la paridad
  source/Wasm es un trade-off asumido.
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
ejercita levantando nodos reales en su lugar), no hay resume de sesión,
`aura undo` todavía no existe, y el catálogo de integraciones es pequeño.
[Estado de los hitos](#estado-de-los-hitos) es el resumen exacto; si ese
apartado y este documento se contradicen alguna vez, el que tiene razón es
Estado de los hitos.*
