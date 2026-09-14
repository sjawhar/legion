---
title: "A bounded counter on a relaunched claim: audit every constructor of the record, advance it only after the stop, reset it in one place"
category: daemon
tags:
  - worker-admission
  - prompt-delivery
  - promptRetires
  - promptFailures
  - worker-died
  - launchWorker
  - freshClaim
  - circuit-breaker
  - spec-correction
date: 2026-09-14
status: active
module: packages/daemon/src/daemon/worker-admission.ts, packages/daemon/src/daemon/processes.ts, packages/daemon/src/daemon/legion-state.ts
problem_type: correctness
severity: high
related_issues:
  - "LEGION-93"
  - "sjawhar/legion#1082"
  - "LEGION-60"
  - "LEGION-146"
applies_when:
  - You add a counter, flag, or timestamp to a persisted record that some lifecycle path rebuilds from scratch (a relaunched worker claim, a resurrected tree, a re-registered session)
  - You bound a retry loop that previously ran forever and must end in a verdict another agent acts on
  - A spec or plan states a "base fact" about how an existing counter behaves across a transition
---

# A bounded counter on a relaunched claim

## What this closes

LEGION-60's retro recorded, as an accepted limit, "no terminal escalation for a worker that always
swallows": three acknowledged-but-unstarted prompts retired the pane, the cold relaunch built a
fresh claim with no `promptFailures`, the relaunched pane got three more, and the cycle repeated
every few minutes with alternating `worker-queued`/`worker-started` and never a `worker-died`
(`a-prompt-is-delivered-when-the-turn-starts-not-when-the-shim-acknowledges.md`, limit 2).
LEGION-93 closes it: `WorkerRoleClaim.promptRetires` counts each prompt-failure retirement,
`MAX_PROMPT_RETIRES = 2` bounds it, and the second retirement is terminal — queue entry removed,
`pendingAssignment` kept, exactly one `worker-died {issue, role}` to the tree's architect, no
`worker-queued` for that failure. The architect's next `spawn_worker` is the retry.

## 1. A base-fact correction about a counter must read every constructor of the record

The spec's first "correction" of the filed mechanics said the relaunch *kept* `promptFailures`, so
the relaunched pane would be retired on its first failed prompt. It was wrong. The architect had
read the sites that increment and judge the counter (`recordPromptFailure`,
`markWorkerDeadLocked`) and not the site that *rebuilds the record*: `launchWorker` assigns a brand
new object literal, `freshClaim`, that names `launchFailures`, `generation`, `pendingAssignment`,
`bootTokenHash`, `expectedSessionId`, `locator` — and nothing else. Every field not named there is
dropped by a relaunch. The planner caught it by reading `freshClaim`; had it stood, the design
would have assumed a carried count that never existed, and `promptRetires` — the new field — would
have been dropped the same way, making the bound unreachable.

The rule: before stating how a field behaves across a transition, list every place the record is
**constructed or copied**, not only where the field is read or written. For `WorkerRoleClaim`:

```sh
grep -n 'roles\[token\] = {\|const freshClaim\|WorkerRoleClaim = {\|\.\.\.current' \
  packages/daemon/src/daemon/processes.ts packages/daemon/src/daemon/api/routes/workers.ts
```

`launchWorker`'s `freshClaim` (named fields only — drops), `launchWorker`'s catch
(`{ issue, role, launchFailures }` only when no claim exists — nothing to drop), and
`handleWorkerStarted`'s `...current` spread (keeps everything). A field survives a relaunch only if
`freshClaim` names it, which is why the LEGION-93 diff's one `processes.ts` change the spec's file
list did not foresee is the line
`...(claim?.promptRetires ? { promptRetires: claim.promptRetires } : {})` beside
`launchFailures: claim?.launchFailures ?? 0`. Dropping `promptFailures` there stays deliberate: the
relaunched pane gets the full `MAX_LAUNCH_FAILURES` prompts before it is judged again (a failed
first prompt right after a cold boot can be slowness).

## 2. Decide the verdict once; hand it to the executor as a parameter

`recordPromptFailure` (`worker-admission.ts`) is the one place the verdict is computed:
`retires >= MAX_PROMPT_RETIRES ? "died" : "relaunch"`. It does not perform the retirement — it
calls the injected `WorkerAdmissionDeps.retireDeadClaim(token, locator, verdict)`, which
`ProcessManager` binds to `retirePromptFailedClaim`: `markWorkerDeadLocked` (graceful shutdown,
kill-pane fallback, locator cleared, session file carried), then on `"died"` only
`removeFromQueue` and `publishWorkerDied` unless `isTreeGone`. Widening a decision from two
outcomes to three means walking the DI contract end to end — the dep's type, its one binding in the
constructor, and every test double — a search for "where is the threshold" finds only the deciding
half. `grep -n MAX_PROMPT_RETIRES` must return the constant, the comparison, and the log line, and
nothing else; the reviewer checked exactly that.

Judge the bound with `>=`, not `===`: a claim that already ended in `worker-died` keeps
`promptRetires: 2`, and the architect's re-spawn of the same still-broken agent must escalate again
(one cold launch, three prompts, `worker-died`) rather than skip the bound because the count is
already past it. The log line reads `relaunch cycle <k> (bound 2)` — `(bound N)`, not `of N` — so
cycle 3 after a re-spawn reads honestly.

## 3. Advance the counters only after the stop succeeded

Inside `recordPromptFailure`, `await this.deps.retireDeadClaim(…)` comes first; only then
`claim.promptRetires = retires; claim.promptFailures = 0;` and the log line. A `StopFailed` from
the retirement (the pane would not die, or could not be listed) propagates with the locator, both
counters, and the queue entry untouched — the in-memory `promptFailures` sits at the threshold, the
next drain re-prompts, the next failure retries the retirement without burning a relaunch cycle. An
optimistic write would either spend a retry budget on a stop that did not happen or reach `"died"`
`MAX_PROMPT_RETIRES` times without ever having stopped anything. The queue entry is removed
*after* the stop for the same reason: a live pane must never be left with no queue entry to retry it.

The accepted cost (review thread r4003071266): `markWorkerDeadLocked` persists before the counters
are written and before `removeFromQueue`, so a crash inside that window leaves relaunch-shaped
state on disk — locator cleared, token queued, `promptFailures` 3, `promptRetires` not yet
incremented — and boot's `reconcileWorkerAdmission` relaunches once more before the bound: one
extra bounded cycle on a millisecond crash window, never a loop. Folding the stop-and-clear into
one caller persist, as `retireUnconfirmedBoot` does, would close it if it ever matters.

## 4. One reset point, stated on the field

`promptRetires` is deleted in exactly one place, `commitPromptDelivery` — the shared commit block
for an in-bound started turn and the late start (`commitLateStart`) — beside `promptFailures = 0`.
A relaunch, a `/worker/started` registration, and an architect's `spawn_worker` never touch it
(resetting on a spawn would let an architect drive the unbounded loop by hand; `launchFailures`
never resets on a spawn either). The field's own doc comment in `legion-state.ts` says so, so a
future "looks like recovery" path does not grow a second reset. Note the asymmetry a reviewer
flagged in the `AGENTS.md` wording: `promptFailures` restarts at each retirement and on the
relaunched claim; only `promptRetires` is cleared solely by a started turn.

## 5. A terminal branch silences the loop's status notice

`queueUnstartedPrompt` — the path behind `spawn_worker`'s direct prompt and `/worker/ready`'s
delivery — published `worker-queued` unconditionally ("acknowledged without a turn, retrying; wait
for `worker-started`"). On the `"died"` verdict that notice is a lie right after the verdict, the
LEGION-60 class of bug (an architect waiting for a `worker-started` that never comes), so the
publish is `if (verdict !== "died")`. When a previously unbounded retry loop gains a terminal
branch, audit every publish inside the loop for the terminal iteration. The `spawn_worker` HTTP
answer stays `queued` when its own direct prompt is the escalating failure — `SpawnWorkerResponse`
is part of the daemon API contract — and `worker-died` is the verdict. This guard has no regression
lock yet (LEGION-146 lists the test: ready-time failure, one drain failure, then `spawnWorker` on
the idle relaunched client; expect `{status: "queued"}`, publications exactly
`[worker-started, worker-queued, worker-died]`).

## The terminal claim, for anyone reading state

`{ issue, role, sessionId, generation, readyConfirmedAt, pendingAssignment: <the task>,
promptFailures: 0, promptRetires: 2, resumeSessionFile: <last ompSessionFile>, expectedSessionId?,
bootTokenHash?, agentId? }` — no `locator`, no `launchFailures` (deleted at ready confirmation),
token absent from `state.workerAdmission.queue`, `phases[issue]` untouched. Optional field, no
state version bump, `GET /legion/v1/state` role rows unchanged (adding `promptFailures`/
`promptRetires` there would bump `LEGION_DAEMON_API_VERSION`).

## Left for LEGION-146 (fast-follow, from review and retro)

The six review threads: the missing regression lock above; the `/worker/ready` row's "terminal
one" sentence (a pane's ready-time prompt is always its first prompt failure — `freshClaim` starts
`promptFailures` fresh and the other two prompt sites gate on `readyConfirmedAt`); "both counters"
in the delivery bullet; "acknowledged every prompt" narrowing in the architect skill row and the
field doc (a refused prompt counts too); the persist-window note; test hygiene (`toHaveLength(4)`
pinning log volume, two `toBeUndefined()` pins on omitted keys). From retro: the retire line is a
shared `retireLine()` helper in `processes.test.ts` but a re-typed literal in
`real-prompt-delivery-e2e.test.ts` — export one.

## Related

- `a-prompt-is-delivered-when-the-turn-starts-not-when-the-shim-acknowledges.md` — the receipt
  pattern, `PromptNotStarted` reasons, and limit 2 this change closes.
- `role-notices-go-through-the-listener-a-bare-nats-publish-reaches-nobody.md` — why
  `publishWorkerDied` goes through `publishRole` → the listener, never a bare NATS publish.
- `one-writer-for-the-active-phase-and-bystander-catchups.md` — `commitPromptDelivery` as the one
  writer of `phases[issue]`, the same block that now clears both counters.
- `../legion/mains-test-fixture-change-is-a-conflict-mergeable-does-not-see.md` — how this PR's
  tests met #1027's harness change on the merge ref.
