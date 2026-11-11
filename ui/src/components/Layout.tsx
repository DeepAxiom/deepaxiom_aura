/**
 * The shell: where you are (sidebar), what you are connected to (top bar),
 * and what just went wrong (toasts).
 */

import { useState } from "react";
import { useTranslation } from "react-i18next";
import { hasToken, setToken } from "../api/client";
import type { Health } from "../api/types";
import { dismissToast, navigate, toggleTheme, useLink, useTheme, useToasts } from "../app/store";
import { LANGUAGES, setLanguage } from "../i18n";
import { fmtUptime } from "../lib/format";
import type { ViewId } from "../lib/route";
import type { NodeStats } from "../lib/stats";
import { SPEC_VERSION } from "../reference/generated";
import {
  IconAlert, IconBook, IconCheck, IconGraph, IconGrid, IconKey, IconLogo, IconMoon, IconPlug,
  IconPulse, IconSeal, IconSearch, IconStudio, IconSun, IconTrace, IconX,
} from "./Icons";
import { Dialog } from "./ui";

export type { ViewId };

const NAV: { id: ViewId; icon: (p: { size?: number }) => React.ReactNode; section?: boolean }[] = [
  { id: "pulse", icon: IconPulse },
  { id: "studio", icon: IconStudio },
  { id: "sessions", icon: IconTrace },
  { id: "evidence", icon: IconSeal },
  { id: "expertise", icon: IconGrid, section: true },
  { id: "graphs", icon: IconGraph },
  { id: "projections", icon: IconPlug },
  { id: "reference", icon: IconBook, section: true },
];

export function Sidebar({ view, stats }: { view: ViewId; stats: NodeStats | null }) {
  const { t } = useTranslation();
  const pending = stats?.approvals_pending ?? 0;
  return (
    <aside className="sidebar">
      <a className="sidebar__brand" href="#/pulse" aria-label="aura">
        <IconLogo size={22} />
        <span className="sidebar__word">aura</span>
      </a>
      <nav className="sidebar__nav" aria-label={t("nav.label")}>
        {NAV.map(({ id, icon: Icon, section }) => (
          <a
            key={id}
            href={`#/${id}`}
            className={`sidebar__link ${view === id ? "sidebar__link--active" : ""} ${section ? "sidebar__link--section" : ""}`}
            aria-current={view === id ? "page" : undefined}
            title={t(`nav.${id}`)}
          >
            <Icon size={16} />
            <span className="sidebar__text">{t(`nav.${id}`)}</span>
            {id === "evidence" && pending > 0 && (
              <span className="sidebar__count" title={t("nav.pending", { count: pending })}>{pending}</span>
            )}
            {id === "sessions" && (stats?.sessions_live ?? 0) > 0 && (
              <span className="sidebar__live" title={t("nav.live", { count: stats?.sessions_live })}>{stats?.sessions_live}</span>
            )}
          </a>
        ))}
      </nav>
    </aside>
  );
}

export function TopBar({
  view, health, stats, onPalette, onToken,
}: {
  view: ViewId;
  health: Health | null;
  stats: NodeStats | null;
  onPalette: () => void;
  onToken: () => void;
}) {
  const { t, i18n } = useTranslation();
  const link = useLink();
  const theme = useTheme();
  const state = link === "unauthorized" ? "unauthorized" : health?.ok ? "ok" : link === "unknown" ? "unknown" : "down";
  const mac = /Mac|iPhone|iPad/.test(navigator.platform ?? "");

  return (
    <header className="topbar">
      <h1 className="topbar__title">{t(`nav.${view}`)}</h1>

      <button
        className={`conn conn--${state}`}
        onClick={onToken}
        title={t(`topbar.help.${state}`)}
        aria-label={t(`topbar.state.${state}`)}
      >
        <span className="conn__dot" aria-hidden />
        {state === "ok" && health ? (
          <>
            <span className="conn__node">{health.node}</span>
            <span className="conn__meta">{health.mode}</span>
            {stats && <span className="conn__meta">{t("topbar.up", { uptime: fmtUptime(stats.uptime_s) })}</span>}
          </>
        ) : (
          <span className="conn__node">{t(`topbar.state.${state}`)}</span>
        )}
      </button>

      <span className="topbar__spacer" />

      <button className="search-trigger" onClick={onPalette} aria-label={t("palette.title")}>
        <IconSearch size={14} />
        <span>{t("palette.trigger")}</span>
        <kbd className="kbd">{mac ? "⌘K" : "Ctrl K"}</kbd>
      </button>

      <select
        className="select select--bare"
        aria-label={t("common.language")}
        value={i18n.language}
        onChange={(e) => setLanguage(e.target.value)}
      >
        {LANGUAGES.map((l) => <option key={l.code} value={l.code}>{l.code.toUpperCase()}</option>)}
      </select>

      <button
        className="iconbtn"
        onClick={toggleTheme}
        aria-label={t(theme === "dark" ? "palette.themeLight" : "palette.themeDark")}
        title={t(theme === "dark" ? "palette.themeLight" : "palette.themeDark")}
      >
        {theme === "dark" ? <IconSun size={16} /> : <IconMoon size={16} />}
      </button>

      {/*
        The node's own version when it reports one, else the spec version this
        UI was generated against. Never a literal: a hand-written version
        string sat in this corner claiming a release the node had long left.
      */}
      <code className="topbar__version" title={stats?.version ? t("topbar.versionNode") : t("topbar.versionSpec")}>
        v{stats?.version ?? SPEC_VERSION}
      </code>
    </header>
  );
}

/** Paste the node token. Kept in sessionStorage by api/client; it dies with the tab. */
export function TokenDialog({ onClose }: { onClose: () => void }) {
  const { t } = useTranslation();
  const [value, setValue] = useState("");
  const link = useLink();
  const save = () => {
    setToken(value);
    onClose();
    // Everything on screen was fetched without it; start again with it.
    location.reload();
  };
  return (
    <Dialog title={t("token.title")} onClose={onClose}>
      <p className="dialog__text">
        {link === "unauthorized" ? t("token.needed") : hasToken() ? t("token.have") : t("token.none")}
      </p>
      <label className="field">
        <span>{t("token.label")}</span>
        <input
          className="input input--mono"
          type="password"
          autoComplete="off"
          data-autofocus
          value={value}
          onChange={(e) => setValue(e.target.value)}
          onKeyDown={(e) => e.key === "Enter" && value.trim() && save()}
        />
        <em>{t("token.help")}</em>
      </label>
      <div className="dialog__actions">
        <button className="btn btn--ghost" onClick={onClose}>{t("common.cancel")}</button>
        <button className="btn btn--primary" onClick={save} disabled={!value.trim()}>
          <IconKey size={14} />{t("token.save")}
        </button>
      </div>
    </Dialog>
  );
}

export function Toasts() {
  const toasts = useToasts();
  const { t } = useTranslation();
  return (
    <div className="toasts" role="region" aria-label={t("common.notifications")} aria-live="polite">
      {toasts.map((x) => (
        <div key={x.id} className={`toast toast--${x.kind}`} role={x.kind === "error" ? "alert" : "status"}>
          {x.kind === "error" ? <IconAlert size={15} /> : <IconCheck size={15} />}
          <span className="toast__text">{x.text}</span>
          <button className="iconbtn iconbtn--sm" onClick={() => dismissToast(x.id)} aria-label={t("common.close")}>
            <IconX size={13} />
          </button>
        </div>
      ))}
    </div>
  );
}

/** A banner under the top bar when the node is not answering or not accepting us. */
export function LinkBanner({ health, onToken }: { health: Health | null; onToken: () => void }) {
  const { t } = useTranslation();
  const link = useLink();
  if (link === "unauthorized") {
    return (
      <div className="banner banner--warn" role="alert">
        <IconKey size={15} />
        <span>{t("topbar.help.unauthorized")}</span>
        <button className="btn btn--sm" onClick={onToken}>{t("token.title")}</button>
      </div>
    );
  }
  if (link === "unreachable" && !health) {
    return (
      <div className="banner banner--err" role="alert">
        <IconAlert size={15} />
        <span>{t("topbar.help.down")}</span>
        <button className="btn btn--sm btn--ghost" onClick={() => navigate("reference")}>{t("nav.reference")}</button>
      </div>
    );
  }
  return null;
}
