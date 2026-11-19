# Plan: inferencia, contexto y memoria

**Estado: las fases 0a y 1 están implementadas y probadas en la rama
`fase-1/paso-por-el-nodo`, sin fusionar. La fase 0b tiene el banco construido y
ninguna medida contra un motor real. Todo lo demás es propuesta.**
Fecha: 2026-11-19 · Base: `main` en `bf8a529` (v0.4.0) · Revisión 6.

Este documento decide tres cosas: cómo el nodo pasa a gobernar la inferencia
(con vLLM como motor principal en servidor), cómo se gasta menos contexto por
decisión, y cómo se recuerda sobre millones de registros sin perder de dónde
salió cada dato.

Frente al [ROADMAP](../../ROADMAP.md), lo que cubre y lo que no:

| Fila del roadmap | Este plan |
|---|---|
| El nodo dueño de su servidor de inferencia | A medias: el nodo ya ve, sitúa, detiene y cuenta la inferencia (fase 1, **hecha**). Arrancar y supervisar el motor: fase 3 |
| Más de un modelo en un acelerador | La cierra para los modelos que sirve un motor; ASR y TTS con pesos propios quedan solo contabilizados (5.5) |
| Prioridad que desaloja | La cierra a nivel de nodo (fase 2); dentro del lote depende de vLLM |
| Presupuesto de contexto en tokens reales | La cierra (fase 4) |
| Plazos que hacen algo | Herencia real, corte y registro: **hecho** (fase 0a). Admisión, rechazo y recorte en el motor: fase 2. Qué se ejecuta *en su lugar* sigue siendo cosa de cada Expertise (5.4) |
| Cambio de modelo sin parar | Se construye en la fase 3 (5.6); necesita poder repetir sesiones contra la instancia candidata |
| Optimización como paso | **No la toca.** Solo amplía la estimación de `model-fit` |

Las cifras de terceros llevan su origen: **(proveedor)** si las midió quien
vende el producto, **(autores)** si son del propio artículo, **sin verificar**
si no se confirmó en fuente primaria. Los 36 datos externos de los que depende
el plan se comprobaron en su fuente el 2026-11-19. Ningún umbral es una
promesa: los criterios de salida se fijan con lo que mida la fase 0b.

**Historial.** El plan pasó tres rondas de contraste con el código.

- La ronda 1 tumbó los tickets firmados (falsificables con el modelo de
  credenciales actual) y el uso del índice de atestaciones para registros por
  inferencia.
- La ronda 2 tumbó dos decisiones de la revisión 2. Guardar el plazo por raíz
  causal habría descartado casi todo un stream. Y separar la pasarela en otro
  proceso no salvaba las generaciones en curso durante un relevo, que era su
  razón de ser.
- La ronda 3 dio por válida la dirección y encontró un defecto latente que la
  herencia de plazos despertaría (un efecto sellado y luego descartado), además
  de tres ajustes al linaje y a las citas.

Lo que sigue incorpora las tres. **Las correcciones de la ronda 3 no se han
vuelto a revisar**: están en 5.1, 5.2, 5.6, 5.7 y en la fase 0a.

**Lo implementado difiere del plan en un punto, a mejor.** La revisión 4 dejaba
abierta una decisión: si un plazo delante de un stream debía cortar la
respuesta o contar solo hasta la primera palabra. Se resolvió sin elegir: son
dos promesas distintas y ahora son dos campos. `deadline_ms` es un límite de
validez y corta; `respond_ms`, nuevo, es un objetivo de respuesta y nunca corta.
Está en 5.1.

---

## 1. Resumen de decisiones

| # | Decisión | Por qué |
|---|---|---|
| 1 | **No se escribe un motor de inferencia ni se hace fork de vLLM.** Se conducen motores existentes con drivers. | Ningún motor supera a vLLM de forma robusta (diferencias de un dígito, casi todas medidas por proveedores). vLLM publica cada dos semanas; un fork es deuda permanente. |
| 2 | **Dos motores de primera clase: vLLM (servidor y borde ≥16 GB) y `llama-server` de llama.cpp (borde de 8 GB, CPU, desarrollo en Windows).** | vLLM no corre en Windows nativo y pesa demasiado para 8 GB. llama.cpp trae paginación de modelos, slots persistibles y caché cuantizada. |
| 3 | **La inferencia servida pasa por el nodo:** un paquete propio dentro del proceso del nodo, separado del executor, con rutas en el listener que ya existe. | El kernel hoy no ve ese tráfico. Dentro del nodo porque ahí están las entregas, las cancelaciones, las conexiones y la memoria, y un proceso aparte obligaría a duplicarlas sin ganar nada (5.2). |
| 4 | **La pieza central es nueva y está en el kernel: el linaje de entregas.** Sostiene cuatro cosas: herencia de plazos, prioridad de una petición, cancelación y cita de evidencia. | Hoy cada una falla por la misma causa: el kernel no recuerda qué entrega causó cuál. |
| 5 | **La cola vive en el nodo; el lote vive en el motor.** | Ningún motor entiende plazos y la prioridad de vLLM no desaloja (issue #40004). Así el comportamiento no depende del motor. |
| 6 | **El contexto se arma por segmentos ordenados por volatilidad y se enruta al slot que ya tiene ese prefijo.** | Es la palanca con mejor evidencia de ahorro, y el nodo es a la vez arnés y enrutador. |
| 7 | **La memoria es un almacén propio de registros que citan su evento de origen; los índices son proyecciones reconstruibles de ese almacén.** Búsqueda híbrida con expansión por enlaces causales. | No hace falta base de grafos ni base vectorial dedicada en el borde. |
| 8 | **Cada efecto cita qué inferencias hubo en su camino y qué había en su ventana,** con registros que escribe el nodo. | Es lo que ningún competidor revisado ofrece. Exige un cambio aditivo de C4. |
| 9 | **Con vLLM se entra aportando datos:** primero reproducir y ayudar a cerrar #40004, después proponer plazos por petición. | Es la contribución con más opciones de entrar: *help wanted*, y un mantenedor la llamó "a serious issue". |

Lo que se descarta está en la [sección 9](#9-lo-que-no-se-hace).

---

## 2. De dónde se parte

Comprobado en el código de `main`.

| Hecho | Dónde | Consecuencia |
|---|---|---|
| Por defecto la inferencia es llama.cpp **dentro del proceso** de cada Expertise. Con `AURA_INFERENCE_URL` va por HTTP directo a un servidor; `OPENAI_API_KEY` tiene preferencia. El planner usa su propio cliente. ASR y TTS cargan sus pesos y no usan HTTP. | [sdk/python/src/aura/llm.py](../../sdk/python/src/aura/llm.py), [expertise/planner/backends.py](../../expertise/planner/backends.py) | El kernel no ve ninguna inferencia. Este plan gobierna solo la servida. |
| El nodo lanza procesos hijo solo para los ejemplos; los mata al parar con orden y no supervisa ninguno. | [kernel/cmd/aura/examples.go](../../kernel/cmd/aura/examples.go) | El supervisor de motores es trabajo nuevo. |
| La admisión es un escalar de RAM por conexión. El kernel no lee `requirements.accelerator`. | [kernel/internal/gateway/admission.go](../../kernel/internal/gateway/admission.go) | Hace falta un fondo por dispositivo (5.5). |
| El envelope lleva `deadline` y `priority` (mayor gana). La prioridad se sella por arista desde el grafo; el kernel no desaloja nada. | [kernel/internal/channel/envelope.go](../../kernel/internal/channel/envelope.go), `executor/session.go` | La señal existe; nadie actúa sobre ella. |
| **El plazo no sobrevive a un salto por un Expertise.** El kernel hereda el `deadline` del envelope que el Expertise emite, y ningún SDK lo emite. Ningún test cubre la herencia. | `executor/session.go` (cálculo de `out.Deadline`), [sdk/python/src/aura/envelope.py](../../sdk/python/src/aura/envelope.py) | Defecto existente frente a lo que dice el README. |
| **`forward` sella el efecto en el ledger antes de comprobar si su plazo venció.** Hoy casi no se alcanza, porque el plazo se calcula en ese mismo salto. Ningún test ni grafo del repositorio usa `deadline_ms`. | `executor/session.go` (`forward`) | Con la herencia funcionando, un efecto quedaría sellado como entregado y luego descartado. Se arregla antes. |
| **El kernel no comprueba el `cause_id` que emite un Expertise,** ni sabe qué conexión emitió: `Dispatch` recibe solo el envelope. | `executor/session.go` (`rootOf`, `Route`), `executor/manager.go` | Un Expertise puede colgar su salida de la entrega que quiera. Afecta ya a las atestaciones. |
| El kernel recuerda entrega → raíz en un índice de 8192 por sesión que cuenta cada reenvío, incluidos los que van al cliente y cada token. No recuerda qué entrega causó cuál. | `executor/causal.go` | No sirve para preguntar por una entrega concreta minutos después. |
| Una conexión de Expertise no guarda con qué token se registró. | `kernel/internal/registry/registry.go` | Dos réplicas de una capacidad son indistinguibles. |
| Ningún SDK expone `deadline` ni `priority` al handler; el de Node tampoco expone el identificador de la entrega. Ninguno de los dos campos está en `envelope.schema.json`. | `sdk/`, `spec/schemas/` | El Expertise "debe degradar" y no puede saber cuánto le queda. |
| **Por defecto todo Expertise corre con el token de operador.** Los tokens acotados existen (`aura token issue`) y solo alcanzan las rutas de `skillRoutes`. | [sdk/python/src/aura/skill.py](../../sdk/python/src/aura/skill.py), [kernel/internal/gateway/scope.go](../../kernel/internal/gateway/scope.go) | Cualquier garantía de "el Expertise no puede…" exige tokens acotados. |
| La atestación C5 la afirma el Expertise. Se liga por raíz causal, con tope de 16 por raíz, en un índice en memoria que no se reconstruye al reanudar. Su spec prohíbe llevar contexto recuperado. | `executor/causal.go`, [spec/c5-attestation.md](../../spec/c5-attestation.md) | No sirve para un registro por inferencia (5.7). En una sesión larga con una sola raíz, los efectos citan las 16 primeras atestaciones, no las suyas. |
| Solo se sellan en el ledger las entregas `data` a un Expertise `motor`. Un recibo debe incluir lo que cita. | `executor/session.go` (`sealEffect`), `ledger/receipt.go` | Lo citado tiene que vivir en un almacén direccionable por hash, no solo en el registro causal. |
| Una entrega retenida en una puerta puede sellarse minutos u horas después. Vive en memoria; al reanudar, la puerta se reconstruye desde el `confirm_request` del registro, y la reanudación lee solo los primeros 1000 eventos. | `executor/session.go`, `executor/resume.go` | Lo que cite debe guardarse en el registro, no solo con ella. |
| Al parar con orden, el nodo espera hasta 10 s a que terminen los handlers HTTP. Hoy vuelve al instante porque los WebSocket no cuentan. | `kernel/cmd/aura/main.go` | Con handlers de streaming largos, cada parada agotaría la espera. |
| Cuando un Expertise pierde su conexión con el kernel, `emit` falla; `llm-chat` responde deteniendo la generación. Tras un relevo, el nuevo líder rechaza salidas de sesiones que aún no se han reanudado, y no reentrega nada. | `sdk/python/src/aura/skill.py`, `expertise/llm-chat/main.py`, `executor/manager.go` | Ninguna generación en curso sobrevive a un relevo, se sirva como se sirva. |
| El respaldo es un segundo proceso sobre **el mismo directorio de datos**; los 21–118 ms son en el mismo host. Al perder el lease, el nodo sale sin drenar. | `kernel/internal/lease/standby.go`, `kernel/cmd/aura/main.go` | Condiciona qué pasa con un motor al relevar (5.6). |
| Las sesiones abiertas por un webhook viven dos minutos. | `kernel/internal/gateway/ingress.go` | Al ligar la cancelación al fin de sesión, una generación larga iniciada por webhook se cortará ahí. |
| El presupuesto de contexto se cobra a bytes/4. | [kernel/internal/executor/budget.go](../../kernel/internal/executor/budget.go) | Se añade un campo nuevo; el actual no cambia. |
| El registro causal se lee solo por sesión. Lo que emite un productor se guarda entero; solo se elide la copia reenviada por una arista `realtime`. La poda borra segmentos completos. La lista de tipos de envelope es cerrada. | [kernel/internal/seglog/seglog.go](../../kernel/internal/seglog/seglog.go), `spec/enums.yaml` | No hay lectura global ni borrado por registro. Un evento nuevo del kernel reutiliza `status`. |
| `EventObserver` es un único hueco y lo ocupa el exportador de trazas. | [kernel/internal/store/store.go](../../kernel/internal/store/store.go) | Un segundo consumidor del registro necesita un reparto nuevo. |
| El kernel usa SQLite en Go puro, sin extensiones cargables. | [kernel/go.mod](../../kernel/go.mod) | El índice vectorial vive en un Expertise de Python. |
| `aura regress` repite sesiones contra el nodo en vivo y por defecto informa, no decide (`--fail-on-regression` lo cambia). | [kernel/cmd/aura/regress.go](../../kernel/cmd/aura/regress.go) | Hoy no puede apuntar a una instancia candidata. |
| `compact_history` ya recorta en bloques para no romper el prefijo. | [sdk/python/src/aura/llm.py](../../sdk/python/src/aura/llm.py) | El principio del plano de contexto ya se aplica en un sitio. |
| El kernel compila el esquema de un puerto a una gramática de decodificación. | `kernel/internal/grammar` | Evita hechos mal formados al consolidar con modelos pequeños. |
| La federación entre nodos no propaga plazo, prioridad ni atestación. | `kernel/internal/fed/bridge.go` | Fuera de alcance; todo lo de este plan es de un nodo. |

---

## 3. Qué dice la investigación

Solo lo que cambia el diseño. Fuentes en la [sección 14](#14-fuentes).

### Motores

- **vLLM v0.34.0** (16 nov 2026). Cadencia quincenal; flags y formatos cambian
  entre versiones, así que la versión se fija.
- **La prioridad de vLLM no desaloja.** `--scheduling-policy priority` ordena la
  cola de espera; solo desaloja cuando se agotan los bloques de caché, y la
  víctima se recalcula desde cero. Valor más bajo = antes (AURA y SGLang, al revés).
- **Ningún motor tiene plazos.** Dos RFC del planificador de vLLM se cerraron
  por inactividad. Su roadmap Q4 incluye "Simplify and harden the engine's
  model loop, data-parallel flow, and scheduler".
- `--scheduler-cls` carga un planificador propio sin fork; la interfaz está
  declarada no pública.
- **Un proceso de vLLM sirve un modelo.** El modo *sleep* solo existe en modo
  desarrollo.
- vLLM y llama.cpp sirven embeddings y rerank.
- **llama.cpp** tiene modo enrutador (`--models-max`, `POST /models/load`),
  slots con `save|restore`, afinidad por `id_slot` y caché KV cuantizada
  (`-ctk/-ctv`). No tiene prioridad por petición.
- **SGLang** sí desaloja por prioridad con umbral y ya expone endpoints para
  modelos de acción. Es la alternativa si vLLM no avanza.
- En Jetson Orin Nano 8 GB (una prueba comunitaria): TensorRT Edge-LLM 60 tok/s
  y 4,3 GB; vLLM 56 tok/s y 6,9 GB; llama.cpp 38 tok/s. No es una comparación
  limpia: el modelo es un VLM, y llama.cpp corrió con otra cuantización y un
  contexto cuatro veces mayor.
- Los modelos de acción (openpi, GR00T, LeRobot) traen cada uno su servidor;
  ninguno habla la API de OpenAI.

### Contexto y tokens

- La caché de prefijo es coincidencia exacta: un byte cambiado invalida todo lo
  que sigue. Ordenar por volatilidad y serializar de forma determinista da
  tasas de acierto del 85–97 % en agentes (proveedor).
- Enrutar a la réplica que ya tiene el prefijo: P90 de primer token 0,54 s
  frente a 31 s aproximado y 92 s aleatorio (medido por el propio proyecto llm-d).
- Restaurar un slot de llama.cpp en vez de releer: 9,9 s → 1,4 s para un chat de
  5K tokens (un usuario, una GPU).
- Enmascarar observaciones antiguas reduce el coste a la mitad e iguala al
  resumen con LLM (JetBrains, independiente, solo en SWE-agent).
- La compresión de prompts rinde poco en agentes y rompe la caché de prefijo.
- Reutilizar caché KV fuera del prefijo pierde 7–18 % de F1. El estado KV es de
  solo prefijo, atado a pesos y cuantización exactos, y no editable.
- Tamaño: unos 144 KiB por token en modelos de 4–8B (bf16). 100K tokens son
  ~14 GB.

### Memoria

- Búsqueda híbrida con expansión a vecinos y **sin LLM en la ingesta**: 83,4 %
  en LongMemEval. Añadir hechos extraídos por LLM *junto a* los originales:
  86,5 %, pero la ingesta pasa de 1,4 s a 247,8 s por sesión (Redis, proveedor).
- A presupuesto de tokens igualado, la ventaja de GraphRAG casi desaparece:
  71,17 frente a 71,01 del RAG con los mismos tokens y 65,77 del RAG estándar
  (un conjunto de datos, un modelo). Los grafos ganan en varios saltos.
- Con un modelo de 3B, la memoria estructurada se degrada (30 % de errores de
  formato).
- Kuzu, la base de grafos embebida habitual, está archivada desde octubre de 2025.
- `sqlite-vec` estable (v0.1.9) es fuerza bruta con cuantización binaria; el
  índice aproximado sigue en prelanzamiento.
- Los benchmarks publicados no son fiables: 6,4 % de la clave de LoCoMo es
  errónea y varias cifras de proveedores no se reprodujeron.
- eMEM (memoria de robot, jun 2026): SQLite + índice vectorial + R-tree; su
  variante de RAG plano pierde ~30 puntos en recuperación dependiente de contexto.

---

## 4. Principios

1. **El nodo decide, el motor ejecuta.** Prioridad, plazo, admisión y evidencia
   se resuelven en un sitio.
2. **El executor no aprende de modelos ni de motores.** Sin tokenizador ni
   plantillas de chat en ningún sitio del nodo. El conocimiento de motores
   (flags, métricas, campos de petición) vive en drivers fijados a una versión,
   en un paquete aparte del executor. Añadir un motor es añadir un driver, y es
   código del binario.
3. **Observado antes que afirmado, y nunca mezclados.** Lo que ve el nodo y lo
   que afirma un Expertise se guardan y se muestran por separado.
4. **Dos almacenes, dicho claro.** El registro causal guarda lo que fluyó. La
   memoria guarda registros que citan su origen y sobreviven a la poda del
   registro. Los índices sí son desechables.
5. **Aditivo.** Los specs están congelados: cada cambio es un campo opcional
   nuevo. Este plan toca el esquema del envelope (añadir dos campos que C3 ya
   describe), C1, C2 y C4. Están listados por fase en la sección 11.
6. **Se mide o no se afirma.**

---

## 5. Plano de inferencia

### 5.1 Linaje de entregas

La pieza de la que depende casi todo lo demás, y la primera que se construye.

**Qué es.** Un índice del kernel con una entrada por cada entrega hecha a una
Expertise (no las que van al cliente). Cada entrada guarda:

- la entrega anterior en su cadena, es decir, la que causó la emisión que ahora
  se reenvía, y la raíz;
- a quién se entregó: nodo del grafo, puerto, conexión y token;
- su plazo absoluto;
- una referencia a las citas de evidencia acumuladas en su camino (5.7). El
  conjunto se comparte entre entregas, no se copia.

**Qué resuelve:**

| Problema de hoy | Con el linaje |
|---|---|
| El plazo se pierde al pasar por un Expertise. | Una emisión hereda el plazo de la entrega que la causó. El Expertise no puede alargarlo, porque no lo transporta. |
| Una petición de inferencia no se puede atribuir a una cadena. | La petición nombra su entrega; el kernel sabe a quién se dirigió y con qué plazo. |
| Cancelar depende de que el Expertise obedezca. | Las peticiones en curso se registran por entrega y por raíz, y el nodo las corta. |
| La evidencia se liga por raíz y mezcla caminos. | Las citas viajan de entrega en entrega por su propio camino. |

**Por qué por entrega y no por raíz.** Todos los tokens de una respuesta, y todo
un stream de sensor, comparten una raíz. Un plazo es un instante absoluto: si se
guardara uno por raíz con la regla "solo se acorta", el primero ganaría y todo
lo posterior nacería vencido.

**Dos límites, no uno.** Un mensaje puede ir bajo los dos a la vez, y fallan de
forma distinta:

| | `deadline_ms` → `deadline` | `respond_ms` → `respond_by` |
|---|---|---|
| Qué dice | Después de esto, ya no sirve. | Empieza a responder antes de esto. |
| A qué afecta | A todo lo que la entrega provoca | Al frente de lo que provoca |
| Si no se cumple | Se descarta; un efecto se sella como `expired` | Se registra; no se descarta nada |
| Para qué | Un acto sobre el mundo | Una respuesta que alguien espera oír empezar |

**Reglas, cada una con su test:**

1. Una emisión hereda el plazo de la entrega que la causó. Un envelope del
   cliente que trae su propio plazo lo conserva.
2. Un Expertise no puede alargar su plazo: no lo transporta. Si lo escribe en
   lo que emite, se ignora.
3. **El vencimiento se comprueba antes de sellar.** Un efecto tardío se sella
   como `expired`, nunca como `delivered`.
4. Una entrega retenida en una puerta conserva el presupuesto con el que se
   retuvo. Una aprobación que llega después del plazo no ejecuta el efecto: se
   sella como `expired`, con la aprobación adjunta, y se avisa a quien aprobó.
   La pregunta de aprobación dice cuándo caduca.
5. Un productor en streaming seguido de una arista con `deadline_ms` se
   comporta como antes: cada entrega recibe su plazo local.
6. `respond_by` lo lleva solo la primera salida de cada entrega. Un retraso se
   registra una vez y no descarta nada.
7. Al cliente se le avisa una vez por cadena de que la salida tardía se está
   descartando; un `done` o un `error` siempre viajan.

**Cambio de comportamiento para grafos existentes.** Un `deadline_ms` delante
de un stream ahora corta la respuesta al vencer, como el contrato decía desde
siempre y nunca ocurrió. Quien lo usaba para decir "responde pronto" debe
cambiarlo a `respond_ms`. Está avisado en la guía.

**Comprobación de la causa.** Cuando un Expertise emite, el kernel comprueba la
causa que declara:

- Si es una entrega hecha a esa conexión, el enlace es válido.
- Si es una entrega hecha a otra, la emisión se puede rechazar por política.
- Si la entrada ya no existe, el enlace se marca como no verificado.

Requiere dos cosas que hoy faltan: que la conexión recuerde su token, y que la
identidad del emisor llegue hasta `Route` por todos los caminos (la cola por
sesión, los Expertises en WASM, los nodos fijados).

**Cuánto vive una entrada.** Se desaloja por última referencia: cada emisión o
petición que la nombra la renueva. Solo quedan fijadas las que tienen una
petición de inferencia en curso, y esas están acotadas por la cola. El tope es
en bytes, con un contador visible de cuántas entradas referenciadas se
desalojaron.

**Lo que se pierde, y cuándo:**

- Si una entrada se desalojó: el plazo se trata como ausente y el enlace como no
  verificado. La prioridad se recupera del grafo, porque es de la arista; para
  eso la petición indica también sesión, nodo y puerto.
- Con varias entradas a un nodo, el linaje sigue la entrega que disparó el
  handler; las demás no quedan en el camino.
- Tras un reinicio o un relevo, el linaje se reconstruye desde el registro
  causal: entrega anterior, destino y plazo están en los envelopes guardados.
  Exige quitar el tope de 1000 eventos que hoy tiene la reanudación.

### 5.2 El camino de una petición

```
 Expertise ◀──ws── executor              (entrega D)
     │                 ▲
     │ HTTP: token + D │ linaje, cancelaciones, memoria
     ▼                 │
 plano de inferencia ──┴── cola ──▶ driver ──▶ motor (vLLM / llama-server)
     │
     └── registros observados ──▶ tabla del kernel · registro causal · Pulse
```

Rutas compatibles con OpenAI (`/chat/completions`, `/embeddings` y la de rerank
del motor) en el listener del nodo, detrás de la autenticación que ya tiene.
Se añaden a las que puede alcanzar un token acotado. `AURA_INFERENCE_URL` pasa
a apuntar ahí.

**Por qué dentro del proceso del nodo.** La revisión 2 lo sacaba a un proceso
aparte para que un relevo del kernel no cortara las generaciones. No las salva:
el Expertise pierde su conexión con el kernel, su `emit` falla y él mismo
detiene la generación. Sin esa ventaja, un proceso aparte solo añade costes:
una credencial y un protocolo nuevos, quién lo arranca y quién lo adopta, y dos
dueños para la misma memoria. Lo que sí se conserva es la frontera: el paquete
habla con el executor por una interfaz estrecha y explícita (resolver una
entrega, recibir cancelaciones y retenciones, pedir memoria, escribir un
registro, cargar un presupuesto, consultar la política). Si algún día hace falta
supervisar motores en otra máquina, esa es la costura.

**Lo que cuesta estar en el mismo proceso:**

- Los handlers de streaming son de larga vida. Al recibir la señal de parada,
  el nodo cancela las peticiones en curso **antes** de esperar a los handlers;
  si no, cada parada agotaría los 10 s de espera, que es también el plazo de
  gracia habitual de un contenedor, y el proceso moriría antes de cerrar el
  almacén.
- El servidor HTTP de Go recupera un pánico dentro de un handler, no dentro de
  una goroutine que el paquete lance. Todas van envueltas, como ya hace el
  executor con las suyas.

**Cómo se sabe de quién es una petición.** Lleva el token del Expertise y la
entrega en la que trabaja: sesión, nodo, puerto e identificador. El plano de
inferencia consulta el linaje (5.1). Solo vale si esa entrega se hizo a la
conexión de ese token. Con el token de operador o con `--no-auth`, esa
comprobación la pasa cualquier conexión.

- No hay nada que firmar, que caduque a mitad de un handler largo ni que acabe
  en el registro.
- Una petición sin entrega (indexado de memoria, consolidación) entra en la
  clase `background`.
- Hay un tope de peticiones simultáneas por entrega.

**Lo que esto garantiza, y cuándo:**

| Despliegue | Garantía |
|---|---|
| Por defecto hoy (token de operador en cada Expertise) | Ninguna frente a un Expertise hostil. Sirve para planificar bien, no como frontera. |
| Tokens acotados + motor solo alcanzable por el nodo | Un Expertise solo obtiene la prioridad y el plazo de una entrega que de verdad se le hizo, y no puede saltarse la cola. |

El segundo modo es requisito previo de cualquier afirmación de seguridad. El
límite de fondo sigue siendo el del README: los Expertises de código fuente no
están aislados.

**Prioridad.** La declara el grafo en sus aristas, y cualquiera con acceso al
plano de control puede registrar un grafo. Se añade un techo por política del
nodo: qué prioridad máxima puede pedir cada grafo.

**Cancelación.** El executor avisa en los tres sitios donde hoy se decide:
cancelación del cliente, especulación descartada y fin de sesión. El plano de
inferencia cierra la conexión con el motor; vLLM aborta al detectar la
desconexión. Una petición que llega para una cadena ya cancelada se rechaza al
resolverla.

**Modos:**

| Modo | Quién arranca el motor | Qué ve el nodo |
|---|---|---|
| `external` (hoy) | Otro | Nada. Sigue soportado. |
| `attached` | Otro (systemd, contenedor) | Tráfico, uso y lo que el motor reporte de sí mismo. |
| `owned` | El nodo | Todo, más ciclo de vida, flags y memoria. |

**Compatibilidad.** Un SDK antiguo no envía credencial a la URL de inferencia;
contra el nodo recibiría un 401. Esos despliegues siguen en `external` hasta
actualizar el SDK.

**Coste a medir:** un salto por fragmento de streaming, en el mismo proceso que
enruta los envelopes. Hipótesis: despreciable frente a los milisegundos por
token. No se da por buena sin medirla (fase 1).

### 5.3 Drivers

Un driver sabe, para una versión fijada de un motor: cómo lanzarlo, cómo
comprobar su salud, qué nombres tienen sus métricas, qué campos añadir a una
petición y cómo leer el uso al final del stream.

| Capacidad | vLLM | llama-server | Uso |
|---|---|---|---|
| Prioridad nativa | Cola, valor bajo primero | No | Segundo nivel tras la cola del nodo |
| Plazo | No | No | Lo pone el nodo |
| Varios modelos | No (un proceso por modelo) | Sí, modo enrutador | Paginación |
| Afinidad de caché | Automática, por bloques | `id_slot` | Enrutado por prefijo |
| Instantánea KV | No; descarga a RAM/disco | `slots save/restore` | 6.4 |
| Adaptadores LoRA en caliente | Sí | Limitado | Varios comportamientos, un modelo base |
| Embeddings y rerank | Sí | Sí | Memoria |
| Caché KV cuantizada | `--kv-cache-dtype fp8…` | `-ctk/-ctv q8_0…` | VRAM |
| Sal de caché por petición | `cache_salt` | No | Aislamiento entre ámbitos |
| Plataforma | Linux, CUDA/ROCm | Todas | — |

**Cómo se toca una petición.** El driver añade o cambia claves de primer nivel
(`priority` invertida para vLLM, `id_slot`, `cache_salt`, `max_tokens`) y
fusiona la opción de incluir uso dentro de `stream_options`. Reglas:

- El cuerpo se edita por posición de bytes, sin volver a serializarlo.
  Reserializar en Go compacta espacios y escapa caracteres, y los bytes de
  `messages` y `tools` deben llegar al motor intactos (6.5).
- Un cuerpo con claves de primer nivel repetidas se rechaza. Si no, el nodo y
  el motor podrían leer valores distintos y colarse una `priority`.
- El tamaño del cuerpo tiene tope.
- Cada registro observado dice qué claves tocó el driver.

SGLang o TensorRT Edge-LLM entran después como drivers nuevos.

### 5.4 Prioridad, plazo y desalojo

**La cola vive en el nodo.** Se admite hacia el motor solo lo que este ejecuta
a la vez, y se ordena lo que espera.

**Clases.** La prioridad numérica del grafo se agrupa en cuatro clases por
umbrales de configuración: `background`, `ordinary`, `elevated`, `critical`.
Dentro de una clase se ordena por plazo más cercano y después por llegada. Para
que las peticiones sin plazo no mueran de hambre, envejecen: tras una espera
configurable compiten como si tuvieran plazo.

Tres mecanismos, del más simple al más delicado:

1. **Carril reservado.** Parte de la capacidad queda fuera del alcance de las
   clases bajas. En llama.cpp es un slot fijado a la cadena crítica, que además
   conserva su prefijo caliente. En vLLM es margen de secuencias y de caché KV.
   En `attached` la capacidad la declara el operador; en `owned` la conoce el
   nodo porque puso los flags.
2. **Plazos que hacen algo.** Antes de admitir se compara el tiempo restante
   con la espera estimada más el tiempo al primer token observado.
   - Si no llega, se responde un error tipado de inmediato y el Expertise toma
     su camino degradado.
   - Si llega justo, se acota `max_tokens` según la velocidad medida.
   - Al vencer el plazo, se corta la petición en el motor.

   Lo que **no** resuelve: qué se ejecuta en su lugar. C2 no tiene una arista de
   reserva, así que degradar sigue en manos de cada Expertise.
3. **Desalojo.** Cuando una petición crítica no cabe ni en el carril, se cancela
   una de clase inferior y se reencola.
   - Seguro si la víctima aún no emitió su primer token.
   - Si ya emitía, reanudar exige continuar desde el texto parcial, lo que
     cambia el muestreo. Queda como experimento explícito, no por defecto.

El desalojo dentro del lote es el hueco de vLLM (#40004). Ahí la solución buena
es la de upstream; la del nodo es la red de seguridad.

### 5.5 Memoria de acelerador

**Un solo dueño.** La admisión del kernel pasa de un escalar por conexión a un
fondo por dispositivo con titulares con nombre: conexiones de Expertise e
instancias de motor. El plano de inferencia pide y devuelve concesiones, y
lleva la política. En Jetson la memoria es unificada: RAM y acelerador son el
mismo fondo.

- **Cada instancia de motor declara su reserva:** pesos más un presupuesto de
  caché KV explícito. En vLLM siempre con `--kv-cache-memory-bytes`; su valor
  por defecto toma toda la GPU.
- **Estimado y medido.** `model-fit` estima antes de cargar (hoy solo conoce
  cuantizaciones GGUF; se amplía a W4A16, FP8 y AWQ y a caché KV cuantizada).
  Tras cargar se registra lo medido y la diferencia queda visible.
- **Clases de residencia:** `pinned` (nunca se descarga), `warm` (se descarga
  por antigüedad de uso) y `on-demand`.
- **Paginación:** en el borde, el modo enrutador de llama.cpp. En vLLM, parar y
  arrancar procesos; un *sleep* utilizable depende de upstream.
- **Quién espera importa.** Una carga pedida por una cadena crítica puede
  descargar un modelo `warm`, nunca uno `pinned`.

**Límite.** Los Expertises que cargan sus propios pesos (ASR, TTS) no se pueden
descargar: no existe un mensaje de C3 para pedírselo. Declaran su memoria con
un campo opcional nuevo en **C1** y el fondo la descuenta mientras estén
conectados; son `pinned` de hecho. La salida a medio plazo es servirlos también
desde un motor.

Receta de partida por tamaño de GPU (síntesis de la investigación, **sin medir**
en hardware propio):

| GPU | Motor | Modelo principal | Caché KV | Voz | Embeddings |
|---|---|---|---|---|---|
| 8 GB | llama.cpp | Un VLM de 2–4B, Q4_K_M con imatrix | 4–8K de contexto, `q8_0` | ASR pequeño; TTS en CPU | En CPU; sin reordenado |
| 16 GB | vLLM o llama.cpp | Un VLM de 7–9B, W4A16 o Q4_K_M | 2–3 GB fijos | ASR tipo Whisper-turbo; TTS < 1 GB | En GPU |
| 24 GB | vLLM | Un VLM de 12–14B W4A16, o 8B en FP8 | 4–6 GB, FP8 | Ambos en GPU | En GPU, con reordenado |

Regla general: un solo modelo multimodal para ver y razonar en vez de dos.

### 5.6 Ciclo de vida, cambio de modelo y relevo

**Arranque.** El nodo anota en el directorio de datos qué va a arrancar (puerto
e identidad) **antes** de lanzarlo, lanza el motor con una clave propia de esa
instancia y escribe sus logs a fichero.

- La clave se pasa por entorno o por fichero, nunca como argumento, y no forma
  parte de la identidad.
- **Adopción.** Si la anotación existe, el nodo comprueba que en ese puerto
  responde un motor que acepta esa clave. Un número de proceso vivo no prueba
  nada: los números se reutilizan. Si responde pero no está sano, espera; no
  lanza un segundo.
- La clave queda en el directorio de datos, legible por un Expertise que corra
  como el mismo usuario. Es la misma exposición que hoy tiene `node.token`.
- Una parada ordenada detiene los motores propios, salvo que se configure
  dejarlos para la adopción.

**Relevo.** Lo que pasa cuando el kernel muere y el respaldo toma el mando:

| | Qué ocurre |
|---|---|
| Generaciones en curso | **Se pierden.** Ya se pierden hoy: el Expertise no puede emitir sin su conexión. |
| Cola y concesiones | Se pierden; están en memoria. El linaje se reconstruye desde el registro (5.1). |
| El motor, sus pesos y su caché | Sobreviven **si el despliegue no mata al motor con el nodo**. Bajo systemd o en un contenedor, por defecto muere con él. Para que sobreviva, el motor corre en su propia unidad (`attached`). Ahí hay que elegir: `attached` sobrevive al relevo pero no admite instantáneas KV (6.4), que exigen `owned`. |
| Tiempo hasta volver a servir | Con motor adoptado: el del relevo más la reconexión de los Expertises (su espera inicial es de 1 s). Sin él: lo que tarde en cargar el modelo, de segundos en servidor a minutos en Jetson. |

No hay un segundo acelerador en el mismo host que permita un motor de respaldo
caliente.

**Cambio de modelo.** Cargar al lado si hay memoria, o drenar y cambiar si no,
con vuelta atrás. Para decidir con datos hace falta repetir sesiones grabadas
contra la instancia candidata, y `aura regress` hoy solo repite contra el nodo
en vivo. Dirigir una repetición a una instancia concreta es trabajo de la fase
3; sin él, el criterio del cambio es manual.

### 5.7 Registros observados

Hoy el ledger dice "quién afirmó qué". El nodo añade lo que vio, en dos
registros:

- **Admisión**, cuando la petición entra: instancia del motor, modelo, entrega a
  la que pertenece, prioridad y plazo aplicados, claves que tocó el driver, y
  el hash del manifiesto de contexto (6.5), que se guarda aparte. Existe antes
  del primer token.
- **Finalización**, al terminar: uso reportado por el motor, espera en cola,
  tiempo al primer token, tokens servidos de caché, resultado (completada,
  cancelada, desalojada, rechazada por plazo).

**Dónde viven.** En una tabla del kernel direccionable por hash, que solo
escribe el nodo y que está aparte de la de atestaciones: un Expertise no puede
producir un registro observado. Además se anota un evento en el registro causal
de la sesión para la línea de tiempo, reutilizando el tipo `status` y sin
destino, para no alterar el recuento que hace la reanudación. Las peticiones
`background` no tienen sesión: solo van a la tabla. Un registro se conserva
mientras lo cite una entrada del ledger; los no citados caducan tras una ventana
configurable.

**Por qué dos.** Un efecto puede sellarse a mitad de un stream (la voz es una
Expertise `motor`). En ese momento el uso aún no existe, pero la admisión sí.

**Cómo llega la cita al efecto.** Hacia delante, por el linaje (5.1):

1. Al admitir una petición para la entrega D: se guarda el registro, se anota
   su hash en D y solo entonces se envía al motor. Con ese orden, ningún token
   puede reenviarse antes de que la cita exista.
2. Cuando una emisión se reenvía, la entrega nueva hereda las citas de la causa
   que declara **y además** las admisiones abiertas o recientes de la conexión
   que emite, en esa sesión.
3. Al sellar un efecto, sus citas ya están en su entrada. No hay que recorrer
   nada hacia atrás.
4. Si la entrega queda retenida en una puerta, sus citas se escriben en un
   evento del registro causal en ese momento. Así llegan al sello aunque pasen
   horas o haya un relevo, y también al sello de una denegación.

**Por qué seguir también al emisor.** Comprobar la causa no basta. Una
Expertise puede pedir una inferencia bajo una entrega y emitir bajo otra, las
dos suyas y las dos válidas: la cita se perdería con el enlace marcado como
bueno. El nodo sabe de qué conexión vino cada admisión, así que no depende de
lo que el Expertise declare. La cita se marca como exacta solo cuando no hay
ambigüedad; si no, como atribuida al emisor.

**Tope.** El conjunto conserva las citas más recientes y dice cuándo está
truncado. Como va por camino y no por raíz, un stream largo no acumula a sus
hermanos; un bucle de agente de una hora sí alcanza el tope, y pierde las más
antiguas.

**En el ledger.** Dos campos nuevos que solo escribe el kernel (**C4,
aditivo**): las admisiones citadas, y cómo de firme es cada una (exacta,
atribuida al emisor, enlace sin verificar, conjunto truncado). El recibo
incluye las admisiones citadas; el manifiesto va por su hash. La finalización,
si ya existe, se adjunta como información: el hash de la entrada no la cubre.

**Identidad de la instancia.** En `owned`: motor, versión, digest del fichero
del modelo, cuantización y flags de arranque. En `attached`: solo lo que el
motor reporte de sí mismo, marcado como tal. Cada admisión cita el hash de esa
identidad.

`aura why` y `aura receipt` muestran lo afirmado y lo observado, y señalan la
discrepancia si no coinciden. El límite sigue siendo honesto: prueba qué
instancia sirvió la petición, no que los pesos sean lo que su nombre dice.

### 5.8 Fuera de alcance

- **Modelos de acción.** No hablan la API de OpenAI. El supervisor y el fondo de
  memoria admiten un tipo de motor "política" más adelante, cuando exista el
  puente a ROS 2 y el camino binario de frames.
- **Motores en otra máquina, supervisados.** `attached` ya permite usarlos; que
  el nodo los arranque exige un agente en esa máquina.
- **Que una generación sobreviva a un relevo.** Exigiría que el SDK guardara lo
  que no pudo emitir y un orden de reconexión definido.

---

## 6. Plano de contexto

No hay una base de datos que resuelva el contexto. La palanca es quién arma el
prompt y dónde aterriza.

### 6.1 Segmentos por volatilidad

Una librería del SDK (`aura.context`) arma el prompt con segmentos tipados, del
más estable al más volátil:

| Orden | Segmento | Cambia |
|---|---|---|
| 1 | `system` | Con la versión del Expertise |
| 2 | `catalog` (herramientas, capacidades) | Con la revisión del grafo |
| 3 | `knowledge` (misión, mapa, manual) | Con la misión |
| 4 | `memory` (lo recuperado) | Por consulta; se añade al final, no se reordena |
| 5 | `history` | Solo crece; se recorta en bloques |
| 6 | `live` (observación actual, hora, pose) | Siempre |

Reglas: serialización determinista, ninguna marca de tiempo ni identificador en
los segmentos 1–3, y nada se inserta en medio.

Un límite que no controla el SDK: dónde coloca las herramientas la plantilla de
chat del modelo. El orden 1–2 es el de la petición, no necesariamente el del
prompt final.

### 6.2 Enrutado por prefijo

El nodo ve todas las peticiones, así que sabe qué prefijo tiene cada slot o
instancia, y envía cada una a donde la coincidencia es más larga. El motor
reporta los tokens servidos de caché; la diferencia con lo previsto es la
métrica. Pulse ya muestra la tasa de aciertos; pasa a mostrarla por grafo y por
Expertise.

### 6.3 Retención guiada por el grafo

El executor sabe qué sesiones tienen una entrega retenida en una puerta de
aprobación, que puede tardar minutos. Con ese dato, el plano de inferencia:

- mantiene caliente la caché de esas sesiones, con caducidad;
- descarga primero lo que más tarde volverá a ejecutarse.

Es el único estado de espera que el executor conoce hoy; no sabe cuándo una
Expertise espera una herramienta.

El nodo no ve un prompt antes de la petición, así que **no precalienta nada**.
Precalentar lo hace el SDK: al recibir una entrega especulativa (las que C2
permite sobre salida parcial), envía sus segmentos estables para que se evalúen
antes de tener el resto.

La evidencia es de artículos (1,6–2,2×, autores) que predicen el grafo. La
ganancia propia está sin medir.

### 6.4 Instantáneas KV

Para contextos largos, estables y calientes (el informe de misión, el manual de
operación): se evalúa una vez, se guarda el estado del slot, y se restaura en
vez de releer. Exige modo `owned`.

- **Clave:** identidad de la instancia (5.7) más el hash encadenado de los
  elementos del prefijo (6.5). No hace falta tokenizador. Si cambia el modelo,
  la cuantización, un flag o un byte del prefijo, no coincide y se recalcula.
- **Varias sesiones parten de la misma instantánea.**
- Primero en llama.cpp. En vLLM el equivalente es la descarga de caché a RAM o
  disco; se declara como capacidad distinta.
- Crear y restaurar una instantánea queda en los registros observados de las
  peticiones que la usan.

Límites:

- Solo prefijo. Dos instantáneas no se combinan.
- No se edita: si el manual cambia, se recalcula entera.
- Ocupa mucho (del orden de cientos de MB por cada 4K tokens).
- En llama.cpp la reutilización de caché no es determinista bit a bit; el
  registro de finalización guarda cuántos tokens vinieron de caché para poder
  atribuir una diferencia al repetir.
- Que un proyector de visión bloquee el guardado del slot está **sin verificar**.

### 6.5 Manifiesto de contexto

**Qué es.** La descripción de lo que entró en la ventana de una petición. Tiene
dos partes, que se guardan y se muestran separadas:

| Parte | Quién la produce | Contenido |
|---|---|---|
| Observada | El nodo | El hash de cada elemento de `messages` y de `tools`, en orden |
| Afirmada | El Expertise | A qué segmento pertenece cada elemento y de qué registros o eventos salió |

**Qué se hashea.** Los bytes JSON de cada elemento **tal como llegaron**, sin
canonicalizar (el mismo criterio que C5). El nodo los recorta del cuerpo
recibido y los reenvía sin tocarlos (5.3), así que lo que hashea es lo que el
motor recibe.

**Cómo viaja la parte afirmada.** En una clave de primer nivel del cuerpo, que
el nodo retira antes de reenviar.

**Qué comprueba el nodo.** Que la parte afirmada cubra cada elemento una vez y
solo una. Si no cuadra, la petición se sirve igual y la admisión lo dice.

El hash del manifiesto va en el registro de admisión, y por ahí queda citado
por los efectos (5.7). `aura why <efecto>` pasa a responder: qué instancia, con qué
elementos, qué dijo el Expertise que eran y de dónde venían, y cuántos tokens
fueron nuevos o de caché.

**Lo que compromete y lo que no:**

- Compromete lo que el nodo reenvió al motor, no lo que el modelo "vio" tras
  aplicar la plantilla.
- Que un elemento sea "memoria" o venga de tal evento lo dice el Expertise; el
  nodo no lo comprueba.
- Una imagen enviada por URL compromete la URL, no los píxeles. Para que
  signifique algo, las imágenes van incrustadas o con su digest.
- Registra qué había en la ventana, no qué usó el modelo. Atribuir uso exige
  ablaciones lentas; cabría para investigar un incidente, no en cada decisión.
- El contenido solo es reconstruible mientras se conserven sus fuentes.

No existe un atributo estándar para esto en las convenciones GenAI de
OpenTelemetry. Proponerlo encaja con los borradores que el repositorio ya escribe.

### 6.6 Edición que respeta la caché

- **Enmascarar observaciones viejas** en los puntos de compactación, por
  bloques, dejando una referencia que el modelo puede volver a pedir. El
  prefijo solo se rompe ahí, como ya hace `compact_history`.
- **Catálogo diferido:** el segmento 2 lista las capacidades de forma compacta;
  el esquema completo se carga al usarlo.

### 6.7 Presupuesto en tokens reales

Campo nuevo y opcional en **C2**, `token_budget`, cobrado del uso observado.
`context_budget` (bytes/4) conserva su significado.

Coste = tokens de entrada nuevos + α · tokens de caché + β · tokens de salida +
γ · tokens-segundo retenidos en caché.

El último término importa: sin él, premiar los aciertos de caché premia no
recortar nunca. α y β se calibran en la fase 0b contra tiempo de GPU medido. γ
no se puede observar hasta que el nodo sea dueño del motor: se calibra en la
fase 4b y hasta entonces vale cero.

---

## 7. Plano de memoria

### 7.1 Modelo

Una **Expertise nuevo** de Python (capacidades `memory.remember` y
`memory.recall`). `memory.context.window` sigue existiendo sin cambios.

- **El almacén de la memoria es primario.** Guarda registros compactos
  (transcripción, descripción, decisión, efecto, hecho), cada uno con el
  identificador y el digest del evento del que salió. Sobreviven a la poda del
  registro causal.
- **Los índices son proyecciones** de ese almacén: se tiran y se reconstruyen.
  Cambiar de modelo de embeddings es reconstruir, no migrar.
- **Los hechos consolidados por LLM no son reproducibles.** Se guardan como
  registros, con cita a sus fuentes; no se recalculan.

**Cómo se alimenta.** Por cableado del grafo: recibe lo que el autor le enruta,
y cada envelope trae su causa. Mínimo privilegio y funciona con el kernel de hoy.

**Alternativa aplazada: seguir el registro.** Indexar todo lo que pasa por un
grafo sin cablearlo exige trabajo de kernel que no es pequeño:

- una lectura global con cursor (segmento, desplazamiento);
- un error explícito cuando la poda adelanta a un lector lento;
- una marca de hasta dónde está volcado a disco;
- un reparto a varios consumidores, porque el único hueco de observador está ocupado;
- un permiso en el token y en la política del nodo. El `permissions` de C1 lo
  declara el propio Expertise y no se aplica a los de código fuente.

Solo se hace si la evaluación muestra que el cableado no basta.

### 7.2 Almacenamiento

Un fichero SQLite por nodo, con cuatro piezas que ya existen:

| Pieza | Para qué |
|---|---|
| FTS5 | BM25 sobre el texto |
| `sqlite-vec` v0.1.9 | Vectores: barrido binario y reordenado con int8 o float |
| Tabla de aristas + CTE recursivas | El grafo |
| R\*Tree | Posición y tiempo |

**El grafo es gratis y exacto.** Aristas: `caused_by`, `same_trace`,
`next_in_session`, `near` (espacial), `derived_from` (un hecho y su origen) y
`supersedes`. No hay base de grafos ni extracción de entidades por LLM en el borde.

**Dos tiempos por registro:** cuándo ocurrió y cuándo se supo. Un hecho que deja
de ser cierto se invalida, no se borra.

**Escala, con aritmética y sin medir:** 1M de vectores de 256 dimensiones ocupa
32 MB en binario y 256 MB en int8. El límite práctico de fuerza bruta en un
Orin o una Pi 5 hay que medirlo; la referencia externa es "pocos millones".

**En servidor**, el mismo Expertise con Postgres y pgvector hasta decenas de
millones; más allá, Qdrant o LanceDB. No se construye hasta que un despliegue
lo necesite.

**Riesgo a comprobar:** que el `sqlite3` de Python permita cargar extensiones en
todas las plataformas objetivo. Alternativa: `usearch` o LanceDB embebido.

### 7.3 Ingesta, sin LLM en el camino caliente

1. Prefijo contextual determinista a partir de los metadatos del evento.
2. Indexado en FTS5.
3. Embedding, por lotes. En cuanto exista la cola (fase 2), en la clase
   `background`; antes compite como cualquier otra petición.
4. Aristas.

Percepción: descripciones de fotogramas clave disparadas por eventos, no cada
fotograma. Las revisitas se fusionan por cercanía espacial y semántica.

**Modelos por defecto** (Apache-2.0 ambos): Qwen3-Embedding-0.6B, de dimensiones
recortables, y bge-reranker-v2-m3. EmbeddingGemma queda bajo los términos de
Gemma; los modelos de Jina son no comerciales. El modelo de embeddings y su
revisión se guardan con el índice: si cambian, se reconstruye.

### 7.4 Recuperación

```
filtro (tiempo, sesión, posición, tipo, vigencia)
   → BM25 top-100  ∥  vectores top-100
   → fusión por rangos (RRF)
   → expansión 1–2 saltos por aristas causales y temporales
   → reordenado con cross-encoder a 5–10
   → empaquetado bajo presupuesto, con cita a evento
```

**Con plazo.** Si no hay tiempo: sin reordenado → solo BM25 → último resultado
en caché.

### 7.5 Consolidación

Asíncrona, en clase `background` y desalojable: hechos extraídos por LLM
guardados **junto a** los originales. Con gramática de decodificación, para que
un modelo pequeño no pueda emitir un hecho mal formado. Es cara (en la medición
de Redis, 177 veces más tiempo de ingesta), así que en el borde solo corre con
el acelerador ocioso, lo que exige el fondo de memoria.

### 7.6 Evaluación

Los benchmarks públicos no sirven de árbitro. Se construye un conjunto propio a
partir de sesiones grabadas; como cada registro cita su evento, la respuesta
correcta ("qué evento contesta esto") sale barata.

Se compara **a presupuesto de tokens igualado**: contexto completo (cuando
cabe), descartar lo más viejo (hoy), solo BM25, híbrido, híbrido con expansión,
y con reordenado. Una capa entra solo si supera a la anterior. La expansión
causal es la idea más nueva y la de evidencia más débil: si no gana, no entra.

### 7.7 Exposición por MCP

MCP no define memoria ni citas. Los registros recuperados se exponen como
recursos con URI estable por evento, con puntuación y contexto de traza en
`_meta`. Es una convención propia, no un estándar.

---

## 8. Seguridad y privacidad

**Ámbito** significa aquí el alcance de política de un grafo. El kernel lo
conoce para cada sesión; un Expertise no, porque el envelope no dice a qué
grafo pertenece.

| Riesgo | Medida |
|---|---|
| La caché KV compartida entre inquilinos filtra prompts: un atacante los reconstruye observando cambios en el orden de servicio (PromptPeek, NDSS 2025). | Slots e instantáneas nunca se comparten entre ámbitos. En vLLM, `cache_salt` por ámbito. llama.cpp no tiene equivalente: ahí el aislamiento es por slot. |
| Un Expertise se salta la cola hablando directo con el motor. | En `owned`, el motor solo acepta la clave del nodo. En `attached` es responsabilidad de quien lo despliega, y se dice. |
| Un Expertise toma prestada la prioridad de otra cadena. | Tokens acotados (5.2). Con el token de operador no hay defensa. |
| Un Expertise desliga su inferencia del efecto declarando otra causa. | Comprobar la causa no basta, porque puede declarar otra entrega suya. Las citas siguen también a la conexión que emite (5.7), y el ledger dice cómo de firme es cada una. |
| Una instantánea KV contiene el prompt. | Misma protección en reposo que los secretos del nodo. |
| Recuperar de la memoria lo que no corresponde. | **Sin resolver dentro de un proceso.** Hoy el aislamiento solo puede ser un proceso de memoria por espacio, con su propio token. Aislar por grafo dentro de uno exige que el kernel le diga al Expertise en qué grafo está. |
| Envenenamiento de memoria desde entradas no confiables. | El origen queda marcado; todo hecho derivado cita su fuente, así que una fuente mala se rastrea y se purga. |
| Borrado. | En la memoria, el registro se elimina de verdad y se reconstruyen los índices. **En el registro causal no hay borrado por registro:** solo desaparece al podarse su segmento. |
| El plano de inferencia como punto único. | Cae con el nodo; el respaldo lo recupera con los límites de 5.6. |

---

## 9. Lo que no se hace

| Idea | Por qué no |
|---|---|
| Motor de inferencia propio | Años de trabajo para igualar lo que ya existe. |
| Fork de vLLM | Cadencia quincenal; deuda permanente. |
| Tickets de inferencia firmados | Con el token de operador en cada Expertise se podrían fabricar; acabarían en el registro causal; caducan a mitad de un handler largo. |
| La pasarela como proceso aparte | No salva las generaciones en un relevo, y obliga a una credencial, un protocolo y un ciclo de vida nuevos, y a dos dueños de la memoria. |
| Guardar el plazo por raíz causal | Un stream entero comparte raíz: todo lo posterior al primer plazo nacería vencido. |
| Reutilizar el índice de atestaciones para lo observado | Se liga por raíz, con tope de 16, y admite lo que envíe un Expertise. |
| Recorrer las causas hacia atrás al sellar | Una entrega retenida horas habría perdido su camino. Las citas viajan hacia delante. |
| Base vectorial propia | Resuelto por otros. |
| Base de grafos | El grafo exacto ya está en las causas. Kuzu archivado; su fork es pre-1.0. |
| Grafo de entidades por LLM en el borde | Una o más llamadas por registro; se degrada con modelos pequeños; casi sin ventaja a presupuesto igualado. |
| Compresión de prompts | Rinde poco en agentes y rompe la caché de prefijo. |
| Mezcla de caché KV fuera del prefijo | Pierde 7–18 % de F1. |
| Cartridges, Doc-to-LoRA, Titans | Investigación; coste de entrenamiento; no componen. |
| Modelos de lenguaje recursivos a profundidad > 1 | La única reproducción halló que empeora y encarece. |

---

## 10. Tendencias de GitHub: qué se adopta

De un barrido de lo destacado entre mediados de 2026 y hoy. Solo las tres
primeras filas tocan este plan; el resto son candidatos de roadmap.

| Proyecto | Licencia | Dónde encaja | Estado |
|---|---|---|---|
| `quic-go` v0.65 (el repo usa v0.61) | MIT | Varios datagramas por paquete, y datagramas que ya no dejan sin turno a los ACK: mejora directa para la clase `realtime`. | Adoptar |
| `sigstore/model-transparency` | Apache-2.0 | Verificar la firma de un modelo antes de cargarlo. Solo Python, poca actividad. | Vigilar |
| Gemma 4 E2B, Qwen3.5 0.8B | Apache-2.0 | Modelos por defecto para el borde. | Evaluar en fase 3 |
| MCP `2026-07-28` | — | Reescritura sin estado; el repo implementa `2025-06-18`. `input_required` es la forma en el cable de una aprobación pendiente. | Deuda fuera de este plan |
| `sherpa-onnx` | Apache-2.0 | Voz en el borde con bindings de Go y arm64. | Candidato |
| Zenoh 1.10 + `rmw_zenoh` | Apache-2.0 (elegir) | El puente a ROS 2, como proceso aislado. | Candidato |
| MCAP + Rerun | MIT / Apache-2.0 | Exportar el registro causal y los sensores. | Candidato |
| LeRobot v0.7, GR00T N1.7 | Apache-2.0 / pesos NVIDIA | Servidores de política como Expertise. | Tras el puente a ROS 2 |
| `cedar-go` | Apache-2.0 | Lenguaje de política en la puerta de aprobación. | Evaluar |
| AG-UI 1.0 | MIT | Que interfaces de terceros muestren ejecuciones e interrupciones. | Evaluar |

**Competencia.** El más cercano es `dimensionalOS/dimos` (Python, catálogo de
hardware, transportes Zenoh/ROS 2, simulación). Su README no muestra puerta de
aprobación, ledger, aislamiento, atestación, plazos ni respaldo. `OpenMind/OM1`,
con runtime en Go y Zenoh, se archivó el 14 de noviembre de 2026 con 996 forks.

Lo que ellos tienen y AURA no: adaptadores de hardware, simulador con
grabación, un transporte vivo a ROS 2 y benchmarks públicos.

**Licencias que no pueden entrar en la edición Apache-2.0:** YOLOE (AGPL),
Depth Anything 3 Large/Giant (no comercial), SAM 3 y FastVLM (licencias propias
o de investigación), Moondream 3 y Restate (BSL), FalkorDB (SSPL), VectorChord
(AGPL/ELv2), modelos de Jina (no comercial).

---

## 11. Fases

Tamaños para una persona, **estimados**: M 2–4 semanas · L 1–2 meses. Dos fases
M, una entre M y L y cuatro L suman entre 6 y 12 meses-persona, sin contar la
pista con vLLM. Es una estimación sin historial propio que la respalde.

### Fase 0a — Linaje y plazos (M–L) — **hecha**

Solo kernel y SDK. Es la base de todo lo demás y corrige defectos que existen
con independencia de este plan. Implementada en `fase-1/paso-por-el-nodo`,
junto con la fase 1; el
detalle de lo construido y lo medido está en la [sección 15](#15-fase-0-lo-construido-y-lo-medido).

- El linaje de entregas (5.1), con su retención y su reconstrucción al reanudar
  (sin el tope de 1000 eventos).
- La identidad del emisor hasta `Route` por todos los caminos, y el token en la
  conexión.
- Las cuatro reglas del plazo, cada una con su test; entre ellas, comprobar el
  vencimiento antes de sellar.
- Esquema y SDK: `deadline` y `priority` en `envelope.schema.json`; plazo,
  prioridad e identificador de entrega en el contexto del handler (Python y Node).

**Sale con:** los plazos heredándose de verdad, sin regresión en streaming y
sin efectos sellados y descartados; el coste del linaje por reenvío, medido con
el banco que ya existe.

### Fase 0b — Medidas (M) — **banco construido, sin medir**

- Banco de carga con forma de robot: una cadena prioritaria corta y frecuente
  con plazo, más N generaciones largas. Contra `llama-server` y contra vLLM en
  el contenedor Linux.
- `ChatBackend` envía `priority` a vLLM, solo para reproducir #40004. El plazo
  se aplica en el cliente (corte y recorte de `max_tokens`): ningún motor lo lee.
- Confirmar en la versión fijada de vLLM: tokens de caché en streaming, límites
  de prioridad.
- Calibrar α y β de `token_budget` contra tiempo de GPU.

**Sale con:** una tabla de números propios y la reproducción de #40004. No
depende de 0a; pueden ir en paralelo.

### Fase 1 — Paso por el nodo (M) — **hecha**

Implementada en `fase-1/paso-por-el-nodo`. Lo construido y lo medido está en la
[sección 16](#16-fase-1-lo-construido-y-lo-medido).

Proxy sin cola. Rutas de inferencia alcanzables con token acotado; el SDK envía
token y entrega; el nodo resuelve por el linaje, fuerza el uso en el stream, lo
mide por grafo en Pulse y corta en el motor al cancelar y al parar. Modo
`attached`. No cambia C1–C5; sí el executor, la tabla de rutas por alcance y
los dos SDK.

**Sale con:** sobrecoste por fragmento; tiempo desde `cancel` hasta motor
libre; una parada ordenada durante una generación que termina dentro de la
espera; uso por grafo visible.

### Fase 2 — Cola, prioridad y evidencia (L)

Clases, orden por plazo, envejecimiento, rechazo por plazo, carril reservado
(con `id_slot` en llama.cpp), desalojo antes del primer token. Tabla de
registros observados, citas hacia delante con atribución al emisor, citas de
las entregas retenidas en el registro, **C4 aditivo**. Techo de prioridad por
política.

**Sale con:** primer token de la cadena crítica bajo saturación frente a sin
carga; un efecto retenido en una puerta y aprobado tras un relevo cuyo `aura
why` muestra la inferencia observada.

### Fase 3 — Motores propios y memoria de acelerador (L)

Supervisor con adopción, fondo por dispositivo con titulares (**C1 aditivo**),
clases de residencia, paginación, identidad completa de instancia, repetición
contra una instancia candidata, cambio con vuelta atrás, `model-fit` para
formatos no GGUF.

**Sale con:** un dispositivo de referencia con visión, lenguaje y voz
conviviendo dentro del presupuesto, medido; tras un relevo, tiempo hasta la
primera inferencia servida, con y sin motor adoptado.

### Fase 4 — Plano de contexto (L)

- **4a** (tras fase 2): `aura.context`, manifiesto, enrutado por prefijo,
  enmascarado en bloques, `token_budget` con γ = 0 (**C2 aditivo**).
- **4b** (tras fase 3): retención guiada por el grafo, instantáneas KV,
  calibración de γ.

**Sale con:** tasa de aciertos de caché y tokens evaluados por decisión, antes
y después; restaurar frente a releer.

### Fase 5 — Memoria (L)

- **5a** (no depende de ninguna otra): Expertise con FTS5, aristas causales y
  el conjunto de evaluación. Funciona con el kernel de hoy.
- **5b** (necesita un servidor de embeddings; por el nodo a partir de la fase
  1): vectores, fusión, expansión, reordenado, escalera de plazo, MCP.
- **5c** (tras fase 3): consolidación.

**Sale con:** comparación a presupuesto igualado contra lo de hoy; latencia de
recuperación con 1M de registros en el dispositivo de referencia.

### Orden

```
F0a ─▶ F1 ─▶ F2 ─▶ F3 ─▶ F4b
              └──▶ F4a     └─▶ F5c
F0b  (en paralelo; alimenta los umbrales de F1 y F2)
F5a ─▶ F5b
```

Con una persona: F0a, F0b, F1, F5a, F2, F4a, F3, F5b, F4b, F5c. Con dos, la
segunda lleva F0b y la fase 5 desde el principio.

### Pista con vLLM, en paralelo desde la fase 0

| Paso | Qué | Cuándo |
|---|---|---|
| U1 | Reproducir #40004 con la carga de robot, publicar el banco y los números, y ayudar a revisar, rebasar y medir los PR #54530, #65282 y #65283. | Fase 0b |
| U2 | Planificador por plazo fuera del árbol con `--scheduler-cls` (plazo más cercano primero dentro de cada prioridad; descartar lo vencido). Con números, RFC de "plazo por petición" e interfaz estable de planificador. | Tras fase 2 |
| U3 | Preparación para el borde: documentación y CI de Jetson, perfil de poca memoria y una API de *sleep/wake* utilizable en producción. | Tras fase 3 |

Condiciones de vLLM: los PR con ayuda de IA se declaran, y no se aceptan los
hechos enteramente por agentes: "A human submitter must understand and defend
the change end-to-end". **Cada contribución necesita un responsable humano con
nombre.** U2 y U3 son plausibles pero menos seguros que U1.

---

## 12. Riesgos

| Riesgo | Qué se hace |
|---|---|
| vLLM cambia flags cada dos semanas | Versión fijada por driver; un trabajo de CI contra la última. |
| La interfaz de `--scheduler-cls` no es pública | El planificador propio es un prototipo para el RFC, no una dependencia. |
| El paso por el nodo añade latencia o carga al proceso que enruta | Se mide en fase 1. El modo `external` sigue existiendo. |
| Un fallo del proxy afecta al nodo | Tope de tamaño de cuerpo; el paquete no comparte estado con el executor salvo por su interfaz. |
| Activar la herencia de plazos cambia el comportamiento de grafos existentes | Un `deadline_ms` aguas arriba de un stream ahora lo trunca. Ningún grafo del repositorio lo usa; los de terceros, no se sabe. Existe `respond_ms` para lo que querían decir, y la guía lo avisa. |
| El linaje encarece cada reenvío | Medido en la fase 0a: 0,3–0,4 µs por entrega, y sin diferencia distinguible del ruido en el banco existente (sección 15). |
| El linaje desaloja una entrada viva | Desalojo por última referencia (5.1); cuando ocurre, queda marcado y contado, no silencioso. |
| Los registros observados y los recibos crecen con cada inferencia | El manifiesto va por hash; los registros no citados caducan (5.7). |
| Una generación iniciada por webhook se corta a los dos minutos | Consecuencia de ligar la cancelación al fin de sesión. Se documenta; alargar esa vida es una decisión aparte. |
| vLLM en Jetson: memoria alta y arranques largos (un usuario reportó ~15 min) | llama.cpp es el motor del borde pequeño; vLLM en el borde solo desde 16 GB y tras medirlo. |
| Las instantáneas KV ocupan mucho y no son deterministas | Presupuesto de disco; solo contextos calientes y estables. |
| El índice aproximado de `sqlite-vec` no es estable | Fuerza bruta binaria hasta su límite medido; después, otro backend. |
| La expansión causal no mejora la recuperación | La evaluación propia decide. |
| Faltan manos para upstream | U1 primero y solo; U2 y U3 no empiezan sin responsable. |

---

## 13. Decisiones que faltan

1. **¿El plano de inferencia dentro del proceso del nodo?** Recomendado, por lo
   dicho en 5.2. Mete en el binario conocimiento de motores, en un paquete
   aparte del executor.
2. **¿Tokens acotados por defecto?** Sin ellos, prioridad y plazo son
   cooperativos. Cambiar el valor por defecto afecta a toda instalación existente.
3. **Dispositivo de referencia.** Con 8 GB el motor es llama.cpp; con 16 GB o
   más puede ser vLLM. Condiciona la fase 3 y la pista U3.
4. **Responsables humanos de las contribuciones a vLLM.**
5. **¿Una arista de reserva en C2** para que "qué se ejecuta en su lugar" sea
   del grafo y no de cada Expertise? Cerraría del todo la fila de plazos.
6. **¿Decirle a un Expertise en qué grafo está?** Es lo que haría falta para
   aislar la memoria por grafo dentro de un solo proceso.
7. ~~¿Qué significa `deadline_ms` aguas arriba de un stream?~~ **Resuelta en
   la fase 0a:** dos campos. `deadline_ms` es validez y corta; `respond_ms` es
   respuesta y no corta (5.1).
8. **¿Rechazar lo que un Expertise emite en nombre de un nodo que no es el
   suyo?** Salió al implementar la fase 0a: el kernel enruta por el `node` que
   el propio Expertise declara y no comprueba que sea el suyo. Ahora que el
   kernel sabe qué conexión emite, comprobarlo es barato. No se hizo porque
   cambia comportamiento fuera de lo aprobado.

---

## 14. Fuentes

Comprobadas en fuente primaria el 2026-11-19. Lo no verificado está marcado en
el texto.

**vLLM** — versiones: <https://github.com/vllm-project/vllm/releases> ·
planificador: <https://docs.vllm.ai/en/stable/api/vllm/config/scheduler/> ·
issue #40004: <https://github.com/vllm-project/vllm/issues/40004> ·
PR #54530, #65282, #65283 (mismo repositorio) ·
roadmap Q4: <https://github.com/vllm-project/vllm/issues/61024> ·
plugins: <https://docs.vllm.ai/en/latest/design/plugin_system/> ·
métricas: <https://docs.vllm.ai/en/latest/usage/metrics/> ·
*sleep*: <https://docs.vllm.ai/en/latest/features/sleep_mode/> ·
gobernanza: <https://docs.vllm.ai/en/stable/governance/process/>

**Otros motores** — llama.cpp server: <https://github.com/ggml-org/llama.cpp/blob/master/tools/server/README.md> ·
SGLang: <https://docs.sglang.io/advanced_features/server_arguments.html> ·
Jetson Orin Nano: <https://github.com/hokwangchoi/jetson-orin-nano-benchmarks> ·
ZenML vLLM/SGLang: <https://www.zenml.io/benchmarks/sglang-vs-vllm> ·
EdgePace: <https://arxiv.org/abs/2610.37307>

**Contexto** — Manus: <https://manus.im/blog/Context-Engineering-for-AI-Agents-Lessons-from-Building-Manus> ·
llm-d: <https://llm-d.ai/blog/kvcache-wins-you-can-see> ·
restaurar slots: <https://ai-muninn.com/en/blog/kv-cache-disk-restore-7x> ·
enmascarado (JetBrains): <https://arxiv.org/abs/2508.21433> ·
reutilización fuera de prefijo: <https://arxiv.org/abs/2603.20218> ·
KVFlow: <https://arxiv.org/abs/2507.07400> ·
fuga por caché compartida: <https://ndss-symposium.org/wp-content/uploads/2025-1772-paper.pdf> ·
OpenTelemetry GenAI: <https://github.com/open-telemetry/semantic-conventions-genai>

**Memoria** — Redis: <https://redis.io/agent-memory-benchmark-report/> ·
GraphRAG a presupuesto igualado: <https://arxiv.org/abs/2502.11371> ·
GraphRAG-Bench: <https://arxiv.org/html/2506.05690v3> ·
memoria con modelos pequeños: <https://arxiv.org/html/2602.19320> ·
auditoría de LoCoMo: <https://dev.to/penfieldlabs/we-audited-locomo-64-of-the-answer-key-is-wrong-and-the-judge-accepts-up-to-63-of-intentionally-33lg> ·
sqlite-vec: <https://github.com/asg017/sqlite-vec/releases> ·
Kuzu archivado: <https://github.com/kuzudb/kuzu> ·
eMEM: <https://arxiv.org/abs/2606.03374> ·
MCP 2026-07-28: <https://modelcontextprotocol.io/specification/2026-07-28/changelog.md>

---

## 15. Fase 0: lo construido y lo medido

Rama `fase-1/paso-por-el-nodo`, sin fusionar. Las fases 0 y 1 van en la misma
rama: tocan los mismos archivos y no se separan limpiamente.

### Qué cambia en el contrato

| Spec | Versión | Cambio |
|---|---|---|
| C2 | v1.3 | `edges[].respond_ms`; regla 9; la regla 7 dice cómo se hereda y que un efecto tardío se sella `expired` |
| C3 | v1.8 | `respond_by`; `deadline`, `priority` y `speculative` entran en la tabla de campos y en el esquema, donde faltaban |
| C4 | v1.8 | Resultado `expired` |
| `std/confirmation@1` | — | `expires_at` y `respond_by` en la pregunta de aprobación |

Todos aditivos.

### Qué cambia en el kernel

- **Linaje de entregas** (`executor/lineage.go`): índice por sesión, acotado,
  con desalojo por última referencia.
- **Herencia de plazos por el linaje**, con los dos límites independientes.
- **El vencimiento se decide antes de sellar**; un efecto tardío se sella
  `expired`.
- **Identidad del emisor** desde el socket hasta el enrutado, también a través
  de la cola por sesión y para los Expertises en WASM. La conexión recuerda su
  token.
- **Comprobación de la causa:** válida, de otro, o desconocida. Las dos últimas
  se cuentan y no heredan nada. No se rechaza nada todavía.
- **Reanudación sobre todo el registro**, en memoria acotada, en lugar de los
  primeros 1000 eventos. Reconstruye el linaje y el presupuesto de las entregas
  retenidas.
- **Métricas nuevas** en `/metrics` y `/v1/stats`: descartes por plazo, efectos
  vencidos, respuestas tardías, y el estado del linaje.
- **Auditoría:** los efectos vencidos se cuentan aparte; el resumen del paquete
  de auditoría los contaba antes como entregados.

### Qué cambia en los SDK

- Python: `ctx.deadline`, `ctx.respond_by`, `ctx.priority`, `ctx.remaining_ms()`,
  `ctx.respond_within_ms()`, `ctx.expired`.
- Node: `ctx.id`, `ctx.deadline`, `ctx.respondBy`, `ctx.priority`,
  `ctx.remainingMs()`.

### Qué se comprobó

| Comprobación | Resultado |
|---|---|
| Tests del kernel con detector de carreras | Pasan. Un fallo aislado de bloqueo de SQLite en `guard` no se repitió en seis ejecuciones; volvió en la fase 1 y resultó ser un defecto real y anterior ([sección 16](#16-fase-1-lo-construido-y-lo-medido)) |
| Tests nuevos del executor (plazos, linaje, reanudación) | 28, todos pasan |
| Suite de conformidad C1/C2/C3 contra un nodo real | 67 de 67 |
| `scripts/deadline_e2e.py`, contra un nodo real por sockets reales, con un Expertise hecho con el SDK | 21 de 21 |
| Relevo a un nodo de respaldo (`failover_e2e.py`) | Pasa; 31 ms |
| Ledger adversarial | Pasa |
| SDK de Python y de Node | 52 y 25 tests, todos pasan |

### Qué se midió

En un portátil (Intel i5-1145G7), dentro de un contenedor Linux sobre Docker
Desktop. Sirve para comparar antes y después en la misma máquina; **no es una
cifra para publicar**.

| Medida | Resultado |
|---|---|
| Registrar una entrega en el linaje | 0,3–0,4 µs, 192 bytes |
| Consultar una entrega | 50–70 ns, sin reservar memoria |
| Coste por token de un stream (consulta más registro) | 0,6–0,9 µs |
| `loadgen`, 200 sesiones, 12 rondas alternadas: rendimiento | 11.538 msg/s antes, 11.320 después (mediana) |
| Ídem: latencia p50 | 12,23 ms antes, 12,21 ms después |
| Ídem: latencia p99 | 29,74 ms antes, 29,82 ms después |

La variación entre rondas (de 8.000 a 14.000 msg/s) es mucho mayor que la
diferencia entre versiones: en este entorno el coste del linaje no se distingue
del ruido.

### Qué no se hizo

- **Ninguna medida contra un motor de inferencia real.** Esta máquina no tiene
  GPU y vLLM no corre en Windows. El banco, el motor simulado y el calibrador
  están en `bench/inference/`, validados solo contra el simulado. Quedan
  pendientes la tabla de números propios, la reproducción de #40004 y la
  calibración de α y β.
- **Las entradas ajenas no se rechazan,** solo se cuentan (decisión 8).
- **La interfaz no muestra aún cuándo caduca una aprobación,** aunque el dato
  ya viaja en la pregunta.

---

## 16. Fase 1: lo construido y lo medido

Rama `fase-1/paso-por-el-nodo`, la misma que la fase 0. No cambia C1–C5.

### Cómo se usa

```bash
aura up --inference-url http://127.0.0.1:8000/v1 [--inference-priority vllm|sglang]
export AURA_INFERENCE_URL=node      # en el entorno del Expertise
```

El Expertise llama al modelo a través de su nodo y dice para qué entrega es la
petición (`ctx.delivery`).

### Qué hay

- **Plano de inferencia** (`kernel/internal/infer`): un paquete aparte del
  executor, sin dependencia entre ambos. Rutas compatibles con OpenAI en el
  listener del nodo: chat, completions, embeddings, rerank y models.
- **Atribución por el linaje.** La petición nombra su entrega; el executor
  responde grafo, prioridad, plazo y si la cadena sigue viva. Con token acotado,
  solo vale para entregas enviadas bajo ese mismo token.
- **La prioridad la pone el nodo.** La que escriba la petición se retira; la de
  la arista se escribe en la convención del motor, si se le dijo cuál es.
- **Corte en el motor** por cadena cancelada, especulación descartada, fin de
  sesión, plazo vencido, cliente que se va y parada del nodo.
- **Parada ordenada:** al recibir la señal, el nodo corta las peticiones en
  curso antes de esperar a los handlers.
- **Uso observado:** el nodo pide el uso al motor por su cuenta y retira el
  fragmento extra del stream. Reintenta sin la opción si el motor la rechaza.
- **El cuerpo no se reserializa:** `messages` y `tools` llegan al motor byte a
  byte. Un cuerpo con una clave de primer nivel repetida se rechaza.
- **Visible** en Pulse, `/v1/stats` y `/metrics` (`aura_inference_*`), por
  grafo: peticiones, tokens, aciertos de caché, tiempo al primer token, y por
  qué se detuvo cada una.
- **SDK:** `ctx.delivery` y `stream_chat(delivery=...)` en Python;
  `ctx.inference()` en Node. `llm-chat` y el planner ya lo usan.

### Qué se comprobó

| Comprobación | Resultado |
|---|---|
| Tests del plano de inferencia, con detector de carreras | 42, todos pasan |
| Tests del executor: resolver entregas y avisos de cancelación | 12, todos pasan |
| Tests del gateway: alcance de tokens, atribución, corte, métricas | 7, todos pasan |
| `scripts/inference_e2e.py`: nodo real con autenticación, motor falso, dos Expertises del SDK con token acotado | 30 de 30 |
| SDK de Python, de Node e interfaz | 102, 27 y 130 tests, todos pasan |

### Qué se midió

En el mismo portátil y contenedor que la fase 0. **Contra un motor falso en
loopback: ninguna cifra es de un motor real, y no son para publicar.**

| Medida | Resultado |
|---|---|
| Coste añadido por petición al pasar por el nodo | 0,3–0,6 ms (mediana de 150 alternadas, en varias ejecuciones) |
| De eso, trabajo propio del nodo antes de enviar | 44–60 µs de media |
| Relevo de un token | ~0,29 µs; el mínimo sin mirar el stream es ~0,14 µs |
| Preparar una petición: chat corto / chat de 200 turnos | ~15 µs / ~0,34 ms |
| De `cancel` del cliente a motor liberado | 19–20 ms |
| De SIGTERM a motor liberado | 18–19 ms |
| De SIGTERM a nodo terminado, con una generación en curso | 0,02 s (la espera máxima es de 10 s) |
| Corte por un plazo de 1200 ms | a los 1218–1219 ms |

Los tres tiempos de corte incluyen los 10 ms con que el motor falso comprueba
si le han colgado, así que su resolución no baja de ahí.

### Lo que el relevo costaba al principio

La primera versión analizaba cada fragmento del stream: 2,6 µs por token. Ahora,
tras el primer token, solo se analizan los fragmentos que traen contabilidad, y
el resto se reenvía sin decodificar. Bajó a 0,29 µs.

### Un fallo anterior que apareció al verificar

Al pasar la suite completa, dos tests de `guard` fallaron de forma intermitente.
No era el test: registrar un grafo mientras el nodo escribía otra cosa podía
devolver "database is locked", porque `SaveGraph` leía y luego escribía en una
transacción que no tomaba el candado de escritura hasta la primera escritura.
Ya estaba en `main`. Un test nuevo lo reproduce (2816 fallos en 2 s antes del
arreglo, ninguno después); el almacén abre ahora sus transacciones con el
candado tomado. Va en un commit aparte.

### Qué no se hizo

- **No hay cola.** Una petición que llega se envía. Cómo le va al trabajo
  urgente bajo carga sigue dependiendo del motor. Es la fase 2.
- **Ninguna prueba contra un motor real**, por lo mismo que en la fase 0.
- **El panel de Pulse no se ha visto renderizado.** Compila, y sus cálculos
  están probados, pero nadie lo ha mirado en un navegador.
- **Nada impide que un Expertise llegue al motor directamente** si puede
  alcanzarlo. Cerrar eso es de la fase 3.
- **Sin registros en el ledger.** Lo observado se cuenta, no se sella. Es la
  fase 2.

