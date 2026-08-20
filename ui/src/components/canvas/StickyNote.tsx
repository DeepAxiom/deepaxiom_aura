/**
 * One sticky note on the canvas.
 *
 * Deliberately behind the nodes and edges in z-order: a note is a label for a
 * region of the graph, and a label that can cover the thing it labels is one
 * an author has to move before they can work. Its own header is the drag
 * handle for the same reason — dragging from the body would fight the text
 * selection you need to actually read it.
 */

import { useEffect, useRef, useState } from "react";
import type { Note } from "../../graph/notes";

interface Props {
  note: Note;
  editable: boolean;
  selected: boolean;
  onSelect: () => void;
  onText: (text: string) => void;
  onMoveStart: (e: React.PointerEvent) => void;
  onResizeStart: (e: React.PointerEvent) => void;
  onMenu: (e: React.MouseEvent) => void;
  placeholder: string;
}

export function StickyNote({
  note, editable, selected, onSelect, onText, onMoveStart, onResizeStart, onMenu, placeholder,
}: Props) {
  const [editing, setEditing] = useState(false);
  const area = useRef<HTMLTextAreaElement>(null);

  useEffect(() => {
    if (editing) area.current?.focus();
  }, [editing]);

  // A note created empty is a note nobody typed in yet, so it opens ready to
  // type. Any other time, editing is something the author asks for.
  useEffect(() => {
    if (editable && note.text === "") setEditing(true);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  return (
    <div
      className={`note note--${note.color} ${selected ? "note--sel" : ""}`}
      style={{ left: note.x, top: note.y, width: note.w, height: note.h }}
      onPointerDown={(e) => { e.stopPropagation(); onSelect(); }}
      onContextMenu={onMenu}
    >
      <div
        className="note__grip"
        onPointerDown={(e) => { if (editable) { e.stopPropagation(); onMoveStart(e); } }}
      />
      {editing && editable ? (
        <textarea
          ref={area}
          className="note__text note__text--editing"
          value={note.text}
          placeholder={placeholder}
          onChange={(e) => onText(e.target.value)}
          onBlur={() => setEditing(false)}
          // Escape leaves the note rather than the canvas: while a textarea has
          // focus it is the innermost thing Escape should dismiss.
          onKeyDown={(e) => { if (e.key === "Escape") { e.stopPropagation(); setEditing(false); } }}
        />
      ) : (
        <div
          className="note__text"
          onDoubleClick={() => editable && setEditing(true)}
        >
          {note.text || <span className="note__placeholder">{placeholder}</span>}
        </div>
      )}
      {editable && (
        <div
          className="note__resize"
          onPointerDown={(e) => { e.stopPropagation(); onResizeStart(e); }}
        />
      )}
    </div>
  );
}
