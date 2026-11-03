---
title: "A refusal test makes every write it forbids observable"
category: testing
tags:
  - negative-control
  - refusal
  - test-doubles
date: 2026-10-28
status: active
module: packages/daemon
related_issues:
  - "sjawhar/legion#2396"
---

# A refusal test makes every write it forbids observable

- A test that proves "the refused call wrote nothing" must make each forbidden write change
  bytes on disk. A stub that returns the same value every call, together with a writer that skips
  unchanged content, hides a write made before the refusal.
- Make the stub return a fresh value on each call, such as a counter in a token. Snapshot every
  file the refusal must leave alone, including the directories a later step writes, and compare
  after each refused path.
- Prove the test bites by moving the forbidden write above the refusal in a mutant. In Go,
  `go test -overlay <json>` runs the real test against a scratch copy of one file, and the working
  copy stays as it is. A mutant the suite still passes is a gap in the test, not a pass.

## Evidence

- `packages/daemon/internal/runtime/tmux/tmux_real_test.go:1115-1119` mints `ghs_mint_<n>` per
  launch. `TestRealTmuxSplitsIntoTheIssueWindow` snapshots the claim's gh directory and compares it
  after the refused spawn and after the refused resume.
- Before commit f0259648 the rig's `staticCredential` returned one bundle, and
  `ghconfig.WriteBundle` rewrites only changed files. So the mutant that writes the gh bundle above
  `refuseLiveIncarnation` failed only the two pod-only tests. After it, that mutant fails with
  "the refused second spawn rewrote the live tester's secret files" (`ghs_mint_2` → `ghs_mint_4`).
