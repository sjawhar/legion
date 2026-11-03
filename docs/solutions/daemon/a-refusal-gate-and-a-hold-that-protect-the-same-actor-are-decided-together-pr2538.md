---
title: "A refusal gate and a hold that protect the same actor are decided together"
category: daemon
tags:
  - authorization
  - grants
  - quiesce
  - stale-plugin
  - cross-package
date: 2026-11-02
status: active
module: packages/daemon
related_issues:
  - "INFRA-484"
  - "sjawhar/legion#2538"
---

# A refusal gate and a hold that protect the same actor are decided together

Extends docs/solutions/daemon/a-hold-is-computed-with-the-check-the-action-that-releases-it-uses-INFRA-252.md.

That document pairs a hold with the action that releases it, inside one package, through one
shared predicate. This adds the case where the hold is in one package and the gate that defeats it
is in another, with no shared predicate to make the contradiction visible.

- Before adding a condition to a refusal gate on a shared route — grants, credentials, ready,
  handoff — **list what the refused caller still has to do and who is waiting on it**. A gate that
  refuses the very call some hold elsewhere exists to make possible turns a graceful wait into a
  deadlock, and nothing in either package reads wrong on its own.
- State in the gate's doc **when its window opens**, in terms of the act that opens it, not the
  decision that schedules that act. "While the daemon is replacing this process" and "once the
  daemon has decided to replace this process" differ by exactly the length of the hold.
- A grant is not a formality: in this system grants mint the credential the held turn needs to
  push. Refusing a credential for the length of a protective wait removes the thing the wait was
  protecting.
- When one gate serves several callers, name each exempt case in the comment rather than writing
  "this is exactly case X". Several shapes reach the same condition, and a reader who finds a
  second one assumes the comment is wrong rather than incomplete.

## Evidence (INFRA-484, sjawhar/legion#2538)

The stale-plugin lane relaunches a worker whose plugin is older than the daemon's contract, and it
**holds** that relaunch while the worker is in a turn, so the turn can push what it has rather
than lose it to a process ended where it stood.

The API's stale-caller gate refused `ready`, `grants` and `handoff/complete` with
`409 STALE_PLUGIN_CONTRACT` for any claim whose recorded contract was not the daemon's — including
that working claim, for the whole length of the held turn. The turn ran on unable to push and
unable to hand off, and its work went with the relaunch the hold had been waiting to start. Each
side read correctly alone: the lane was protecting the turn, and the gate was refusing a stale
caller.

Its doc even said the window lasted "until the relaunch has taken effect", which was true of every
case the author had in mind and false of the held one, where the window begins when the relaunch
**starts**, not when the daemon decides on it.

The fix adds the state to the gate, so a working stale claim is served and the same claim out of
its turn is refused as before. The test drives a real turn and asserts the grant carries a handle,
not merely that the status is not 409 — a grant without a handle is no use to the turn. The
out-of-turn control passes with and without the fix, which is what says the change narrowed the
refusal rather than removing it.

Two neighbouring tests had seeded a working claim and expected the refusal. Both describe the
refusal of a stale caller rather than the held relaunch, so each kept every assertion and simply
ended the worker's turn first.
