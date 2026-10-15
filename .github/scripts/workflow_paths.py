"""What a GitHub Actions workflow's path filters cover, read as GitHub and dorny/paths-filter read
them: a walk of the repository's files, a path glob as a regex, an ordered pattern list where `!`
negates, and a workflow's events.

check-image-trigger-paths.sh imports it from the repository it checks, running
`PYTHONDONTWRITEBYTECODE=1 PYTHONPATH=.github/scripts python3` from that repository's root.
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
    `**` spans directories, `*` and `?` stay inside one. GitHub's own `paths` agree except for
    `?`, `+` and `\\`, which unread_patterns finds; a .dockerignore line reads the same."""
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
    (https://docs.github.com/en/actions/writing-workflows/workflow-syntax-for-github-actions#filter-pattern-cheat-sheet).
    A check refuses these rather than guess."""
    return [p for p in patterns if any(char in str(p) for char in "?+\\")]
