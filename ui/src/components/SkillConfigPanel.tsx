import { useEffect, useState } from "react";
import { useTranslation } from "react-i18next";
import { api } from "../api/client";
import type { ConfigParam, SkillConfig } from "../api/types";

/** Inline editor for one skill's runtime-tunable config (C1 `config`).
 * Values are read/written straight from the kernel's store — this panel
 * doesn't own any config state itself, it's a thin form over
 * GET/PUT /v1/skills/config. */
export function SkillConfigPanel({ skillId }: { skillId: string }) {
  const { t } = useTranslation();
  const [data, setData] = useState<SkillConfig | null>(null);
  const [form, setForm] = useState<Record<string, unknown>>({});
  const [error, setError] = useState("");
  const [saving, setSaving] = useState(false);
  const [savedNote, setSavedNote] = useState("");

  useEffect(() => {
    let cancelled = false;
    api
      .skillConfig(skillId)
      .then((res) => {
        if (cancelled) return;
        setData(res);
        setForm(res.values);
      })
      .catch((e) => setError((e as Error).message));
    return () => {
      cancelled = true;
    };
  }, [skillId]);

  if (error) return <div className="error-banner">{t("common.error", { message: error })}</div>;
  if (!data) return <div className="empty">{t("common.loading")}</div>;

  const setField = (key: string, value: unknown) => setForm((f) => ({ ...f, [key]: value }));

  const resetDefaults = () => {
    const defaults: Record<string, unknown> = {};
    for (const p of data.schema) defaults[p.key] = p.default;
    setForm(defaults);
  };

  const save = async () => {
    const patch: Record<string, unknown> = {};
    for (const p of data.schema) {
      if (form[p.key] !== data.values[p.key]) patch[p.key] = form[p.key];
    }
    if (Object.keys(patch).length === 0) return;
    setSaving(true);
    setError("");
    setSavedNote("");
    try {
      const res = await api.updateSkillConfig(skillId, patch);
      setData(res);
      setForm(res.values);
      setSavedNote(res.pushed_live ? t("skills.config.savedLive") : t("skills.config.savedNext"));
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setSaving(false);
    }
  };

  const dirty = data.schema.some((p) => form[p.key] !== data.values[p.key]);

  return (
    <div className="config-panel">
      <div className="form-grid">
        {data.schema.map((p) => (
          <ConfigField key={p.key} param={p} value={form[p.key]} onChange={(v) => setField(p.key, v)} />
        ))}
      </div>
      <div className="config-panel__actions">
        <button className="btn btn--sm" onClick={save} disabled={!dirty || saving}>
          {saving ? t("skills.config.saving") : t("skills.config.save")}
        </button>
        <button className="btn btn--ghost btn--sm" onClick={resetDefaults} disabled={saving}>
          {t("skills.config.resetDefaults")}
        </button>
        {savedNote && <span className="config-panel__note">{savedNote}</span>}
      </div>
    </div>
  );
}

function ConfigField({
  param,
  value,
  onChange,
}: {
  param: ConfigParam;
  value: unknown;
  onChange: (v: unknown) => void;
}) {
  const { t } = useTranslation();
  return (
    <div>
      <label>
        {param.key}
        {param.restart_required && (
          <span className="config-panel__badge" title={t("skills.config.restartRequiredHint")}>
            {t("skills.config.restartRequired")}
          </span>
        )}
      </label>
      {param.type === "bool" ? (
        <input
          type="checkbox"
          checked={Boolean(value)}
          onChange={(e) => onChange(e.target.checked)}
        />
      ) : param.type === "enum" ? (
        <select className="select" value={String(value ?? "")} onChange={(e) => onChange(e.target.value)}>
          {(param.options ?? []).map((opt) => (
            <option key={opt} value={opt}>
              {opt}
            </option>
          ))}
        </select>
      ) : param.type === "int" || param.type === "float" ? (
        <input
          className="input"
          type="number"
          min={param.min}
          max={param.max}
          step={param.type === "int" ? 1 : "any"}
          value={typeof value === "number" ? value : ""}
          onChange={(e) => {
            const n = param.type === "int" ? parseInt(e.target.value, 10) : parseFloat(e.target.value);
            if (!Number.isNaN(n)) onChange(n);
          }}
        />
      ) : (
        <input className="input" type="text" value={String(value ?? "")} onChange={(e) => onChange(e.target.value)} />
      )}
      {param.description && <p className="config-panel__desc">{param.description}</p>}
    </div>
  );
}
