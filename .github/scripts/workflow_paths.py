"""What a GitHub Actions workflow's path filters cover, read as GitHub and dorny/paths-filter read
them: a walk of the repository's files, a path glob as a regex, an ordered pattern list where `!`
negates, a dorny/paths-filter filter under its predicate quantifier, and a workflow's events.

check-image-trigger-paths.sh and check-ci-filters.sh (through ci_filter_coverage.py) import it from
the repository they check, running `PYTHONDONTWRITEBYTECODE=1 PYTHONPATH=.github/scripts python3`
from that repository's root.
"""

import re
from pathlib import Path

# Directories a walk never enters: version control and installed dependencies.
SKIP_DIRS = {".git", ".jj", "node_modules", ".venv", "vendor"}


def walk(root: Path = Path(".")) -> list[str]:
    """Every file under root, as a path relative to the working directory. It walks the tree rather
    than asking git, so an unsnapshotted change cannot read green locally."""
    found = []
    stack = [root]
    while stack:
        for entry in sorted(stack.pop().iterdir()):
            if entry.is_symlink() and entry.is_dir():
                continue
            if entry.is_dir():
                if entry.name not in SKIP_DIRS:
                    stack.append(entry)
            else:
                found.append(entry.as_posix())
    return found


def glob_regex(pattern: str) -> re.Pattern:
    """A path glob as a regex over a repository-relative path, with picomatch's meaning — the
    matcher dorny/paths-filter runs (https://github.com/micromatch/picomatch#globbing-features):
    `**` spans directories, `*` and `?` stay inside one. `{` and `}` read as literal characters,
    where picomatch expands `{a,b}` and GitHub documents no meaning for them, so a check refuses
    them in either (unread_patterns, unread_filter_patterns). GitHub's own `paths` agree except
    for `?`, `+` and `\\`, which unread_patterns finds. A .dockerignore line and a COPY source read
    the same: Docker matches them with Go's filepath.Match, where `{` is a literal character."""
    out, index = "", 0
    while index < len(pattern):
        if pattern.startswith("**/", index):
            out += "(?:.*/)?"
            index += 3
        elif pattern.startswith("**", index):
            out += ".*"
            index += 2
        elif pattern[index] == "*":
            out += "[^/]*"
            index += 1
        elif pattern[index] == "?":
            out += "[^/]"
            index += 1
        elif pattern[index] == "[" and "]" in pattern[index + 1 :]:
            end = pattern.index("]", index + 1)
            out += pattern[index : end + 1]
            index = end + 1
        else:
            out += re.escape(pattern[index])
            index += 1
    return re.compile(out)


class Filter:
    """An ordered pattern list where the last match wins and `!` negates, as GitHub reads
    `paths`. `everything` is an event with no path filter."""

    def __init__(self, patterns: list[str] | None, ignore: bool = False):
        self.everything = patterns is None
        self.ignore = ignore
        self.rules = [
            (p.startswith("!"), glob_regex(p.removeprefix("!"))) for p in (patterns or [])
        ]

    def covers(self, path: str) -> bool:
        if self.everything:
            return True
        matched = False
        for negated, regex in self.rules:
            if regex.fullmatch(path):
                matched = not negated
        return not matched if self.ignore else matched


class PathsFilter:
    """One dorny/paths-filter filter, as v4.0.3 evaluates it (src/filter.ts:58-66,133-149,195-213
    at ceb8a2b8f2d89434be7ff52d3de7ec3738c5cc9d), under one of the two quantifiers this models.
    Under `some`, the default, a file matches when any pattern matches it; picomatch reads a
    leading `!` as "not this pattern", so there a `!` pattern matches every file outside it. Under
    `some-with-excludes` a file matches when a pattern without `!` matches it and no `!` pattern
    does, so a `!` pattern excludes."""

    QUANTIFIERS = ("some", "some-with-excludes")

    def __init__(self, patterns: list[str], quantifier: str):
        if quantifier not in self.QUANTIFIERS:
            raise ValueError(f"predicate-quantifier {quantifier} is not modelled")
        self.excludes = quantifier == "some-with-excludes"
        self.rules = [
            (p.startswith("!"), glob_regex(p.removeprefix("!"))) for p in patterns
        ]

    def covers(self, path: str) -> bool:
        if self.excludes:
            matched = [negated for negated, regex in self.rules if regex.fullmatch(path)]
            return False in matched and True not in matched
        return any(bool(regex.fullmatch(path)) != negated for negated, regex in self.rules)


def events(document: dict) -> dict:
    """The workflow's `on:` as a mapping; YAML 1.1 reads a bare `on` key as True."""
    triggers = document.get("on", document.get(True))
    if isinstance(triggers, str):
        return {triggers: None}
    if isinstance(triggers, list):
        return {name: None for name in triggers}
    return triggers if isinstance(triggers, dict) else {}


def unread_patterns(patterns: list) -> list:
    """The patterns glob_regex would misread as GitHub reads them: GitHub's filter patterns give
    `?` and `+` regex meanings (zero or one, one or more of the preceding character) and read `\\`
    as an escape
    (https://docs.github.com/en/actions/writing-workflows/workflow-syntax-for-github-actions#filter-pattern-cheat-sheet),
    and that cheat sheet documents no meaning for `{` or `}`, which glob_regex reads literally.
    A check refuses these rather than guess."""
    return [p for p in patterns if any(char in str(p) for char in "?+\\{}")]


def unread_filter_patterns(patterns: list) -> list:
    """The patterns glob_regex would misread as dorny/paths-filter reads them: picomatch expands a
    brace (`{a,b}`, `{a..c}`), which glob_regex reads as literal characters. A check refuses these
    rather than guess."""
    return [p for p in patterns if any(char in str(p) for char in "{}")]
