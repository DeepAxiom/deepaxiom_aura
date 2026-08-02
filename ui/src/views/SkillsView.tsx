import { useCallback, useEffect, useState } from "react";
import { useTranslation } from "react-i18next";
import { api } from "../api/client";
import type { SkillManifest } from "../api/types";
import { SkillConfigPanel } from "../components/SkillConfigPanel";

export function SkillsView() {
  const { t } = useTranslation();
  const [skills, setSkills] = useState<SkillManifest[]>([]);
  const [error, setError] = useState("");
  const [openConfig, setOpenConfig] = useState<string | null>(null);

  const load = useCallback(() => {
    api.skills().then((list) => setSkills(list ?? [])).catch((e) => setError((e as Error).message));
  }, []);

  useEffect(() => {
    load();
    const timer = setInterval(load, 8_000);
    return () => clearInterval(timer);
  }, [load]);

  return (
    <section className="view">
      <div className="page-head">
        <h1>{t("skills.title")}</h1>
        <p>{t("skills.subtitle")}</p>
      </div>
      {error && <div className="error-banner">{t("common.error", { message: error })}</div>}
      <div style={{ display: "flex", alignItems: "center", gap: 12, marginBottom: 16 }}>
        <button className="btn btn--ghost btn--sm" onClick={load}>{t("skills.refresh")}</button>
        <span style={{ color: "var(--text-faint)", fontSize: 12.5 }}>
          {t("skills.connected", { count: skills.length })}
        </span>
      </div>
      {skills.length === 0 ? (
        <div className="empty">{t("skills.empty")}</div>
      ) : (
        <div className="grid">
          {skills.map((s) => (
            <article className="card" key={s.id}>
              <div className="skill-card__head">
                <div>
                  <div className="skill-card__name">{s.name}</div>
                  <div className="skill-card__id">{s.id} · v{s.version}</div>
                </div>
                <span className={`badge badge--${s.type}`}>
                  {s.type} · {t(`skills.types.${s.type}`)}
                </span>
              </div>
              <p className="skill-card__desc">{s.description}</p>
              <div className="skill-card__ports">
                <span className="port-chip" style={{ color: "var(--accent)" }}>{s.capability}</span>
                {s.ports.ingress?.map((p) => (
                  <span className="port-chip" key={`i-${p.name}`}>→ {p.name}: {p.schema}</span>
                ))}
                {s.ports.egress?.map((p) => (
                  <span className="port-chip" key={`e-${p.name}`}>{p.name} → : {p.schema}</span>
                ))}
              </div>
              {s.config && s.config.length > 0 && (
                <>
                  <button
                    className="btn btn--ghost btn--sm"
                    style={{ marginTop: 12 }}
                    onClick={() => setOpenConfig(openConfig === s.id ? null : s.id)}
                  >
                    {t(openConfig === s.id ? "skills.config.hide" : "skills.config.configure")}
                  </button>
                  {openConfig === s.id && <SkillConfigPanel skillId={s.id} />}
                </>
              )}
            </article>
          ))}
        </div>
      )}
    </section>
  );
}
