/**
 * The live rail: what the node is running, beside what you are drawing.
 *
 * A graph here may never have been opened in this UI — the planner registers
 * one from a sentence, a federated peer brings its own. Clicking a row loads it
 * onto the canvas, which is the shortest path from "something is running" to
 * "show me what it actually wired".
 */

import { useTranslation } from "react-i18next";
import type { GraphActivity } from "../../hooks/useLiveGraphs";

interface Props {
  activity: Map<string, GraphActivity>;
  openId: string;
  following: boolean;
  error: string;
  onToggleFollow: () => void;
  onOpen: (graphId: string) => void;
}

/** Newest first, but anything live outranks anything idle. */
function ordered(activity: Map<string, GraphActivity>): GraphActivity[] {
  return [...activity.values()].sort((a, b) => {
    if ((b.live > 0 ? 1 : 0) !== (a.live > 0 ? 1 : 0)) return (b.live > 0 ? 1 : 0) - (a.live > 0 ? 1 : 0);
    return b.lastStarted - a.lastStarted;
  });
}

function ago(ms: number, never: string): string {
  if (!ms) return never;
  const s = Math.max(0, Math.round((Date.now() - ms) / 1000));
  if (s < 60) return `${s}s`;
  if (s < 3600) return `${Math.round(s / 60)}m`;
  if (s < 86400) return `${Math.round(s / 3600)}h`;
  return `${Math.round(s / 86400)}d`;
}

export function LiveRail({ activity, openId, following, error, onToggleFollow, onOpen }: Props) {
  const { t } = useTranslation();
  const rows = ordered(activity);
  const liveCount = rows.filter((r) => r.live > 0).length;

  return (
    <aside className="rail">
      <header className="rail__head">
        <span className="rail__title">{t("studio.live.title")}</span>
        <button
          className={`rail__follow ${following ? "rail__follow--on" : ""}`}
          onClick={onToggleFollow}
          title={t("studio.live.followHelp")}
        >
          <span className={`rail__pulse ${following && liveCount > 0 ? "rail__pulse--beating" : ""}`} />
          {following ? t("studio.live.following") : t("studio.live.paused")}
        </button>
      </header>

      {error && <div className="rail__error">{error}</div>}

      {rows.length === 0 && <div className="rail__empty">{t("studio.live.empty")}</div>}

      <div className="rail__list">
        {rows.map((r) => (
          <button
            key={r.graphId}
            className={`rail__row ${r.graphId === openId ? "rail__row--open" : ""}`}
            onClick={() => onOpen(r.graphId)}
          >
            <span className={`rail__dot ${r.live > 0 ? "rail__dot--live" : ""}`} />
            <span className="rail__id">{r.graphId}</span>
            {r.live > 0 && <span className="rail__badge">{t("studio.live.n", { count: r.live })}</span>}
            <span className="rail__meta">
              {r.sessions > 0
                ? t("studio.live.meta", { events: r.events, when: ago(r.lastStarted, "") })
                : t("studio.live.neverRan")}
            </span>
            {r.errors > 0 && <span className="rail__errors">{r.errors}</span>}
          </button>
        ))}
      </div>
    </aside>
  );
}
