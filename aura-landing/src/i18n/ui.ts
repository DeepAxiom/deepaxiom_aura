// src/i18n/ui.ts

export const languages = {
  en: 'English',
  es: 'Español',
};

export const defaultLang = 'es';

// The runtime's repository. Private for now; will resolve once the org repo is public.
export const githubUrl = 'https://github.com/DeepAxiom/deepaxiom_aura';
export const githubReadmeUrl = `${githubUrl}/blob/main/README.md`;
// The install walkthrough lives in the README itself; there is no separate
// getting-started file. Both names below point at that section.
export const githubGettingStartedUrl = `${githubUrl}/blob/main/README.md#install--quickstart`;
export const githubGettingStartedEsUrl = `${githubUrl}/blob/main/README-ES.md#instalación-y-arranque-rápido`;
export const githubLicenseUrl = `${githubUrl}/blob/main/LICENSE.md`;
// There is no examples/ directory; the worked, runnable examples are the
// first-party skills themselves.
export const githubDemoUrl = `${githubUrl}/tree/main/skills`;

export const ui = {
  en: {
    // SEO
    'seo.home.title': 'Deep Axiom — AI Runtime for Existing Software, built on the AURA kernel architecture',
    'seo.description': 'Deep Axiom is an open-source runtime for wiring real-time AI into software that already exists: any model, one small binary, human-approval gates on every action, dual-licensed (Apache-2.0 spec/SDK, AGPLv3 or commercial kernel).',
    'seo.about.title': 'About Deep Axiom',
    'seo.home.description': 'Connect existing APIs, compose skills into real-time graphs, run locally or federate across a fleet, publish and install skills like packages, explain and replay every session. No account, no cloud required.',
    'seo.waitlist.title': 'Stay Updated | Deep Axiom',
    'seo.waitlist.description': 'Deep Axiom is open source and available today. Get notified about releases, the marketplace, and the commercial/nonprofit licensing program.',
    'seo.privacy.title': 'Privacy Policy | Deep Axiom',
    'seo.terms.title': 'Terms of Service | Deep Axiom',
    'seo.docs.title': 'Documentation | Deep Axiom',
    'seo.docs.description': 'Install, build, and run Deep Axiom: quickstart, CLI reference, and the flagship voice + vision + smart-home example.',

    // Header & Footer
    'header.family': 'Architecture',
    'header.capabilities': 'Features',
    'header.axioms': 'Principles',
    'header.useCases': 'Examples',
    'header.docs': 'Docs',
    'header.contact': 'Contact',
    'header.blog': 'Blog',
    'header.github': 'GitHub',
    'footer.about': 'About Us',
    'footer.privacy': 'Privacy Policy',
    'footer.terms': 'Terms of Service',

    // Hero Section
    'sectionHero.subtitle': 'An open-source AI runtime for software that already exists',
    'sectionHero.buttonPlay': 'Play',
    'sectionHero.buttonStop': 'Stop',
    'sectionHero.buttonPrimary': 'Get Started',
    'sectionHero.buttonSecondary': 'View on GitHub',

    'sectionSystem.title': 'Any model, any device, one interface',
    'sectionSystem.subtitle': "Deep Axiom's kernel — built on the AURA architecture — is not for deploying one AI model; it's the runtime that unifies all your tools: legacy APIs, databases, edge devices, and AI models of every kind, into one coherent system that perceives, reasons, and acts in real time. This is possible because of one atomic unit: the Skill. There are five types:",
    'sectionSystem.skills.subtitle': "The five Skill types:",
    'sectionSystem.skills.sensory.title': "Sensorial",
    'sectionSystem.skills.sensory.description': "Perceives — turns the world into data. ASR (speech-to-text), OCR, cameras, file readers, or a projected read-only endpoint from an existing API.",
    'sectionSystem.skills.motor.title': "Motor",
    'sectionSystem.skills.motor.description': "Acts — produces effects in the world. Sends a message, writes to an ERP, speaks out loud. Every edge into a motor skill can carry a human-approval gate — the kernel holds the message until a person says yes.",
    'sectionSystem.skills.cognitive.title': "Cognitive",
    'sectionSystem.skills.cognitive.description': "Reasons — decides, plans, generates. Any LLM behind one interface: a local GGUF via llama.cpp with no account and no cloud, or any OpenAI-compatible API (OpenAI, Gemini) if you'd rather use a cloud model.",
    'sectionSystem.skills.logical.title': "Logical",
    'sectionSystem.skills.logical.description': "Transforms and validates — deterministic data-to-data. Parsers, validators, format bridges: the plumbing that turns one skill's output into the next skill's input.",
    'sectionSystem.skills.memory.title': "Memory",
    'sectionSystem.skills.memory.description': "Remembers — persists and retrieves context. Per-session conversation history today; durable, queryable memory as the architecture matures.",
    'sectionSystem.hierarchy.subtitle': "How it composes",
    'sectionSystem.hierarchy.intro': "With this palette of five skill types, everything else — from a two-line test graph to a gated multi-agent pipeline — is built on three concepts:",
    'sectionSystem.hierarchy.level1.level': "Skill",
    'sectionSystem.hierarchy.level1.title': "The atomic, reusable unit.",
    'sectionSystem.hierarchy.level1.description': "A Skill is something the system knows how to do: identity, typed ports, a manifest. It can be logic, a model, or a projection of a system you already run — connected to the kernel, never rewritten.",
    'sectionSystem.hierarchy.level2.level': "Graph",
    'sectionSystem.hierarchy.level2.title': "Skills wired by typed channels.",
    'sectionSystem.hierarchy.level2.description': "A human writes a graph in a few lines of JSON, or a planner generates one from a natural-language goal — both compile to the exact same intermediate representation and run through the same executor. One debugger, one permission model, one replay path.",
    'sectionSystem.hierarchy.level3.level': "Session",
    'sectionSystem.hierarchy.level3.title': "A live, causal, explainable run.",
    'sectionSystem.hierarchy.level3.description': "Every session persists an append-only causal event log — each message names the message that caused it. `aura why` narrates a failure's root cause from that log; `aura replay` turns recorded traffic into an eval suite.",

    // Capabilities Section
    'sectionCapabilities.title': 'What the runtime actually does',
    'sectionCapabilities.subtitle': 'Four commitments the whole design falls out of.',
    'sectionCapabilities.card1.title': 'Legacy-first',
    'sectionCapabilities.card1.description': 'The first useful command is not "create a project" — it\'s "connect what you already have." Point it at an OpenAPI spec and its operations become skills, read-only by default, writes gated behind human approval.',
    'sectionCapabilities.card2.title': 'Real-time by default',
    'sectionCapabilities.card2.description': 'The unit of communication is a typed, causal, back-pressured stream, not a function call. Text streams token by token; the same primitive carries audio and events.',
    'sectionCapabilities.card3.title': 'Any model, any device',
    'sectionCapabilities.card3.description': 'LLMs, ASR, OCR, TTS, embeddings — all live behind one interface, local or remote. The same logical graph runs on a laptop or spreads across a fleet by changing only where skills are placed.',
    'sectionCapabilities.card4.title': 'Owned by everyone',
    'sectionCapabilities.card4.description': 'The spec, kernel, and SDKs are neutral and open forever; the registry is federable, so no single party controls distribution. Forking the standard is always trivial — that\'s what makes the neutrality credible.',

    // --- COMPARISON SECTION ---
    'sectionComparison.title': 'Every existing option is wrong in the same way',
    'sectionComparison.subtitle': "Workflow tools, agent frameworks, embeddable SDKs, voice frameworks, model marketplaces — each solved one piece. None of them led with \"connect the software you already run.\"",

    'comparison.feature.paradigm': 'Core paradigm',
    'comparison.feature.devs': 'For developers',
    'comparison.feature.users': 'Legacy systems',
    'comparison.feature.execution': 'Execution model',

    // Workflow tools (n8n / Zapier / Make)
    'comparison.competitor.paradigm': 'Batch triggers, outside your code',
    'comparison.competitor.devs': 'You hand-build every integration; nothing is real-time or edge-aware by design.',
    'comparison.competitor.users': 'Pokes the system from the outside — never touches the running application.',
    'comparison.competitor.execution': 'Trigger & polling (passive)',

    // Deep Axiom / AURA
    'comparison.deepaxiom.paradigm': 'Typed, causal, real-time streams',
    'comparison.deepaxiom.devs': 'Write a ~30-line skill against the SDK; publish it to a federable registry others can install by capability.',
    'comparison.deepaxiom.users': '`aura connect --openapi` projects an existing API as skills in minutes — read-only by default, writes gated.',
    'comparison.deepaxiom.execution': 'Reactive streaming (WebSocket, back-pressured)',

    // --- SHOWCASE SECTION (runnable examples, replaces client case studies) ---
    'sectionShowcase.title': 'Runnable today, not a roadmap slide',
    'sectionShowcase.subtitle': 'Every example below is real code in the repository. Clone it, build the kernel, and run it — no account required.',
    'sectionShowcase.category.deployed': 'Worked examples',
    'sectionShowcase.readMore': 'View source',

    // EXAMPLE 1: Back-office demo
    'examples.backoffice.tag': 'FLAGSHIP EXAMPLE',
    'examples.backoffice.title': 'A voice assistant that acts, safely',
    'examples.backoffice.description': 'Voice, live cameras, and real smart-home devices, wired together by nothing but the planner. Every action carries a real human-approval gate spoken out loud: say no, and nothing happens. Measured hard-cancel included.',
    'examples.backoffice.linkText': 'Run the demo',

    // EXAMPLE 2: Voice loop
    'examples.voiceloop.tag': 'REAL-TIME',
    'examples.voiceloop.title': 'Speech in, reasoning, speech out',
    'examples.voiceloop.description': 'ASR → LLM → TTS wired into one graph. A spoken sentence comes back spoken, streaming the whole way, local models only — no cloud, no account.',
    'examples.voiceloop.linkText': 'View source',

    // EXAMPLE 3: Legacy connect
    'examples.legacy.tag': 'LEGACY-FIRST',
    'examples.legacy.title': 'An existing API, AI-operable in minutes',
    'examples.legacy.description': '`aura connect --openapi` turns a real OpenAPI spec into skills. Reads go live immediately; every write starts disabled until you promote it, and even then a human approves it.',
    'examples.legacy.linkText': 'Read the walkthrough',

    // EXAMPLE 4: Federation
    'examples.federation.tag': 'EDGE + CLOUD',
    'examples.federation.title': 'One command, work runs on another machine',
    'examples.federation.description': 'Federate a node and it starts resolving capabilities that physically live elsewhere — replies stream back with causality intact. Zero kernel changes were needed to build it.',
    'examples.federation.linkText': 'Read the walkthrough',

    // Tools Section
    'sectionTools.title': 'Two ways in: write or install',
    'sectionTools.subtitle': 'The SDK for people who build skills, the marketplace for everyone who wants to use them without writing code.',
    'sectionTools.button': 'Read the SDK guide',
    'sectionTools.card1.title': 'The SDK',
    'sectionTools.card1.description': 'A skill is a directory: a manifest, code, dependencies. The Python SDK handles connection, causality, and idempotency — a working skill is about 30 lines.',
    'sectionTools.card2.title': 'The Marketplace',
    'sectionTools.card2.description': '`aura publish` signs and uploads a skill to a federable registry; `aura add --capability` installs by function, not by name. Anyone can host a registry — it\'s not a platform you\'re locked into.',

    // Safety Section
    'sectionSafety.title': 'Security is progressive, not bolted on',
    'sectionSafety.subtitle': 'Zero friction on localhost — no account, no signing, nothing to configure. Publishing signs every package (Ed25519); installing verifies hash and signature. And every edge into a skill that acts on the world is gated for human approval by the kernel itself — not by the graph, not by a prompt, so a graph that forgets the gate still gets one. This is pre-1.0: a node has no authentication yet, and skills are not sandboxed.',

    // Axioms Section (grounded principles, not poetic marketing)
    'sectionAxioms.title': 'The principles the design doesn\'t bend on',
    'sectionAxioms.subtitle': 'Eight commitments the design is built around — not slogans. Where the kernel does not enforce one yet, it says so.',
    'sectionAxioms.axiom1.title': 'Legacy-first',
    'sectionAxioms.axiom1.description': 'Connecting what already exists is the first command, not an afterthought bolted on later.',
    'sectionAxioms.axiom2.title': 'Real-time by default',
    'sectionAxioms.axiom2.description': 'Every channel is a typed, causal, back-pressured stream — not a request/response call pretending to be one.',
    'sectionAxioms.axiom3.title': 'Any model, any device',
    'sectionAxioms.axiom3.description': 'One interface for local and cloud models alike; the same graph runs on a laptop or across a fleet.',
    'sectionAxioms.axiom4.title': 'Owned by everyone',
    'sectionAxioms.axiom4.description': 'The spec, kernel, and SDKs stay neutral and open; the registry is federable so no party controls distribution.',
    'sectionAxioms.axiom5.title': 'Progressive security',
    'sectionAxioms.axiom5.description': 'Zero friction on localhost; package signing is always on. Node authentication and skill sandboxing are designed, not yet built.',
    'sectionAxioms.axiom6.title': 'Capability-based permissions',
    'sectionAxioms.axiom6.description': 'A skill declares every permission it needs, and the registry shows them before you install. Runtime enforcement is not built yet — today the declaration is a contract, not a sandbox.',
    'sectionAxioms.axiom7.title': 'Human-in-the-loop for actions',
    'sectionAxioms.axiom7.description': 'Every edge into a skill that acts on the world carries a human-approval gate, applied by the kernel — the safety rule lives there, not in the graph and not in a prompt.',
    'sectionAxioms.axiom8.title': 'Causal auditability',
    'sectionAxioms.axiom8.description': 'Every session is an append-only causal log. `aura why` explains it; `aura replay` turns it into a regression test.',

    // CTA Section
    'sectionCTA.title': 'Clone it. Build it. Run it today.',
    'sectionCTA.subtitle': 'Deep Axiom is open source and available now — no waitlist, no account. Ten minutes from a cold clone to a gated multi-agent pipeline running on your own machine.',
    'sectionCTA.buttonPrimary': 'View on GitHub',
    'sectionCTA.buttonSecondary': 'Contact Team',
    'sectionCTA.followUs': 'Follow Us',

    // About Page
    'about.title': 'About Deep Axiom',
    'about.intro': 'Deep Axiom builds and maintains the AURA kernel architecture: an open-source runtime for wiring real-time AI into software organizations already run.',
    'about.product.title': 'What we build',
    'about.product.text': 'A single self-contained binary — kernel, control-plane UI, state store, and message bus in one file — that composes skills (LLMs, vision, speech, OCR, existing business APIs) into live graphs a planner can assemble from a goal, runs them locally or across a fleet, and explains or replays every session after the fact.',
    'about.mission.title': 'Our Mission',
    'about.mission.text': 'Give AI hands and eyes inside the systems companies already run, without asking anyone to rewrite them — and without asking a human to hand over approval for every action it takes.',
    'about.vision.title': 'Our Vision',
    'about.vision.text': 'A standard for real-time, legacy-aware AI orchestration that stays neutral by construction: the spec and SDKs are Apache-2.0 forever, the registry is federable, and forking is always trivial — so no single company, including ours, controls the ecosystem.',

    // Docs Page
    'docs.hero.title': 'Documentation',
    'docs.hero.subtitle': 'Everything here also lives in the repository — this page mirrors the README so you don\'t have to leave the browser to get oriented.',
    'docs.requirements.title': 'Requirements',
    'docs.requirements.text': 'Go 1.25+ to build the kernel, Python 3.11+ to run skills, Node 20+ only if you want to rebuild the UI (it ships prebuilt and embedded). Any consumer machine (8 GB RAM) is enough — no account, no cloud, no Docker.',
    'docs.quickstart.title': 'Quickstart',
    'docs.quickstart.step1': 'Clone the repo and build the kernel: `cd kernel && go build -o aura.exe ./cmd/aura`',
    'docs.quickstart.step2': 'Start it: `.\\aura.exe up` — opens the control-plane UI at localhost:9080',
    'docs.quickstart.step3': 'Talk to it: `aura chat --graph echo "test"` needs no model at all',
    'docs.quickstart.step4': 'Bring a local LLM online, then run the gated example — see the README for the full walkthrough',
    'docs.cli.title': 'CLI reference (selected)',
    'docs.cli.up': 'Start a node and its embedded UI',
    'docs.cli.chat': 'REPL or one-shot conversation with a graph, streaming replies',
    'docs.cli.do': 'Natural-language goal → plan → gated execution',
    'docs.cli.connect': 'Project an existing OpenAPI spec as skills',
    'docs.cli.publish': 'Sign and upload a skill to a federable registry',
    'docs.cli.why': 'Narrate a session\'s root cause from its causal log',
    'docs.cli.federate': 'Proxy a remote node\'s skills into the local node',
    'docs.full.title': 'The full reference',
    'docs.full.text': 'The README covers every command, the three frozen contracts (C1 manifest, C2 graph IR, C3 channel protocol), the HTTP/WebSocket API, and the security model in full.',
    'docs.full.readme': 'Read the full README',
    'docs.full.gettingstarted': 'Read the step-by-step guide',
    'docs.full.license': 'Licensing (Apache-2.0 / AGPLv3 / commercial)',

    // Waitlist / Get Updates Page
    'waitlist.title': 'The runtime is out. This is just for updates.',
    'waitlist.subtitle': 'Deep Axiom doesn\'t require signup to use — clone the repo and run it today. Leave your email if you want release notes, marketplace news, and updates on the commercial/nonprofit licensing program instead.',
    'waitlist.badge': 'STAY UPDATED',
    'waitlist.step1.title': 'Where do we reach you?',
    'waitlist.step1.subtitle': 'Just an inbox for release notes — no account is created.',
    'waitlist.step2.subtitle': 'Optional — helps us send fewer, more relevant emails.',
    'waitlist.form.email': 'Email Address',
    'waitlist.form.whatsapp': 'WhatsApp (Optional)',
    'waitlist.form.interests': 'What would you like updates about?',
    'waitlist.form.interest.forge': 'New skills in the marketplace',
    'waitlist.form.interest.studio': 'Contributing to the kernel/SDK',
    'waitlist.form.interest.health': 'Commercial / nonprofit licensing',
    'waitlist.form.interest.logistics': 'Enterprise & federation deployments',
    'waitlist.form.interest.general': 'General updates',
    'waitlist.form.button': 'Register',
    'waitlist.form.success': 'Thank you! You\'ll hear from us when there\'s something worth saying.',
    'waitlist.getStartedNow': 'Don\'t want to wait? Get started right now',
    'waitlist.back': 'Back',
    'waitlist.next': 'Continue',
    'waitlist.connecting': 'Connecting...',
    'waitlist.error': 'Error — Try Again',
    'waitlist.success.desc': 'In the meantime, the fastest way to see it in action is still cloning the repo. Grab a coffee while the local model downloads on first run!',
    'waitlist.success.home': 'Return to Home',

    // Legal Pages (Templates)
    'privacy.title': 'Privacy Policy',
    'privacy.lastUpdated': 'Last updated: October 23, 2025',
    'privacy.p1': 'Your privacy is important to us. It is Deep Axiom\'s policy to respect your privacy regarding any information we may collect from you across our website, and other sites we own and operate.',
    'privacy.p2': 'We only ask for personal information when we truly need it to provide a service to you. We collect it by fair and lawful means, with your knowledge and consent. We also let you know why we’re collecting it and how it will be used.',
    'terms.title': 'Terms of Service',
    'terms.lastUpdated': 'Last updated: October 23, 2025',
    'terms.p1': 'By accessing the website at [Your Website URL], you are agreeing to be bound by these terms of service, all applicable laws and regulations, and agree that you are responsible for compliance with any applicable local laws.',
    'terms.p2': 'Permission is granted to temporarily download one copy of the materials (information or software) on Deep Axiom\'s website for personal, non-commercial transitory viewing only. This is the grant of a license, not a transfer of title.',
  },
  es: {
    // SEO
    'seo.home.title': 'Deep Axiom — Runtime de IA para software existente, construido sobre la arquitectura de kernel AURA',
    'seo.description': 'Deep Axiom es un runtime de código abierto para conectar IA en tiempo real al software que ya existe: cualquier modelo, un binario pequeño, puertas de aprobación humana en cada acción, licencia dual (Apache-2.0 spec/SDK, AGPLv3 o comercial para el kernel).',
    'seo.about.title': 'Sobre Deep Axiom',
    'seo.home.description': 'Conecta APIs existentes, compón skills en grafos en tiempo real, corre localmente o federa entre una flota, publica e instala skills como paquetes, explica y reproduce cada sesión. Sin cuenta, sin nube requerida.',
    'seo.waitlist.title': 'Mantente al Día | Deep Axiom',
    'seo.waitlist.description': 'Deep Axiom es open source y está disponible hoy. Recibe novedades sobre releases, el marketplace y el programa de licenciamiento comercial/sin fines de lucro.',
    'seo.privacy.title': 'Política de Privacidad | Deep Axiom',
    'seo.terms.title': 'Términos de Servicio | Deep Axiom',
    'seo.docs.title': 'Documentación | Deep Axiom',
    'seo.docs.description': 'Instala, compila y ejecuta Deep Axiom: guía rápida, referencia de la CLI, y el ejemplo insignia de voz + visión + casa inteligente.',

    // Header & Footer
    'header.family': 'Arquitectura',
    'header.capabilities': 'Funciones',
    'header.axioms': 'Principios',
    'header.useCases': 'Ejemplos',
    'header.docs': 'Docs',
    'header.contact': 'Contacto',
    'header.blog': 'Blog',
    'header.github': 'GitHub',

    'footer.about': 'Nosotros',
    'footer.privacy': 'Política de Privacidad',
    'footer.terms': 'Términos de Servicio',

    // Hero Section
    'sectionHero.subtitle': 'Un runtime de IA de código abierto para el software que ya existe',
    'sectionHero.buttonPlay': 'Play',
    'sectionHero.buttonStop': 'Stop',
    'sectionHero.buttonPrimary': 'Empieza Ahora',
    'sectionHero.buttonSecondary': 'Ver en GitHub',

    'sectionSystem.title': 'Cualquier modelo, cualquier dispositivo, una sola interfaz',
    'sectionSystem.subtitle': 'El kernel de Deep Axiom —construido sobre la arquitectura AURA— no sirve para desplegar un modelo de IA; es el runtime que unifica todas tus herramientas: APIs legacy, bases de datos, dispositivos edge y modelos de IA de todo tipo, en un solo sistema coherente que percibe, razona y actúa en tiempo real. Esto es posible gracias a una sola unidad atómica: el Skill. Hay cinco tipos:',
    'sectionSystem.skills.subtitle': 'Los cinco tipos de Skill:',
    'sectionSystem.skills.sensory.title': 'Sensorial',
    'sectionSystem.skills.sensory.description': 'Percibe — convierte el mundo en datos. ASR (voz a texto), OCR, cámaras, lectores de archivos, o un endpoint de solo lectura proyectado desde una API existente.',
    'sectionSystem.skills.motor.title': 'Motor',
    'sectionSystem.skills.motor.description': 'Actúa — produce efectos en el mundo. Envía un mensaje, escribe en un ERP, habla en voz alta. Toda arista hacia un skill motor puede llevar una puerta de aprobación humana — el kernel retiene el mensaje hasta que una persona dice que sí.',
    'sectionSystem.skills.cognitive.title': 'Cognitivo',
    'sectionSystem.skills.cognitive.description': 'Razona — decide, planifica, genera. Cualquier LLM tras una misma interfaz: un GGUF local vía llama.cpp sin cuenta ni nube, o cualquier API compatible con OpenAI (OpenAI, Gemini) si prefieres un modelo en la nube.',
    'sectionSystem.skills.logical.title': 'Lógico',
    'sectionSystem.skills.logical.description': 'Transforma y valida — datos a datos, de forma determinista. Parsers, validadores, puentes de formato: la plomería que convierte la salida de un skill en la entrada del siguiente.',
    'sectionSystem.skills.memory.title': 'Memoria',
    'sectionSystem.skills.memory.description': 'Recuerda — persiste y recupera contexto. Historial de conversación por sesión hoy; memoria duradera y consultable a medida que madura la arquitectura.',
    'sectionSystem.hierarchy.subtitle': 'Cómo se compone',
    'sectionSystem.hierarchy.intro': 'Con esta paleta de cinco tipos de skill, todo lo demás —desde un grafo de prueba de dos líneas hasta un pipeline multiagente con puertas— se construye sobre tres conceptos:',
    'sectionSystem.hierarchy.level1.level': 'Skill',
    'sectionSystem.hierarchy.level1.title': 'La unidad atómica y reutilizable.',
    'sectionSystem.hierarchy.level1.description': 'Un Skill es algo que el sistema sabe hacer: identidad, puertos tipados, un manifiesto. Puede ser lógica, un modelo, o la proyección de un sistema que ya operas —conectado al kernel, nunca reescrito.',
    'sectionSystem.hierarchy.level2.level': 'Graph',
    'sectionSystem.hierarchy.level2.title': 'Skills cableados por channels tipados.',
    'sectionSystem.hierarchy.level2.description': 'Un humano escribe un grafo en pocas líneas de JSON, o un planner genera uno a partir de un objetivo en lenguaje natural —ambos compilan a la misma representación intermedia y corren por el mismo ejecutor. Un depurador, un modelo de permisos, una vía de replay.',
    'sectionSystem.hierarchy.level3.level': 'Session',
    'sectionSystem.hierarchy.level3.title': 'Una ejecución viva, causal y explicable.',
    'sectionSystem.hierarchy.level3.description': 'Cada sesión persiste un log de eventos causal append-only —cada mensaje nombra al mensaje que lo causó. `aura why` narra la causa raíz de un fallo a partir de ese log; `aura replay` convierte tráfico grabado en una suite de evaluación.',

    // Capabilities Section
    'sectionCapabilities.title': 'Lo que el runtime hace de verdad',
    'sectionCapabilities.subtitle': 'Cuatro compromisos de los que se deriva todo el diseño.',
    'sectionCapabilities.card1.title': 'Legacy primero',
    'sectionCapabilities.card1.description': 'El primer comando útil no es "crea un proyecto" —es "conecta lo que ya tienes". Apúntalo a una especificación OpenAPI y sus operaciones se vuelven skills, de solo lectura por defecto, con las escrituras gateadas tras aprobación humana.',
    'sectionCapabilities.card2.title': 'Tiempo real por defecto',
    'sectionCapabilities.card2.description': 'La unidad de comunicación es un stream tipado, causal y con back-pressure, no una llamada a función. El texto fluye token a token; la misma primitiva transporta audio y eventos.',
    'sectionCapabilities.card3.title': 'Cualquier modelo, cualquier dispositivo',
    'sectionCapabilities.card3.description': 'LLMs, ASR, OCR, TTS, embeddings —todos tras una misma interfaz, locales o remotos. El mismo grafo lógico corre en un portátil o se reparte por una flota cambiando solo dónde se colocan los skills.',
    'sectionCapabilities.card4.title': 'Propiedad de todos',
    'sectionCapabilities.card4.description': 'La spec, el kernel y los SDKs son neutrales y abiertos para siempre; el registro es federable, así que ninguna parte controla la distribución. Forkear el estándar es siempre trivial —eso es lo que hace creíble la neutralidad.',

    // --- COMPARISON SECTION ---
    'sectionComparison.title': 'Cada opción existente falla de la misma manera',
    'sectionComparison.subtitle': 'Herramientas de workflows, frameworks de agentes, SDKs embebibles, frameworks de voz, marketplaces de modelos —cada uno resolvió una pieza. Ninguno lideró con "conecta el software que ya operas".',

    'comparison.feature.paradigm': 'Paradigma central',
    'comparison.feature.devs': 'Para programadores',
    'comparison.feature.users': 'Sistemas legacy',
    'comparison.feature.execution': 'Modelo de ejecución',

    // Herramientas de workflows (n8n / Zapier / Make)
    'comparison.competitor.paradigm': 'Triggers por lotes, fuera de tu código',
    'comparison.competitor.devs': 'Construyes cada integración a mano; nada es de tiempo real ni consciente del edge por diseño.',
    'comparison.competitor.users': 'Pincha el sistema desde fuera —nunca toca la aplicación en ejecución.',
    'comparison.competitor.execution': 'Disparadores y polling (pasivo)',

    // Deep Axiom / AURA
    'comparison.deepaxiom.paradigm': 'Streams tipados, causales, en tiempo real',
    'comparison.deepaxiom.devs': 'Escribe un skill de ~30 líneas contra el SDK; publícalo en un registro federable que otros instalan por capacidad.',
    'comparison.deepaxiom.users': '`aura connect --openapi` proyecta una API existente como skills en minutos —de solo lectura por defecto, escrituras gateadas.',
    'comparison.deepaxiom.execution': 'Streaming reactivo (WebSocket, con back-pressure)',

    // --- SHOWCASE SECTION (ejemplos ejecutables, reemplaza casos de cliente) ---
    'sectionShowcase.title': 'Ejecutable hoy, no una diapositiva de roadmap',
    'sectionShowcase.subtitle': 'Cada ejemplo de abajo es código real en el repositorio. Clónalo, compila el kernel, y ejecútalo —sin necesidad de cuenta.',
    'sectionShowcase.category.deployed': 'Ejemplos completos',
    'sectionShowcase.readMore': 'Ver código fuente',

    // EJEMPLO 1: Back-office demo
    'examples.backoffice.tag': 'EJEMPLO INSIGNIA',
    'examples.backoffice.title': 'Un asistente de voz que actúa, con seguridad',
    'examples.backoffice.description': 'Voz, cámaras en vivo, y dispositivos reales de casa inteligente, conectados sin nada más que el planificador. Cada acción lleva una puerta de aprobación humana real, dicha en voz alta: di que no, y no pasa nada. Incluye cancelación real medida.',
    'examples.backoffice.linkText': 'Ejecutar la demo',

    // EJEMPLO 2: Voice loop
    'examples.voiceloop.tag': 'TIEMPO REAL',
    'examples.voiceloop.title': 'Voz de entrada, razonamiento, voz de salida',
    'examples.voiceloop.description': 'ASR → LLM → TTS cableados en un solo grafo. Una frase hablada vuelve hablada, en streaming todo el camino, solo con modelos locales —sin nube, sin cuenta.',
    'examples.voiceloop.linkText': 'Ver código fuente',

    // EJEMPLO 3: Legacy connect
    'examples.legacy.tag': 'LEGACY-FIRST',
    'examples.legacy.title': 'Una API existente, operable por IA en minutos',
    'examples.legacy.description': '`aura connect --openapi` convierte una especificación OpenAPI real en skills. Las lecturas se activan de inmediato; toda escritura empieza deshabilitada hasta que la promueves, y aun así un humano la aprueba.',
    'examples.legacy.linkText': 'Leer el recorrido',

    // EJEMPLO 4: Federation
    'examples.federation.tag': 'EDGE + NUBE',
    'examples.federation.title': 'Un comando, el trabajo corre en otra máquina',
    'examples.federation.description': 'Federa un nodo y empieza a resolver capacidades que viven físicamente en otro lugar —las respuestas vuelven en streaming con la causalidad intacta. No hizo falta ni un cambio en el kernel para construirlo.',
    'examples.federation.linkText': 'Leer el recorrido',

    // Tools Section
    'sectionTools.title': 'Dos formas de entrar: escribir o instalar',
    'sectionTools.subtitle': 'El SDK para quienes construyen skills, el marketplace para quienes quieren usarlos sin escribir código.',
    'sectionTools.button': 'Leer la guía del SDK',
    'sectionTools.card1.title': 'El SDK',
    'sectionTools.card1.description': 'Un skill es un directorio: un manifiesto, código, dependencias. El SDK de Python gestiona la conexión, la causalidad y la idempotencia —un skill funcional son unas 30 líneas.',
    'sectionTools.card2.title': 'El Marketplace',
    'sectionTools.card2.description': '`aura publish` firma y sube un skill a un registro federable; `aura add --capability` instala por función, no por nombre. Cualquiera puede alojar un registro —no es una plataforma que te encierra.',

    // Safety Section
    'sectionSafety.title': 'La seguridad es progresiva, no un añadido',
    'sectionSafety.subtitle': 'Cero fricción en localhost —sin cuenta, sin firma, nada que configurar. Publicar firma cada paquete (Ed25519); instalar verifica hash y firma. Y toda arista hacia un skill que actúa sobre el mundo lleva aprobación humana aplicada por el kernel mismo —no por el grafo ni por un prompt, así que un grafo que olvida la puerta la tiene igual. Esto es pre-1.0: un nodo todavía no tiene autenticación, y los skills no están aislados.',

    // Axioms Section (principios reales, no marketing poético)
    'sectionAxioms.title': 'Los principios que el diseño no negocia',
    'sectionAxioms.subtitle': 'Ocho compromisos sobre los que está construido el diseño —no eslóganes. Donde el kernel todavía no hace cumplir uno, lo dice.',
    'sectionAxioms.axiom1.title': 'Legacy primero',
    'sectionAxioms.axiom1.description': 'Conectar lo que ya existe es el primer comando, no una ocurrencia tardía añadida después.',
    'sectionAxioms.axiom2.title': 'Tiempo real por defecto',
    'sectionAxioms.axiom2.description': 'Todo channel es un stream tipado, causal y con back-pressure —no una llamada petición/respuesta disfrazada.',
    'sectionAxioms.axiom3.title': 'Cualquier modelo, cualquier dispositivo',
    'sectionAxioms.axiom3.description': 'Una interfaz para modelos locales y en la nube por igual; el mismo grafo corre en un portátil o en una flota.',
    'sectionAxioms.axiom4.title': 'Propiedad de todos',
    'sectionAxioms.axiom4.description': 'La spec, el kernel y los SDKs se mantienen neutrales y abiertos; el registro es federable así que ninguna parte controla la distribución.',
    'sectionAxioms.axiom5.title': 'Seguridad progresiva',
    'sectionAxioms.axiom5.description': 'Cero fricción en localhost; la firma de paquetes está siempre activa. La autenticación del nodo y el aislamiento de skills están diseñados, aún no construidos.',
    'sectionAxioms.axiom6.title': 'Permisos basados en capacidades',
    'sectionAxioms.axiom6.description': 'Un skill declara cada permiso que necesita, y el registry te los enseña antes de instalar. La aplicación en tiempo de ejecución aún no está construida —hoy la declaración es un contrato, no un sandbox.',
    'sectionAxioms.axiom7.title': 'Humano en el bucle para las acciones',
    'sectionAxioms.axiom7.description': 'Toda arista hacia un skill que actúa sobre el mundo lleva una puerta de aprobación humana, aplicada por el kernel —la regla de seguridad vive ahí, no en el grafo ni en un prompt.',
    'sectionAxioms.axiom8.title': 'Auditabilidad causal',
    'sectionAxioms.axiom8.description': 'Cada sesión es un log causal append-only. `aura why` lo explica; `aura replay` lo convierte en una prueba de regresión.',

    // CTA Section
    'sectionCTA.title': 'Clónalo. Compílalo. Ejecútalo hoy.',
    'sectionCTA.subtitle': 'Deep Axiom es open source y está disponible ahora —sin lista de espera, sin cuenta. Diez minutos desde un clon limpio hasta un pipeline multiagente con puertas corriendo en tu propia máquina.',
    'sectionCTA.buttonPrimary': 'Ver en GitHub',
    'sectionCTA.buttonSecondary': 'Contactar al Equipo',
    'sectionCTA.followUs': 'Síguenos',

    // About Page
    'about.title': 'Sobre Deep Axiom',
    'about.intro': 'Deep Axiom construye y mantiene la arquitectura de kernel AURA: un runtime de código abierto para conectar IA en tiempo real al software que las organizaciones ya operan.',
    'about.product.title': 'Qué construimos',
    'about.product.text': 'Un único binario autocontenido —kernel, UI del plano de control, almacén de estado y bus de mensajes en un solo archivo— que compone skills (LLMs, visión, voz, OCR, APIs de negocio existentes) en grafos vivos que un planner puede ensamblar a partir de un objetivo, los ejecuta localmente o en una flota, y explica o reproduce cada sesión a posteriori.',
    'about.mission.title': 'Nuestra Misión',
    'about.mission.text': 'Darle a la IA manos y ojos dentro de los sistemas que las empresas ya operan, sin pedirle a nadie que los reescriba —y sin pedirle a un humano que ceda la aprobación de cada acción que toma.',
    'about.vision.title': 'Nuestra Visión',
    'about.vision.text': 'Un estándar para la orquestación de IA en tiempo real, consciente del legacy, que se mantiene neutral por construcción: la spec y los SDKs son Apache-2.0 para siempre, el registro es federable, y forkear es siempre trivial —así que ninguna empresa, ni siquiera la nuestra, controla el ecosistema.',

    // Docs Page
    'docs.hero.title': 'Documentación',
    'docs.hero.subtitle': 'Todo esto también vive en el repositorio —esta página refleja el README para que no tengas que salir del navegador para orientarte.',
    'docs.requirements.title': 'Requisitos',
    'docs.requirements.text': 'Go 1.25+ para compilar el kernel, Python 3.11+ para ejecutar skills, Node 20+ solo si quieres reconstruir la UI (ya viene precompilada y embebida). Cualquier máquina de consumo (8 GB de RAM) basta —sin cuenta, sin nube, sin Docker.',
    'docs.quickstart.title': 'Guía rápida',
    'docs.quickstart.step1': 'Clona el repo y compila el kernel: `cd kernel && go build -o aura.exe ./cmd/aura`',
    'docs.quickstart.step2': 'Arráncalo: `.\\aura.exe up` —abre la UI del plano de control en localhost:9080',
    'docs.quickstart.step3': 'Háblale: `aura chat --graph echo "test"` no necesita ningún modelo',
    'docs.quickstart.step4': 'Pon en línea un LLM local, y luego corre el ejemplo con puerta —ver el README para el recorrido completo',
    'docs.cli.title': 'Referencia de la CLI (selección)',
    'docs.cli.up': 'Arranca un nodo y su UI embebida',
    'docs.cli.chat': 'REPL o conversación de un turno con un grafo, con respuestas en streaming',
    'docs.cli.do': 'Objetivo en lenguaje natural → plan → ejecución con puertas',
    'docs.cli.connect': 'Proyecta una especificación OpenAPI existente como skills',
    'docs.cli.publish': 'Firma y sube un skill a un registro federable',
    'docs.cli.why': 'Narra la causa raíz de una sesión a partir de su log causal',
    'docs.cli.federate': 'Proyecta los skills de un nodo remoto dentro del nodo local',
    'docs.full.title': 'La referencia completa',
    'docs.full.text': 'El README cubre cada comando, los tres contratos congelados (manifiesto C1, IR de grafo C2, protocolo de channel C3), la API HTTP/WebSocket, y el modelo de seguridad completo.',
    'docs.full.readme': 'Leer el README completo',
    'docs.full.gettingstarted': 'Leer la guía paso a paso',
    'docs.full.license': 'Licenciamiento (Apache-2.0 / AGPLv3 / comercial)',

    // Waitlist / Página de novedades
    'waitlist.title': 'El runtime ya está disponible. Esto es solo para novedades.',
    'waitlist.subtitle': 'Deep Axiom no requiere registro para usarse —clona el repo y ejecútalo hoy. Deja tu correo si prefieres recibir notas de release, novedades del marketplace, y actualizaciones del programa de licenciamiento comercial/sin fines de lucro.',
    'waitlist.badge': 'MANTENTE AL DÍA',
    'waitlist.step1.title': '¿Dónde te contactamos?',
    'waitlist.step1.subtitle': 'Solo un correo para notas de release —no se crea ninguna cuenta.',
    'waitlist.step2.subtitle': 'Opcional —nos ayuda a enviar menos correos, más relevantes.',
    'waitlist.form.email': 'Correo Electrónico',
    'waitlist.form.whatsapp': 'WhatsApp (Opcional)',
    'waitlist.form.interests': '¿Sobre qué te gustaría recibir novedades?',
    'waitlist.form.interest.forge': 'Nuevos skills en el marketplace',
    'waitlist.form.interest.studio': 'Contribuir al kernel/SDK',
    'waitlist.form.interest.health': 'Licenciamiento comercial / sin fines de lucro',
    'waitlist.form.interest.logistics': 'Despliegues enterprise y federación',
    'waitlist.form.interest.general': 'Actualizaciones generales',
    'waitlist.form.button': 'Registrarme',
    'waitlist.form.success': '¡Gracias! Sabrás de nosotros cuando haya algo que valga la pena contar.',
    'waitlist.getStartedNow': '¿No quieres esperar? Empieza ahora mismo',
    'waitlist.back': 'Atrás',
    'waitlist.next': 'Continuar',
    'waitlist.connecting': 'Conectando...',
    'waitlist.error': 'Error — Reintentar',
    'waitlist.success.desc': 'Mientras tanto, la forma más rápida de verlo en acción sigue siendo clonar el repo. ¡Tómate un café mientras el modelo local se descarga en el primer arranque!',
    'waitlist.success.home': 'Regresar al Inicio',

    // Legal Pages (Templates)
    'privacy.title': 'Política de Privacidad',
    'privacy.lastUpdated': 'Última actualización: 23 de Octubre, 2025',
    'privacy.p1': 'Tu privacidad es importante para nosotros. Es política de Deep Axiom respetar tu privacidad con respecto a cualquier información que podamos recopilar de ti a través de nuestro sitio web y otros sitios que poseemos y operamos.',
    'privacy.p2': 'Solo pedimos información personal cuando realmente la necesitamos para brindarte un servicio. La recopilamos por medios justos y legales, con tu conocimiento y consentimiento. También te informamos por qué la recopilamos y cómo se utilizará.',
    'terms.title': 'Términos de Servicio',
    'terms.lastUpdated': 'Última actualización: 23 de Octubre, 2025',
    'terms.p1': 'Al acceder al sitio web en deepaxiom.com y cualquier página o subdominio asociado a deepaxiom.com, aceptas estar sujeto a estos términos de servicio, todas las leyes y regulaciones aplicables, y aceptas que eres responsable del cumplimiento de las leyes locales aplicables.',
    'terms.p2': 'Se concede permiso para descargar temporalmente una copia de los materiales (información o software) en el sitio web de Deep Axiom solo para visualización transitoria personal y no comercial. Esta es la concesión de una licencia, no una transferencia de título.',
  },
} as const;
