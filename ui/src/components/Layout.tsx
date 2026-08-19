import { useEffect, useState } from "react";
import { useTranslation } from "react-i18next";
import { api } from "../api/client";
import type { Health } from "../api/types";
import { LANGUAGES, setLanguage } from "../i18n";
import { SPEC_VERSION } from "../reference/generated";
import {
  IconBolt,
  IconBook,
  IconCanvas,
  IconChat,
  IconGraph,
  IconGrid,
  IconList,
  IconLogo,
  IconMic,
  IconPlug,
} from "./Icons";

export type ViewId =
  | "chat"
  | "voice"
  | "operate"
  | "canvas"
  | "skills"
  | "projections"
  | "graphs"
  | "sessions"
  | "reference";

const NAV: { id: ViewId; icon: (p: { size?: number }) => React.ReactNode }[] = [
  { id: "chat", icon: IconChat },
  { id: "voice", icon: IconMic },
  { id: "operate", icon: IconBolt },
  { id: "canvas", icon: IconCanvas },
  { id: "skills", icon: IconGrid },
  { id: "projections", icon: IconPlug },
  { id: "graphs", icon: IconGraph },
  { id: "sessions", icon: IconList },
  { id: "reference", icon: IconBook },
];

export function Sidebar({ view, onNavigate }: { view: ViewId; onNavigate: (v: ViewId) => void }) {
  const { t, i18n } = useTranslation();
  return (
    <aside className="sidebar">
      <div className="sidebar__brand">
        <IconLogo />
        <span>aura</span>&nbsp;control
      </div>
      <nav className="sidebar__nav">
        {NAV.map(({ id, icon: Icon }) => (
          <button
            key={id}
            className={`sidebar__link ${view === id ? "sidebar__link--active" : ""}`}
            onClick={() => onNavigate(id)}
          >
            <Icon size={16} />
            {t(`nav.${id}`)}
          </button>
        ))}
      </nav>
      <div className="sidebar__footer">
        <select
          className="select"
          aria-label={t("common.language")}
          value={i18n.language}
          onChange={(e) => setLanguage(e.target.value)}
        >
          {LANGUAGES.map((l) => (
            <option key={l.code} value={l.code}>
              {l.label}
            </option>
          ))}
        </select>
      </div>
    </aside>
  );
}

export function TopBar() {
  const { t } = useTranslation();
  const [health, setHealth] = useState<Health | null>(null);
  useEffect(() => {
    const load = () => api.health().then(setHealth).catch(() => setHealth(null));
    load();
    const timer = setInterval(load, 10_000);
    return () => clearInterval(timer);
  }, []);
  return (
    <header className="topbar">
      <span className={`topbar__dot ${health?.ok ? "topbar__dot--ok" : ""}`} />
      {health
        ? t("topbar.connected", { node: health.node, mode: health.mode, protocol: health.protocol })
        : t("topbar.disconnected")}
      <span className="topbar__spacer" />
      {/*
        From spec/VERSION, via the generated reference — not a literal.
        It was hand-written as `v0.1.0-h1` and never updated, so it sat in the
        corner of every screen claiming a version the node had long since left
        behind: `aura version` and the Reference tab both said 0.3.0. Two places
        on one screen disagreed, which is what a hand-written version string
        eventually always does.
      */}
      <code>v{SPEC_VERSION}</code>
    </header>
  );
}
