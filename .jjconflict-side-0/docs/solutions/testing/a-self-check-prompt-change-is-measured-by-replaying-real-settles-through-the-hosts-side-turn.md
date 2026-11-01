---
title: "A self-check prompt change is measured by replaying real settles through the host's side turn: widening one case regressed a correct firing"
date: 2026-10-24
category: testing
module: pi-envoy
problem_type: best_practice
component: testing
severity: high
applies_when:
  - "A change to the wording of pi-envoy's run-end self-check (ASK_SELF_CHECK_PROMPT in packages/pi-envoy/extensions/envoy.ts)"
  - "Any one-word model verdict that gates an action, where the unit tests stub the model's answer"
tags: [pi-envoy, self-check, side-turn, prompt, probe, dispatch-ask-reminder, regression]
status: active
---

# A self-check prompt change is measured by replaying real settles through the host's side turn: widening one case regressed a correct firing

## Context

pi-envoy's run-end self-check asks the session's own model, in a hidden side turn, to answer WAITING
or PROCEEDING; only WAITING sends the `dispatch-ask-reminder` steer that tells the agent to open a
Dispatch ask. Agents waiting for a go-ahead message their user had promised answered WAITING and were
told to ask for it. The first fix widened PROCEEDING to any reply "a human already said they will
send". Every unit test passed: they stub the model and pin only the substring "WAITING or
PROCEEDING". On the model, that wording also turned a review the human had put off ("I'll take a look
later") into PROCEEDING, 5 of 5 runs, where main answered WAITING 5 of 5 and the firing had been
judged correct. The shipped wording requires the reply to be on its way and names deferral as
WAITING.

## Guidance

Measure each candidate wording on the model, over the settles it is meant to change and over settles
it must not change:

1. **Cases.** Take transcripts whose `dispatch-ask-reminder` entry is the verdict in question: the
   false firings being fixed, and past firings judged correct as negative controls. Cut each at the
   line before that entry and write the copy under a fresh session id; work on the copy, never on
   the original transcript. Add fresh sessions for shapes no transcript has (a genuine undecided
   decision; a promised next message).
2. **Arms.** Build each arm's prompt by evaluating `ASK_SELF_CHECK_PROMPT` from each revision's
   source (`git show <rev>:packages/pi-envoy/extensions/envoy.ts`, transpiled with
   `Bun.Transpiler`), or check that each arm equals that revision's output; never copy the text by
   hand unchecked. Include main.
3. **The call.** Resume each copy in an interactive omp session in tmux with
   `--no-extensions -e probe.ts --approval-mode always-ask`. The probe registers a slash command
   that calls pi-envoy's own `sideTurn` wrapper on the context kept from `session_start`, so the
   question goes out in the same `/btw` prompt over the same conversation snapshot and model:

   ```ts
   import type { PiApi, SessionContext } from "@legion/pi-shared/pi-types";
   import { sideTurn } from "../packages/pi-envoy/src/side-turn.ts";

   export default function probe(pi: PiApi): void {
     let sessionContext: SessionContext | undefined;
     pi.on("session_start", async (_event, context) => {
       sessionContext = context;
     });
     pi.registerCommand("probe", {
       description: "A/B the run-end self-check prompts",
       handler: async (args, context) => {
         const ask = sideTurn(sessionContext);
         // for each run and each arm: await ask({ prompt }), then record replyText
       },
     });
   }
   ```

4. **Runs.** Five per arm per case; each run is one whole-context model call over the transcript.

## Why This Matters

The prompt is the classifier. A unit test that stubs the reply proves the parse and the steer, never
where the model puts the boundary, so a wording that fixes the reported case can move a neighbouring
case across it unseen. Here the cost is asymmetric (a false WAITING pages a human with a question
nobody had), which makes the negative controls the part that is easy to skip and the part that
caught the regression.

## When to Apply

- Before shipping any change to the self-check's question, its WAITING or PROCEEDING definitions,
  or the line that lists (or denies) open asks.
- For any other one-word model gate whose tests stub the model.

## Examples

Results on Opus 5.5, five runs per arm per case ("W" is WAITING). The main and first-wording columns
come from one run; the shipped column from a second run over the same scenarios (fresh copies of
the same transcripts, and fresh sessions given the same text).

| Case | Right verdict | main | first wording | shipped |
|---|---|---|---|---|
| Four sessions waiting for a promised go-ahead message | PROCEEDING | W 20/20 | W 0/20 | W 0/20 |
| Fresh session, task promised in the next message | PROCEEDING | W 5/5 | W 0/5 | W 0/5 |
| Fresh session, a decision nobody has made | WAITING | W 5/5 | W 5/5 | W 5/5 |
| A review the human put off for later | WAITING | W 5/5 | W 0/5 | W 5/5 |

A later finding came from the same method: with no asks of its own, the prompt said "There are no
open asks in Dispatch.", which denied another session's ask that covered the wait. On two such past
firings, the shipped definitions with that old line answered WAITING 5 of 15 and 5 of 5 runs, and
with "You have no open asks in Dispatch." 0 of 15 and 0 of 5.

## Related

- `packages/pi-envoy/AGENTS.md`, the run-end nudge: why a false WAITING is the expensive error.
- `docs/solutions/testing/the-next-real-binary-legion-case-is-one-runlegionpane-call-answer-runs-before-the-gateway-one-transcript-per-pane-and-a-subagents-side-turns-LEGION-634.md`:
  the real-binary harness, whose stand-in gateway answers the self-check with a fixed word.
