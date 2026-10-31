---
title: "A frame seq a held viewer dedupes on outlives the process, and an armed publisher republishes its state"
category: envoy
tags:
  - agent-stream
  - sequence-number
  - resume
  - last-writer-wins
  - pi-envoy
date: 2026-10-26
status: active
module: packages/pi-envoy/src/agent-stream.ts
related_issues:
  - "INFRA-195"
  - "sjawhar/legion#2205"
---

# A frame seq a held viewer dedupes on outlives the process, and an armed publisher republishes its state

## Rule

- A producer that can be relaunched while a consumer keeps its state (`omp --resume` under a page
  held open) numbers its frames so the new process's first frame beats the old process's last one.
  A counter that starts at 0 in each process makes the consumer drop every frame of the new process
  as already seen, with no error anywhere. Seed the counter from the clock (milliseconds × 1,000)
  and never move it back; write the bound you accept into the frame's contract doc (it holds while a
  process averages under 1,000 frames a millisecond and the clock does not run back across a
  relaunch).
- A publisher that reads its state before anyone watches publishes nothing then. When a control
  message arms a publisher nobody was watching, publish the current state frames at once
  (`publishState`). A relay that re-arms a relaunched process asks it for no replay, so without this
  a held page never sees state the new process read at startup. A viewer that also replays gets the
  frames twice under the same `seq` and keeps one.
- Prove each part on its own: a relaunch with a change after the first watch (numbering), and a
  relaunch onto different state with no change at all (arming). Put a barrier before the change:
  wait for the relay's first control message on the session's subject, or the change can land
  before any watch and test the arming path instead.

## Evidence

- `packages/pi-envoy/src/agent-stream.ts` (`SEQ_PER_MS`, the seed in the constructor,
  `publishState`); the call on arming in `packages/pi-envoy/extensions/envoy.ts` (`arming =
  !agentStream.watched`); the clause on `AgentStreamFrame.seq` in
  `packages/contracts/src/agent-stream.ts`. The page keeps a `settings` or `commands` frame only
  when its `seq` beats the one it applied (`web/src/features/agent-view/conversation.ts`).
- The tester's held-page step failed with numbering from 0: `FAIL resume-keeps-switch: timed out
  after 20s`. A first sequence-only fix also failed that step, because the change landed before the
  first watch; the barrier on `agentStreamControlSubject(S)` made the step isolate numbering.
- With `publishState` removed (scratch): `FAIL resume-keeps-switch: arming: timed out after 20s
  … after a reload the header shows {"model":"anthropic/claude-haiku-5-5",…}`, while the numbering
  step passed. The rig is `scripts/e2e/dispatch-session-settings.sh`, check `resume-keeps-switch`.
