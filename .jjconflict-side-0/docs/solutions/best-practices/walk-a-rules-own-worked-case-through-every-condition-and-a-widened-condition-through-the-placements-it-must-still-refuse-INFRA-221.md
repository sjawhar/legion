---
title: "Walk a rule's own worked case through every condition, and a widened condition through the placements it must still refuse"
category: best-practices
tags:
  - documentation
  - agent-facing-text
  - review-round
  - worked-example
  - skills
date: 2026-10-25
status: active
module: skills/legion-worker
related_issues:
  - "INFRA-221"
  - "sjawhar/legion#2149"
---

# Walk a rule's own worked case through every condition, and a widened condition through the placements it must still refuse

Extends docs/solutions/best-practices/the-caveat-belongs-in-the-copy-its-failing-audience-renders-INFRA-113.md.

That note drives one example on each side of a sentence's axis. The delta is for a rule with
several conditions and a worked case on the same page:

- **Every time a condition or the worked case changes, read the case through each condition word
  by word.** Read it as the reviewer will: literally, with no meaning carried over from the
  author's intent. If the case fails a condition, either the rule or the case is wrong. Fix one
  before the push, never after the review.
- **A condition you widen to admit the case must still refuse what the page and its cited
  learnings forbid.** List those placements first (here the shared workspace, `$HOME`, `/tmp`),
  then read each one through the new wording. Widening is the edit most likely to let one of them
  through, because it is written to say yes.
- **Ground each move on a measurement and say so where the reader sees the final wording.** A
  sentence that moved three times reads as arbitrary unless the body names what each move was
  answering.

## Evidence

`skills/legion-worker/references/tools.md`, "The test: where it lands, and what it leaves
resolving", went through three wordings of its first condition in sjawhar/legion#2149:

- **Narrow:** "through one of the variables the runtime actually sets". This refused the page's own
  worked case. The Dispatch harness root at `/legion/harness` is on the issue's volume but under
  none of the nine variables, which point to `/legion/scratch/…`, `/legion/go/…` and
  `/legion/uv/…`. Both thermonuclear reviewers found it independently in review round 9. The
  reviewer had missed it in round 8, though the sentence was already in the diff.
- **Widened** on the architect's ruling: "or under a path on that volume your harness names
  itself". This admitted `$LEGION_WORKSPACE`, which the INFRA-43 learning forbids. The tester drove
  why: a two-file root written there showed as `A` in `jj status`, because
  `snapshot.auto-track` is `all()`. That was tester round 12's failure.
- **Bounded:** outside the issue's workspace and the runtime's own directories, keeping a
  repository's own dependency install into its ignored directories. Tester round 13 checked it
  against every entry of `ls -d /legion/*`.

The same class appeared three times before the consolidation. The pin heading said "version" while
its own `biome` pair differed by name. The ground given for the withheld capability forbade the
runners the page prescribes. The `$HOME` example failed the first condition two sentences after
it.
