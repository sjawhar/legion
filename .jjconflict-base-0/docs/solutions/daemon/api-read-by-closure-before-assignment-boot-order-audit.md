---
title: "A `let api` read by injected closures before it is assigned: the boot-order bug class, the closure audit that finds every instance, and why only a real-boot test can catch it"
category: daemon
tags:
  - boot-ordering
  - dependency-injection
  - closure-audit
  - process-manager
  - launch-hold
  - regression-test
date: 2026-09-13
status: active
module: packages/daemon/src/daemon/index.ts, packages/daemon/src/daemon/processes.ts, packages/daemon/src/daemon/__tests__/index.test.ts
related_issues:
  - "LEGION-11"
  - "sjawhar/legion#955"
  - "LEGION-6"
  - "sjawhar/legion#980"
symptoms:
  - "[legion] worker reconnection failed: TypeError: undefined is not an object (evaluating 'api.revokeSessionCapability')"
  - "[legion] failed to reconcile worker <token>: TypeError: undefined is not an object (evaluating 'api.revokeSessionCapability')"
  - "After a daemon restart a worker that died while the daemon was down keeps its locator in `legion state`, its secrets/<token> file, and its running-worker slot"
---

# A `let api` Read by Injected Closures Before It Is Assigned

## Context

`startDaemonLocked` (`packages/daemon/src/daemon/index.ts`) declares `let api: LegionApi;`,
then builds `new ProcessManager({...})` whose four capability deps are thin forwarders that
read `api` by reference:

```ts
mintControllerCapability: async () => api.mintControllerCapability(),
mintBootToken: (tree, generation) => api.mintBootToken(tree, generation),
mintWorkerBootToken: (...) => api.mintWorkerBootToken(...),
revokeSessionCapability: (sessionId) => api.revokeSessionCapability(sessionId),
```

`api = startLegionApi(...)` is assigned much later in the same function. TypeScript accepts
this: a `let` captured inside a lambda body is not a use-before-assignment error, so nothing
type-checks the order. Before LEGION-11, `processManager.reconnectWorkers()` ran between the
construction and the assignment. A ready-confirmed worker found dead at boot goes
`markWorkerDeadLocked → retireWorkerLocator → revokeRoleClaim → deps.revokeSessionCapability`,
which dereferenced `undefined`. The `try/catch` around `reconnectWorkers` logged
`worker reconnection failed` and boot continued — with the dead worker's locator, secret file,
and slot all retained. The LEGION-6 retro had recorded the crash as an open edge; the comment
above the call claimed the probe "needs no `api` reference", which was the false belief the bug
hid behind.

## The bug class

A deps interface (`ProcessManagerDeps`) whose fields are typed as functions but implemented as
one-line forwarders to an object assigned *after* the consumer is constructed. The consumer is
correct; the assembly is correct; only the *order of first invocation* is wrong, and it is
wrong only on a path that reaches one of the forwarders — here a dead confirmed worker, so a
clean restart never showed it.

## The closure audit

Fixing the crash that fired is not enough: every forwarder is a latent instance. The audit that
found the second one:

1. List every dep that closes over the late-assigned binding (the four above).
2. For each, walk *every* path into it — not just the crashing one. `mintControllerCapability`
   is reached from `ensureController`, which the pending-controller-notice block at boot also
   called before `api` existed. It survived only because `ensureController` awaited a tmux
   probe first and the API happened to bind in the meantime — luck of timing, the same bug
   waiting for a slower disk.
3. Classify each path as: HTTP handler (needs `api` to exist to be reached), NATS callback or
   timer (later event-loop turn), promotion cascade (gated behind `enableLaunches()`), or a
   direct boot call. Only direct boot calls can violate the order; move all of them.
4. State the invariant structurally: the span from `new ProcessManager(...)` to `api = ...`
   contains no top-level `await` (every `await` in it is inside a closure), so no callback can
   interleave. An `awk` over that span for `^  .*await` proves it in one command and belongs in
   the handoff, since every rebase can reintroduce an `await` there.

The audit table (dep → direct caller → every path → reachable before `api`?) went into the plan
and, in plain words, into the PR body: a reviewer can re-derive it, which is what makes a "pure
move" reviewable at all.

## Placement after the move — forced, not chosen

While the PR was open, `main` (#980) introduced the launch hold: boot probes start early and are
awaited at `await probes` just before `enableLaunches()`. Two positions satisfied "after `api`,
before `enableLaunches()`": (a) right after the API and worker-stream listener, before the hold;
(b) after the hold, right before `enableLaunches()`. (a) is the only conflict-only choice: main's
own comment and its `AGENTS.md` bullet say "state load, NATS, the API bind, and the worker
reconnect all proceed while a probe is still retrying", so (b) would silently delay the reconnect
behind the probes — a behaviour change outside the spec. When a rebase offers two placements,
read what `main` says its neighbours are *for*; one of them is usually already ruled out.

## Why `processes.test.ts` structurally cannot catch this

Its `manager()` helper hands `ProcessManager` a `revokeSessionCapability` stub that is valid from
construction. The failure mode — `api` unassigned at call time — does not exist in that harness.
The regression test lives in `index.test.ts` and boots through the real `startDaemon`: a seeded
claim with `readyConfirmedAt`, `sessionId`, and a `runtime: "tmux"` locator; `connectWorkerRpc`
rejecting; the fake runner answering `kill-pane` as pane-gone. It asserts what a consumer sees —
no `TypeError` and no cleanup-failure line in `console.error`, `kill-pane` issued once, locator
gone with `resumeSessionFile` kept in the saved state *and* in `GET /legion/v1/state`, the
secret file removed. It filters the log on `instanceof TypeError` plus the failure-line text, so
it survived #980 renaming the wrapper line from `worker reconnection failed` to
`failed to reconcile worker`. It was red on `main` with the exact production stack before the
move, and stayed red on every later `main` the branch was rebased onto — re-checked each time by
restoring `main`'s `index.ts` into the working copy with the test file kept.

## Rules

1. A deps field that forwards to a late-assigned `let` is a boot-ordering hazard. Audit every
   path into every such field, not only the one that crashed; move all direct boot calls after
   the assignment in one change.
2. Record the ordering invariant as a structural fact (no top-level `await` in the span) next
   to the code and in `AGENTS.md`, and re-verify it after every rebase.
3. Prove ordering bugs through the real assembly (`startDaemon`), never through a harness that
   injects the dep; the harness cannot represent "not yet assigned".
4. Never guard the forwarder (`if (api)`) or defer the revoke — that leaves the dead worker's
   credential valid, which is the bug. A reintroduction must crash loud.

## Related

- `launch-hold-serve-state-spawn-nothing-until-proven.md` — the hold that fixed the position of
  the moved block on the final `main`.
- `refcounted-hold-guards-a-derived-prune.md` — the LEGION-6 retro that first recorded this
  edge; its secret-file prune is what the retirement now reaches.
- `../testing/race-regression-tests-that-fail-before-the-fix.md` — the red-first discipline the
  regression test follows.
- `../legion/conflict-only-rebases-keep-the-diff-auditable.md` — how the block move stayed
  verifiable across four rebases.
