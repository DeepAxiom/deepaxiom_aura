/**
 * A right-click menu, driven by a list of items rather than by what it is on.
 *
 * Every editing operation this canvas has already existed behind a keyboard
 * shortcut or a toolbar button; none of them were findable by right-clicking,
 * which is the first thing most people try on a canvas. This component does
 * not know what a node or an edge is — the caller builds the item list — so
 * adding an operation is one line where the operation lives, not a change here.
 */

import { useEffect, useRef } from "react";

export type MenuItem =
  | { kind: "separator" }
  | {
      kind?: "item";
      label: string;
      /** Shown right-aligned: the keyboard route to the same thing. */
      hint?: string;
      danger?: boolean;
      disabled?: boolean;
      onSelect: () => void;
    };

interface Props {
  /** Viewport coordinates — this floats above the canvas, not inside it. */
  x: number;
  y: number;
  items: MenuItem[];
  onClose: () => void;
}

/** Roughly what the menu occupies, used to keep it inside the window. */
const MENU_W = 210;
const ROW_H = 28;

export function ContextMenu({ x, y, items, onClose }: Props) {
  const ref = useRef<HTMLDivElement>(null);

  useEffect(() => {
    // Pointerdown rather than click: a menu that survives until mouseup can
    // eat the click that was meant for whatever is underneath it.
    const away = (e: PointerEvent) => {
      if (!ref.current?.contains(e.target as Node)) onClose();
    };
    const esc = (e: KeyboardEvent) => {
      if (e.key === "Escape") onClose();
    };
    window.addEventListener("pointerdown", away, true);
    window.addEventListener("keydown", esc);
    return () => {
      window.removeEventListener("pointerdown", away, true);
      window.removeEventListener("keydown", esc);
    };
  }, [onClose]);

  // Flip rather than clip. A menu opened near the right or bottom edge would
  // otherwise put its last item — usually Delete — off screen.
  const h = items.length * ROW_H + 8;
  const left = Math.min(x, window.innerWidth - MENU_W - 8);
  const top = Math.min(y, Math.max(8, window.innerHeight - h - 8));

  return (
    <div className="ctxmenu" ref={ref} style={{ left, top }} role="menu">
      {items.map((item, i) =>
        item.kind === "separator" ? (
          <div key={i} className="ctxmenu__sep" />
        ) : (
          <button
            key={i}
            role="menuitem"
            className={`ctxmenu__item ${item.danger ? "ctxmenu__item--danger" : ""}`}
            disabled={item.disabled}
            onClick={() => {
              item.onSelect();
              onClose();
            }}
          >
            <span>{item.label}</span>
            {item.hint && <kbd className="ctxmenu__hint">{item.hint}</kbd>}
          </button>
        ),
      )}
    </div>
  );
}
