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

/**
 * A cubic bezier between two ports, with the control points pushed
 * horizontally. The push scales with the gap so short hops stay tight and long
 * ones bow enough to be followed across a busy graph, and it has a floor so a
 * node wired back to one on its left still bulges instead of folding into a
 * straight line through both boxes.
 */
export function curve(a: Point, b: Point): string {
  const dx = Math.max(40, Math.abs(b.x - a.x) * 0.5);
  return `M ${a.x} ${a.y} C ${a.x + dx} ${a.y}, ${b.x - dx} ${b.y}, ${b.x} ${b.y}`;
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
