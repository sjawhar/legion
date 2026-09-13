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
  - "LEGION-27"
  - "sjawhar/legion#981"
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

## It happened again the same day (LEGION-27, #981)

The pane-identity branch had bumped v24 -> v25; #993 (LEGION-33) took v25, so it moved to
v25 -> v26; then #991 -- this note's own case -- landed its v25 -> v26 and the branch moved to
`migrateV26State` -> v27, four hours later. Each time: keep main's step verbatim, renumber, and add
the one row that decides it -- a file written at main's current version under main's meaning (here
a v26 file with `{ kind, task }` pendings and identity-less locators) loads through exactly the
branch's step, with `.vN.bak` and every other field byte-identical. The number is not the branch's
to hold; check it at every rebase, not only when GitHub says CONFLICTING.

## Before every push of a schema-bump branch

Look at main's version, not at your own: `jj -R <ws> file show -r main@origin
packages/daemon/src/daemon/legion-state.ts | grep -n 'z.literal('`. If it moved past what your
branch consumes, renumber before pushing rather than after the tester finds `CONFLICTING`. A
schema bump is the one change that cannot be conflict-resolved by picking a side.

## The same collision, three times on one branch, and two neighbours of the same shape (LEGION-20)

LEGION-20's design-gate migration claimed v25 first; LEGION-33 (#993) took v25 on main during
review, so it became v26; LEGION-37 (#991, this document's own issue) took v26 on main during
the retro, so it became v27; then LEGION-27 (#981) took v27 while the merger was waiting to
publish READY, so it became v28 — three renumbers in one review cycle, each found only when
GitHub flipped the PR to CONFLICTING. The loop ends only when the queue orders the schema-bump
branch ahead of its neighbours; every wait for re-approval otherwise hands the next number to
whoever merges first. Three things from that branch that this checklist does not
already say:

- **Renumber inside the commit that introduced the number**, with `jj squash --into <that
  change>`, so no commit on the branch declares a version it does not implement and the reviewer
  can still read the chain commit by commit. A squash into a mid-chain commit conflicts with every
  later commit that touched the same lines; if that happens, `jj undo` once and commit the
  wording-only sweep at the head instead.
- **A migration that reads an external system must run exactly once**, so the renumber has to
  keep that property: `loadState` writes the migrated state right after the `.bak`, and the test
  reloads the file with a resolver that throws.
- **`LEGION_DAEMON_API_VERSION` and the plugin manifest's `legion.daemonApiVersion` are the same
  kind of serial**, and Dispatch's `0025_…` SQL migration numbers are too. Two branches that each
  change a `LegionDaemonApi` shape will both bump 1 → 2; the check before every push is the same
  `file show -r main@origin … | grep` on each counter your branch claims. See
  `../daemon/plugin-daemon-api-contract-version-gate.md`.

When the collision arrives *after* the reviewer's approval is pinned to a head, resolve it with a
merge commit on top of the approved head (`jj new <branch head> main@origin`, resolve, describe)
rather than a rebase, so the approved SHAs stay real —
`long-lived-branch-mechanics-jj-new-and-merge-not-rebase.md` §2 — and say what the merge
renumbered in the PR body's "Merge with main" note.

## Not every serial renumbers: the daemon/plugin contract integer (LEGION-52)

The bullet above says `LEGION_DAEMON_API_VERSION` is "the same kind of serial" — for the *check*,
yes; for the *remedy*, no. LEGION-52 (#1018) bumped it 1 → 2 so a daemon would refuse every
plugin release without the credential file; LEGION-20 (#975) took 2 on `main` 52 minutes after
the first handoff, and release 1.23.0 — the first to declare 2 — already wrote the credential
file. A migration version keys a step, so two branches cannot share one and the second renumbers;
a contract integer only has to differ from what every incompatible release declares, so one bump
satisfied both branches. Bumping to 3 would have refused 1.23.0 for no behaviour change. The
branch dropped its bump on rebase (a clean one — GitHub never said CONFLICTING) and landed the
widened rule and the history as a docs PR. The check is the same `file show -r main@origin …
| grep` at every phase boundary; the decision differs: for a contract integer, look at what the
release declaring the new number already contains before renumbering. Detail in
`../daemon/plugin-daemon-api-contract-version-gate.md`, "When another branch takes the number
first".

## Related

- `conflict-only-rebases-keep-the-diff-auditable.md` — the added/removed-line identity check for
  everything *else* in the rebase, and diffing against the true merge base (`main@origin` may
  have moved again by the time you read the diff).
- `../github/conflicting-pr-gets-no-pull-request-ci.md` — why no CI ran on the conflicting head.
- `../testing/live-proof-over-real-daemon-state-snapshots.md` — the `.vN.bak` files `loadState`
  leaves, and how to load them without touching a live daemon.
