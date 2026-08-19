/**
 * One node box on the canvas.
 *
 * Rendered as HTML rather than SVG: a node is mostly text, a couple of badges
 * and a stack of hit targets, all of which the browser lays out better in flow
 * than we would by hand in SVG. Edges stay in SVG underneath, where curves and
 * z-order are cheap. Both live inside the same transformed container, so one
 * pan/zoom moves them together and there is no second coordinate system to keep
 * in sync.
 */

import type { CanvasNode, ResolvedPorts } from "../../graph/model";
import { NODE_HEAD, NODE_W, PORT_PAD, PORT_ROW, nodeHeight } from "../../graph/model";

interface Props {
  node: CanvasNode;
  ports: ResolvedPorts;
  selected: boolean;
  /** Ports currently legal to drop on, while a link is being dragged. */
  dropTargets: Set<string> | null;
  candidates: number;
  onPointerDown: (e: React.PointerEvent, ref: string) => void;
  onSelect: (ref: string) => void;
  onPortDown: (e: React.PointerEvent, ref: string, port: string, side: "in" | "out") => void;
  onPortUp: (e: React.PointerEvent, ref: string, port: string, side: "in" | "out") => void;
}

export function GraphNode({
  node,
  ports,
  selected,
  dropTargets,
  candidates,
  onPointerDown,
  onSelect,
  onPortDown,
  onPortUp,
}: Props) {
  const h = nodeHeight(ports);
  const type = node.isClient ? "client" : (ports.type ?? "unknown");
  const rows = Math.max(ports.ingress.length, ports.egress.length);

  return (
    <div
      className={`gnode ${selected ? "gnode--sel" : ""} gnode--${type}`}
      style={{ left: node.x, top: node.y, width: NODE_W, height: h }}
      onPointerDown={(e) => onPointerDown(e, node.ref)}
      onClick={(e) => {
        e.stopPropagation();
        onSelect(node.ref);
      }}
    >
      <header className="gnode__head" style={{ height: NODE_HEAD }}>
        <span className="gnode__ref">{node.ref}</span>
        <span className="gnode__sub">
          {node.isClient ? "pseudo-node" : (node.use ?? node.resolve ?? "unresolved")}
        </span>
        {!node.isClient && (
          <span className={`gnode__type gnode__type--${type}`}>{type}</span>
        )}
      </header>

      <div className="gnode__ports" style={{ paddingTop: PORT_PAD, height: rows * PORT_ROW + PORT_PAD * 2 }}>
        <div className="gnode__col">
          {ports.ingress.map((p, i) => {
            const key = `${node.ref}.${p.name}`;
            const droppable = dropTargets?.has(key) ?? false;
            const dimmed = dropTargets !== null && !droppable;
            return (
              <div
                className={`gport gport--in ${droppable ? "gport--drop" : ""} ${dimmed ? "gport--dim" : ""}`}
                key={p.name}
                style={{ height: PORT_ROW, top: i * PORT_ROW }}
                title={`${p.name} — ${p.schema}`}
              >
                <span
                  className="gport__dot"
                  onPointerDown={(e) => onPortDown(e, node.ref, p.name, "in")}
                  onPointerUp={(e) => onPortUp(e, node.ref, p.name, "in")}
                />
                <span className="gport__name">{p.name}</span>
              </div>
            );
          })}
        </div>
        <div className="gnode__col gnode__col--right">
          {ports.egress.map((p, i) => {
            const dimmed = dropTargets !== null;
            return (
              <div
                className={`gport gport--out ${dimmed ? "gport--dim" : ""}`}
                key={p.name}
                style={{ height: PORT_ROW, top: i * PORT_ROW }}
                title={`${p.name} — ${p.schema}`}
              >
                <span className="gport__name">{p.name}</span>
                <span
                  className="gport__dot"
                  onPointerDown={(e) => onPortDown(e, node.ref, p.name, "out")}
                  onPointerUp={(e) => onPortUp(e, node.ref, p.name, "out")}
                />
              </div>
            );
          })}
        </div>
      </div>

      {!node.isClient && ports.egress.length === 0 && ports.ingress.length === 0 && (
        <div className="gnode__unresolved">
          no manifest in the catalogue — ports unknown
        </div>
      )}
      {candidates > 1 && (
        <span className="gnode__badge" title="More than one skill satisfies this capability">
          {candidates} candidates
        </span>
      )}
    </div>
  );
}
