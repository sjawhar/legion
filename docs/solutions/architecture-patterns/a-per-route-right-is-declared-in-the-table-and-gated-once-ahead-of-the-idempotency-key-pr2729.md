---
title: "A per-route right is declared in the table and gated once, ahead of the idempotency key"
category: architecture-patterns
tags:
  - authorization
  - route-table
  - idempotency
  - middleware-order
  - oauth-scopes
date: 2026-11-04
status: active
module: packages/envoy/internal/dispatch/api
related_issues:
  - "sjawhar/legion#2729"
---
# A per-route right is declared in the table and gated once, ahead of the idempotency key

A new right a route requires is a property of the route, so it belongs in the route table and is
enforced in one wrapper. A handler that restates it is a second place to be wrong.

- Declare it beside the row, in a map keyed `"<METHOD> <pattern>"` stamped onto the route struct,
  as `writeClasses` already is; do not add a column every row must then carry. Make `Register`
  refuse to mount a map naming a route the table does not mount, and a row that needs a value and
  names none, so a row added later cannot be left undeclared.
- Wrap the gate ahead of the keyed (idempotency), project and session layers. An authorization
  refusal must never be captured under an idempotency key: the person who grants the right then
  retries and is answered the stored refusal. It must also come before a project or session
  refusal, so a caller that lacks the right is told that rather than told its write belongs to
  somebody else.
- Decide the right from the request's own credential, never from the actor the write records. The
  recorded actor of a bearer is the session it acts for, so a check reading it refuses a person's
  own token what the same person's browser is allowed. Keep two predicates: a silent one for a
  branch that falls through to another refusal, and one that writes the refusal for a field only
  that credential may send.
- When the new gate weakens an older layer's invariant, decide that from the row when the route is
  wrapped, not per request: the gate has already refused everybody the older layer was guarding
  against, so re-asking is dead work that reads as a second rule. Say at the weakened site which
  wrapper stands ahead of it.
- A check that cannot sit at the gate because it needs row state read inside the transaction must
  answer the gate's refusal exactly, header included. Build the protocol's refusal in one function
  and give the error type the field that header needs, so the two paths are indistinguishable to a
  caller.
- A surface that is not HTTP restates the code and says in a comment why the header is absent.

## Evidence

INFRA-1017 replaced Dispatch's `403 HUMAN_ONLY` on 44 browser-only rows with two OAuth scopes. The
first implementation called `requireScope(...)` inside each of 47 handlers; review round 1 refused
it on two grounds, both observable: the scope check ran after `keyedWrites`, so a refusal was
stored under the caller's `Idempotency-Key` and replayed after the person ticked the scope back,
and after `withinProjectScope`, so a scopeless caller was told its write belonged to another
project. The gate is now `gateRouteScopes`, wrapped once in `register`
(`packages/envoy/internal/dispatch/api/server.go`), over `routeScopes`
(`api/routes_table.go`), with `scopedRoutesNotMounted()` and `register`'s panic as the twin of
`undecidableWrites`.

Round 2 then deleted `callerHoldsRouteScopes`, a per-request helper `withinSessionScope` used to
re-ask what the gate had decided; it is `personsOwnWrite := len(route.OAuthScopes) > 0`, computed
when the route is wrapped (`api/project_scope.go`). `issue_claim.go`'s `personRule` carries the
credential-not-actor rule, and `api/ask_transitions.go` runs the `approve` check inside the ask's
row lock while answering the gate's own `403 OAUTH_SCOPE_REQUIRED` under RFC 6750's challenge
(`insufficientScopeChallenge`, `apiError.scope`). `api/live_agent.go` restates the code for a
socket frame.
