#!/usr/bin/env bash
# A prescribed check command must reach this repository's own pinned tool, not whatever the name
# happens to answer. `bunx biome check .` with no `node_modules` installs a squatted npm package
# (`biome` 0.3.3, unrelated to `@biomejs/biome`) that checks nothing and exits 0; a bare `tsc` in a
# recipe is answered by the worker image's own `/usr/local/bin/tsc`, a compiler this repository
# does not pin; `bunx playwright` fetches whatever the registry serves rather than the
# `@playwright/test` the lockfile holds. What this gate is for is that shape exactly: a prescribed
# command that silently runs the wrong tool and passes, which is worse than one that fails.
#
# `scripts/repo-tool.sh <tool>` resolves `./node_modules/.bin/<tool>` then the root's, never PATH
# and never the registry, so it either runs the version the lockfile installed or names the
# missing tool and exits 1. The rule here is therefore "a prescribed command invokes a pinned
# check tool through `scripts/repo-tool.sh`", not "no `bunx`": the pinned check tools are `biome`,
# `tsc`, `tsserver` and `playwright`, and the gate judges commands, never prose.
#
# A package runner is the vulnerable spelling, whatever it is called: `bunx`, `pnpx`, `npx` and
# the two-word `bun x`, `npm exec`, `pnpm exec`, `pnpm dlx`, `yarn exec` and `yarn dlx` all hand
# a bare name to a resolver that reaches the registry. Measured on the worker image with no
# `node_modules`, `bun x biome check .` and `npx biome check .` each exit 0 having checked
# nothing, and `--version` answers `0.3.3` to both — the squat, not the Biome the lockfile pins;
# `bun x` is `bunx`'s own spelling (one binary, one resolution) and the image ships
# `/usr/local/bin/npx` beside bun. `pnpm` and `yarn` are on the list although no image here
# installs them today, because the rule is about what a reader may be told to run, not about
# which runner happens to be on this month's PATH: a guide outlives an image, and a runner added
# to one later must not open the hole again.
#
# `vite` and `astro` are deliberately not on that list: they are build recipes rather than the
# checks this gate is about, no binary on the image shadows them, and a bare one in a `bun run`
# recipe is already fail-closed, since Bun puts `node_modules/.bin` on the recipe's PATH and
# nothing else answers the name. A `bunx vite` is still caught, by the package-runner half of
# rule 1.
#
# One tokenizer serves every rule, so a command is read the same way wherever it is written: a
# command string loses its comment — a `#` that begins a word and stands outside quotes ends the
# command, as the shell reads it — and what is left is split into segments on `&&`, `||`, `;`
# and `|`; each segment's leading brackets, braces, quotes, a prompt's `$ ` and any `FOO=bar`
# assignments are stepped over; and the words that remain are the command the shell would run.
# Quotes are stepped over rather than obeyed, so a command nested in
# `bash -c '… && bunx playwright …'` — how `docs/site/media/broker/screenshots.sh` used to call
# Playwright — is judged like any other. The comment is where the trailing exemption marker is
# read from, and the only place it is read from, so `… # check-check-commands: allow <reason>`
# exempts the line it ends while a marker quoted inside a string —
# `echo "# check-check-commands: allow x" && bunx biome check .` — is text and exempts nothing.
# The comment is taken before the split, so a `;` inside a comment starts no segment of its own.
#
# A segment is judged by the tool it reaches, never by the word it starts with. Judging the
# first word is what failed twice: `exec bunx biome`, `env FOO=1 bunx biome`, `command bunx
# biome`, `nice -n 5 bunx biome` and `bun --bun x biome` each run the squat and each passed a
# gate that read only the word in front, and the prefix set only looks closed — `sudo`,
# `timeout 60`, `xargs`, `sh -c '…'` and a variable holding the runner are the same shape again.
# So the rule is inverted: in a judged context, a segment that reaches `biome`, `tsc`, `tsserver`
# or `playwright` is a problem unless it reaches that tool the one approved way. Every runner,
# every prefix and every option is irrelevant by construction.
#
# The tool is the first word whose whole basename is one of the four names — whole-word and
# case-sensitive, nothing by prefix or by substring — so the rule is about the command a segment
# runs and not the text it contains. `tsconfig.check.json`, `biome.json` and
# `playwright.config.ts` are identifiers that merely contain a tool's name, a capitalised `Biome`
# is prose, and a name written in a comment is gone before the words are read, which is what
# lets `bun run typecheck   # tsc against tsconfig.check.json` stand in a guide. The two
# approved ways to reach the tool are the wrapper, the word immediately before the tool ending
# in `scripts/repo-tool.sh` however it is spelled — so a workflow's
# `run: ../../scripts/repo-tool.sh biome check src/`, whose segment starts with neither — and
# the pinned binary itself, a word ending `node_modules/.bin/<tool>`. Anything else is reported,
# whatever stands in front of it, and the report says what the gate knows rather than more: the
# segment names the tool where this gate reads a command, and reaches it neither approved way.
# Whether the line runs the tool is the author's to say — `which biome`, `grep -rn biome
# package.json` and `rm -rf ~/.cache/biome` each name it without running it, and are reported
# like any other — so the message names the exemption marker as the way to record that, as much
# as to record a line that genuinely cannot be written through the wrapper.
#
# A package manager's own arguments are not commands the shell resolves, but which words those
# are depends on the subcommand: `bun`, `npm`, `pnpm` and `yarn` each take one first, read as
# the first word after the manager that is not an option, so `bun --bun x biome` is the `bun x`
# it spells. There are four cases, and the segment is judged by the one its subcommand falls in:
#
#   * a runner subcommand — `x` for bun, `exec` for npm, `pnpm` and `yarn`, `dlx` for pnpm and
#     yarn — hands the name after it to a resolver that reaches the registry, so the segment is
#     reported as the package runner it is, whatever it names;
#   * a subcommand that is itself a pinned tool's basename is that tool being run —
#     `yarn biome check .` runs `biome` — so the tool rule judges it, which still approves
#     `yarn ./node_modules/.bin/biome`;
#   * an ordinary subcommand's following words are its own arguments and no command of theirs:
#     bun's `run`, `test`, `install`, `add`, `build`, `init`, `link`, `update`, `remove`, `pm`,
#     `publish`, `create` and `outdated`; npm's `view`, `install`, `ci`, `run`, `publish`,
#     `pack`, `version`, `test`, `audit`, `why` and `info`; and for `pnpm` and `yarn` the same
#     words, less the three only another manager has (`pm` is bun's, `ci` is npm's, and bun's
#     `build` is a subcommand where theirs would be a recipe name). The set is named here, not
#     guessed: these are the subcommands this repository's own prescribed commands use, so
#     `npm view biome version` names a package and `bun run lint` names a recipe, which reaches
#     `node_modules/.bin` rather than the registry;
#   * anything else is refused, not swallowed: a subcommand this gate does not know, in a
#     segment that names a pinned tool, is reported as a command it cannot read for certain,
#     naming the subcommand and the exemption marker. This case is the point of the branch.
#     While it swallowed every segment whose subcommand was not the one runner spelling tabled,
#     `pnpm exec biome check .` and `yarn biome check .` both passed it — and a gate that waves
#     an unfamiliar shape through hands back the silent green this file exists to stop. A
#     segment that names no pinned tool is not this gate's business, so an unknown subcommand
#     there, `bun frobnicate --help`, still passes.
#
# The package-runner half of rule 1 survives the inversion because it catches what the tool rule
# cannot: a runner fetches whatever the registry serves for any name, and `bunx vite build`
# names no pinned tool. A segment whose first word's basename is `bunx`, `pnpx` or `npx` — so
# `/usr/local/bin/npx` and `./node_modules/.bin/bunx` as much as a bare `bunx` — or whose
# package manager carries its runner subcommand, is a problem whatever it names. A segment is
# reported at most once, so such a line gives the runner's message and not the tool's as well.
#
# What the tokenizer cannot read, it refuses. A first word carrying an unexpanded shell expansion
# (`$name`, `${…}`, `$(…)` or a backtick substitution) may be the wrapper and may be `bunx`, and
# a gate that waves through the shapes it cannot read hands back exactly the silent green this
# gate exists to stop. So a segment that reaches a pinned tool outside the approved spellings and
# begins with such a word is reported as unreadable rather than as a bare tool — spell the
# command out through the wrapper, or say in the marker why it is safe. A segment that reaches no
# pinned tool is never accused, so an ordinary `"$ROOT/scripts/release.sh" --dry-run` line passes.
#
# The gate fails, in code order:
#
#   1. a `scripts` value in any `package.json` (parsed as JSON, never grepped) whose segments
#      reach a pinned check tool outside the two approved spellings, call a package runner at
#      all, or cannot be read for certain, reported at the line the recipe's own key sits on,
#   2. a `run:` in any `.github/workflows/*.yml` or `*.yaml` (parsed as YAML with a line-recording
#      loader) doing the same, each line of a multi-line `run:` judged on its own and reported at
#      its own line where the loader's span gives it, otherwise at the step's,
#   3. a command line in any `*.sh` outside `.github/scripts/`, or inside a fenced code block of
#      the prescriptive markdown set — every `AGENTS.md` and every `README.md` at any depth,
#      `skills/**/*.md`, `packages/claude-envoy/legion/skills/**/*.md`, `docs/solutions/**/*.md`,
#      `docs/kubernetes.md`, `docs/deployment.md` — doing the same. Every guide at any depth,
#      because a command prescribed in `packages/envoy/infra/AGENTS.md` binds its reader exactly
#      as the root guide's does, and a set written depth by depth left fifteen of them unjudged.
#      Prose and inline code are never judged: a note may name `bunx biome` in a sentence or in
#      backticks, because only a line inside a fenced block is a command someone runs. Which
#      fenced blocks those are is the first word of the info string: no info string at all, a
#      shell (`sh`, `shell`, `bash`, `zsh`, `ksh`), a command session (`console`, `terminal`,
#      `shell-session`) or `yaml`, because a workflow's `run:` step is a prescription wherever
#      it is quoted. A block labelled as data is not judged — `text`, `json`, `diff`, `ts`,
#      `go`, `md`, and a language this gate does not know with them — because a transcript
#      quoting `error: Cannot find module 'tsc'` is output rather than an instruction; the
#      first word, because an info string carries attributes here (```bash title="x"),
#   4. an exemption that gives no reason. A fenced block whose immediately preceding non-blank
#      line is `<!-- check-check-commands: allow <reason> -->` is not judged, and neither is a
#      line whose comment ends `# check-check-commands: allow <reason>` — its comment as the
#      shell reads it, so a line that quotes the marker inside a string exempts nothing it goes
#      on to run; the reason is what makes the exemption reviewable, so an empty one is itself
#      a problem.
#
# Excluded, each for its own reason:
#
#   * `docs/plans/**`, `docs/superpowers/**` and `.superpowers/**` — human-authored history of
#     what was done at the time, not commands anyone is told to run now,
#   * `.github/scripts/**` — these guards quote the very command names they forbid, as data
#     (`check-release-credentials.sh`'s `"bunx"` in a Python set, and this file's own rules),
#   * Dockerfiles, which are not judged at all: an image's own build is where a tool legitimately
#     comes from PATH, which is why `packages/daemon/docker/worker.Dockerfile`'s
#     `ln -s bun /usr/local/bin/bunx` — the line that creates the `bunx` every agent then misuses
#     — passes,
#   * `.git`, `.jj`, `node_modules`, `.venv` and `vendor`, which hold nothing this repository
#     prescribes.
#
# What this gate reads is what prescribes a command: the manifests, the workflows, the shell
# files and the guides above. It does not read source, so a prescribed command written inside a
# `.ts`, `.go` or `.py` file is outside this gate and stays right by review alone —
# `packages/dispatch/e2e/local-instance.ts` printed one to the developer on every run. The limit
# is recorded here so the next person meets it rather than discovers it.
#
# Files are discovered by walking the tree, not by asking jj or git, so an unsnapshotted change
# cannot read green locally. Every problem is printed with its file and line; the only non-zero
# exit is a named problem, and a file that cannot be parsed, cannot be decoded, or cannot be
# read at all — a dangling symlink the walk finds — is a named problem too.
#
# Run from anywhere: .github/scripts/check-check-commands.sh
# CI runs it in the lint job of pr-and-main.yaml, and its tests in that workflow's test job.
set -euo pipefail

cd "$(dirname "$0")/../.."

python3 <<'PY'
import json
import re
import sys
from pathlib import Path

import yaml


class LineLoader(yaml.SafeLoader):
    """SafeLoader that records where each mapping and each value was written, so a problem names
    the line the reader has to edit."""


class Mapping(dict):
    """A YAML mapping that remembers its lines. They are attributes, not keys: a document is never
    searched for a key the loader invented, so a job or a step named `__line__` is judged like any
    other."""

    line: int
    lines: dict[str, int]
    spans: dict[str, tuple[int, int]]


def _mapping_with_line(loader, node):
    mapping = Mapping(loader.construct_mapping(node, deep=False))
    mapping.line = node.start_mark.line + 1
    pairs = [(key, value) for key, value in node.value if hasattr(key, "value")]
    mapping.lines = {key.value: key.start_mark.line + 1 for key, _ in pairs}
    # The value's own span, so each line of a block scalar is reported where it is written rather
    # than where its key is.
    mapping.spans = {
        key.value: (value.start_mark.line + 1, value.end_mark.line + 1) for key, value in pairs
    }
    return mapping


LineLoader.add_constructor(yaml.resolver.BaseResolver.DEFAULT_MAPPING_TAG, _mapping_with_line)

WRAPPER = "scripts/repo-tool.sh"
# The one other approved way to reach a tool: the pinned binary itself, by a path that ends
# here. `./node_modules/.bin/biome` and `node_modules/.bin/tsc` are the file the lockfile
# installed, not a name PATH or the registry answers.
PINNED_PATH = "node_modules/.bin"
MARKER = "check-check-commands: allow"
PINNED_TOOLS = ("biome", "tsc", "tsserver", "playwright")
# Every package runner, by the basename of the word that invokes it: the one-word spellings
# first, then each manager's own runner subcommands, which hand the name after them to a
# resolver exactly as `bunx` does. `pnpm` and `yarn` are here although no image installs them
# today: the rule is about what a guide may tell a reader to run.
RESOLVERS = ("bunx", "pnpx", "npx")
RUNNER_SUBCOMMANDS = {
    "bun": frozenset({"x"}),
    "npm": frozenset({"exec"}),
    "pnpm": frozenset({"exec", "dlx"}),
    "yarn": frozenset({"exec", "dlx"}),
}
# The ordinary subcommands, whose following words are that subcommand's own arguments rather
# than commands the shell resolves: `npm view biome version` names a package, `bun run lint`
# names a recipe. Named, never guessed, and deliberately short — a subcommand that is not here
# is refused rather than swallowed, because swallowing the untabled ones is how
# `pnpm exec biome check .` and `yarn biome check .` escaped this gate twice.
ORDINARY_SUBCOMMANDS = {
    "bun": frozenset(
        "run test install add build init link update remove pm publish create outdated".split()
    ),
    "npm": frozenset("view install ci run publish pack version test audit why info".split()),
}
# pnpm and yarn take the same ordinary words, less the three only another manager has: `pm` is
# bun's, `ci` is npm's, and bun's `build` is a subcommand where theirs would be a recipe name.
ORDINARY_SUBCOMMANDS["pnpm"] = (ORDINARY_SUBCOMMANDS["bun"] | ORDINARY_SUBCOMMANDS["npm"]) - {
    "pm",
    "ci",
    "build",
}
ORDINARY_SUBCOMMANDS["yarn"] = ORDINARY_SUBCOMMANDS["pnpm"]
SKIP_DIRS = {".git", ".jj", "node_modules", ".venv", "vendor"}
EXCLUDED_DIRS = ("docs/plans", "docs/superpowers", ".superpowers", ".github/scripts")
GUIDES = (
    "**/AGENTS.md",
    "**/README.md",
    "skills/**/*.md",
    "packages/claude-envoy/legion/skills/**/*.md",
    "docs/solutions/**/*.md",
    "docs/kubernetes.md",
    "docs/deployment.md",
)
SEGMENT = re.compile(r"&&|\|\||[;|]")
ENV_PREFIX = re.compile(r"^[A-Za-z_][A-Za-z0-9_]*=")
# Stepped over before the first word is read: a subshell's or group's opener, a brace and a
# quote. A `$` is stepped over only when it is a prompt's — `$ bun run lint` in a guide — so a
# word that an expansion begins stays whole for rule 3 instead of being read as the text after
# the `$`.
LEADING = " \t({}\"'"
PROMPT = re.compile(r"^\$(?:\s|$)")
# An unexpanded expansion anywhere in a word: `$name`, `${…}`, `$(…)`, `$1`, or a substitution in
# backticks. What carries one, the gate cannot resolve to a command.
EXPANSION = re.compile(r"\$\S|`")
FENCE = re.compile(r"^\s*(?P<fence>`{3,}|~{3,})(?P<info>.*)$")
# The info strings a fenced block is read as commands under: none at all, a shell or a command
# session, and yaml — a workflow's `run:` step is a prescription wherever it is quoted. A fence
# that names anything else is data: a transcript in a `text` block, a manifest in a `json` one
# and a module in a `ts` one are output or source, not an instruction. A language this gate does
# not know is not judged either, because reporting a quoted transcript is a report that is
# simply untrue, and the exemption marker stays for the judged block that legitimately shows the
# wrong form.
JUDGED_FENCES = frozenset(
    "sh shell bash zsh ksh console terminal shell-session shellsession sh-session yaml yml".split()
)
ALLOW_COMMENT = re.compile(rf"^<!--\s*{MARKER}(?P<reason>.*?)-->$")
ALLOW_TRAILING = re.compile(rf"#\s*{MARKER}(?P<reason>[^#]*)$")

problems = []


def one_line(value: object) -> str:
    """A value quoted into a message cannot carry newlines: a block scalar in a workflow would
    otherwise reach the Actions log as its own `::stop-commands::` line."""
    return " ".join(str(value).split())


def command_words(segment: str) -> list[str]:
    """The words a segment would run, from its command onwards: brackets, quotes, a prompt's `$ `
    and any `FOO=bar` assignments are stepped over. Quotes are stepped over rather than obeyed,
    so the inner command of `bash -c '… && bunx playwright …'` is read as a command of its own.
    Each word keeps the text that was written, an expansion included, because a word the gate
    cannot read is itself something rule 3 reports."""
    text = segment.strip()
    while True:
        text = text.lstrip(LEADING)
        if PROMPT.match(text):
            text = text[1:]
            continue
        words = text.split(None, 1)
        if not words:
            return []
        if not ENV_PREFIX.match(words[0]):
            return text.split()
        text = words[1] if len(words) > 1 else ""


def spoken(word: str) -> str:
    """A word as the shell would name it: the quotes it was written inside are not part of it."""
    return word.strip("\"'`")


def runner_problem(spelling: str) -> str:
    return (
        f"runs `{one_line(spelling)}`, a package runner that resolves the name it is given from "
        f"the npm registry rather than this repository's lockfile (with no `node_modules`, "
        f"`{one_line(spelling)} biome` fetches a squatted package that checks nothing and exits "
        f"0) — run `{WRAPPER} <tool>` instead"
    )


def split_comment(line: str) -> tuple[str, str]:
    """A line as the shell reads it: the command it runs, and the comment it carries. A `#` that
    begins a word and stands outside quotes ends the command, so everything from there is the
    comment — the one place the exemption marker is honoured, since a marker quoted inside a
    string is part of the word it sits in and exempts nothing. The split happens before the
    segments are split, so a `;` or a `|` written inside a comment starts no segment of its
    own."""
    quote = ""
    for index, character in enumerate(line):
        if quote:
            if character == quote:
                quote = ""
        elif character in "\"'":
            quote = character
        elif character == "#" and (index == 0 or line[index - 1] in " \t"):
            return line[:index], line[index:]
    return line, ""


def basename(word: str) -> str:
    """The name a word would run as: the quotes it was written inside are not part of it, and a
    path is the file it ends with."""
    return spoken(word).rsplit("/", 1)[-1]


def reached(words: list[str]) -> tuple[int, str] | None:
    """Where a segment reaches a pinned check tool — (the word's index, the tool) — or None. The
    tool is the first word whose whole basename is one of the four names: whole-word and
    case-sensitive, nothing by prefix or by substring, so `tsconfig.check.json`, `biome.json`,
    `playwright.config.ts` and a prose `Biome` are the identifiers and words they are."""
    for index, word in enumerate(words):
        name = basename(word)
        if name in PINNED_TOOLS:
            return index, name
    return None


def offenders(command: str):
    """Every segment of a command — its comment already removed by `judge` — that reaches a
    pinned check tool outside the two approved spellings, that calls a package runner at all, or
    that cannot be read for certain, as the sentence naming what is wrong with it (the segment's
    own words are quoted inside it). A segment yields at most one sentence, so a line never
    gives two messages. One reading of what the shell would run serves a recipe, a workflow
    `run:` line, a shell line and a fenced command line alike."""
    for segment in SEGMENT.split(command):
        words = command_words(segment)
        if not words:
            continue
        first = spoken(words[0])
        manager = basename(first)
        if manager in RESOLVERS:
            yield runner_problem(first)
            continue
        if manager in RUNNER_SUBCOMMANDS:
            # A package manager takes a subcommand first — the first word after it that is not
            # an option, so `bun --bun x biome` is the `bun x` it spells — and the subcommand
            # decides what the words after it are: a runner resolves the first of them from the
            # registry; a pinned tool's own basename is that tool, left to the tool rule below;
            # an ordinary subcommand's words are its arguments, so `bun run lint` and
            # `npm view biome version` are the ordinary commands they look like; and a
            # subcommand this gate does not know is refused rather than waved through, since an
            # unfamiliar shape is exactly how a runner has escaped before.
            subcommand = next(
                (spoken(word) for word in words[1:] if not spoken(word).startswith("-")), ""
            )
            if subcommand in RUNNER_SUBCOMMANDS[manager]:
                yield runner_problem(f"{first} {subcommand}")
                continue
            if basename(subcommand) not in PINNED_TOOLS:
                if subcommand in ORDINARY_SUBCOMMANDS[manager]:
                    continue
                unknown = reached(words)
                if unknown is None:
                    # No pinned tool in the segment: whatever this subcommand is, it is not
                    # this gate's business.
                    continue
                yield (
                    f"cannot be read for certain: `{one_line(segment.strip())}` runs "
                    f"`{one_line(first)} {one_line(subcommand)}`, a subcommand this gate does "
                    f"not know, so whether the words after it are commands or that "
                    f"subcommand's own arguments is not knowable here, and it names "
                    f"`{unknown[1]}` — spell the command out as `{WRAPPER} {unknown[1]}`, or "
                    f"write `{MARKER} <reason>` to say why this line is safe"
                )
                continue
        found = reached(words)
        if found is None:
            continue
        index, tool = found
        word = spoken(words[index])
        before = spoken(words[index - 1]) if index else ""
        if word.endswith(f"{PINNED_PATH}/{tool}") or before.endswith(WRAPPER):
            continue
        if EXPANSION.search(words[0]):
            yield (
                f"cannot be read for certain: `{one_line(segment.strip())}` starts with a "
                f"shell expansion, so what it runs is not knowable here, and it names "
                f"`{tool}` — spell the command out as `{WRAPPER} {tool}`, or write "
                f"`{MARKER} <reason>` to say why what this expands to is safe"
            )
        else:
            # True of every line this is printed for, the line that names a tool as data
            # included: what the gate read is a command, and what it found there is the tool's
            # own name outside the two approved spellings. Whether the line runs the tool is
            # the author's to say, through the marker, which is why the sentence ends by
            # offering it.
            yield (
                f"names `{tool}` where this gate reads a command, and reaches it neither "
                f"through `{WRAPPER}` nor as the pinned `{PINNED_PATH}/{tool}` — a name reached "
                f"any other way is answered by whatever PATH holds (the worker image ships its "
                f"own tsc, and with no `node_modules` the name reaches a squatted package) "
                f"rather than the {tool} this repository pins, so run `{WRAPPER} {tool}` or the "
                f"pinned `{PINNED_PATH}/{tool}`, or write `{MARKER} <reason>` to say why this "
                f"line does not run {tool} at all, or cannot reach it that way"
            )


def judge(path: Path, number: int, text: str, context: str) -> None:
    """Judges one command line, honouring the exemption its comment carries. The marker is read
    from the comment `split_comment` finds and from nowhere else, so a line that quotes the
    marker inside a string exempts nothing it goes on to run. An exemption with no reason is
    reported instead: the reason is the whole of what makes it reviewable."""
    command, comment = split_comment(text.rstrip())
    exemption = ALLOW_TRAILING.search(comment)
    if exemption:
        if not exemption.group("reason").strip():
            problems.append(
                f"{path}:{number}: this exemption gives no reason — write "
                f"`# {MARKER} <reason>`"
            )
        return
    for wrong in offenders(command):
        problems.append(f"{path}:{number}: {context} {wrong}")


def glob_to_regex(pattern: str) -> re.Pattern[str]:
    """`**/` spans directories, `*` stops at one: `packages/*/AGENTS.md` is a package's own guide,
    not one nested three levels under it."""
    parts = []
    for piece in re.split(r"(\*\*/|\*)", pattern):
        if piece == "**/":
            parts.append(r"(?:[^/]+/)*")
        elif piece == "*":
            parts.append(r"[^/]*")
        else:
            parts.append(re.escape(piece))
    return re.compile("".join(parts) + r"\Z")


GUIDE_PATTERNS = [glob_to_regex(pattern) for pattern in GUIDES]


def walk() -> list[Path]:
    """Every file in the tree, the directories that hold nothing prescriptive skipped. A symlinked
    directory is not descended into: it would be walked twice or leave the tree entirely."""
    found = []
    stack = [Path(".")]
    while stack:
        for entry in sorted(stack.pop().iterdir()):
            if entry.is_symlink() and entry.is_dir():
                continue
            if entry.is_dir():
                if entry.name not in SKIP_DIRS:
                    stack.append(entry)
            else:
                found.append(entry)
    return found


def excluded(path: Path) -> bool:
    text = path.as_posix()
    return any(text.startswith(f"{directory}/") for directory in EXCLUDED_DIRS)


def read_text(path: Path) -> str | None:
    """None when the file cannot be read or cannot be decoded — named like an unparseable
    workflow, not raised. `OSError` beside `UnicodeDecodeError` because the walk finds files no
    one can open: a dangling symlink is one, and a required job that ends in a traceback names
    no file at all. UTF-8 explicitly: the gate must not depend on the runner's or a developer's
    locale."""
    try:
        return path.read_bytes().decode("utf-8")
    except UnicodeDecodeError as error:
        problems.append(f"::error file={path}::{path} is not readable as text: {one_line(error)}")
        return None
    except OSError as error:
        problems.append(f"::error file={path}::{path} cannot be read: {one_line(error)}")
        return None


files = [path for path in walk() if not excluded(path)]
manifests = [path for path in files if path.name == "package.json"]
workflows = [
    path
    for path in files
    if path.parent == Path(".github/workflows") and path.suffix in {".yml", ".yaml"}
]
shell_scripts = [path for path in files if path.suffix == ".sh"]
guides = [
    path for path in files if any(pattern.match(path.as_posix()) for pattern in GUIDE_PATTERNS)
]


def recipe_line(lines: list[str], name: str) -> int:
    """The line a recipe's own key sits on, found in the file's text from the `scripts` key down:
    the parsed document carries no lines of its own, and a recipe named like a dependency must be
    reported where it is written, not where the name first appears."""
    needle = f'"{name}"'
    start = next(
        (number for number, text in enumerate(lines) if '"scripts"' in text),
        0,
    )
    for number in range(start, len(lines)):
        if needle in lines[number]:
            return number + 1
    return start + 1


for manifest in manifests:
    content = read_text(manifest)
    if content is None:
        continue
    try:
        document = json.loads(content)
    except json.JSONDecodeError as error:
        # One named line, never a traceback: this gate runs on every pull request, and a file it
        # cannot parse is that file's problem to report, not a crash for everyone.
        problems.append(f"::error file={manifest}::{manifest} is not valid JSON: {one_line(error)}")
        continue
    scripts = document.get("scripts") if isinstance(document, dict) else None
    if not isinstance(scripts, dict):
        continue
    lines = content.splitlines()
    for name, command in scripts.items():
        if isinstance(command, str):
            judge(manifest, recipe_line(lines, name), command, f'the "{name}" recipe')


def run_lines(raw: list[str], span: tuple[int, int], value: str):
    """(line number, command line) for a `run:` value. Each line of a block scalar is matched to
    its own line inside the span the loader gives the value, a cursor forward so two identical
    commands are reported separately; a folded or quoted scalar, whose text is not in the file
    line for line, falls back to the line the value starts on."""
    start, end = span
    cursor = start
    for text in value.splitlines():
        stripped = text.strip()
        if not stripped or stripped.startswith("#"):
            continue
        number = start
        for candidate in range(cursor, min(end, len(raw)) + 1):
            if raw[candidate - 1].strip() == stripped:
                number = candidate
                cursor = candidate + 1
                break
        yield number, stripped


for workflow in workflows:
    content = read_text(workflow)
    if content is None:
        continue
    try:
        document = yaml.load(content, Loader=LineLoader)
    except yaml.YAMLError as error:
        problems.append(f"::error file={workflow}::{workflow} is not valid YAML: {one_line(error)}")
        continue
    raw = content.splitlines()
    jobs = document.get("jobs") if isinstance(document, dict) else None
    for job in jobs.values() if isinstance(jobs, dict) else []:
        if not isinstance(job, dict):
            continue
        for step in job.get("steps") or []:
            if not isinstance(step, dict) or not isinstance(step.get("run"), str):
                continue
            step_line = getattr(step, "lines", {}).get("run", getattr(step, "line", 0))
            span = getattr(step, "spans", {}).get("run", (step_line, step_line))
            for number, command in run_lines(raw, span, step["run"]):
                judge(workflow, number, command, "this run: line")

for script in shell_scripts:
    content = read_text(script)
    if content is None:
        continue
    for number, text in enumerate(content.splitlines(), start=1):
        stripped = text.strip()
        if not stripped or stripped.startswith("#"):
            continue
        judge(script, number, stripped, "this line")


def judged_fence(info: str) -> bool:
    """Whether a fenced block is read as commands, by the first word of its info string — the
    first word, because an info string carries attributes in this repository's markdown. An
    empty info string, a shell, a command session and yaml are commands; every other language
    is data, and one this gate does not know is data too, since a block it cannot name is as
    likely a transcript as a recipe."""
    words = info.strip().split()
    return not words or words[0].lower() in JUDGED_FENCES


for guide in guides:
    content = read_text(guide)
    if content is None:
        continue
    fence = ""
    exempt = judged = False
    previous, previous_number = "", 0
    for number, text in enumerate(content.splitlines(), start=1):
        match = FENCE.match(text)
        if not fence:
            if match:
                fence = match.group("fence")
                judged = judged_fence(match.group("info"))
                exemption = ALLOW_COMMENT.match(previous.strip())
                exempt = bool(exemption and exemption.group("reason").strip())
                if exemption and not exempt:
                    problems.append(
                        f"{guide}:{previous_number}: this exemption gives no reason — write "
                        f"`<!-- {MARKER} <reason> -->`"
                    )
            elif text.strip():
                previous, previous_number = text, number
            continue
        # A fence closes on the same character, at least as long, and carrying no info string.
        closing = match and match.group("fence")[0] == fence[0] and not match.group("info").strip()
        if closing and len(match.group("fence")) >= len(fence):
            fence = ""
            # The closing fence is the block's last non-blank line, so the marker above this
            # block cannot exempt the next one too.
            previous, previous_number = text, number
            continue
        stripped = text.strip()
        if exempt or not judged or not stripped or stripped.startswith("#"):
            continue
        judge(guide, number, stripped, "this fenced command")

for problem in problems:
    print(problem, file=sys.stderr)
if problems:
    print(
        f"::error::{len(problems)} place(s) prescribe a check command that does not run this "
        f"repository's pinned tool through {WRAPPER}",
        file=sys.stderr,
    )
    raise SystemExit(1)
print(
    f"every prescribed biome, tsc, tsserver and playwright command runs through {WRAPPER} "
    f"({len(manifests)} manifest(s), {len(workflows)} workflow(s), {len(shell_scripts)} shell "
    f"script(s), {len(guides)} guide(s))"
)
PY
