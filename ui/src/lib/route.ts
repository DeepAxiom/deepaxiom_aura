/**
 * Hash routing. The node serves one document and has no server-side routes,
 * so the fragment is the only part of the address the UI owns.
 *
 *   #/pulse
 *   #/studio/<graph>
 *   #/sessions/<session>/<envelope>
 *   #/evidence/approvals
 *   #/expertise/<id>            (ids contain slashes; each segment is encoded)
 *   #/reference/<tab>
 *
 * `#token=…` is not a route: `aura up` prints it and api/client strips it.
 */

export const VIEWS = [
  "pulse", "studio", "sessions", "evidence", "expertise", "graphs", "projections", "reference",
] as const;

export type ViewId = (typeof VIEWS)[number];

export interface Route {
  view: ViewId;
  args: string[];
}

export const DEFAULT_ROUTE: Route = { view: "pulse", args: [] };

/** Old addresses that still have to land somewhere sensible. */
const ALIASES: Record<string, ViewId> = { skills: "expertise", ledger: "evidence", home: "pulse" };

export function parseRoute(hash: string): Route {
  const raw = hash.replace(/^#/, "");
  if (!raw.startsWith("/")) return DEFAULT_ROUTE;
  const parts = raw.split("?")[0].split("/").filter(Boolean).map((p) => {
    try { return decodeURIComponent(p); } catch { return p; }
  });
  const head = parts[0] ?? "";
  const view = (VIEWS as readonly string[]).includes(head) ? (head as ViewId) : ALIASES[head];
  if (!view) return DEFAULT_ROUTE;
  return { view, args: parts.slice(1) };
}

export function buildRoute(view: ViewId, ...args: (string | number | undefined | null)[]): string {
  const tail = args
    .filter((a): a is string | number => a !== undefined && a !== null && a !== "")
    .map((a) => encodeURIComponent(String(a)));
  return `#/${[view, ...tail].join("/")}`;
}

export function sameRoute(a: Route, b: Route): boolean {
  return a.view === b.view && a.args.length === b.args.length && a.args.every((x, i) => x === b.args[i]);
}
