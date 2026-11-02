---
title: "Untrusted text in an agent's prompt goes last, behind the same fixed label, in every message that carries it"
category: best-practices
tags:
  - untrusted-input
  - prompt-injection
  - agent-messages
date: 2026-10-31
status: active
module: packages/daemon/internal/record
related_issues:
  - "sjawhar/legion#2571"
---

# Untrusted text in an agent's prompt goes last, behind the same fixed label, in every message that carries it

A value a third party chose — a CI check's name, a branch, a review body, a webhook payload's
field — that a daemon composes into a sentence an agent reads is untrusted input in a context with
no escaping. Interleaved with the daemon's own words it reads as the daemon's own words.

- Put it **last** in the message and **behind a fixed label** (`Reason: …`), so everything before
  the label is the daemon's and everything after it is the third party's.
- Use the **same label in every message that carries that value**. The argument that loses the
  label is "this text already appears elsewhere": if the other place fences it and this one does
  not, a reader learns the fence means nothing.
- Pin the **label**, not the sentence, in the test, and let the live proof print the whole message
  so a human reading the evidence sees the fence.
- Widening who receives such a message is the moment to look again: a message that reached one
  role now reaches four.

This is hardening, not a hole: the worst such a value can do is stop or redirect the recipient's
own work. Treat it at that weight — a cheap, uniform fence rather than a mechanism.

## Evidence

- `packages/daemon/internal/record/outbox.go`'s `PhaseEnded.Message` put the checks-red reason —
  GitHub's own check and workflow names — in the middle of the daemon's sentence. The reviewer
  raised it as `Security[prompt]` hardening when the message began reaching the tester, the
  reviewer and the merger as well as the implementer.
- The fix puts the reason last, behind the `Reason:` label a started worker's task already carries
  it under (`packages/daemon/internal/workflow`'s task text), so both places fence it the same way.
- `packages/daemon/internal/daemon/phase_ended_test.go` pins the label for all five roles.
  Negative control, run at the head: the reason put back inline after "now:" fails
  `TestACIRedTellsTheRoleWhosePhaseEndedOnItsOwnTopic` on every one of them.
- The live proof prints the whole message at step 3:
  `…the issue is in implementing now. Nothing of yours is stopped: … Reason: CI is red at h1: ci.`
