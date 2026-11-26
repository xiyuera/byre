#!/usr/bin/env python3
"""Check that every in-site link points at something that exists.

`mkdocs build --strict` already fails on a link to a missing *file*. It says nothing
about a link to a missing *fragment*: `operations.md#rotation` builds green even when
`operations.md` has no `#rotation`, and the reader lands at the top of the page
wondering what they clicked. A fragment-only link (`#anchor`) is the same failure
inside one page, so it is checked too.

Usage:
    python3 scripts/check-doc-links.py [--root docs] [--verbose]

Exit codes: 0 clean, 1 broken fragments found, 2 usage/setup error.
"""

from __future__ import annotations

import argparse
import re
import sys
import unicodedata
from pathlib import Path

FENCE = re.compile(r"^\s*(```|~~~)")
HEADING = re.compile(r"^(#{1,6})\s+(.*)$")
# [text](target) and [text](target "title"); also <a href="target">
MD_LINK = re.compile(r"\[[^\]]*\]\(\s*([^)\s]+)(?:\s+[\"'][^)]*[\"'])?\s*\)")
HTML_LINK = re.compile(r"""<a\s[^>]*href\s*=\s*["']([^"']+)["']""", re.IGNORECASE)
# An explicit id on a heading: `## Title {#custom-id}` (python-markdown attr_list)
ATTR_ID = re.compile(r"\{[^}]*#([\w-]+)[^}]*\}")
# A raw HTML anchor: <a id="x"></a> / <a name="x"></a>
HTML_ANCHOR = re.compile(r"""<a\s[^>]*(?:id|name)\s*=\s*["']([^"']+)["']""", re.IGNORECASE)


def slug(text: str) -> str:
    """python-markdown's toc slug: strip markup, lowercase, drop punctuation, spaces -> '-'.

    CJK survives (it is not punctuation), which matters here because the site ships a
    Chinese edition. Whitespace runs collapse to one hyphen; leading/trailing hyphens go.
    """
    text = re.sub(r"`([^`]*)`", r"\1", text)          # inline code -> its text
    text = re.sub(r"\[([^\]]*)\]\([^)]*\)", r"\1", text)  # links -> their text
    text = re.sub(r"[*_]{1,3}([^*_]+)[*_]{1,3}", r"\1", text)
    text = unicodedata.normalize("NFKC", text).lower()
    text = re.sub(r"[^\w\s\u4e00-\u9fff-]", "", text)
    text = re.sub(r"\s+", "-", text.strip())
    return text.strip("-")


def anchors_of(text: str) -> set[str]:
    """Every fragment a reader can land on in this file."""
    out: set[str] = set()
    seen: dict[str, int] = {}
    in_fence = False
    for line in text.splitlines():
        if FENCE.match(line):
            in_fence = not in_fence
            continue
        if in_fence:
            continue
        m = HEADING.match(line)
        if m:
            raw = m.group(2).strip()
            explicit = ATTR_ID.search(raw)
            if explicit:
                out.add(explicit.group(1))
                continue
            base = slug(raw)
            # python-markdown de-duplicates repeated headings with _1, _2, ...
            n = seen.get(base, 0)
            seen[base] = n + 1
            out.add(base if n == 0 else f"{base}_{n}")
        for m2 in HTML_ANCHOR.finditer(line):
            out.add(m2.group(1))
    return out


def links_of(text: str) -> list[str]:
    out: list[str] = []
    in_fence = False
    for line in text.splitlines():
        if FENCE.match(line):
            in_fence = not in_fence
            continue
        if in_fence:
            continue
        # A fenced block is skipped on purpose: shell comments and YAML ids look like
        # markdown links to a naive regex, and they never render as links.
        out.extend(m.group(1) for m in MD_LINK.finditer(line))
        out.extend(m.group(1) for m in HTML_LINK.finditer(line))
    return out


def is_external(target: str) -> bool:
    return bool(re.match(r"^(https?:|mailto:|tel:|ftp:|//)", target))


def is_asset(target: str) -> bool:
    return bool(re.search(r"\.(png|jpe?g|gif|svg|webp|ico|pdf|zip|tar|gz|txt|json|ya?ml)$", target, re.I))


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--root", default="docs", help="docs directory (default: docs)")
    ap.add_argument("--verbose", action="store_true", help="list every fragment checked")
    args = ap.parse_args()

    root = Path(args.root)
    if not root.is_dir():
        print(f"error: {root} is not a directory", file=sys.stderr)
        return 2

    files = sorted(root.rglob("*.md"))
    if not files:
        print(f"error: no markdown under {root}", file=sys.stderr)
        return 2

    # Key by resolved absolute path so a link written from anywhere matches the same file.
    root_abs = root.resolve()
    texts = {p.resolve(): p.read_text(encoding="utf-8", errors="replace") for p in files}
    anchors = {p: anchors_of(t) for p, t in texts.items()}

    broken: list[tuple[Path, str, Path]] = []
    checked = 0

    for path in files:
        for target in links_of(texts[path.resolve()]):
            if is_external(target) or is_asset(target):
                continue
            if "#" not in target:
                continue  # mkdocs --strict already fails a missing file
            where, _, frag = target.partition("#")
            if not frag:
                continue  # "/page/#" -- the page itself
            if where:
                dest = (path.parent / where).resolve()
                if dest.is_dir():
                    dest = dest / "index.md"
                if not dest.exists() or dest not in anchors:
                    continue  # outside docs_dir, or a file mkdocs will report itself
            else:
                dest = path.resolve()
            checked += 1
            if frag.lower() not in {a.lower() for a in anchors[dest]}:
                broken.append(
                    (path.resolve().relative_to(root_abs), target, dest.relative_to(root_abs))
                )

    if args.verbose:
        print(f"checked {checked} in-site fragment link(s) across {len(files)} file(s)")

    if broken:
        print(f"\n{len(broken)} link(s) point at a fragment that does not exist:\n")
        for src, target, dest in broken:
            print(f"  {src}\n    -> {target}   (no such anchor in {dest})")
        print(
            "\nA reader who follows one of these lands at the top of the page. mkdocs --strict\n"
            "does not catch this: it validates files, not fragments."
        )
        return 1

    print(f"ok: {checked} in-site fragment link(s) resolve across {len(files)} file(s)")
    return 0


if __name__ == "__main__":
    sys.exit(main())
