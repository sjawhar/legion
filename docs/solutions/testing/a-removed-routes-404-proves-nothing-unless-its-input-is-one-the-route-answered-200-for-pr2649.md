---
title: "A removed route's 404 proves nothing unless its input is one the route answered 200 for"
category: testing
tags:
  - deletion
  - negative-control
  - http-routes
  - fail-before-pass-after
date: 2026-11-02
status: active
module: packages/envoy
related_issues:
  - "sjawhar/legion#2649"
---

# A removed route's 404 proves nothing unless its input is one the route answered 200 for

A deletion's test has to fail before the change and pass after it, and a 404 is the easiest
assertion to get wrong, because the mux answers 404 for an absent route and the handler answers
404 for an absent thing.

- **Seed the input the route used to serve, then assert the refusal.** A 404 on an id nothing
  holds passes at the base too, so it measures nothing. A 404 on a record the store holds — one
  the deleted test asserted 200 for — is red before the deletion and green after it, and the only
  thing that changed is the mux.
- **Say which of the two 404s you mean, in the test's own comment.** The next reader cannot tell
  from the assertion, and the distinction is the whole value of the test.
- **Replace the deleted route's test; do not merely delete it.** Deleting it leaves the acceptance
  line "shows the route gone" answered by nothing.
- **A route count and a symbol census are necessary and are not this proof.** A pin on the number
  of mounted routes moves whenever any route moves, and a grep for the handler's name proves the
  symbol is gone, not that nothing serves the path. Keep both; neither discriminates a route that
  is unmounted from one a test merely failed to reach.
- **Watch which layer answers first.** On the same server, the same shape of request with a
  fabricated id was refused by authentication before the record was ever looked up, so an id
  nobody holds exercises the auth layer and never reaches the route's own behaviour at all.

## Evidence (sjawhar/legion#2649, INFRA-879)

The secrets broker's `GET /v1/credential-requests/{record}` went with the transition-era approval
paths. `packages/envoy/internal/broker/api/api_test.go:1233` replaced the route's own test rather
than deleting it, and states the distinction in its comment:

> The record this test reads back is one the store holds and the old route answered 200 for, so
> what the 404 says is that the mux has no such route, not that the record is missing.

It opens a real pending secret request through `POST /v1/requests` and asserts 404 on the removed
path; the test it replaced asserted 200 on exactly that record.

Beside it, two pins that are not this proof: `registeredRoutes` in
`packages/envoy/internal/dispatch/api/routes_table_test.go`, a count of the mounted table, and the
reviewer's census across `packages/`, `scripts/`, `docs/` and `.github/` for the handler and its
callers.

The layer ordering showed up in the live checks of the same pull request: the broker's decide route
answered `403 DECIDE_ON_BROKER_PAGE` for a real pending secret request and `401
ACCOUNT_TOKEN_INVALID` for a fabricated 64-hex record id, so the fabricated id proved only that the
caller was unauthenticated.
