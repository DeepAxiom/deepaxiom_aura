"""
memory-context — memory.context.window.

Persistent, per-session conversation memory — the piece cognitive.llm.chat
does NOT have: llm-chat keeps history in an in-process dict (see
skills/llm-chat/main.py, `_history`), lost on restart and never trimmed to
a real budget. This skill persists turns to a local SQLite file (no
external services, same "embedded, no Docker" ethos as the kernel's own
state) and hands back a context window trimmed to a configurable token
budget on demand — see skill.yaml's `config` for max_context_tokens,
max_turns, trim_strategy, storage_path.

Not wired into llm-chat automatically — this is a standalone skill a graph
connects explicitly, e.g.:

    client.text_out  -> memory.event_in   ("remember: user: " + text)
    memory.result_out -> llm-chat.text_in  (after a "recall")

or reached directly by capability (memory.context.window) — the planner
can call it like any other skill since it takes plain text commands.

Commands on event_in (the std/text@1 "text" field):
  "remember: <role>: <content>"   store a turn. "<role>:" is optional and
                                   defaults to "user" when omitted or not
                                   one of user/assistant/system.
  "recall"                        emit the assembled, trimmed context on
                                   result_out.
  "clear"                         wipe this session's stored memory.

Approximate token counting, on purpose (no tokenizer dependency, same
"honest limitation over silent precision" style as the rest of this repo):
len(text) // 4. Treat max_context_tokens as a soft budget, not an exact cap.
"""
import logging
import os
import re
import sqlite3
import time
from pathlib import Path

from aura import ChatBackend, Context, Skill

logging.basicConfig(level=logging.INFO)
log = logging.getLogger("memory-context")

skill = Skill()

# storage_path is restart_required (see skill.yaml) — the connection below
# is opened once, at whatever value was effective at process start.
_DB_PATH = Path(os.path.expanduser(skill.config["storage_path"]))
_DB_PATH.parent.mkdir(parents=True, exist_ok=True)
_db = sqlite3.connect(str(_DB_PATH), check_same_thread=False)
_db.execute("""
CREATE TABLE IF NOT EXISTS turns (
  session TEXT NOT NULL,
  seq     INTEGER NOT NULL,
  role    TEXT NOT NULL,
  content TEXT NOT NULL,
  ts      INTEGER NOT NULL
)
""")
_db.execute("CREATE INDEX IF NOT EXISTS idx_turns_session ON turns(session, seq)")
_db.commit()

_ROLE_RE = re.compile(r"^(user|assistant|system):\s*(.*)$", re.IGNORECASE | re.DOTALL)
_summarizer: ChatBackend | None = None


def _approx_tokens(text: str) -> int:
    return max(1, len(text) // 4)


def _next_seq(session: str) -> int:
    row = _db.execute(
        "SELECT COALESCE(MAX(seq), -1) FROM turns WHERE session = ?", (session,)
    ).fetchone()
    return row[0] + 1


def _remember(session: str, role: str, content: str) -> None:
    seq = _next_seq(session)
    _db.execute(
        "INSERT INTO turns (session, seq, role, content, ts) VALUES (?,?,?,?,?)",
        (session, seq, role, content, int(time.time())),
    )
    _db.commit()
    # Hard cap regardless of token budget: drop everything older than the
    # newest max_turns entries for this session.
    max_turns = skill.config["max_turns"]
    floor = seq - max_turns + 1
    if floor > 0:
        _db.execute("DELETE FROM turns WHERE session = ? AND seq < ?", (session, floor))
        _db.commit()


def _load(session: str) -> list[tuple[int, str, str]]:
    return _db.execute(
        "SELECT seq, role, content FROM turns WHERE session = ? ORDER BY seq", (session,)
    ).fetchall()


def _summarizer_backend() -> ChatBackend:
    global _summarizer
    if _summarizer is None:
        _summarizer = ChatBackend()
    return _summarizer


def _summarize(turns: list[tuple[int, str, str]]) -> str:
    """Best-effort: compress dropped turns into one dense summary using the
    same local-or-cloud backend as skills/llm-chat. Falls back to an empty
    string (caller then just drops the turns) if generation fails — a
    summarization skill should never be why the whole recall fails."""
    transcript = "\n".join(f"{role}: {content}" for _, role, content in turns)
    messages = [
        {"role": "system", "content": (
            "Summarize the following conversation history in a few dense "
            "sentences. Preserve names, facts, and decisions. Output only "
            "the summary, no preamble."
        )},
        {"role": "user", "content": transcript},
    ]
    try:
        return "".join(_summarizer_backend().stream_chat(messages, max_tokens=256)).strip()
    except Exception as exc:  # noqa: BLE001 — best-effort, never fatal
        log.warning("summarize failed, falling back to drop-oldest: %s", exc)
        return ""


def _recall(session: str) -> str:
    turns = _load(session)
    if not turns:
        return ""
    budget = skill.config["max_context_tokens"]
    strategy = skill.config["trim_strategy"]

    kept: list[tuple[int, str, str]] = []
    used = 0
    for seq, role, content in reversed(turns):  # newest first while budgeting
        cost = _approx_tokens(content)
        if used + cost > budget and kept:
            break
        kept.append((seq, role, content))
        used += cost
    kept.reverse()

    kept_from = kept[0][0] if kept else turns[-1][0] + 1
    dropped = [t for t in turns if t[0] < kept_from]

    body = "\n".join(f"{role}: {content}" for _, role, content in kept)
    if not dropped:
        return body

    if strategy == "summarize":
        summary = _summarize(dropped)
        if summary:
            prefix = f"[summary of {len(dropped)} earlier turn(s)]: {summary}"
            return f"{prefix}\n{body}" if body else prefix
    return body  # drop-oldest (default), or summarize fell through on failure


def _clear(session: str) -> None:
    _db.execute("DELETE FROM turns WHERE session = ?", (session,))
    _db.commit()


@skill.on("event_in")
async def handle(ctx: Context) -> None:
    text = ((ctx.payload or {}).get("text") or "").strip()
    if not text:
        return
    session = ctx.session
    lowered = text.lower()

    if lowered == "recall":
        await ctx.emit("result_out", {"text": _recall(session), "final": True})
        return

    if lowered == "clear":
        _clear(session)
        await ctx.emit("result_out", {"text": "memory cleared", "final": True})
        return

    if lowered.startswith("remember:"):
        rest = text[len("remember:"):].strip()
        match = _ROLE_RE.match(rest)
        role, content = (match.group(1).lower(), match.group(2)) if match else ("user", rest)
        if not content.strip():
            await ctx.emit("result_out", {
                "text": "remember: needs content, e.g. 'remember: user: hola'",
                "final": True,
            })
            return
        _remember(session, role, content)
        await ctx.emit("result_out", {"text": f"remembered ({role})", "final": True})
        return

    await ctx.emit("result_out", {
        "text": (
            f"No entendí {text!r}. Comandos: 'remember: <role>: <content>', "
            "'recall', 'clear'."
        ),
        "final": True,
    })


if __name__ == "__main__":
    skill.run()
