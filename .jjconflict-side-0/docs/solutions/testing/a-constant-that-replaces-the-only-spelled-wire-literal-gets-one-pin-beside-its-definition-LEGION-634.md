---
title: "A constant that replaces the only spelled wire literal gets one pin beside its definition"
category: testing
tags:
  - simplify-pass
  - wire-literal
  - transcript-entries
  - persisted-keys
  - regression-pin
date: 2026-10-09
status: active
module: packages/pi-legion/src/phase-stall.ts, packages/pi-legion/extensions
related_issues:
  - "LEGION-634"
  - "sjawhar/legion#1865"
---

# A constant that replaces the only spelled wire literal gets one pin beside its definition

Extends docs/solutions/testing/a-test-over-a-contract-derives-the-enumeration-and-hand-writes-only-what-the-contract-cannot-prove-about-itself.md.
A value that is persisted, grepped by a script, or read back by a `--resume` is what a contract
cannot prove about itself: the constant that spells it compares only to itself.

## The rules

- **When a simplify pass swaps a string literal for the exported constant in the last test that
  spelled it, add one equality beside the constant's own unit tests:**
  `expect(PHASE_STALL_ENTRY).toBe("legion-phase-stall")`. The swap is right for the test (a rename
  of the constant now fails at the import, not as an empty read), and wrong for the suite as a
  whole unless something still asserts the wire value.
- **Before accepting a "use the constant" finding, grep the repository for the literal.** A hit in
  a shell script, a fixture, a transcript reader or a migration says the value is a wire contract;
  a hit only in the module that defines the constant says nothing pins it any more.
- **A value that reaches disk is pinned where its reader is named.** The pin's comment names the
  readers: the e2e scripts that grep `"customType":"legion-phase-stall"` and the `session_start`
  restore that filters transcript entries by it.

## Evidence

- `packages/pi-legion/src/phase-stall.ts:35` defines `PHASE_STALL_ENTRY = "legion-phase-stall"`;
  `extensions/legion.ts` passes it to `pi.appendEntry` and the restore filters by it.
- LEGION-634's simplify pass replaced the literal in
  `extensions/legion-phase-stall-omp.test.ts` (`transcriptEntries("legion-phase-stall")`) with the
  import, on the reuse and quality reviewers' finding, the correct test-side fix. Afterwards a grep
  over `packages/pi-legion` and `packages/pi-shared` found the string only in the definition, while
  `scripts/e2e/stage3-4b13b-acceptance.sh:790,794,826` and
  `scripts/e2e/stage4b-sandbox-tree.sh:1467` still grep it from real transcripts.
- The reviewer of sjawhar/legion#1865 recorded it as its first minor finding and the pull request's
  one `Fast-follow:` item: the pin, beside the `src/phase-stall` unit tests.
