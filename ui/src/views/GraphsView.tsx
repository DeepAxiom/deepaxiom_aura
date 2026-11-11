/**
 * Every graph registered on this node, with what it is made of and whether it
 * is running. The canvas is one click away; this is the list you scan.
 */

import { useEffect, useState } from "react";
import { useTranslation } from "react-i18next";
import { api } from "../api/client";
import type { GraphIR } from "../api/types";
import { navigate, useNow } from "../app/store";
import { IconCode, IconStudio } from "../components/Icons";
import { Empty, ErrorNote, Loading } from "../components/ui";
import { fmtAgo } from "../lib/format";
import { useLiveGraphs } from "../hooks/useLiveGraphs";

export function GraphsView() {
  const { t } = useTranslation();
  const live = useLiveGraphs(true, 3000);
  const now = useNow(5000);
  const [detail, setDetail] = useState<Record<string, GraphIR>>({});
  const [open, setOpen] = useState<string | null>(null);
  const [error, setError] = useState("");

  // The IR of each graph, fetched once per id: the row shows node and edge
  // counts, which the id list alone does not carry.
  useEffect(() => {
    for (const id of live.graphs) {
      if (detail[id]) continue;
      api.graph(id)
        .then((ir) => setDetail((prev) => ({ ...prev, [id]: ir })))
        .catch((e) => setError((e as Error).message));
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [live.graphs]);

  return (
    <section className="view">
      <p className="lead">{t("graphs.subtitle")}</p>
      {(error || live.error) && <ErrorNote message={error || live.error} />}
      {!live.loaded && <Loading />}
      {live.loaded && live.graphs.length === 0 && (
        <Empty
          title={t("graphs.empty")}
          action={<button className="btn btn--primary" onClick={() => navigate("studio")}>{t("graphs.build")}</button>}
        />
      )}
      {live.graphs.length > 0 && (
        <div className="table-wrap">
          <table className="table table--rows">
            <thead>
              <tr>
                <th>{t("graphs.col.id")}</th>
                <th>{t("graphs.col.origin")}</th>
                <th className="num">{t("graphs.col.nodes")}</th>
                <th className="num">{t("graphs.col.edges")}</th>
                <th>{t("graphs.col.gates")}</th>
                <th className="num">{t("graphs.col.live")}</th>
                <th className="num">{t("graphs.col.sessions")}</th>
                <th>{t("graphs.col.last")}</th>
                <th />
              </tr>
            </thead>
            <tbody>
              {live.graphs.map((id) => {
                const ir = detail[id];
                const a = live.activity.get(id);
                const gates = ir?.edges.filter((e) => e.gate === "human-approval").length ?? 0;
                return [
                  <tr key={id}>
                    <td><a className="mono strong" href={`#/studio/${encodeURIComponent(id)}`}>{id}</a></td>
                    <td>{ir ? <span className={`pill pill--${ir.origin.kind === "planner" ? "info" : "neutral"}`}>{t(`graphs.origin.${ir.origin.kind}`)}</span> : "…"}</td>
                    <td className="num">{ir?.nodes.length ?? "…"}</td>
                    <td className="num">{ir?.edges.length ?? "…"}</td>
                    <td>{gates > 0 ? <span className="pill pill--warn">{t("graphs.gated", { count: gates })}</span> : <span className="cell-na">0</span>}</td>
                    <td className="num">{a?.live ? <span className="pill pill--live">{a.live}</span> : 0}</td>
                    <td className="num">{a?.sessions ?? 0}{a?.errors ? <span className="cell-err"> · {t("sessions.errors", { count: a.errors })}</span> : null}</td>
                    <td>{a?.lastStarted ? t("graphs.ago", { when: fmtAgo(a.lastStarted, now) }) : t("studio.live.neverRan")}</td>
                    <td className="row-actions">
                      <button className="iconbtn" title={t("graphs.viewIR")} aria-label={t("graphs.viewIR")} aria-expanded={open === id} onClick={() => setOpen(open === id ? null : id)}>
                        <IconCode size={15} />
                      </button>
                      <button className="iconbtn" title={t("graphs.openStudio")} aria-label={t("graphs.openStudio")} onClick={() => navigate("studio", id)}>
                        <IconStudio size={15} />
                      </button>
                    </td>
                  </tr>,
                  open === id && ir && (
                    <tr key={`${id}-ir`} className="row-detail">
                      <td colSpan={9}><pre className="code">{JSON.stringify(ir, null, 2)}</pre></td>
                    </tr>
                  ),
                ];
              })}
            </tbody>
          </table>
        </div>
      )}
    </section>
  );
}
