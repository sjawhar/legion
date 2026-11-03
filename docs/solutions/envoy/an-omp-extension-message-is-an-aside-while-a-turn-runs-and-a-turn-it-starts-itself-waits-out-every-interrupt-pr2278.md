---
title: "An Oh My Pi extension's message is an aside while a turn runs, and a turn it starts itself waits out every interrupt"
category: envoy
tags:
  - pi-envoy
  - oh-my-pi
  - aside
  - steer
  - triggerTurn
  - interrupt
date: 2026-10-28
status: active
module: packages/pi-envoy
---

# An Oh My Pi extension's message is an aside while a turn runs, and a turn it starts itself waits out every interrupt

For an extension that sends messages into a live Oh My Pi session (pinned
`v18.12.2-sami.20261026-135627`):

- **While a turn runs, send `deliverAs: "aside"`; on an idle host, `"steer"` with `triggerTurn`.**
  The host takes one steering-queue message per step, first in, first out, so steers sent
  during a run hold a person's Send back one step each, and an Esc empties that queue. An aside
  is folded in at the next step boundary, after any steer.
- **An aside can be folded with no turn, so its sender owes a wake.** The host folds an aside
  that reaches an idle host, or lands as a run settles, while an interrupt's latch is set or
  plan mode is on. Wake with `deliverAs: "nextTurn"`, `triggerTurn: true` and
  `LOCAL_ENVOY_NOTICE` in `details`: on an idle host it starts a turn, and on a host that turned
  busy it waits in the hidden next-turn queue, never ahead of a Send.
- **Decide "already read" from the branch, never from event order.** The extension `agent_end`
  is emitted unawaited, and `session_compact`/`auto_compaction_end` can arrive in either order.
  A message is read when an assistant message that ends a model turn (not a tool call, not an
  error) follows it in `sessionManager.getBranch()`.
- **Never start a turn by yourself while an interrupt is in effect.** An extension sees an
  interrupt only as an assistant message with `stopReason: "aborted"` and `errorMessage`
  `"Interrupted by user"`, `auto_compaction_end` with `aborted: true`, or `auto_retry_end` with
  `finalError` `"Retry cancelled"`; hold the flag until the next `agent_start`. Treat "host busy,
  no `agent_start` since the last `agent_end`" (prompt setup, a `willContinue` gap) as an
  interrupt you cannot see: never wake a message sent in that window. A message that was waiting
  for some other turn obeys the same rule.
- **Track whether a run is active by reading `ctx.isIdle()` at events, never by pairing or
  counting events.** A new or resumed session and a manual compaction disconnect from the agent
  before aborting, so the run's `agent_end` never reaches extensions. Clear "run active" when
  `isIdle()` is true at `session_compact`, `session_switch`, `input` and every delivery.
- **A turn started by itself right after startup is safe only in `ctx.mode === "rpc"`.** Print
  and json mode call `session.prompt` after their own post-startup awaits and throw
  `AgentBusyError` if a turn is running; the terminal subscribes and applies its plan-mode
  default only after startup. Pin each mode with a test.

## Evidence

- `packages/pi-envoy/src/card-wake.ts` (`CardWake.deliveryFor`, `unreadCards`, `endsModelTurn`,
  the interrupt flag, `noteIfIdle`) holds every rule above; `src/card-wake.test.ts` pins them.
- The interrupt rule regressed in three forms across #2278's review rounds: a card sent during
  prompt setup (commit b72b1fd5), a run dropped behind a disconnect (655b7ac0, then 5741f678,
  which replaced an automatic-compaction counter that depended on event order), and a card
  waiting for the controller's start turn that ignored the latch (d7184ba2). Each time the wake
  started the turn the person had stopped.
- The print-mode gate (b72b1fd5) came from a tester run that exited 1 with `AgentBusyError`
  without it and 0 with it.
- `extensions/card-delivery-omp.test.ts` proves the delivery and interrupt rules on the pinned
  binary against a real NATS server.
