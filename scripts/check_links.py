#!/usr/bin/env python3
"""Fail if any Markdown file links to a path that does not exist.

The docs once described a directory that had been deleted — ~45 dead
references and 11 broken links across the README, the walkthrough and the
contributing guide. This turns that class of drift into a CI failure.

Only relative links are checked: external URLs are not this script's job.
Anchors are ignored beyond the file part (`README.md#section` checks
`README.md`).

    python scripts/check_links.py
"""
from __future__ import annotations

import re
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
SKIP_DIRS = {".git", "node_modules", "dist", ".astro", ".firebase", "__pycache__"}
LINK_RE = re.compile(r"\[[^\]]*\]\(([^)\s]+)\)")


def markdown_files() -> list[Path]:
    out = []
    for path in ROOT.rglob("*.md"):
        if any(part in SKIP_DIRS for part in path.relative_to(ROOT).parts):
            continue
        out.append(path)
    return sorted(out)


def main() -> int:
    broken: list[str] = []
    checked = 0

    for doc in markdown_files():
        text = doc.read_text(encoding="utf-8", errors="replace")
        for match in LINK_RE.finditer(text):
            target = match.group(1)
            if target.startswith(("http://", "https://", "mailto:", "#", "<")):
                continue
            path_part = target.split("#", 1)[0]
            if not path_part:
                continue
            checked += 1
            resolved = (doc.parent / path_part).resolve()
            if not resolved.exists():
                line = text[: match.start()].count("\n") + 1
                broken.append(f"{doc.relative_to(ROOT).as_posix()}:{line} -> {target}")

    if broken:
        print(f"{len(broken)} broken relative link(s):\n")
        for item in broken:
            print(f"  {item}")
        return 1

    print(f"OK: {checked} relative link(s) across {len(markdown_files())} file(s) all resolve.")
    return 0


if __name__ == "__main__":
    sys.exit(main())
