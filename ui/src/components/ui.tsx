/**
 * The small shared pieces every view is built from. Styling lives in
 * styles/global.css under the same class names; these components exist so the
 * ARIA and keyboard behaviour is written once.
 */

import { useEffect, useId, useRef } from "react";
import { useTranslation } from "react-i18next";
import { sparkSegments } from "../lib/stats";
import { toast } from "../app/store";
import { IconX, TYPE_GLYPH } from "./Icons";

/** A line of recent values. Gaps stay gaps; an empty series draws only the baseline. */
export function Sparkline({
  values, slots, width = 120, height = 28, tone = "live", label,
}: {
  values: (number | null)[];
  slots?: number;
  width?: number;
  height?: number;
  tone?: "live" | "in" | "out" | "warn";
  label: string;
}) {
  const segments = sparkSegments(values, width, height, slots ?? values.length);
  return (
    <svg className={`spark spark--${tone}`} width={width} height={height} viewBox={`0 0 ${width} ${height}`} role="img" aria-label={label}>
      <line className="spark__base" x1="0" x2={width} y1={height - 0.5} y2={height - 0.5} />
      {segments.map((points, i) =>
        points.includes(" ") ? (
          <polyline key={i} className="spark__line" points={points} />
        ) : (
          <circle key={i} className="spark__dot" cx={points.split(",")[0]} cy={points.split(",")[1]} r="1.5" />
        ),
      )}
    </svg>
  );
}

/** A horizontal fill. `value` null draws an empty, hatched track: not reported. */
export function Meter({ value, tone = "live", label }: { value: number | null; tone?: "live" | "warn" | "err" | "neutral"; label: string }) {
  const pct = value === null ? 0 : Math.max(0, Math.min(1, value)) * 100;
  return (
    <div
      className={`bar bar--${tone} ${value === null ? "bar--na" : ""}`}
      role="meter"
      aria-label={label}
      aria-valuemin={0}
      aria-valuemax={100}
      aria-valuenow={value === null ? undefined : Math.round(pct)}
    >
      <div className="bar__fill" style={{ width: `${pct}%` }} />
    </div>
  );
}

export function Empty({ title, children, action }: { title: string; children?: React.ReactNode; action?: React.ReactNode }) {
  return (
    <div className="empty">
      <div className="empty__title">{title}</div>
      {children && <div className="empty__body">{children}</div>}
      {action && <div className="empty__action">{action}</div>}
    </div>
  );
}

export function Loading() {
  const { t } = useTranslation();
  return (
    <div className="loading" role="status">
      <span className="loading__dots" aria-hidden><i /><i /><i /></span>
      {t("common.loading")}
    </div>
  );
}

export function ErrorNote({ message, onRetry }: { message: string; onRetry?: () => void }) {
  const { t } = useTranslation();
  return (
    <div className="alert" role="alert">
      <span>{message}</span>
      {onRetry && <button className="btn btn--ghost btn--sm" onClick={onRetry}>{t("common.retry")}</button>}
    </div>
  );
}

/**
 * A modal dialog: scrim, Escape, focus moved in on open and returned on close,
 * Tab kept inside. Enough for the handful of dialogs this UI has.
 */
export function Dialog({
  title, onClose, children, wide, className = "",
}: {
  title: string;
  onClose: () => void;
  children: React.ReactNode;
  wide?: boolean;
  className?: string;
}) {
  const { t } = useTranslation();
  const ref = useRef<HTMLDivElement>(null);
  const titleId = useId();

  useEffect(() => {
    const previous = document.activeElement as HTMLElement | null;
    const box = ref.current;
    const focusables = () =>
      [...(box?.querySelectorAll<HTMLElement>("button, [href], input, select, textarea, [tabindex]:not([tabindex='-1'])") ?? [])]
        .filter((el) => !el.hasAttribute("disabled"));
    (box?.querySelector<HTMLElement>("[data-autofocus]") ?? focusables()[0] ?? box)?.focus();

    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") { e.stopPropagation(); onClose(); return; }
      if (e.key !== "Tab") return;
      const list = focusables();
      if (!list.length) return;
      const first = list[0];
      const last = list[list.length - 1];
      if (e.shiftKey && document.activeElement === first) { e.preventDefault(); last.focus(); }
      else if (!e.shiftKey && document.activeElement === last) { e.preventDefault(); first.focus(); }
    };
    window.addEventListener("keydown", onKey, true);
    return () => {
      window.removeEventListener("keydown", onKey, true);
      previous?.focus?.();
    };
  }, [onClose]);

  return (
    <div className="scrim" onPointerDown={onClose}>
      <div
        ref={ref}
        className={`dialog ${wide ? "dialog--wide" : ""} ${className}`}
        role="dialog"
        aria-modal="true"
        aria-labelledby={titleId}
        tabIndex={-1}
        onPointerDown={(e) => e.stopPropagation()}
      >
        <header className="dialog__head">
          <h2 id={titleId}>{title}</h2>
          <button className="iconbtn" onClick={onClose} aria-label={t("common.close")} title={t("common.close")}>
            <IconX size={15} />
          </button>
        </header>
        {children}
      </div>
    </div>
  );
}

export function Tabs<T extends string>({
  tabs, value, onChange, label,
}: {
  tabs: { id: T; label: string; count?: number | null }[];
  value: T;
  onChange: (id: T) => void;
  label: string;
}) {
  return (
    <div className="tabs" role="tablist" aria-label={label}>
      {tabs.map((tab) => (
        <button
          key={tab.id}
          role="tab"
          aria-selected={value === tab.id}
          tabIndex={value === tab.id ? 0 : -1}
          className={`tab ${value === tab.id ? "tab--on" : ""}`}
          onClick={() => onChange(tab.id)}
          onKeyDown={(e) => {
            if (e.key !== "ArrowRight" && e.key !== "ArrowLeft") return;
            const i = tabs.findIndex((x) => x.id === value);
            const next = tabs[(i + (e.key === "ArrowRight" ? 1 : tabs.length - 1)) % tabs.length];
            onChange(next.id);
            (e.currentTarget.parentElement?.children[tabs.indexOf(next)] as HTMLElement | undefined)?.focus();
          }}
        >
          {tab.label}
          {tab.count !== undefined && tab.count !== null && <span className="tab__count">{tab.count}</span>}
        </button>
      ))}
    </div>
  );
}

/** Expertise type, as glyph + word. The word is the wire value; it is not translated. */
export function TypeBadge({ type, compact }: { type: string; compact?: boolean }) {
  const Glyph = TYPE_GLYPH[type];
  return (
    <span className={`type type--${type}`}>
      {Glyph && <Glyph size={12} />}
      {!compact && type}
    </span>
  );
}

/** An id or hash: shortened on screen, whole on click-to-copy. */
export function Copyable({ value, children }: { value: string; children: React.ReactNode }) {
  const { t } = useTranslation();
  return (
    <button
      className="copyable"
      title={`${value}\n${t("common.clickToCopy")}`}
      onClick={(e) => {
        e.stopPropagation();
        navigator.clipboard?.writeText(value).then(
          () => toast("ok", t("common.copied")),
          () => toast("error", t("common.copyFailed")),
        );
      }}
    >
      {children}
    </button>
  );
}

export function Kbd({ children }: { children: React.ReactNode }) {
  return <kbd className="kbd">{children}</kbd>;
}
