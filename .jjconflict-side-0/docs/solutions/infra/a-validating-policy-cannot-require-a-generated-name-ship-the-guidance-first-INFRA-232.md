---
title: "A validating policy cannot require a generated name; ship the guidance first"
category: infra
tags:
  - kubernetes
  - admission
  - validating-admission-policy
  - mutating-admission-policy
  - generate-name
date: 2026-10-25
status: active
module: docs/kubernetes.md
related_issues:
  - "INFRA-232"
  - "INFRA-383"
  - "sjawhar/legion#2195"
---

# A validating policy cannot require a generated name; ship the guidance first

1. **A `ValidatingAdmissionPolicy` alone cannot tell a server-generated name from one the client
   chose.** The server generates a name only when `metadata.name` is empty, and it does so before
   validating admission runs. A client that sends `{name: X, generateName: x}` passes
   `has(generateName) && name.startsWith(generateName)`, and so does a client name of exactly the
   generator's shape. To require a generated name, put a `MutatingAdmissionPolicy` in front of the
   validating rule. Its JSON patch removes `/metadata/name` whenever `generateName` is also set.
2. **Change the published guidance before the rule that enforces it, and make every sentence true
   in both states.** Until the policy applies, do not list the form as one admission enforces.
   Give the reason for the form instead, and add a dated line that says admission did not yet
   require it on that date, naming the issue that adds the rule.
3. **Make the policy's apply wait on observable preconditions:** the guidance merged, and its
   production check passed. A role on an older checkout then gets a create refusal that leaves
   nothing behind. Make the refusal message name the fix.

## Evidence

- The INFRA-232 plan, measured on a local `kube-apiserver` v1.36.2. A validating policy that
  denies every request and prints the fields saw `{name: P-bcdfg, generateName: P-}` exactly as a
  generated name looks, and saw `{name: X, generateName: x}` admitted under `X`. With the mutating
  policy in front, every admitted name was generated: the client's `P-bcdfg` became `P-6swwr`.
- On the live cluster on 2026-10-25, a server dry run admitted a generated-name Job, a
  client-named Job, and `{name: X, generateName: x}` under `X`. So sjawhar/legion#2195 changed
  the guidance with no window in which the cluster refused what the page published. Its evidence
  bullet ends "…did not require a generated name, which dispatch://INFRA-383 adds".
- On the live cluster, a create that admission refused still named a generated name
  (`The jobs "infra-232-postgres-2p459" is invalid`), which confirms that generation runs first.
