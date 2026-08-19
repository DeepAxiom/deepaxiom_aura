/**
 * The five frozen contracts, in full.
 *
 * Verbatim from `spec/c*.md` — not a summary. These are normative documents
 * and the node that implements them is the right place to read them, offline,
 * at the version this binary actually speaks.
 */

import { useMemo, useState } from "react";
import { useTranslation } from "react-i18next";
import { Markdown } from "../../markdown/Markdown";
import { outline, parse } from "../../markdown/parse";
import { CONTRACTS } from "../../reference/generated";

export function ContractsPane({ query }: { query: string }) {
  const { t } = useTranslation();
  const [openId, setOpenId] = useState(CONTRACTS[0]?.id ?? "");
  const contract = CONTRACTS.find((c) => c.id === openId) ?? CONTRACTS[0];

  const toc = useMemo(
    () => (contract ? outline(parse(contract.body)).filter((h) => h.level === 2) : []),
    [contract],
  );

  const q = query.trim().toLowerCase();
  const hits = useMemo(
    () =>
      q
        ? CONTRACTS.map((c) => ({
            id: c.id,
            n: c.body.toLowerCase().split(q).length - 1,
          })).filter((h) => h.n > 0)
        : [],
    [q],
  );

  if (!contract) return null;

  return (
    <div className="ref-pane ref-pane--split">
      <nav className="ref-side">
        {CONTRACTS.map((c) => {
          const hit = hits.find((h) => h.id === c.id);
          return (
            <button
              key={c.id}
              className={`ref-side__item ${c.id === openId ? "ref-side__item--active" : ""}`}
              onClick={() => setOpenId(c.id)}
            >
              <span className="ref-side__id">{c.id}</span>
              <span className="ref-side__label">{c.title}</span>
              {hit && <span className="ref-side__hits">{hit.n}</span>}
            </button>
          );
        })}

        {toc.length > 0 && (
          <div className="ref-toc">
            <span className="ref-toc__title">{t("reference.onThisPage")}</span>
            {toc.map((h) => (
              <a key={h.slug} href={`#${h.slug}`} className="ref-toc__link">
                {h.text}
              </a>
            ))}
          </div>
        )}
      </nav>

      <article className="ref-doc">
        <header className="ref-doc__head">
          <h1>
            {contract.id} — {contract.title}
          </h1>
          <code>{contract.file}</code>
          {contract.status && <p className="ref-doc__status">{contract.status}</p>}
        </header>
        <Markdown source={contract.body} />
      </article>
    </div>
  );
}
