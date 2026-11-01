---
title: "An access check that compares a map value needs two scopes and crossed pairs, and a negative control that is a plausible wrong new version"
category: testing
tags:
  - mutation-testing
  - authorization
  - fixtures
  - negative-control
  - secrets-broker
date: 2026-10-25
status: active
module: packages/envoy
related_issues:
  - "INFRA-354"
  - "sjawhar/legion#2185"
---

# An access check that compares a map value needs two scopes and crossed pairs, and a negative control that is a plausible wrong new version

Extends docs/solutions/testing/mutation-proof-probe-tests.md.

That document says a fixture must contain the ambiguity the code resolves. This is the same rule
applied to an access check, where the ambiguity is easy to leave out without noticing.

- When access is granted by `m[provenID] == claimedScope`, register **at least two scopes** in the
  fixture, and **cross them**: an ID listed for scope X, asking under a claim for scope Y, in both
  directions. A fixture with one scope cannot tell equality (`m[id] == scope`) from membership
  (`_, ok := m[id]`). Its "unlisted ID" control is denied by both forms, so it proves only that the
  ID is listed somewhere.
- Name your negative controls before you write the change. Each must be **a plausible wrong version
  of the new code**, not only a revert to the old code. A revert proves that the tests see the
  change. A wrong new version proves that they see the boundary. Run each one in a copy outside the
  checkout and record which test fails.
- When such a mutant passes every suite, the test that makes it fail is the most valuable artefact
  of the change. Keep it as its own test, named for the boundary, and run it again on the same
  mutant after any merge or refactor that moves its setup. A green run alone does not show that it
  is still the same test.

## Evidence

INFRA-354 (sjawhar/legion#2185) changed the secrets broker's `BROKER_SERVICES` lookup. Before, the
map was keyed by service and named one account for each. After, it is keyed by account, and the
service the account proves must equal the service the pod's launcher names. In
`packages/envoy/internal/broker/requests/machine_state.go`, `requester` reads:
`e.Kind == "pod" && e.Subject != nil && e.Service != nil && services[*e.Subject] == *e.Service`.

- The unit fixture (`requests/machine_test.go`, `newFixture`) registers one service. The
  implementer's real-binary test (`cmd/broker/service_accounts_test.go`) also registers one service.
  Its only control is pod C, whose account no entry names.
- The implementer's negative controls were reverts only: the old service-keyed lookup, and the old
  duplicate-service refusal. Both failed the suites as expected.
- The tester wrote the plausible wrong version: `if _, listed := services[deref(e.Subject)]; listed
  && e.Kind == "pod" && e.Service != nil`. With it, any listed account would trust whatever service
  its launcher claims. The requests, api, policy, config and e2e packages all passed with it, and so
  did the implementer's real-binary test. Meanwhile one service's account received another
  service's secret.
- `cmd/broker/service_account_scope_test.go` (`TestAnAccountProvesOnlyTheServiceItIsListedFor`)
  registers two services, each with its own login, and crosses each launcher with the other
  service's accounts. It fails on that mutant with three `granted` replies where it requires
  `denied`. Review round 2 moved its setup into a shared fixture
  (`cmd/broker/pod_broker_test.go`), and a forward merge of `main` followed. After each one, the
  implementer ran it on the same mutant again, and it still failed the same way.
