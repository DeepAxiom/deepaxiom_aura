import { envelope, newId, PROTOCOL_MAJOR } from "./envelope.js";
import type { ConfigParam, Envelope, Manifest } from "./generated/types.js";

/**
 * Expose functions an app already has as AURA skills.
 *
 * The connector (skills/connector) is for systems you cannot change. This is
 * the other half: when the code is yours, the cheapest possible integration is
 * the app announcing itself. No spec to write, no adapter to maintain, no
 * second copy of your business logic to keep in sync — the function that
 * already exists becomes the skill.
 *
 * An exposed function is deliberately indistinguishable from a projected API
 * operation: same capability shape, same `std/api-request@1` ports, same
 * description contract the planner parses. So it resolves in graphs, appears as
 * an MCP tool, and gets gated when it writes, without knowing any of that.
 */

const BACKOFF_SECONDS = [1, 2, 5, 10, 30, 60];
const HEALTHY_CONNECTION_MS = 30_000;
const DEDUP_WINDOW = 4096;

export interface ExposeOptions {
  /** What it does. The planner reads this to decide whether to use it. */
  summary?: string;
  /**
   * True if calling it changes something. Writes become `motor.*`, which the
   * kernel gates for human approval on every edge, and which callers must
   * promote deliberately. Default false.
   */
  write?: boolean;
  /**
   * Argument names, in the order a caller would pass them. Declared so the
   * planner can tell a real parameter from a key that belongs in the body.
   */
  params?: string[];
  /** Runtime-tunable settings, readable from the handler via `ctx.config`. */
  config?: ConfigParam[];
}

export interface HandlerContext {
  /** The full `std/api-request@1` payload, if you need more than the input. */
  raw: Record<string, unknown>;
  /** Current effective config, kept up to date by the kernel while connected. */
  config: Record<string, unknown>;
  session: string;
  /** True once the caller has abandoned this chain — stop early if you can. */
  readonly cancelled: boolean;
}

export type Handler = (
  input: Record<string, unknown>,
  ctx: HandlerContext,
) => unknown | Promise<unknown>;

export interface NodeOptions {
  /** Package namespace, e.g. your company. Lowercase letters, digits, dashes. */
  org: string;
  /** This app's name — it groups the capabilities it exposes. */
  app: string;
  /** Kernel skill endpoint. Defaults to $AURA_WS_URL or localhost:9080. */
  url?: string;
  /** Called on connection state changes; defaults to console. */
  log?: (message: string, detail?: unknown) => void;
}

const slug = (s: string) => s.toLowerCase().replace(/[^a-z0-9]+/g, "-").replace(/(^-|-$)/g, "");

interface Exposed {
  manifest: Manifest;
  handler: Handler;
  config: Record<string, unknown>;
  socket?: WebSocket;
  seq: Map<string, number>;
  seen: Set<string>;
  seenOrder: string[];
  cancelled: Set<string>;
}

export class AuraNode {
  private readonly options: Required<Pick<NodeOptions, "org" | "app" | "url">>;
  private readonly log: (message: string, detail?: unknown) => void;
  private readonly exposed: Exposed[] = [];
  private running = false;

  constructor(options: NodeOptions) {
    this.options = {
      org: slug(options.org),
      app: slug(options.app),
      url:
        options.url ??
        (globalThis as { process?: { env?: Record<string, string | undefined> } }).process?.env
          ?.AURA_WS_URL ??
        "ws://localhost:9080/ws/skill",
    };
    this.log = options.log ?? ((m, d) => console.log(`[aura] ${m}`, d ?? ""));
  }

  /** Register one function as a skill. Call before `start()`. */
  expose(name: string, handler: Handler, options: ExposeOptions = {}): this {
    if (this.running) throw new Error("expose() must be called before start()");
    const fn = slug(name);
    const kind = options.write ? "motor" : "sensorial";
    const capability = `${kind}.api.${this.options.app.replace(/-/g, "_")}.${fn.replace(/-/g, "_")}`;

    let description = options.summary?.trim() || `${name} on ${this.options.app}`;
    if (!description.endsWith(".")) description += ".";
    description += ` (app "${this.options.app}")`;
    // ojo: esta lista de parametros al final NO es cosmetica, es un
    // contrato con el planner — el planner la parsea para separar los
    // argumentos reales del resto del body. Si cambias el formato aca,
    // cambialo tambien en skills/connector y en
    // kernel/internal/projection/host.go, si no se desincroniza todo.
    if (options.params?.length) {
      description += " parameters: " + options.params.map((p) => `arg:${p}`).join(", ");
    }

    const manifest: Manifest = {
      id: `${this.options.org}/${kind}/${this.options.app}-${fn}`,
      version: "1.0.0",
      protocol: PROTOCOL_MAJOR,
      name: `${this.options.app} ${name}`,
      description,
      capability,
      type: kind,
      format: "source",
      runtime: { language: "node", version: ">=20" },
      ports: {
        ingress: [{ name: "request_in", schema: "std/api-request@1" }],
        egress: [{ name: "response_out", schema: "std/api-response@1" }],
      },
      permissions: { egress_http: [], filesystem: "none", channels: "declared-only" },
      ...(options.config?.length ? { config: options.config } : {}),
    };

    this.exposed.push({
      manifest,
      handler,
      config: Object.fromEntries((options.config ?? []).map((c) => [c.key, c.default])),
      seq: new Map(),
      seen: new Set(),
      seenOrder: [],
      cancelled: new Set(),
    });
    return this;
  }

  /** Connect every exposed function and serve. Resolves only on shutdown. */
  async start(): Promise<void> {
    if (!this.exposed.length) throw new Error("nothing exposed — call expose() first");
    this.running = true;
    this.log(
      `connecting ${this.exposed.length} skill(s) to ${this.options.url}`,
      this.exposed.map((e) => e.manifest.capability),
    );
    await Promise.all(this.exposed.map((e) => this.serve(e)));
  }

  /** One skill's connection, reconnecting for as long as the process lives. */
  private async serve(skill: Exposed): Promise<void> {
    let attempt = 0;
    for (;;) {
      const startedAt = Date.now();
      try {
        await this.session(skill);
      } catch (error) {
        this.log(`${skill.manifest.capability}: ${error}`);
      }
      // si la conexion se mantuvo arriba un rato, eso ya es evidencia de
      // que el kernel esta sano — asi que el proximo corte arranca el
      // backoff desde cero de nuevo. Sin esto: un uptime largo despues de
      // un tropiezo inicial igual reconecta tarde, un minuto entero tarde,
      // solo porque el attempt counter nunca se reseteo. Molesto de debuggear.
      if (Date.now() - startedAt > HEALTHY_CONNECTION_MS) attempt = 0;
      const delay = BACKOFF_SECONDS[Math.min(attempt, BACKOFF_SECONDS.length - 1)] ?? 60;
      attempt++;
      this.log(`${skill.manifest.capability} reconnecting in ${delay}s`);
      await new Promise((r) => setTimeout(r, delay * 1000));
    }
  }

  private session(skill: Exposed): Promise<void> {
    return new Promise((resolve, reject) => {
      const socket = new WebSocket(this.options.url);
      skill.socket = socket;

      socket.addEventListener("open", () => {
        socket.send(JSON.stringify(envelope({ kind: "register", payload: skill.manifest })));
      });

      socket.addEventListener("message", (event) => {
        let env: Envelope;
        try {
          env = JSON.parse(String(event.data)) as Envelope;
        } catch {
          return;
        }
        void this.dispatch(skill, env);
      });

      // A close is an ordinary disconnect, not a failure — only never getting
      // connected is worth reporting as an error.
      socket.addEventListener("close", () => resolve());
      socket.addEventListener("error", () => reject(new Error("connection failed")));
    });
  }

  private async dispatch(skill: Exposed, env: Envelope): Promise<void> {
    if (env.kind === "status") {
      const payload = env.payload as { state?: string; config?: Record<string, unknown> } | null;
      if (payload?.state === "registered") {
        if (payload.config) Object.assign(skill.config, payload.config);
        this.log(`registered ${skill.manifest.capability}`);
      }
      return;
    }
    if (env.kind === "error") {
      this.log(`kernel rejected ${skill.manifest.id}`, env.payload);
      return;
    }
    if (env.kind === "config_update") {
      Object.assign(skill.config, env.payload as Record<string, unknown>);
      return;
    }
    if (env.kind === "cancel") {
      if (env.cause_id) skill.cancelled.add(env.cause_id);
      return;
    }
    if (env.kind !== "data" || env.port !== "request_in") return;

    // At-least-once delivery: the same envelope can arrive twice, and a
    // handler that writes must not run twice for it.
    if (env.idem) {
      if (skill.seen.has(env.idem)) return;
      skill.seen.add(env.idem);
      skill.seenOrder.push(env.idem);
      if (skill.seenOrder.length > DEDUP_WINDOW) {
        const oldest = skill.seenOrder.shift();
        if (oldest) skill.seen.delete(oldest);
      }
    }

    const raw = (env.payload ?? {}) as Record<string, unknown>;
    const body = raw.body;
    const input: Record<string, unknown> = {
      ...((raw.params as Record<string, unknown>) ?? {}),
      ...(body && typeof body === "object" && !Array.isArray(body)
        ? (body as Record<string, unknown>)
        : {}),
    };

    const causeId = env.cause_id ?? "";
    const ctx: HandlerContext = {
      raw,
      config: skill.config,
      session: env.session ?? "",
      get cancelled() {
        return causeId !== "" && skill.cancelled.has(causeId);
      },
    };

    let result: Record<string, unknown>;
    try {
      const value = await skill.handler(input, ctx);
      result = { ok: true, status: 200, body: value ?? null };
    } catch (error) {
      // The failure belongs in the response, not in a crashed process: the
      // caller asked a question and deserves an answer it can act on.
      result = {
        ok: false,
        status: 500,
        error: error instanceof Error ? `${error.name}: ${error.message}` : String(error),
      };
    } finally {
      skill.cancelled.delete(causeId);
    }

    this.emit(skill, env, "response_out", result);
  }

  private emit(skill: Exposed, cause: Envelope, port: string, payload: unknown): void {
    const key = `${cause.session}:${port}`;
    const seq = (skill.seq.get(key) ?? 0) + 1;
    skill.seq.set(key, seq);

    const out = envelope({
      kind: "data",
      cause_id: cause.id,
      session: cause.session,
      node: cause.node,
      port,
      seq,
      idem: `${cause.idem ?? newId()}:${cause.node}:${port}:${seq}`,
      schema: "std/api-response@1",
      payload,
    });
    skill.socket?.send(JSON.stringify(out));
  }
}

/** Create a node. See `AuraNode.expose`. */
export function createNode(options: NodeOptions): AuraNode {
  return new AuraNode(options);
}
