---
title: "A branch that bumps the state schema version re-checks main's version at every rebase: when main took the number, renumber, keep main's step verbatim, prove the two compose, and re-prove against a main-written file"
category: legion
tags:
  - jj
  - rebase
  - state-migration
  - legion-state.ts
  - schema-version
  - conflict-resolution
  - long-lived-pr
date: 2026-09-13
status: active
module: packages/daemon
related_issues:
  - "LEGION-37"
  - "sjawhar/legion#991"
  - "sjawhar/legion#1001"
symptoms:
  - "GitHub flips the PR to CONFLICTING right after an unrelated daemon PR merges; `legion-state.ts` and its test are the only conflicted files"
  - "two branches both declare `z.literal(N)` with different `migrateV(N-1)State` bodies"
  - "the deployed daemon's `state.json` is already `version: N` — under the other branch's meaning"
---

# A branch that bumps the state schema version re-checks main's version at every rebase: when main took the number, renumber, keep main's step verbatim, prove the two compose, and re-prove against a main-written file

## Context

#991 changed `WorkerRoleClaim.pendingAssignment` from a string to `{ kind, task }` and declared
state version 25 with a payload-classifying `migrateV24State`. Six review/test rounds later, #1001
(LEGION-33) merged first and took version 25 as a pure bump (`migrateV24State` = `{ ...state,
version: 25 }`; three optional PR fields, absent is correct). GitHub marked #991 CONFLICTING; the
tester's read-only `git merge-tree` showed the two files; and — the part that decides the fix —
production's `state.json` was already `version: 25` under main's meaning, because the production
daemon runs main.

## Why the number cannot be shared or "merged"

A migration step is keyed on the version it consumes: `if (state.version !== 24) return state;`.
Keeping the classifying step at v24 → v25 after main also owns v24 → v25 has one of two outcomes,
both wrong: on a deployed daemon whose file already says 25, the step is *skipped* and the
bare-string `pendingAssignment` values then fail the strict schema at boot with no migration left
to classify them; on a file still at 24, whichever of the two bodies survives the conflict runs
and the other's work is silently lost. Two branches cannot both own a version. The one that lands
second renumbers.

## The renumber, as a checklist

Done in the working copy after `jj rebase -s <first commit> -d main@origin`, then squashed into
the first conflicted commit (`jj squash --from @ --into <it> -u -- <the two files>`), which cleared
the conflict from all fourteen descendants at once:

1. Keep main's step **verbatim** (body, doc comment, position) — its "why a pure bump is fine"
   comment is what the next author audits (`../daemon/webhook-rule-across-listener-and-daemon.md`
   §5).
2. Rename yours `migrateV(N)State`, guard `state.version !== N`, return `version: N+1`; append it
   **after** main's in the `migrations` array (array order is version order). Say in its doc
   comment that it sits after the other step and why it must be the one that reaches N+1.
3. Bump every chain-end marker together: `LegionState.version`, `z.literal(N+1)`,
   `newLegionState`, `loadState`'s `version !== N+1` check.
4. Tests: every title and expectation that names the chain end (`initializes empty vN+1 state`,
   `… through v17 … and vN+1`, `toBe(N+1)`); the backup name your test expects (`.vN.bak`, since
   `loadState` names the backup after the *input* version); your classification tests' input files
   become **version-N files in main's shape carrying the legacy field** — the file a deployed
   daemon actually wrote. Main's own migration test keeps its input at N-1 and now ends at N+1.
5. One composition test: an N-1 file with legacy values → both steps → N+1 objects, `.v(N-1).bak`
   byte-identical. It is the only test that exercises the two steps as a chain.
6. Docs that name the step (`AGENTS.md`, handoff files) follow.

Then tsc, biome, the affected suites, and the guarded-code mutations again — the rebase touched
the files they pin.

## Re-prove the migration on the deployed shape

A rig proof taken before the rebase ("a main-written v24 file → `.v24.bak` + v25 objects") now
proves the *composition* path, not the deployed one. The tester re-takes it against a file the
main checkout wrote at the new pre-version: expect `state.json.vN.bak` byte-identical to the input
and `state.json` at N+1 with the objects, then a normal delivery. Say so in the PR body's
Since-review line, with the reason (production already at N under main's meaning), so the tester
and reviewer know the earlier proof is superseded rather than missing.

## Before every push of a schema-bump branch

Look at main's version, not at your own: `jj -R <ws> file show -r main@origin
packages/daemon/src/daemon/legion-state.ts | grep -n 'z.literal('`. If it moved past what your
branch consumes, renumber before pushing rather than after the tester finds `CONFLICTING`. A
schema bump is the one change that cannot be conflict-resolved by picking a side.

## Related

- `conflict-only-rebases-keep-the-diff-auditable.md` — the added/removed-line identity check for
  everything *else* in the rebase, and diffing against the true merge base (`main@origin` may
  have moved again by the time you read the diff).
- `../github/conflicting-pr-gets-no-pull-request-ci.md` — why no CI ran on the conflicting head.
- `../testing/live-proof-over-real-daemon-state-snapshots.md` — the `.vN.bak` files `loadState`
  leaves, and how to load them without touching a live daemon.
