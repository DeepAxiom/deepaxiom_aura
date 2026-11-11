/**
 * Shared canvas geometry: the edge curve, and the anchor an edge starts from.
 *
 * Extracted because two views draw the same graph — the editor, where you build
 * one, and the studio, where you watch one run. Two copies of a bezier would
 * drift by a few pixels and the same graph would look subtly different
 * depending on which screen you were on.
 */

import { NODE_W, type CanvasNode, type ResolvedPorts, portOffsetY } from "./model";

export interface Point {
  x: number;
  y: number;
}

/** How far below its ports a right-to-left edge runs, and how far it swings out. */
export const BACK_DROP = 84;
export const BACK_SWING = 56;
/** Vertical gap between two right-to-left edges, so they do not draw as one line. */
export const BACK_LANE = 16;

/** True when an edge runs right-to-left: the reply hop most graphs have. */
export function isBackward(a: Point, b: Point): boolean {
  return b.x < a.x - 8;
}

/**
 * The path between two ports.
 *
 * Left-to-right it is a cubic bezier with the control points pushed
 * horizontally: the push scales with the gap so short hops stay tight and long
 * ones bow enough to be followed, with a floor so a short hop still curves.
 *
 * Right-to-left it goes *under*: out of the egress, down, back along a
 * straight run below the nodes, and up into the ingress. A single bezier for
 * that case is a flat S drawn straight through every node between the two
 * ends — the reply edge of a four-node chain crossed out the whole graph.
 */
export function curve(a: Point, b: Point, lane = 0): string {
  if (isBackward(a, b)) {
    const y = Math.max(a.y, b.y) + BACK_DROP + lane * BACK_LANE;
    const s = BACK_SWING + lane * 8;
    return (
      `M ${a.x} ${a.y} C ${a.x + s} ${a.y}, ${a.x + s} ${y}, ${a.x} ${y} ` +
      `L ${b.x} ${y} C ${b.x - s} ${y}, ${b.x - s} ${b.y}, ${b.x} ${b.y}`
    );
  }
  const dx = Math.max(40, Math.abs(b.x - a.x) * 0.5);
  return `M ${a.x} ${a.y} C ${a.x + dx} ${a.y}, ${b.x - dx} ${b.y}, ${b.x} ${b.y}`;
}

/** Where an edge's label sits: on the path, halfway along. */
export function edgeMid(a: Point, b: Point, lane = 0): Point {
  if (isBackward(a, b)) return { x: (a.x + b.x) / 2, y: Math.max(a.y, b.y) + BACK_DROP + lane * BACK_LANE };
  return { x: (a.x + b.x) / 2, y: (a.y + b.y) / 2 };
}

/**
 * World-space point where an edge meets a port: the left face for an ingress,
 * the right face for an egress, at that port's own row.
 *
 * Returns null when the port is not one the node declares — an edge naming a
 * port that does not exist is drawn as nothing rather than at the origin, where
 * it would look like a real connection to the top-left corner of the canvas.
 */
export function portAnchor(
  node: CanvasNode | undefined,
  ports: ResolvedPorts | undefined,
  port: string,
  side: "in" | "out",
): Point | null {
  if (!node || !ports) return null;
  const list = side === "in" ? ports.ingress : ports.egress;
  const i = list.findIndex((p) => p.name === port);
  if (i < 0) return null;
  return { x: node.x + (side === "in" ? 0 : NODE_W), y: node.y + portOffsetY(i) };
}
