import type { NodeStats } from "../lib/stats";
import type {
  Approval,
  Envelope,
  GraphIR,
  GraphRevision,
  Health,
  LedgerEntry,
  LedgerHead,
  LedgerVerify,
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

/**
 * Whether the node is reachable and whether it accepts us. The shell shows the
 * states differently because the remedies are different: start the node, or
 * paste a token.
 */
export type Link = "unknown" | "ok" | "unauthorized" | "unreachable";

let link: Link = "unknown";
const linkListeners = new Set<(l: Link) => void>();

function setLink(next: Link): void {
  if (next === link) return;
  link = next;
  for (const fn of linkListeners) fn(next);
}

export function currentLink(): Link {
  return link;
}

export function onLink(fn: (l: Link) => void): () => void {
  linkListeners.add(fn);
  return () => { linkListeners.delete(fn); };
}

/** An HTTP failure, with the kernel's own error text as the message. */
export class ApiError extends Error {
  status: number;
  constructor(status: number, message: string) {
    super(message);
    this.status = status;
  }
}

/**
 * The route does not exist on this node.
 *
 * Not a 404: the kernel's `GET /` catch-all answers an unknown path with the
 * UI's own HTML and a 200, so a node older than a route says "here is a web
 * page" rather than "not found". Anything that is not JSON is this.
 */
export class Unsupported extends Error {}

/** fetch with the node token attached. */
async function call(path: string, init: RequestInit = {}): Promise<Response> {
  let res: Response;
  try {
    res = await fetch(path, { ...init, headers: authHeaders(init.headers) });
  } catch (e) {
    setLink("unreachable");
    throw new Error((e as Error).message || "node unreachable");
  }
  setLink(res.status === 401 ? "unauthorized" : "ok");
  return res;
}

async function json<T>(res: Response, okStatuses: number[] = []): Promise<T> {
  const isJSON = (res.headers.get("Content-Type") ?? "").includes("json");
  if (!res.ok && !okStatuses.includes(res.status)) {
    const body = isJSON ? await res.json().catch(() => ({})) : {};
    if (res.status === 401) {
      throw new ApiError(401,
        (body as { error?: string }).error ??
          "unauthorized — open the URL `aura up` printed, or paste the node token",
      );
    }
    throw new ApiError(res.status, (body as { error?: string }).error ?? `HTTP ${res.status}`);
  }
  if (!isJSON) throw new Unsupported("this node does not serve that route");
  return res.json() as Promise<T>;
}

/** Null instead of a throw when the node predates the route. */
async function optional<T>(p: Promise<T>): Promise<T | null> {
  try {
    return await p;
  } catch (e) {
    if (e instanceof Unsupported || (e instanceof ApiError && e.status === 404)) return null;
    throw e;
  }
}

/**
 * `/v1/expertise` is the current name for `/v1/skills`; a node older than the
 * rename only serves the latter. Decided once per page load, on first use.
 */
let catalogBase: "/v1/expertise" | "/v1/skills" | null = null;

async function catalog(): Promise<SkillManifest[]> {
  if (catalogBase === null) {
    const list = await optional(call("/v1/expertise").then((r) => json<SkillManifest[] | null>(r)));
    if (list !== null) {
      catalogBase = "/v1/expertise";
      return list;
    }
    // `null` is also what an empty catalog can decode to; the Content-Type
    // check in json() is what tells the two apart, so reaching here with a
    // JSON null means the route exists.
    const probe = await call("/v1/expertise");
    if ((probe.headers.get("Content-Type") ?? "").includes("json")) {
      catalogBase = "/v1/expertise";
      return [];
    }
    catalogBase = "/v1/skills";
  }
  return (await call(catalogBase).then((r) => json<SkillManifest[] | null>(r))) ?? [];
}

export const api = {
  health: () => call("/healthz").then((r) => json<Health>(r)),

  /** GET /v1/stats; null on a node that predates it. */
  stats: () => optional(call("/v1/stats").then((r) => json<NodeStats>(r))),

  /** The Expertise catalog: one manifest per connection. */
  expertise: catalog,
  /** Same call under its wire name, for code written before the rename. */
  skills: catalog,

  skillConfig: (id: string) =>
    call(`${catalogBase ?? "/v1/skills"}/config?id=${encodeURIComponent(id)}`).then((r) => json<SkillConfig>(r)),

  updateSkillConfig: (id: string, patch: Record<string, unknown>) =>
    call(`${catalogBase ?? "/v1/skills"}/config?id=${encodeURIComponent(id)}`, {
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
      .then((r) => json<{ session: string; events: Envelope[] | null; timestamps?: number[] | null }>(r)),

  sessionLedger: (id: string) =>
    optional(call(`/v1/sessions/${encodeURIComponent(id)}/ledger`)
      .then((r) => json<{ entries: LedgerEntry[] | null }>(r)))
      .then((d) => d?.entries ?? []),

  /** A page of sealed effects. Null when this node keeps no ledger. */
  ledger: (fromSeq = 1, limit = 200) =>
    optional(call(`/v1/ledger?from_seq=${fromSeq}&limit=${limit}`)
      .then((r) => json<{ entries: LedgerEntry[] | null; returned: number; total: number }>(r)))
      .then((d) => (d ? { entries: d.entries ?? [], total: d.total } : null)),

  /** Recompute the chain. 409 is an answer ("not sound"), not a failure. */
  ledgerVerify: () => call("/v1/ledger/verify").then((r) => json<LedgerVerify>(r, [409])),

  ledgerHead: () => optional(call("/v1/ledger/head").then((r) => json<LedgerHead>(r))),

  ledgerEntry: (hash: string) =>
    call(`/v1/ledger/entries/${encodeURIComponent(hash)}`).then((r) => json<LedgerEntry>(r)),

  /** Gates waiting on a human who is not holding the session socket. */
  approvals: () =>
    optional(call("/v1/approvals").then((r) => json<{ approvals: Approval[] | null }>(r)))
      .then((d) => (d ? d.approvals ?? [] : null)),

  resolveApproval: (id: string, approve: boolean) =>
    call(`/v1/approvals/${encodeURIComponent(id)}`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ approve }),
    }).then((r) => json<{ id: string; approved: boolean; signed_by: string }>(r)),

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
