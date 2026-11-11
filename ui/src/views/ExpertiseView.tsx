/**
 * The Expertise catalog: every typed function connected to this node.
 *
 * An Expertise is what the wire and the SDK still call a skill — a running,
 * typed function that is expert at one thing. One entry per package; when the
 * same package is connected more than once, those are replicas and the kernel
 * spreads sessions across them, which is what the load column shows.
 */

import { useCallback, useEffect, useMemo, useState } from "react";
import { useTranslation } from "react-i18next";
import { api } from "../api/client";
import type { SkillManifest, SkillType } from "../api/types";
import { replaceRoute } from "../app/store";
import { ExpertiseConfigPanel } from "../components/ExpertiseConfigPanel";
import { IconChevron, IconSearch } from "../components/Icons";
import { Empty, ErrorNote, Loading, TypeBadge } from "../components/ui";
import { dedupeSkills } from "../graph/model";
import type { NodeStats } from "../lib/stats";

const TYPES: SkillType[] = ["sensorial", "cognitive", "motor", "memory", "logical"];

/** Permissions as short, readable facts. Unknown keys are shown as they are. */
function permissionLines(p: Record<string, unknown> | undefined): { key: string; value: string }[] {
  if (!p) return [];
  return Object.entries(p).map(([key, v]) => ({
    key,
    value: Array.isArray(v) ? (v.length ? v.join(", ") : "none") : typeof v === "object" && v !== null ? JSON.stringify(v) : String(v),
  }));
}

export function ExpertiseView({ selected, stats }: { selected?: string; stats: NodeStats | null }) {
  const { t } = useTranslation();
  const [list, setList] = useState<SkillManifest[] | null>(null);
  const [error, setError] = useState("");
  const [query, setQuery] = useState("");

  const load = useCallback(() => {
    api.expertise().then((l) => { setList(l ?? []); setError(""); }).catch((e) => setError((e as Error).message));
  }, []);

  useEffect(() => {
    load();
    const timer = setInterval(() => { if (!document.hidden) load(); }, 4000);
    return () => clearInterval(timer);
  }, [load]);

  const catalog = useMemo(() => dedupeSkills(list ?? []), [list]);
  const load_ = useMemo(() => new Map((stats?.expertise ?? []).map((e) => [e.id, e])), [stats]);

  const q = query.trim().toLowerCase();
  const shown = catalog.filter((s) =>
    !q || `${s.name} ${s.id} ${s.capability} ${s.type} ${s.description} ${[...(s.ports?.ingress ?? []), ...(s.ports?.egress ?? [])].map((p) => `${p.name} ${p.schema}`).join(" ")}`
      .toLowerCase().includes(q));

  const groups = TYPES.map((type) => ({ type, items: shown.filter((s) => s.type === type) })).filter((g) => g.items.length);
  const other = shown.filter((s) => !TYPES.includes(s.type));
  if (other.length) groups.push({ type: "other" as SkillType, items: other });

  return (
    <section className="view expertise">
      <div className="tablebar">
        <label className="searchbox searchbox--lg">
          <IconSearch size={15} />
          <input
            placeholder={t("expertise.search")}
            aria-label={t("expertise.search")}
            value={query}
            onChange={(e) => setQuery(e.target.value)}
          />
        </label>
        <span className="tablebar__note">
          {list !== null && t("expertise.connected", { count: catalog.length, connections: list.length })}
        </span>
      </div>

      {error && <ErrorNote message={error} onRetry={load} />}
      {list === null && !error && <Loading />}
      {list !== null && catalog.length === 0 && (
        <Empty title={t("expertise.emptyTitle")}>{t("expertise.empty")}</Empty>
      )}
      {catalog.length > 0 && shown.length === 0 && <Empty title={t("expertise.noMatch")} />}

      {groups.map((g) => (
        <section key={g.type} className="xgroup" aria-labelledby={`xg-${g.type}`}>
          <header className="xgroup__head">
            <h2 id={`xg-${g.type}`}><TypeBadge type={g.type} /></h2>
            <span className="xgroup__what">{TYPES.includes(g.type) ? t(`expertise.types.${g.type}`) : ""}</span>
            <span className="xgroup__count">{g.items.length}</span>
          </header>
          <div className="xlist">
            {g.items.map((s) => {
              const open = selected === s.id;
              const live = load_.get(s.id);
              const replicas = live?.replicas ?? null;
              const perms = permissionLines(s.permissions);
              return (
                <article key={s.id} className={`xcard xcard--${s.type} ${open ? "xcard--open" : ""}`}>
                  <button
                    className="xcard__row"
                    aria-expanded={open}
                    onClick={() => replaceRoute("expertise", open ? undefined : s.id)}
                  >
                    <span className={`xcard__twist ${open ? "xcard__twist--open" : ""}`}><IconChevron size={12} /></span>
                    <span className="xcard__main">
                      <span className="xcard__name">{s.name}</span>
                      <span className="xcard__cap">{s.capability}</span>
                    </span>
                    <span className="xcard__ports" aria-hidden>
                      {(s.ports?.ingress ?? []).length} {t("expertise.ingress")} · {(s.ports?.egress ?? []).length} {t("expertise.egress")}
                    </span>
                    <span className="xcard__load" title={t("expertise.loadHelp")}>
                      {replicas ? (
                        <>
                          <span className="minibars" aria-hidden>
                            {replicas.map((n, i) => (
                              <i key={i} style={{ height: `${20 + (n / Math.max(1, ...replicas)) * 80}%` }} className={n ? "minibars--busy" : ""} />
                            ))}
                          </span>
                          {t("expertise.replicas", { count: replicas.length })} · {t("expertise.sessions", { count: live?.sessions ?? 0 })}
                        </>
                      ) : (
                        t("expertise.replicas", { count: s.instances })
                      )}
                    </span>
                  </button>

                  {open && (
                    <div className="xcard__body">
                      <p className="xcard__desc">{s.description}</p>
                      <div className="xcard__cols">
                        <div>
                          <h3>{t("expertise.ports")}</h3>
                          <table className="ports">
                            <tbody>
                              {(s.ports?.ingress ?? []).map((p) => (
                                <tr key={`i-${p.name}`}><td className="ports__dir">{t("expertise.in")}</td><td className="mono">{p.name}</td><td className="mono ports__schema">{p.schema}</td></tr>
                              ))}
                              {(s.ports?.egress ?? []).map((p) => (
                                <tr key={`e-${p.name}`}><td className="ports__dir ports__dir--out">{t("expertise.out")}</td><td className="mono">{p.name}</td><td className="mono ports__schema">{p.schema}</td></tr>
                              ))}
                            </tbody>
                          </table>
                        </div>
                        <div>
                          <h3>{t("expertise.permissions")}</h3>
                          {perms.length ? (
                            <dl className="kv">
                              {perms.map((p) => <div key={p.key}><dt className="mono">{p.key}</dt><dd className="mono">{p.value}</dd></div>)}
                            </dl>
                          ) : <p className="panel__na">{t("expertise.noPermissions")}</p>}
                        </div>
                        <div>
                          <h3>{t("expertise.package")}</h3>
                          <dl className="kv">
                            <div><dt>id</dt><dd className="mono">{s.id}</dd></div>
                            <div><dt>version</dt><dd className="mono">{s.version}</dd></div>
                            <div><dt>format</dt><dd className="mono">{s.format}</dd></div>
                            <div><dt>protocol</dt><dd className="mono">{s.protocol}</dd></div>
                            {replicas && <div><dt>{t("expertise.loadPerReplica")}</dt><dd className="mono">{replicas.join(" · ")}</dd></div>}
                          </dl>
                        </div>
                      </div>
                      <h3>{t("expertise.config.title")}</h3>
                      {s.config && s.config.length > 0
                        ? <ExpertiseConfigPanel skillId={s.id} />
                        : <p className="panel__na">{t("expertise.config.none")}</p>}
                    </div>
                  )}
                </article>
              );
            })}
          </div>
        </section>
      ))}
    </section>
  );
}
