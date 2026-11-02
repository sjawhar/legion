---
title: "A fingerprint that moved is reported by diffing the two fingerprint inputs, not by naming the merge"
category: legion
tags:
  - forward-merge
  - fingerprint
  - review
  - bun
  - lockfile
date: 2026-11-01
status: active
module: skills/legion-worker
related_issues:
  - "sjawhar/legion#2519"
---

# A fingerprint that moved is reported by diffing the two fingerprint inputs, not by naming the merge

The forward-merge fingerprint answers one question — did this branch's own diff over its base
change — and a moved fingerprint says only that something did. A reviewer who has approved a head
needs the lines, because "changed" covers both a resolution that moved a comment and one that
changed what the code does.

- **Keep the two inputs, not just the two hashes.** The fingerprint command ends in `sha256sum`;
  run it without that step into a file before the merge and after it, and `diff` the files. What
  that prints is exactly what a reviewer must re-read.
- **Put those lines in the forward-merge comment**, under the `fingerprint <before> → <after>`
  line the procedure requires. A merge with no conflict can still move the fingerprint.
- **Expect the lockfile to be one of them.** A workspace install after a merge writes back the
  versions the merged manifests name, so a base that released a workspace package while the
  branch was open moves lines a conflict never touched.
- **A workspace package's build output is not in the lockfile.** After that install, rebuild the
  package that others import from its `dist/`, or the first typecheck fails on a module that is
  plainly in the tree.

## Evidence

sjawhar/legion#2519's second forward merge took no conflict at all, and its fingerprint still
moved, `552ce84721fa` → `16450fd783de`. Running the fingerprint command without `sha256sum` into
two files and diffing them printed four lines and nothing else: `packages/pi-envoy`
`13.11.1 → 14.2.0` and `packages/pi-legion` `15.7.1 → 16.3.0`, which `bun install` wrote back
because main released both packages while the branch was open. The branch's four
`patchedDependencies` were untouched, and that diff is what the pull request comment carried, so
the reviewer re-read four lines rather than a 265-file merge.

The same install left `packages/dispatch`'s typecheck failing with
`TS2307: Cannot find module '@legion/contracts'` and its suite red, until
`cd packages/contracts && bun run build` rebuilt the `dist/` the web package imports; every
measurement reported for that head was taken after that build.
