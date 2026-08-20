/**
 * The properties panel: every C2 field an author may set, and nothing invented.
 *
 * The edge form is the important half. C2 v1.2 gives an edge five optional
 * properties beyond its endpoints — `gate`, `qos`, `speculative`, `deadline_ms`
 * and `priority` — and until now the only way to set any of them was to write
 * the IR by hand. Each is presented with the consequence rather than the name
 * alone, because "reliable / realtime / bulk" tells an author nothing about
 * which one drops frames.
 */

import { useTranslation } from "react-i18next";
import type { CanvasEdge, CanvasNode, Finding, ResolvedPorts } from "../../graph/model";
import type { SkillManifest } from "../../api/types";

interface Props {
  node: CanvasNode | null;
  edge: CanvasEdge | null;
  ports: ResolvedPorts | null;
  skills: SkillManifest[];
  findings: Finding[];
  destIsMotor: boolean;
  onNodeChange: (patch: Partial<CanvasNode>) => void;
  onEdgeChange: (patch: Partial<CanvasEdge>) => void;
  onDelete: () => void;
}

export function Inspector({
  node,
  edge,
  ports,
  skills,
  findings,
  destIsMotor,
  onNodeChange,
  onEdgeChange,
  onDelete,
}: Props) {
  const { t } = useTranslation();

  if (!node && !edge) {
    return (
      <aside className="inspector">
        <div className="inspector__empty">{t("studio.inspector.nothing")}</div>
      </aside>
    );
  }

  return (
    <aside className="inspector">
      {node && (
        <>
          <h3 className="inspector__title">{t("studio.inspector.node")}</h3>
          {node.isClient ? (
            <p className="inspector__note">{t("studio.inspector.clientNote")}</p>
          ) : (
            <>
              <label className="field">
                <span>{t("studio.inspector.ref")}</span>
                <input
                  className="input"
                  value={node.ref}
                  onChange={(e) => onNodeChange({ ref: e.target.value })}
                />
                <em>{t("studio.inspector.refHelp")}</em>
              </label>

              <label className="field">
                <span>{t("studio.inspector.resolve")}</span>
                <input
                  className="input"
                  list="aura-capabilities"
                  value={node.resolve ?? ""}
                  placeholder="cognitive.llm.chat"
                  onChange={(e) => onNodeChange({ resolve: e.target.value || undefined })}
                  disabled={!!node.use}
                />
                <em>{t("studio.inspector.resolveHelp")}</em>
              </label>

              <label className="field">
                <span>{t("studio.inspector.use")}</span>
                <input
                  className="input"
                  list="aura-packages"
                  value={node.use ?? ""}
                  placeholder="example/logical/echo"
                  onChange={(e) => onNodeChange({ use: e.target.value || undefined })}
                />
                <em>{t("studio.inspector.useHelp")}</em>
              </label>

              <datalist id="aura-capabilities">
                {[...new Set(skills.map((s) => s.capability))].map((c) => (
                  <option key={c} value={c} />
                ))}
              </datalist>
              <datalist id="aura-packages">
                {skills.map((s) => (
                  <option key={s.id} value={s.id} />
                ))}
              </datalist>

              {ports?.manifest && (
                <div className="inspector__manifest">
                  <div className="inspector__mrow">
                    <span>{t("studio.inspector.type")}</span>
                    <span className={`badge badge--${ports.manifest.type}`}>{ports.manifest.type}</span>
                  </div>
                  <div className="inspector__mrow">
                    <span>{t("studio.inspector.format")}</span>
                    <code>{ports.manifest.format}</code>
                  </div>
                  <p className="inspector__desc">{ports.manifest.description}</p>
                </div>
              )}
            </>
          )}
        </>
      )}

      {edge && (
        <>
          <h3 className="inspector__title">{t("studio.inspector.edge")}</h3>
          <p className="inspector__route">
            <code>
              {edge.fromRef}.{edge.fromPort}
            </code>
            <span>→</span>
            <code>
              {edge.toRef}.{edge.toPort}
            </code>
          </p>

          <label className="field">
            <span>{t("studio.inspector.gate")}</span>
            <select
              className="select"
              value={edge.gate ?? ""}
              onChange={(e) =>
                onEdgeChange({ gate: (e.target.value || undefined) as CanvasEdge["gate"] })
              }
            >
              <option value="">{t("studio.inspector.gateUnset")}</option>
              <option value="human-approval">human-approval</option>
              <option value="none">none</option>
            </select>
            <em>
              {destIsMotor
                ? t("studio.inspector.gateMotorHelp")
                : t("studio.inspector.gateHelp")}
            </em>
          </label>

          <label className="field">
            <span>{t("studio.inspector.qos")}</span>
            <select
              className="select"
              value={edge.qos ?? ""}
              onChange={(e) =>
                onEdgeChange({ qos: (e.target.value || undefined) as CanvasEdge["qos"] })
              }
            >
              <option value="">{t("studio.inspector.qosUnset")}</option>
              <option value="reliable">reliable — {t("studio.inspector.qosReliable")}</option>
              <option value="realtime">realtime — {t("studio.inspector.qosRealtime")}</option>
              <option value="bulk">bulk — {t("studio.inspector.qosBulk")}</option>
            </select>
          </label>

          <label className="field field--row">
            <input
              type="checkbox"
              checked={!!edge.speculative}
              onChange={(e) => onEdgeChange({ speculative: e.target.checked || undefined })}
            />
            <span>{t("studio.inspector.speculative")}</span>
          </label>
          <em className="field__note">{t("studio.inspector.speculativeHelp")}</em>

          <label className="field">
            <span>{t("studio.inspector.deadline")}</span>
            <input
              className="input"
              type="number"
              min={0}
              value={edge.deadline_ms ?? ""}
              placeholder="—"
              onChange={(e) =>
                onEdgeChange({
                  deadline_ms: e.target.value === "" ? undefined : Number(e.target.value),
                })
              }
            />
            <em>{t("studio.inspector.deadlineHelp")}</em>
          </label>

          <label className="field">
            <span>{t("studio.inspector.priority")}</span>
            <input
              className="input"
              type="number"
              value={edge.priority ?? ""}
              placeholder="—"
              onChange={(e) =>
                onEdgeChange({
                  priority: e.target.value === "" ? undefined : Number(e.target.value),
                })
              }
            />
            <em>{t("studio.inspector.priorityHelp")}</em>
          </label>
        </>
      )}

      {findings.length > 0 && (
        <div className="inspector__findings">
          {findings.map((f, i) => (
            <div key={i} className={`finding finding--${f.severity}`}>
              {f.rule && <span className="finding__rule">C2 §{f.rule}</span>}
              {f.message}
            </div>
          ))}
        </div>
      )}

      {(edge || (node && !node.isClient)) && (
        <button className="btn btn--ghost btn--sm inspector__del" onClick={onDelete}>
          {t("studio.inspector.delete")}
        </button>
      )}
    </aside>
  );
}
