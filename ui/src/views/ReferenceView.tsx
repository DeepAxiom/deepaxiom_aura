/**
 * The reference: every command, every contract, every schema, every enum —
 * complete, offline, and served by the node it documents.
 *
 * A shell only. Each tab is its own component and each reads from
 * `reference/generated.ts`, which is produced from GUIDE.md, the binary's usage
 * block and spec/ by `scripts/gen_ui_reference.py`. Nothing on this screen is
 * typed by hand, which is the only way a reference of this size stays true: the
 * generator fails CI when a command exists and the guide has not documented it.
 */

import { useState } from "react";
import { useTranslation } from "react-i18next";
import { CommandsPane } from "../components/reference/CommandsPane";
import { ContractsPane } from "../components/reference/ContractsPane";
import { EnumsPane } from "../components/reference/EnumsPane";
import { SchemasPane } from "../components/reference/SchemasPane";
import { COMMANDS, CONTRACTS, SCHEMAS, SPEC_VERSION } from "../reference/generated";

const TABS = ["commands", "contracts", "schemas", "enums"] as const;
type Tab = (typeof TABS)[number];

export function ReferenceView() {
  const { t } = useTranslation();
  const [tab, setTab] = useState<Tab>("commands");
  const [query, setQuery] = useState("");

  const counts: Record<Tab, number> = {
    commands: COMMANDS.reduce((n, g) => n + g.commands.length, 0),
    contracts: CONTRACTS.length,
    schemas: SCHEMAS.length,
    enums: 0,
  };

  return (
    <section className="view view--flush reference">
      <div className="ref-bar">
        <nav className="ref-tabs">
          {TABS.map((id) => (
            <button
              key={id}
              className={`ref-tab ${tab === id ? "ref-tab--active" : ""}`}
              onClick={() => setTab(id)}
            >
              {t(`reference.tab.${id}`)}
              {counts[id] > 0 && <span className="ref-tab__count">{counts[id]}</span>}
            </button>
          ))}
        </nav>
        <span className="ref-bar__spacer" />
        <input
          className="input ref-search"
          placeholder={t("reference.search")}
          value={query}
          onChange={(e) => setQuery(e.target.value)}
        />
        <code className="ref-bar__version">v{SPEC_VERSION}</code>
      </div>

      <div className="ref-body">
        {tab === "commands" && <CommandsPane query={query} />}
        {tab === "contracts" && <ContractsPane query={query} />}
        {tab === "schemas" && <SchemasPane query={query} />}
        {tab === "enums" && <EnumsPane query={query} />}
      </div>
    </section>
  );
}
