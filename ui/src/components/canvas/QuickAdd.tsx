/**
 * Add a node where you are looking, by typing.
 *
 * The palette on the left is a catalogue you browse; this is the one you use
 * once you know what you want. Double-click the canvas — or an edge, to splice
 * — and the skill lands at that exact point instead of at the centre of the
 * view, which is the difference between placing a node and then dragging it
 * where you meant.
 *
 * Keyboard-first on purpose: it opens focused, arrows move, Enter picks. A
 * dialog that needs the mouse to finish what the keyboard started is slower
 * than the palette it was meant to beat.
 */

import { useEffect, useMemo, useRef, useState } from "react";
import { useTranslation } from "react-i18next";
import type { SkillManifest } from "../../api/types";

interface Props {
  /** Viewport coordinates for the popup itself. */
  x: number;
  y: number;
  skills: SkillManifest[];
  /**
   * When set, only skills that can sit in the middle of a chain are offered:
   * splicing needs something with both an ingress and an egress.
   */
  throughOnly?: boolean;
  title: string;
  onPick: (skill: SkillManifest) => void;
  onClose: () => void;
}

const PANEL_W = 280;
const PANEL_H = 320;

export function QuickAdd({ x, y, skills, throughOnly, title, onPick, onClose }: Props) {
  const { t } = useTranslation();
  const [query, setQuery] = useState("");
  const [cursor, setCursor] = useState(0);
  const inputRef = useRef<HTMLInputElement>(null);
  const listRef = useRef<HTMLDivElement>(null);

  useEffect(() => { inputRef.current?.focus(); }, []);

  const matches = useMemo(() => {
    const q = query.trim().toLowerCase();
    return skills
      .filter((s) => !throughOnly || (s.ports?.ingress?.length && s.ports?.egress?.length))
      .filter((s) => !q || s.name.toLowerCase().includes(q) || s.capability.toLowerCase().includes(q))
      .slice(0, 40);
  }, [skills, query, throughOnly]);

  // A filtered list that leaves the cursor past its end would Enter into
  // nothing, which reads as the dialog ignoring you.
  useEffect(() => { setCursor(0); }, [query]);

  useEffect(() => {
    listRef.current?.querySelector(".quickadd__item--on")?.scrollIntoView({ block: "nearest" });
  }, [cursor]);

  const left = Math.min(x, window.innerWidth - PANEL_W - 8);
  const top = Math.min(y, Math.max(8, window.innerHeight - PANEL_H - 8));

  return (
    <div className="quickadd" style={{ left, top }} role="dialog" aria-label={title}>
      <div className="quickadd__title">{title}</div>
      <input
        ref={inputRef}
        className="input quickadd__search"
        placeholder={t("studio.searchSkills")}
        value={query}
        onChange={(e) => setQuery(e.target.value)}
        onKeyDown={(e) => {
          if (e.key === "Escape") { e.preventDefault(); onClose(); return; }
          if (e.key === "ArrowDown") { e.preventDefault(); setCursor((c) => Math.min(c + 1, matches.length - 1)); return; }
          if (e.key === "ArrowUp") { e.preventDefault(); setCursor((c) => Math.max(c - 1, 0)); return; }
          if (e.key === "Enter" && matches[cursor]) { e.preventDefault(); onPick(matches[cursor]); }
        }}
      />
      <div className="quickadd__list" ref={listRef}>
        {matches.length === 0 && <div className="quickadd__empty">{t("studio.noSkills")}</div>}
        {matches.map((s, i) => (
          <button
            key={s.id}
            className={`quickadd__item ${i === cursor ? "quickadd__item--on" : ""}`}
            onPointerEnter={() => setCursor(i)}
            onClick={() => onPick(s)}
          >
            <span className={`palette__dot palette__dot--${s.type}`} />
            <span className="quickadd__name">{s.name}</span>
            <span className="quickadd__cap">{s.capability}</span>
          </button>
        ))}
      </div>
    </div>
  );
}
