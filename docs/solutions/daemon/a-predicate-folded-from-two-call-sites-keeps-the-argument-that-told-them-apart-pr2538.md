---
title: "A predicate folded from two call sites keeps the argument that told them apart"
category: daemon
tags:
  - refactoring
  - predicate
  - update-round
  - simplify-pass
date: 2026-11-02
status: active
module: packages/daemon
related_issues:
  - "INFRA-484"
  - "sjawhar/legion#2538"
---

# A predicate folded from two call sites keeps the argument that told them apart

- When you fold two call sites onto one shared predicate, compare the **questions**, not today's
  answers. If one caller asks about a value it is carrying — the thing being decided — and the
  other asks about a field on the record, a predicate that takes no argument answers the wrong
  question for one of them, and does it silently.
- Keep the parameter. Name **both** readings in the doc comment, and say which caller passes
  which, so the next fold does not drop it again.
- A predicate whose two readings coincide unless **two different values of the same kind** are in
  play needs a test that puts two in play. That test is the fold's acceptance criterion, not an
  extra: without it the fold is unproven, however green the suite is.
- "No behaviour change" is a claim about every caller, so it is evidence you owe, not a
  description of your intent. A fold is a behaviour change until a test distinguishes the two
  readings.

The shape to recognise: N callers, N−1 of which happen to pass the record's own field. The
exception is the whole rule, and it is the one the suite does not cover.

## Evidence (INFRA-484, sjawhar/legion#2538)

The daemon opens an update round over a drifted pod and replaces that pod only once every worker
has answered. A claim whose process is relaunched into another pod **leaves** the round: it takes
no further part, and the replacement no longer waits for it.

A review round asked for the left-round rule to be stated once rather than restated in two
packages. The fold produced `Claim.LeftPodRound()`, reading `UpdateRound == UpdateLeftRound` —
"did this claim leave its **own** recorded round". Every caller inside the machine meant exactly
that. The sweep did not: it asks "has this claim left **the round I am deciding**".

So a claim that had left round X, and that round Y's own open had not yet reached, still recorded
X, read as one that had left, and **stopped holding Y's pod**. Y could then replace that pod and
delete its disk before the claim had been asked to push anything, and the stalled-round notice
stopped naming it. The claim's own relaunch into that pod is what had kept the disk, so it is
exactly the claim that can hold unpushed work there.

Every existing test had the claim standing in the round being decided, where the two readings
coincide. The distinguishing state needs a pod death and a relaunch — three events deep — which
is why a suite of several hundred tests passed throughout.

The fix is `LeftPodRound(round string)`, with every caller naming the round it means, and
`IsPodRound` added beside `IsPluginRound` so a round id can be read on its own. The test is a
sweep rather than a count on the helper, because the harm is the disk and not the number: it
asserts no `ReplacePod` at the tick, then that the round's ask reaches the claim, then that the
pod goes once that claim has answered. With the parameterless predicate restored it fails with
`the pod was replaced 1 times while the round had asked the tester nothing; the disk it holds
carries work no worker was told to push`.
