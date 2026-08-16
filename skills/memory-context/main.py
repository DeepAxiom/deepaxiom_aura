"""
memory-context — memory.context.window.

Persistent, per-session conversation memory — the piece cognitive.llm.chat
does NOT have: llm-chat keeps history in an in-process dict (see
skills/llm-chat/main.py, `_history`), lost on restart and never trimmed to
a real budget. This skill persists turns to a local SQLite file (no
external services, same "embedded, no Docker" ethos as the kernel's own
state) and hands back a context window trimmed to a configurable token
budget on demand — see skill.yaml's `config` for max_context_tokens,
max_turns, trim_strategy, storage_path, summarize_max_tokens.

Persistence (schema, remember/load/clear, the token-budget split) lives in
`store.py`, which has no `aura` import so it can be tested without the
runtime's heavier dependencies. This module keeps the command parsing, the
handler, and `_summarize` — the one piece that needs `aura.llm.ChatBackend`.

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
"""
import logging
import re

from aura import ChatBackend, Context, Skill

import store

logging.basicConfig(level=logging.INFO)
log = logging.getLogger("memory-context")

skill = Skill()

# storage_path is restart_required (see skill.yaml) — the connection below
# is opened once, at whatever value was effective at process start.
_DB = store.connect(skill.config["storage_path"])

_ROLE_RE = re.compile(r"^(user|assistant|system):\s*(.*)$", re.IGNORECASE | re.DOTALL)
_summarizer: ChatBackend | None = None


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
        max_tokens = skill.config["summarize_max_tokens"]
        return "".join(
            _summarizer_backend().stream_chat(messages, max_tokens=max_tokens)
        ).strip()
    except Exception as exc:  # noqa: BLE001 — best-effort, never fatal
        log.warning("summarize failed, falling back to drop-oldest: %s", exc)
        return ""


def _recall_text(session: str) -> str:
    """Assemble the text 'recall' returns: store.recall() does the token-
    budget split (pure, no LLM); summarize only touches what it already
    decided to drop, and only when trim_strategy asks for it."""
    result = store.recall(_DB, session, skill.config["max_context_tokens"])
    if not result.dropped:
        return result.kept
    if skill.config["trim_strategy"] == "summarize":
        summary = _summarize(result.dropped)
        if summary:
            prefix = f"[summary of {len(result.dropped)} earlier turn(s)]: {summary}"
            return f"{prefix}\n{result.kept}" if result.kept else prefix
    return result.kept  # drop-oldest (default), or summarize fell through on failure


def _parse_remember(rest: str) -> tuple[str, str] | None:
    """Parse the text after 'remember:' into (role, content). Returns None
    when there is no content to store."""
    match = _ROLE_RE.match(rest)
    role, content = (match.group(1).lower(), match.group(2)) if match else ("user", rest)
    if not content.strip():
        return None
    return role, content


@skill.on("event_in")
async def handle(ctx: Context) -> None:
    text = ((ctx.payload or {}).get("text") or "").strip()
    if not text:
        return
    session = ctx.session
    lowered = text.lower()

    if lowered == "recall":
        await ctx.emit("result_out", {"text": _recall_text(session), "final": True})
        return

    if lowered == "clear":
        store.clear(_DB, session)
        await ctx.emit("result_out", {"text": "memory cleared", "final": True})
        return

    if lowered.startswith("remember:"):
        rest = text[len("remember:"):].strip()
        parsed = _parse_remember(rest)
        if parsed is None:
            await ctx.emit("result_out", {
                "text": "remember: needs content, e.g. 'remember: user: hola'",
                "final": True,
            })
            return
        role, content = parsed
        store.remember(_DB, session, role, content, skill.config["max_turns"])
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
