/**
 * The contract enumerations, from spec/enums.yaml.
 *
 * The same file `scripts/gen_ssot.py` generates the Go constants, the Python
 * tuples and the TypeScript unions from — so what is listed here is what the
 * kernel actually accepts, not a second copy that can disagree with it.
 */

import { useTranslation } from "react-i18next";
import { ENUMS } from "../../reference/generated";
import { Highlight } from "./Highlight";

interface Member {
  name: string;
  description?: string;
  default?: boolean;
}

/** enums.yaml holds scalars (version, effect_type) beside the real lists. */
function enumGroups(): [string, Member[]][] {
  return Object.entries(ENUMS as Record<string, unknown>).filter(
    (entry): entry is [string, Member[]] => Array.isArray(entry[1]),
  );
}

export function EnumsPane({ query }: { query: string }) {
  const { t } = useTranslation();
  const q = query.trim().toLowerCase();

  const groups = enumGroups()
    .map(([name, members]) => ({
      name,
      members: members.filter(
        (m) => !q || m.name.toLowerCase().includes(q) || (m.description ?? "").toLowerCase().includes(q) || name.includes(q),
      ),
    }))
    .filter((g) => g.members.length > 0);

  return (
    <div className="ref-pane">
      <p className="ref-lead">{t("reference.enumsLead")}</p>

      {groups.length === 0 && <div className="empty">{t("reference.noMatch")}</div>}

      {groups.map((g) => (
        <section key={g.name} className="ref-group">
          <h2 className="ref-group__title">
            <code>{g.name}</code>
          </h2>
          <div className="enum-grid">
            {g.members.map((m) => (
              <div key={m.name} className="enum-row">
                <code className="enum-row__name">
                  <Highlight text={m.name} query={q} />
                  {m.default && <span className="enum-row__default">{t("reference.default")}</span>}
                </code>
                <span className="enum-row__desc">
                  <Highlight text={m.description ?? ""} query={q} />
                </span>
              </div>
            ))}
          </div>
        </section>
      ))}
    </div>
  );
}
