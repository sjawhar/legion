---
title: "A prompt is delivered when the turn starts, not when the shim acknowledges: the receipt pattern, one handler per not-started reason, and the two limits it accepts"
category: daemon
tags:
  - worker-rpc
  - prompt-delivery
  - agent_start
  - PromptReceipt
  - PromptNotStarted
  - worker-admission
  - one-handler-per-event
  - omp-rpc-mode
date: 2026-09-13
status: active
module: packages/daemon
problem_type: correctness
severity: high
related_issues:
  - "LEGION-60"
  - "sjawhar/legion#1030"
  - "LEGION-10"
applies_when:
  - The daemon sends a command to a worker over the shim's RPC and does bookkeeping on the reply
  - A "success" response arrives before the effect it names has happened
  - An operation can fail for two structurally different reasons (the peer is slow, the peer is dead) and two recovery paths could both claim it
---

# A prompt is delivered when the turn starts, not when the shim acknowledges

## What went wrong (LEGION-10)

The daemon handed a queued task to an idle live worker with `client.prompt(task)`, and the moment
the shim answered `{success:true}` it removed the queue entry, cleared `pendingAssignment`, and
wrote `phases[issue]`. The worker never ran the task. Oh My Pi's rpc mode answers `prompt` before
the turn begins (`session.prompt(...)` is started, not awaited, in `rpc-mode.ts`'s `case "prompt"`),
and since fork commit `0e57a6ca` it can accept a message that starts no turn at all. State said
"delivered", the worker sat idle, the architect had been told `worker-queued` and was waiting for a
`worker-started` that never came. Nothing surfaced the loss for ten hours.

The general shape: **a response frame is proof the request was received, never proof of its
effect.** Any daemon bookkeeping that means "the effect happened" must wait for the effect's own
signal.

## The contract (`worker-rpc.ts`, `processes.ts`, `worker-admission.ts`)

- `WorkerRpcClient.prompt()` returns a `PromptReceipt`: `turnStarted` settles on the first
  `agent_start` frame — or a `get_state` answer with `isStreaming: true` — seen on that connection
  after the prompt was written; `hasStarted` is its synchronous view; `abandonWait()` restores the
  client's pre-prompt `runState` silently (no `onIdle`), exactly like a refused prompt. One slot per
  client, armed **before** the request is written, because `agent_start` can beat the
  acknowledgement. A later `prompt()` supersedes the slot; a superseded receipt never settles.
- `ProcessManager.promptExistingWorker` — the one prompt site behind queue promotion, `spawn_worker`'s
  direct prompt, and `/worker/ready` — races the receipt against `worker_rpc_timeout_seconds` on the
  injectable clock (`awaitTurnStart`) and runs the one commit block (`commitPromptDelivery`) only
  on a started turn. A turn that starts after the bound is the same delivery: `commitLateStart`
  commits it once, guarded by session id, locator presence, the task **compared by value**, and the
  tree still alive — never a second prompt.
- No turn within the bound throws `PromptNotStarted` with a `reason`:
  - `"no-turn"` — socket still up, the worker started nothing. Every site treats it like a refused
    prompt: `promptFailures` +1, one log line naming the token and the observation
    (`get_state: isStreaming=false` / `get_state failed: …`), the entry stays queued (the two direct
    paths queue it through `queueUnstartedPrompt`), retried on the next drain, retired at
    `MAX_LAUNCH_FAILURES` so the task relaunches cold with `--resume`. On `/worker/ready` the boot is
    still confirmed — it did succeed.
  - `"socket-closed"` — the shim's socket closed before any turn. That is a death, not a slow
    worker: no site counts it, no late-start continuation is left (nothing more arrives on that
    connection), and the socket-close handler alone retires the worker — `markWorkerDead` for a
    confirmed claim, `retireUnconfirmedBoot` for `/worker/ready`'s unconfirmed boot (locator cleared,
    slot released, `launchFailures` counted, task re-queued and relaunched cold, `worker-started`).

### One event, one handler

Round 1 of review found the bug this rule exists for: `/worker/ready`'s catch treated every
`PromptNotStarted` as "alive but swallowed", confirmed the boot and queued the task — while
`onWorkerClientClosed`, already running on the same `closed` promise, had snapshotted the claim as
*unconfirmed* and routed its dead verdict to `retireUnconfirmedBoot`, which declines a confirmed
claim. Both handlers ran; neither retired the worker. End state: a dead locator kept, `readyConfirmedAt`
set, the task on the claim. At `worker_cap: 1` the dead locator held the only slot forever; at
cap ≥ 2 the next drain dropped the queue entry as stale and orphaned the task.

The fix was not "make the close handler re-read the claim" but a typed discriminant on the error so
each reason has exactly one owner and the other path returns early. When an operation can fail for
two structurally different reasons, encode the reason where it is observed (`awaitTurnStart` knows
whether the socket closed) rather than letting two downstream handlers infer it from ambient state.

### Two subtleties that only show up as wrong counters

- `awaitTurnStart` consults `receipt.hasStarted` **after** the confirming `get_state` however that
  call ended. An `agent_start` that lands while `get_state` is in flight or failing is a started turn;
  returning "not started" from the catch produced a spurious `promptFailures`, a spurious
  `worker-queued`, two persists, and a misleading late-start log line — with a correct end state,
  which is why it slipped to "minor" in review. A refactor that reorders the check reintroduces it.
- The not-started paths never trigger a drain themselves. The plan assumed `/worker/ready`'s
  confirming `get_state` would drive the retry through the client's idle transition; it cannot —
  `onIdle → promoteWorkerQueue` reads `queue[0]` synchronously *before* `queueUnstartedPrompt` has
  pushed the token. All three paths retry on the next drain (an idle/dead event, the 60 s sweep).
  "Helping" a swallowed prompt retry at once would re-widen the window in which a merely slow
  worker receives the task twice.

## The two limits this change accepts (filed for the controller, not fixed here)

1. **Turn attribution is by order.** The first `agent_start` after a prompt is credited to that
   prompt. A turn started by a message another role sent through Envoy in the same window would be
   credited to the daemon's task. Oh My Pi does emit a definitive negative — `prompt_result` with
   `agentInvoked: false`, and a late `success: false` for a busy worker — that the client could
   consume to attribute by id.
2. **No terminal escalation for a worker that always swallows.** Three unanswered prompts retire it;
   the relaunch starts at `launchFailures: 0`; its first ready-time prompt fails again; the cycle
   repeats every few minutes with alternating `worker-queued`/`worker-started` and no `worker-died`.
   The same loop pre-existed for a worker that refuses prompts; this is the missing ceiling.
3. **A slower corner, not a new class.** A ready-time socket close whose shim then *accepts* the
   reconnect (the shim keeps listening while its Oh My Pi child lives) leaves the claim unconfirmed
   with the task on it and a live socket; nothing delivers the task until the boot watchdog's
   registration deadline (about six minutes by default) retires and relaunches. A refused
   ready-time prompt left the identical shape before this change. The parity option is small: on
   `socket-closed`, dial once and, if it connects, fall through to the confirm-and-queue branch.

## Where the words live

`packages/daemon/src/daemon/AGENTS.md`: the `POST /legion/v1/worker/ready` and `/worker/spawn` rows
and the "A prompt to a live worker … is delivered only when the worker's turn is observed to
start" bullet. `skills/legion-architect/SKILL.md`'s `worker-queued` row tells the architect a queued
notice also means "acknowledged without a turn, retrying — wait for `worker-started`".
