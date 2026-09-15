---
title: "The consumer side of a probe marker: require it only under the setting that needs it, and cache the confirmation, not just the pass"
category: daemon
tags:
  - worker-image-probe
  - probe-image
  - session-storage
  - SESSION_STORAGE_PROBE_MARK
  - image-probe-cache
  - postgres
date: 2026-09-15
status: active
module: packages/daemon/src/daemon/worker-image-probe.ts (judgeProbeLog, ImageProbeCacheSchema, verifyWorkerImage), packages/daemon/src/daemon/boot-probes.ts (SESSION_STORAGE_PROBE_MARK)
related_issues:
  - "LEGION-81"
  - "sjawhar/legion#1108"
  - "LEGION-80"
  - "LEGION-25"
---

# The consumer side of a probe marker: require it only under the setting that needs it, and cache the confirmation, not just the pass

## Context

`probe-the-build-that-runs-and-mark-the-ok-line-never-a-pin-constant-guard.md` records the
*producer* half: `legion probe-image` runs the session-storage probe inside the worker image and
prints `session-storage=probed` (`SESSION_STORAGE_PROBE_MARK`) on its OK line. LEGION-81 wrote the
*consumer* half in the in-cluster daemon's image probe, and three rules from it generalise to any
"an image must carry capability X before setting Y is safe" gate.

## 1. Require the marker only under the setting that depends on it

`judgeProbeLog` (`worker-image-probe.ts`) checks `logTail.includes(SESSION_STORAGE_PROBE_MARK)`
right after the `daemon-api-version=<N>` check, and refuses — definitively, in the same
`pod <name> Succeeded without printing <marker>, which session_store: postgres requires (its Oh My
Pi or legion CLI predates the session-storage setting) — log tail: …` shape as the contract
refusal — **only when `deps.sessionStore === "postgres"`**. Under `pvc` the marker is not
required. Two reasons: a `pvc` deployment on an older image must keep working, and a tmux
deployment never reaches this code. The dependency is `VerifyWorkerImageDeps.sessionStore: SessionStoreName`
(`index.ts` passes `kubernetes.sessionStore.kind`) — the discriminated union's tag, not the
whole union: the probe does not need the key.

Match with `includes`, never a full-line regex: the OK line's suffix order belongs to the CLI
(`probe-image: OK (<omp>) session-storage=probed daemon-api-version=<N>` on `main`, and two
pull requests had appended to it in different orders), and the existing
`daemon-api-version=(\d+)$` anchor is unaffected because the marker precedes it. Import the
constant; never retype the string.

## 2. Cache the confirmation as its own field, and read absence as "not confirmed"

The probe cache (`<state_dir>/image-probes/<hex>.json`) remembers a pass per (digest,
contract) so a crash-restart loop never launches a second probe pod. A pass alone is not enough
once a mode depends on the marker: a pass cached under `pvc` says nothing about whether the
marker was seen. So `ImageProbeCacheSchema` gained `sessionStorageProbed: z.boolean().optional()`,
written `true` only when the log carried the marker (omitted otherwise), and the reuse rule is

```
cached.daemonApiVersion === daemonApiVersion &&
(deps.sessionStore !== "postgres" || cached.sessionStorageProbed === true)
```

with its own ignore line: `it records no session-storage probe, and this daemon runs
session_store: postgres`. Three consequences to keep intact:

- A pass cached under `pvc` on an image that **did** print the marker *is* reused under
  `postgres` — the marker was confirmed; nothing about the setting changes what the image
  printed. Recording the marker under `pvc` even though it is not required there is what makes
  the later switch free.
- A file written before the field existed, or for an older image, is absent → not confirmed →
  the pod runs again under `postgres`. Absence and `false` are the same state; do not default the
  field on read or coerce it, or an old cache is silently trusted.
- `false` is never written. The field is either `true` or omitted, so a strict schema keeps
  every pre-existing `pvc` cache valid without a migration.

## 3. What was rejected, and why each loses

- **A pin-version check** (`OMP_FORK_PIN` at or past the release with the setting): the daemon's
  own constant says nothing about the Oh My Pi inside a digest-pinned image built from some pin
  at some commit. The producer doc records the same rejection from the other side.
- **Running the host-side probe (`verifySessionStorageSetting`) from the daemon**: under
  `runtime: kubernetes` it would probe the host's Oh My Pi, not the image the pods run — false
  confidence — and its negative answer is an exit code, so a launch-prefix failure (a `secrets`
  denial) classifies as transient and the daemon's *unbounded* boot retry would retry it forever.
  The spec's Rejected table records this; the daemon calls that function nowhere.
- **Persisting the marker in the daemon's state file**: the probe cache already exists per
  digest and contract, is written atomically, and is the thing the reuse decision reads. A second
  record would be two sources of truth for one verdict.

## Tests that pin the contract (and two that were deleted)

Six cases in `worker-image-probe.test.ts`'s `session_store` describe survived review: postgres
passes with the marker and writes the flag; postgres refuses without it (exact message, no
cache, pod deleted); pvc records the marker and that cache is reused under postgres with no API
call; postgres ignores a flagless cache with the exact ignore line, runs the pod, rewrites the
cache with the flag. Two more were deleted at review as implementation pins: "under pvc a cache
written without the marker carries no `sessionStorageProbed` key" (pins key-absence over the
contract, which only says "not `true`"), and "under pvc a flagless cache is reused" (a duplicate
of the pre-existing cache-hit test). A test that pins implementation is deleted, not re-pinned.
