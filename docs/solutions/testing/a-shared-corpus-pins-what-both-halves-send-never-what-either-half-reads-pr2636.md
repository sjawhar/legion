---
title: "A shared corpus pins what both halves send, never what either half reads"
category: testing
tags:
  - two-implementations
  - wire-corpus
  - contract-testing
  - broker-client
date: 2026-11-04
status: active
module: packages/broker-client, packages/envoy/internal/brokerclient
related_issues:
  - "sjawhar/legion#2636"
---

# A shared corpus pins what both halves send, never what either half reads

One contract with two implementations held to a shared corpus of recorded exchanges.
The corpus is strong where it compares, and blind where it substitutes.

- **List what the corpus supplies to the client, and treat that list as untested on both sides.**
  Every placeholder is a step the corpus performs *for* the implementation instead of checking:
  the endpoint it derives from configuration, the session name it was configured with, anything
  read from the environment. Those steps need their own tests in each language.
- **A change to a rule the two halves share adds its corpus vector in the same commit.**
  Without the vector, each suite still passes and the corpus certifies an agreement that has
  stopped existing.
- **Have each half's doc comment name its counterpart by file and symbol**, so the next person
  changing one is told where the other is rather than having to know.
- A field added to the wire is caught for free when the suites assert *set equality* rather than
  containment: the half that does not send it fails its own test.

## Evidence

In sjawhar/legion#2636 one token client exists in Go
(`packages/envoy/internal/brokerclient`) and TypeScript (`packages/broker-client`), both replaying
`internal/brokerclient/testdata/wire.json`. No form field ever drifted, because the suites assert
set equality (`wire.json:6`). Both drifts that did happen landed on the corpus's placeholders
(`wire.json:18-25`): `${token_endpoint}`, "the stub broker's own origin followed by
endpoint.path", and `${session}`, "the session name the client was configured with".

The permanent-refusal rule drifted **twice**, each time for want of a vector. First the
TypeScript half dropped a live held token on a 429 where Go kept it, with no 429 vector in the
corpus; `ca63e520` fixed the rule and added the vector. Then `556118ac` exempted
`invalid_dpop_proof` and `use_dpop_nonce` in Go only (`internal/brokerclient/brokerclient.go:145-151`)
— again with no vector, so neither suite saw it, and a clock-skewed proof dropped a live token on
the TypeScript side alone.

The second drift was how each half read `AGENT_SECRETS_URL` to build the endpoint the corpus
substitutes: Go trimmed one trailing slash, TypeScript trimmed every one. It cost three rounds
and two tester-written red tests, and it produced a refusal naming the credential rather than the
URL an operator could fix, because the DPoP proof's `htu` was signed for `//v1/enrollments` while
the broker's mux redirected to the cleaned path.
