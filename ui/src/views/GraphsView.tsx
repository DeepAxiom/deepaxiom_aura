import { useEffect, useState } from "react";
import { useTranslation } from "react-i18next";
import { api } from "../api/client";
import type { GraphIR } from "../api/types";

export function GraphsView() {
  const { t } = useTranslation();
  const [graphs, setGraphs] = useState<string[]>([]);
  const [detail, setDetail] = useState<Record<string, GraphIR | null>>({});
  const [error, setError] = useState("");

  useEffect(() => {
    api.graphs().then(setGraphs).catch((e) => setError((e as Error).message));
  }, []);

  const toggle = async (id: string) => {
    if (detail[id] !== undefined) {
      setDetail((prev) => {
        const next = { ...prev };
        delete next[id];
        return next;
      });
      return;
    }
    setDetail((prev) => ({ ...prev, [id]: null }));
    try {
      const ir = await api.graph(id);
      setDetail((prev) => ({ ...prev, [id]: ir }));
    } catch (e) {
      setError((e as Error).message);
    }
  };

  return (
    <section className="view">
      <div className="page-head">
        <h1>{t("graphs.title")}</h1>
        <p>{t("graphs.subtitle")}</p>
      </div>
      {error && <div className="error-banner">{t("common.error", { message: error })}</div>}
      {graphs.length === 0 ? (
        <div className="empty">{t("graphs.empty")}</div>
      ) : (
        graphs.map((id) => {
          const ir = detail[id];
          return (
            <div key={id}>
              <div className="graph-row">
                <span className="graph-row__id">{id}</span>
                {ir && (
                  <>
                    <span className={`badge badge--${ir.origin.kind === "planner" ? "cognitive" : "logical"}`}>
                      {t(`graphs.origin.${ir.origin.kind}`)}
                    </span>
                    <span className="graph-row__meta">{t("graphs.nodes", { count: ir.nodes.length })}</span>
                  </>
                )}
                <button className="btn btn--ghost btn--sm" onClick={() => toggle(id)}>
                  {t("graphs.viewIR")}
                </button>
              </div>
              {ir && (
                <pre className="ir-view" style={{ marginBottom: 14 }}>
                  {JSON.stringify(ir, null, 2)}
                </pre>
              )}
            </div>
          );
        })
      )}
    </section>
  );
}
