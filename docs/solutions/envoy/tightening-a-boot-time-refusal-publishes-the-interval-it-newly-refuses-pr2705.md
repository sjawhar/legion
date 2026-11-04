---
title: "Tightening a boot-time refusal publishes the interval it newly refuses, with the remedy"
category: envoy
tags:
  - configuration
  - rollout
  - refusal
  - changelog
date: 2026-11-04
status: active
module: packages/envoy/internal/broker/config
related_issues:
  - "sjawhar/legion#2705"
---

# Tightening a boot-time refusal publishes the interval it newly refuses, with the remedy

Extends docs/solutions/envoy/a-setting-another-repositorys-deployment-sets-is-removed-in-two-releases-pr2674.md.

That page covers a setting another repository still sets, where refusing is wrong and the reader
warns instead. This is the other case: the refusal is right, the setting is this process's own, and
the obligation is arithmetic.

- **Diff the two predicates and derive the interval between them** — the values the old rule
  accepted and the new one refuses. "No deployment that boots today stops booting" is a claim about
  that interval, and it is false unless the interval is empty. Compute it before you write the
  sentence.
- **Publish the interval, in the units an operator sets, with the action they take before the
  roll**, in the changelog and in the operations guide. Each new pod of a rolling deploy exits at
  boot rather than serving, so the fix has to land before the roll, not after the first pod fails.
- **Make the refusal print the value that passes**, not only that this one does not. The process
  knows the floor; the operator should not have to re-derive it from a rule.
- **Pin a row that every shipped default still boots**, since a refusal that rejects the defaults
  stops every deployment at once.

## Evidence

`sjawhar/legion#2705` (dispatch://INFRA-454) replaced the broker's lease rule with one that walks
both renewers' retry schedules. A review round found the changelog claiming "so no deployment that
boots today stops booting", which was false: the old rule was `2 × lease ≤ 9 × sweep`
(`packages/envoy/internal/broker/config/config.go`), so everything between four and a half times
the sweep and the new floor booted before the change and exits at `fatal` after it.

The corrected text names the interval and the remedy — "a 60-second lease at the default
five-second sweep, for example, or anything from 271 s to 359 s at a sixty-second sweep … Read the
floor for your sweep above, raise `BROKER_LEASE_SECONDS` to at least that, and roll that first" —
in the changelog fragment and in `docs/site/src/content/docs/broker/operate.md`.

The printing rule is in the model: `leasetime.Model.Shortest` is documented as "the figure a
refused broker prints: the operator is told the lease to set, not only that this one is too short",
and the refusal in `config.Load` carries it. `TestShortestLeavesEveryDeploymentBooting` pins that
the shipped 900-second default passes at every sweep the broker takes.
