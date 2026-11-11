/**
 * Pulse — what the node is doing this second.
 *
 * Everything here is GET /v1/stats, polled once a second while the tab is
 * visible. Counters become rates in lib/stats; nothing is smoothed, estimated
 * or filled in. A section the node does not report says so in words, because
 * a tile showing 0 for "this node does not run that" reads as "idle" and is
 * a different fact.
 */

import { useTranslation } from "react-i18next";
import type { Health } from "../api/types";
import { STATS_WINDOW, navigate, type StatsFeed } from "../app/store";
import { Empty, ErrorNote, Loading, Meter, Sparkline, TypeBadge } from "../components/ui";
import { NOT_REPORTED, fmtBytes, fmtCompact, fmtInt, fmtMs, fmtPct, fmtUptime, shortId } from "../lib/format";
import {
  cacheHitRatio, mailboxFill, rateSeries, replicaScale, replicaViews, speculationHitRate,
} from "../lib/stats";

function last(values: (number | null)[]): number | null {
  return values.length ? values[values.length - 1] : null;
}

function Tile({
  label, value, unit, sub, children, tone, onClick, wide,
}: {
  label: string;
  value: string;
  unit?: string;
  sub?: React.ReactNode;
  children?: React.ReactNode;
  tone?: "warn" | "err";
  onClick?: () => void;
  wide?: boolean;
}) {
  const body = (
    <>
      <div className="tile__label">{label}</div>
      <div className="tile__value">
        <span className={value === NOT_REPORTED ? "tile__na" : ""}>{value}</span>
        {unit && value !== NOT_REPORTED && <span className="tile__unit">{unit}</span>}
      </div>
      {sub && <div className="tile__sub">{sub}</div>}
      {children && <div className="tile__viz">{children}</div>}
    </>
  );
  const cls = `tile ${tone ? `tile--${tone}` : ""} ${wide ? "tile--wide" : ""}`;
  return onClick ? (
    <button className={`${cls} tile--link`} onClick={onClick}>{body}</button>
  ) : (
    <div className={cls}>{body}</div>
  );
}

export function PulseView({ feed, health }: { feed: StatsFeed; health: Health | null }) {
  const { t } = useTranslation();
  const s = feed.latest;

  if (feed.state === "unsupported") {
    return (
      <section className="view">
        <Empty title={t("pulse.unsupportedTitle")}>{t("pulse.unsupported")}</Empty>
      </section>
    );
  }
  if (!s) {
    return (
      <section className="view">
        {feed.state === "error" ? <ErrorNote message={feed.error} /> : <Loading />}
      </section>
    );
  }

  const inRate = rateSeries(feed.samples, (r) => r.tokensIn);
  const outRate = rateSeries(feed.samples, (r) => r.tokensOut);
  const commitRate = rateSeries(feed.samples, (r) => r.commits);
  const sessionsSeries = feed.samples.map((x) => x.sessions_live);
  const slots = STATS_WINDOW - 1;

  const cache = cacheHitRatio(s);
  const spec = speculationHitRate(s);
  const fill = mailboxFill(s);
  const views = replicaViews(s);
  const scale = replicaScale(views);
  const pending = s.approvals_pending;
  const na = t("pulse.notReported");

  return (
    <section className="view pulse">
      {feed.state === "error" && <ErrorNote message={t("pulse.stale", { message: feed.error })} />}

      <div className="pulse__grid">
        <Tile
          label={t("pulse.sessions")}
          value={fmtInt(s.sessions_live)}
          sub={t("pulse.sessionsSub")}
          onClick={() => navigate("sessions")}
        >
          <Sparkline values={sessionsSeries} slots={STATS_WINDOW} width={200} height={34} tone="live" label={t("pulse.sessionsChart")} />
        </Tile>

        <Tile
          label={t("pulse.tokensIn")}
          value={s.tokens ? fmtCompact(s.tokens.input_tokens) : NOT_REPORTED}
          sub={s.tokens ? t("pulse.perSecond", { rate: fmtCompact(last(inRate)) }) : na}
        >
          <Sparkline values={inRate} slots={slots} width={200} height={34} tone="in" label={t("pulse.tokensInChart")} />
        </Tile>

        <Tile
          label={t("pulse.tokensOut")}
          value={s.tokens ? fmtCompact(s.tokens.output_tokens) : NOT_REPORTED}
          sub={s.tokens ? t("pulse.perSecond", { rate: fmtCompact(last(outRate)) }) : na}
        >
          <Sparkline values={outRate} slots={slots} width={200} height={34} tone="out" label={t("pulse.tokensOutChart")} />
        </Tile>

        <Tile
          label={t("pulse.cache")}
          value={fmtPct(cache)}
          sub={
            s.tokens
              ? cache === null
                ? t("pulse.cacheNone")
                : t("pulse.cacheSub", { cached: fmtCompact(s.tokens.cached_input_tokens), input: fmtCompact(s.tokens.input_tokens) })
              : na
          }
        >
          <Meter value={cache} label={t("pulse.cache")} />
        </Tile>

        <Tile
          label={t("pulse.approvals")}
          value={pending === undefined ? NOT_REPORTED : fmtInt(pending)}
          sub={pending === undefined ? t("pulse.approvalsOff") : pending > 0 ? t("pulse.approvalsWaiting") : t("pulse.approvalsNone")}
          tone={pending ? "warn" : undefined}
          onClick={() => navigate("evidence", "approvals")}
        />

        <Tile
          label={t("pulse.inferences")}
          value={s.tokens ? fmtInt(s.tokens.inferences) : NOT_REPORTED}
          sub={s.tokens ? t("pulse.inferencesSub") : na}
        />
      </div>

      <div className="pulse__cols">
        <section className="panel pulse__load" aria-labelledby="pulse-load">
          <header className="panel__head">
            <h2 id="pulse-load">{t("pulse.load")}</h2>
            <span className="panel__note">{t("pulse.loadNote")}</span>
          </header>
          {views.length === 0 ? (
            <Empty title={t("pulse.loadEmptyTitle")}>{t("expertise.empty")}</Empty>
          ) : (
            <div className="load">
              {views.map((v) => (
                <button key={v.id} className="load__row" onClick={() => navigate("expertise", v.id)} title={v.id}>
                  <span className="load__name">
                    <TypeBadge type={v.type} compact />
                    <span className="load__cap">{v.capability}</span>
                  </span>
                  <span
                    className="load__bars"
                    role="img"
                    aria-label={t("pulse.loadAria", { count: v.replicas.length, loads: v.replicas.join(", ") })}
                  >
                    {v.replicas.map((load, i) => (
                      <span key={i} className="load__bar" title={t("pulse.replica", { n: i + 1, load })}>
                        <span className={`load__fill load__fill--${v.type}`} style={{ height: `${(load / scale) * 100}%` }} />
                        <span className="load__n">{load}</span>
                      </span>
                    ))}
                  </span>
                  <span className="load__meta">
                    <b>{v.sessions}</b> {t("pulse.sessionsUnit", { count: v.sessions })}
                    <span className="load__sep">·</span>
                    {t("pulse.replicas", { count: v.replicas.length })}
                    {v.replicas.length > 1 && (
                      <span className={`load__spread ${v.spread > 1 ? "load__spread--off" : ""}`} title={t("pulse.spreadHelp")}>
                        Δ{v.spread}
                      </span>
                    )}
                  </span>
                </button>
              ))}
            </div>
          )}
        </section>

        <div className="pulse__side">
          <section className="panel" aria-labelledby="pulse-mailbox">
            <header className="panel__head"><h2 id="pulse-mailbox">{t("pulse.mailbox")}</h2></header>
            {s.mailbox?.enabled ? (
              <>
                <Meter value={fill} tone={fill !== null && fill > 0.8 ? "err" : fill !== null && fill > 0.4 ? "warn" : "live"} label={t("pulse.mailbox")} />
                <dl className="kv">
                  <div><dt>{t("pulse.queued")}</dt><dd>{fmtInt(s.mailbox.queued)} / {fmtInt(s.mailbox.depth)}</dd></div>
                  <div><dt title={t("pulse.waitsHelp")}>{t("pulse.waits")}</dt><dd className={s.mailbox.waits ? "kv--warn" : ""}>{fmtInt(s.mailbox.waits)}</dd></div>
                  <div><dt title={t("pulse.droppedHelp")}>{t("pulse.dropped")}</dt><dd>{fmtInt(s.mailbox.dropped)}</dd></div>
                </dl>
              </>
            ) : (
              <p className="panel__na">{s.mailbox ? t("pulse.mailboxInline") : na}</p>
            )}
          </section>

          <section className="panel" aria-labelledby="pulse-spec">
            <header className="panel__head"><h2 id="pulse-spec">{t("pulse.speculation")}</h2></header>
            {s.speculation ? (
              <>
                <div className="panel__big">
                  {fmtPct(spec)}
                  <span>{spec === null ? t("pulse.specNone") : t("pulse.specSub")}</span>
                </div>
                <dl className="kv">
                  <div><dt>{t("pulse.attempts")}</dt><dd>{fmtInt(s.speculation.attempts)}</dd></div>
                  <div><dt>{t("pulse.hits")}</dt><dd>{fmtInt(s.speculation.hits)}</dd></div>
                  <div><dt>{t("pulse.misses")}</dt><dd>{fmtInt(s.speculation.misses)}</dd></div>
                  <div><dt title={t("pulse.refusedHelp")}>{t("pulse.refused")}</dt><dd>{fmtInt(s.speculation.refused)}</dd></div>
                </dl>
              </>
            ) : <p className="panel__na">{na}</p>}
          </section>
        </div>
      </div>

      <div className="pulse__cols pulse__cols--3">
        <section className="panel" aria-labelledby="pulse-store">
          <header className="panel__head">
            <h2 id="pulse-store">{t("pulse.store")}</h2>
            <Sparkline values={commitRate} slots={slots} width={110} height={22} tone="live" label={t("pulse.commitsChart")} />
          </header>
          {s.store ? (
            <dl className="kv">
              <div><dt>{t("pulse.commits")}</dt><dd>{fmtInt(s.store.commits)}</dd></div>
              <div><dt>{t("pulse.rows")}</dt><dd>{fmtInt(s.store.rows)}</dd></div>
              <div><dt title={t("pulse.batchHelp")}>{t("pulse.batchMean")}</dt><dd>{s.store.commits ? s.store.batch_mean.toFixed(2) : NOT_REPORTED}</dd></div>
              <div><dt>{t("pulse.batchLargest")}</dt><dd>{fmtInt(s.store.batch_largest)}</dd></div>
              <div><dt>{t("pulse.commitMs")}</dt><dd>{s.store.commits ? fmtMs(s.store.commit_ms_mean) : NOT_REPORTED}</dd></div>
            </dl>
          ) : <p className="panel__na">{na}</p>}
        </section>

        <section className="panel" aria-labelledby="pulse-log">
          <header className="panel__head"><h2 id="pulse-log">{t("pulse.eventLog")}</h2></header>
          {s.event_log ? (
            <dl className="kv">
              <div><dt>{t("pulse.logBytes")}</dt><dd>{fmtBytes(s.event_log.bytes)}</dd></div>
              <div><dt>{t("pulse.segments")}</dt><dd>{fmtInt(s.event_log.segments)}</dd></div>
              <div><dt>{t("pulse.logSessions")}</dt><dd>{fmtInt(s.event_log.sessions)}</dd></div>
            </dl>
          ) : <p className="panel__na">{na}</p>}
          <header className="panel__head panel__head--sub"><h2>{t("pulse.traces")}</h2></header>
          {s.traces ? (
            <dl className="kv">
              <div><dt>{t("pulse.exported")}</dt><dd>{fmtInt(s.traces.exported)}</dd></div>
              <div><dt>{t("pulse.dropped")}</dt><dd>{fmtInt(s.traces.dropped)}</dd></div>
              <div><dt>{t("pulse.failed")}</dt><dd className={s.traces.failed ? "kv--warn" : ""}>{fmtInt(s.traces.failed)}</dd></div>
              <div><dt>{t("pulse.sampledOut")}</dt><dd>{fmtInt(s.traces.sampled_out)}</dd></div>
            </dl>
          ) : <p className="panel__na">{t("pulse.tracesOff")}</p>}
          {s.watch && (
            <>
              <header className="panel__head panel__head--sub"><h2>{t("pulse.watch")}</h2></header>
              <dl className="kv">
                <div><dt title={t("pulse.watchersHelp")}>{t("pulse.watchers")}</dt><dd>{fmtInt(s.watch.watchers)}</dd></div>
                <div><dt title={t("pulse.cutOffHelp")}>{t("pulse.cutOff")}</dt><dd className={s.watch.cut_off ? "kv--warn" : ""}>{fmtInt(s.watch.cut_off)}</dd></div>
              </dl>
            </>
          )}
        </section>

        <section className="panel" aria-labelledby="pulse-node">
          <header className="panel__head"><h2 id="pulse-node">{t("pulse.node")}</h2></header>
          <p className="panel__about">{t("pulse.about")}</p>
          <dl className="kv">
            <div><dt>{t("pulse.nodeId")}</dt><dd className="mono">{health?.node ?? NOT_REPORTED}</dd></div>
            <div><dt>{t("pulse.version")}</dt><dd className="mono">{s.version ?? NOT_REPORTED}</dd></div>
            <div><dt>{t("pulse.uptime")}</dt><dd>{fmtUptime(s.uptime_s)}</dd></div>
            <div><dt>{t("pulse.mode")}</dt><dd>{health?.mode ?? NOT_REPORTED}</dd></div>
            <div><dt>{t("pulse.protocol")}</dt><dd className="mono">{health ? `c3/${health.protocol} · ir/${health.ir}` : NOT_REPORTED}</dd></div>
            <div>
              <dt>{t("pulse.ledgerKey")}</dt>
              <dd className="mono">{health?.ledger?.node_pubkey ? shortId(health.ledger.node_pubkey, 8, 4) : NOT_REPORTED}</dd>
            </div>
            <div>
              <dt>{t("pulse.sealed")}</dt>
              <dd>
                {health?.ledger ? (
                  <a href="#/evidence">{fmtInt(health.ledger.entries)}</a>
                ) : NOT_REPORTED}
              </dd>
            </div>
          </dl>
        </section>
      </div>
    </section>
  );
}
