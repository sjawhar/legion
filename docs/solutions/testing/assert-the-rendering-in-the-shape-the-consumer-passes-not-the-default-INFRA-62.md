---
title: "Assert a rendering in the input shape the consumer passes, not the default — a rule appended behind a condition is unasserted where it matters"
category: testing
tags:
  - pulumi
  - networkpolicy
  - golden-files
  - test-coverage
  - mock-engine
date: 2026-10-21
status: active
module: packages/envoy/infra/gcp/dispatch_test.go
applies_when:
  - A component renders differently under an optional input the production stack always sets
  - A list of rules, peers or arguments is built by appending to a conditional base
  - A golden pins one example of a rendering that has more than one shape
related_issues:
  - "INFRA-62"
  - "sjawhar/legion#2089"
---

# Assert a rendering in the input shape the consumer passes, not the default

- **Test the shape production runs.** A component with an optional input has at least two
  renderings. Asserting only the default proves the arrangement in a configuration nobody deploys;
  the one the consuming stack passes can be broken while the suite stays green.
- **An index into a list built by appending is a trap.** `ingress[0]` is the first rule *of that
  configuration*. Add a conditional rule above it and the assertion silently moves to a different
  rule. Pin the rule count, then read each rule by what it admits, not by position.
- **A helper that hard-codes "exactly one" cannot cover the many case.** When a reading helper
  requires a single element, the test for the multi-element shape hand-rolls its own walk — which is
  how the two drift. Split the helper: one part reads the container (its own selector, its type, its
  element count), one part reads a single element. Both shapes then go through the same reader.
- **Prove the assertion bites on the failure it exists to prevent.** Make the one-line production
  edit that would cause it (an `append` turned into an assignment, a conditional branch widened),
  run the test, record the message, restore the file byte-identically and say so. An assertion never
  observed failing is a claim.

## Evidence

INFRA-62 split `envoy-infra:gcp:Dispatch` into three workloads, and the app's NetworkPolicy gained a
rule admitting the new front door on 8766 — without which Tailscale Serve, a pod of the same
namespace that no `namespaceSelector` covers, reaches the app at no port at all. The rule is
appended after the optional `allowedNamespaces` rule, so:

- with `allowedNamespaces` unset it is `ingress[0]`, which the default-shape test read;
- with `allowedNamespaces: [envoy, legion]` — what the consuming stack passes — it is `ingress[1]`,
  which no test read.

The reviewer named the exact silent failure: turning that `append` into an assignment inside the
conditional leaves the whole suite green while every tailnet reader loses the app. The fix pinned
two rules in the production shape and split `policyIngressPods` into `policyIngressRules` (the
policy's own podSelector, its `Ingress` type, the rule count) and `ingressPods` (one rule's peers,
refusing another port, a `namespaceSelector`, an `ipBlock` or an empty `from`). The inversion was
then run: `"…" should have 2 item(s), but has 1`, with `dispatch_kube.go` restored at the same
sha256.
