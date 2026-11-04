---
title: "A bound that predicts a loop's behaviour imports the loop's own steps, from a stdlib-only leaf when it must"
category: architecture-patterns
tags:
  - shared-constants
  - drift
  - leaf-package
date: 2026-11-04
status: active
module: packages/envoy/internal/broker/leasetime
related_issues:
  - "sjawhar/legion#2705"
---

# A bound that predicts a loop's behaviour imports the loop's own steps, from a stdlib-only leaf when it must

A validation, readiness check or capacity bound that models what another loop will do is only
true while every term of the model equals the term the loop runs. Restating a term is drift with
a delay fuse: the next person edits the literal in the loop, the bound keeps refusing and
accepting by the old arithmetic, and nothing is red.

- Every term of the bound — each wait, divisor, floor, backoff and client timeout — is one
  exported function or constant that both the model and the running loop call. Count the
  restatements before you start: a term spelled in two binaries and the validator is three places
  to move.
- Where the loop lives in a binary that cannot import the package enforcing the bound, put the
  shared steps in a leaf package with no imports beyond the standard library, and have the
  validator, the loop and the tests all depend on that. The dependency wall is a reason for the
  leaf, not a reason to restate.
- Apply each renewer's or client's schedule in **one** function that the search, the simulation
  and the tests all call, so a fix to the schedule cannot reach one of them and miss another.
- Then pin the equality: a test that drives the real loop asserts the value the production code
  used equals the value the model used, rather than asserting a literal of its own.

## Evidence

`sjawhar/legion#2705` (dispatch://INFRA-454). Before the fix, the client timeout the bound counted
was restated as `&http.Client{Timeout: 30 * time.Second}` in `cmd/agent-secrets/client.go` and
`cmd/agent-secrets-helper/main.go`, and the renew divisor as `lease / 3` in
`cmd/agent-secrets/main.go`'s `renewLoop` and `internal/broker/helper/server.go`'s `renewLoop`.
The review named the hazard: "A change to any one of those four literals moves a renewer and not
the refusal, which is the drift this package exists to stop."

`packages/envoy/internal/broker/leasetime` is the leaf. Its package comment states the constraint
that shaped it — "It is a leaf on purpose: the standard library and nothing else. Both renewers
are client binaries (`cmd/agent-secrets`, `cmd/agent-secrets-helper`), and `internal/broker/config`,
the one place that refuses a lease, pulls in the broker's API, policy, secret and Postgres
packages" — and it exports `RenewAfter`, `NextBackoff`, `HelperRenewAfter`, `RenewClientTimeout`
and `HelperMinRenew`, each called by the renewer it belongs to and by the model.

Inside the model, `Model.step` is the one place either renewer's schedule is applied, so the
interval search, `Walk` and both test oracles read the same steps; the round-3 review found two
copies of that step and they were merged into it.

The loop-drive rows then assert the equality rather than a literal: the `agent-secrets` row checks
the first wait the real `renewLoop` asked for against `leasetime.RenewAfter(lease)`, and fails
when they differ.
