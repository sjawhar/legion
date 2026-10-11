---
title: "A forward merge also ports main's prose about a component the branch moved: changelog, guide and README entries arrive in the old package with no conflict"
category: legion
tags:
  - jj
  - forward-merge
  - package-split
  - changelog
  - agents-md
  - file-moves
date: 2026-10-07
status: active
module: packages/pi-legion
applies_when:
  - Main is forward-merged into a branch that moved a component to another package
  - Main's round added `[Unreleased]` changelog bullets, AGENTS.md rows or README lines about that component
  - A file the branch kept merges cleanly while the component it describes now lives elsewhere
related_issues:
  - "LEGION-247"
  - "sjawhar/legion#1831"
---

# A forward merge also ports main's prose about a component the branch moved

Extends docs/solutions/legion/a-forward-merge-into-a-branch-that-moved-files-ports-mains-edits-to-the-new-paths-LEGION-247.md.

- After porting main's code edits to the new paths, read every hand-maintained text main appended
  in the merged range — `[Unreleased]` changelog sections, AGENTS.md tables, README lists — for
  entries that describe what you moved. They merge with no conflict marker because the file is
  still where it was, so neither the conflict list nor the serials bullet flags them, and they are
  now attached to the wrong package.
- Move each such entry to the package that ships the component, in the merge commit, and delete
  it from the old package's text; then read the old file's own statements against what remains,
  since a changelog that keeps `legion.daemonApiVersion is 14` while its split entry says the
  manifest has no `legion` key contradicts itself at the reader.
- `jj diff --from <last base> --to main@origin --name-only` is the list to walk; a `CHANGELOG.md`
  or `AGENTS.md` on it under a package you split from is a file to read, not a merge to trust.

## Evidence

sjawhar/legion#1831, forward merges 61a99564 and 013b1415: main's LEGION-592 round appended to
`packages/pi-envoy/CHANGELOG.md` an `[Unreleased] ### Added` bullet about the daemon-launched
controller session and a `### Changed` bullet `legion.daemonApiVersion is 14 (LEGION-592)`. The
merge copied both into `packages/pi-legion/CHANGELOG.md`, where the Legion entry now lives, but
left the originals in the Envoy plugin's changelog beside its own split entry stating "this
manifest has no `legion` key". No conflict marked them, the tester's round passed, and the
reviewer's round-4 pair recorded the duplication as a fast-follow (`review.json`
`keyFindings[0]`, severity minor). The same round's `packages/pi-envoy/AGENTS.md` hunk, which did
conflict, was ported to `packages/pi-legion/AGENTS.md` correctly — the conflict is what made it
visible.
