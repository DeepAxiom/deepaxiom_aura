# Roadmap

*[English version below](#roadmap-english) · Estado a 2026-08-01 · v0.3.0 → v0.5.0 (Beta abierta)*

> **Fases 0 y 1 completadas.** Los tres agujeros que contradecían lo que el
> runtime promete están cerrados, y la cuña —el ledger de efectos— está
> construida, probada y verificable sin conexión. Lo que sigue es la Fase 2 —
> reversibilidad y resume de sesión. El detalle está más abajo, en "Fase 0 ·
> Seguridad y verdad" y "Fase 1 · El ledger de efectos".

Este documento es el **estado** del runtime y su dirección, no su historia: qué
está construido, qué contradice hoy lo que el runtime promete, y en qué orden
tiene que resolverse. Nada de lo de abajo se apoya en el historial de git — se
apoya en el código, y el [estado de los hitos](README-ES.md#estado-de-los-hitos)
del README dice qué parte de ese código está cubierta por tests y qué parte solo
se verificó a mano.

**Este roadmap cambia de dirección respecto al anterior**, y el porqué merece
leerse antes que las fases. Los tres bloques ya construidos —cimientos,
conectividad y voz— funcionan de extremo a extremo, pero dejan el proyecto
compitiendo en cinco categorías donde tres competidores son más maduros. La
arquitectura no resuelve eso; una cuña sí. La que sigue no se inventa desde
cero: es lo que este runtime ya casi es.

---

## La tesis

> **Cada efecto sobre el mundo pasa por un único checkpoint del kernel que lo
> autoriza por política, lo sella en un ledger encadenado y firmado, y sabe cómo
> revertirlo.**

La unidad de trabajo no es el mensaje ni la ejecución: es **el efecto**.

Cuatro propiedades, y ningún runtime del mercado tiene las cuatro:

| | Qué significa | Quién más lo tiene |
|---|---|---|
| **Autorizado** | Una política de nodo decide si un efecto ocurre — no el grafo que lo pide | n8n y LangGraph lo dejan en userland |
| **Atestiguado** | Cada efecto en un ledger encadenado y firmado, verificable sin el nodo | Nadie; todos lo tratan como logging |
| **Reversible** | La compensación se declara en el manifiesto; el kernel conoce el orden inverso | Temporal (saga), sin política ni atestación |
| **Reproducible** | Replay determinista contra el ledger como oráculo | Temporal, con clúster |

**El 60% ya está construido:** el log causal, el gate como invariante del
kernel, Ed25519 con JSON canónico, el replay y los contratos tipados. No es un
rewrite — es completar lo que la forma ya implicaba.

Y hay una razón estructural para que esto sea defendible: las arquitecturas
competidoras son *basadas en ejecuciones* y *basadas en clúster*. Ésta es basada
en envelopes y en binario único. Aquí el coste por efecto es un sha256 y un
INSERT, lo bastante barato para una Raspberry Pi. Ahí no lo es.

---

## Lo que ya está construido ✅

Tres bloques, en el orden en que había que construirlos. **El orden no era
negociable**: el barge-in es imposible sin cancelación transitiva, y el QoS
`realtime` que la voz necesita elimina el único detector de conexiones muertas
que había. No era un calendario, era un grafo de dependencias.

**Cimientos** — suite de tests en Go, Python y TypeScript donde no había
ninguna, más un CI que bloquea merges: formato, vet, build, detector de carreras,
bundle embebido de la UI, conformidad contra un binario recién compilado y
comprobador de enlaces. El gate `motor.*` pasó de estar duplicado en userland a
ser un invariante que aplica el executor. Liveness en los dos sockets de larga
vida. Cancel transitivo con supresión del lado del kernel. Spec resincronizada
con lo que el kernel emite de verdad.

**Conectividad** — cuatro caminos para los cuatro casos reales: un sistema que no
puedes cambiar ([`skills/connector`](skills/connector/)), una app que es tuya
([`sdk/node`](sdk/node/)), eventos que entran (ingreso HTTP con HMAC) y un
sistema que prefieres no describir (`aura observe` + `aura generate connector`).
Los conectores son skills, no `kind`s compilados en el kernel.

**Voz streaming multicanal** — schemas `std` ejecutables, QoS `realtime` con
elisión del log, ASR en streaming, chunker de frases, TTS en chunks, preempción,
y cliente de navegador con AudioWorklet, VAD con preroll y barge-in funcionando.
Primer sonido a 0,41 s; una pregunta nueva desplaza a la anterior en ~0,2 s
(medidas a mano en una máquina de desarrollo, no benchmarks).

Que funcione de extremo a extremo no significa que esté terminado. Sigue siendo
pre-1.0, y lo que viene ahora explica exactamente por qué.

---

## Fase 0 · Seguridad y verdad ✅ *(completada)*

**Era el bloqueante de la Beta abierta.** Tres agujeros que no eran deuda
técnica sino contradicciones con lo que el runtime dice garantizar. Los tres
están cerrados; abajo queda escrito qué eran, porque un agujero cerrado sin
memoria vuelve.

Por qué esto iba antes incluso que el ledger: **una atestación emitida por un
nodo sin autenticación no atestigua nada.** Prueba que un efecto ocurrió, no que
lo autorizó alguien con derecho a autorizarlo. El valor del ledger era
exactamente cero hasta que el nodo supiera quién le habla.

### Agujero 1 — el invariante no resistía a quien escribe el grafo

Un nodo no tenía autenticación de ningún tipo y `aura up` escuchaba en todas las
interfaces; además un grafo podía eximirse del gate con `"gate": "none"`, y un
grafo es un JSON que cualquiera hacía POST. Sumado: la mejor garantía del
runtime no valía nada contra alguien en la misma red.

| Qué se hizo | Dónde |
|---|---|
| Bind a **loopback por defecto**; `--listen 0.0.0.0` es una renuncia explícita que imprime un aviso | `gateway/auth.go`, `cmd/aura/main.go` |
| **Token bearer** autogenerado al primer arranque, 0600, impreso una vez; exigido en `/v1/*`, `/ws/*`, `/mcp` y la A2A card | `gateway/auth.go` |
| **`CheckOrigin` con allowlist** real en lugar de `return true` — el token no basta, porque un navegador adjunta credenciales a un handshake cross-site por su cuenta | `gateway/auth.go` |
| **TLS** vía `--tls-cert` / `--tls-key`, con aviso si se expone sin él | `cmd/aura/main.go` |
| **Motor de política** — `aura.policy.yaml`, cargado al arrancar y hasheado | `executor/policy.go` |

**La política es lo que cierra el agujero de verdad.** `gate: "none"` pasó de ser
la última palabra a ser una *petición*, honrada solo si la política del nodo la
concede. La regla en una línea: **un grafo puede ser más estricto que la
política, nunca más laxo.** Deny-by-default para `motor.*`, primera regla que
casa gana, sin lenguaje de reglas — una política que un auditor no puede leer no
es una política.

Tres decisiones que merecen quedar escritas:

- **La política por defecto es un documento real**, no un camino implícito en el
  código: tiene hash, se imprime al arrancar y reproduce exactamente el
  comportamiento anterior, así que `aura up` sin `--policy` sigue corriendo sus
  propios grafos sembrados. Un default implícito es uno que nadie puede auditar.
- **En un fichero de política, el permiso de exención es `false` si no se dice
  nada.** Un nodo al que le entregan una política es un nodo que quiere ser la
  autoridad; que el campo ausente concediera la exención habría reabierto el
  agujero para todo el que escribiera una política sin conocerlo.
- **En `published` el grafo nunca vota**, diga lo que diga la política. Eso da
  por fin a la escalera de modos una segunda diferencia real, en lugar de la
  única que tenía.

### Agujero 2 — las garantías no cruzaban una federación

`fed/bridge.go` descartaba los envelopes `cancel` y regeneraba `idem` con un id
nuevo. Es decir: **la cancelación transitiva y la idempotencia —las dos cosas
que el README vende como diferenciales— dejaban de cumplirse en cuanto federabas
un nodo.** Un agujero en la historia, no solo en el código.

- El `cancel` se reenvía ahora en ambos sentidos, indexado por el `cause_id` que
  el proxy conoce; cancelar cierra el socket remoto, así que para el nodo remoto
  también para el trabajo.
- `idem` se **deriva** del envelope entrante en lugar de inventarse, así que un
  reintento vuelve a ser deduplicable al otro lado.
- El grafo remoto se registra **una vez por sesión**, no por envelope, y su
  rechazo se reporta en lugar de descartarse.
- Cada puerto de egreso remoto conserva su identidad, en lugar de colapsar todos
  sobre `client.text_in`.
- El tipo del skill remoto se preserva, para que una federación no pueda
  blanquear un efecto y colarlo por delante del gate.

### Agujero 3 — el ingress era reproducible y sin tope

El HMAC firmaba solo el body, sin timestamp ni nonce, y el `idem` se generaba
fresco por entrega: quien capturara una entrega firmada válida podía
reproducirla indefinidamente, y cada réplica abría sesión y volvía a ejecutar el
grafo. Y cada entrega lanzaba una goroutine dormida dos minutos, sin tope — en
el único endpoint público por diseño.

- **Detección de replay persistente**: el digest de cada entrega se recuerda en
  SQLite dentro de una ventana. En disco y no en memoria, porque un replay que
  funciona tras un reinicio sigue siendo un replay.
- **Timestamp firmado opcional** (`timestamp_header` + tolerancia), al estilo
  Stripe: el timestamp entra *dentro* de la firma, así que una captura antigua
  no se revalida poniéndole una hora nueva.
- **`idem` derivado de la entrega**, lo que arregla el replay y la deduplicación
  aguas abajo de una sola vez.
- **Rate limit por ruta**, tope global de sesiones y **un solo reaper** en lugar
  de una goroutine por entrega.
- Un replay se contesta **200, no 4xx**: un emisor reintentando bajo
  at-least-once se está portando bien, y un error solo hace que reintente más.

### SSOT

| Duplicado antes | Fuente única ahora |
|---|---|
| La versión, en tres sitios con **dos valores distintos** (`0.1.0-h1` y `0.1.0`) | `spec/VERSION` → generada a kernel, agent card y SDKs |
| Los cinco tipos de skill, en regex Go, slice Go, tupla Python y JSON Schema | `spec/enums.yaml` → constantes generadas en Go, Python y TypeScript |
| Kinds de envelope, clases de QoS, gates, formatos | igual, desde el mismo fichero |

`scripts/gen_ssot.py` genera; `--check` falla en CI si algo quedó obsoleto. Los
**JSON Schemas se verifican en lugar de regenerarse**: C1 los llama normativos y
están escritos a mano, así que reescribirlos mecánicamente habría reformateado
el documento entero para decir lo mismo y habría escondido la única línea que
cambió de verdad. Verificar da la misma garantía sin quitarle la autoría al
fichero.

### Cobertura y multiplataforma

**Los siete paquetes que estaban en cero ya no lo están.** Los cinco portantes
sostenían cuatro de los once hitos:

| Paquete | Antes | Ahora | | Paquete | Antes | Ahora |
|---|---|---|---|---|---|---|
| `channel` | 97.5% | 97.5% | | `signing` | **0%** | 89.4% |
| `registry` | 88.6% | 88.6% | | `fed` | **0%** | 87.1% |
| `store` | 85.2% | 79.0% | | `hub` | **0%** | 81.2% |
| `executor` | 74.8% | 88.1% | | `identity` | **0%** | 81.2% |
| `gateway` | 60.1% | 68.0% | | `mcpsrv` | **0%** | 62.7% |
| `cmd/aura` | 12.1% | 11.2% | | `projection` | **0%** | 61.0% |
| | | | | `config` | **0%** | 100.0% |

`internal/` está en **76.0%**. El agregado del módulo entero es 51.8%, y la
diferencia es enteramente `cmd/aura`: 2.648 líneas de CLI al 11.2%, que se
prueban levantando nodos de verdad y no con tests unitarios. **El objetivo que
este documento se puso era ≥80% de kernel y no está alcanzado**; lo que sí está
es que ningún paquete sigue en cero y que los subsistemas de confianza —
`signing`, `policy`, `fed` — están por encima del 85%. Subir `cmd/aura` es
trabajo de la Fase 1.

Y **seis binarios cross-compilados** en CI (linux/darwin/windows × amd64/arm64),
16-17 MB cada uno, sin cgo. Raspberry Pi 5 y Jetson incluidos.

### Lo que CI comprueba ahora

Tres jobs nuevos, además de los que ya había:

- **`adversarial`** — arranca un binario real **con los flags por defecto** y
  comprueba que rechaza lo que debe: la superficie de control sin token, un
  token equivocado, un bind público, una credencial legible por todos, y un
  grafo cuya capacidad la política deniega. Probar el camino endurecido mientras
  se distribuye un default permisivo no probaría nada, así que lo que se prueba
  es el default (`scripts/adversarial.sh`).
- **`ssot`** — falla si algún artefacto generado divergió de `spec/`.
- **`cross-compile`** — los seis targets, en cada cambio.

---

## Fase 1 · El ledger de efectos ✅ *(completada)* — la cuña

**Al terminar esta fase se abre la Beta.** La Fase 0 dejó puesta la mitad del
andamiaje: la política que decide *si* un efecto ocurre ya existía y ya estaba
hasheada — exactamente el campo `policy` que cada entrada del ledger tiene que
citar. Lo que faltaba era escribir lo ocurrido en algún sitio que nadie pudiera
alterar sin que se notara. Ya está escrito.

Un primitivo nuevo en el kernel, y exactamente uno: **P5, el Effect Ledger**
([`kernel/internal/ledger`](kernel/internal/ledger/)). Sigue pasando el test de
pureza del proyecto — no importa un solo LLM, ni el marketplace, ni nada que no
sea hashes, firmas y SQLite.

**C4 — Effect Ledger & Policy**, congelado en
[spec/c4-ledger.md](spec/c4-ledger.md), junto a
[C1](spec/c1-manifest.md), [C2](spec/c2-graph-ir.md) y
[C3](spec/c3-channel.md). **C1, C2 y C3 no se tocaron** — la innovación
aterrizó como contrato nuevo más dos campos opcionales: `compensates` en C1
(qué puerto revierte un efecto) y `receipt` en C3 (el hash del sello, sobre el
envelope que lo llevó). Un skill escrito antes de esta fase sigue funcionando
sin cambiar una línea.

**El checkpoint** vive en `Session.forward()`
([executor/session.go](kernel/internal/executor/session.go)) y en la rama de
denegación de `Session.resolveGate()` — los dos puntos por los que ya pasaba
toda entrega y toda resolución de puerta. Ningún tercer punto de inserción hizo
falta, que es la señal de que la arquitectura de la Fase 0 ya estaba bien
puesta:

```
envelope → [ Effect Checkpoint ] → skill
             1. clasificar   ¿el destino es motor.* y el kind es data?
             2. leer         el gate ya lo resolvió la política al construir la sesión
             3. atestiguar   sella una entrada (con la compensación declarada, si existe) → recibo
             4. entregar     con el recibo dentro del envelope (C3, campo receipt)
```

Un matiz que solo se ve implementando: la *autorización* (paso "2" en el
diseño original) ya había ocurrido antes, en `applyPolicy` al construir la
sesión — el checkpoint en `forward()` no vuelve a decidir, solo lee lo ya
decidido. Y una denegación de puerta (`resolveGate` cuando el humano dice que
no) también se sella — `decision: gate, outcome: denied` — porque un auditor
que pregunta "qué intentó hacer esta sesión" necesita la propuesta rechazada,
no solo las entregadas.

**La entrada del ledger**, tal y como quedó (~350-400 bytes):

```json
{ "seq": 42, "prev": "sha256:9f2a…", "ts": 1754083200000, "node": "node-a1b2",
  "session": "sess-8f3a", "envelope": "01J9ZK…", "cause": "01J9ZK…",
  "actor": "acme/motor/erp-writer@1.2.0", "capability": "motor.erp.invoice.create",
  "decision": "gate", "outcome": "delivered", "policy": "sha256:7c1e…",
  "payload_sha256": "b5b3…", "compensation": { "port": "undo_in", "schema": "acme/erp-undo@1" } }
```

`outcome` es una adición sobre el boceto original: `decision` es lo que la
política resolvió (`allow` \| `gate`), `outcome` es lo que pasó después
(`delivered` \| `denied`) — un `gate` que un humano deniega es una entrada, no
dos, con la propuesta y su resolución juntas.

Las cuatro decisiones de diseño originales se sostuvieron sin cambios:
`payload_sha256` nunca el payload, `policy` el hash del documento, `actor` con
versión, `prev` encadenando. `Hash()` se computa con `json.Marshal` directo —
sin canonicalización de claves de mapa como la que sí necesita
`CanonicalManifestHash` para YAML de usuario — porque el orden de campos de un
struct Go es fijo en tiempo de compilación, ya determinista por construcción.

**Checkpoints firmados** — Ed25519 sobre la cabeza cada 100 entradas o 60
segundos, lo que ocurra antes (`checkpointEveryN`, `checkpointEveryT` en
[ledger.go](kernel/internal/ledger/ledger.go)). El truco de Certificate
Transparency: firmar una entrada cuesta lo mismo que firmar mil. La cadencia
sobrevive a un reinicio — el ledger relee el último checkpoint al abrir, en vez
de olvidar cuánto llevaba desde la última firma.

**Identidad de nodo** — [identity.go](kernel/internal/identity/identity.go)
gana un par Ed25519 en `<data-dir>/identity/`, reutilizando `LoadOrCreate` de
[signing](kernel/internal/signing/signing.go) — el mismo mecanismo que ya usa
`aura publish`, apuntado a un subdirectorio distinto a propósito: la clave que
firma paquetes que alguien publica y la clave que firma el ledger de un nodo en
ejecución son identidades distintas, y mezclarlas en el mismo directorio habría
sido un error fácil con mal desenlace.

**`aura verify [--data <dir>]`** — recalcula la cadena entera y comprueba cada
firma de checkpoint **sin el nodo corriendo**, y sin tocar nunca la clave
privada — solo lee la pública
([`signing.LoadPublicKey`](kernel/internal/signing/signing.go), que a
propósito nunca crea una clave como efecto colateral. Sale con código de salida
distinto de cero si algo no verifica, que es lo que un pipeline de CI necesita
para poder alertar:

```
aura verify
  aura verify — /home/user/.aura

    2 entries · chain intact · 1/1 checkpoint(s) valid · key 484c34f7f338

    SOUND — the chain recomputes cleanly and every checkpoint signature verifies.
```

`GET /v1/ledger` (paginado) y `GET /v1/ledger/verify` (`200` si es sólida,
`409` si no) exponen lo mismo sobre HTTP para un nodo en ejecución — la misma
función `ledger.Verify`, así que las dos vías nunca pueden discrepar en
silencio. `/healthz` incluye un resumen de una línea en cada probe.

**La prueba que importa** no es que el diseño suene bien — es que atacarlo de
verdad falla de verdad. Dos escenarios, probados en `ledger_test.go`:

1. **Manipular una entrada interior.** Cambiar el contenido de la entrada 3 de
   5 rompe la entrada 4, cuyo `prev` deja de coincidir con el hash recalculado
   de la 3. La cadena por sí sola lo atrapa.
2. **Manipular una entrada ya sellada por checkpoint, y reparar la cadena
   después.** Un atacante con acceso directo a SQLite no se limita a editar una
   fila — también reescribe cada `prev` posterior para que la cadena vuelva a
   recomputar limpia. Esto **no** lo atrapa el encadenado de hashes por
   construcción; lo atrapa la firma del checkpoint, porque el atacante no
   tiene la clave privada del nodo para firmar la nueva cabeza. Este es el
   caso que de verdad justifica que los checkpoints existan, y es el que
   demuestra que la cadena sola no basta.

Ambos escenarios se repiten contra un **binario real**, no solo en tests Go: un
job de CI ([`scripts/ledger_adversarial.py`](scripts/ledger_adversarial.py))
registra un skill motor de verdad, sella dos efectos a través del camino de
aprobación real, edita `kernel.db` directamente — sin pasar por ninguna API del
nodo — y comprueba que tanto `aura verify` como `GET /v1/ledger/verify` lo
reportan.

**Cobertura:** `ledger` 89.2%, con los dos escenarios adversariales arriba
incluidos, más restart, cadencia de checkpoints, y sellado concurrente
(50 goroutines sellando a la vez, cadena resultante verificada íntegra).
`executor` subió a 88.0% con la integración del checkpoint probada de punta a
punta: un efecto sin puerta se sella `allow`/`delivered`, uno aprobado se sella
`gate`/`delivered`, uno denegado se sella `gate`/`denied`, y el recibo en el
envelope entregado coincide exactamente con el hash de la entrada sellada.

---

## Fase 2 · Reversibilidad y resume ← *siguiente*

**~4 semanas. Post-beta, sobre una beta ya en manos de gente.**

Van juntas porque son el mismo problema —reconstruir estado desde el log— y
porque las dos se abaratan al existir la Fase 1. Parte del trabajo ya está
hecho: `compensates` (C1) y el campo `compensation` de cada entrada del ledger
(C4) llevan construidos desde la Fase 1 — un skill motor ya puede declarar su
reverso y el ledger ya lo registra en cada efecto sellado. Lo que falta es la
mitad activa.

**`aura undo <sesión|recibo>`.** Camina el ledger hacia atrás y acciona el
puerto de undo (`compensates.port`) de cada efecto autorizado, en orden causal
inverso. Cada undo **es a su vez un efecto**: pasa por política y se sella
— no hay un camino especial "esto es un undo" que se salte el checkpoint. Los
efectos sin compensación declarada se reportan como irreversibles, que ya es
información visible hoy en cualquier entrada del ledger sin campo
`compensation` — "esta sesión hizo cuatro cosas, tres se pueden deshacer, una
no" se puede leer ya con `aura verify` o `GET /v1/ledger`, aunque `aura undo`
en sí todavía no exista.

**Resume de sesión.** Reconectar reconstruyendo puertas pendientes, tracking en
vuelo y ventana de dedup. Es el hueco más grande frente a la apuesta de un
runtime persistente, y hasta que exista, "persistente" significa el log y no la
sesión. Se aplazó hasta aquí porque una sesión de voz puede reconectar
reiniciándose al coste de un turno — eso lo hizo aplazable, no descartable — y
porque la maquinaria de reconstrucción que necesita es la misma que la
compensación.

**Replay determinista** contra el ledger como oráculo, cerrando la cuarta
propiedad de la tesis.

---

## Fase 3 · Alcance

**Post-beta, en orden de valor.**

- **`libaura`** — el mismo cliente de canales como librería enlazable vía
  `-buildmode=c-shared`, con bindings Kotlin y Swift, más `GOOS=wasip1` para
  edge y navegador. Es empaquetado: el protocolo ya es el contrato.
- **Skills Wasm con sandbox real** — un executor `format: wasm` sobre wazero.
  Esto es lo que hace **verdad** la regla 3 de C1: hoy `permissions` es una
  declaración que el registro enseña al instalar, no un sandbox que el kernel
  aplique. Con wazero, `egress_http` y `filesystem` se aplican de verdad.
- **CDC / replicación lógica de Postgres** — sigue siendo la vía más
  diferenciadora frente a n8n para legacy sin API, y encaja limpiamente con la
  tesis: un cambio de fila se vuelve un evento causal y sellado.
- **Transporte P2P negociado** — la regla 5 de C3 ya lo contempla; hoy todo el
  plano de datos relaya por el kernel, que es el cuello de botella estructural.

---

## Cómo se abre la Beta

Base segura primero, cuña después: no se lanza la innovación sobre cimientos
inseguros, y un ledger firmado por un nodo que cualquiera puede conducir sería
peor que no tener ledger, porque daría una falsa sensación de prueba. Con las
Fases 0 y 1 completas, ese orden se cumplió.

Los criterios de salida son verificables, no opinables. Estado a hoy:

| | Criterio | Estado |
|---|---|---|
| 1 | El job `adversarial` pasa: un nodo por defecto rechaza la superficie de control sin token, un bind público, una credencial legible por todos y un grafo denegado | ✅ |
| 2 | El gate de SSOT pasa: ningún artefacto generado está obsoleto | ✅ |
| 3 | Los seis binarios compilan en cada cambio | ✅ |
| 4 | Ningún paquete del kernel en 0% de cobertura | ✅ |
| 5 | `go test ./... -race` verde (CI, con cgo) | ✅ |
| 6 | Cadena del ledger íntegra y `aura verify` funcionando sin el nodo | ✅ |
| 7 | Alterar una fila del ledger hace fallar `aura verify` — incluido el caso donde el atacante repara la cadena después de manipularla, que solo la firma del checkpoint atrapa | ✅ probado con Go tests y con un job de CI contra un binario real |
| 8 | Cobertura de kernel ≥80% | ⚠️ `internal/` 77.4%; el módulo entero 55.6% por `cmd/aura` (los subsistemas de confianza — `ledger` 89%, `signing` 90%, `registry` 93%, `executor` 88% — superan el objetivo) |
| 9 | El flujo completo corre en un binario arm64 sobre una Raspberry Pi | pendiente de hardware |
| 10 | `SECURITY.md` con política de divulgación, releases firmados y SBOM | pendiente |

**8 de 10 criterios cumplidos.** Los dos que faltan no son de código: uno
necesita hardware físico para probarse (el binario ya compila para arm64, ver
`cross-compile` en CI), y el otro es papeleo de proceso de release
(`SECURITY.md`, firma de artefactos, SBOM) que no depende de ninguna decisión
de arquitectura pendiente. Ninguno de los dos bloquea que el runtime *funcione*
como promete; ambos son honestos de dejar fuera de "listo" hasta que estén
hechos, y por eso siguen marcados como pendientes en vez de darse por buenos.

---

## Aplazado, y por qué

- **Multidispositivo** (varios clientes observando una sesión viva) — la voz no
  lo necesita; solo se dejó preparado el terreno.
- **QoS `bulk`** — nombrado en el enum y especificado en ninguna parte. Necesita
  una decisión de producto, no de código. Se trata como `reliable` para que nada
  se pierda en silencio contra una regla sin definir.
- **Frames binarios de WebSocket** — ahorra ~26 KB/s y cuesta un RFC de
  transporte más seis subsistemas. El campo `ref` de `std/audio-chunk@1` está
  reservado para que esa decisión siga siendo de payload y no de envelope.
- **Editor visual de grafos** — con el gate como invariante del kernel y la
  autorización en política de nodo, deja de ser un riesgo de seguridad y pasa a
  ser trabajo puro de UI.
- **Periféricos MQTT/BLE, nube gestionada** — post-1.0, como siempre. La nube es
  deliberadamente lo último: es solo otro nodo, y el runtime abierto tiene que
  estar completo antes.

---

## Horizonte · La red (post-Fase 3, sin fecha)

**Esto no es la beta.** Todo lo de arriba (Fases 0-3) es lo que hace que `aura
up` sea seguro de correr hoy. Lo que sigue es otra pregunta: qué necesita
AURA para dejar de ser *lo que un operador instala* y pasar a ser *lo que
varias organizaciones que no se conocen usan para probarse cosas entre sí*.
Ninguna fase de acá abajo tiene fecha ni compromiso — tiene orden y criterio
de salida, igual que las anteriores, porque eso es lo único que este
documento promete alguna vez.

Renumera desde Fase 3 para no chocar con las fases ya comprometidas arriba.
Cinco reglas valen para las cinco fases siguientes, sin excepción:

1. **Todo cambio a C1-C4 es aditivo**, versión menor — igual que todo lo
   construido hasta hoy. Si una fase de acá abajo necesitara romper un
   contrato ya congelado, esa fase está mal diseñada, no el contrato.
2. **Ninguna fase empieza sin que la anterior cumpla su criterio de salida**
   verificable. No hay trabajo en paralelo que se adelante — salvo que se
   demuestre por qué un criterio es innecesario, y eso se escribe acá, no se
   omite en silencio.
3. **`motor.actuator.*` nunca es waivable por política**, sin flag, sin modo,
   sin excepción operativa. Es una regla dura del kernel, no una
   configuración — la única razón real para esa dureza en la sección Fase 7.
4. **La Fase 6 (`motor.self.*`) no se activa por defecto sin el executor Wasm
   de la Fase 3.** Sin sandbox, un skill que se auto-modifica corre con los
   mismos privilegios que cualquier otro `motor.*` hoy — proceso, sin
   aislamiento — así que declarar el tipo de efecto sin el sandbox no cambia
   el riesgo, solo lo nombra. Eso se documenta en el propio `skill.yaml` como
   una advertencia, no un bloqueo automático nuevo — la Fase 3 ya resuelve
   esto mejor que un gate ad hoc.
5. **Cada fase se prueba igual que las anteriores**: cobertura de Go donde
   aplique, un job de CI adversarial donde el invariante importa, y —siendo
   coherentes con la Fase 1— nada se declara "hecho" sin un test que lo
   demuestre atacándolo, no solo ejercitándolo en el camino feliz.

### Fase 4 · Gobernanza del protocolo y el nombre

**Por qué primero:** ninguna organización ajena a Deep Axiom va a correr un
nodo propio, ni va a co-firmar el checkpoint de nadie, mientras el protocolo
que ese nodo implementa sea "lo que una persona decide". Esto no se resuelve
con más código — se resuelve con menos control unilateral.

Qué se construye:

- **`spec/GOVERNANCE.md`** — quién puede proponer un cambio a C1-C4, quién lo
  revisa, cuánto dura el período de comentario, y bajo qué regla se acepta.
  Un RFC versionado por contrato (`C1-RFC-001`, …), no un PR que un solo
  maintainer aprueba.
- **Al menos un steward externo** con permiso de merge sobre `spec/` — no
  sobre `kernel/`, sobre el contrato. Kernel y SDKs siguen siendo
  implementaciones del contrato, no el contrato mismo — eso ya lo dice la
  arquitectura de hoy, esto lo hace cierto en los permisos del repo.
- **Resolución del choque de nombre.** Un nombre de protocolo distinguible
  del de Mezmo AURA y del de "Deep Axiom" — el protocolo y la empresa que
  mantiene la implementación de referencia no deberían compartir identidad,
  por la misma razón que C1-C4 no deberían depender de que Deep Axiom exista.

**Criterio de salida:**

| # | Criterio |
|---|---|
| 1 | `spec/GOVERNANCE.md` publicado y enlazado desde `README.md` |
| 2 | Al menos un colaborador externo a Deep Axiom con permiso de merge sobre `spec/` |
| 3 | Un RFC real (no de ejemplo) propuesto y resuelto siguiendo el proceso documentado |
| 4 | Nombre de protocolo decidido y comunicado — con o sin cambio de marca del kernel |

### Fase 5 · Ledger cross-witnessed + federación descubrible

**Por qué en este orden:** es la infraestructura que convierte "somos una
red" de afirmación a algo que un tercero puede verificar. Sin esto, federar
dos nodos sigue significando "confío en el operador del otro" — exactamente
el límite de confianza que la Fase 1 ya eliminó *dentro* de un nodo, pero
nunca *entre* nodos.

Qué se construye:

- **`witness` en el checkpoint (extensión aditiva de C4).** Cuando dos nodos
  están federados, cada uno ofrece el hash de su checkpoint más reciente al
  otro para que lo co-firme con su propia clave Ed25519 — misma cadencia que
  ya existe (100 entradas o 60s). El witness se guarda junto al checkpoint
  propio; no lo reemplaza.
- **`aura verify` extendido** para chequear witnesses cuando existen: la
  firma del witness tiene que validar contra la clave pública del nodo
  remoto, registrada al momento de federar — trust-on-first-use, el mismo
  patrón que ya usa el registro de paquetes para el `id` de un skill.
- **Federación descubrible.** El A2A card en `/.well-known/agent.json`
  (ya existe) gana un campo de federación: qué capacidades ofrece un nodo a
  quién quiera pedirlas, para que `aura federate` deje de requerir que un
  humano escriba la URL de memoria — un directorio liviano (puede ser el
  mismo registro federado que ya existe) es donde los nodos se anuncian.

**Criterio de salida:**

| # | Criterio |
|---|---|
| 1 | Dos nodos con claves y operadores distintos co-firman el checkpoint del otro, verificable con `aura verify` sin ninguno de los dos corriendo |
| 2 | Un test adversarial prueba que, si la clave *propia* de un nodo se compromete pero su witness no, `aura verify` sigue detectando la manipulación — ese es el punto entero del witness |
| 3 | Un nodo nuevo descubre y solicita federación con otro sin que un humano copie una URL a mano |

### Fase 6 · Cuña vertical piloto

**Por qué en este orden:** una red no crece por convicción arquitectónica —
crece porque alguien necesita, este trimestre, probarle algo a otra
organización que no confía en su palabra. El Artículo 12 del EU AI Act
(trazabilidad de decisiones de IA, vigente desde agosto de 2026) es el
forzador de adopción real; no hace falta inventar uno.

Qué se construye:

- **Un flujo real, no una demo** — 3 a 5 organizaciones de un mismo sector
  regulado (salud, banca, gobierno) con un caso donde hoy no se confían entre
  sí y necesitan probarlo ante un tercero (un regulador, un auditor, una
  contraparte contractual).
- **Un kit de despliegue de referencia para ese caso** — paquete de skills +
  plantilla de policy + script de federación, versionado y publicado como
  cualquier otro paquete del registro.
- Nada de esto es kernel nuevo — es la Fase 4 y la Fase 5 puestas a prueba
  contra organizaciones reales que no son Deep Axiom.

**Criterio de salida:**

| # | Criterio |
|---|---|
| 1 | 3+ nodos de organizaciones distintas, operando de forma independiente, federados y co-firmándose |
| 2 | Un revisor externo a las tres organizaciones (el regulador o auditor del caso elegido) verifica el ledger combinado offline, sin confiar en ningún operador individual |
| 3 | El kit de despliegue está publicado en el registro federado, no solo documentado |

### Fase 7 · `motor.self.*` y `motor.actuator.*`

**Por qué en este orden, y no antes:** generalizar qué tipo de efecto pasa
por el checkpoint del kernel solo vale la pena una vez que hay una red que
verifica lo que ese checkpoint certifica. Un nodo aislado que se
auto-modifica o mueve un brazo no necesita nada de esto — necesita la Fase 3
(sandbox) primero, y un testigo después.

Qué se construye:

- **`motor.self.*`** (extensión aditiva de C1, ningún cambio de kernel): un
  skill `cognitive` o `logical` lee el causal log (`aura why` ya expone
  esto), propone un parche a un grafo o a otro skill, y esa propuesta entra
  al kernel como cualquier edge hacia un `motor.*` — gateada, sellada,
  reversible por `aura undo` (Fase 2) si `aura replay` prueba después que
  empeoró algo. Sin camino especial.
- **`motor.actuator.*`** (extensión aditiva de C1 + una regla nueva y dura en
  `executor/policy.go`): `GraphWaiverAllowed` devuelve `false` incondicional
  para cualquier capability bajo este prefijo, sin importar lo que diga la
  policy cargada — la única excepción a "el nodo decide" en todo el sistema,
  y se justifica exactamente una vez, acá: un efecto físico irreversible no
  admite el mismo margen que hablar en voz alta.

**Criterio de salida:**

| # | Criterio |
|---|---|
| 1 | Un test adversarial prueba que ninguna policy, ni siquiera una escrita a propósito para intentarlo, puede waivear un edge `motor.actuator.*` |
| 2 | Un ejemplo de punta a punta: un skill se auto-observa, propone un parche, el parche se gatea y se sella, y `aura undo` lo revierte cuando se marca como regresión |
| 3 | `motor.self.*` está documentado como no-recomendado-por-defecto sin el executor Wasm de la Fase 3, con el aviso viviendo en el propio manifiesto del skill |

### Fase 8 · Gate de settlement generalizado

**Por qué al final:** es el incentivo económico para que a alguien le
convenga correr un nodo y federarse — pero un incentivo sin red (Fase 5) ni
casos reales (Fase 6) es una feature sin nadie que la use.

Qué se construye:

- **`gate` deja de ser binario** (extensión aditiva de C2/C3): hoy es
  `human-approval` o su ausencia; gana variantes tipadas —
  `settlement:x402`, `settlement:ap2` — resueltas por el mismo punto único
  del executor que ya aplica `human-approval` hoy. El kernel no aprende qué
  es un pago; solo aplica una prueba de autorización más, sea cual sea su
  forma.
- **`price` opcional en C1** para un skill `motor.*` — declarativo, mismo
  patrón que `permissions`.
- **El recibo de C4 carga la prueba de settlement** (hash del recibo de pago)
  en vez de, o además de, la aprobación humana — mismo campo `receipt`, sin
  cambiar de forma.

**Criterio de salida:**

| # | Criterio |
|---|---|
| 1 | Una llamada federada a un skill remoto con `price` declarado se rechaza sin prueba de settlement válida, y se entrega con una |
| 2 | La prueba de settlement es verificable offline junto con el resto de la cadena, por `aura verify` |
| 3 | Un test adversarial prueba que un recibo de settlement reutilizado (replay) se rechaza — mismo principio que ya aplica hoy al ingress de webhooks |

---
---

# Roadmap (English)

*Status as of 2026-08-01 · v0.3.0 → v0.5.0 (open beta). Phases 0 and 1 are
done.* This document is the
**state** of the runtime and its direction, not its history. None of it rests on
the git history — it rests on the code, and the README's [milestone
status](README.md#milestone-status) says which part of that code is covered by
tests and which was only hand-verified.

**This roadmap changes direction**, and the reason is worth reading before the
phases. The three blocks already built — foundations, connectivity and voice —
work end to end, but leave the project competing in five categories where three
competitors are more mature. Architecture does not fix that; a wedge does. The
one below is not invented from scratch: it is what this runtime nearly already
is.

## The thesis

> **Every effect on the world passes through one kernel checkpoint that
> authorises it by policy, seals it in a hash-chained signed ledger, and knows
> how to reverse it.**

The unit of work is not the message or the run. It is **the effect**. Four
properties — authorised, attested, reversible, reproducible — and no runtime on
the market has all four. Temporal has compensation but neither policy nor
attestation; LangGraph's interrupt lives in userland; n8n has an approval node
and logs; Dapr and MCP have none of it.

**Sixty per cent is already built:** the causal log, the kernel-invariant gate,
Ed25519 with canonical JSON, replay, and typed contracts. This is not a rewrite;
it is finishing what the shape already implied. And it is structurally
defensible: competing architectures are *run-based* and *cluster-based*, while
this one is envelope-based and single-binary. Here an effect costs one sha256 and
one INSERT — cheap enough for a Raspberry Pi. There it is not.

## Already built

Foundations (tests and a blocking CI pipeline where there were none; the
`motor.*` gate made a kernel invariant; connection liveness; transitive
cancellation with kernel-side suppression; the spec resynchronised),
connectivity (four ways in, for the four situations that actually occur), and
streaming multi-channel voice (first sound at 0.41s, preemption at ~0.2s, both
hand-measured on one developer machine). The build order was a dependency graph,
not a schedule: barge-in is impossible without transitive cancellation, and the
`realtime` QoS voice needs removes the only dead-connection detector the kernel
had.

## Phase 0 · Security and truth ✅ *(done)*

Three holes that **contradicted what the runtime claims to guarantee**, which is
why they came before anything new. All three are closed. They stay written down
because a hole closed without a record comes back.

**An attestation issued by a node with no authentication attests nothing.** It
proves an effect happened, not that someone entitled to authorise it did. The
ledger was worth exactly zero until the node knew who was talking to it — which
is why this came first, ahead even of the wedge.

**Hole 1 — the invariant did not survive whoever wrote the graph.** A node had no
authentication and listened on every interface, and a graph could waive the gate
with `"gate": "none"` — and a graph is JSON anyone could POST. Now: loopback
binding by default with an explicit, warned opt-out; a bearer token generated on
first start, written 0600 and printed once; a real `CheckOrigin` allowlist (the
token alone does not help, because a browser attaches credentials to a
cross-site handshake by itself); TLS; and a **policy engine**. The policy is what
actually closes it: `gate: "none"` went from being the last word to being a
*request*, honoured only if the node grants it. The rule in one line — **a graph
may be stricter than policy, never laxer.** The built-in default is a real
hashed document rather than an implicit code path, an absent waiver field in a
policy file means *false*, and in `published` mode the graph never gets a vote —
which finally gives the mode ladder a second real difference.

**Hole 2 — the guarantees did not cross a federation.** The bridge discarded
`cancel` envelopes and minted a fresh `idem`, so transitive cancellation and
idempotency — the two headline differentiators — stopped holding the moment you
federated a node. Now `cancel` is forwarded both ways and closes the remote
socket, `idem` is derived rather than invented, the remote graph is registered
once per session instead of once per envelope, every remote egress port keeps its
identity, and the remote skill's type is preserved so a federation cannot
launder an effect past the gate.

**Hole 3 — ingress was replayable and uncapped.** Now: persistent replay
detection keyed on a delivery digest (on disk, because a replay that works after
a restart is still a replay); an optional signed timestamp, Stripe-style, with
the timestamp *inside* the signature so an old capture cannot be revalidated
with a fresh clock; `idem` derived from the delivery, which fixes replay and
downstream dedup in one move; a per-route rate limit, a global session cap, and
one reaper instead of a goroutine per delivery. A replay is answered 200 rather
than 4xx, because a sender retrying under at-least-once is behaving correctly and
an error only makes it retry harder.

**SSOT.** The version lived in three files with two different values; the five
skill types were written out by hand in a Go regex, a Go slice, a Python tuple
and the JSON Schemas. `spec/VERSION` and `spec/enums.yaml` now generate the
constants for Go, Python and TypeScript, with a CI gate that fails on drift. The
JSON Schemas are *verified* rather than regenerated: C1 calls them normative and
they are hand-authored, so rewriting them mechanically would reformat the whole
document to say the same thing and bury the one line that changed.

**Coverage.** All seven zero-coverage packages now have tests — `signing` 89.4%,
`fed` 87.1%, `hub` 81.2%, `identity` 81.2%, `mcpsrv` 62.7%, `projection` 61.0%,
`config` 100%. `internal/` sits at **76.0%**. The whole-module figure is 51.8%,
and the gap is entirely `cmd/aura`: 2,648 lines of CLI at 11.2%, which is tested
by running real nodes rather than by unit tests. **The ≥80% target this document
set itself is not met**; what is true is that no package is at zero any more and
that the trust-carrying subsystems are above 85%. Raising `cmd/aura` is Phase 1
work.

**CI** gained three jobs: `adversarial`, which starts a real binary **with
default flags** and checks it refuses what it should; `ssot`; and
`cross-compile`, building all six targets (linux/darwin/windows × amd64/arm64,
16-17 MB, no cgo) on every change.

## Phase 1 · The effect ledger ✅ *(done)* — the wedge

One new kernel primitive, exactly one: **P5, the Effect Ledger**
([`kernel/internal/ledger`](kernel/internal/ledger/)). It still passes the
project's purity test — nothing in it needs an LLM, the marketplace, or
anything but hashes, signatures and SQLite. A fourth frozen contract, **C4**
([spec/c4-ledger.md](spec/c4-ledger.md)), alongside C1/C2/C3, which were
**not touched** — the innovation landed as a new contract plus two optional
fields: `compensates` on C1 and `receipt` on C3. A skill written before this
phase keeps working unchanged.

The checkpoint lives in `Session.forward()`
([executor/session.go](kernel/internal/executor/session.go)) and in the denial
branch of `Session.resolveGate()` — the two points every delivery and every
gate resolution already passed through. No third insertion point was needed,
which is the sign Phase 0's architecture was already right. One nuance only
implementation surfaces: *authorization* had already happened earlier, in
`applyPolicy` when the session was built — the checkpoint in `forward()` only
reads the gate that was already decided. And a denied gate is sealed too
(`decision: gate, outcome: denied`), not just a delivered one, because an
auditor asking "what did this session try to do" needs the refused proposal,
not only what succeeded.

Entries are ~350-400 bytes and store `payload_sha256` rather than the payload,
because the ledger is evidence, not a data lake. `Hash()` uses plain
`json.Marshal` — no map-key canonicalization the way `CanonicalManifestHash`
needs for arbitrary user YAML, because a Go struct's field order is fixed at
compile time and therefore already deterministic. Signed checkpoints amortise
Ed25519 over 100 entries or 60 seconds, whichever comes first, the Certificate
Transparency trick, and the cadence survives a restart. `aura verify [--data
<dir>]` recomputes the chain and checks signatures **without the node
running**, and never touches the private key — only the public one
(`signing.LoadPublicKey`, which deliberately never creates a key as a side
effect). `GET /v1/ledger` and `GET /v1/ledger/verify` expose the same thing
over HTTP for a running node, backed by the identical `ledger.Verify` function.

**The proof that matters is that attacking it for real actually fails.** Two
scenarios, both proven in `ledger_test.go` and both re-proven against a real
binary by a CI job
([`scripts/ledger_adversarial.py`](scripts/ledger_adversarial.py)) that
registers a real motor skill, seals two effects through the real
approval path, and edits `kernel.db` directly: tampering an interior entry
breaks the forward hash chain by itself; tampering a checkpointed entry *and
repairing every `prev` pointer after it* — the sophisticated attack a plain
hash chain cannot catch by construction — breaks the checkpoint signature,
because the attacker never had the node's private key. That second case is
the one that actually justifies checkpoints existing at all.

**Coverage:** `ledger` 89.2%, including both adversarial scenarios, a restart,
checkpoint cadence, and 50 concurrent goroutines sealing at once with the
resulting chain verified intact. `executor` rose to 88.0% with the checkpoint
integration proven end to end: an ungated effect seals `allow`/`delivered`, an
approved one seals `gate`/`delivered`, a denied one seals `gate`/`denied`, and
the receipt on the delivered envelope matches the sealed entry's own hash
exactly.

## Phase 2 · Reversibility and resume ← next — ~4 weeks, post-beta

The two ship together because they are the same problem — reconstructing state
from the log — and get cheaper once Phase 1 exists. Part of the work is already
done: `compensates` (C1) and every sealed entry's `compensation` field (C4)
shipped in Phase 1 — a motor skill can already declare its reverse and the
ledger already records it on every effect. What is missing is the active half:
`aura undo <session|receipt>` walking the ledger backwards in reverse causal
order, with each undo *itself* a sealed, policy-checked effect — no special
"this is an undo" path that skips the checkpoint. Effects without declared
compensation are already visible as irreversible today, in any ledger entry
with no `compensation` field — readable now via `aura verify` or
`GET /v1/ledger`, even though `aura undo` itself does not exist yet. Session
resume ships alongside it. Plus deterministic replay against the ledger,
closing the fourth property.

## Phase 3 · Reach — post-beta

`libaura` via `-buildmode=c-shared` with Kotlin/Swift bindings and `GOOS=wasip1`;
Wasm skills on wazero, which is what finally makes C1 rule 3 **true** rather than
declarative; Postgres CDC, still the most differentiating path against n8n for
legacy with no API; and negotiated P2P transport, already contemplated by C3 rule
5.

## How the beta opens

Security first, wedge second: a ledger signed by a node anyone can drive would
be worse than no ledger, because it would offer a false sense of proof. With
Phases 0 and 1 both done, that ordering held.

Exit criteria are verifiable rather than opinions, and **8 of 10 are met**:
the adversarial job passes against a default node, the SSOT gate is green, all
six binaries build on every change, no kernel package sits at zero coverage,
`go test -race` is green in CI, the ledger's hash chain and `aura verify` work
with no node running, and tampering — including the repaired-chain case only a
checkpoint signature catches — is proven to make verification fail, both in Go
tests and against a real binary in CI. Outstanding: kernel coverage ≥80%
(`internal/` is at 77.4%, the module 55.6% because of `cmd/aura`; the
trust-carrying subsystems — `ledger` 89%, `signing` 90%, `registry` 93%,
`executor` 88% — already clear it), a run on real Raspberry Pi hardware, and
`SECURITY.md` plus signed release artifacts and an SBOM. Neither remaining item
is a code gap: one needs physical hardware to prove (the binary already
cross-compiles for arm64), the other is release-process paperwork with no
architecture decision pending behind it.

## Deferred, and why

Multi-device, `bulk` QoS, binary WebSocket frames, a visual graph editor,
MQTT/BLE peripherals and a managed cloud — the reasoning for each is in the
Spanish section above. The cloud is deliberately last: it is just another node,
and the open runtime has to be complete first.

---

## Horizon · The network (post-Phase 3, no date)

**This is not the beta.** Phases 0-3 above are what make `aura up` safe to
run today. What follows is a different question: what AURA needs to stop
being *what one operator installs* and become *what several organizations
that don't know each other use to prove things to one another*. Nothing
below has a date — it has order and an exit criterion, same as everything
above, because that is the only thing this document ever promises.

Numbered onward from Phase 3 so it never collides with the committed phases
above. Five rules apply to all five phases below, no exceptions — full
reasoning for each is in the Spanish section:

1. **Every change to C1-C4 is additive**, a minor version — same as
   everything built so far. A phase below that needed to break a frozen
   contract would be a badly designed phase, not a reason to break the
   contract.
2. **No phase starts before the previous one clears its verifiable exit
   criterion.** No jumping ahead in parallel, unless a criterion is shown to
   be unnecessary and that reasoning is written down here, not silently
   skipped.
3. **`motor.actuator.*` is never waivable by policy** — no flag, no mode, no
   operational exception. A hard kernel rule, not a configuration, justified
   exactly once, in Phase 7.
4. **Phase 6 (`motor.self.*`) does not activate by default without Phase 3's
   Wasm executor.** Without a sandbox, a self-modifying skill runs with the
   same privileges as any other `motor.*` skill today — a process, no
   isolation — so declaring the effect type without the sandbox names the
   risk without changing it. That is documented as a warning in the skill's
   own manifest, not a new ad hoc gate — Phase 3 already solves this better.
5. **Every phase is tested the same way as the ones above it**: Go coverage
   where it applies, an adversarial CI job where the invariant matters, and
   — consistent with Phase 1 — nothing is called done without a test that
   attacks it, not one that only exercises the happy path.

### Phase 4 · Protocol governance and the name

**Why first:** no organization outside Deep Axiom is going to run its own
node, let alone co-sign anyone else's checkpoint, while the protocol that
node implements is "whatever one person decides." Code doesn't fix this —
giving up unilateral control does.

- `spec/GOVERNANCE.md` — who proposes a change to C1-C4, who reviews it, and
  under what rule it's accepted. A versioned RFC per contract, not a PR one
  maintainer approves alone.
- At least one external steward with merge rights on `spec/` — the contract,
  not the kernel. Kernel and SDKs stay implementations of the contract, same
  as today; this makes that true in the repo's permissions, not just in prose.
- The name collision resolved — a protocol identity distinguishable from
  both Mezmo's AURA and from Deep Axiom the company.

**Exit criterion:** `GOVERNANCE.md` published and linked from `README.md`;
at least one external merge-right holder on `spec/`; one real RFC proposed
and resolved through the documented process; the protocol name decided and
announced.

### Phase 5 · Cross-witnessed ledger + discoverable federation

**Why in this order:** it's the infrastructure that turns "we are a network"
from a claim into something a third party can check. Without it, federating
two nodes still means "I trust the other operator" — the exact trust
boundary Phase 1 already removed *inside* one node, never removed *between*
nodes.

- `witness` on the checkpoint (additive C4 extension): federated nodes offer
  each other their latest checkpoint hash for co-signing, same cadence as
  today's checkpoint signing. The witness is stored alongside the node's own
  checkpoint, never replacing it.
- `aura verify` checks a witness when one exists, against the peer's public
  key recorded at federation time — trust-on-first-use, the same pattern the
  package registry already uses.
- Discoverable federation: the A2A card gains a federation field so a node
  advertises what it offers to whoever wants to ask — `aura federate` stops
  requiring a human to type a URL from memory.

**Exit criterion:** two independently-keyed, independently-operated nodes
co-sign each other's checkpoints, verifiable with neither running; an
adversarial test proves that a compromised *local* signing key still gets
caught by an intact witness; a new node discovers and requests federation
without a human copying a URL by hand.

### Phase 6 · A regulated vertical wedge

**Why in this order:** a network doesn't grow from architectural conviction —
it grows because someone needs, this quarter, to prove something to another
organization that doesn't take their word for it. EU AI Act Article 12
(in force since August 2026) is the real adoption forcer; there's no need to
invent one.

- A real workflow, not a demo — 3 to 5 organizations in one regulated
  sector with a case where they don't trust each other today and need to
  prove something to a third party (a regulator, an auditor, a counterparty).
- A reference deployment kit for that case — skill bundle + policy template
  + federation script, versioned and published through the registry like
  anything else.
- None of this is new kernel — it's Phase 4 and Phase 5 tested against real
  organizations that are not Deep Axiom.

**Exit criterion:** 3+ independently-operated nodes from distinct
organizations, federated and cross-signing; a reviewer external to all
three verifies the combined ledger offline, trusting no single operator;
the deployment kit published in the federated registry, not just documented.

### Phase 7 · `motor.self.*` and `motor.actuator.*`

**Why not sooner:** generalizing which effect types pass through the kernel
checkpoint only pays off once a network exists to verify what that
checkpoint certifies. An isolated node that rewrites itself or moves an arm
needs Phase 3's sandbox first, and a witness after — not this.

- `motor.self.*` (additive C1 extension, no kernel change): a `cognitive` or
  `logical` skill reads the causal log, proposes a patch to a graph or
  another skill, and that proposal enters the kernel as an ordinary edge
  into a `motor.*` skill — gated, sealed, reversible by `aura undo` (Phase 2)
  if `aura replay` later proves it made things worse. No special path.
- `motor.actuator.*` (additive C1 extension plus one hard new rule in
  `executor/policy.go`): `GraphWaiverAllowed` returns `false`
  unconditionally for this prefix regardless of the loaded policy — the one
  exception to "the node decides" anywhere in the system, justified exactly
  once: an irreversible physical effect doesn't get the same latitude as
  speaking out loud.

**Exit criterion:** an adversarial test proves no policy, not even one
written on purpose to try, can waive a `motor.actuator.*` edge; an
end-to-end example where a skill observes itself, proposes a patch, the
patch is gated and sealed, and `aura undo` reverts it once flagged as a
regression; `motor.self.*` documented as not-recommended-by-default without
Phase 3's Wasm executor, the warning living in the skill's own manifest.

### Phase 8 · A generalized settlement gate

**Why last:** it's the economic reason to run a node and federate — but an
incentive with no network (Phase 5) and no real cases (Phase 6) is a feature
nobody uses.

- `gate` stops being binary (additive C2/C3 extension): today it's
  `human-approval` or its absence; it gains typed variants —
  `settlement:x402`, `settlement:ap2` — resolved at the same single
  checkpoint that already applies `human-approval` today. The kernel never
  learns what a payment is; it just applies one more proof of authorization,
  whatever shape it takes.
- Optional `price` in C1 for a `motor.*` skill — declarative, same pattern
  as `permissions`.
- C4's receipt carries the settlement proof (a payment receipt hash) instead
  of, or alongside, human approval — same `receipt` field, same shape.

**Exit criterion:** a federated call to a remote skill with `price` declared
is refused without valid settlement proof and delivered with one; the
settlement proof verifies offline alongside the rest of the chain; an
adversarial test proves a replayed settlement receipt is rejected — the same
principle already applied to webhook ingress today.
