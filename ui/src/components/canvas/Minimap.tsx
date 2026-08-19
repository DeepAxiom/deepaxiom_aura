/**
 * A minimap: where the graph is, and where you are looking at it.
 *
 * Earns its place once a planner graph is wider than the pane — which is the
 * common case, not the exceptional one. Click to jump; the viewport rectangle
 * is the part people actually read.
 *
 * Drawn from node boxes alone, never edges. Edges bow well outside their
 * endpoints and would make the map's bounds jump as an author drags a node
 * around, which is exactly the instability a minimap exists to remove.
 */

import type { CanvasNode } from "../../graph/model";
import { NODE_W } from "../../graph/model";

interface Props {
  nodes: CanvasNode[];
  heightOf: (n: CanvasNode) => number;
  /** Current viewport in world coordinates. */
  view: { x: number; y: number; k: number };
  surface: { w: number; h: number };
  onJump: (world: { x: number; y: number }) => void;
}

const MAP_W = 168;
const MAP_H = 108;
const PAD = 8;

export function Minimap({ nodes, heightOf, view, surface, onJump }: Props) {
  if (nodes.length === 0) return null;

  const xs = nodes.map((n) => n.x);
  const ys = nodes.map((n) => n.y);
  const minX = Math.min(...xs);
  const minY = Math.min(...ys);
  const maxX = Math.max(...xs) + NODE_W;
  const maxY = Math.max(...nodes.map((n) => n.y + heightOf(n)));

  const worldW = Math.max(maxX - minX, 1);
  const worldH = Math.max(maxY - minY, 1);
  const scale = Math.min((MAP_W - PAD * 2) / worldW, (MAP_H - PAD * 2) / worldH);

  const toMap = (x: number, y: number) => ({
    x: PAD + (x - minX) * scale,
    y: PAD + (y - minY) * scale,
  });

  // The viewport, expressed in world units, then mapped.
  const viewWorld = {
    x: -view.x / view.k,
    y: -view.y / view.k,
    w: surface.w / view.k,
    h: surface.h / view.k,
  };
  const vp = toMap(viewWorld.x, viewWorld.y);

  return (
    <svg
      className="minimap"
      width={MAP_W}
      height={MAP_H}
      onPointerDown={(e) => {
        const r = e.currentTarget.getBoundingClientRect();
        onJump({
          x: minX + (e.clientX - r.left - PAD) / scale,
          y: minY + (e.clientY - r.top - PAD) / scale,
        });
      }}
    >
      <rect className="minimap__bg" x={0} y={0} width={MAP_W} height={MAP_H} rx={6} />
      {nodes.map((n) => {
        const p = toMap(n.x, n.y);
        return (
          <rect
            key={n.ref}
            className={`minimap__node ${n.isClient ? "minimap__node--client" : ""}`}
            x={p.x}
            y={p.y}
            width={Math.max(2, NODE_W * scale)}
            height={Math.max(2, heightOf(n) * scale)}
            rx={1.5}
          />
        );
      })}
      <rect
        className="minimap__view"
        x={vp.x}
        y={vp.y}
        width={Math.max(4, viewWorld.w * scale)}
        height={Math.max(4, viewWorld.h * scale)}
        rx={2}
      />
    </svg>
  );
}
