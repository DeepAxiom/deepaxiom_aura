import type {
  Envelope,
  GraphIR,
  GraphRevision,
  Health,
  Projection,
  SessionMeta,
  SkillConfig,
  SkillManifest,
} from "./types";

/** REST client for the kernel API (same origin — the UI ships in the binary). */

const TOKEN_KEY = "aura.token";

/**
 * The node's bearer token.
 *
 * `aura up` prints a URL carrying it in the fragment. The fragment is used
 * rather than a query string on purpose: browsers never send it to the server,
 * so the token stays out of access logs and out of the Referer header on any
 * link the page later follows. We read it once, keep it in sessionStorage —
 * which dies with the tab, unlike localStorage — and strip it from the address
 * bar so it is not left sitting in browser history.
 */
function readToken(): string {
  const fromHash = /[#&]token=([^&]+)/.exec(location.hash);
  if (fromHash) {
    const token = decodeURIComponent(fromHash[1]);
    sessionStorage.setItem(TOKEN_KEY, token);
    history.replaceState(null, "", location.pathname + location.search);
    return token;
  }
  return sessionStorage.getItem(TOKEN_KEY) ?? "";
}

let token = readToken();

/** Replace the stored token (the UI prompts when a request comes back 401). */
export function setToken(next: string): void {
  token = next.trim();
  sessionStorage.setItem(TOKEN_KEY, token);
}

export function hasToken(): boolean {
  return token !== "";
}

function authHeaders(extra?: HeadersInit): HeadersInit {
  return token ? { ...(extra ?? {}), Authorization: `Bearer ${token}` } : (extra ?? {});
}

/** fetch with the node token attached. */
function call(path: string, init: RequestInit = {}): Promise<Response> {
  return fetch(path, { ...init, headers: authHeaders(init.headers) });
}

async function json<T>(res: Response): Promise<T> {
  if (!res.ok) {
    const body = await res.json().catch(() => ({}));
    if (res.status === 401) {
      throw new Error(
        (body as { error?: string }).error ??
          "unauthorized — open the URL `aura up` printed, or paste the node token",
      );
    }
    throw new Error((body as { error?: string }).error ?? `HTTP ${res.status}`);
  }
  return res.json() as Promise<T>;
}

export const api = {
  health: () => call("/healthz").then((r) => json<Health>(r)),

  skills: () => call("/v1/skills").then((r) => json<SkillManifest[]>(r)),

  skillConfig: (id: string) =>
    call(`/v1/skills/config?id=${encodeURIComponent(id)}`).then((r) => json<SkillConfig>(r)),

  updateSkillConfig: (id: string, patch: Record<string, unknown>) =>
    call(`/v1/skills/config?id=${encodeURIComponent(id)}`, {
      method: "PUT",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(patch),
    }).then((r) => json<SkillConfig>(r)),

  graphs: () =>
    call("/v1/graphs").then((r) => json<{ graphs: string[] }>(r)).then((g) => g.graphs ?? []),

  graph: (id: string) =>
    call(`/v1/graphs/${encodeURIComponent(id)}`).then((r) => json<GraphIR>(r)),

  /** Every version of a graph that was ever registered, newest first. */
  graphRevisions: (id: string) =>
    call(`/v1/graphs/${encodeURIComponent(id)}/revisions`)
      .then((r) => json<{ revisions: GraphRevision[] }>(r))
      .then((d) => d.revisions ?? []),

  graphRevision: (id: string, n: number) =>
    call(`/v1/graphs/${encodeURIComponent(id)}/revisions/${n}`).then((r) => json<GraphIR>(r)),

  registerGraph: (ir: GraphIR) =>
    call("/v1/graphs", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(ir),
    }).then((r) => json<{ graph_id: string }>(r)),

  sessions: () =>
    call("/v1/sessions")
      .then((r) => json<{ sessions: SessionMeta[] | null }>(r))
      .then((s) => s.sessions ?? []),

  sessionEvents: (id: string) =>
    call(`/v1/sessions/${encodeURIComponent(id)}/events`)
      .then((r) => json<{ session: string; events: Envelope[] }>(r)),

  projections: () =>
    call("/v1/projections")
      .then((r) => json<{ projections: Projection[] }>(r))
      .then((p) => p.projections ?? []),

  connectProjection: (req: {
    kind: "openapi";
    name?: string;
    base_url?: string;
    headers?: Record<string, string>;
    spec: string;
  }) =>
    call("/v1/projections", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(req),
    }).then((r) => json<Projection>(r)),

  promote: (projection: string, op: string, mode: string) =>
    call(`/v1/projections/${encodeURIComponent(projection)}/promote`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ op, mode }),
    }).then((r) => json<Projection>(r)),
};

/**
 * WS URL for a client stream session against a graph.
 *
 * The token rides in the query string here, which it does nowhere else: the
 * browser WebSocket API cannot set request headers, so this is the only way a
 * page can authenticate an upgrade. The kernel accepts `?token=` on the two WS
 * paths only, so an API token never lands in an HTTP access log.
 */
export function streamURL(graph: string): string {
  const proto = location.protocol === "https:" ? "wss" : "ws";
  const auth = token ? `&token=${encodeURIComponent(token)}` : "";
  return `${proto}://${location.host}/v1/stream?graph=${encodeURIComponent(graph)}${auth}`;
}

let counter = 0;

/** Lexicographically unique-enough envelope id for client emissions. */
export function newId(): string {
  counter += 1;
  return (
    Date.now().toString(36).toUpperCase() +
    counter.toString(36).toUpperCase() +
    Math.random().toString(36).slice(2, 10).toUpperCase()
  );
}
