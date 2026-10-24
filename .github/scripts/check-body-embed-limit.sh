#!/usr/bin/env bash
# A comment's, a comment edit's and a message's body is embedded whole, and the embedder cuts a
# text it is handed to `maxInputChars` runes (embed.go, through `text.HeadRunes`) without an
# error. `DISPATCH_BODY_MAX` (dispatch-tools.ts) is the longest body the Dispatch server accepts,
# counted in UTF-16 code units. A text's rune count is never greater than its UTF-16 length, so a
# body of at most `DISPATCH_BODY_MAX` UTF-16 units is at most that many runes and reaches the
# model whole. The comparison is therefore sound in exactly one direction: this check fails iff
# `DISPATCH_BODY_MAX > maxInputChars`. Equal passes. Raise the body limit past the embedder's and
# every comment tail past it would vanish from search by meaning, silently; that is what this
# check stops, and it is why each of the two numbers' sites must name the other.
#
# What makes equality safe is that a comment's and a message's embedded text is the body alone,
# with nothing prepended: the triggers in
# packages/envoy/internal/dispatch/store/migrations/0075_comments_embeddings_trigger.up.sql:6 and
# .../0077_messages_embeddings_trigger.up.sql:10 enqueue `new.body`, and the backfill selects the
# same column (packages/envoy/internal/dispatch/embedqueue/queue.go:724 and :732).
#
# What this check does not cover: a document's text and an ask's question joined to its answer are
# embedded too, are cut at the same limit with the same silence, and are not capped by
# `DISPATCH_BODY_MAX` - dispatch://INFRA-114 owns those. This check claims only the comment,
# comment-edit and message bodies `DISPATCH_BODY_MAX` caps.
#
# The body limit is also committed into generated files (packages/envoy/internal/contracts/
# generated.go, packages/daemon/internal/dispatch/generated.go, packages/claude-envoy/dist/*.js).
# This check reads the two defining sources only and keeps no copy of either number;
# packages/contracts/scripts/gen-go.test.ts and claude-envoy's check-dist own the copies'
# freshness.
#
# Run from anywhere: .github/scripts/check-body-embed-limit.sh
# CI runs it in the lint job of pr-and-main.yaml, and its tests in that workflow's test job.
set -euo pipefail

cd "$(dirname "$0")/../.."

python3 <<'PY'
import re
import sys
from pathlib import Path
from typing import NamedTuple

BODY_FILE = "packages/contracts/src/dispatch-tools.ts"
EMBED_FILE = "packages/envoy/internal/dispatch/embed/embed.go"
CONTRACTS_AGENTS = "packages/contracts/AGENTS.md"
ENVOY_AGENTS = "packages/envoy/AGENTS.md"

BODY_CONST = "DISPATCH_BODY_MAX"
EMBED_CONST = "maxInputChars"

BODY_PATTERN = rf"^\s*export\s+const\s+{BODY_CONST}\s*=\s*(0|[1-9][0-9]*)\s*;\s*(?://.*)?$"
EMBED_PATTERN = rf"^\s*{EMBED_CONST}\s*=\s*(0|[1-9][0-9]*)\s*(?://.*)?$"

problems: list[tuple[str, int | None, str]] = []


def one_line(value: object) -> str:
    """A value quoted into a message cannot carry newlines, or it reaches the Actions log as a
    line of its own."""
    return " ".join(str(value).split())


def problem(path: str, line: int | None, message: str) -> None:
    problems.append((path, line, message))


def load(path: str) -> list[str] | None:
    """The file's lines, or None after recording why it could not be read."""
    try:
        text = Path(path).read_bytes().decode("utf-8")
    except FileNotFoundError:
        problem(path, None, f"{path} is missing: this check cannot prove the limits agree without it")
        return None
    except UnicodeDecodeError as error:
        problem(path, None, f"{path} is not readable as UTF-8 text: {one_line(error)}")
        return None
    return text.splitlines()


class Declaration(NamedTuple):
    value: int
    line: int


def declaration(path: str, lines: list[str], const: str, pattern: str) -> Declaration | None:
    """The declared number and its line number, or None after recording the failure."""
    matches = [
        (number, match)
        for number, line in enumerate(lines, start=1)
        if (match := re.match(pattern, line))
    ]
    if not matches:
        problem(
            path,
            None,
            f"{path} declares no {const} this check can read: no line matches {pattern}. A "
            f"renamed constant, a moved file, or the number written another way (digit "
            f"separators, a leading zero, hexadecimal, an expression) is this failure. Restore a "
            f"plain decimal declaration, or update this check's pattern with the sites that moved",
        )
        return None
    if len(matches) > 1:
        where = ", ".join(f"{path}:{number}" for number, _ in matches)
        problem(
            path,
            matches[0][0],
            f"{path} declares {const} more than once ({where}), so this check cannot tell which "
            f"number is in force and refuses to read the harmless one. Leave exactly one "
            f"declaration",
        )
        return None
    number, match = matches[0]
    return Declaration(int(match.group(1)), number)


def comment_block_above(lines: list[str], line_number: int) -> str:
    """The unbroken run of comment lines directly above the declaration: `//` lines, or a
    `/** ... */` block."""
    block: list[str] = []
    index = line_number - 2
    while index >= 0:
        stripped = lines[index].strip()
        if stripped.startswith("//") or stripped.startswith("*") or stripped.startswith("/*"):
            block.append(stripped)
            if stripped.startswith("/*"):
                break
            index -= 1
            continue
        break
    return "\n".join(block)


def cross_reference(path: str, lines: list[str], line: int, const: str, other: str) -> None:
    if other in comment_block_above(lines, line):
        return
    where = f"the comment above {const} ({path}:{line})"
    problem(
        path,
        line,
        f"{where} does not name {other}. Each limit's site must name the other, so whoever "
        f"moves {const} sees that {other} bounds it: write {other} into {where}",
    )


body_lines = load(BODY_FILE)
embed_lines = load(EMBED_FILE)
contracts_agents_lines = load(CONTRACTS_AGENTS)
envoy_agents_lines = load(ENVOY_AGENTS)

body = declaration(BODY_FILE, body_lines, BODY_CONST, BODY_PATTERN) if body_lines is not None else None
embed = declaration(EMBED_FILE, embed_lines, EMBED_CONST, EMBED_PATTERN) if embed_lines is not None else None

if body is not None and embed is not None and body.value > embed.value:
    problem(
        BODY_FILE,
        body.line,
        f"{BODY_CONST} is {body.value} ({BODY_FILE}:{body.line}), past the embedder's {EMBED_CONST} "
        f"of {embed.value} ({EMBED_FILE}:{embed.line}): every comment, comment edit and message body "
        f"longer than {embed.value} runes is cut before it is embedded, and its tail vanishes from "
        f"search by meaning with no error. Lower {BODY_CONST} back to at most {embed.value}, or "
        f"raise {EMBED_CONST} deliberately - it bounds every text Dispatch embeds and the spend "
        f"tokenRateCeiling paces (packages/envoy/internal/dispatch/embedqueue/queue.go), and "
        f"moving it also moves packages/envoy/internal/dispatch/embedqueue/queue_test.go's "
        f"25 x 8,000 = 200,000 arithmetic and packages/envoy/cmd/dispatch/README.md",
    )

if body is not None:
    cross_reference(BODY_FILE, body_lines, body.line, BODY_CONST, EMBED_CONST)
if embed is not None:
    cross_reference(EMBED_FILE, embed_lines, embed.line, EMBED_CONST, BODY_CONST)

for agents_path, agents_lines in ((CONTRACTS_AGENTS, contracts_agents_lines), (ENVOY_AGENTS, envoy_agents_lines)):
    if agents_lines is None:
        continue
    if any(BODY_CONST in line and EMBED_CONST in line for line in agents_lines):
        continue
    problem(
        agents_path,
        None,
        f"no line of {agents_path} names both {BODY_CONST} and {EMBED_CONST}. Write one line "
        f"there saying that {BODY_CONST} must never exceed {EMBED_CONST}, so a reader raising "
        f"either one finds the other",
    )

for path, line, message in problems:
    location = f"file={path}" + (f",line={line}" if line is not None else "")
    print(f"::error {location}::{message}", file=sys.stderr)
if problems:
    print(
        f"::error::{len(problems)} problem(s): the body limit and the embedder's input limit do "
        f"not demonstrably agree",
        file=sys.stderr,
    )
    raise SystemExit(1)

print(
    f"body limit {body.value} ({BODY_FILE}:{body.line}) fits the embedder's {embed.value} "
    f"({EMBED_FILE}:{embed.line}), and each names the other"
)
PY
