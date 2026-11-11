/**
 * One node box on the canvas.
 *
 * Rendered as HTML rather than SVG: a node is mostly text, a couple of badges
 * and a stack of hit targets, all of which the browser lays out better in flow
 * than we would by hand in SVG. Edges stay in SVG underneath, where curves and
 * z-order are cheap. Both live inside the same transformed container, so one
 * pan/zoom moves them together and there is no second coordinate system to keep
 * in sync.
 *
 * The box itself never changes size while a session runs: what a node is
 * saying hangs below it (`gnode__stream`), outside the geometry the edges are
 * anchored to.
 */

import { useTranslation } from "react-i18next";
import type { CanvasNode, Finding, ResolvedPorts } from "../../graph/model";
import { NODE_HEAD, NODE_W, PORT_PAD, PORT_ROW, nodeHeight } from "../../graph/model";
import { fmtCompact, shortSchema } from "../../lib/format";
import type { Usage } from "../../lib/trace";
import { IconAlert, TYPE_GLYPH } from "../Icons";

interface Props {
  node: CanvasNode;
  ports: ResolvedPorts;
  selected: boolean;
  /** Ports currently legal to drop on, while a link is being dragged. */
  dropTargets: Set<string> | null;
  candidates: number;
  /** Validation findings located on this node. */
  findings?: Finding[];
  /** Envelopes this node has handled in the session being watched. */
  count?: number;
  /** The tail of what this node is emitting right now. */
  stream?: { text: string; open: boolean };
  usage?: Usage;
  onPointerDown: (e: React.PointerEvent, ref: string) => void;
  onSelect: (ref: string) => void;
  onPortDown: (e: React.PointerEvent, ref: string, port: string, side: "in" | "out") => void;
  onPortUp: (e: React.PointerEvent, ref: string, port: string, side: "in" | "out") => void;
}

export function GraphNode({
  node, ports, selected, dropTargets, candidates, findings = [], count = 0, stream, usage,
  onPointerDown, onSelect, onPortDown, onPortUp,
}: Props) {
  const { t } = useTranslation();
  const h = nodeHeight(ports);
  const type = node.isClient ? "client" : (ports.type ?? "unknown");
  const rows = Math.max(ports.ingress.length, ports.egress.length);
  const Glyph = TYPE_GLYPH[type];
  const worst = findings.some((f) => f.severity === "error") ? "error" : findings.some((f) => f.severity === "warn") ? "warn" : "";
  const flagged = findings.filter((f) => f.severity !== "info");

  return (
    <div
      className={`gnode ${selected ? "gnode--sel" : ""} gnode--${type} ${worst ? `gnode--${worst}` : ""}`}
      style={{ left: node.x, top: node.y, width: NODE_W, height: h }}
      data-ref={node.ref}
      onPointerDown={(e) => onPointerDown(e, node.ref)}
      onClick={(e) => {
        e.stopPropagation();
        onSelect(node.ref);
      }}
    >
      <header className="gnode__head" style={{ height: NODE_HEAD }}>
        <span className="gnode__glyph" aria-hidden>{Glyph ? <Glyph size={14} /> : "?"}</span>
        <span className="gnode__titles">
          <span className="gnode__ref gnode__name">{node.ref}</span>
          <span className="gnode__sub">
            {node.isClient ? t("studio.node.client") : (node.use ?? node.resolve ?? t("studio.node.unresolved"))}
          </span>
        </span>
        {!node.isClient && <span className={`gnode__type gnode__type--${type}`}>{type}</span>}
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
                title={`${p.name} — ${p.schema || t("studio.node.anySchema")}`}
              >
                <span
                  className="gport__dot"
                  onPointerDown={(e) => onPortDown(e, node.ref, p.name, "in")}
                  onPointerUp={(e) => onPortUp(e, node.ref, p.name, "in")}
                />
                <span className="gport__name">{p.name}</span>
                {p.schema && <span className="gport__schema">{shortSchema(p.schema)}</span>}
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
                title={`${p.name} — ${p.schema || t("studio.node.anySchema")}`}
              >
                {p.schema && <span className="gport__schema">{shortSchema(p.schema)}</span>}
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
        <div className="gnode__unresolved">{t("studio.node.noManifest")}</div>
      )}

      {flagged.length > 0 && (
        <span className={`gnode__flag gnode__flag--${worst}`} title={flagged.map((f) => f.message).join("\n")}>
          <IconAlert size={11} />{flagged.length}
        </span>
      )}
      {candidates > 1 && (
        <span className="gnode__badge" title={t("studio.node.candidatesHelp")}>
          {t("studio.node.candidates", { count: candidates })}
        </span>
      )}
      {count > 0 && (
        <span className="gnode__count" title={t("studio.node.countHelp")}>
          {fmtCompact(count)}
          {usage && <b>{fmtCompact(usage.input_tokens ?? null)}→{fmtCompact(usage.output_tokens ?? null)} tok</b>}
        </span>
      )}
      {stream && stream.text && (
        <div className={`gnode__stream ${stream.open ? "gnode__stream--open" : ""}`} style={{ top: h + 6 }} aria-live="off">
          <span>
            {stream.text}
            {stream.open && <span className="cursor" />}
          </span>
        </div>
      )}
    </div>
  );
}
