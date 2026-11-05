---
title: "Subtract the generated trees before a sweep, and let the generator name their extent"
category: best-practices
tags:
  - rename
  - sweep
  - generated-files
date: 2026-11-04
status: active
module: repository-wide
related_issues:
  - "cybermonklabs/legion#2734"
---

# Subtract the generated trees before a sweep, and let the generator name their extent

Extends docs/solutions/best-practices/an-identifier-sweep-is-bounded-by-what-the-token-addresses-and-closed-by-the-literals-its-own-diff-removed-pr2734.md.

That page says **exclude no file from the sweep**, and it is right about every file a human wrote.
A tracked generator output is the one class it does not reach: such a file is not an occurrence of
the identifier, it is a *copy* of one, and the copy is the generator's to write.

- **Subtract the generated trees before the first rewrite**, and restore them from the base
  afterwards. A tree-wide rewrite enumerates paths, and a tracked build output is a path like any
  other, so it is swept in silence.
- **Ask the generator rather than the conflict.** When one of its outputs conflicts, the question
  is not how to resolve it but why the file is in the change at all. Run the generator: in this
  repository it refuses and says so, which is both the rule and the remedy.
- **The conflict count is not the extent.** Only the outputs the base happened to touch conflict;
  the rest are as wrong and nothing reports them.
- **Expect no gate to catch either direction.** A pull-request head whose source and tracked copy
  disagree is the ordinary state between a merge and the rebuild that follows it, so every check
  is green over the disagreement. That is by design, and it moves one reading into the production
  check: name the rebuild's run and its commit, and read the regenerated copies.
- Check what dropping those paths does to the *changed-package* set: a package that leaves it
  leaves its changelog obligation with it
  (`docs/solutions/legion/a-changelog-fragment-claims-only-what-the-diff-does-and-every-changed-package-gets-one-pr2354.md`).

The prohibition itself, with its restore command, is already written:
`docs/solutions/legion/a-pull-request-never-commits-the-claude-envoy-build-and-ships-a-new-bundle-before-its-entry-pr2423.md`,
and `packages/claude-envoy/AGENTS.md`. This page is about the sweep that walks into it.

## Evidence

cybermonklabs/legion#2734 renamed this repository's own address across 109 files. Its sweep
rewrote nine files under `packages/claude-envoy/legion/skills/`, the tracked copy of `skills/`
that a Claude Code pane loads.

- Three of the nine conflicted at the second forward merge, the three main's own rebuild
  (`6f23228e0`) had touched. The other six had been wrong since the first sweep and nothing had
  reported them.
- `packages/claude-envoy/scripts/build.ts` refuses every mode that would write them, and says why:
  "`dist/` and `legion/` stay tracked on main, written only by main's Rebuild Claude Envoy plugin
  workflow after each merge … a pull request never commits them." Running `bun run build` is what
  turned a three-file conflict into a nine-file correction.
- No gate reports the disagreement in either direction. `packages/claude-envoy/scripts/verify-build.sh`
  builds twice into scratch directories and compares those two builds **with each other**, never
  with the tracked copy; the only reader of the tracked copy is `bun scripts/build.ts --check`
  (`check-dist`), which appears in no workflow. Measured at `56dc9ac4da44` with every check green:
  `skills/legion-worker/references/conflicts-and-rewrites.md` line 8 reads
  "Every path it cites is in cybermonklabs/legion." while its generated twin under
  `packages/claude-envoy/legion/` still reads "sjawhar/legion".
- After the revert, `git diff` against main under `packages/claude-envoy` leaves only
  `README.md` and `package.json` — the change's own two edits — so the package stays in the
  changed set and the three changelog fragments it carries stay owed.
