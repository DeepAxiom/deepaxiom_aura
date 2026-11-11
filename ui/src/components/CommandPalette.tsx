/**
 * Ctrl/Cmd+K: every place and every action, by typing.
 *
 * The list is built when the palette opens — views, the node's registered
 * graphs, its recent sessions and its connected Expertise — so it finds
 * things that exist on this node right now, not a fixed menu.
 */

import { useEffect, useMemo, useRef, useState } from "react";
import { useTranslation } from "react-i18next";
import { api } from "../api/client";
import type { SessionMeta, SkillManifest } from "../api/types";
import { navigate, toggleTheme, useTheme } from "../app/store";
import { LANGUAGES, setLanguage } from "../i18n";
import { type Command, filterCommands } from "../lib/palette";
import { VIEWS } from "../lib/route";
import { dedupeSkills } from "../graph/model";
import { IconSearch } from "./Icons";

interface Runnable extends Command {
  run: () => void;
}

/** Views and overlays listen for these; the palette does not reach into them. */
export function emitAction(name: string, detail?: unknown): void {
  window.dispatchEvent(new CustomEvent(`aura:${name}`, { detail }));
}

export function CommandPalette({ onClose, onToken }: { onClose: () => void; onToken: () => void }) {
  const { t, i18n } = useTranslation();
  const theme = useTheme();
  const [query, setQuery] = useState("");
  const [cursor, setCursor] = useState(0);
  const [graphs, setGraphs] = useState<string[]>([]);
  const [sessions, setSessions] = useState<SessionMeta[]>([]);
  const [expertise, setExpertise] = useState<SkillManifest[]>([]);
  const listRef = useRef<HTMLDivElement>(null);
  const inputRef = useRef<HTMLInputElement>(null);

  useEffect(() => {
    inputRef.current?.focus();
    api.graphs().then(setGraphs).catch(() => undefined);
    api.sessions().then(setSessions).catch(() => undefined);
    api.expertise().then(setExpertise).catch(() => undefined);
  }, []);

  const commands = useMemo<Runnable[]>(() => {
    const out: Runnable[] = [];
    const nav = t("palette.group.navigate");
    for (const v of VIEWS) {
      out.push({ id: `go.${v}`, group: nav, label: t("palette.goTo", { view: t(`nav.${v}`) }), run: () => navigate(v) });
    }
    out.push({ id: "go.approvals", group: nav, label: t("palette.approvals"), keywords: "gate inbox", run: () => navigate("evidence", "approvals") });

    const act = t("palette.group.actions");
    out.push(
      { id: "theme", group: act, label: t(theme === "dark" ? "palette.themeLight" : "palette.themeDark"), keywords: "theme dark light", run: toggleTheme },
      { id: "verify", group: act, label: t("palette.verify"), keywords: "ledger chain audit", run: () => { navigate("evidence"); setTimeout(() => emitAction("verify"), 50); } },
      { id: "newgraph", group: act, label: t("palette.newGraph"), keywords: "studio canvas", run: () => { navigate("studio"); setTimeout(() => emitAction("studio", "new"), 50); } },
      { id: "keys", group: act, label: t("palette.shortcuts"), keywords: "keyboard help", run: () => { navigate("studio"); setTimeout(() => emitAction("studio", "keys"), 50); } },
      { id: "token", group: act, label: t("palette.token"), keywords: "auth bearer login", run: onToken },
    );
    for (const l of LANGUAGES) {
      if (l.code !== i18n.language) {
        out.push({ id: `lang.${l.code}`, group: act, label: t("palette.language", { language: l.label }), keywords: "language idioma", run: () => setLanguage(l.code) });
      }
    }

    for (const g of graphs) {
      out.push({ id: `graph.${g}`, group: t("palette.group.graphs"), label: t("palette.openGraph", { id: g }), keywords: "studio canvas", run: () => navigate("studio", g) });
    }
    for (const e of dedupeSkills(expertise)) {
      out.push({ id: `exp.${e.id}`, group: t("nav.expertise"), label: e.name, hint: e.capability, keywords: `${e.id} ${e.type}`, run: () => navigate("expertise", e.id) });
    }
    for (const s of sessions.slice(0, 20)) {
      out.push({
        id: `sess.${s.session_id}`, group: t("nav.sessions"), label: s.session_id, hint: s.graph_id,
        keywords: s.ended ? "ended" : "live", run: () => navigate("sessions", s.session_id),
      });
    }
    return out;
  }, [t, i18n.language, theme, graphs, sessions, expertise, onToken]);

  const matches = useMemo(() => filterCommands(commands, query, 40), [commands, query]);
  useEffect(() => setCursor(0), [query]);
  useEffect(() => {
    listRef.current?.querySelector("[aria-selected='true']")?.scrollIntoView({ block: "nearest" });
  }, [cursor]);

  const run = (c: Runnable | undefined) => {
    if (!c) return;
    onClose();
    c.run();
  };

  let lastGroup = "";
  return (
    <div className="scrim scrim--top" onPointerDown={onClose}>
      <div className="cmdk" role="dialog" aria-modal="true" aria-label={t("palette.title")} onPointerDown={(e) => e.stopPropagation()}>
        <div className="cmdk__input">
          <IconSearch size={16} />
          <input
            ref={inputRef}
            role="combobox"
            aria-expanded="true"
            aria-controls="cmdk-list"
            aria-activedescendant={matches[cursor] ? `cmdk-${matches[cursor].id}` : undefined}
            placeholder={t("palette.placeholder")}
            value={query}
            onChange={(e) => setQuery(e.target.value)}
            onKeyDown={(e) => {
              if (e.key === "Escape") { e.preventDefault(); onClose(); }
              else if (e.key === "ArrowDown") { e.preventDefault(); setCursor((c) => Math.min(c + 1, matches.length - 1)); }
              else if (e.key === "ArrowUp") { e.preventDefault(); setCursor((c) => Math.max(c - 1, 0)); }
              else if (e.key === "Enter") { e.preventDefault(); run(matches[cursor]); }
            }}
          />
          <kbd className="kbd">Esc</kbd>
        </div>
        <div className="cmdk__list" id="cmdk-list" role="listbox" ref={listRef}>
          {matches.length === 0 && <div className="cmdk__empty">{t("palette.empty")}</div>}
          {matches.map((c, i) => {
            const heading = c.group !== lastGroup ? c.group : "";
            lastGroup = c.group;
            return (
              <div key={c.id}>
                {heading && <div className="cmdk__group">{heading}</div>}
                <div
                  id={`cmdk-${c.id}`}
                  role="option"
                  aria-selected={i === cursor}
                  className={`cmdk__item ${i === cursor ? "cmdk__item--on" : ""}`}
                  onPointerMove={() => setCursor(i)}
                  onClick={() => run(c)}
                >
                  <span className="cmdk__label">{c.label}</span>
                  {c.hint && <span className="cmdk__hint">{c.hint}</span>}
                </div>
              </div>
            );
          })}
        </div>
      </div>
    </div>
  );
}
