---
title: "Proving an envoy-infra input on the real Pulumi CLI, and which refusals that surface can show"
category: testing
tags:
  - pulumi
  - envoy-infra
  - plugin
  - proof-surface
  - credentials
date: 2026-10-30
status: active
module: packages/envoy/infra
related_issues:
  - "sjawhar/legion#2529"
---

# Proving an envoy-infra input on the real Pulumi CLI, and which refusals that surface can show

The consumer surface of an `envoy-infra` component is the built plugin driven by the real Pulumi
CLI, which is how a consuming program takes it. Two things to know before you plan a proof on it.

- **Pick the surface by where the refusal runs.** A component resolves its plain inputs, then
  calls `providers.Require`, then registers, and only then renders. So a resolver refusal is
  reachable from `pulumi preview` with no cloud provider configured, and it is the control that
  shows the inputs were accepted. A refusal that lives inside the rendering — in an `ApplyT` over
  an input that resolves later — is behind `providers.Require` and **cannot** be shown there: the
  preview reports the missing provider first. Prove that one on the mock engine instead. A plan
  that names a negative control must name the surface on which that control is reachable.
- **The recipe, and one credential hazard.** Set `PULUMI_HOME`, `PULUMI_BACKEND_URL=file://…`
  under `$TMPDIR` and an empty `PULUMI_ACCESS_TOKEN` **before the first CLI call**: the first
  `pulumi package add` run without them mints an ephemeral Pulumi agent account by itself and
  prints a claim URL asking that someone take ownership of it. Build the plugin with a version —
  `go build -ldflags '-X main.version=0.0.1' -o pulumi-resource-envoy-infra
  ./cmd/pulumi-resource-envoy-infra` — and `pulumi plugin install resource envoy-infra <version>
  --file <binary>`; an unversioned build is `0.0.0-dev`, and the preview then tries to download
  that version from GitHub releases and fails with a 404 instead of running your binary.
- **A Go doc comment on an `*Args` field reaches no consumer.** `pulumi package get-schema`
  carries no `description` for any `gcp:Dispatch` input property, because the module's `Annotate`
  methods set tokens only. The consumer's documentation is
  `packages/envoy/infra/README.md`; write the input's meaning there as well as in the field's
  comment.

## Evidence

- `packages/envoy/infra/gcp/dispatch.go:1176` resolves the settings and `:1180` calls
  `providers.Require(… "gcp" …)`, while the `natsUrl` check runs in the `ApplyT` at
  `gcp/dispatch_kube.go:401-413`. Measured on the built plugin with the pinned CLI v3.268.0: a
  program setting `natsReplicas: 6` fails with `envoy-infra:gcp:Dispatch example-dispatch:
  natsReplicas 6 is above 5, the most copies NATS keeps`, while the same program with
  `natsInCluster` and `nats://127.0.0.1:4222` fails with `needs a "gcp" provider in its providers
  option` and never reaches the URL refusal, which is proven by
  `TestDispatchRefusesABadNatsURLOnceItResolves/a loopback URL with natsInCluster` instead.
- The plan of this change named that second control for the plugin surface; the implementer
  discovered it was unreachable there and moved it, and the tester re-ran both independently.
- `pulumi package get-schema` of the built plugin listed `natsInCluster`, `natsReplicas` and
  `streamMaxBytes` with their types and `plain: true`, none in `requiredInputs`, and no
  `description` on any of the resource's 52 input properties (53 since sjawhar/legion#2528 added
  `clusterOnly`).
