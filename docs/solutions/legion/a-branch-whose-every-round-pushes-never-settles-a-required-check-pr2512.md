---
title: "A branch whose every round pushes never settles a required check: run the required set locally at the head you push"
category: legion
tags:
  - github-actions
  - cancel-in-progress
  - review-rounds
  - required-checks
date: 2026-10-30
status: active
module: .github/workflows
related_issues:
  - "sjawhar/legion#2512"
---

# A branch whose every round pushes never settles a required check: run the required set locally at the head you push

Extends docs/solutions/legion/an-implement-handoff-push-runs-ci-in-full-and-cancels-the-code-heads-runs-cite-the-tips-runs-and-give-the-lane-the-code-heads-image-LEGION-631.md.

That page covers one push cancelling the previous head's run. The steady state over many review
rounds is worse: where the pull-request workflow cancels in progress per pull request, each
round's push kills the round before it, the branch produces **no completed run at all**, and a red
required check survives round after round with nobody able to see it.

- Run the required set yourself, at the head you are about to push, before you push it. Name the
  commands and their output in the round's report, since that is the only settled verdict anybody
  has.
- Read "queued" as no verdict. Reporting it twice in a row without acting is how a red check
  reaches its fourth round.
- When a settled verdict at a code head is what the next role needs, push the code alone and let
  the handoff follow in a `.legion/`-only commit carrying `skip-checks: true`: that push starts no
  workflow, so it cancels nothing and the code head's run completes.
- A reviewer who runs the lint or format tool by hand finds what the branch's own CI never
  reported. Expect that, and treat such a finding as a gap in the branch's evidence rather than a
  new defect.

## Evidence

`.github/workflows/ci.yaml` groups its runs by pull request number with
`cancel-in-progress` on the `pull_request` event. In sjawhar/legion#2512, rounds 2, 3, 4 and 5
each recorded "No CI run has completed on this branch since `65da1690`"; run 41003510761 at
`9862c8d8` was cancelled by the next round's push like the rest.

The cost: a Biome formatting failure — the digest constant on one long line in
`packages/envoy-client/src/__tests__/nats-server.ts` and two one-element `apt` arrays in
`.github/e2e-scripts.json` — was present from run 40957622905 at `a744ce61`, red in both `lint`
and `envoy-client`, and `ci-ok` could not pass while it stood. No round saw it. The reviewer
found it in round 5 by running Biome 2.4.11, the version `bun.lock` pins, by hand.

From that round on, each push was preceded by the required set at the head: `bunx biome check .`
over the whole tree, the touched package's `bun run typecheck`, and every gate script of the
repository, each quoted in the round's report. The drain that followed also proved the trailer's
half: the handoff commit above a code head started no workflow and cancelled nothing, so the code
head's run completed for the reviewer.
