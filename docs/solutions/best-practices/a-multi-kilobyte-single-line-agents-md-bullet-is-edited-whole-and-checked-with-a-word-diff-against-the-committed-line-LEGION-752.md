---
title: "A multi-kilobyte single-line AGENTS.md bullet is edited whole and checked with a word diff against the committed line"
category: best-practices
tags:
  - agents-md
  - documentation-edits
  - verification
  - envoy
date: 2026-10-14
status: active
module: packages/envoy/AGENTS.md
related_issues:
  - "LEGION-752"
  - "sjawhar/legion#1957"
---

# A multi-kilobyte single-line AGENTS.md bullet is edited whole and checked with a word diff against the committed line

- Several bullets of `packages/envoy/AGENTS.md` are one line of ten kilobytes or more (the
  document-approval bullet is one). A line-based edit tool replaces the whole line, so the edit
  is the whole bullet retyped with the changed clauses in it, and a dropped or doubled character
  anywhere in it is invisible in the tool's echo.
- Before committing, diff the committed line against the edited one word by word and read the
  removed and added runs alone; every run must be one you meant:

  ```bash
  jj -R "$LEGION_WORKSPACE" file show -r @- packages/envoy/AGENTS.md | sed -n '<line>p' > "$dir/old.txt"
  sed -n '<line>p' packages/envoy/AGENTS.md > "$dir/new.txt"
  git diff --no-index --word-diff=plain --word-diff-regex='[^ ]+' "$dir/old.txt" "$dir/new.txt" \
    | grep -o -E '\[-[^]]*-\]|\{\+[^}]*\+\}'
  ```

  (`$dir` a `mktemp -d` outside the workspace.) A run you did not intend is a typo in the retyped
  line; fix it and diff again.
- When the change renames a symbol the bullet cites (`rowBackedStates` → `rowBackedBlocks`), grep
  the whole file for the old name afterwards: a long bullet cites a symbol more than once, and a
  learning under `docs/solutions/` may cite it as current code too.

## Evidence (LEGION-752, sjawhar/legion#1957)

The approval bullet (`packages/envoy/AGENTS.md:1780`, about 11 KB) needed four clauses changed:
the renamed predicate, how the approved version is read, the rule for its blocks, and one more
item in the "all carry nothing" list. The edit tool took the whole line; the word diff listed
exactly those four removed/added runs and nothing else, and a grep for the old name afterwards
found two more mentions in the LEGION-573 learning's rule section, which the same commit renamed.
