---
title: "Every REST reply on a review thread fires an empty-body `pr-review` (commented) webhook: a five-thread round is five extra wakes"
category: legion
tags:
  - legion
  - github-webhooks
  - pull-request-review
  - review-threads
  - envoy
  - implementer
  - reviewer
  - wake-budget
date: 2026-09-13
status: active
module: legion
related_issues:
  - "LEGION-15"
  - "sjawhar/legion#1027"
symptoms:
  - "an active role's inbox shows `{\"type\":\"pr-review\",\"state\":\"commented\",\"body\":\"\"}` once per thread reply, seconds after the implementer or reviewer answered threads"
  - "a corrective round with N threads delivers N `pr-review` wakes with nothing in them before the one review that matters"
---

# Every REST reply on a review thread fires an empty-body `pr-review` webhook

## What happened

On sjawhar/legion#1027 the implementer answered review round 1's five threads the way the
`legion-worker` skill prescribes — one
`POST repos/{owner}/{repo}/pulls/{n}/comments/{id}/replies` per thread, each naming the fixing
commit. Within seconds the implementer's own session received five Envoy deliveries, one per
reply:

```
{"type":"pr-review","state":"commented","author":"legion-implementer[bot]","body":""}
```

The reviewer saw the mirror image when it accepted the five threads in round 2: five
`Accepted:` replies, five empty `pr-review` wakes to whichever role was active. GitHub models a
standalone review-thread reply as a one-comment review with no body, so `pull_request_review`
fires (`action: submitted`, `state: commented`, empty body) for each one, and Envoy forwards it on
`pr.<n>.review` exactly like a real review. Nothing is wrong on the wire; the cost is attention:
a five-thread round is five extra wakes to an active role before the single review that carries
the verdict.

## What to do about it

- **Expect and ignore them.** An empty-body `pr-review` with `state: commented` from the PR's own
  implementer or reviewer App is a thread reply, not a round. The verdict arrives as the one
  `pr-review` whose body is non-empty and whose state is `changes_requested`, `commented` (a
  clean round while `.legion/` is still on the head), or `approved`. Do not re-read the PR or
  re-run anything on the empty ones.
- **Reply to threads immediately before the push, not spread across the round.** The wakes land
  in a burst that way and the following `pr-review`/`ci-green` pair is the signal; interleaving
  replies with other work turns each into a separate interruption for the active role.
- **Do not fold replies into one review submission to avoid the fan-out.** The skill's contract
  is one reply per thread naming its commit, and `legion threads resolve` reads each thread's
  newest comment individually; a single review with `comments[]` is the *reviewer's* submission
  shape (one submission fires one wake), not the implementer's answer shape.
- **A future reducer fix** would drop `pr-review` payloads with an empty body and
  `state: commented` from the two Legion Apps before publishing them to the active role; until
  then this is the shape to recognise.

## Related

- `../../../packages/daemon/src/daemon/AGENTS.md`, GitHub Apps — why the reviewer cannot resolve
  the threads it accepts and the implementer runs `legion threads resolve`.
- `fast-follow-pr-mechanics-and-queue-registration.md` — the review-round mechanics these wakes
  sit inside.
