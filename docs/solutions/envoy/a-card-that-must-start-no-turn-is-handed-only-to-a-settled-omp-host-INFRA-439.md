---
title: "A card that must start no turn is handed only to a settled Oh My Pi host"
category: envoy
tags:
  - pi-envoy
  - oh-my-pi
  - triggerTurn
  - steer
  - message_end
date: 2026-10-27
status: active
module: packages/pi-envoy
related_issues:
  - "INFRA-439"
  - "sjawhar/legion#2228"
---

# A card that must start no turn is handed only to a settled Oh My Pi host

**The live proof the evidence names, `packages/pi-envoy/scripts/smoke-replay.sh`, was removed
with the replay path by dispatch://INFRA-771**; a frame that comes back now comes from the
session's own consumer, and the repository's `scripts/e2e/envoy-session-delivery.sh` is the live
proof of that path. The rule below is unchanged: an inert card is handed only to a settled host.

`docs/solutions/envoy/session-id-remint-stale-transcript-identity.md` says a `triggerTurn: false`
steer "lands when the agent next runs instead of starting an unsolicited turn". That holds only
on an idle host. While a turn streams, the same card makes the agent run another model turn.

## Rule

- `pi.sendMessage(card, { deliverAs: "steer", triggerTurn: false })` starts no turn only on an idle
  host. omp ignores `triggerTurn` while a turn streams: the card is steered into that turn, and
  the agent loop runs one more model turn for it at the stop boundary.
- So a card that must never run a turn (a stale question, a notice) is handed only when the host
  is settled: `isIdle() === true && hasPendingMessages() === false`, not shutting down, and on the
  session it is for. Until then, hold it (its claim pending, its record written if a restart must
  not lose it), and hand it at the first settled heartbeat. `isIdle()` alone is not enough: it
  already reads true in the `agent_end` of an aborted run while omp still holds queued cards.
- Count a card as shown only at its `message_end`, never at `pi.sendMessage`. omp holds a card sent
  during a turn in memory until the turn takes it. A SIGKILL, an `abort_and_restore_queue` or a
  session switch before that loses it.
- Put the settled check in one function and use it for every such hand-off and hand-back
  (`hostSettled` in `packages/pi-envoy/extensions/envoy.ts`, read by `UnwrittenFrames.sendCard` and
  `handBackWithdrawn` in `packages/pi-envoy/src/unwritten-frames.ts`), so two gates cannot drift.

## Evidence

- Review round 3 of sjawhar/legion#2228 drove an "inert" replayed BTW (`triggerTurn: false`) into a
  streaming turn on omp 18.12.2 with claude-haiku-4-5. After `turn_end` came a new `turn_start`,
  the card's `message_end`, then the model answering the stale question in the main transcript.
- Idle on omp 18.8.9, the same send wrote the card about 3 ms later with no `agent_start`.
- Commit 28cb09bf holds an inert card until `hostSettled()`. `packages/pi-envoy/scripts/smoke-replay.sh`
  leg 7 checks it live: a BTW replayed while a turn streams is not handed during the turn
  (`queuedMessageCount` stays 0), then is shown once after the turn ends on its own, with one
  `agent_start` and one `turn_start`. The plugin at cdc5696f fails that leg with
  `queuedMessageCount: 1`.
