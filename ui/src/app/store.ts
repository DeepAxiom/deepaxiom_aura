/**
 * The few pieces of state the whole shell shares: where you are, which theme,
 * what just failed, and what the node last said about itself.
 *
 * Module-level stores read through `useSyncExternalStore`, not a context
 * provider and not a state library: there are four of them, none is nested,
 * and anything outside React (the API client, a keyboard handler) can write
 * to them without a hook.
 */

import { useEffect, useRef, useState, useSyncExternalStore } from "react";
import { api, currentLink, onLink, type Link } from "../api/client";
import type { Health } from "../api/types";
import { type Route, type ViewId, buildRoute, parseRoute } from "../lib/route";
import { type NodeStats, pushBounded } from "../lib/stats";

function createStore<T>(initial: T) {
  let value = initial;
  const subs = new Set<() => void>();
  return {
    get: () => value,
    set: (next: T) => {
      if (Object.is(next, value)) return;
      value = next;
      for (const fn of subs) fn();
    },
    subscribe: (fn: () => void) => {
      subs.add(fn);
      return () => { subs.delete(fn); };
    },
  };
}

/* ── route ──────────────────────────────────────────────────────── */

const routeStore = createStore<Route>(parseRoute(location.hash));
window.addEventListener("hashchange", () => routeStore.set(parseRoute(location.hash)));

export function useRoute(): Route {
  return useSyncExternalStore(routeStore.subscribe, routeStore.get);
}

export function navigate(view: ViewId, ...args: (string | number | undefined | null)[]): void {
  const next = buildRoute(view, ...args);
  if (location.hash !== next) location.hash = next;
}

/** Change the address without adding a history entry (a selection, not a navigation). */
export function replaceRoute(view: ViewId, ...args: (string | number | undefined | null)[]): void {
  const next = buildRoute(view, ...args);
  if (location.hash === next) return;
  history.replaceState(null, "", location.pathname + location.search + next);
  routeStore.set(parseRoute(next));
}

/* ── theme ──────────────────────────────────────────────────────── */

export type Theme = "dark" | "light";
const THEME_KEY = "aura.theme";

function systemTheme(): Theme {
  return window.matchMedia?.("(prefers-color-scheme: light)").matches ? "light" : "dark";
}

function storedTheme(): Theme | null {
  const v = localStorage.getItem(THEME_KEY);
  return v === "dark" || v === "light" ? v : null;
}

const themeStore = createStore<Theme>(storedTheme() ?? systemTheme());

function applyTheme(t: Theme): void {
  document.documentElement.dataset.theme = t;
}
applyTheme(themeStore.get());

// Follow the system until the person has chosen; after that their choice stands.
window.matchMedia?.("(prefers-color-scheme: light)").addEventListener?.("change", () => {
  if (storedTheme()) return;
  themeStore.set(systemTheme());
  applyTheme(themeStore.get());
});

export function useTheme(): Theme {
  return useSyncExternalStore(themeStore.subscribe, themeStore.get);
}

export function setTheme(t: Theme): void {
  localStorage.setItem(THEME_KEY, t);
  themeStore.set(t);
  applyTheme(t);
}

export function toggleTheme(): void {
  setTheme(themeStore.get() === "dark" ? "light" : "dark");
}

/* ── toasts ─────────────────────────────────────────────────────── */

export interface Toast {
  id: number;
  kind: "error" | "ok" | "info";
  text: string;
}

const toastStore = createStore<Toast[]>([]);
let toastSeq = 0;

export function toast(kind: Toast["kind"], text: string): void {
  const current = toastStore.get();
  // The same failure every poll would otherwise stack a column of copies.
  if (current.some((t) => t.kind === kind && t.text === text)) return;
  toastSeq += 1;
  const id = toastSeq;
  toastStore.set([...current.slice(-3), { id, kind, text }]);
  setTimeout(() => dismissToast(id), kind === "error" ? 9000 : 4000);
}

/** A failure, in the kernel's own words. */
export function toastError(e: unknown): void {
  toast("error", e instanceof Error ? e.message : String(e));
}

export function dismissToast(id: number): void {
  toastStore.set(toastStore.get().filter((t) => t.id !== id));
}

export function useToasts(): Toast[] {
  return useSyncExternalStore(toastStore.subscribe, toastStore.get);
}

/* ── node link + identity ───────────────────────────────────────── */

export function useLink(): Link {
  return useSyncExternalStore(onLink, currentLink);
}

/** /healthz, refreshed slowly: identity does not change, reachability does. */
export function useHealth(): Health | null {
  const [health, setHealth] = useState<Health | null>(null);
  useEffect(() => {
    let alive = true;
    const load = () => api.health().then((h) => alive && setHealth(h)).catch(() => alive && setHealth(null));
    void load();
    const timer = setInterval(load, 5000);
    return () => { alive = false; clearInterval(timer); };
  }, []);
  return health;
}

/* ── stats poll ─────────────────────────────────────────────────── */

export interface StatsFeed {
  /** Newest last. Empty until the first poll resolves. */
  samples: NodeStats[];
  latest: NodeStats | null;
  /** "unsupported": this node has no /v1/stats. */
  state: "loading" | "live" | "unsupported" | "error";
  error: string;
}

/** How many one-second samples the Pulse charts keep. */
export const STATS_WINDOW = 90;

/**
 * Poll /v1/stats while the tab is visible.
 *
 * A hidden tab stops asking: the numbers are for a person, and a node should
 * not answer a question nobody is looking at once a second.
 */
export function useStats(intervalMs = 1000): StatsFeed {
  const [feed, setFeed] = useState<StatsFeed>({ samples: [], latest: null, state: "loading", error: "" });
  const busy = useRef(false);

  useEffect(() => {
    let alive = true;
    let timer: ReturnType<typeof setInterval> | null = null;

    const poll = async () => {
      if (busy.current) return;
      busy.current = true;
      try {
        const s = await api.stats();
        if (!alive) return;
        if (s === null) {
          setFeed((f) => ({ ...f, state: "unsupported", error: "" }));
          stop(); // an older node will not grow the route while we watch
          return;
        }
        setFeed((f) => ({
          samples: pushBounded(f.samples, s, STATS_WINDOW), latest: s, state: "live", error: "",
        }));
      } catch (e) {
        if (alive) setFeed((f) => ({ ...f, state: "error", error: (e as Error).message }));
      } finally {
        busy.current = false;
      }
    };
    const start = () => {
      if (timer !== null) return;
      void poll();
      timer = setInterval(() => void poll(), intervalMs);
    };
    const stop = () => {
      if (timer !== null) clearInterval(timer);
      timer = null;
    };
    const onVisibility = () => (document.hidden ? stop() : start());

    if (!document.hidden) start();
    document.addEventListener("visibilitychange", onVisibility);
    return () => {
      alive = false;
      stop();
      document.removeEventListener("visibilitychange", onVisibility);
    };
  }, [intervalMs]);

  return feed;
}

/** Re-render on an interval, for "12s ago" labels. Paused while hidden. */
export function useNow(intervalMs = 1000): number {
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    const timer = setInterval(() => { if (!document.hidden) setNow(Date.now()); }, intervalMs);
    return () => clearInterval(timer);
  }, [intervalMs]);
  return now;
}
