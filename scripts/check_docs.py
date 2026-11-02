#!/usr/bin/env python3
"""Verify that every relative link and anchor in the docs resolves.

GitHub turns a heading into an anchor by lowercasing it, dropping
punctuation and replacing spaces with hyphens. Reproducing that rule is
how this catches a link to a section that was renamed months ago -- a
class of breakage that renders as a perfectly normal-looking page with
one dead link in it.

Usage: run from the repository root.

    python3 scripts/check_docs.py

Exits non-zero if anything is broken, so CI fails instead of letting a
dead link ship.
"""

from __future__ import annotations

import os
import re
import sys

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))

# Files whose links we check: the front page and every guide.
SOURCES = ["README.md"] + sorted(
    os.path.join("docs", f) for f in os.listdir(os.path.join(ROOT, "docs"))
    if f.endswith(".md")
)

FENCE = re.compile(r"^```.*?^```", re.S | re.M)
INLINE_CODE = re.compile(r"`[^`\n]*`")
LINK = re.compile(r"\[[^\]]*\]\(([^)\s]+)\)")
HEADING = re.compile(r"^(#{1,6})\s+(.*)$", re.M)


def slug(text: str) -> str:
    """Approximate GitHub's heading-to-anchor conversion."""
    text = text.strip().lower()
    text = text.replace("`", "")
    text = re.sub(r"[^\w\s-]", "", text)
    return re.sub(r"\s+", "-", text)


def anchors_of(path: str) -> set[str]:
    with open(path, encoding="utf-8") as fh:
        body = fh.read()
    return {slug(m.group(2)) for m in HEADING.finditer(body)}


def strip_code(text: str) -> str:
    """Drop fenced and inline code so sample syntax is not read as a link."""
    text = FENCE.sub("", text)
    return INLINE_CODE.sub("", text)


def main() -> int:
    problems: list[str] = []

    for rel in SOURCES:
        path = os.path.join(ROOT, rel)
        if not os.path.exists(path):
            problems.append(f"{rel}: source file missing")
            continue

        with open(path, encoding="utf-8") as fh:
            text = strip_code(fh.read())

        for raw in LINK.findall(text):
            # A bare "#anchor" points into the same file and must still be
            # checked; only truly external targets are skipped.
            if raw.startswith(("http://", "https://", "mailto:")):
                continue

            target, _, fragment = raw.partition("#")
            if target:
                resolved = os.path.normpath(
                    os.path.join(os.path.dirname(path), target)
                )
            else:
                resolved = path

            if not os.path.exists(resolved):
                problems.append(f"{rel}: missing file for {raw!r}")
                continue

            if fragment and resolved.endswith(".md"):
                have = anchors_of(resolved)
                if fragment not in have:
                    problems.append(
                        f"{rel}: no anchor #{fragment} in {os.path.relpath(resolved, ROOT)}"
                    )

    if problems:
        print(f"check_docs: {len(problems)} problem(s)\n")
        for p in problems:
            print(f"  {p}")
        return 1

    print(f"check_docs: {len(SOURCES)} files, all relative links and anchors resolve")
    return 0


if __name__ == "__main__":
    sys.exit(main())
