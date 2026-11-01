---
title: "Proving a pure refactor behind an interface: grep gates over the old symbols, a parameterized contract suite, a fake-runtime lifecycle, and pinned argv"
category: architecture-patterns
tags:
  - refactor
  - interface-extraction
  - grep-gate
  - contract-test
  - fake-implementation
  - pinned-argv
  - discriminated-union
  - zod-refine
date: 2026-09-12
status: active
module: packages/daemon
related_issues:
  - "LEGION-21"
  - "sjawhar/legion#962"
---

# Proving a Pure Refactor Behind an Interface

## Context

LEGION-21 extracted every tmux operation from `ProcessManager` (`packages/daemon/src/daemon/
processes.ts`) into a `Runtime` interface (`runtime.ts`: `spawn`, `probe`, `connect`, `stop`,
`reconcileOrphans` over a discriminated `Locator` union) with `TmuxRuntime` (`runtime-tmux.ts`)
as its only implementation, so that a Kubernetes runtime can plug in without touching the
manager. The spec's headline requirement was "tmux behaviour byte-for-byte unchanged", with one
enumerated exception. This document records what proved that, what the proofs missed, and what
the next interface extraction should set up on day one.

## What proved the boundary holds

1. **A source gate over the old implementation's identifiers, pinned as a test.** The plan
   listed the eleven symbols that had to vanish from `processes.ts` (`tmux.`, `prepareSocket`,
   `writePaneSecret`, `launchShimmedProcess`, `workerClient(`, `recordedWindowId`,
   `rewriteIssueWindowId`, `probedWindowId`, `reconcileTmuxWindows`, `connectWorkerRpc`,
   `readProcessCmdline`) plus the regex `/locator\.runtime|runtime ===/` that would betray a
   runtime branch. `runtime.contract.test.ts` reads the source and asserts zero matches; every
   phase (plan, implement, test, review) re-ran the same grep. Type-checking alone does not give
   this: a cast, an `in` narrowing, or a re-export keeps the types happy while crossing the seam.
   Comments count — a comment that still names `reconcileTmuxWindows` matches the grep, and the
   right response is to rewrite the comment to describe current behaviour, not to exempt it.
2. **A contract suite parameterized over both implementations.** The same cases run against
   `TmuxRuntime` with a fake `run` and against `FakeRuntime` (`__tests__/fake-runtime.ts`, an
   in-memory process table whose locators are the *other* union member). A second real
   implementation plugs in by adding one row.
3. **The manager's lifecycle driven end to end through the fake.** `spawnRoot → probe →
   confirmRootReady → closeTree → reconcileOrphans` over `FakeRuntime` with kubernetes-shaped
   locators, asserting the manager issued no tmux command of its own. Any manager path that still
   assumed a pane fails here. This test also corrected the author's own expectation: `closeTree`
   passes `{ skipGraceful: false }` explicitly, and the assertion had guessed `undefined`.
4. **Byte identity through pinned argv.** The pre-existing tests that pin the full
   `new-window`/`split-window` argv (every `-e KEY=VALUE`, the secret's `*_FILE` last, the shim
   command) were left untouched and stayed green across the cutover and across two later
   deduplication commits. The tester added the live-surface half: a normalized diff of the real
   daemon's tmux argv across rounds (`r4 == r3 == r2` for all four spawn kinds). Together they
   turn "unchanged" from a claim into a comparison.
5. **The one non-identity, named everywhere.** `spawnRoot`'s persist-failure reap became
   `kill-pane` instead of `kill-window` (the window also holds live sibling workers). The spec,
   the plan, the PR body, and the test change all call it the only behavioural narrowing. A pure
   refactor's PR should enumerate every non-identity this way so a reviewer can verify "and
   nothing else".

## What the proofs missed, and the gate that would have caught each

- **Renamed-but-still-flavoured identifiers.** `panePath`, `paneSecretFiles`, `holdPaneSecret`,
  `removeTreeWindow` survived the grep (they were not on the list) and cost a hardening round.
  The identifier-level vocabulary assertion now in the contract suite (`ProcessManager's members
  carry no tmux vocabulary`) belongs in the first commit, alongside the symbol list.
- **The test helpers.** The gate inspects `processes.ts`; the helpers that construct the runtime
  in tests drifted from production and only CI's live-tmux lane noticed —
  `docs/solutions/testing/fixtures-derive-what-production-derives.md`.
- **A new discriminant value without a policy at every call site.** `ProbeResult.status` gained
  `unknown` (no tmux producer; reserved for Kubernetes). `ProcessManager.probe` threw on it, but
  `controllerAlive` treated it as `dead` and deleted `controllerLocator` — which would spawn a
  second controller beside a possibly-live one. Rule: when an interface introduces a value nothing
  produces yet, every consumer needs an explicit branch (here: throw naming the tree or the
  controller), and a `FakeRuntime` returning that value is the test.
- **Strictness lost by widening to a union.** Persisted worker locators had required
  `tmuxPaneId` and `socketPath`; the union made both optional for every member. A `superRefine` on
  `WorkerRoleClaim` restored the load-time refusal (`worker claim <issue>/<role> has a tmux
  locator without <field>`), verified against a copy of the live v23 state (47 locators, 30 worker
  claims load; one with `socketPath` removed is refused by name). Whenever a schema is widened
  for a new variant, ask which invariants the old narrow schema was carrying.

## Setup for the next extraction

Before the first cutover commit: the forbidden-symbol list and vocabulary regex as a test; the
fake implementation with locators of the other variant; the existing argv pins identified as the
identity proof; and the non-identities enumerated in the spec. The type-only fixture sweep that
a new discriminant forces (`runtime: "tmux"` on every locator literal in tests, because `tsc`
includes them) is bulk work — plan it as a population sweep
(`docs/solutions/testing/argv-format-change-is-a-fixture-population-sweep.md`), not as
incidental edits.

## Related

- `packages/daemon/src/daemon/AGENTS.md`, the `runtime.ts` / `runtime-tmux.ts` / `processes.ts`
  rows: the boundary as the current contract.
- `docs/solutions/testing/mutation-proof-probe-tests.md`: proving a probe distinguishes signal
  from noise, the analogous discipline for a single check rather than a boundary.
