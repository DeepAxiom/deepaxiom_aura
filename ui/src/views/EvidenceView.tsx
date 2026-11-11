/**
 * Evidence — what this node did to the world, and whether the record holds.
 *
 * Three things, all read from routes the kernel already serves:
 *
 *   - the verify result (`GET /v1/ledger/verify`), which recomputes the whole
 *     chain with the same function `aura verify` runs offline;
 *   - the sealed entries (`GET /v1/ledger`), one per effect;
 *   - the approvals inbox (`GET /v1/approvals`): gates raised for a client
 *     that is not a person, waiting for someone to answer.
 *
 * Nothing here can seal or alter an entry. The ledger's write side is inside
 * the executor and has no HTTP surface.
 */

import { useCallback, useEffect, useMemo, useState } from "react";
import { useTranslation } from "react-i18next";
import { api } from "../api/client";
import type { Approval, LedgerEntry, LedgerHead, LedgerVerify } from "../api/types";
import { navigate, replaceRoute, toast, toastError, useNow } from "../app/store";
import { IconAlert, IconCheck, IconRefresh, IconSeal, IconSearch } from "../components/Icons";
import { Copyable, Empty, ErrorNote, Loading, Tabs } from "../components/ui";
import { NOT_REPORTED, fmtAgo, fmtInt, shortId } from "../lib/format";

type Tab = "ledger" | "approvals";

export function EvidenceView({ args }: { args: string[] }) {
  const { t } = useTranslation();
  const tab: Tab = args[0] === "approvals" ? "approvals" : "ledger";
  const entrySeq = args[0] === "entry" ? Number(args[1]) : null;

  const [verify, setVerify] = useState<LedgerVerify | null>(null);
  const [verifying, setVerifying] = useState(false);
  const [verifyError, setVerifyError] = useState("");
  const [verifiedAt, setVerifiedAt] = useState(0);
  const [head, setHead] = useState<LedgerHead | null>(null);
  const [entries, setEntries] = useState<LedgerEntry[] | null>(null);
  const [total, setTotal] = useState(0);
  const [noLedger, setNoLedger] = useState(false);
  const [approvals, setApprovals] = useState<Approval[] | null | undefined>(undefined);
  const [error, setError] = useState("");
  const now = useNow(1000);

  const runVerify = useCallback(() => {
    setVerifying(true);
    setVerifyError("");
    api.ledgerVerify()
      .then((v) => { setVerify(v); setVerifiedAt(Date.now()); })
      .catch((e) => setVerifyError((e as Error).message))
      .finally(() => setVerifying(false));
  }, []);

  const load = useCallback(async () => {
    try {
      const [page, h, a] = await Promise.all([api.ledger(1, 1000), api.ledgerHead(), api.approvals()]);
      setNoLedger(page === null);
      setEntries(page?.entries ?? []);
      setTotal(page?.total ?? 0);
      setHead(h);
      setApprovals(a);
      setError("");
    } catch (e) {
      setError((e as Error).message);
    }
  }, []);

  useEffect(() => {
    void load();
    runVerify();
    const timer = setInterval(() => { if (!document.hidden) void load(); }, 2000);
    const onVerify = () => runVerify();
    window.addEventListener("aura:verify", onVerify);
    return () => { clearInterval(timer); window.removeEventListener("aura:verify", onVerify); };
  }, [load, runVerify]);

  // An entry does not carry its own hash; the next entry's `prev` is it, and
  // the head's is the last one's. That is the chain, read back.
  const hashOf = useMemo(() => {
    const m = new Map<number, string>();
    (entries ?? []).forEach((e, i, all) => {
      const next = all[i + 1];
      if (next && next.seq === e.seq + 1) m.set(e.seq, next.prev);
      else if (head && head.seq === e.seq) m.set(e.seq, head.head_hash);
    });
    return m;
  }, [entries, head]);

  const report = verify?.report;
  const pending = approvals?.length ?? 0;

  return (
    <section className="view evidence">
      <div className={`verdict ${verify ? (verify.sound ? "verdict--ok" : "verdict--bad") : ""}`} role="status">
        <div className="verdict__mark" aria-hidden>
          {verify ? (verify.sound ? <IconCheck size={22} /> : <IconAlert size={22} />) : <IconSeal size={22} />}
        </div>
        <div className="verdict__body">
          <div className="verdict__title">
            {verifyError
              ? t("evidence.verifyFailed")
              : !verify
                ? t("evidence.verifying")
                : verify.sound
                  ? t("evidence.sound", { count: report?.total_entries ?? 0 })
                  : t("evidence.unsound")}
          </div>
          <div className="verdict__sub">
            {verifyError || (report && (
              <>
                {t(report.chain_intact ? "evidence.chainIntact" : "evidence.chainBroken")}
                {" · "}{t("evidence.checkpoints", { valid: report.checkpoints_valid, total: report.checkpoints })}
                {" · "}{t("evidence.witnesses", { valid: report.witnesses_valid, total: report.witnesses })}
                {" · "}{t("evidence.signedApprovals", { count: report.approvals, invalid: report.approvals_invalid })}
                {report.merkle_mismatches > 0 && <b>{" · "}{t("evidence.merkleMismatch", { count: report.merkle_mismatches })}</b>}
                {!report.key_available && <b>{" · "}{t("evidence.noKey")}</b>}
              </>
            ))}
          </div>
        </div>
        <div className="verdict__side">
          {verifiedAt > 0 && <span className="verdict__when">{t("evidence.verifiedAgo", { when: fmtAgo(verifiedAt, now) })}</span>}
          <button className="btn btn--sm" onClick={runVerify} disabled={verifying}>
            <IconRefresh size={13} />{verifying ? t("evidence.verifying") : t("evidence.verifyAgain")}
          </button>
        </div>
      </div>

      {(head || report) && (
        <dl className="facts facts--panel">
          <div><dt>{t("evidence.head.seq")}</dt><dd>{head ? fmtInt(head.seq) : NOT_REPORTED}</dd></div>
          <div><dt>{t("evidence.head.hash")}</dt><dd>{head?.head_hash ? <Copyable value={head.head_hash}><code>{shortId(head.head_hash, 10, 6)}</code></Copyable> : NOT_REPORTED}</dd></div>
          <div><dt>{t("evidence.head.merkle")}</dt><dd>{head?.merkle_root ? <Copyable value={head.merkle_root}><code>{shortId(head.merkle_root, 10, 6)}</code></Copyable> : NOT_REPORTED}</dd></div>
          <div><dt>{t("evidence.head.key")}</dt><dd>{report?.pubkey_fingerprint ? <code>{report.pubkey_fingerprint}</code> : NOT_REPORTED}</dd></div>
          <div><dt>{t("evidence.head.rotations")}</dt><dd>{report ? fmtInt(report.key_rotations) : NOT_REPORTED}</dd></div>
        </dl>
      )}

      <Tabs
        label={t("nav.evidence")}
        value={tab}
        onChange={(id) => navigate("evidence", id === "ledger" ? undefined : id)}
        tabs={[
          { id: "ledger", label: t("evidence.tab.ledger"), count: total },
          { id: "approvals", label: t("evidence.tab.approvals"), count: approvals ? pending : null },
        ]}
      />

      {error && <ErrorNote message={error} onRetry={() => void load()} />}

      {tab === "ledger" && (
        noLedger ? <Empty title={t("evidence.noLedger")} />
        : entries === null ? <Loading />
        : <LedgerTable entries={entries} total={total} hashOf={hashOf} openSeq={entrySeq} now={now} />
      )}

      {tab === "approvals" && (
        approvals === undefined ? <Loading />
        : approvals === null ? <Empty title={t("evidence.noQueue")}>{t("evidence.noQueueHelp")}</Empty>
        : <ApprovalsInbox approvals={approvals} now={now} onResolved={() => void load()} />
      )}
    </section>
  );
}

function LedgerTable({
  entries, total, hashOf, openSeq, now,
}: {
  entries: LedgerEntry[];
  total: number;
  hashOf: Map<number, string>;
  openSeq: number | null;
  now: number;
}) {
  const { t } = useTranslation();
  const [query, setQuery] = useState("");
  const q = query.trim().toLowerCase();
  const shown = useMemo(
    () =>
      [...entries].reverse().filter((e) =>
        !q || `${e.seq} ${e.capability} ${e.actor} ${e.decision} ${e.outcome} ${e.session} ${e.envelope} ${e.approver?.operator ?? ""} ${hashOf.get(e.seq) ?? ""}`
          .toLowerCase().includes(q)),
    [entries, q, hashOf],
  );

  if (entries.length === 0) {
    return <Empty title={t("evidence.empty")}>{t("evidence.emptyHelp")}</Empty>;
  }

  return (
    <>
      <div className="tablebar">
        <label className="searchbox">
          <IconSearch size={14} />
          <input placeholder={t("evidence.search")} aria-label={t("evidence.search")} value={query} onChange={(e) => setQuery(e.target.value)} />
        </label>
        <span className="tablebar__note">
          {total > entries.length ? t("evidence.showingSome", { shown: entries.length, total }) : t("evidence.showingAll", { count: total })}
        </span>
      </div>
      <div className="table-wrap">
        <table className="table table--rows">
          <thead>
            <tr>
              <th className="num">{t("evidence.col.seq")}</th>
              <th>{t("evidence.col.when")}</th>
              <th>{t("evidence.col.capability")}</th>
              <th>{t("evidence.col.decision")}</th>
              <th>{t("evidence.col.approver")}</th>
              <th>{t("evidence.col.session")}</th>
              <th>{t("evidence.col.receipt")}</th>
            </tr>
          </thead>
          <tbody>
            {shown.map((e) => {
              const open = e.seq === openSeq;
              const hash = hashOf.get(e.seq);
              return [
                <tr
                  key={e.seq}
                  className={`${open ? "row--on" : ""} ${e.outcome !== "delivered" ? "row--denied" : ""}`}
                  tabIndex={0}
                  aria-expanded={open}
                  onClick={() => replaceRoute("evidence", ...(open ? [] : ["entry", e.seq]))}
                  onKeyDown={(ev) => { if (ev.key === "Enter") replaceRoute("evidence", ...(open ? [] : ["entry", e.seq])); }}
                >
                  <td className="num mono">{e.seq}</td>
                  <td title={new Date(e.ts).toISOString()}>{fmtAgo(e.ts, now)}</td>
                  <td>
                    <div className="mono">{e.capability}</div>
                    <div className="cell-sub">{e.actor}</div>
                  </td>
                  <td>
                    <span className={`pill pill--${e.outcome === "delivered" ? "ok" : "err"}`}>{e.outcome}</span>
                    <span className="cell-sub cell-sub--inline">{e.decision}{e.waived ? ` · ${t("evidence.waived")}` : ""}</span>
                  </td>
                  <td>
                    {e.approver?.operator
                      ? <span className="approver"><IconCheck size={12} />{e.approver.operator}</span>
                      : <span className="cell-na" title={t("evidence.unsignedHelp")}>{e.decision === "gate" ? t("evidence.unsigned") : NOT_REPORTED}</span>}
                  </td>
                  <td><a className="mono" href={`#/sessions/${encodeURIComponent(e.session)}/${e.envelope}`} onClick={(ev) => ev.stopPropagation()}>{e.session}</a></td>
                  <td>{hash ? <Copyable value={hash}><code>{shortId(hash, 8, 4)}</code></Copyable> : NOT_REPORTED}</td>
                </tr>,
                open && (
                  <tr key={`${e.seq}-d`} className="row-detail">
                    <td colSpan={7}>
                      <div className="chainlink">
                        <span className="chainlink__box"><b>prev</b><code>{e.prev ? shortId(e.prev, 12, 6) : t("evidence.genesis")}</code></span>
                        <span className="chainlink__arrow" aria-hidden>→</span>
                        <span className="chainlink__box chainlink__box--this"><b>#{e.seq}</b><code>{hash ? shortId(hash, 12, 6) : NOT_REPORTED}</code></span>
                      </div>
                      <dl className="kv kv--wide">
                        <div><dt>ts</dt><dd>{new Date(e.ts).toISOString()}</dd></div>
                        <div><dt>node</dt><dd className="mono">{e.node}</dd></div>
                        <div><dt>envelope</dt><dd><Copyable value={e.envelope}><code>{e.envelope}</code></Copyable></dd></div>
                        <div><dt>cause</dt><dd><Copyable value={e.cause}><code>{e.cause}</code></Copyable></dd></div>
                        <div><dt>policy</dt><dd><Copyable value={e.policy}><code>{shortId(e.policy, 12, 6)}</code></Copyable></dd></div>
                        <div><dt>payload_sha256</dt><dd><Copyable value={e.payload_sha256}><code>{shortId(e.payload_sha256, 12, 6)}</code></Copyable></dd></div>
                        {e.inference?.length ? <div><dt>inference</dt><dd>{e.inference.map((h) => <Copyable key={h} value={h}><code>{shortId(h, 12, 6)}</code></Copyable>)}</dd></div> : null}
                        {e.compensates ? <div><dt>compensates</dt><dd><code>{shortId(e.compensates, 12, 6)}</code></dd></div> : null}
                      </dl>
                      <details className="raw">
                        <summary>{t("evidence.raw")}</summary>
                        <pre>{JSON.stringify(e, null, 2)}</pre>
                      </details>
                    </td>
                  </tr>
                ),
              ];
            })}
          </tbody>
        </table>
      </div>
    </>
  );
}

function ApprovalsInbox({ approvals, now, onResolved }: { approvals: Approval[]; now: number; onResolved: () => void }) {
  const { t } = useTranslation();
  const [busy, setBusy] = useState("");

  const answer = async (a: Approval, approve: boolean) => {
    setBusy(a.id);
    try {
      await api.resolveApproval(a.id, approve);
      toast("ok", t(approve ? "evidence.approved" : "evidence.denied"));
      onResolved();
    } catch (e) {
      toastError(e);
    } finally {
      setBusy("");
    }
  };

  return (
    <>
      <p className="prose-note">{t("evidence.inboxNote")}</p>
      {approvals.length === 0 ? (
        <Empty title={t("evidence.inboxEmpty")}>{t("evidence.inboxEmptyHelp")}</Empty>
      ) : (
        <div className="inbox">
          {approvals.map((a) => {
            const needsContext = (a.context_required?.length ?? 0) > 0;
            const expires = Date.parse(a.expires);
            return (
              <article key={a.id} className="inbox__card">
                <header>
                  <span className="pill pill--warn">{t("evidence.held")}</span>
                  <code className="inbox__cap">{a.capability || a.tool || a.id}</code>
                  <span className="inbox__when">
                    {t("evidence.asked", { when: fmtAgo(Date.parse(a.requested), now) })}
                    {Number.isFinite(expires) && ` · ${t("evidence.expires", { when: fmtAgo(now, Math.max(expires, now)) })}`}
                  </span>
                </header>
                <p className="inbox__q">{a.question}</p>
                <dl className="kv kv--wide">
                  {a.origin && <div><dt>{t("evidence.origin")}</dt><dd>{a.origin}{a.tool ? ` · ${a.tool}` : ""}</dd></div>}
                  {a.session && <div><dt>{t("evidence.col.session")}</dt><dd><a className="mono" href={`#/sessions/${encodeURIComponent(a.session)}`}>{a.session}</a></dd></div>}
                  {a.envelope && <div><dt>envelope</dt><dd><code>{a.envelope}</code></dd></div>}
                </dl>
                {a.arguments !== undefined && a.arguments !== null && (
                  <pre className="inbox__args">{JSON.stringify(a.arguments, null, 2)}</pre>
                )}
                {needsContext ? (
                  <p className="prose-note prose-note--warn">{t("evidence.needsSigned", { labels: a.context_required!.join(", "), id: a.id })}</p>
                ) : (
                  <div className="inbox__actions">
                    <button className="btn btn--primary" disabled={busy === a.id} onClick={() => void answer(a, true)}>{t("gate.approve")}</button>
                    <button className="btn btn--danger" disabled={busy === a.id} onClick={() => void answer(a, false)}>{t("gate.deny")}</button>
                    <span className="inbox__hint">{t("evidence.unsignedNote", { id: a.id })}</span>
                  </div>
                )}
              </article>
            );
          })}
        </div>
      )}
    </>
  );
}
