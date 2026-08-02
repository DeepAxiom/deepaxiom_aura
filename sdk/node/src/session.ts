import { envelope, newId } from "./envelope.js";
import type { Envelope } from "./generated/types.js";

/**
 * A typed client for one live session against a graph (C3 over WebSocket).
 *
 * Without this, talking to AURA from a frontend meant copying the control
 * plane's `useSession.ts` into your own project — a copy that stops matching
 * the protocol the moment either side moves. This is the same thing as an
 * installable, generated-from-the-schemas package, with no framework attached:
 * wrap it in a React hook, a Svelte store, or nothing at all.
 */

export interface SessionOptions {
  /** Kernel base URL, e.g. "http://localhost:9080" or "https://node.internal". */
  baseUrl?: string;
  /** Resume-by-name is not supported yet; passing this only labels the session. */
  session?: string;
  onEnvelope?: (env: Envelope) => void;
  onOpen?: (session: string) => void;
  onClose?: (reason: string) => void;
}

function streamUrl(baseUrl: string, graph: string, session?: string): string {
  const url = new URL(baseUrl);
  url.protocol = url.protocol === "https:" ? "wss:" : "ws:";
  url.pathname = "/v1/stream";
  url.searchParams.set("graph", graph);
  if (session) url.searchParams.set("session", session);
  return url.toString();
}

export class Session {
  private socket?: WebSocket;
  private seq = 0;
  private sessionId = "";
  private readonly options: SessionOptions;
  readonly graph: string;

  constructor(graph: string, options: SessionOptions = {}) {
    this.graph = graph;
    this.options = options;
  }

  /** Open the socket and resolve once the kernel says the session is ready. */
  open(): Promise<string> {
    const base = this.options.baseUrl ?? "http://localhost:9080";
    return new Promise((resolve, reject) => {
      const socket = new WebSocket(streamUrl(base, this.graph, this.options.session));
      this.socket = socket;

      socket.addEventListener("message", (event) => {
        let env: Envelope;
        try {
          env = JSON.parse(String(event.data)) as Envelope;
        } catch {
          return;
        }
        const payload = env.payload as { state?: string; session?: string } | null;
        if (env.kind === "status" && payload?.state === "ready" && payload.session) {
          this.sessionId = payload.session;
          this.options.onOpen?.(this.sessionId);
          resolve(this.sessionId);
        }
        this.options.onEnvelope?.(env);
      });

      socket.addEventListener("close", () => {
        this.options.onClose?.("closed");
        reject(new Error("session closed before it was ready"));
      });
      socket.addEventListener("error", () => reject(new Error("connection failed")));
    });
  }

  /** The kernel-assigned session id — what `aura why` takes. */
  get id(): string {
    return this.sessionId;
  }

  /**
   * Send on a client port. Returns the envelope id, which is what `cancel`
   * names: without it a caller cannot abandon what it just asked for.
   */
  send(port: string, schema: string, payload: unknown): string {
    if (!this.socket) throw new Error("session is not open");
    this.seq += 1;
    const env = envelope({
      kind: "data",
      node: "client",
      port,
      seq: this.seq,
      idem: `client:${port}:${this.seq}:${newId()}`,
      schema,
      payload,
    });
    this.socket.send(JSON.stringify(env));
    return env.id;
  }

  /** Shorthand for the common case: text in on `client.text_out`. */
  sendText(text: string): string {
    return this.send("text_out", "std/text@1", { text, final: true });
  }

  /**
   * Abandon a chain. The kernel stops routing anything belonging to it, so
   * output already being generated never arrives — see C3 §Cancel.
   */
  cancel(envelopeId: string): void {
    this.socket?.send(JSON.stringify(envelope({ kind: "cancel", cause_id: envelopeId })));
  }

  /** Answer a human-approval gate, using the `confirm_request`'s id. */
  respondGate(requestId: string, approve: boolean): void {
    this.socket?.send(
      JSON.stringify(
        envelope({ kind: "confirm_response", cause_id: requestId, payload: { approve } }),
      ),
    );
  }

  close(): void {
    this.socket?.close();
    this.socket = undefined;
  }
}

/** Open a session in one call. */
export async function openSession(graph: string, options?: SessionOptions): Promise<Session> {
  const session = new Session(graph, options);
  await session.open();
  return session;
}
