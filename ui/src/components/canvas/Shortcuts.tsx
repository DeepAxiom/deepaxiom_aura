/**
 * The keyboard map, in the app rather than in the docs.
 *
 * Written as data so the panel and the context menus cannot disagree about
 * what a key does: the menus take their `hint` strings from the same table.
 * A help screen that has drifted from the behaviour is worse than none, since
 * it is believed.
 */

import { useEffect } from "react";
import { useTranslation } from "react-i18next";

/** Ctrl on Windows and Linux, Cmd on a Mac. Cosmetic; both are handled. */
const MOD = typeof navigator !== "undefined" && /Mac|iPhone|iPad/.test(navigator.platform ?? "")
  ? "⌘"
  : "Ctrl";

/** id → key label. `id` is an i18n key under `studio.keys`. */
export const SHORTCUTS: { id: string; keys: string }[] = [
  { id: "undo", keys: `${MOD}+Z` },
  { id: "redo", keys: `${MOD}+Shift+Z` },
  { id: "copy", keys: `${MOD}+C` },
  { id: "paste", keys: `${MOD}+V` },
  { id: "duplicate", keys: `${MOD}+D` },
  { id: "selectAll", keys: `${MOD}+A` },
  { id: "copyIR", keys: `${MOD}+Shift+C` },
  { id: "pasteIR", keys: `${MOD}+Shift+V` },
  { id: "delete", keys: "Del" },
  { id: "deselect", keys: "Esc" },
  { id: "boxSelect", keys: "Shift+drag" },
  { id: "pan", keys: "drag" },
  { id: "zoom", keys: "wheel" },
  { id: "quickAdd", keys: "dbl-click" },
  { id: "rename", keys: "dbl-click node" },
  { id: "menu", keys: "right-click" },
  { id: "fit", keys: "F" },
  { id: "tidy", keys: "T" },
  { id: "note", keys: "N" },
  { id: "disable", keys: "D" },
  { id: "frame", keys: "G" },
  { id: "history", keys: "H" },
  { id: "help", keys: "?" },
];

/** The label for one shortcut id, for a menu hint. */
export function keyFor(id: string): string {
  return SHORTCUTS.find((s) => s.id === id)?.keys ?? "";
}

export function Shortcuts({ onClose }: { onClose: () => void }) {
  const { t } = useTranslation();

  useEffect(() => {
    const esc = (e: KeyboardEvent) => { if (e.key === "Escape") onClose(); };
    window.addEventListener("keydown", esc);
    return () => window.removeEventListener("keydown", esc);
  }, [onClose]);

  return (
    <div className="shortcuts__scrim" onPointerDown={onClose}>
      <div className="shortcuts" onPointerDown={(e) => e.stopPropagation()} role="dialog">
        <header className="shortcuts__head">
          <span>{t("studio.shortcuts")}</span>
          <button className="btn btn--ghost btn--sm" onClick={onClose}>{t("common.close")}</button>
        </header>
        <div className="shortcuts__grid">
          {SHORTCUTS.map((s) => (
            <div key={s.id} className="shortcuts__row">
              <kbd>{s.keys}</kbd>
              <span>{t(`studio.keys.${s.id}`)}</span>
            </div>
          ))}
        </div>
      </div>
    </div>
  );
}
