---
title: "A handler-side hook that writes the store takes the decision context, not the request context"
category: daemon
tags:
  - context
  - WithoutCancel
  - api-routes
  - decision
  - store-write
  - client-disconnect
date: 2026-10-10
status: active
module: packages/daemon/internal/api
related_issues:
  - "LEGION-663"
  - "sjawhar/legion#1874"
---
# A handler-side hook that writes the store takes the decision context, not the request context

- A claim route builds its decision context once — `ctx, decided := s.decision(r, token)`
  (`context.WithoutCancel(r.Context())`, ended by the API's drain, released by `defer decided()`)
  — and runs the machine's decision on it so a caller that hangs up mid-request leaves nothing
  half-recorded. Every write the route adds after that decision — a hook that persists what the
  request carried, a log the store keeps — runs on the same `ctx`, never on `r.Context()`.
  `defer decided()` cancels it only after the hook has returned.
- On `r.Context()` the plugin's own retry of a timed-out request, or a pod dying, cancels the
  write: the daemon logs a misleading "could not be persisted" and the next boot loads a store
  that never held what the ready carried, which is the one thing the persistence exists for.
- Lock the rule with a handler test whose fake hook asserts `ctx.Err() == nil` after the client
  has disconnected (a request whose connection is closed before the route returns), so the next
  hook added to a route is held to it.

## Evidence

LEGION-663's `ready` handler built `ctx` for `m.Handle(ctx, supervise.RequestReady{…})` and then
called `s.capabilityReported(r.Context(), c, report)`, whose first act is
`store.PutCapabilityReport`. The reviewer's finding 2 named the window; the fix is the one token
(`59dac701`). The tester added `TestReadyHandsTheCapabilityReportAContextTheAgentsDisconnectDoesNotCancel`
(`41ac961a`), red with the request context put back ("the report was handed over on a context
that is already done (context canceled)"), and drove a raw-socket ready with `Connection: close`
and `SO_LINGER 0` against a real `legion start`: `capabilities: session reported` logged, zero
"could not be persisted" lines, and after a restart the claim's view carried its report.
