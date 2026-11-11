"""
memory-context — SQLite persistence.

Everything here is plain data access: schema, inserts, the hard `max_turns`
cap, and the token-budget split that "recall" needs. No `aura` import, no
`ChatBackend` — this module only needs `sqlite3` (stdlib), so it can be
imported and unit-tested without the rest of the skill's runtime
dependencies (`websockets`, `llama-cpp-python`, ...).

`recall()` deliberately stops at "here is what fits, here is what got
dropped" — it does not summarize. Compressing the dropped turns into text
needs `aura.llm.ChatBackend`, which lives in `main.py` (`_summarize`) so this
module stays free of it.
"""
import logging
import sqlite3
import time
from dataclasses import dataclass
from pathlib import Path

log = logging.getLogger("memory-context.store")

_SCHEMA = """
CREATE TABLE IF NOT EXISTS turns (
  session TEXT NOT NULL,
  seq     INTEGER NOT NULL,
  role    TEXT NOT NULL,
  content TEXT NOT NULL,
  ts      INTEGER NOT NULL
)
"""


def connect(path: str) -> sqlite3.Connection:
    """Open (creating if needed) the SQLite file at `path`, schema applied.

    `check_same_thread=False`: the caller (main.py) opens this once at
    import time on the connecting thread, but handlers run as asyncio tasks
    that may hop threads via `asyncio.to_thread` elsewhere in the skill
    family — same rationale the original single-file version used.
    """
    db_path = Path(path).expanduser()
    db_path.parent.mkdir(parents=True, exist_ok=True)
    db = sqlite3.connect(str(db_path), check_same_thread=False)
    db.execute(_SCHEMA)
    db.execute("CREATE INDEX IF NOT EXISTS idx_turns_session ON turns(session, seq)")
    db.commit()
    return db


def approx_tokens(text: str) -> int:
    """Approximate token counting, on purpose (no tokenizer dependency, same
    "honest limitation over silent precision" style as the rest of this
    repo): len(text) // 4. Treat budgets built on this as soft, not exact."""
    return max(1, len(text) // 4)


def next_seq(db: sqlite3.Connection, session: str) -> int:
    row = db.execute(
        "SELECT COALESCE(MAX(seq), -1) FROM turns WHERE session = ?", (session,)
    ).fetchone()
    return row[0] + 1


def remember(db: sqlite3.Connection, session: str, role: str, content: str, max_turns: int) -> None:
    """Insert one turn, then drop anything older than the newest `max_turns`
    entries for this session — a hard cap independent of the token budget
    `recall()` applies later."""
    seq = next_seq(db, session)
    db.execute(
        "INSERT INTO turns (session, seq, role, content, ts) VALUES (?,?,?,?,?)",
        (session, seq, role, content, int(time.time())),
    )
    db.commit()
    floor = seq - max_turns + 1
    if floor > 0:
        db.execute("DELETE FROM turns WHERE session = ? AND seq < ?", (session, floor))
        db.commit()


def load(db: sqlite3.Connection, session: str) -> list[tuple[int, str, str]]:
    return db.execute(
        "SELECT seq, role, content FROM turns WHERE session = ? ORDER BY seq", (session,)
    ).fetchall()


def clear(db: sqlite3.Connection, session: str) -> None:
    db.execute("DELETE FROM turns WHERE session = ?", (session,))
    db.commit()


@dataclass
class Recall:
    """Result of budgeting a session's stored turns to a token window.

    `kept` is the assembled "role: content" text that fits. `dropped` is
    whatever didn't, oldest-first, for the caller to either discard
    (drop-oldest) or summarize."""
    kept: str
    dropped: list[tuple[int, str, str]]


def recall(db: sqlite3.Connection, session: str, budget: int) -> Recall:
    turns = load(db, session)
    if not turns:
        return Recall(kept="", dropped=[])

    kept: list[tuple[int, str, str]] = []
    used = 0
    for seq, role, content in reversed(turns):  # newest first while budgeting
        cost = approx_tokens(content)
        if used + cost > budget and kept:
            break
        kept.append((seq, role, content))
        used += cost
    kept.reverse()

    kept_from = kept[0][0] if kept else turns[-1][0] + 1
    dropped = [t for t in turns if t[0] < kept_from]
    body = "\n".join(f"{role}: {content}" for _, role, content in kept)
    return Recall(kept=body, dropped=dropped)
