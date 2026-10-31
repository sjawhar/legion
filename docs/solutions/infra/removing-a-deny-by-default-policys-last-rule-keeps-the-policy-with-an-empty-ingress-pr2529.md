---
title: "Removing a deny-by-default policy's last rule keeps the policy, with an empty ingress"
category: infra
tags:
  - kubernetes
  - networkpolicy
  - pulumi-kubernetes
  - deny-by-default
date: 2026-10-30
status: active
module: packages/envoy/infra
related_issues:
  - "sjawhar/legion#2529"
---

# Removing a deny-by-default policy's last rule keeps the policy, with an empty ingress

When a mode stops a workload needing the one ingress rule a NetworkPolicy grants, **keep
registering the policy with an empty `Ingress` array; never stop registering the policy.** A
NetworkPolicy that selects a pod denies every source it does not admit, so an empty rule list is
the strictest state there is; deleting the policy leaves the pod selected by nothing and admitted
by everything. The two edits read the same in a diff and mean the opposite, so assert the policy's
continued existence with zero rules rather than asserting the absence of a rule.

## Evidence

- `packages/envoy/infra/gcp/dispatch_kube.go:710-720`: the front door's policy is built from
  `frontDoorIngress := networkingv1.NetworkPolicyIngressRuleArray{}`, the port-1055 rule appended
  only when `!s.natsInCluster`, and `policy(tailnetName, frontDoorIngress)` called in both modes.
  Port 1055 — the SOCKS5 proxy the app and the listener dialled the NATS hub through — was that
  policy's only rule, so the obvious edit (drop the rule, drop the policy) would have opened the
  front door's pods, which hold the deployment's tailnet egress, to every pod in the cluster.
- Pinned by `TestDispatchInClusterNatsLeavesNoSocks5Path` (`gcp/dispatch_test.go:1608`), whose
  `policyIngressRules(t, m, "example-dispatch-tailnet", 0)` asserts the policy is registered, its
  `podSelector` unchanged, and its rule list empty — an assertion that fails both ways round: if
  the policy disappears, the lookup fails; if a rule survives, the count fails.
