---
title: "A read that gathers several reads bounds each one and degrades per field, so a grant not yet applied costs one field"
category: daemon
tags:
  - nats
  - permissions
  - deadlines
  - legion-state
date: 2026-10-29
status: active
module: packages/daemon
---

# A read that gathers several reads bounds each one and degrades per field, so a grant not yet applied costs one field

Extends docs/solutions/envoy/a-jetstream-answer-is-not-the-outcome-a-sequence-zero-duplicate-stored-nothing-and-a-timed-out-update-may-have-applied-pr2362.md.

- A NATS server that refuses a request's publish permission never answers it, so the request
  takes its whole deadline. A read that gathers several sub-reads (one per consumer, one per
  field) runs them concurrently, gives each its own bound, and nests the bound of a dependent read
  inside its parent's: one sub-read that hangs then costs only its own field.
- Read the cheap, load-bearing field first and return what you have when a later read fails:
  name the failure in the result (`unavailable: read the oldest waiting message: …`) and never fail
  the whole read.
- A change that adds a NATS grant and the code that uses it ships the code before every
  deployment has the grant. Test that window with a real server and a user without the grant, not
  a stand-in that refuses at once: a stand-in's refusal comes back fast and hides the timeout.

## Evidence

- sjawhar/legion#2478, review finding 2: `Consumers.Lag` read three consumers one after another
  under one 2 s deadline. Without `$JS.API.STREAM.MSG.GET.ENVOY_NOTIFICATIONS`, the first waiting
  consumer's get spent the bound and the GitHub consumer showed `waiting 0, read the consumer's
  info: context deadline exceeded`. Fixed in db9c30ee: `packages/daemon/internal/intake/consume.go`
  (`consumerLagBound` 2 s per consumer, `consumerOldestBound` 1 s nested, `lagOf`). Live with the
  grant denied: every count kept, `oldest unknown; unavailable: …`, 1.06 s.
