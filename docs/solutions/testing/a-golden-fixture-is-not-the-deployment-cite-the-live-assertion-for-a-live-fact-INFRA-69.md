---
title: "A golden fixture is not the deployment: cite the live assertion for a live fact, and treat every other value that fixture carries the same way"
category: testing
tags:
  - fixtures
  - golden-files
  - issue-pod
  - evidence
  - kubernetes
date: 2026-10-21
status: active
module: packages/daemon/internal/runtime/sandbox
related_issues:
  - "INFRA-69"
  - "sjawhar/legion#2088"
---

# A golden fixture is not the deployment

A recorded pod shape is what the renderer produced for the inputs the fixture carries. It is
authoritative for the renderer and says nothing about the cluster, because the operator's own
configuration is one of those inputs.

- **For a claim about what runs, cite the live assertion, not the fixture.** A fixture tells you
  what the code emits; a live test tells you what a deployment admits.
- **A field absent from a fixture is absent from that fixture's inputs, not from the world.** Read
  the live assertion before concluding anything from a gap.
- **Having found one such value, treat every other value in that fixture as the same class.** The
  trap is per-file, not per-field.

## Evidence

INFRA-69's note said the issue pod *"sets `automountServiceAccountToken: false` and names no
ServiceAccount of its own"*, citing
`packages/daemon/internal/runtime/sandbox/testdata/golden/root.json` and, beside it,
`packages/daemon/internal/runtime/sandbox/live_operator_test.go`.

The fixture does carry `automountServiceAccountToken: false` and no `serviceAccountName`. The live
test asserts the opposite of the second half: `live_operator_test.go:44-49` refuses a pod whose
`ServiceAccountName` is **not** the operator's, or whose `AutomountServiceAccountToken` is nil or
true, and lines 50-57 exec the pod to confirm
`/var/run/secrets/kubernetes.io/serviceaccount/token` is absent, failing if it is present. So a
live pod runs under a ServiceAccount — the operator's — and what is absent is the token.

The fixture named none because its operator configuration sets none. Citing it for a live fact is
what produced the wrong sentence; the corrected sentence cites the live assertion alone, and the
verified-from-inside half (`/var/run/secrets/kubernetes.io` missing, `kubectl` answering
*connection refused*) stands beside it as the reader's own check.

The same fixture carries per-role CPU, memory and ephemeral-storage reservations, volume kinds and
mount paths. Each is the renderer's output for the same inputs; each needs the same treatment
before a sentence about a running pod rests on it.
