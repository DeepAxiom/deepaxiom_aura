/**
 * Number and time formatting for the control plane.
 *
 * One rule runs through all of it: a value the node did not report is `null`
 * and renders as an em dash, never as zero. "0 tokens" and "this node does not
 * count tokens" are different facts and a dashboard that prints the same glyph
 * for both is lying about one of them.
 */

export const NOT_REPORTED = "—";

export function fmtInt(n: number | null | undefined): string {
  if (n === null || n === undefined || !Number.isFinite(n)) return NOT_REPORTED;
  return Math.round(n).toString().replace(/\B(?=(\d{3})+(?!\d))/g, ",");
}

/** 1234 → "1.2k", 3400000 → "3.4M". Below 1000 the integer itself. */
export function fmtCompact(n: number | null | undefined): string {
  if (n === null || n === undefined || !Number.isFinite(n)) return NOT_REPORTED;
  const abs = Math.abs(n);
  if (abs < 1000) return String(Math.round(n * 10) / 10).replace(/\.0$/, "");
  const units: [number, string][] = [[1e9, "B"], [1e6, "M"], [1e3, "k"]];
  for (const [size, suffix] of units) {
    if (abs >= size) {
      const v = n / size;
      return (Math.abs(v) >= 100 ? v.toFixed(0) : v.toFixed(1)).replace(/\.0$/, "") + suffix;
    }
  }
  return String(n);
}

export function fmtBytes(n: number | null | undefined): string {
  if (n === null || n === undefined || !Number.isFinite(n)) return NOT_REPORTED;
  if (n < 1024) return `${n} B`;
  const units = ["KiB", "MiB", "GiB", "TiB"];
  let v = n / 1024;
  let i = 0;
  while (v >= 1024 && i < units.length - 1) { v /= 1024; i += 1; }
  return `${v >= 100 ? v.toFixed(0) : v.toFixed(1)} ${units[i]}`;
}

/** A ratio in [0,1] as a percentage; null stays not-reported. */
export function fmtPct(r: number | null | undefined, digits = 0): string {
  if (r === null || r === undefined || !Number.isFinite(r)) return NOT_REPORTED;
  return `${(r * 100).toFixed(digits)}%`;
}

/** Seconds as the two largest units: 93784 → "1d 2h", 75 → "1m 15s". */
export function fmtUptime(seconds: number | null | undefined): string {
  if (seconds === null || seconds === undefined || !Number.isFinite(seconds)) return NOT_REPORTED;
  const s = Math.max(0, Math.floor(seconds));
  const parts: [number, string][] = [
    [Math.floor(s / 86400), "d"], [Math.floor((s % 86400) / 3600), "h"],
    [Math.floor((s % 3600) / 60), "m"], [s % 60, "s"],
  ];
  const first = parts.findIndex(([v]) => v > 0);
  if (first < 0) return "0s";
  return parts.slice(first, first + 2).map(([v, u]) => `${v}${u}`).join(" ");
}

/** Milliseconds at the precision a person reads: 0.4 ms, 12 ms, 1.24 s, 2m 05s. */
export function fmtMs(ms: number | null | undefined): string {
  if (ms === null || ms === undefined || !Number.isFinite(ms)) return NOT_REPORTED;
  if (ms < 1) return `${ms.toFixed(2).replace(/0$/, "")} ms`;
  if (ms < 1000) return `${Math.round(ms)} ms`;
  if (ms < 60_000) return `${(ms / 1000).toFixed(2)} s`;
  const m = Math.floor(ms / 60_000);
  const s = Math.floor((ms % 60_000) / 1000);
  return `${m}m ${String(s).padStart(2, "0")}s`;
}

/** How long ago `then` was, relative to `now` (both unix millis). */
export function fmtAgo(then: number, now: number): string {
  if (!then) return NOT_REPORTED;
  const s = Math.max(0, Math.round((now - then) / 1000));
  if (s < 60) return `${s}s`;
  if (s < 3600) return `${Math.round(s / 60)}m`;
  if (s < 86400) return `${Math.round(s / 3600)}h`;
  return `${Math.round(s / 86400)}d`;
}

/** Shorten an id or hash for a table cell, keeping both ends recognisable. */
export function shortId(id: string | undefined, head = 6, tail = 4): string {
  if (!id) return "";
  const body = id.replace(/^sha256:/, "");
  if (body.length <= head + tail + 1) return body;
  return `${body.slice(0, head)}…${body.slice(-tail)}`;
}

/** `std/text@1` → `text@1`. The namespace is noise on a 90px port row. */
export function shortSchema(schema: string | undefined): string {
  if (!schema) return "";
  const slash = schema.lastIndexOf("/");
  return slash < 0 ? schema : schema.slice(slash + 1);
}
