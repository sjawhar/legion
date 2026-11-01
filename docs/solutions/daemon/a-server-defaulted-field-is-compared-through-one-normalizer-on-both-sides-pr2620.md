---
title: "A field the API server defaults is compared through one normalizer, on both sides, in every language that reads it"
category: daemon
tags:
  - kubernetes
  - drift
  - defaulting
  - jq
  - duplicated-rule
date: 2026-11-01
status: active
module: packages/daemon
related_issues:
  - "sjawhar/legion#2620"
---

# A field the API server defaults is compared through one normalizer, on both sides, in every language that reads it

- **Before you compare a live Kubernetes object's field against the configuration, answer two
  questions in the code.** Does the API server write a default into this field when the manifest
  names none? Then normalize both sides through one function and never compare raw. Does the API
  server rewrite this field after admission? Then compare the stored template, not the live object.
  The two answers point at different surfaces, so decide each field on its own.
- **Put the normalizer beside the drift predicate** and send both sides through it, so a later
  reader cannot compare one side raw. A drift term that misreads the default does not fail a test
  on one object: it reports every object of a deployment that names none as drifted and replaces
  the whole fleet.
- **Lock the defaulted case with two rows, not one:** configuration naming none against an object
  carrying the server's default (kept), and configuration naming none against an object on some
  other value (replaced). One row alone passes under a predicate that always answers "no drift".
- **Never spell "unset or empty means X" as jq's `.field // "X"`.** `//` falls back on `null` and
  `false` alone and keeps `""`. Write `if (. // "") == "" then "X" else . end`.
- **When the same rule must exist in more than one language** because no module crosses the
  boundary — a Go daemon, a jq filter, a bash proof — give each copy a named function, have each
  comment name its siblings, and give each copy its own test for the **empty** value, not only for
  the absent one. That per-copy empty-value test is what pays for the duplication; without it the
  copies drift silently, because the absent case agrees under every spelling.
- **When you add a second "unfilled placeholder" refusal to a reader that already has one, the
  reader's field order decides which fires.** Write the new test with every earlier placeholder
  filled, and say in its comment why it is a pair of tests and not one.

## Evidence

sjawhar/legion#2620 added a second drift term to the Kubernetes runtime: a pod running as a
ServiceAccount other than `runtime.kubernetes.pod.service_account` is replaced at the next launch,
as one on another image already was.

The daemon writes `""` into the pod template when `service_account` is unset
(`config/kubernetes.go`'s `optionalString`), where the API server stores `"default"` on the pod it
creates. `accountDrift` and `serviceAccountOr` in
`packages/daemon/internal/runtime/sandbox/sandbox.go` read both sides through one helper for that
reason; comparing either raw would have replaced every pod of a deployment naming no account. The
two rows that lock it are in `account_drift_test.go`: "no configured ServiceAccount and a pod on
default" (kept) and "no configured ServiceAccount and a pod on another one" (replaced).

The two surfaces were chosen on opposite grounds in the same file pair. `accountDrift` reads the
live pod's `spec.serviceAccountName`, because the API server keeps a named account as written;
`templateResized` in `repocache.go` reads the Sandbox's stored template, because GKE Autopilot
rewrites a pod's resource requirements at admission. `docs/kubernetes.md` states both.

The jq trap cost a round. `scripts/e2e/stage4b-sandbox-tree.sh` read a deployment block's account
as `jq -r '.service_account // "default"'` while the `def account` the same pull request added to
`scripts/e2e/lib/deployment-pod-block-verdict.jq` read an empty value as `default`, and so did the
daemon. A block naming `service_account` empty then held every pod of the run to a nameless
account and failed the pod-shape checkpoint on a pod that was right. The fix gave the script one
`block_account` function, from which its three readings of that field all come; the three cases in
`scripts/e2e/lib/stage4b-pods.test.ts`'s `describe("the account a deployment's pod block names")`
went from 3 failing to passing. The review deliberately kept the jq rule in two copies rather than
sharing it: the verdict filter would have had to include Stage 4b's library, and the Go copy stays
a third copy whatever happens.

The paired refusal is `TestLoadForValidationRefusesTheOperatorRouteWithItsAccountUnfilled` beside
`…WithItsAudienceUnfilled` in `packages/daemon/internal/config/kubernetes_test.go`: `readPod` reads
the volumes before `service_account`, so only a route whose audience is already filled reaches the
account's own refusal. Both refusals come from one `unfilledPlaceholder` helper in
`config/kubernetes.go`.
