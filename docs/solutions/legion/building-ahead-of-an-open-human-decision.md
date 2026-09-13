---
title: "Building ahead of an open human decision: with gates.design off the gate never reaches a human, so the decision is its own ask; write the runbook as an option; when the answer defers, correct the docs to the decided state and record the deferral where it will be carried"
category: legion
tags:
  - dispatch_ask
  - design-gate
  - gates.design
  - decisions-needed
  - runbook
  - deferral
  - docs-correction
date: 2026-09-13
status: active
module: legion (architect spec lifecycle, implementer docs)
related_issues:
  - "LEGION-77"
  - "sjawhar/legion#1017"
  - "sjawhar/legion#1033"
  - "LEGION-19"
  - "LEGION-74"
---

# Building ahead of an open human decision

## What happened

LEGION-77 built a daemon mechanism (`private_key_secret`, the human-tier secretsd source) and a
runbook prescribing it for the LEGION deployment — move the two App keys to the human tier,
relaunch the daemon from a terminal pane, rotate. The store choice (human tier now / wait for
Kubernetes / second Unix user) needed Sami's risk appetite and his dotfiles, so it was an open
"Decisions needed" item while the tree ran. PR #1017 merged with the runbook stating the human
tier as this deployment's instructions. Sami then answered **Wait for Kubernetes** (2026-09-13
13:46 UTC, after a "Why is this important?" round), and a second, docs-only PR (#1033) had to
correct the runbook. Three things a future architect and implementer should take from it.

## 1. With `gates.design: off`, the design gate is not a human channel

On a deployment whose `legion.yaml` sets `gates.design: off`, the daemon approves the registered
gate itself the moment it is registered (`legion-architect` skill: "do not request approval, do not
register a gate"; the deployment instructions say the same). Nothing the architect writes into the
spec reaches a human through the gate. A decision that genuinely needs the human — authority, money,
risk appetite — therefore has to travel by its own channel, opened as soon as the question is known,
never folded into "Decisions needed" on the assumption the gate will surface it.

Evidence from this tree on which channel worked: the operator session put the same choice to Sami
as a plain `dispatch_message` on LEGION-74 at 08:45 UTC and it went unanswered; the architect's
separate `dispatch_ask` on LEGION-77 (three options, cost stated, recommendation) drew a
clarification reply and then the answer. The deployment's standing rule decides the channel; what
this tree proves is only that the gate is not one.

## 2. A runbook written before the answer describes an option, not the deployment

Implementation proceeded on the recommended option because the exposure was live and the code was
small — a reasonable call, and the code was worth merging under every answer (the config form is
true regardless). The runbook was the part that could not be: it stated "They live in secretsd's
human tier…" and a seven-step rotation as the LEGION deployment's instructions while the answer was
open. Written from the start as "On a shared box that chooses the human-tier boundary, they live
in…", the same text would have needed no correction under any answer.

Rule: while a "Decisions needed" item is open, any artifact whose *meaning* depends on the answer —
a runbook, a deployment instruction, an operator step — is written conditionally on every branch of
the answer, or it waits. Code whose correctness does not depend on the answer may land.

## 3. When the answer defers the mechanism, correct the docs and record the deferral

The correction (#1033, one commit) was a reframing, not a revert:

- A new section directly after the introduction, "Current decision for the LEGION deployment",
  stating the decided state in plain sentences with the date and the decider, what the operator must
  *not* do here, when the deferred step happens instead (the Kubernetes cutover), and where it is
  recorded (LEGION-74). One closing sentence says what the rest of the page is for.
- The openers of the two prescriptive sections reworded as the mechanism a deployment may choose.
  Every command, quoted daemon message, and ordering fact left character for character (verified
  against `main` and `config.ts`); the mechanism stays documented because another shared-box
  deployment, or this one before the cutover changes shape, may still choose it.

And the deferral was recorded where it will be carried: a comment on LEGION-19 ("Legion on
Kubernetes") stating the requirement the pod boundary must now satisfy — App keys mounted into the
daemon pod only, rotation at the cutover — with the ask reference and Sami's answer as provenance.
The LEGION-77 spec kept its deferred acceptance items in place, each marked deferred with that
provenance, rather than deleting them; the record of what was asked and decided stays with the issue
that asked.

## Related

- `docs/solutions/daemon/human-tier-secretsd-keys-from-the-daemon.md`: the mechanism this tree
  built and its status on the LEGION deployment.
- `docs/deployment.md`, "Current decision for the LEGION deployment": the corrected runbook.
- `docs/solutions/daemon/a-gate-that-ignores-events-for-unregistered-subjects-must-read-the-source-at-registration.md`:
  the gate's mechanics, including that `gates.design: off` is the only non-human path.
