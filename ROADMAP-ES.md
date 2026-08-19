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
| **Failover de nodo** | Un nodo es un proceso. Si muere, las sesiones vivas mueren con él — el log de eventos sobrevive, el estado de ruteo no. Contesta esto primero y con honestidad: si AURA se cae, ¿tu app degrada o se detiene? Si degrada, ya es desplegable. | Grande. El estado de sesión tiene que volverse recuperable por un segundo proceso, lo que toca los índices en memoria del executor y el modelo de admisión. |
| **Backup y restore, documentados** | El directorio de datos tiene la identidad del nodo, el ledger de efectos y los secretos cifrados del broker — y la clave del broker se *deriva* de la identidad, así que un restore sin `identity/` produce texto cifrado que nadie puede abrir. No hay procedimiento escrito, lo que significa que la primera persona que lo necesite lo va a escribir durante un incidente. | Pequeño. Un procedimiento documentado, un par `aura backup`/`aura restore`, y un test que restaure en un nodo limpio y verifique el ledger. |
| **Rotación de la clave del nodo** | La clave de firma es para siempre. Un operador que sospeche compromiso no tiene ninguna jugada que no invalide todos los checkpoints. | Medio. Necesita un registro de sucesión de claves en C4 para que los checkpoints viejos sigan verificando con la clave vieja. |

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

## Trabajo de estándares

| | Estado |
|---|---|
| **[Aprobación humana firmada](spec/proposals/draft-signed-human-approval.md)** | Escrito como Internet-Draft, sin enviar. El hueco que llena es real: el draft de audit trail para agentes que ya existe en la IETF registra un id de operador pseudónimo sin firma. Enviarlo es lo que convierte un diseño local en una reclamación sobre la categoría. |
| **[Recibos de efecto para MCP](spec/proposals/mcp-effect-receipts.md)** | Escrito, sin proponer aguas arriba. |

## Cobertura de tests, dónde es delgada y por qué importa

Medida, no recordada — los números están en [Estado de los
hitos](GUIDE-ES.md#estado-de-los-hitos).

- **`cmd/aura` al 7,5%.** Parseo de argumentos y formateo de salida sobre lógica
  que está probada donde vive. El outlier honesto, y de bajo riesgo.
- **`gateway` 56%, `store` 56%, `projection` 59%, `grammar` 52%.** Las garantías
  que implementan están cubiertas a fondo; los caminos de error de los accesores
  alrededor no.
- **La UI está probada solo en su capa de modelo.** El modelo de grafos del
  lienzo corre contra los mismos vectores de conformidad C2 que el kernel, y el
  parser de Markdown tiene su propia suite; los componentes, la captura de audio
  y las vistas de sesiones no tienen nada.
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
