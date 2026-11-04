---
title: "A fingerprint computed outside the workspace root stops excluding the ledger, and says so only on stderr"
category: legion
tags:
  - unchanged-diff
  - fingerprint
  - jj
  - filesets
  - review-rounds
date: 2026-10-25
status: active
module: skills/legion-worker
related_issues:
  - "INFRA-251"
  - "sjawhar/legion#2163"
---

# A fingerprint computed outside the workspace root stops excluding the ledger, and says so only on stderr

Extends `docs/solutions/legion/unchanged-diff-fingerprint-one-fileset-verified-by-a-pair.md`, which
pairs two **heads** and requires one fileset expression. The pair it never checks is two **roles**
at one commit, and that is where this failure hides.

- **jj resolves a fileset against the current directory, not the repository root.** `-R` chooses
  the repository; it does not choose where `~(.legion | docs/solutions)` points. Run the
  fingerprint from a package directory and the expression names
  `<that package>/.legion` and `<that package>/docs/solutions`, which match nothing, so the hash
  covers the handoff ledger and the retro learnings — the two things the fileset exists to leave
  out. The command the skill gives starts `cd -- "$LEGION_WORKSPACE"` for exactly this reason; a
  session whose shell persists may already be somewhere else.
- **The only warning is on stderr, and every pipeline eats it.** jj prints
  `Warning: No matching entries for paths: .legion, docs/solutions`; the fingerprint line pipes
  stdout into `sed` and `sha256sum`, so the warning never reaches the recorded value. Nothing fails,
  and a wrong hash looks exactly like a right one.
- **The consequence is a verdict, not a number.** A role whose hash includes the ledger sees
  `changed` whenever another role pushed a handoff, so it runs a full round it did not owe; and
  two roles comparing their own pairs never notice they measured different things.
- **Cross-check one shared commit before anyone reads `unchanged` or `changed`.** Every role that
  computes a fingerprint posts the head it computed at. Take any commit two roles both hashed and
  compare the two values. They must be equal. When they are not, the commands differ — the working
  directory first — and no `unchanged` built on either is worth anything until that is settled.
- **Watch for the stderr line yourself.** Run the fingerprint without the pipe once if you are
  unsure where you are: a `No matching entries` warning naming `.legion` or `docs/solutions` means
  you are not at the workspace root.

## Evidence (INFRA-251, sjawhar/legion#2163)

The fresh-eyes read of the second retro found the roles' posted fingerprints disagreeing at one
commit: at head `9d688fa557a1` the implementer posted `cb5c1756…` and the tester `e218c890…`, and
at `ea350c3a` the implementer posted `799d32d6…` and the tester `52945ed7…`. The reviewer's two
values agree with the implementer's. No verdict turned on it this round, because every role read
`changed` and then accounted for the delta line by line.

The mechanism was then reproduced in this workspace, one command run twice at one commit, differing
only in the directory it ran from:

```
$ cd -- "$LEGION_WORKSPACE" && jj -R "$LEGION_WORKSPACE" diff --from "fork_point(\"main\"@origin | 9d688fa557a1)" \
    --to 9d688fa557a1 --git --context 0 '~(.legion | docs/solutions)' | sed -e '/^@@/d' -e '/^index /d' | sha256sum
cb5c1756176e802a7214ed064c022788f674f618027256850a5cef1f3dab3f8d

$ cd packages/daemon && jj -R "$LEGION_WORKSPACE" diff --from … --to 9d688fa557a1 \
    --git --context 0 '~(.legion | docs/solutions)' | sed … | sha256sum
Warning: No matching entries for paths: .legion, docs/solutions
8ccdc2ed41d90995fd036f67fa650a1badbbb2cfe571cd675d8e14e22729b9bb
```

Same repository, same commit, same expression, two hashes, and the only sign of it is the warning
line — which the recorded command's pipe discards.
