/**
 * Frames: a labelled region of the canvas that moves what it contains.
 *
 * The same storage bargain as notes and positions — local, never in the IR.
 * A frame says "these four nodes are the retry path"; the runtime has no
 * opinion about that and must not learn one, or two graphs that run
 * identically stop being byte-identical on the wire.
 *
 * **Membership is geometric, never stored.** A frame contains whatever is
 * inside it right now, so dragging a node into one joins it and dragging it
 * out leaves — with no gesture to learn and nothing to go stale. The
 * alternative, an explicit member list, has to be reconciled every time a node
 * is renamed, deleted, pasted or bypassed, and every one of those is a chance
 * for a frame to claim a node that is no longer there.
 *
 * The cost is that two overlapping frames both own what they overlap. That is
 * the honest answer to an ambiguous drawing, and it is the same answer the
 * author's eye gives.
 */

import type { CanvasNode } from "./model";

const KEY = "aura.canvas.groups";

/** Space left around the nodes a frame is drawn to enclose. */
export const GROUP_PAD = 28;
/** Room at the top for the label, so a frame never covers a node's header. */
export const GROUP_HEAD = 26;
export const GROUP_MIN = 120;

export const GROUP_COLORS = ["violet", "teal", "amber", "rose", "slate"] as const;
export type GroupColor = (typeof GROUP_COLORS)[number];

export interface Group {
  id: string;
  x: number;
  y: number;
  w: number;
  h: number;
  label: string;
  color: GroupColor;
}

type GroupBook = Record<string, Group[]>;

function readBook(): GroupBook {
  try {
    return JSON.parse(localStorage.getItem(KEY) ?? "{}") as GroupBook;
  } catch {
    return {};
  }
}

function isGroup(v: unknown): v is Group {
  const g = v as Group;
  return (
    !!g && typeof g.id === "string" &&
    typeof g.x === "number" && typeof g.y === "number" &&
    typeof g.w === "number" && typeof g.h === "number" &&
    typeof g.label === "string" &&
    (GROUP_COLORS as readonly string[]).includes(g.color)
  );
}

export function loadGroups(graphId: string): Group[] {
  if (!graphId) return [];
  const found = readBook()[graphId];
  return Array.isArray(found) ? found.filter(isGroup) : [];
}

export function saveGroups(graphId: string, groups: Group[]): void {
  if (!graphId) return;
  try {
    const book = readBook();
    if (groups.length === 0) delete book[graphId];
    else book[graphId] = groups;
    localStorage.setItem(KEY, JSON.stringify(book));
  } catch {
    /* a frame is a convenience; losing it must not break the canvas */
  }
}

/* ── operations ──────────────────────────────────────────────────── */

export interface Box {
  x: number;
  y: number;
  w: number;
  h: number;
}

/** Draw a frame around the given nodes, with room for its label. */
export function frame(
  groups: Group[],
  nodes: CanvasNode[],
  sizeOf: (n: CanvasNode) => { w: number; h: number },
  label = "",
): { groups: Group[]; id: string } | null {
  if (nodes.length === 0) return null;

  const boxes = nodes.map((n) => ({ ...sizeOf(n), x: n.x, y: n.y }));
  const x = Math.min(...boxes.map((b) => b.x)) - GROUP_PAD;
  const y = Math.min(...boxes.map((b) => b.y)) - GROUP_PAD - GROUP_HEAD;
  const right = Math.max(...boxes.map((b) => b.x + b.w)) + GROUP_PAD;
  const bottom = Math.max(...boxes.map((b) => b.y + b.h)) + GROUP_PAD;

  const id = `g${Date.now().toString(36)}-${groups.length}`;
  const group: Group = {
    id,
    x: Math.round(x),
    y: Math.round(y),
    w: Math.max(GROUP_MIN, Math.round(right - x)),
    h: Math.max(GROUP_MIN, Math.round(bottom - y)),
    label,
    color: GROUP_COLORS[groups.length % GROUP_COLORS.length],
  };
  return { groups: [...groups, group], id };
}

/**
 * The nodes a frame currently holds.
 *
 * A node counts as inside when its whole box is, not when it merely overlaps.
 * Half a node hanging over an edge is the author mid-drag, and moving the
 * frame should not drag along something they were taking out of it.
 */
export function contained(
  group: Box,
  nodes: CanvasNode[],
  sizeOf: (n: CanvasNode) => { w: number; h: number },
): string[] {
  return nodes
    .filter((n) => {
      const { w, h } = sizeOf(n);
      return (
        n.x >= group.x && n.y >= group.y &&
        n.x + w <= group.x + group.w && n.y + h <= group.y + group.h
      );
    })
    .map((n) => n.ref);
}

export function patchGroup(groups: Group[], id: string, patch: Partial<Group>): Group[] {
  return groups.map((g) => (g.id === id ? { ...g, ...patch, id: g.id } : g));
}

export function removeGroup(groups: Group[], id: string): Group[] {
  return groups.filter((g) => g.id !== id);
}

export function moveGroup(groups: Group[], id: string, dx: number, dy: number): Group[] {
  return groups.map((g) => (g.id === id ? { ...g, x: g.x + dx, y: g.y + dy } : g));
}

export function resizeGroup(groups: Group[], id: string, dw: number, dh: number): Group[] {
  return groups.map((g) =>
    g.id === id ? { ...g, w: Math.max(GROUP_MIN, g.w + dw), h: Math.max(GROUP_MIN, g.h + dh) } : g,
  );
}

export function cycleGroupColor(groups: Group[], id: string): Group[] {
  return groups.map((g) => {
    if (g.id !== id) return g;
    const i = GROUP_COLORS.indexOf(g.color);
    return { ...g, color: GROUP_COLORS[(i + 1) % GROUP_COLORS.length] };
  });
}
