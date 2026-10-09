---
title: "A round that changes no code writes its handoff again, so its completion keys to a fresh commit, and strips the four keys the CLI stamps"
category: legion
tags:
  - handoff
  - handoff-complete
  - HANDOFF_ALREADY_RECORDED
  - forward-merge
  - legion-handoff-write
date: 2026-10-09
status: active
module: packages/daemon/cmd/legion
symptoms:
  - "legion handoff complete: 409 HANDOFF_ALREADY_RECORDED: the implementer completion of phase implementing (round N) at commit <sha> was already received; this call changed nothing"
  - "legion handoff write: data carries issue, which this command writes itself (schemaVersion, phase, completed and issue); send only the phase's own fields"
applies_when:
  - An implementing round changes no code (a conflict-forced forward merge whose unchanged-diff fingerprint did not move, a round sent back by a withdrawn READY)
  - You rebuild a phase's handoff from the file already on disk instead of re-emitting the whole payload
related_issues:
  - "LEGION-629"
  - "sjawhar/legion#1848"
---

# A round that changes no code writes its handoff again, so its completion keys to a fresh commit, and strips the four keys the CLI stamps

- A completion of a file-backed phase reports the last commit on the issue branch that changed
  `.legion/<issue>/<phase>.json` (`handoffCommit`, `packages/daemon/cmd/legion/handoff.go`), and the
  daemon deduplicates completions on the tree generation, the issue generation, the role, the
  phase, the review round and that commit (`packages/daemon/internal/api/handoff.go`, the event id
  above `HANDOFF_ALREADY_RECORDED`). The round counter moves only on a rejected round, so a round
  that merges `main` and changes nothing else, completed without a new handoff, reports the previous
  round's handoff commit and is refused with 409 `HANDOFF_ALREADY_RECORDED`. Write and commit the
  phase's handoff on every such round: merge commit → `jj new` → `handoff_write` → `jj split -m
  "<phase>: record handoff" .legion/<issue>/<phase>.json` → one `legion push` carrying both. When
  retro ran before the round, the new handoff commit sits above retro's commits.
- When the fingerprint is unchanged the handoff's `proof` entries stay at the heads they ran at;
  the round goes into `deviations` and `filesChanged` (what moved, the one conflict and how it was
  resolved, the fingerprint at both heads, the checks run at the merge), and the rebase re-check
  goes into the PR body's `E2E` line.
- Rebuilding the handoff from the file on disk: `legion handoff write` stamps `schemaVersion`,
  `phase`, `completed` and `issue` itself and refuses data carrying any of them
  (`cmd/legion/handoff.go`, the `reserved` loop), so the pipe deletes all four —
  `delete h.schemaVersion; delete h.phase; delete h.completed; delete h.issue;` — before
  `console.log(JSON.stringify(h))`. The refusal names the key it found. A snippet that deletes
  three (the worker skill's as of pi-legion 8.4.1) fails on every handoff written after
  dispatch://LEGION-565 stamped `issue`; when the CLI's reserved list grows, the sweep in
  docs/solutions/daemon/handoff-schema-migration-patterns.md ("Search for `handoff_write` across the
  role prompts and `skills/legion-worker/SKILL.md`") covers that snippet too.

## Evidence

sjawhar/legion#1848 (LEGION-629). Conflict round 1 (merge 137de544, 2026-10-08): the implementer
completed without a new handoff and the daemon answered `HANDOFF_ALREADY_RECORDED` at the previous
handoff commit 6d8c298f; on the architect's direction it wrote `implement.json` again above retro's
commit and completed at the new handoff commit 1420735884db (`implement.json` `deviations`, "Conflict
round"). Conflict round 2 (merge 66c26bbc, 2026-10-09): the handoff was rewritten first, from the
file on disk through `bun -e … | legion handoff write --phase implement`; the first attempt, with the
worker skill's three `delete`s, was refused `data carries issue, which this command writes itself
(schemaVersion, phase, completed and issue)` and the file on disk was left untouched; the second,
with `delete h.issue` added, wrote it (`deviations` 10 → 11, `filesChanged` 20 → 21, `proof`
unchanged at 12), `jj split` made 2484acc9, one `legion push` carried the merge and the handoff, and
the completion was accepted. The CLI's reserved list is
`[]string{"schemaVersion", "phase", "completed", "issue"}` at `cmd/legion/handoff.go:120` of that
head; the skill snippet at `skills/legion-worker/SKILL.md:355` (and the installed 8.3.1 copy) deletes
three.
