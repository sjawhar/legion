---
title: "An act an agent makes on its own object stays its own, and one answer both decides and records"
category: architecture-patterns
tags:
  - acting-for-a-person
  - audit-record
  - event-routing
  - actor
date: 2026-11-04
status: active
module: packages/envoy/internal/dispatch
related_issues:
  - "sjawhar/legion#2729"
---
# An act an agent makes on its own object stays its own, and one answer both decides and records

When a credential may act for a person, the rule is about the object, not about the credential:
only an act the actor could not make on its own becomes the person's.

- An act on the actor's own object is recorded exactly as it was before the delegation existed.
  Recording it as the person changes who every downstream reader thinks wrote it: a notice feed
  that routes a person's write to the sessions following it sends the writing session its own
  write back, and a reader that matches "the person answered" starts matching the agent's own
  work.
- An act on another's object is the person's, recorded as the person with a side field naming the
  delegate — never as the delegate, and never with the person erased.
- Decide which from one function's answer and then switch on it. Never normalise a value to decide
  ("is this my own session?") and store the un-normalised one: a padded id then passes the
  comparison and is written as a writer nobody can address. One call, one answer, one switch —
  and the branch that refuses reports that call's own error.
- Record the authority as it stood at the act and read that record later, never the live
  credential: narrowing or revoking a token must not retroactively un-approve what it approved
  while it held the right. Make the predicate treat "no delegate" as the whole authority, so rows
  stored before delegation existed keep their meaning.
- Keep the request's credential out of the recorded actor: give the field `json:"-"`, a named
  stripping constructor, and a note at every `==` on that type, because a stored actor decoded
  from JSON carries none and would otherwise never compare equal to a live one.
- Where a route writes no domain event, record the delegate on the access-log line instead, and
  say so where the route is defined.

## Evidence

INFRA-1017 let an agent holding its person's Dispatch token act for them. Review round 1's one
correctness finding: `editAsk` and `changeAskFollower` took the person path for every credential
holding `person`, so an agent rewording its own ask wrote `ask.edited` with `actor.kind: user`,
and `events.Broker.Notify` sent the asking session its own reword back as the person's.
`packages/envoy/internal/dispatch/api/asks.go` and `api/ask_followers.go` now take that path only
for another session's ask or another session, pinned by
`TestAnAgentsOwnRewordAndFollowAreStillTheSessionsOwnAct` — removing the predicate makes
`ask.edited` read `{Kind:"user", ID:"alice", Act:…}` and the test fails.

Review round 2's finding on the same route: it trimmed the body's session id to decide "this is my
own session" while `bearerSessionActor` recorded `supplied.ID` as sent, so a body actor of `" s1"`
followed `s1` and was stored with the space. It is now one
`asTheSession, sessionErr := bearerSessionActor(...)` and a switch on it; the regression assertion
in `ask_followers_test.go` reads `status=204` against the commit before the fix.

`model.ActorAct.Scopes` is the snapshot at the act, read by `docs/approval_carry.go`'s
`carryVerdict`, with `TestTheActsScopesAreTheSnapshotTakenWhenTheAnswerWasMade` storing a narrowed
and revoked token row, and `TestAnAnswerStoredBeforeActsExistedStillCarriesTheApproval` writing
the literal pre-delegation JSON. `model.Actor.Credential` is `json:"-"` with `Actor.Recorded()`,
because `docs/lostedit.go` compares actors with `==`. `api/credential_requests.go` records the
delegate on the access-log line, where the broker records the decision and Dispatch writes no
event.
