---
title: "A consumer's fail-closed branches are driven by a shared stand-in package and, live, by a proxy interposed between the two real servers"
category: testing
tags:
  - test-seam
  - fail-closed
  - live-proof
  - proxy
date: 2026-10-31
status: active
module: packages/envoy/internal/brokertoken
related_issues:
  - "sjawhar/legion#2541"
---

# A consumer's fail-closed branches are driven by a shared stand-in package and, live, by a proxy interposed between the two real servers

A reader's refusals are the part no happy-path run reaches, because the producer never writes the
answers they refuse. Two seams reach them without forging a credential.

- **Put the stand-in in an ordinary package, not a `_test.go` file**, when a second package's
  suite needs it: Go cannot share test-only code across packages, and the copy that gets written
  beside the second suite drifts. `packages/envoy/internal/brokertoken/brokertokentest` is the
  shape — the issuer's stand-in and a caller holding one key — read by both
  `internal/brokertoken`'s suite and `internal/dispatch/api`'s.
- **Let the stand-in produce its credentials with the production producer.** That caller signs
  its proofs with `internal/broker/helper`'s own DPoP transport rather than a second signer
  written beside the test, so a test passing is evidence about what a real caller sends.
- **Make the stand-in count and keep what it was presented.** Counting introspections and keeping
  every `Authorization` header is what makes "ten concurrent requests cost one introspection" and
  "the consumer authenticated as itself" assertable at all; a stand-in that only answers can
  prove neither.
- **Live, interpose a rewriting proxy between the two real servers** rather than forging a token:
  the producer and the consumer are both real, and the proxy rewrites one field of the answer in
  flight, with the rewrite switched off as the control. That is how a branch the producer can
  never write — a `named` scope with an empty project — is driven on the real pair, and the same
  proxy counts the calls that prove a single flight and a rate bucket.

## Evidence

sjawhar/legion#2541 (INFRA-264). The implementer's live proof ran `scripts/dev-broker.sh` and
`packages/dispatch/e2e/run-server.sh` as one pair; the tester's rounds put a counting proxy
between them and drove `PUT /__rewrite {"project_scope":"named","project":""}`, the one refusal
no live token can produce, recorded in `.legion/INFRA-264/test.json`. The stand-in package came
out of the `ce-simplify-code` pass (`14a3ecbb` → `e9d7da2e`), which found the same broker stubbed
twice.
