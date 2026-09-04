# Roadmap

Lo que falta, en el orden en que le bloquea a alguien. Corto a propósito: la
versión anterior de este archivo tenía 95 KB y la mayor parte describía trabajo
ya terminado, que es un changelog con nombre de roadmap. Lo que ya está hecho
está en [Estado de los hitos](GUIDE-ES.md#estado-de-los-hitos); esto es solo lo
que no.

[English version](ROADMAP.md)

---

## Bloquea producción

Nada de lo de abajo es un problema de investigación. Es la diferencia entre
"desplegable como servicio auxiliar" y "desplegable como algo con SLA".

| | Por qué bloquea | Forma del trabajo |
|---|---|---|
| **Failover de nodo** | Un nodo es un proceso, y nada arranca un reemplazo. El hueco es más estrecho de lo que decía: el estado de sesión *sí* es recuperable — `resumeFromLog` reconstruye la ventana de deduplicación, los índices causales, los gates pendientes y los contadores por salto, muera el cliente o el kernel — y el SDK reconecta solo. Un único escritor ya está garantizado por un arrendamiento, así que un standby no puede corromper el ledger arrancando junto a un nodo vivo. Falta la orquestación: algo que note que el líder murió y arranque el reemplazo, y decidir si ese reemplazo vive en la misma máquina u otra. | **Reconsiderar antes de construir.** La recuperación tras un SIGKILL mide 10.04s, de los cuales 10.03s son esperar el arrendamiento del muerto; el nodo vuelve en 3ms, incluso sobre un ledger de 4,000 entradas. Así que `--lease-ttl` ya *es* la perilla del tiempo de recuperación, y un standby tibio gastaría complejidad real en ahorrarse un arranque de milisegundos. Lo que sigue sin resolver es la pérdida de máquina, que exige el directorio de datos alcanzable desde dos hosts — lo primero aquí que necesitaría almacenamiento que no trae. |

## Bloquea adopción

La ingeniería va muy por delante de la distribución, y esa distancia es el
problema real.

| | Por qué bloquea | Forma del trabajo |
|---|---|---|
| **Publicar el repositorio** | El README dice `git clone` contra una URL que devuelve 404. Nada de esto existe todavía para nadie. | Horas. |
| **Publicar los SDKs** | `@deepaxiom/aura` y `aura-sdk` están empaquetados y sin publicar. Un equipo full-stack que siga [la guía de integración](GUIDE-ES.md) no puede hacer `npm install` — lo copia desde el repo. | Horas: npm y PyPI, más un job de CI que publique en tag. |
| **Publicar binarios y una imagen de contenedor** | CI compila para seis targets y los sube como artifacts con retención de siete días, así que una release no es descargable. La imagen ahora se construye y se ejercita en cada push — llega a healthy sobre un volumen nuevo, drena con SIGTERM y conserva su identidad tras un reinicio — pero no se publica en ningún sitio. | Horas: un job de release, y un registry al que empujar. |
| **Un witness público** | El anclaje es el efecto de red. Un witness al que acuden varios nodos independientes vale más que dos nodos anclándose mutuamente, y operarlo no cuesta casi nada. | Días: una instancia, una URL, y una política de retención que alguien sostenga. |

## Estrechar los huecos declarados

Cada uno de estos es un sitio donde la documentación dice hoy "esto no está
garantizado", y cerrarlo mueve una línea en [Estado de los
hitos](GUIDE-ES.md#estado-de-los-hitos).

| | Qué cambiaría |
|---|---|
| **Sandbox microVM para `format: source`** | Hoy `--sandbox process` limpia el entorno y encierra el directorio de trabajo; no contiene código hostil. Hasta que esto aterrice, el pitch del marketplace es *publica y aloja los tuyos*, no *instala código de desconocidos*. `format: wasm` ya está genuinamente aislado. |
| **Verificación de cadena TEE** | La evidencia de hardware llega a `bound` — el quote está atado a esa declaración concreta y es comprobable sin conexión. `verified` exige verificar la cadena del fabricante, que está declarada y rechazada en vez de stubbeada, porque nunca ha corrido contra hardware real. |
| **Credenciales cortas para el navegador** | Un frontend web no puede tener el token del operador. Hoy el backend de la app hace de proxy. Una credencial corta con alcance de sesión quitaría ese salto. |
| **Vista multidispositivo de una sesión viva** | Una sesión, un socket. La forma "muchos clientes mirando una conversación" no existe. |
| **Un modelo activo para todo el nodo** | `model-manager` marca un modelo como activo y `llm-chat` lo sigue; el backend local del planner sigue leyendo `AURA_MODEL_FILE` con su propio default, así que el cambio mueve la mitad del sistema. Además cada uno carga su propia copia de los pesos, lo que con un 4B sale caro en una tarjeta pequeña. |

## Leer una imagen, y las dos negativas que trae

**Construido:** `deepaxiom/cognitive/imaging-read` — entra una muestra de
imágenes de un estudio, salen hallazgos con coordenadas normalizadas, la
correlación contra el informe que ya está en el expediente, un borrador de
impresión y lo que no se pudo decir. Dos esquemas nuevos, `std/image-study@1` y
`std/imaging-finding@1`.

**Cognitiva y nunca motor**, que es todo el arreglo: aquí no se escribe nada. El
borrador llega a un expediente sólo por un efecto `motor.*` que aprueba un
médico con nombre en el gate, y la atestación C5 que sale con la respuesta es lo
que le permite a ese efecto decir sobre qué base ocurrió: qué modelo, qué
revisión, sobre qué prompt. Sin ella una bitácora dice quién firmó y no qué le
enseñaron.

Dos cosas que rechaza, y las dos se encuentran el primer día:

- **Por dónde lee.** Por la API de Gemini, con una llave. No está cubierta
  por el acuerdo de tratamiento de datos de Google, y eso es una decisión de
  quien despliega y no de esta skill: lo que la skill debe es que la decisión se
  vea —ese host es el único de su lista de salida— y que el motor quede en la
  atestación C5 de cada lectura, para que «por dónde salió esta imagen» se
  responda desde el registro y no desde la memoria de alguien.
- **Imágenes que nadie des-identificó.** Un ultrasonido lleva el nombre de la
  paciente **quemado en los píxeles**, no sólo en las etiquetas — DICOM hasta
  tiene un atributo que lo dice, `BurnedInAnnotation`. Esta skill no puede
  revisar píxeles, así que exige que quien las manda declare que las limpió, y
  si no, declina. Ser el sitio donde nadie revisó es peor que declinar.

**Lo que todavía no hace**, en el orden en que va a hacer falta:

| Falta | Por qué importa |
|---|---|
| **Tapar lo quemado** | Hoy el requisito se empuja a quien manda, así que un estudio cuyos píxeles llevan un nombre sencillamente no se puede leer. Va aquí: es el mismo decodificado de fotogramas que el subsistema de medios ya hace, y tiene una persona que aprueba y un artefacto que sellar. |
| **Un lector local** | La API de Gemini significa que las imágenes salen del despliegue. Un nodo dentro de la red de un hospital va a querer que la lectura se quede ahí, y la forma de la skill no cambia: cambia el backend detrás de `reader.py`. |
| **Un endpoint bajo contrato** | Vertex AI se construyó y se quitó: un segundo camino que nadie ejercita es un segundo camino que se pudre, y quien lo consume eligió la llave. Vuelve el día que un despliegue necesite acuerdo de tratamiento de datos, y es la misma forma: un módulo detrás de `reader.ask`. |
| **Leer la serie entera y no una muestra** | Una tomografía son mil doscientas imágenes y al lector se le dan dieciséis. Muestrear es honesto y se declara en `limitations`, y no es lo mismo que leer el estudio. |

## Medios, y la IA sobre ellos

**El piso está construido; las capacidades no.** [`media/`](media/) es un segundo
artefacto, `aura-media`, con su propia línea de versión: entra un activo, sale
una dirección. Llegó desde NAAT, que contrató un CDN de video y un servidor de
WebRTC en vez de esperar a esto — la decisión correcta, y no quita la necesidad.
Cuatro capacidades que ese producto necesita son IA sobre video, todas necesitan
fotogramas, y **ninguna de las cuatro está construida**:

| Capacidad | Por qué el kernel es su sitio |
|---|---|
| **De-identificar un clip antes de publicarlo** | Caras y texto en pantalla fuera de un video que un clínico grabó en consultorio. Tiene un humano que aprueba y un artefacto que sellar, así que es un efecto `logical.*` y no un filtro. Y es la que obliga a la mitad difícil: **produce un activo de video nuevo**, así que este subsistema codifica, no sólo decodifica. |
| **Transcribir y subtitular** | `sensorial.asr.transcribe` ya existe para el dictado. Apuntarlo a un video publicado exige demuxear el audio, y compra accesibilidad más un cuerpo de contenido buscable. |
| **Borrador de un juicio de moderación** | Una red de salud sin criterio de moderación es una plataforma de desinformación con distintivos. Un borrador que cite qué contradice una afirmación, con una persona decidiendo detrás, es exactamente el patrón del gate. Necesita fotogramas muestreados y la transcripción. |
| **Portada y texto alternativo** | Sugerir, con una persona confirmando. Fotogramas otra vez. |

**Se construye aquí y corre al lado del kernel, no dentro.** Compartir el código
es gratis; compartir el proceso es lo que cuesta. Tres razones, y ninguna es de
estilo:

- **Curvas de carga opuestas.** Aprobar un efecto son milisegundos y no puede
  hacer cola nunca; codificar es un núcleo al tope durante minutos. Un solo
  proceso significa que una subida de video puede dejar esperando a un clínico
  que va a firmar una nota.
- **Cruce de zonas.** El producto que consume esto mantiene los medios de la red
  y los datos de paciente en procesos separados a propósito, y el kernel es donde
  se maneja el dictado con PHI. Los bytes de un video público no van ahí — y
  cuando el kernel **sí** cruza las dos, que sea en el gate de de-identificación,
  que es donde el cruce está declarado y aprobado.
- **El ancla deja de ser gobernable.** Quien consume fija un commit para poder
  decir qué kernel selló una nota. Si libav viaja en el mismo artefacto, cada CVE
  de códec obliga a mover la versión del kernel — y mover esa versión debe ser un
  acto deliberado con un changelog que leer. **Así que esto sale como un segundo
  artefacto con su propia línea de versión**, y el archivo de anclaje de quien
  consume gana un segundo renglón en vez de que un renglón signifique dos cosas.

**Lo que existe.** Una cola con `FOR UPDATE SKIP LOCKED` y N workers, donde un
claim es un lease y un worker que muere pierde su trabajo en vez de llevárselo;
ffmpeg para decodificar y codificar; una escalera HLS que nunca escala hacia
arriba, con keyframes alineados entre peldaños; fotogramas y portada salidos de
esa misma decodificación, porque toda capacidad de arriba los necesita y
decodificar dos veces cuesta el doble; un almacén de objetos cuyas fuentes no se
le sirven a nadie. Se registra como `logical.media.transcode` (C1
`format: projection`), así que un grafo lo puede llamar sin que la codificación
entre al kernel.

**Lo que falta del piso.** Un almacén compatible con S3 al lado del de archivos
—la interfaz está, la implementación no—. Shaka Packager donde el muxer HLS de
ffmpeg deje de alcanzar. Progreso por trabajo, en vez de un estado que sólo se
mueve cuando el trabajo termina.

**LiveKit va en el mismo subsistema, y no está empezado.** Es lo que NAAT contrató para el vivo y
para la teleconsulta, y ya habla las dos direcciones que esto necesita: Ingress
acepta RTMP y WHIP, Egress devuelve fotogramas compuestos y HLS. Tiempo real de
entrada, fotogramas de salida, una sola integración — y es lo que hace posible la
IA sobre una transmisión en vivo, que es la línea de producto para la que existe
toda esta sección.

## Trabajo de estándares

| | Estado |
|---|---|
| **[Aprobación humana firmada](spec/proposals/draft-signed-human-approval.md)** | En `-01`, sin enviar. El hueco que llena es real: el draft de audit trail para agentes que ya existe en la IETF registra un id de operador pseudónimo sin firma. `-01` agrega el miembro `context` — digests de lo que se le mostró a quien aprueba —, que es la mitad que convierte "una persona con nombre hizo clic" en "una persona con nombre consintió este documento", y que el kernel implementa como C4 v1.7. Enviarlo es lo que convierte un diseño local en una reclamación sobre la categoría. |
| **[Recibos de efecto para MCP](spec/proposals/mcp-effect-receipts.md)** | Escrito, sin proponer aguas arriba. |

## Cobertura de tests, dónde es delgada y por qué importa

Medida, no recordada — los números están en [Estado de los
hitos](GUIDE-ES.md#estado-de-los-hitos).

- **`cmd/aura` al 8,8%.** Parseo de argumentos y formateo de salida sobre lógica
  que está probada donde vive. El outlier honesto, y de bajo riesgo.
- **`gateway` 54%, `store` 54%, `projection` 59%, `grammar` 52%.** Las garantías
  que implementan están cubiertas a fondo; los caminos de error de los accesores
  alrededor no.
- **La UI está probada solo en su capa de modelo.** Esa capa es ya casi toda la
  lógica — el modelo de grafos contra los mismos vectores de conformidad C2 que
  usa el kernel, las operaciones de edición, el bypass, el fold de
  conversación, los marcos y cada clave de traducción que el código pide. Lo que
  no tiene nada es la parte que toca un navegador: los componentes, la captura
  de audio y las vistas de sesiones.

  La división es deliberada, no un plan a medio ejecutar. Todo lo que puede
  producir IR que el kernel rechazaría es puro y está probado; la aritmética de
  punteros y el CSS se verifican manejando un nodo de verdad y mirando. Los dos
  bugs por los que se escribió la suite de i18n pasaron revisión y se
  publicaron, que es el argumento para llevar más de la mitad-navegador a algo
  mecánico.
- **Las costuras entre componentes son el hueco real.** Cada bug preexistente que
  apareció en la última ronda de trabajo — WebTransport sirviendo `/ws/skill` sin
  autenticar, una ruta abierta elevando una credencial acotada, `/metrics`
  público porque una regla comparaba una forma de ruta, `store.Open` sin crear su
  propio directorio — estaba entre dos componentes que cada uno estaba bien
  probado. La suite de conformidad ejercita contratos y los unit tests ejercitan
  paquetes; nada ejercitaba las uniones.

## Deliberadamente fuera

- **Un plano de control alojado.** El argumento de neutralidad solo funciona si
  el registry y el witness son cosas que puedes alojar tú.
- **Un router de policy aprendido.** La policy está pensada para leerse de un
  vistazo por quien la audita. Un modelo que decide qué se gatea, no.
- **Embeber roots TEE de fabricante en el binario.** Un root que no se puede
  rotar falla cerrado en el peor momento o abierto en el equivocado.
