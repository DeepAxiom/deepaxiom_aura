/**
 * Sessions, read as traces.
 *
 * The kernel logs every envelope with the id of the one that caused it, so a
 * session is already a causal tree — `aura why` walks it on the command line.
 * This draws it: one row per hop (streams collapsed), placed on a time axis,
 * with what each hop carried, what it cost in tokens, which trace it belongs
 * to, and the ledger receipt when the kernel sealed an effect there.
 */

import { useCallback, useEffect, useMemo, useState } from "react";
import { useTranslation } from "react-i18next";
import { api } from "../api/client";
import type { Envelope, LedgerEntry, SessionMeta } from "../api/types";
import { navigate, replaceRoute, useNow } from "../app/store";
import { IconAlert, IconChevron, IconRefresh, IconSeal, IconSearch, IconStudio } from "../components/Icons";
import { Copyable, Empty, ErrorNote, Loading } from "../components/ui";
import { NOT_REPORTED, fmtAgo, fmtCompact, fmtInt, fmtMs, shortId, shortSchema } from "../lib/format";
import { type Trace, type TraceRow, filterTrace, foldTrace, visibleRows } from "../lib/trace";

type ListFilter = "all" | "live" | "ended" | "errors";

/** The kernel returns at most this many events per session (store.SessionEvents). */
const EVENT_CAP = 1000;

export function SessionsView({ sessionId, eventId }: { sessionId?: string; eventId?: string }) {
  const { t } = useTranslation();
  const now = useNow(1000);
  const [sessions, setSessions] = useState<SessionMeta[] | null>(null);
  const [error, setError] = useState("");
  const [filter, setFilter] = useState<ListFilter>("all");
  const [query, setQuery] = useState("");

  const load = useCallback(() => {
    api.sessions().then((s) => { setSessions(s); setError(""); }).catch((e) => setError((e as Error).message));
  }, []);

  useEffect(() => {
    load();
    const timer = setInterval(() => { if (!document.hidden) load(); }, 2000);
    return () => clearInterval(timer);
  }, [load]);

  const shown = useMemo(() => {
    const q = query.trim().toLowerCase();
    return (sessions ?? []).filter((s) => {
      if (filter === "live" && s.ended) return false;
      if (filter === "ended" && !s.ended) return false;
      if (filter === "errors" && !s.errors) return false;
      return !q || s.session_id.toLowerCase().includes(q) || s.graph_id.toLowerCase().includes(q);
    });
  }, [sessions, filter, query]);

  const selected = sessions?.find((s) => s.session_id === sessionId);
  const counts = {
    all: sessions?.length ?? 0,
    live: sessions?.filter((s) => !s.ended).length ?? 0,
    ended: sessions?.filter((s) => s.ended).length ?? 0,
    errors: sessions?.filter((s) => s.errors > 0).length ?? 0,
  };

  return (
    <section className="view view--flush split">
      <aside className="split__list" aria-label={t("nav.sessions")}>
        <div className="split__tools">
          <label className="searchbox">
            <IconSearch size={14} />
            <input
              placeholder={t("sessions.search")}
              aria-label={t("sessions.search")}
              value={query}
              onChange={(e) => setQuery(e.target.value)}
            />
          </label>
          <div className="chips" role="group" aria-label={t("sessions.filter")}>
            {(["all", "live", "ended", "errors"] as ListFilter[]).map((f) => (
              <button key={f} className={`chip ${filter === f ? "chip--on" : ""}`} aria-pressed={filter === f} onClick={() => setFilter(f)}>
                {t(`sessions.filters.${f}`)} <span>{counts[f]}</span>
              </button>
            ))}
          </div>
        </div>

        {error && <ErrorNote message={error} onRetry={load} />}
        {sessions === null && !error && <Loading />}
        {sessions !== null && shown.length === 0 && (
          <Empty title={sessions.length ? t("sessions.noMatch") : t("sessions.empty")}>
            {sessions.length ? undefined : t("sessions.emptyHelp")}
          </Empty>
        )}

        <div className="slist" role="list">
          {shown.map((s) => (
            <a
              key={s.session_id}
              role="listitem"
              href={`#/sessions/${encodeURIComponent(s.session_id)}`}
              className={`slist__row ${s.session_id === sessionId ? "slist__row--on" : ""}`}
              aria-current={s.session_id === sessionId ? "true" : undefined}
            >
              <span className={`dot ${s.ended ? "" : "dot--live"} ${s.errors ? "dot--err" : ""}`} aria-hidden />
              <span className="slist__id">{s.session_id}</span>
              <span className="slist__graph">{s.graph_id}</span>
              <span className="slist__meta">
                {s.ended ? fmtMs(s.ended - s.started) : t("sessions.live")}
                {" · "}{t("sessions.events", { count: s.events })}
                {s.errors > 0 && <b className="slist__err"> · {t("sessions.errors", { count: s.errors })}</b>}
              </span>
              <span className="slist__ago">{fmtAgo(s.started, now)}</span>
            </a>
          ))}
        </div>
      </aside>

      <div className="split__detail">
        {!sessionId ? (
          <Empty title={t("sessions.pickTitle")}>{t("sessions.pick")}</Empty>
        ) : (
          <SessionTrace key={sessionId} sessionId={sessionId} meta={selected} eventId={eventId} />
        )}
      </div>
    </section>
  );
}

function SessionTrace({ sessionId, meta, eventId }: { sessionId: string; meta?: SessionMeta; eventId?: string }) {
  const { t } = useTranslation();
  const [events, setEvents] = useState<Envelope[] | null>(null);
  const [stamps, setStamps] = useState<number[]>([]);
  const [ledger, setLedger] = useState<LedgerEntry[]>([]);
  const [error, setError] = useState("");
  const [query, setQuery] = useState("");
  const [kinds, setKinds] = useState<string[]>([]);
  const [errorsOnly, setErrorsOnly] = useState(false);
  const [collapsed, setCollapsed] = useState<Set<string>>(new Set());
  const live = meta ? !meta.ended : false;

  const load = useCallback(() => {
    api.sessionEvents(sessionId)
      .then((d) => { setEvents(d.events ?? []); setStamps(d.timestamps ?? []); setError(""); })
      .catch((e) => setError((e as Error).message));
    api.sessionLedger(sessionId).then(setLedger).catch(() => undefined);
  }, [sessionId]);

  useEffect(() => {
    load();
    if (!live) return;
    const timer = setInterval(() => { if (!document.hidden) load(); }, 1000);
    return () => clearInterval(timer);
  }, [load, live]);

  // One more read after a session ends, so the tail is not missed.
  useEffect(() => { if (!live) load(); }, [live, load]);

  const trace: Trace = useMemo(() => foldTrace(events ?? [], stamps), [events, stamps]);
  const rows = useMemo(
    () => visibleRows(filterTrace(trace.rows, { query, kinds, errorsOnly }), collapsed),
    [trace.rows, query, kinds, errorsOnly, collapsed],
  );
  const kindsPresent = useMemo(() => [...new Set(trace.rows.map((r) => r.kind))], [trace.rows]);
  const selected = trace.rows.find((r) => r.key === eventId || r.ids.includes(eventId ?? "")) ?? null;
  const byEnvelope = useMemo(() => new Map(ledger.map((e) => [e.envelope, e])), [ledger]);
  const span = Math.max(trace.duration, 1);

  const toggle = (key: string) =>
    setCollapsed((prev) => {
      const next = new Set(prev);
      if (next.has(key)) next.delete(key); else next.add(key);
      return next;
    });

  if (error && events === null) return <ErrorNote message={error} onRetry={load} />;
  if (events === null) return <Loading />;

  const ticks = [0, 0.25, 0.5, 0.75, 1];

  return (
    <div className="trace">
      <header className="trace__head">
        <div className="trace__title">
          <span className={`dot ${live ? "dot--live" : ""}`} aria-hidden />
          <Copyable value={sessionId}><code>{sessionId}</code></Copyable>
          {meta && (
            <button className="btn btn--ghost btn--sm" onClick={() => navigate("studio", meta.graph_id)} title={t("sessions.openGraph")}>
              <IconStudio size={13} />{meta.graph_id}
            </button>
          )}
          <span className="trace__spacer" />
          <button className="iconbtn" onClick={load} title={t("common.refresh")} aria-label={t("common.refresh")}>
            <IconRefresh size={15} />
          </button>
        </div>
        <dl className="facts">
          <div><dt>{t("sessions.fact.state")}</dt><dd>{live ? t("sessions.live") : t("sessions.ended")}</dd></div>
          <div><dt>{t("sessions.fact.duration")}</dt><dd>{stamps.length ? fmtMs(trace.duration) : NOT_REPORTED}</dd></div>
          <div><dt>{t("sessions.fact.events")}</dt><dd>{fmtInt(trace.events)}</dd></div>
          <div><dt>{t("sessions.fact.errors")}</dt><dd className={trace.errors ? "facts--err" : ""}>{fmtInt(trace.errors)}</dd></div>
          <div>
            <dt title={t("sessions.tokensHelp")}>{t("sessions.fact.tokens")}</dt>
            <dd>
              {trace.usage
                ? `${fmtCompact(trace.usage.input_tokens ?? null)} → ${fmtCompact(trace.usage.output_tokens ?? null)}`
                : NOT_REPORTED}
              {trace.usage?.cached_input_tokens ? <span className="facts__sub"> {t("sessions.cached", { n: fmtCompact(trace.usage.cached_input_tokens) })}</span> : null}
            </dd>
          </div>
          <div>
            <dt>{t("sessions.fact.trace")}</dt>
            <dd>
              {trace.traceIds.length
                ? trace.traceIds.map((id) => <Copyable key={id} value={id}><code>{shortId(id, 8, 4)}</code></Copyable>)
                : <span title={t("sessions.noTraceHelp")}>{NOT_REPORTED}</span>}
            </dd>
          </div>
          <div><dt>{t("sessions.fact.sealed")}</dt><dd>{ledger.length ? <a href="#/evidence">{ledger.length}</a> : "0"}</dd></div>
        </dl>
      </header>

      <div className="trace__tools">
        <label className="searchbox">
          <IconSearch size={14} />
          <input placeholder={t("sessions.searchEvents")} aria-label={t("sessions.searchEvents")} value={query} onChange={(e) => setQuery(e.target.value)} />
        </label>
        <div className="chips" role="group" aria-label={t("sessions.kinds")}>
          {kindsPresent.map((k) => (
            <button
              key={k}
              className={`chip ${kinds.includes(k) ? "chip--on" : ""}`}
              aria-pressed={kinds.includes(k)}
              onClick={() => setKinds((prev) => (prev.includes(k) ? prev.filter((x) => x !== k) : [...prev, k]))}
            >
              {k}
            </button>
          ))}
          <button className={`chip chip--err ${errorsOnly ? "chip--on" : ""}`} aria-pressed={errorsOnly} onClick={() => setErrorsOnly((v) => !v)}>
            {t("sessions.errorsOnly")}
          </button>
        </div>
      </div>

      {events.length >= EVENT_CAP && <div className="trace__cap">{t("sessions.capped", { n: EVENT_CAP })}</div>}

      {trace.rows.length === 0 ? (
        <Empty title={t("sessions.noEvents")} />
      ) : rows.length === 0 ? (
        <Empty title={t("sessions.noMatch")} />
      ) : (
        <div className="wf" role="tree" aria-label={t("sessions.waterfall")}>
          <div className="wf__axis" aria-hidden>
            <span className="wf__axis-label">{t("sessions.cols.hop")}</span>
            <span className="wf__ticks">
              {ticks.map((f) => <i key={f} style={{ left: `${f * 100}%` }}>{stamps.length ? fmtMs(span * f) : ""}</i>)}
            </span>
            <span className="wf__axis-right">{t("sessions.cols.tokens")}</span>
          </div>
          {rows.map((r) => (
            <WaterfallRow
              key={r.key}
              row={r}
              span={span}
              selected={selected?.key === r.key}
              collapsed={collapsed.has(r.key)}
              sealed={r.ids.some((id) => byEnvelope.has(id)) || !!r.receipt}
              onToggle={() => toggle(r.key)}
              onSelect={() => replaceRoute("sessions", sessionId, selected?.key === r.key ? undefined : r.key)}
            />
          ))}
        </div>
      )}

      {selected && (
        <RowDetail
          row={selected}
          events={events}
          entry={selected.ids.map((id) => byEnvelope.get(id)).find(Boolean)}
          onClose={() => replaceRoute("sessions", sessionId)}
        />
      )}
    </div>
  );
}

function WaterfallRow({
  row, span, selected, collapsed, sealed, onToggle, onSelect,
}: {
  row: TraceRow;
  span: number;
  selected: boolean;
  collapsed: boolean;
  sealed: boolean;
  onToggle: () => void;
  onSelect: () => void;
}) {
  const { t } = useTranslation();
  const left = (row.t0 / span) * 100;
  const width = Math.max(((row.t1 - row.t0) / span) * 100, 0);
  const state = row.error ? "err" : row.suppressed ? "sup" : row.kind === "confirm_request" || row.kind === "confirm_response" ? "gate" : row.kind === "cancel" ? "cancel" : "data";
  return (
    <div
      role="treeitem"
      aria-level={row.depth + 1}
      aria-selected={selected}
      aria-expanded={row.hasChildren ? !collapsed : undefined}
      tabIndex={0}
      className={`wf__row wf__row--${state} ${selected ? "wf__row--on" : ""}`}
      onClick={onSelect}
      onKeyDown={(e) => {
        if (e.key === "Enter" || e.key === " ") { e.preventDefault(); onSelect(); }
        else if (e.key === "ArrowRight" && collapsed) { e.preventDefault(); onToggle(); }
        else if (e.key === "ArrowLeft" && !collapsed && row.hasChildren) { e.preventDefault(); onToggle(); }
        else if (e.key === "ArrowDown") { e.preventDefault(); (e.currentTarget.nextElementSibling as HTMLElement | null)?.focus(); }
        else if (e.key === "ArrowUp") { e.preventDefault(); (e.currentTarget.previousElementSibling as HTMLElement | null)?.focus(); }
      }}
    >
      <span className="wf__hop" style={{ paddingLeft: 8 + row.depth * 14 }}>
        {row.hasChildren ? (
          <button
            className={`wf__twist ${collapsed ? "" : "wf__twist--open"}`}
            aria-label={t(collapsed ? "sessions.expand" : "sessions.collapse")}
            onClick={(e) => { e.stopPropagation(); onToggle(); }}
            tabIndex={-1}
          >
            <IconChevron size={11} />
          </button>
        ) : <span className="wf__twist wf__twist--leaf" />}
        <span className="wf__where">
          {row.node || "—"}{row.port && <span className="wf__port">.{row.port}</span>}
        </span>
        {row.kind !== "data" && <span className={`kind kind--${row.kind}`}>{row.kind}</span>}
        {row.count > 1 && <span className="wf__count" title={t("sessions.streamHelp", { count: row.count })}>×{row.count}</span>}
        {row.suppressed && <span className="kind kind--sup" title={t("sessions.suppressedHelp")}>{t("sessions.suppressed")}</span>}
        {sealed && <span className="wf__seal" title={t("sessions.sealedHelp")}><IconSeal size={12} /></span>}
        {row.error && <span className="wf__err" title={row.error}><IconAlert size={12} />{row.error}</span>}
        {!row.error && row.note && <span className="wf__note">{row.note}</span>}
        {!row.error && !row.note && row.text.trim() && <span className="wf__text">{row.text.trim()}</span>}
      </span>
      <span className="wf__lane">
        <span className="wf__bar" style={{ left: `${left}%`, width: `${width}%` }} />
        <span
          className={`wf__dur ${left + width > 82 && left < 18 ? "wf__dur--in" : ""}`}
          style={left + width > 82 ? (left < 18 ? { right: 0 } : { right: `${100 - left}%`, marginRight: 6 }) : { left: `${left + width}%`, marginLeft: 6 }}
        >
          {row.count > 1 ? fmtMs(row.t1 - row.t0) : `+${fmtMs(row.t0)}`}
        </span>
      </span>
      <span className="wf__tok">
        {row.usage ? `${fmtCompact(row.usage.input_tokens ?? null)} → ${fmtCompact(row.usage.output_tokens ?? null)}` : ""}
      </span>
    </div>
  );
}

function RowDetail({
  row, events, entry, onClose,
}: {
  row: TraceRow;
  events: Envelope[];
  entry?: LedgerEntry;
  onClose: () => void;
}) {
  const { t } = useTranslation();
  const first = events.find((e) => e.id === row.key);
  const lastEnv = events.find((e) => e.id === row.ids[row.ids.length - 1]);
  return (
    <aside className="detail" aria-label={t("sessions.detail")}>
      <header className="detail__head">
        <code>{row.node}{row.port ? `.${row.port}` : ""}</code>
        <span className={`kind kind--${row.kind}`}>{row.kind}</span>
        <span className="trace__spacer" />
        <button className="btn btn--ghost btn--sm" onClick={onClose}>{t("common.close")}</button>
      </header>
      <dl className="kv kv--wide">
        <div><dt>{t("sessions.d.id")}</dt><dd><Copyable value={row.key}><code>{row.key}</code></Copyable></dd></div>
        {first?.cause_id && <div><dt>{t("sessions.d.cause")}</dt><dd><a href={`#/sessions/${encodeURIComponent(first.session ?? "")}/${first.cause_id}`}><code>{first.cause_id}</code></a></dd></div>}
        {row.schema && <div><dt>{t("sessions.d.schema")}</dt><dd><code title={row.schema}>{shortSchema(row.schema)}</code></dd></div>}
        <div><dt>{t("sessions.d.at")}</dt><dd>+{fmtMs(row.t0)}{row.count > 1 && ` … +${fmtMs(row.t1)}`}</dd></div>
        {row.count > 1 && <div><dt>{t("sessions.d.envelopes")}</dt><dd>{row.count}</dd></div>}
        {row.usage && (
          <div>
            <dt>{t("sessions.d.usage")}</dt>
            <dd>
              {t("sessions.usageLine", {
                input: fmtInt(row.usage.input_tokens ?? null),
                output: fmtInt(row.usage.output_tokens ?? null),
                cached: fmtInt(row.usage.cached_input_tokens ?? null),
              })}
            </dd>
          </div>
        )}
        {lastEnv?.attest?.engine && <div><dt>{t("sessions.d.attest")}</dt><dd><code>{String(lastEnv.attest.engine)} · {String(lastEnv.attest.model ?? "")}</code></dd></div>}
        {row.traceId && <div><dt>{t("sessions.d.trace")}</dt><dd><Copyable value={row.traceId}><code>{row.traceId}</code></Copyable></dd></div>}
        {row.elidedBytes > 0 && <div><dt>{t("sessions.d.elided")}</dt><dd title={t("sessions.elidedHelp")}>{t("sessions.elidedLine", { bytes: fmtInt(row.elidedBytes) })}</dd></div>}
        {(row.receipt || entry) && (
          <div>
            <dt>{t("sessions.d.receipt")}</dt>
            <dd>
              {entry ? (
                <a className="receipt" href={`#/evidence/entry/${entry.seq}`}>
                  <IconSeal size={13} />#{entry.seq} · {entry.capability} · {entry.decision}/{entry.outcome}
                </a>
              ) : (
                <Copyable value={row.receipt}><code>{shortId(row.receipt, 10, 6)}</code></Copyable>
              )}
            </dd>
          </div>
        )}
        {row.suppressed && <div><dt>{t("sessions.suppressed")}</dt><dd>{t("sessions.suppressedHelp")}</dd></div>}
      </dl>
      {row.error && <div className="alert">{row.error}</div>}
      {row.text.trim() ? (
        <pre className="detail__text">{row.text}</pre>
      ) : (
        first?.payload !== undefined && <pre className="detail__text detail__text--json">{JSON.stringify(first.payload, null, 2)}</pre>
      )}
    </aside>
  );
}
