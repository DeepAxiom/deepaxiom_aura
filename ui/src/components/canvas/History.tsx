/**
 * Every version of this graph that was ever registered.
 *
 * Named for the graph, not just `History`, because this codebase already has
 * one: the undo stack in graph/editor. They are different things and the two
 * names have to say so.
 *
 * Not an undo stack. Undo is what you did in this tab since you opened it;
 * this is what the *node* has: each registration that changed the document,
 * with the SHA-256 that identifies it. Two entries with the same digest are
 * the same graph, so a version can be recognised rather than guessed at from
 * a timestamp.
 *
 * Restoring loads the old IR onto the canvas and leaves it there unregistered.
 * It does not roll the node back, because that would make the history a thing
 * you can rewrite — and the one property worth having here is that it only
 * ever grows. Register the restored graph and the list gains an entry saying
 * so, which is the truth: you went back, deliberately, at a known time.
 */

import { useEffect, useState } from "react";
import { useTranslation } from "react-i18next";
import { api } from "../../api/client";
import type { GraphIR, GraphRevision } from "../../api/types";

interface Props {
  graphId: string;
  /** The digest of what is on the canvas, so "you are here" is visible. */
  currentDigest: string | null;
  onRestore: (ir: GraphIR, n: number) => void;
  onClose: () => void;
}

function when(ms: number): string {
  const s = Math.max(0, Math.round((Date.now() - ms) / 1000));
  if (s < 60) return `${s}s`;
  if (s < 3600) return `${Math.round(s / 60)}m`;
  if (s < 86400) return `${Math.round(s / 3600)}h`;
  return `${Math.round(s / 86400)}d`;
}

export function GraphHistory({ graphId, currentDigest, onRestore, onClose }: Props) {
  const { t } = useTranslation();
  const [revs, setRevs] = useState<GraphRevision[] | null>(null);
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(0);

  useEffect(() => {
    let alive = true;
    api.graphRevisions(graphId)
      .then((r) => { if (alive) setRevs(r); })
      .catch((e: Error) => { if (alive) setError(e.message); });
    return () => { alive = false; };
  }, [graphId]);

  return (
    <div className="shortcuts__scrim" onPointerDown={onClose}>
      <div className="history" onPointerDown={(e) => e.stopPropagation()} role="dialog">
        <header className="shortcuts__head">
          <span>{t("studio.history.title", { graph: graphId })}</span>
          <button className="btn btn--ghost btn--sm" onClick={onClose}>{t("common.close")}</button>
        </header>

        {error && <div className="alert">{error}</div>}
        {revs === null && !error && <div className="history__empty">{t("common.loading")}</div>}
        {revs?.length === 0 && <div className="history__empty">{t("studio.history.empty")}</div>}

        <div className="history__list">
          {revs?.map((r) => {
            const here = currentDigest !== null && r.digest === currentDigest;
            return (
              <div key={r.n} className={`history__row ${here ? "history__row--here" : ""}`}>
                <span className="history__n">#{r.n}</span>
                <code className="history__digest" title={r.digest}>{r.digest.slice(0, 12)}</code>
                <span className="history__meta">
                  {t("studio.history.meta", { when: when(r.created), bytes: r.bytes })}
                </span>
                {here && <span className="pill pill--live">{t("studio.history.here")}</span>}
                <button
                  className="btn btn--ghost btn--sm"
                  disabled={busy === r.n || here}
                  onClick={() => {
                    setBusy(r.n);
                    api.graphRevision(graphId, r.n)
                      .then((ir) => { onRestore(ir, r.n); onClose(); })
                      .catch((e: Error) => setError(e.message))
                      .finally(() => setBusy(0));
                  }}
                >
                  {t("studio.history.restore")}
                </button>
              </div>
            );
          })}
        </div>

        <p className="history__note">{t("studio.history.note")}</p>
      </div>
    </div>
  );
}
