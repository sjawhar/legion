---
title: "Re-send chains are keyed by the message, not the transport's event id; a late receipt is receipt_timeout, not delivery_failed — and the stopgap that said otherwise was superseded on purpose"
category: daemon
tags:
  - envoy
  - role-topics
  - exception-lane
  - resend-ledger
  - dedupe-key
  - receipt_timeout
  - delivery_failed
  - worker-queued
  - handleException
  - supersession
  - ttl
  - claim-settle
date: 2026-09-15
status: active
module: packages/daemon
problem_type: correctness
severity: high
related_issues:
  - "LEGION-107"
  - "sjawhar/legion#1085"
  - "sjawhar/legion#1120"
  - "LEGION-101"
  - "LEGION-103"
  - "sjawhar/legion#1053"
  - "sjawhar/legion#1057"
  - "LEGION-108"
  - "LEGION-109"
symptoms:
  - "one role message reaches a busy architect every few seconds, each copy with a fresh Envoy event id, until the holder happens to acknowledge quickly (LEGION-101: 28 copies in two minutes)"
  - "eight `worker-queued` notices for one queued daemon catch-up (LEGION-60's evidence item 3)"
  - "a re-send cap that passes its unit test and bounds nothing in production"
  - "the daemon journal shows `re-sending event … attempt 1 of 3` one second after `re-send cap reached` for the same dedupe key; 83 re-sends against 11 cap lines in five hours"
  - "every exception for one message is logged `arrived while its re-send is pending; dropped without counting` and nothing is ever re-sent"
applies_when:
  - Adding any counter, budget, or dedupe keyed on a message that Envoy re-publishes
  - Touching `ProcessManager.handleException`, `resendToRootArchitect`, or `WorkerAdmission.publishQueued`
  - A stopgap for the same symptom has landed on main while the contract-driven fix was being planned
  - Any code between a ledger's `claim()` and its `settle()` gains a new early return or a call that can throw
---

# Re-send chains are keyed by the message, and a late receipt is `receipt_timeout`

## What was wrong (LEGION-101, 2026-09-13)

The listener forwards a role message to its holder and waits two seconds for a receipt. A busy
holder (a model turn in progress) misses that window; the listener reported the miss as
`delivery_failed`, the same reason as "holder stale, never forwarded". The daemon's
`handleException` then probed the architect's pane — alive — and re-sent the original at once,
unbounded and unrecognisable to the receiver: the listener mints a fresh `event_id` and a fresh
dedupe key for every publish. Each copy was another two-second window the busy holder missed.
For a worker, every exception queued a daemon catch-up and told the architect `worker-queued`
again. One human message reached one architect 28 times.

## The three rules LEGION-107 put into the daemon

### 1. A re-send budget is keyed by the message, never by the event id

`ResendLedger` (`resend-ledger.ts`) is keyed by `resendChainKey(original) = topic + "\n" + payload`
(`processes.ts`). The exception the listener publishes names the **failed copy's** event id
(`publishDeliveryException`, `delivery.go`), and every re-published copy gets a new one
(`messageEnvelope`, `api.go`). A ledger keyed by `event_id` therefore counts to exactly 1 per
chain in production — every exception starts a "new" chain — while its unit test, which feeds the
same id four times, passes. The planner caught this by reading the listener (`plan.json` decision
D1, then the LEGION-101 spec was edited to match); the lesson generalises: **when the transport
re-mints identity per copy, key your budget on what the caller controls and re-sends verbatim.**

Cost of the key, documented at `resendChainKey`: two byte-identical messages to one role whose
deliveries both fail within `RESEND_LEDGER_TTL_MS` (5 min) share one budget of three. Each re-send
still carries its own exception's dedupe key, so the plugin's dedupe never conflates them. The spec
rejected "de-duplicate by payload at the receiver" for exactly the opposite reason — two legitimate
identical wakes would be dropped — so the payload is the *budget* key, never the *delivery* key.

### 2. `receipt_timeout` means slow, not gone; `delivery_failed` keeps its meaning

Once the listener (LEGION-108) classifies "forwarded to a live holder, no receipt in the window" as
`receipt_timeout`, `handleException` treats that reason on a process that probes alive (root pane;
worker's cached shim client or its pane, `workerAlive`) as delivered: one log line naming the role
token and event id, nothing re-sent, nothing queued. A dead process takes the existing path for
every reason. `delivery_failed` and `no_holder` on an alive root architect keep the
`reclaim-architect` directive but re-send through the ledger: at most `MAX_RESENDS` (3), each after
its pause (`RESEND_PAUSES_MS` = 5 s, 15 s, 45 s — the first included, so a busy holder's turn can
end), the root re-probed after the pause (dead → resurrect, never a directive it cannot ack), an
exception during a pending pause dropped uncounted (it is the same failure, not the pending copy's),
one `re-send cap reached` line (and, since #1120, the entry kept until the TTL so later reports are
silent — see the addendum), the triggering exception's `dedupe_key` on every copy through
`publishRole → envoyPublish → envoyPublishBody`. The directive JSON is unchanged (`{topic, payload,
eventId}` destructured — assigning `exception.original` whole would leak `dedupeKey` into it, and
TypeScript's excess-property check does not fire on a variable).

### 3. `worker-queued` has one gate

`WorkerAdmission.publishQueued(newlyQueued, …, pending)` is the only call that publishes
`worker-queued`: once, when the token joins the queue (`enqueueClaimForLaunch`/`enqueueIdleWorker`
return whether it did), and only for `pending.kind === "assignment"`. A daemon catch-up is queued
silently — the architect did not ask for it and cannot act on it. `launchOrQueue`,
`queueUnstartedPrompt`, and `resumeOrQueueExisting` all route through it, so the invariant is
auditable in one place (fresh-eyes retro observation). When fixing a duplicate-notification bug,
build the gate as one function every path calls, not a condition copied into each path.

## The stopgap this supersedes, and why it had to go

Between LEGION-107's planning and its implementation, LEGION-103 (#1053, #1057) landed on `main`
inside the same `handleException`: every `delivery_failed` on an alive holder was read as a late
receipt and dropped (logged once per role per 30 min), and `no_holder` republishes were capped per
`(role token, payload)` per 30 min while the reclaim directive still went out every time. It
stopped the bleeding on the day. It contradicts the LEGION-101 contract, and the implementer's
first push came back `CONFLICTING` because of it.

Why the ledger wins, not both: with LEGION-108's listener deployed, `delivery_failed` means the
message was **never forwarded** (holder stale, lookup failed, publish error) — exactly the lost
message the reclaim re-send exists to recover (the LEGION-29 listener-restart case the spec's
Rejected list names). A blanket "ignore `delivery_failed` on an alive holder" would drop those.
The late-receipt case LEGION-103 guarded against becomes `receipt_timeout`, which the new branch
already ignores for a live process. In the deploy window before LEGION-108 lands, a slow holder's
`delivery_failed` yields up to three paused, dedupe-keyed copies instead of LEGION-103's zero —
the root spec's Design accepted that ("a new daemon with an old listener … is bounded by the cap"),
and the operator deploys the three parts together. The architect confirmed: do not keep the guard
on either side.

How it was removed: `MAX_ROLE_REDELIVERIES`, `ROLE_REDELIVERY_PERIOD_MS`, `roleRedeliveries`,
`noteLateReceipt`, `countRoleRedelivery`, `countRedelivery`, and exactly its three tests, in the
conflict-forced rebase; `handleException`'s doc comment names LEGION-103 and says why it is
superseded, so a reader of the code does not take the deletion for a lost merge; the PR body says
the same. **Name a superseded stopgap in the code that replaces it** (fresh-eyes retro
observation) — a reviewer can then verify the newer mechanism covers the same cases instead of
suspecting a bad rebase. The process side of the same event is in
`../legion/a-stopgap-that-lands-on-main-mid-plan-is-superseded-not-merged.md`.

## Addendum (#1120, 2026-09-15): the cap is judged against the message's whole failure stream

### What production showed the day after #1085 deployed

The operator restarted the daemon at 01:04Z on 2de4aec6. By 06:04Z the journal held **83
`re-sending event` lines against 11 `re-send cap reached` lines** — about 7.5 copies per capped
message, not at most three — and at 06:03:51 → 06:03:52 one message restarted at `attempt 1 of 3`
one second after its own cap line (LEGION-101 comment 53813157 and its production-hour artifact;
the implementer's own production check on #1085 had flagged the same three post-cap restarts as
"a design consequence for LEGION-101" before the finding was ruled a defect).

The mechanism: a **second, older listener** (a laptop still on the pre-LEGION-108 build) reported
the same message failed 4–16 s after the local listener had already delivered it. `ResendLedger`
dropped its entry the moment the cap was reached, so that late report looked like a brand-new
message and started a fresh three-copy chain. Every part of the code did what the spec said: the
spec's first version — its own Errors row read "the ledger entry is dropped" — was the defect, and
the implementation reproduced it faithfully. **A cap bounds a message's whole failure stream, not
one chain of three.** When a stream of reports about one message can come from more than one
reporter, "three then forget" is not a cap; it is a rate.

### The rule now (spec version 7)

A capped entry is **kept** until `RESEND_LEDGER_TTL_MS`; `claim()` answers `capped` for every later
report inside the window, refreshes the entry's clock on each, and the caller does nothing — no
pause, no probe, no directive, no publish, and no new log line. A message that keeps failing
therefore stays silent rather than restarting every window; only a whole quiet window forgets it.
The one cap line fires on the transition: `ResendEntry.capped` is set on the first `capped`
answer and `claim()` returns `justCapped` so the caller can tell. **Encode "have I already said
this" as a stored flag, never as "does the entry still exist"** — the old ledger could tell first-cap
from already-capped only by absence, which is exactly what let the late report look new
(fresh-eyes retro observation).

### The review finding this fix introduced, and the discipline that closes it

To make a capped message cost nothing, #1120 moved `resendLedger.claim()` ahead of the pre-pause
liveness probe in `resendToRootArchitect`. `claim()` marks the entry in flight; the probe can
throw (a `tmux list-panes` that proves nothing — `server not responding` — is rethrown by
`probeLocator`). The round-1 review reproduced the consequence: the throw propagated to
`handleException`'s catch, nothing settled the entry, and every later failure of that message was
dropped as `in-flight` with the TTL refreshed — the message silenced for as long as it kept
failing, and the log line misstating the state.

Fix: `try { alive = … probe … } catch (error) { resendLedger.settle(key); throw error; }` —
settle-and-rethrow, so the existing catch still logs once and the chain stays claimable. **Every
exit between `claim()` and `settle()` must settle**, including thrown ones, at the boundary where
the failable call sits. Audit as landed: the pre-pause probe throw (was unsettled, now settled);
the pre-pause dead branch (settled before `resurrect`); the pause never rejects and the
disposed/superseded return is covered by `dispose()` clearing the ledger; the post-pause probe,
`resurrect`, and `controlDirective` all run *after* the post-pause `settle()`. That last point is
the fragile one: the post-pause probe is safe only by ordering — a refactor that moves `settle()`
below it "for symmetry", or inserts a failable step between them, reintroduces the bug and no test
exercises a post-pause throw (fresh-eyes retro observation). The comment above the try/catch
states the rule; keep it beside the code.

The regression lock's negative control: after the settled throw the next exception is **attempt 2
of 3 with a 15 s pause**, not attempt 1 — `settle()` makes the chain claimable but refunds nothing,
so the budget still bounds the chain. A test that pins "the failure line appears once" alone would
pass a fix that also reset the count; pin the attempt number too.

### Deploy-order consequence, for the record

With a pre-LEGION-108 listener still answering every role publish with `delivery_failed`, an alive
architect sees at most three copies per message per five-minute window (was: three per ~65–125 s
cycle under #1085), and the local LEGION-108 listener's dedupe by `dedupe_key` absorbs them, so no
paid duplicate turn results today — which is why this landed as tier T3. The chain lines and the
2026-09-14 23:10–23:49Z storm (a clean one-send / +5 s / +20 s / quiet cadence against old laptop
listeners until the operator upgraded them at 23:48:53Z) are recorded on LEGION-101.

### What #1120's tests lock

- `resend-ledger.test.ts`: "claims three attempts … then reports capped and keeps the entry capped
  until the TTL elapses" (fifth claim `capped` inside the window; `resend` only after a whole quiet
  window) and "a capped claim refreshes the TTL clock, so a message that keeps failing stays silent
  instead of restarting" (four capped claims across two windows never expire).
- `processes.test.ts`: "later delivery_failed exceptions for a capped message re-send nothing, arm
  no pause, send no directive, and log no new line until the ledger TTL elapses" (fifth and sixth
  exceptions on a mutable `now`: `clock.pending` empty, publications, directives, `console.error`
  count, and `list-panes` count all unchanged; a seventh after `RESEND_LEDGER_TTL_MS` arms a fresh
  5 s pause); P14's negative control rewritten to the same contract; and "a pre-pause probe that
  throws settles the ledger entry: the throw is logged once and the next exception for the same
  message is a normal attempt, not dropped as in-flight" (red before the fix: timed out on the
  never-armed pause).

## What the tests lock, and how

- `resend-ledger.test.ts`: attempts 1/2/3 with pauses 5/15/45 s, `capped` on the fourth **and kept**
  — a fifth claim inside the TTL is `capped` again, a fresh chain only after a whole quiet window
  (rewritten in #1120; see the addendum above), a capped claim refreshes the TTL clock,
  `in-flight` uncounted until `settle`, TTL forgets an idle entry but keeps a fresh one,
  independent counts per key.
- `processes.test.ts` P1–P9 and the two-variant P14 integration test (see
  `../testing/model-the-receivers-dedupe-in-the-test-when-a-rig-is-forbidden.md`): P7 proves an
  in-flight drop is uncounted by asserting the *next* pause is 15 s, not 45 s; P8 a root dying
  during the pause is resurrected, not directed; P9 `dispose()` cancels a pending pause; P14 the
  directive JSON never contains `dedupeKey`.
- `events.test.ts`: `receipt_timeout` parses with the payload's `dedupe_key`; an unknown reason
  reaches no recovery (the allow-list is the negative control).
- `index.test.ts`: `envoyPublishBody` carries `dedupe_key` only when a key is given — the
  wire-shape proof without a listener.
- P10–P13: one `worker-queued` per enqueue, none for a catch-up, none for a second task on an
  already-queued role.

Two existing tests (`reclaims a live root architect…`, `reports a reclaim-architect nack…`) needed
`sleep: async () => {}` injected: with a real timer they now wait the 5 s first pause.

## Open fast-follow (recorded, not fixed here)

Review pullrequestreview-5195677076 on #1085 batched five wording/duplication nits as the named
fast-follow **`resend-hardening-and-wording`**, one PR after the merge, no behaviour change:

1. `resendToRootArchitect`: an `if (this.disposed) return;` before `resendLedger.claim()` (the
   `armIdleRetire` convention) — a handler sitting in its pre-ledger probe when `dispose()` runs
   otherwise arms one 5 s pause that sends nothing (≤5 s extra shutdown latency, no output).
2. `AGENTS.md`'s `resend-ledger.ts` row says "stores and logs no payload"; the key *is*
   topic + verbatim payload — say "stores nothing beyond the key string, never logs a payload" as
   the module doc does.
3. `ResendLedger.settle`'s doc says "once the re-send has fired"; it runs after the pause and
   before the re-probe/`controlDirective` (settling early is the safe choice — a throwing
   `natsRequest` cannot strand the in-flight mark — and the doc should say so).
4. `controlDirective`'s `| false` arm has only a test caller.
5. The four-line queue-join tail is duplicated between `enqueueClaimForLaunch` and
   `enqueueIdleWorker`; P14's fake receiver has an unreachable `keyless-` branch.

## Related

- `role-notices-go-through-the-listener-a-bare-nats-publish-reaches-nobody.md` — why every one of
  these publishes goes through `envoyPublish`, which is also where `dedupe_key` rides.
- `a-prompt-is-delivered-when-the-turn-starts-not-when-the-shim-acknowledges.md` — the same
  "acknowledgement is not delivery" lesson one hop earlier, on the worker socket.
- `one-writer-for-the-active-phase-and-bystander-catchups.md` — why a catch-up is never an
  assignment, which is what lets `publishQueued` stay silent for it.
