import { useCallback, useEffect, useState } from "react";
import { useTranslation } from "react-i18next";
import { api } from "../api/client";
import type { OpMode, Projection } from "../api/types";

export function ProjectionsView() {
  const { t } = useTranslation();
  const [projections, setProjections] = useState<Projection[]>([]);
  const [error, setError] = useState("");
  const [showForm, setShowForm] = useState(false);
  const [spec, setSpec] = useState("");
  const [name, setName] = useState("");
  const [baseUrl, setBaseUrl] = useState("");
  const [busy, setBusy] = useState(false);

  const load = useCallback(() => {
    api.projections().then(setProjections).catch((e) => setError((e as Error).message));
  }, []);

  useEffect(load, [load]);

  const connect = async () => {
    if (!spec.trim() || busy) return;
    setBusy(true);
    setError("");
    try {
      await api.connectProjection({
        kind: "openapi",
        name: name.trim() || undefined,
        base_url: baseUrl.trim() || undefined,
        spec,
      });
      setSpec(""); setName(""); setBaseUrl(""); setShowForm(false);
      load();
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setBusy(false);
    }
  };

  const promote = async (projection: string, op: string, mode: OpMode) => {
    setError("");
    try {
      await api.promote(projection, op, mode);
      load();
    } catch (e) {
      setError((e as Error).message);
    }
  };

  return (
    <section className="view">
      <div className="page-head">
        <h1>{t("projections.title")}</h1>
        <p>{t("projections.subtitle")}</p>
      </div>
      {error && <div className="error-banner">{t("common.error", { message: error })}</div>}

      <div style={{ marginBottom: 18 }}>
        <button className="btn" onClick={() => setShowForm(!showForm)}>
          {t("projections.connect")}
        </button>
      </div>

      {showForm && (
        <div className="card proj-card">
          <div className="form-grid">
            <div>
              <label>{t("projections.specLabel")}</label>
              <textarea
                className="textarea"
                placeholder={t("projections.specPlaceholder")}
                value={spec}
                onChange={(e) => setSpec(e.target.value)}
              />
            </div>
            <div style={{ display: "grid", gridTemplateColumns: "1fr 2fr", gap: 12 }}>
              <div>
                <label>{t("projections.name")}</label>
                <input className="input" value={name} onChange={(e) => setName(e.target.value)} />
              </div>
              <div>
                <label>{t("projections.baseUrl")}</label>
                <input className="input" value={baseUrl} onChange={(e) => setBaseUrl(e.target.value)} />
              </div>
            </div>
            <div>
              <button className="btn" onClick={connect} disabled={busy}>
                {busy ? t("projections.connecting") : t("projections.submit")}
              </button>
            </div>
          </div>
        </div>
      )}

      {projections.length === 0 ? (
        <div className="empty">{t("projections.empty")}</div>
      ) : (
        projections.map((p) => (
          <article className="card proj-card" key={p.name}>
            <div className="proj-card__head">
              <span className="proj-card__name">{p.name}</span>
              <span className="proj-card__url">{p.base_url}</span>
            </div>
            {p.ops.map((op) => (
              <div className="op-row" key={op.op_id}>
                <span className={`badge badge--${op.mode}`}>{t(`projections.mode.${op.mode}`)}</span>
                <span className="op-row__method">{op.method}</span>
                <span className="op-row__id">{op.op_id}</span>
                <span className="op-row__path">{op.path}</span>
                <span className="badge">{op.write ? t("projections.write") : t("projections.readOnly")}</span>
                <select
                  className="select"
                  value={op.mode}
                  title={op.write ? t("projections.writeWarning") : undefined}
                  onChange={(e) => promote(p.name, op.op_id, e.target.value as OpMode)}
                >
                  <option value="live">live</option>
                  <option value="dry-run">dry-run</option>
                  <option value="disabled">disabled</option>
                </select>
              </div>
            ))}
          </article>
        ))
      )}
    </section>
  );
}
