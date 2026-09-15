---
title: "A “resume the same agent” guarantee is kept by session identity, and the resume reference must survive a refused generation"
category: daemon
tags:
  - same-agent
  - expectedSessionId
  - resume
  - session-store
  - postgres
  - process-started
  - worker-started
  - boot-token
  - registration-deadline
date: 2026-09-15
status: active
module: packages/daemon/src/daemon/processes.ts (spawnTree, launchWorker), packages/daemon/src/daemon/api/routes/process.ts, packages/daemon/src/daemon/api/routes/workers.ts, packages/daemon/src/daemon/api/http.ts (SAME_AGENT_REFUSAL), packages/pi-envoy/extensions/legion.ts (exitOnRegistrationRefusal)
related_issues:
  - "LEGION-81"
  - "sjawhar/legion#1108"
  - "LEGION-80"
  - "LEGION-31"
---

# A “resume the same agent” guarantee is kept by session identity, and the resume reference must survive a refused generation

## The problem

Legion promises that a relaunched process — a phase worker, a sub-architect, a tree's root
architect — is the *same agent*: `omp --resume=<recorded session>`, never a fresh agent under
the old role token. On a file store that promise had a cheap guard: stat the recorded session
file before launching (tmux on the daemon host; a Kubernetes pod through its init container's
`LEGION_RESUME_SESSION_FILE` check). A missing file fails the launch before Oh My Pi runs.

Under a database session store (`runtime.kubernetes.session_store: postgres`, LEGION-81) the
transcript is a row in Oh My Pi's `omp_session_files` table, keyed by the same path-shaped string
a file would have had. Nothing in the pod path can stat a row. And Oh My Pi does not refuse a
`--resume` of a path with no entries: `SessionManager.#setSessionFile` (fork tag
v18.1.21-sami.20260914-080519) "starts fresh but keeps the requested path" — a **new session id**
at the recorded path, no error frame, exit 0, `ready`. Without a guard, a root whose row is gone
boots as a fresh architect under the old tree with no signal at all: the silent fallback the
setting exists to rule out.

## The rule: identity, not a stat

The guard that works under both stores is the session id the previous generation registered.

1. **Mint the expectation.** Every relaunch that passes `--resume` mints its boot token with the
   recorded session id as `expectedSessionId`. Workers already did (`launchWorker` →
   `mintWorkerBootToken(…, claim.sessionId)`); roots did not — `spawnTree` minted
   `mintBootToken(tree, generation)` with no expectation, and `/process/started` recorded whatever
   `rootSessionId` arrived. LEGION-81 made the root path symmetrical: `spawnTree` reads
   `roles[<tree>-architect].sessionId` exactly as `launchWorker` reads `claim.sessionId`, and
   `mintBootToken` gained the same trailing `expectedSessionId` parameter (`BootToken` in
   `api/auth.ts` gained the same field). One mechanism, the same field name — never a second
   concept for roots.
2. **Refuse at registration, before any write.** `/process/started` checks
   `boot.expectedSessionId !== rootSessionId` after the 403 boot-token validation and before
   `boot.sessionId = rootSessionId`, so the refusal consumes nothing and writes nothing: the
   recorded session and locator stand, and the same token still registers the recorded session.
   `/worker/started` already did this (in memory, and from the claim's persisted
   `expectedSessionId` after a restart). Both throw one string — `SAME_AGENT_REFUSAL` in
   `api/http.ts`, `Worker respawn must resume the same agent session` — so the extension, the
   docs, and an operator's log read the same refusal whichever role it was. It is deliberately
   *not* reworded to be role-neutral: it is already what `main` answered for workers and nothing
   matches on it.
3. **The refused process must exit.** The pi-envoy extension exited only on a 403 at the
   registration routes; a 409 was rethrown out of `session_start`, and an Oh My Pi that kept
   running would sit `Running` unregistered under the boot watchdog — which re-arms on a live
   process — forever, with nothing retiring the role. `exitOnRegistrationRefusal` (one helper for
   `bootstrapRoot` and `bootstrapWorker`) exits on 403 and on **every** 409 those routes answer
   (same-agent, `Stale process generation`/`Stale worker generation`, `TreeClosingError`): none
   changes on retry, and the daemon already holds the decision each one names. A 5xx or a
   transport failure still propagates without exiting. Note the match is on the **status**, not
   the message: a route author who adds a new 409 to `/process/started` or `/worker/started` for
   some unrelated condition makes every such process exit — 409 at those two routes *means* "die
   and let the daemon decide"; use another status for anything that should be retried in place.

For workers the expectation has two carriers with two lifetimes: the in-memory `WorkerBootToken`
(lost on a daemon restart) and the persisted `WorkerRoleClaim.expectedSessionId` beside
`bootTokenHash`, which `/worker/started`'s `else if` branch reads when the mint record is gone. A
change to the in-memory record that forgets the durable mirror passes every test that does not
restart the daemon between mint and registration. Roots have one carrier on purpose (below).

## The half that a reviewer's reproduction caught: the reference must survive the refusal

Steps 1–3 hold for exactly one relaunch unless the resume reference survives a generation that
never registers. `spawnTree` wrote `tree.locator = locator` (the runtime's locator carries no
`ompSessionFile` — only `/process/started` writes one) and then `delete tree.resumeSessionFile`.
A refused root — 409, the extension exits, the pod is `Failed` — never registers, so on the
registration deadline `retireUnconfirmedRoot` → `escalateOrRetryUnconfirmedRoot` → `resurrect` →
`resurrectDeadTree` reads `tree.locator?.ompSessionFile ?? tree.resumeSessionFile` = undefined →
`spawnRoot(tree, true, undefined)` → no `--resume`, **no expectation** → the fresh agent registers,
and `/process/ready` resets `launchFailures`. The silent fallback, one deadline cycle later.

The fix mirrors what `launchWorker` already did for a respawned claim: pin the resumed path onto
the fresh locator before deleting the standalone copy —

```ts
tree.locator = {
  ...locator,
  ...(priorSessionFile !== undefined ? { ompSessionFile: priorSessionFile } : {}),
};
delete tree.resumeSessionFile;
```

Now every retry re-resumes the same path and re-mints the same expectation until
`MAX_LAUNCH_FAILURES` turns the tree `launch-failed` (which drops `resumeSessionFile`, so a
re-admission starts fresh by design); `recordRootExit` keeps the path for a resumed root that
dies before registering; and after a daemon restart between launch and registration the
persisted locator carries the path, `reconnectRoots` re-arms the deadline, and the resurrection
re-mints the recorded session (the in-memory mint is the expectation's only carrier; the
persisted locator is the path's).

**Generalisable:** any "resume the same X" guarantee has two halves — the identity check at
registration, and the resume reference surviving a *refused* attempt. Audit the second by asking
what the recovery path reads after a failed attempt; if it reads a field the failed attempt's
launch cleared and the registration never re-wrote, the guarantee is one relaunch deep.

## The test shape that catches it

Drive the retries through the registration-deadline path itself, not by calling `resurrect()`
twice by hand: the deadline's own retry is what runs in production after a refused root. In
`processes.test.ts` the injected `sleep` gates each armed deadline (one `Promise.withResolvers`
per launched generation, exactly as the existing "resurrects directly, once, when the root
registration deadline elapses" test does), `list-panes` answers every probe gone, and no
`/process/started` is ever called. Assert the whole cycle: the fresh locator carries
`ompSessionFile`; generations 3 and 4 each mint `{ expectedSessionId: <recorded> }` and launch
with `--resume=<the same file>`; deadline 3 ends `launch-failed` with `launchFailures: 3`, no
locator, no `resumeSessionFile`, and **no** generation 5. Watch it fail first: before the pin the
fresh locator has no `ompSessionFile` — the reviewer's reproduction as the assertion it should
satisfy.

Two guards on the expectation itself deserve their own pins. The root expectation is gated on an
actual `--resume` (`priorSessionFile !== undefined`), not on the claim's mere existence: a
`launch-failed` root keeps `roles[architect].sessionId` (only the capability is revoked) while the
re-admit drops `resumeSessionFile`, so minting with that stale session would 409 the fresh root
forever — the negative-control test ("mints no expected session for a launch that resumes
nothing…") defends it. And the extension's 500 negative control pins that a daemon error is
*not* an exit.

## Rejected

- **A row-existence check in the init container** (`legion workspace-init` querying
  `omp_session_files`), mirroring the pvc stat: it would mount the connection string into the
  init container and couple the daemon CLI to Oh My Pi's private table, to detect earlier a
  condition the registration check already refuses loudly and boundedly.
- **Persisting `expectedSessionId` on the root's `spawnCapabilities` record**: `/process/started`
  reads only the in-memory mint and has no persisted-record path, so the field would be written
  and never read. A restart between launch and registration answers 403, the pod exits, and the
  resurrection re-mints from the pinned locator — the check is re-derived, never dropped.
