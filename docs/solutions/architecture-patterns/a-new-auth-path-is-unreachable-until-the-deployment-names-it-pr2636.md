---
title: "A new auth path stays unreachable until the deployment names it, and the blast radius is proven from configuration"
category: architecture-patterns
tags:
  - rollout
  - feature-gating
  - refusals
  - contract-change-census
date: 2026-11-04
status: active
module: packages/envoy
related_issues:
  - "sjawhar/legion#2636"
---

# A new auth path stays unreachable until the deployment names it, and the blast radius is proven from configuration

A change that adds a credential path, a refusal or a new authenticated route has two jobs: the
new path must work, and the path that was already there must not change for anyone who has not
asked for the new one.

- **Gate the new branch on the deployment having been told about it**, and take that branch only
  then. A branch that runs before the "is this configured" check turns an unconfigured
  deployment's working requests into refusals.
- **Name the test for the old behaviour, not the new feature.** A test called
  "…IsTheListenerItAlwaysWas" fails loudly when someone reorders the branch; a test named for the
  feature passes while the regression ships.
- **Prove the blast radius of a new refusal from configuration that makes it unreachable**, not
  from counting rows in production. "This deployment sets none of these settings, so the code
  path refuses at *this* line" is checkable by a reader; "we found no affected rows" is a claim
  about a moment.
- **Give every consumer of a removed or changed contract a verdict**, including the ones you are
  not changing, and say which rollout step each needs.

## Evidence

In sjawhar/legion#2636 the Envoy listener's broker-token branch ran *before* its open-listener
check, so every loopback listener with no `ENVOY_AGENT_SECRETS_URL` answered 401 to clients that
had just been given a machine identity — requests it had served before the change. The gate is
now one clause, `cmd/listener/apiauth.go:80`
(`if _, presented := brokertoken.Presented(r); brokers != nil && presented`), and the test is
`TestAPIAuthWithNoBrokerSettingIsTheListenerItAlwaysWas`, named for the invariant.

The pull request's contract-change census shows the configuration proof. A migration retiring
every live `vm` enrollment was shown to retire nothing in the deployment, not by a row count but
from the deployment's own declaration: its Pulumi golden sets none of
`BROKER_GOOGLE_OIDC_AUDIENCE` or `BROKER_VM_*`, so `discoverVerifiers` builds a nil verifier and
`verifyVMToken` refuses every VM token at `enroll/enroll.go:661`. The new `SIGN_IN_REQUIRED`
refusal was shown to close nothing yet, because `AGENT_SECRETS_MACHINE_IDENTITY` has no occurrence
in the deployment repository, so no token that names a service and no person exists to be refused.
