---
title: "A send during Oh My Pi's startup starts no turn, and a cross-plugin hold is capped"
category: legion
tags:
  - oh-my-pi
  - startup
  - rpc
  - pi-envoy
  - pi-legion
  - plugin-interface
date: 2026-10-28
status: active
module: packages/pi-envoy
---

# A send during Oh My Pi's startup starts no turn, and a cross-plugin hold is capped

Extends docs/solutions/legion/an-oh-my-pi-extensions-own-first-turn-runs-from-a-command-line-slash-word-sent-before-any-inbound-lane-opens-pr2300.md.

- **A message that reaches an extension while Oh My Pi starts must not start a turn.**
  `initializeExtensions` holds every startup send, then drains every tracked send, those started
  while it waits included, and RPC mode reads no stdin until it returns. A send with
  `triggerTurn` lasts its whole run, so a message from NATS during startup starves the daemon's
  `get_state` and task for that run.
- **Append it with no turn and start the turn from a timer check.** Send
  `deliverAs: "followUp"`, `triggerTurn: false`: the host emits its `message_end` only after the
  startup hold settles. Arm a check on the host's managed timer from that `message_end`; a check
  that finds `isIdle()` true runs outside the drain, and its wake starts the turn. Record the
  message as pending before calling `sendMessage`, and cap how long an unseen one may hold the
  check.
- **A hold one plugin places on another through the shared interface is scoped, capped and
  cleared.** Name the session it holds for and ignore it for any other. Give an unsent hold a
  cap counted from the reader's first read, never renewed, and re-read it on a short poll, since
  no event announces the send or a refusal. Clear it on every failure path of the writer (a
  refused claim, a hand-over to a new session). Changing what either side reads bumps
  `ENVOY_PLUGIN_INTERFACE_VERSION`; sweep the prose that spells the number too.

## Evidence

- On 2026-10-26 a card during a Legion worker's startup ran a turn for 72 minutes, and the
  daemon's `get_state` failed 81 times. `extensions/card-delivery-omp.test.ts`'s startup case
  (#2278) fails against the earlier `envoy.ts` with `get_state` unanswered for 5 s.
- `startupTurn` (interface version 5, `packages/pi-shared/src/startup-turn.ts`) holds a
  controller's startup cards until its start turn. With no cap on an unsent mark, a claim that
  never answered or a start command that never ran held cards for good (tester's round-5 red
  tests); commit 5ce792cd added the 30 s first-read cap and the 1 s poll. Round 6 found
  `packages/pi-legion/AGENTS.md` still saying "(currently 4)".
