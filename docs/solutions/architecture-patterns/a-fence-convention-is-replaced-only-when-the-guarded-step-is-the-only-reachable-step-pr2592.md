---
title: "A fence convention is replaced only when the guarded step is the only reachable step"
category: architecture-patterns
tags:
  - concurrency
  - serial-queue
  - shared-mutable-state
  - refactoring
  - pi-envoy
date: 2026-10-31
status: active
module: packages/pi-envoy
related_issues:
  - "sjawhar/legion#2592"
---

# A fence convention is replaced only when the guarded step is the only reachable step

Extends docs/solutions/architecture-patterns/a-deadline-that-abandons-without-cancelling-makes-a-serial-queue-concurrent-INFRA-246.md.

That document prices the hand-written guard a deadline costs and says to make it one named
predicate per scope with the rule in a comment. This is what happened when the guard was taken
away a release later: the predicate is now a handle whose steps refuse for themselves. Making the
state private was the easy half and was done at the first head; three paths still let a refused
task write, and a review found all three after the change had passed its own unit tests and a
real-listener smoke.

- **The completion test is reachability, not privacy.** Ask of every step a task takes after a
  wait: can this be reached except through the handle? Private state satisfies "no call site
  writes a shared variable" and says nothing about a listener call, a connection or a
  subscription. One raw `client.unsubscribe` survived inside a queued task with two hand-written
  checks around it (`packages/pi-envoy/extensions/envoy.ts`, the recovery's unregister, now
  `handle.unregisterTopics`).
- **Latch the first refusal.** A predicate recomputed per step answers "may I write" again from
  the world, and the world comes back: a session closes and opens while a task waits on I/O, and
  a task that caught its refusal passes the next step. The shape that holds is one flag, one
  `refuse()` that sets it, and the predicate answering `!refused && …`
  (`packages/pi-envoy/src/registration-state.ts:486-505`).
- **Fence the acquisition, not its innermost primitive.** Guarding the raw dial left the waits
  inside the function that acquires the connection unguarded, and a refused task went on to open
  subjects and replace the stream control. The step is `connect()`, which guards, awaits the whole
  acquisition and checks again (`registration-state.ts:745-752`); what it must cover is every
  await the caller does not see.
- **A check before a self-guarded step is the old convention under a new name.** Audit each
  surviving check and keep only those that no step can make for you. Two shapes survive here:
  after a catch-all that would otherwise swallow the refusal, and before an action outside the
  handle. Twelve calls became four (`envoy.ts:2044-2046` states the rule, `:2460`, `:2470`,
  `:2485`, `:2597` are the survivors). A comment claiming "nothing re-checks a fence" while the
  file re-checks six times is worse than the checks.
- **A boundary that collapses outcomes deletes the distinctions its callers' messages were made
  of.** When a refusal narrows "claimed, then the session moved" and "never claimed" into one
  resolved `undefined`, carry the distinction across as data — here `claimAdopted` on the
  refusal the queue hands out (`registration-state.ts:224-230`, `:639`, `:245`) — and let the one
  reader choose the sentence. The detector is "who reads this value", not "is this text
  duplicated": the copies left inside the module were invisible precisely because nothing read
  them.
- **Mint a per-instance identity where the instance is, and make a caller unable to pass its
  own.** A module-scope constant is per process, and one process can host several instances. The
  factory mints it (`registration-state.ts:338`) and the input type removes the field from what a
  caller may send (`registration-state.ts:61`,
  `RegistrationInput = Omit<SubscribeInput, "clientInstanceID">`).
- **A completeness grep proves the old shape is absent and never that the new one covers it.**
  The acceptance criterion here was three greps over the rewired file; they were clean at the head
  whose review found two guards simply deleted. Run the grep against the base too, so its silence
  is told from a grep that searched nothing, and map every site the base guarded to the step that
  covers it now, by hand.
- **State the seam you could not drive.** The latched refusal and the fenced acquisition rest on
  unit tests with injected fakes: the smoke drives the claim path but cannot move a session inside
  one claim on demand. A reader who copies this shape inherits that seam, and it is where both
  defects lived.

One correction to the document this extends. Its bullet "a guard a compiler cannot enforce is a
convention, so say so where someone will read it — one named predicate per fenced scope, defined
at its top with the rule in a comment, is what survived review" was followed and did not hold: the
predicate was handed across ten function boundaries, a second hand-written guard grew beside it,
and the count reached 29 sites with some writes after a wait carrying no check at all. Read that
bullet as: a guard a compiler cannot enforce is a defect with a schedule, and the comment buys one
review round. Its site count, "about twenty", is 29 on the release this change replaced.

## Evidence

sjawhar/legion#2592 (dispatch://INFRA-404) moved two serial queues, each task's deadline, the
fence and fifteen pieces of shared state out of a 4,172-line extension into
`packages/pi-envoy/src/registration-state.ts`, whose queue hands each task a handle.

Round 1 of review requested changes with six findings, four of them the bullets above: the
refusal that did not latch (reproduced with a probe — bind to a session, move the open session
away, see the step refuse, move back, see the next step pass and a state write land), the lost
re-check after the connection acquisition, the raw listener call inside a queued task, and six of
twelve checks that guarded nothing beside three comments saying no call site re-checks a fence.
Round 2 found the dual of the message bullet: the two refusal strings the module still built were
unread, because the queue swallows a refused step's message.

The change is `f6e09f07` (the five correctness fixes) and `fcf43506` (the message chosen once),
on a branch whose first head had already passed 1,757 package tests and a smoke against a real
listener built from the branch.
