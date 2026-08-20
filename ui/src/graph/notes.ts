/**
 * Sticky notes: what an author wants to say about a graph, next to the graph.
 *
 * The one thing every workflow tool grows and every serialisation format
 * regrets. C2 has no field for them and must not gain one — a note is a fact
 * about a person's understanding, not about what the runtime does, and two
 * graphs that differ only in their annotations have to stay byte-identical on
 * the wire or `aura verify` starts comparing prose.
 *
 * So notes live where positions live: in localStorage, keyed by graph id, on
 * exactly the same terms. The cost is the same too, and it is the right cost —
 * annotations do not travel between machines. Anything that must travel with
 * the graph belongs in a node's `resolve` or in the docs, not on a sticky.
 *
 * Everything here is a pure function over a note list. Reading and writing the
 * store are the only two that touch the browser, and both swallow failure: a
 * disabled or full localStorage should cost you your notes, never your canvas.
 */

const KEY = "aura.canvas.notes";

/** Wide enough for a sentence, short enough not to hide the graph behind it. */
export const NOTE_W = 200;
export const NOTE_H = 120;
export const NOTE_MIN = 80;

/** The palette. Names, not hex, so the CSS owns what they actually look like. */
export const NOTE_COLORS = ["amber", "green", "blue", "pink", "slate"] as const;
export type NoteColor = (typeof NOTE_COLORS)[number];

export interface Note {
  id: string;
  x: number;
  y: number;
  w: number;
  h: number;
  text: string;
  color: NoteColor;
}

type NoteBook = Record<string, Note[]>;

function readBook(): NoteBook {
  try {
    return JSON.parse(localStorage.getItem(KEY) ?? "{}") as NoteBook;
  } catch {
    return {};
  }
}

export function loadNotes(graphId: string): Note[] {
  if (!graphId) return [];
  const found = readBook()[graphId];
  return Array.isArray(found) ? found.filter(isNote) : [];
}

export function saveNotes(graphId: string, notes: Note[]): void {
  if (!graphId) return;
  try {
    const book = readBook();
    // Drop the key entirely rather than storing an empty array, so clearing
    // every note leaves no trace to migrate later.
    if (notes.length === 0) delete book[graphId];
    else book[graphId] = notes;
    localStorage.setItem(KEY, JSON.stringify(book));
  } catch {
    /* notes are a convenience; losing them must not break the canvas */
  }
}

/**
 * Guard what comes back out of storage.
 *
 * localStorage is shared with every other version of this app the browser has
 * ever loaded, so a note written by an older build is a real possibility and a
 * missing `w` would render a zero-sized box the author cannot click to fix.
 */
function isNote(v: unknown): v is Note {
  const n = v as Note;
  return (
    !!n && typeof n.id === "string" &&
    typeof n.x === "number" && typeof n.y === "number" &&
    typeof n.w === "number" && typeof n.h === "number" &&
    typeof n.text === "string" &&
    (NOTE_COLORS as readonly string[]).includes(n.color)
  );
}

/* ── operations ──────────────────────────────────────────────────── */

export function addNote(notes: Note[], at: { x: number; y: number }): { notes: Note[]; id: string } {
  const id = `n${Date.now().toString(36)}-${notes.length}`;
  const note: Note = {
    id,
    // Centred on the point the author asked for, which is where their pointer
    // is; anchoring the corner there puts the note down and to the right of
    // the place they were looking at.
    x: Math.round(at.x - NOTE_W / 2),
    y: Math.round(at.y - NOTE_H / 2),
    w: NOTE_W,
    h: NOTE_H,
    text: "",
    color: "amber",
  };
  return { notes: [...notes, note], id };
}

export function patchNote(notes: Note[], id: string, patch: Partial<Note>): Note[] {
  return notes.map((n) => (n.id === id ? { ...n, ...patch, id: n.id } : n));
}

export function removeNote(notes: Note[], id: string): Note[] {
  return notes.filter((n) => n.id !== id);
}

export function moveNote(notes: Note[], id: string, dx: number, dy: number): Note[] {
  return notes.map((n) => (n.id === id ? { ...n, x: n.x + dx, y: n.y + dy } : n));
}

/** Resize from the bottom-right corner, floored so a note stays grabbable. */
export function resizeNote(notes: Note[], id: string, dw: number, dh: number): Note[] {
  return notes.map((n) =>
    n.id === id
      ? { ...n, w: Math.max(NOTE_MIN, n.w + dw), h: Math.max(NOTE_MIN, n.h + dh) }
      : n,
  );
}

/** Cycle to the next colour. One control instead of a picker nobody opens. */
export function cycleColor(notes: Note[], id: string): Note[] {
  return notes.map((n) => {
    if (n.id !== id) return n;
    const i = NOTE_COLORS.indexOf(n.color);
    return { ...n, color: NOTE_COLORS[(i + 1) % NOTE_COLORS.length] };
  });
}
