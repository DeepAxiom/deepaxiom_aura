# Roadmap

AURA se está construyendo para ser el sistema operativo de los robots
autónomos: la capa que toma lo que una máquina percibe, corre los modelos que
deciden y autoriza lo que hace — lo bastante rápido para estar dentro del lazo,
con cada decisión trazable hasta lo que la argumentó, y sin que nada actúe
sobre el mundo que el nodo no haya revisado antes.

Este archivo es la distancia entre esa frase y el código. Está ordenado por lo
que un robot necesita primero, no por lo más fácil. Lo que ya existe está en
[Estado de los hitos](GUIDE-ES.md#estado-de-los-hitos); esto es solo lo que no.

[English version](ROADMAP.md)

---

## Dónde está la línea hoy

Dicho primero, porque todo lo de abajo depende de ello.

**Para lo que el kernel sirve hoy** es la capa de decisión de una máquina:
streams de entrada, modelos razonando sobre ellos, efectos de salida a través
de un gate, y todo registrado. Las latencias son las de un proceso Go ruteando
envelopes JSON sobre sockets — por debajo del milisegundo por salto en
loopback, sin contención. Ese es el nivel correcto para "¿debe el brazo
recoger esto?", "¿hay una persona en el camino?", "cambia la ruta", y el
equivocado para el lazo de servo que mueve el brazo.

**Lo que no es, ni va a ser,** es un controlador de tiempo real duro. Un lazo
de control a 1 kHz pertenece al controlador del propio robot, donde está hoy.
La afirmación es más estrecha y más útil: nada de lo que a ese controlador se
le *pide hacer* llega sin haber pasado la policy del nodo, y nada de lo que
hizo falta en el registro.

**Lo que todavía no ha pasado:** ningún robot físico ha sido conducido por este
código en nada de lo que este repositorio prueba. Cada punto de "El robot en
sí", abajo, es el trabajo de volver falsa esa frase.

---

## El robot en sí

Nada de esto es investigación. Es el conjunto de conexiones que una máquina
necesita antes de que el resto de este runtime trate de ella.

| | Por qué va primero | Forma del trabajo |
|---|---|---|
| **Un puente al middleware del robot** | Un robot ya tiene uno — ROS 2 en la mayoría de los casos — y sus sensores, actuadores y controladores ya lo hablan. Hasta que AURA pueda suscribirse a un tópico y llamar una acción, es una capa de decisión sin nada sobre qué decidir. | Tópicos, servicios y acciones proyectados como Expertises, igual que `aura connect` proyecta un documento OpenAPI: un tópico es un stream `sensorial`, una acción es un efecto `motor` y por tanto se gatea y se sella como cualquier otro. De solo lectura por defecto, promoción por acción. |
| **Un paro que sea invariante del kernel** | Un gate pregunta antes de un efecto. Un robot necesita además lo contrario: una señal tras la cual *ninguna* entrega `motor.*` sale del nodo, esté lo que esté en cola, diga lo que diga un grafo, hasta que una persona con nombre la levante. Hoy lo más parecido es editar la policy y reiniciar. | Un alto a nivel de nodo en el executor, en el mismo punto donde se aplica el gate, sellado en el ledger cuando se levanta y cuando se retira, activable desde una entrada de hardware. Tiene que sostenerse con la UI caída y sin red. |
| **Datos de sensores que no sean base64** | El PCM y los fotogramas viajan como base64 dentro de JSON: un tercio más grandes de lo que son, codificados y decodificados en cada salto. Tolerable para voz, no para cámaras. | Una trama binaria para aristas `realtime` — un cambio aditivo a C3, aún sin especificar — y un camino en el mismo host que no copie a través del kernel. C3 ya dice que los datos no tienen que transitar el kernel cuando existe ruta directa; solo está implementado el relay. |
| **El tiempo** | "Sobre qué fotograma se tomó esta decisión" exige que ambos lleven un reloj en el que la máquina esté de acuerdo. Los envelopes llevan un id y un padre causal, no un tiempo de captura. | Un timestamp de captura en los envelopes de sensores, monótono por fuente, y el hash del fotograma que consumió una decisión conservado aunque el fotograma no (el log de eventos ya reduce los payloads `realtime` a un digest; ese digest tiene que volverse citable desde un efecto). |
| **Correr en la máquina** | Se compilan binarios Linux arm64 y existe un smoke test en Raspberry Pi. Nadie ha medido un nodo en un Jetson con una cámara y un modelo compartiendo su GPU. | Un dispositivo de referencia, un presupuesto medido de percepción → decisión → efecto sobre él, y ese número en este archivo. |

## Rápido

"Ultrarrápido" es una afirmación, y por ahora no es una medida. 0.4 quitó los
lugares donde se perdía tiempo sin motivo; no produjo el número.

| | Estado | Qué falta |
|---|---|---|
| **Un presupuesto de latencia, medido** | Las colas de despacho por sesión, las réplicas por carga, el agrupado de deltas y un prefijo de prompt estable están dentro y probados en corrección. | Una cifra. `kernel/cmd/loadgen` conduce un round-trip de echo — sin consumidor lento, un envelope por mensaje — así que no muestra nada de eso. Un generador de carga con un productor en streaming, un consumidor deliberadamente lento y un p99 por salto es lo que convierte "una sesión lenta solo se frena a sí misma" de propiedad probada en propiedad medida. |
| **Deadlines que hagan algo** | Existen deadlines absolutos y heredados: un salto puede apretar uno y nunca extenderlo, y una entrega tardía se descarta en vez de enviarse. | Se *espera* que el Expertise que recibe degrade y nada comprueba que lo haga. Un deadline necesita su segunda mitad: qué corre en su lugar cuando el presupuesto se acabó — un modelo más pequeño, una respuesta en caché, un valor seguro por defecto. |
| **Prioridad que desaloje** | Las aristas declaran una prioridad y el kernel la estampa en cada entrega. | Actuar sobre ella queda en manos del Expertise que la recibe; el kernel no desaloja nada. En una máquina con una sola GPU, una cadena relevante para la seguridad tiene que poder quitarle el modelo a una conversacional, y eso tiene que ser decisión del nodo y no de cada Expertise. |
| **Un presupuesto de contexto en tokens reales** | Los Expertises pueden reportar lo que contó un motor (C5 v1.2) y el kernel lo mide. | El presupuesto por grafo se sigue cobrando a bytes/4. Cobrarlo desde el uso reportado cambia lo que significa un presupuesto existente, y por eso no se hizo de paso. |

## Cada decisión, trazable

Esta es la parte que más existe. El gate, el ledger encadenado por hash, la
aprobación firmada, la atestación de inferencia, el witness, el replay y la
regresión están construidos y probados. Lo que un robot agrega:

| | Por qué lo necesita una máquina |
|---|---|
| **Un registrador que sobreviva a la máquina** | La evidencia está en el directorio de datos, y el directorio de datos está en el robot. Cuando lo que falló es el robot, ese es el lugar equivocado. El ledger ya se puede anclar en un witness; el *log* no se puede enviar a ningún lado. El envío continuo de segmentos sellados a una segunda máquina es además lo que haría que el failover sobreviva a perder la primera. |
| **Replay contra la percepción grabada** | `aura replay` y `aura regress` vuelven a correr sesiones grabadas y comparan los efectos. Para un robot la entrada son datos de sensores, la mayoría de los cuales el log deliberadamente no guarda. Reproducir una decisión necesita los fotogramas que vio, opt-in y acotado. |
| **Verificación de cadena TEE** | La evidencia de hardware llega a `bound`: el quote está atado a una declaración y es comprobable sin conexión. `verified` exige la cadena del fabricante, que está declarada y rechazada en vez de stubbeada, porque nunca ha corrido contra hardware real. |
| **Una traza que incluya al controlador** | Un contexto de traza W3C viaja ya en cada salto dentro de un nodo. Se detiene en el puente al middleware del robot, que todavía no existe. |

## Seguro

| | Estado | Qué falta |
|---|---|---|
| **Contención para código que no es tuyo** | Los Expertises `format: wasm` corren en un sandbox WASI real. Los `format: source` no: `--sandbox process` limpia el entorno y encierra el directorio de trabajo, y no contiene código hostil. | Una frontera de microVM, que está declarada y rechazada al arrancar en vez de degradada. Hasta que aterrice, la posición honesta es *corre tus propios Expertises*, no *instala los de un desconocido*. |
| **Una credencial para un espectador, y otra para un navegador** | Un ticket de observación deja que un espectador mire una sesión y nada más. | Un frontend todavía no puede *conducir* una sesión sin el backend de la aplicación en medio. La credencial con alcance de sesión para eso no está construida. |
| **Cadena de suministro de lo que corre el robot** | Los paquetes se firman y las versiones son inmutables; las revisiones de modelo se fijan y se atestiguan; los pesos en formato pickle se rechazan. | Nada verifica que el modelo que a un nodo se le *dijo* correr sea el que un operador de flota *aprobó*. Un manifiesto de despliegue firmado, comprobado al cargar, es el eslabón que falta entre el registry y la atestación. |
| **Operar sin red** | Un nodo no necesita nada externo para correr. | La aprobación sí. Una máquina en campo tiene que poder actuar sobre una policy decidida de antemano, y el registro tiene que decir que eso fue lo que pasó — preautorización con caducidad, sellada como cualquier otra aprobación. |

## Modelos: despliegue y optimización

Un robot es sobre todo un problema de despliegue de modelos con motores
pegados. Lo que existe es un comienzo: un catálogo y descargador
(`model-manager`), un estimador de ajuste al hardware (`model-fit`),
revisiones fijadas y atestiguadas, y — desde 0.4 — todo Expertise de primera
parte capaz de compartir un solo juego de pesos a través de un servidor de
inferencia.

| | Qué falta |
|---|---|
| **Que el nodo sea dueño de su servidor de inferencia** | `AURA_INFERENCE_URL` apunta a un servidor; nada lo arranca, lo supervisa ni lo reinicia. Marcar un modelo como activo no cambia lo que ese servidor tiene cargado, y la admisión (`--memory-budget`) no sabe que existe. Verificado solo contra un endpoint falso. |
| **Cambiar un modelo sin parar la máquina** | Un cambio de pesos es un reinicio de quien los tenga. Hace falta cargar al lado, desviar, descargar — con `aura regress` como el gate que decide si el desvío ocurre, porque esa es exactamente la pregunta que ya contesta. Y un camino de vuelta. |
| **La optimización como un paso, no como una descarga** | `model-fit` elige entre cuantizaciones que produjo alguien más. Producir el artefacto correcto para un dispositivo dado — cuantizar, compilar para su acelerador — no está en este repositorio, y la atestación no tiene cómo decir que un artefacto se derivó de una revisión fijada en vez de descargarse. |
| **Más de un modelo en un acelerador** | Cada Expertise que carga pesos carga su propia copia. Percepción, lenguaje y voz en una GPU pequeña necesitan un planificador que sepa qué está cargado, qué cuesta y qué cadena está esperando. |
| **Voz realmente incremental** | Una policy LocalAgreement opcional da un prefijo estable. Medida en CPU con un modelo pequeño no abarató los parciales, y empeoró los finales en un idioma. Un reconocedor en streaming, en vez de uno por lotes decodificado repetidamente, sigue siendo el arreglo de verdad. |

## Video

El sentido principal de un robot. `media/` es un artefacto aparte con su
propia línea de versión, a propósito: codificar ocupa un núcleo durante minutos
y aprobar un efecto no puede hacer cola detrás de eso nunca.

`aura-media` 0.2 publica mientras codifica — un activo es reproducible segundos
después de empezar, con progreso reportado —, recibe una transmisión en vivo
por RTMP y saca una escalera HLS adaptativa con una ventana DVR que se vuelve
una grabación normal cuando la transmisión termina, muestrea fotogramas en la
misma decodificación que la codificación, y se le puede decir qué encoder usar.
Todo se corrió de punta a punta contra ffmpeg real en Linux. Nada se reprodujo
en un reproductor real, ningún encoder por hardware ha codificado un fotograma,
y la latencia en vivo está calculada, no medida. [`media/README.md`](media/README.md)
tiene la lista.

| | Qué falta |
|---|---|
| **Una cámara, en vivo, a una latencia que un robot pueda usar** | HLS es la forma correcta para una audiencia y la equivocada para un lazo de control: segundos de retraso por diseño. La percepción necesita fotogramas en decenas de milisegundos, lo que significa WebRTC o un transporte crudo, no una playlist. |
| **Fotogramas a un modelo sin tocar un disco** | Los fotogramas se muestrean a archivos y se suben a un almacén. Un Expertise de percepción los necesita entregados en memoria, por el camino binario de arriba. |
| **De-identificación antes de guardar nada** | Caras y texto fuera de lo que una máquina graba en un lugar donde hay personas. Tiene un humano que aprueba y un artefacto que sellar, así que es un efecto gateado y no un filtro — y es la capacidad que obliga a este subsistema a codificar un video *nuevo* y no solo a transcodificar uno. |
| **Un almacén de objetos que no sea un directorio** | La interfaz existe; una implementación compatible con S3 no. |
| **Distribución a escala** | No es un objetivo. Los segmentos son archivos estáticos; un CDN delante de ellos es la respuesta, y construir uno aquí sería construir lo equivocado. |

## Una flota

| | Estado | Qué falta |
|---|---|---|
| **Un nodo que muere** | `aura up --standby` se queda listo junto a un nodo vivo y toma el relevo cuando muere: medido en decenas de milisegundos en Linux cuando ambos corren en un mismo host, porque el standby puede ver que el proceso ya no existe en vez de esperar el arrendamiento. Las sesiones se reanudan desde el log causal. | Perder la *máquina*. El standby necesita el directorio de datos, y el directorio de datos está en la máquina que murió. Ese es el punto de envío del log de arriba, y es la diferencia entre failover y alta disponibilidad. |
| **Muchas máquinas** | Los nodos se federan: un grafo puede resolver un Expertise en otro nodo. | Nada administra una flota como una sola cosa — el despliegue de una policy o un modelo a cien máquinas, con el registro de qué máquina corrió qué versión y cuándo. |
| **Muchos clientes en una sesión** | Observadores de solo lectura, ninguno capaz de frenar la sesión, con un ticket por espectador y una reproducción del historial reciente al conectarse. | Lo mismo sobre QUIC, y una cuenta de observadores que escale más allá de los sockets de un nodo retransmitiendo en vez de subiendo el límite. |

## Ponerlo en manos de alguien

La ingeniería va por delante de la distribución, y esa distancia es el
obstáculo real para que alguien averigüe si algo de lo anterior es cierto.

| | Forma del trabajo |
|---|---|
| **Publicar los SDKs** | `@deepaxiom/aura` y `aura-sdk` están empaquetados y sin publicar. Horas: npm y PyPI, y un job de CI que publique en tag. |
| **Publicar una imagen de contenedor** | Se construye y se ejercita en cada push; no se empuja a ningún lado. Horas. |
| **Un witness público** | El anclaje es el efecto de red, y operar uno no cuesta casi nada. Días: una instancia, una URL, una política de retención que alguien sostenga. |
| **Binarios de 0.4** | La última release firmada es la 0.3.0. |

## Trabajo de estándares

| | Estado |
|---|---|
| **[Aprobación humana firmada](spec/proposals/draft-signed-human-approval.md)** | En `-01`, sin enviar. Agrega el miembro `context` — digests de lo que se le mostró a quien aprueba —, que es lo que convierte "una persona con nombre hizo clic" en "una persona con nombre consintió esto", y que el kernel implementa como C4 v1.7. |
| **[Recibos de efecto para MCP](spec/proposals/mcp-effect-receipts.md)** | Escrito, sin proponer aguas arriba. |

## Deliberadamente fuera

- **Un kernel de tiempo real duro.** Ver el inicio de este archivo. El lazo de
  servo se queda donde está; lo que le llega es lo que esto controla.
- **Un plano de control alojado.** El argumento de neutralidad solo funciona si
  el registry y el witness son cosas que puedes alojar tú.
- **Un router de policy aprendido.** La policy está pensada para leerse de un
  vistazo por quien la audita. Un modelo que decide qué se gatea, no.
- **Un CDN de video.** Segmentos estáticos y el borde de alguien más.
- **Embeber roots TEE de fabricante en el binario.** Un root que no se puede
  rotar falla cerrado en el peor momento o abierto en el equivocado.
