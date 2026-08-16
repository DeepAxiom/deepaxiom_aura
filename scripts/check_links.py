#!/usr/bin/env python3
"""Fail if any Markdown link points at a path — or an anchor — that does not exist.

The docs once described a directory that had been deleted — ~45 dead
references and 11 broken links across the README, the walkthrough and the
contributing guide. This turns that class of drift into a CI failure.

Two checks run:

1. **Paths.** Every relative link resolves to a file that exists. External
   URLs are not this script's job.
2. **Anchors.** Every `#fragment` — whether same-file (`#security-model`) or
   cross-file (`spec/c4-ledger.md#checkpoints`) — names a heading that is
   actually in the target document.

Check 2 exists because check 1 alone let a real bug through: the README's
table of contents pointed at `#the-three-contracts` while the heading had
been renamed to "The four contracts". The file part resolved, so CI passed,
and the link was dead for every reader.

Anchors are derived the way GitHub derives them (lowercase, strip anything
that is not a letter/number/space/hyphen, spaces to hyphens), plus any
explicit `<a id="...">` the docs declare by hand.

    python scripts/check_links.py
"""
from __future__ import annotations

import re
import sys
import unicodedata
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
SKIP_DIRS = {".git", "node_modules", "dist", ".astro", ".firebase", "__pycache__"}
LINK_RE = re.compile(r"\[[^\]]*\]\(([^)\s]+)\)")
HEADING_RE = re.compile(r"^(#{1,6})\s+(.*?)\s*$", re.MULTILINE)
EXPLICIT_ID_RE = re.compile(r"""<a\s+id=["']([^"']+)["']""")
FENCE_RE = re.compile(r"^\s*(```|~~~)")


def markdown_files() -> list[Path]:
    out = []
    for path in ROOT.rglob("*.md"):
        if any(part in SKIP_DIRS for part in path.relative_to(ROOT).parts):
            continue
        out.append(path)
    return sorted(out)


def strip_code_fences(text: str) -> str:
    """Blank out fenced code blocks so a `# comment` inside one is not a heading."""
    out, in_fence = [], False
    for line in text.splitlines():
        if FENCE_RE.match(line):
            in_fence = not in_fence
            out.append("")
            continue
        out.append("" if in_fence else line)
    return "\n".join(out)


def slugify(heading: str) -> str:
    """GitHub's heading -> anchor rule, closely enough for our docs.

    Inline markdown is stripped first (`**bold**`, `` `code` ``, links), then
    everything that is not a letter, number, space or hyphen is dropped, and
    spaces become hyphens. Unicode letters are kept — the Spanish README has
    headings like "Los cuatro contratos" and "Configuración".
    """
    text = heading
    text = re.sub(r"\[([^\]]*)\]\([^)]*\)", r"\1", text)  # links -> their text
    text = re.sub(r"<[^>]+>", "", text)                    # inline HTML
    text = text.replace("`", "").replace("*", "").replace("_", "")
    text = text.strip().lower()
    kept = []
    for ch in text:
        if ch.isalnum() or ch in " -":
            kept.append(ch)
        elif unicodedata.category(ch).startswith("M"):
            kept.append(ch)
    return "".join(kept).strip().replace(" ", "-")


def anchors_of(path: Path) -> set[str]:
    """Every fragment a link may legally target in this document."""
    raw = path.read_text(encoding="utf-8", errors="replace")
    text = strip_code_fences(raw)
    found: set[str] = set()
    seen: dict[str, int] = {}
    for _, heading in HEADING_RE.findall(text):
        slug = slugify(heading)
        if not slug:
            continue
        # GitHub disambiguates repeats with -1, -2, ... — mirror that so a
        # doc with two identical headings still validates.
        n = seen.get(slug, 0)
        found.add(slug if n == 0 else f"{slug}-{n}")
        seen[slug] = n + 1
    found.update(EXPLICIT_ID_RE.findall(raw))
    return found


def main() -> int:
    broken_paths: list[str] = []
    broken_anchors: list[str] = []
    checked_paths = 0
    checked_anchors = 0
    anchor_cache: dict[Path, set[str]] = {}

    for doc in markdown_files():
        text = doc.read_text(encoding="utf-8", errors="replace")
        for match in LINK_RE.finditer(text):
            target = match.group(1)
            if target.startswith(("http://", "https://", "mailto:", "<")):
                continue
            line = text[: match.start()].count("\n") + 1
            where = f"{doc.relative_to(ROOT).as_posix()}:{line}"

            path_part, _, fragment = target.partition("#")

            if path_part:
                checked_paths += 1
                resolved = (doc.parent / path_part).resolve()
                if not resolved.exists():
                    broken_paths.append(f"{where} -> {target}")
                    continue
            else:
                resolved = doc  # same-document anchor

            if not fragment or resolved.is_dir() or resolved.suffix != ".md":
                continue

            checked_anchors += 1
            if resolved not in anchor_cache:
                anchor_cache[resolved] = anchors_of(resolved)
            if fragment not in anchor_cache[resolved]:
                broken_anchors.append(f"{where} -> {target}  (no such heading)")

    if broken_paths or broken_anchors:
        if broken_paths:
            print(f"{len(broken_paths)} broken relative link(s):\n")
            for item in broken_paths:
                print(f"  {item}")
            print()
        if broken_anchors:
            print(f"{len(broken_anchors)} broken anchor(s):\n")
            for item in broken_anchors:
                print(f"  {item}")
        return 1

    print(
        f"OK: {checked_paths} relative link(s) and {checked_anchors} anchor(s) "
        f"across {len(markdown_files())} file(s) all resolve."
    )
    return 0


if __name__ == "__main__":
    sys.exit(main())
