---
title: "A capability a plan declares unavailable is a claim until someone tried it, and it propagates into every later artefact"
category: legion
tags:
  - planning
  - gap-analysis
  - proof
  - handoff
  - e2e
date: 2026-10-20
status: active
module: skills/legion-worker
related_issues:
  - "INFRA-44"
  - "sjawhar/legion#2083"
---

# A capability a plan declares unavailable is a claim until someone tried it, and it propagates into every later artefact

- **Write "not attempted" or attempt it.** A plan may route around a capability it has not got,
  but it must say which of the two it is doing. "X cannot run here" with no recorded attempt is a
  claim wearing a constraint's clothes, and no later phase re-tests it — the planner's sentence
  becomes the implementer's surface choice, the implementer's surface choice becomes the pull
  request body's, and the body is what a human reads.
- **The phase that pays is the implementer.** A proof surface the plan rules out is a proof the
  implementer cites instead of drives, which is the weaker of the two and the one the skill asks
  it not to settle for.
- **An inherited constraint is worth one cheap probe.** The cost of being wrong is a whole phase
  proving its change by reading someone else's run; the cost of checking is usually minutes.

## Evidence

INFRA-44's plan recorded, in its gap analysis, that "the Playwright suite cannot run in a Legion
worker pod", and resolved the gap by making CI jobs the production-like surface and telling the
architect that the two alternatives were human-only. Nothing had been attempted; the inference
came from the absence of a container runtime and of a Postgres package.

That sentence travelled. The implementer built to it and cited CI run ids for both of its proofs.
Review round 1 flagged the resulting line as one of two claims in the body that were not true at
that head. The tester then stood the entire stack up inside its own worker pod — the same pod
class, no extra grant — and ran 299 rows green in sixteen minutes, with seventeen fault-injection
probes of its own, and re-proved each of the implementer's recorded proofs on the running
application rather than re-reading them.

Nothing was lost: the change is correct and the tester's proof is the stronger one. What the
claim cost was the implementer's own `E2E (implementer)` line, which still names CI jobs it read
rather than a surface it drove, and one review finding spent on a body sentence instead of on
code.

The recipe that turned out to work is
docs/solutions/testing/a-browser-harness-in-an-issue-pod-lives-on-the-issue-volume-INFRA-43.md and
its extension
docs/solutions/testing/a-pod-harness-moves-tmpdir-too-and-the-failure-mode-is-a-silent-kill-INFRA-44.md.
