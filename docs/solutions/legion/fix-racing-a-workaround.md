---
title: "A code-layer fix that races a test-layer workaround on main: re-verify the plan's premises, remove the workaround, and prove the seam with a test only the seam can pass"
category: legion
tags:
  - legion
  - planner
  - implementer
  - plan-drift
  - main-moved
  - test-isolation
  - workaround-retirement
  - docs-hygiene
date: 2026-09-12
status: active
module: daemon
related_issues:
  - "LEGION-13"
  - "sjawhar/legion#978"
  - "LEGION-22"
  - "sjawhar/legion#967"
symptoms:
  - "The plan's acceptance criteria already pass on main@origin before the implementer changes anything"
  - "plan.json branchState says 'rebase not required'; hours later main touches every file the plan names"
  - "A reviewer finds a second doc still pointing readers at the workaround the PR retired"
---

# A Code-Layer Fix That Races a Test-Layer Workaround

LEGION-13 fixed one CLI test (`legion start --check-config`) that read the ambient `process.env` and so failed
in every Legion pane (`DISPATCH_URL` set, `DISPATCH_TOKEN` unset) while passing in CI. The spec's design was an
injected `env` argument; its Rejected table forbade two alternatives — an `env -u` prefix on `bun test`, and
scrubbing `process.env` in `beforeEach`/`afterEach`. Between the plan and the implementation, another issue's PR
(`sjawhar/legion#967`, LEGION-22) landed exactly the second rejected alternative on the same `describe` block.
The acceptance criteria were therefore green on `main` before the implementer touched a file, and the
requirement they stood for was still unmet.

## 1. A plan's "branch state" is a timestamp, not a promise

`.legion/plan.json` recorded `mainOrigin: 1 commit ahead; its diff touches none of <the four target files>;
rebase not required`. Ten commits later, at implement start, `main@origin` had modified all four
(`jj diff --from <plan base> --to main@origin --stat -- <files>`: index.ts +84, index.test.ts +91, the gotchas
doc +201). Every line number in the plan was stale and the test file had three more tests (18, not 15).

Rule for the implementer's first minute: rebase if the architect asks, then run that `--stat` over the plan's
target files *before* reading the plan's line references. Treat any non-empty output as "the plan's premises
need re-checking", not as noise to rebase through. Rule for the planner: name the files the plan depends on
and the base you measured against, as this one did — that is what made the drift detectable in one command.

## 2. Green acceptance on main does not mean the requirement is met — check the Rejected table

Acceptance 1 and 2 (`bun test` passes in the pane / in the CI shape) passed on `main` because #967's scrub
deleted `LEGION_*`/`DISPATCH_*`/`ENVOY_*` from `process.env` around each case. The requirement — "resolves
configuration from an injectable environment" — was false: `loadStartConfig` still hardcoded `env: process.env`.
The spec's Rejected table is the instrument that tells the two apart: if what already landed is a row in that
table, the requirement is unmet by definition.

The competing workaround then has to go in the same PR. Left in place it is dead code (the command no longer
reads `process.env`), it advertises the pattern the spec rejected as if it were load-bearing, and its comment
("`cmdCheckConfig` resolves against the real process env") becomes false. LEGION-13 removed the scrub, its
comment, and the now-unused `beforeEach`/`afterEach` imports, after publishing the observation to the architect
and getting the go-ahead — a divergence from the plan, recorded in `implement.json` and repeated in the PR body.

## 3. A test only the seam can pass

The two pre-existing tests pass under the scrub *and* under injection; they cannot tell CI which one is in
force. The third test (`resolves Dispatch settings from the injected env, not the process environment`) hands
`cmdCheckConfig` an env with `DISPATCH_URL` and no `DISPATCH_TOKEN` and expects the refusal, then one with both
and expects success — with `process.env` scrubbed or not, only the injected argument can produce that pair.
Every phase re-ran the same revert guard: flip `env,` back to `env: process.env,` in `loadStartConfig` and
confirm exactly one test fails, in the CI shape and with all `DISPATCH_*` unset (17 pass / 1 fail both times;
pane-unprefixed 16 / 2). See
[race-regression-tests-that-fail-before-the-fix](../testing/race-regression-tests-that-fail-before-the-fix.md)
for the discipline; the specific point here is that the guard must fail in the *CI* environment, because the
pane environment is not what protects `main`.

Two signature rules the plan pinned and the review upheld: no `= process.env` default on the injected parameter
(a default lets a test silently fall back to ambient state — the bug itself), and `process.env` is named only in
the citty `start`/`restart` handlers. Commands that only need the legions-registry path (`cmdStop`, `cmdStatus`,
`cmdLegions`) still read `process.env` directly; that is a scope boundary, not an inconsistency — they do not
resolve configuration. `config-resolution-patterns.md` Pattern 6 carries the current signature.

## 4. Retiring a documented workaround means retiring every pointer to it

The plan said "delete § 2 of `worker-pane-shell-gotchas.md`" (the `env -u` recipe). By implement time that
section held three paragraphs from three issues; deleting it would have removed LEGION-29's and LEGION-14's
notes. It was rewritten, not deleted. The reviewer then found `live-proof-over-real-daemon-state-snapshots.md`
still telling testers the gotchas note held "the `env -u … prefix for bun test`" — a pointer to guidance that no
longer existed.

Before committing a workaround's retirement: `grep -rn '<incantation>' docs/` for the recipe itself *and*
grep for the section's title or path for cross-references. Dated plan records under `docs/plans/` that quote
the old recipe are history and stay as they are; a `docs/solutions/` note that *directs* a reader to it is a
live instruction and must change. Give the rewritten paragraph its provenance (issue and PR) in the same
commit — the review's other thread was exactly that omission.
