/**
 * Every CLI command, grouped as the guide groups them.
 *
 * The data comes from `reference/generated.ts`, which is produced from GUIDE.md
 * and checked against the binary's own usage block — so a command that exists
 * and is undocumented fails CI rather than quietly missing from this list.
 */

import { useTranslation } from "react-i18next";
import { COMMANDS, UNDOCUMENTED } from "../../reference/generated";
import { Highlight } from "./Highlight";

export function CommandsPane({ query }: { query: string }) {
  const { t } = useTranslation();
  const q = query.trim().toLowerCase();

  const groups = COMMANDS.map((g) => ({
    ...g,
    commands: g.commands.filter(
      (c) =>
        !q ||
        c.syntax.toLowerCase().includes(q) ||
        c.purpose.toLowerCase().includes(q) ||
        c.options.some((o) => (o.syntax + o.purpose).toLowerCase().includes(q)),
    ),
  })).filter((g) => g.commands.length > 0);

  const total = groups.reduce((n, g) => n + g.commands.length, 0);

  return (
    <div className="ref-pane">
      {UNDOCUMENTED.length > 0 && (
        <div className="error-banner">
          {t("reference.undocumented", { list: UNDOCUMENTED.join(", ") })}
        </div>
      )}

      {total === 0 && <div className="empty">{t("reference.noMatch")}</div>}

      {groups.map((g) => (
        <section key={g.section} className="ref-group">
          <h2 className="ref-group__title">{g.section}</h2>
          {g.commands.map((c) => (
            <article key={c.syntax} className="cmd">
              <code className="cmd__syntax">
                <Highlight text={c.syntax} query={q} />
              </code>
              <p className="cmd__purpose">
                <Highlight text={c.purpose} query={q} />
              </p>
              {c.options.length > 0 && (
                <div className="cmd__opts">
                  {c.options.map((o) => (
                    <div key={o.syntax} className="cmd__opt">
                      <code>
                        <Highlight text={o.syntax} query={q} />
                      </code>
                      <span>
                        <Highlight text={o.purpose} query={q} />
                      </span>
                    </div>
                  ))}
                </div>
              )}
            </article>
          ))}
        </section>
      ))}
    </div>
  );
}
