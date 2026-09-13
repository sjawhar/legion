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
  - "a retired worker resumed by such a wake re-runs `legion handoff complete` for a phase it already reported"
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

The wakes also land on roles that are *not* active. The round-2 reviewer had finished, reported
with `legion handoff complete`, and been idle-retired; its five `Accepted:` replies then reached
the reviewer role's own topic as five empty `pr-review` wakes, each of which resumed the retired
session with `--resume` and a daemon catch-up prompt. The resumed reviewer treated every wake as
a fresh turn and re-ran `legion handoff complete` four more times for a phase that was already
complete — four spurious `phase-complete` publishes to the architect, each racing whatever the
architect was doing with the tree at that moment.

## What to do about it

- **Expect and ignore them.** An empty-body `pr-review` with `state: commented` from the PR's own
  implementer or reviewer App is a thread reply, not a round. The verdict arrives as the one
  `pr-review` whose body is non-empty and whose state is `changes_requested`, `commented` (a
  clean round while `.legion/` is still on the head), or `approved`. Do not re-read the PR or
  re-run anything on the empty ones.
- **Reply to threads immediately before the push, not spread across the round.** The wakes land
  in a burst that way and the following `pr-review`/`ci-green` pair is the signal; interleaving
  replies with other work turns each into a separate interruption for the active role.
- **Resumed with no assignment: read the catch-up, then go idle.** A wake is not an assignment.
  A worker resumed by a `pr-review` (or any event) whose catch-up shows its own phase already
  reported — its handoff committed, `phases[issue]` not naming it, no `pendingAssignment` — has
  nothing to do: it reads the catch-up, answers any direct question in it, and stops. It never
  re-runs `legion handoff complete`, re-pushes, or re-reviews; a second completion for the same
  phase is a duplicate the daemon answers 409 at best and a wrong `phases` write at worst. The
  `legion-worker` skill's continuation rule ("a later event can deliver a new prompt to this same
  session … treat it as a continuation, never as a fresh identity") is what this is.
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
