import { useCallback, useEffect, useState } from "react";
import { useTranslation } from "react-i18next";
import { api } from "../api/client";
import type { Envelope, SessionMeta } from "../api/types";

export function SessionsView() {
  const { t } = useTranslation();
  const [sessions, setSessions] = useState<SessionMeta[]>([]);
  const [selected, setSelected] = useState<string | null>(null);
  const [events, setEvents] = useState<Envelope[] | null>(null);
  const [error, setError] = useState("");

  const load = useCallback(() => {
    api
      .sessions()
      .then(setSessions)
      .catch((e) => setError((e as Error).message));
  }, []);

  useEffect(load, [load]);

  const inspect = async (id: string) => {
    if (selected === id) {
      setSelected(null);
      setEvents(null);
      return;
    }
    setSelected(id);
    setEvents(null);
    try {
      const { events } = await api.sessionEvents(id);
      setEvents(events ?? []);
    } catch (e) {
      setError((e as Error).message);
    }
  };

  const fmtTime = (ms: number) =>
    ms ? new Date(ms).toLocaleTimeString() : "—";

  return (
    <section className="view">
      <div className="page-head">
        <h1>{t("sessions.title")}</h1>
        <p>{t("sessions.subtitle")}</p>
      </div>
      {error && <div className="error-banner">{t("common.error", { message: error })}</div>}
      <div style={{ marginBottom: 14 }}>
        <button className="btn btn--ghost btn--sm" onClick={load}>{t("sessions.refresh")}</button>
      </div>
      {sessions.length === 0 ? (
        <div className="empty">{t("sessions.empty")}</div>
      ) : (
        sessions.map((s) => (
          <div key={s.session_id}>
            <div className="graph-row" style={{ cursor: "pointer" }}
              onClick={() => inspect(s.session_id)}>
              <span className="graph-row__id">{s.session_id}</span>
              <span className="badge">{s.graph_id}</span>
              {s.errors > 0 ? (
                <span className="badge badge--motor">{t("sessions.errors", { count: s.errors })}</span>
              ) : (
                <span className="badge badge--logical">ok</span>
              )}
              <span className="graph-row__meta">
                {t("sessions.events", { count: s.events })} · {fmtTime(s.started)}
              </span>
            </div>
            {selected === s.session_id && (
              <div style={{ margin: "0 0 16px" }}>
                {events === null ? (
                  <div className="status-line">{t("common.loading")}</div>
                ) : events.length === 0 ? (
                  <div className="empty">{t("sessions.noEvents")}</div>
                ) : (
                  <table className="table">
                    <thead>
                      <tr>
                        <th>#</th>
                        <th>id</th>
                        <th>{t("sessions.cols.cause")}</th>
                        <th>kind</th>
                        <th>{t("sessions.cols.origin")}</th>
                        <th>payload</th>
                      </tr>
                    </thead>
                    <tbody>
                      {events.map((e, i) => (
                        <tr key={e.id ?? i}
                          style={e.kind === "error" ? { background: "var(--err-soft)" } : undefined}>
                          <td style={{ color: "var(--text-faint)" }}>{i + 1}</td>
                          <td><code>{e.id?.slice(0, 10)}…</code></td>
                          <td><code>{e.cause_id ? `${e.cause_id.slice(0, 10)}…` : ""}</code></td>
                          <td>{e.kind}</td>
                          <td><code>{e.node}{e.port ? `.${e.port}` : ""}</code></td>
                          <td><pre>{JSON.stringify(e.payload ?? null)}</pre></td>
                        </tr>
                      ))}
                    </tbody>
                  </table>
                )}
              </div>
            )}
          </div>
        ))
      )}
    </section>
  );
}
