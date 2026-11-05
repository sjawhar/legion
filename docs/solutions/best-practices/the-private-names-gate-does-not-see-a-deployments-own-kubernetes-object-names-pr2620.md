---
title: "The private-names gate does not see a deployment's own Kubernetes object names"
category: best-practices
tags:
  - public-repository
  - private-data
  - test-fixtures
  - ci-gates
date: 2026-11-01
status: active
module: scripts/e2e
related_issues:
  - "sjawhar/legion#2620"
---

# The private-names gate does not see a deployment's own Kubernetes object names

Extends docs/solutions/best-practices/a-public-repository-fed-with-private-data-names-no-private-issue-key-and-takes-the-issue-as-a-required-operator-input-pr2466.md.

- **A green `check-private-names.sh` is no evidence that a fixture holds no private name.** The
  gate matches the deployment repository's name, the company's, private internal hosts and
  Tailscale MagicDNS hosts. A ServiceAccount, namespace, cluster, StorageClass or PriorityClass
  name copied from the private deployment passes it.
- **Use the plan's neutral fixture name in a test and a fixture**, not the value your deployment
  happens to run on, even when the value is convenient and the gate is green.
- **When you claim a name is gone, prove it with a `grep` for that name**, and say which name you
  grepped for. The gate's own output answers a different question.

## Evidence

sjawhar/legion#2620 moved the operator route's ServiceAccount into a `${POD_SERVICE_ACCOUNT}`
placeholder so the repository holds no account at all. A test added in the same change,
`scripts/e2e/lib/stage4b-pods.test.ts`'s `a block naming an account runs its pods as that
account`, used the account the private deployment creates for its own pods as its example value.
`check-private-names.sh` passed over it, the `docs` and `lint` jobs were green, and the reviewer
caught it by reading the line. It was changed to the neutral name the plan had already chosen,
`operator-account`, which the Go fixture and the regenerated golden also use.

The evidence that closed it was a `grep` for the name itself over the tree, excluding `.legion/`,
not the gate's own pass line.
