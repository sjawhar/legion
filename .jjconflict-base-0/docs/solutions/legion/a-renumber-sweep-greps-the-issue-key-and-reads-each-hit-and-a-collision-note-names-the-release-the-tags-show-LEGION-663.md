---
title: "A renumber's sweep greps the issue key and reads each hit, and a collision note names the release the tags show, never one assumed or one that does not exist yet"
category: legion
tags:
  - daemon-api-contract
  - renumber
  - collision-note
  - changelog
  - release-tags
date: 2026-10-11
status: active
module: packages/daemon/internal/api
related_issues:
  - "LEGION-663"
  - "sjawhar/legion#1874"
---

# A renumber's sweep greps the issue key and reads each hit, and a collision note names the release the tags show, never one assumed or one that does not exist yet

Extends docs/solutions/legion/a-contract-numbers-prose-is-declaration-history-or-current-write-current-as-a-pointer-and-sweep-docs-solutions-too-LEGION-578.md.

- A `contract N` grep misses the number on a wrapped line. Markdown prose wraps at 100 columns, so
  "daemon API contract" ends one line and "N (LEGION-KEY)." starts the next; `grep 'contract 19'`
  never sees it. Sweep by the issue key instead and read every hit: `grep -rn 'LEGION-<KEY>'
  <prose files> | grep -E '\b<old>\b'`, plus `grep -rn -B1 '^ *<old> (LEGION-<KEY>)'` for the
  wrapped form. Run it after each renumber and once more at the head you push.
- A history sentence that names a release ("pi-legion X.Y.Z declaring it without the report") is
  a fact to read, not to recall. Read the manifests at the tags before writing it:
  `for t in pi-legion-v8.10.1 pi-legion-v8.11.0; do jj file show -r "$t@origin"
  packages/pi-legion/package.json | grep daemonApiVersion; done`. The first tag whose
  `legion.daemonApiVersion` equals the colliding number is the release the note names; the
  releases around a sibling's merge are not it by default (8.10.0 and 8.10.1 declared 18 here;
  8.11.0 was the first at 19).
- Never write "no release declares N yet". It is a *current* claim in that document's terms, true
  for hours: the sibling's number ships at the next release (8.14.0 declared 20 within the same
  day) and the sentence is wrong on `main` from then on. Name the release once a tag carries the
  number; until then, name the issue that declares it and say nothing of releases.
- A second collision renumbers again, and the note records both: which number this branch took
  first, who landed at it and which release declared it, then the same for the second. One
  paragraph in `version.go`, the same facts once in the plugin's CHANGELOG rollout sentence, and
  `AGENTS.md`'s "after contract N's …" chain gains the sibling's clause ahead of the older ones.
  Keep `main`'s paragraph for the number it kept verbatim; yours goes below it.

## Evidence

LEGION-663 (sjawhar/legion#1874). The retarget renumbered 19 → 20 and the review found the
renumber half-applied in prose: `packages/contracts/CHANGELOG.md:14` read `19 (LEGION-663).` on
the line after "daemon API contract" (invisible to the `contract 19` grep the implementer ran), and
`version.go:132` and `packages/pi-legion/CHANGELOG.md:57` named pi-legion 8.10.0 and 8.10.1 as the
release that burned 19 where the tags read 8.9.0–8.10.1 → 18 and 8.11.0–8.13.0 → 19 (fixed in
`1d5a2121`). After #1878 landed at 20 the branch renumbered to 21 (`6da3ade3`) and wrote "20 is the
LEGION-668 entry below, which no release declares yet"; pi-legion 8.14.0 declared 20 before the
next review, which named the stale sentence (fixed in `01e4275f`).
