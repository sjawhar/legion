"""Corpus: items (issues and asks) and the chunks each dense model embeds.

An issue is one item; its chunks are its spec split at heading and paragraph boundaries, each
prefixed with the issue title and heading path. An ask is one item and one chunk: its issue's
title, question, options and answer. Ask blocks inside specs are stripped, as production's
document index strips them, because each ask is indexed as its own item.
"""

import json
import os
import re
from dataclasses import dataclass, field

ASK_BLOCK = re.compile(r":::ask\{.*?\n:::", re.S)
HEADING = re.compile(r"^(#{1,6})\s+(.*?)\s*#*\s*$")
FENCE = re.compile(r"^\s*(```|~~~)")

TARGET_CHARS = 1200  # merge paragraphs under one heading up to about 300 tokens
MAX_CHARS = 2400  # split a single paragraph longer than this at line boundaries


@dataclass
class Item:
    id: str  # issue key, or "ask:<uuid>"
    kind: str  # "issue" | "ask"
    issue_key: str | None
    title: str
    created_at: str
    keyword_title: str  # weight-A text for keyword search
    keyword_body: str  # weight-D text for keyword search
    answered_at: str | None = None
    asof_for: str | None = None  # set on a survivor's filing-time copy: the duplicate whose query may see it
    raw: dict = field(default_factory=dict, repr=False)


@dataclass
class Chunk:
    id: str  # "<item id>#<n>"
    item_id: str
    text: str


def strip_ask_blocks(md: str) -> str:
    return ASK_BLOCK.sub("", md)


def _paragraphs(md: str):
    """Yield (heading_path, paragraph_text) in document order."""
    path: list[tuple[int, str]] = []
    buf: list[str] = []
    in_fence = False

    def flush():
        if buf and any(line.strip() for line in buf):
            yield [h for _, h in path], "\n".join(buf).strip()
        buf.clear()

    for line in md.splitlines():
        if FENCE.match(line):
            in_fence = not in_fence
            buf.append(line)
            continue
        if not in_fence:
            m = HEADING.match(line)
            if m:
                yield from flush()
                level = len(m.group(1))
                path[:] = [(lvl, h) for lvl, h in path if lvl < level]
                path.append((level, m.group(2)))
                continue
            if not line.strip():
                yield from flush()
                continue
        buf.append(line)
    yield from flush()


def _split_long(text: str) -> list[str]:
    if len(text) <= MAX_CHARS:
        return [text]
    out, cur = [], ""
    for line in text.splitlines():
        while len(line) > MAX_CHARS:  # a single enormous line: cut at a sentence or space
            cut = max(line.rfind(". ", 0, MAX_CHARS), line.rfind(" ", 0, MAX_CHARS))
            cut = cut + 1 if cut > MAX_CHARS // 2 else MAX_CHARS
            if cur:
                out.append(cur)
                cur = ""
            out.append(line[:cut].strip())
            line = line[cut:]
        if cur and len(cur) + len(line) + 1 > MAX_CHARS:
            out.append(cur)
            cur = ""
        cur = f"{cur}\n{line}" if cur else line
    if cur.strip():
        out.append(cur)
    return out


def chunk_markdown(title: str, md: str) -> list[str]:
    """Split a spec into chunk texts, each prefixed with the title and heading path."""
    groups: list[tuple[list[str], str]] = []
    for path, para in _paragraphs(md):
        for piece in _split_long(para):
            if groups and groups[-1][0] == path and len(groups[-1][1]) + len(piece) + 2 <= TARGET_CHARS:
                groups[-1] = (path, groups[-1][1] + "\n\n" + piece)
            else:
                groups.append((path, piece))
    out = []
    for path, body in groups:
        head = title + ("\n" + " > ".join(path) if path else "")
        out.append(f"{head}\n\n{body}")
    return out or [title]


def ask_text(a: dict) -> str:
    """Question, options and answer: the searchable text of a decision."""
    parts = [a["question"].strip()]
    opts = a.get("options") or []
    if opts:
        parts.append(
            "Options:\n"
            + "\n".join(f"- {o['label']}" + (f": {o['description']}" if o.get("description") else "") for o in opts)
        )
    ans = a.get("answer")
    if ans:
        chosen = ", ".join(ans.get("selected") or [])
        text = (ans.get("text") or "").strip()
        line = "Answer: " + "; ".join(p for p in (chosen, text) if p)
        parts.append(line)
    return "\n\n".join(parts)


def load(data_dir: str) -> tuple[dict[str, Item], list[Chunk]]:
    items: dict[str, Item] = {}
    chunks: list[Chunk] = []
    titles: dict[str, str] = {}
    with open(os.path.join(data_dir, "issues.jsonl")) as f:
        for line in f:
            r = json.loads(line)
            titles[r["key"]] = r["title"]
            spec = strip_ask_blocks(r["spec"] or "")
            it = Item(
                id=r["key"],
                kind="issue",
                issue_key=r["key"],
                title=r["title"],
                created_at=r["created_at"],
                keyword_title=r["title"],
                keyword_body=spec,
                raw=r,
            )
            items[it.id] = it
            for n, text in enumerate(chunk_markdown(r["title"], spec)):
                chunks.append(Chunk(f"{it.id}#{n}", it.id, text))
    with open(os.path.join(data_dir, "asks.jsonl")) as f:
        for line in f:
            a = json.loads(line)
            body = ask_text(a)
            ctx = titles.get(a["issue_key"] or "", "")
            it = Item(
                id=f"ask:{a['id']}",
                kind="ask",
                issue_key=a["issue_key"],
                title=a["question"][:120],
                created_at=a["created_at"],
                # The question is an ask's title: weight A, as an issue's title is, so keyword
                # search does not rank every ask below issues whose titles match.
                keyword_title=a["question"],
                keyword_body=body[len(a["question"].strip()) :],
                answered_at=(a.get("answer") or {}).get("at"),
                raw=a,
            )
            items[it.id] = it
            chunks.append(Chunk(f"{it.id}#0", it.id, (f"{ctx}\n\n" if ctx else "") + body))
    asof_path = os.path.join(data_dir, "asof.jsonl")
    if os.path.exists(asof_path):
        with open(asof_path) as f:
            for line in f:
                r = json.loads(line)
                if not r["survivor_changed"]:
                    continue
                live = items[r["key"]]
                spec = strip_ask_blocks(r["spec"] or "")
                it = Item(
                    id=f"{r['key']}@{r['duplicate']}",
                    kind="issue",
                    issue_key=r["key"],
                    title=r["title"],
                    created_at=live.created_at,
                    keyword_title=r["title"],
                    keyword_body=spec,
                    asof_for=r["duplicate"],
                    raw=r,
                )
                items[it.id] = it
                for n, text in enumerate(chunk_markdown(r["title"], spec)):
                    chunks.append(Chunk(f"{it.id}#{n}", it.id, text))
    return items, chunks


if __name__ == "__main__":
    import statistics
    import sys

    items, chunks = load(sys.argv[1])
    lens = [len(c.text) for c in chunks]
    print(f"{len(items)} items ({sum(i.kind == 'issue' for i in items.values())} issues), {len(chunks)} chunks")
    print(f"chunk chars: total {sum(lens)}, median {statistics.median(lens)}, max {max(lens)}")
