---
title: "A field of a provider's request can need a permission the consumer's credential lacks, and no mock sees it"
category: infra
tags:
  - pulumi
  - tailscale
  - tailscale-services
  - credentials
  - grants
date: 2026-11-02
status: active
module: packages/envoy/infra/gcp
related_issues:
  - "sjawhar/legion#2668"
---

# A field of a provider's request can need a permission the consumer's credential lacks, and no mock sees it

Extends docs/solutions/infra/moving-a-tailnet-name-from-a-node-to-a-tailscale-service-needs-a-client-grant-to-the-service-INFRA-560.md.

- A vendor's reference documents a permission per endpoint. "The credential may call this
  endpoint" is not "the credential may set every field of it". Before a component puts one of its
  inputs into a resource argument, ask which credential the consumer gives that provider and
  whether the field is inside it.
- A field the component gains nothing from is not worth that risk. Send it only where the
  component needs what it does.
- No mock-engine test can see this class of refusal. The mocks record a registration and answer
  from its inputs; nothing calls the provider, so nothing calls the vendor, so the vendor's
  authorization never runs. Only a live apply shows it, and the refusal arrives mid-apply, after
  whatever destructive step came first.
- A consumer carrying a transform that strips one of a component's inputs is an unfiled bug report
  against the component. Read it that way.
- For a Tailscale Service specifically, the document this one extends offers two ways to give
  clients access: a grant with `dst: ["svc:<name>"]`, or a tag on the Service that their grants
  already name. The second no longer exists for a Service `gcp:Dispatch` declares: it carries no
  tags, and the OAuth client that creates it may be allowed to set none. Use the `dst` grant on
  the Service's own name. The hosts' tags still matter, for `autoApprovers.services` alone.

## Evidence

sjawhar/legion#2668. `envoy-infra`'s `gcp:Dispatch` declared its Tailscale Service
`svc:<hostname>` with the component's own `tags`. The OAuth client a consumer gives the
`tailscale` provider holds the `services` scope, which lets it write the Service; it may set no
tags, and Tailscale answered `requested tags ... are invalid or not permitted (400)`. That stopped
a production apply after it had already deleted the old tailnet device, so Dispatch had no tailnet
name until a workaround landed in the consumer — a stack transform that stripped the tags before
the request. Tailscale's API reference for `PUT /tailnet/{tailnet}/services/{serviceName}` states
the scope (`services`) and describes `tags` as "a list of optional tags associated with the
Service", with nothing about who may set them.

The fix declares the Service with no `Tags`
(`packages/envoy/infra/gcp/dispatch_frontdoor.go:502-506`). The component's `tags` input is
untouched and still reaches the front door's nodes, which is where it is needed:
`dispatch_frontdoor.go:576` passes `tags: s.tags` to the pod, and
`packages/envoy/infra/gcp/tailnet_service.go:163` renders
`TS_EXTRA_ARGS=--advertise-tags=<tags>`, which is what `autoApprovers.services` approves.

Why no test caught it: `packages/envoy/infra/internal/pulumitest/mocks.go` answers every
registration from its own inputs and calls no provider, so the component's whole unit suite ran
green with a Service the vendor would refuse. `gcp:Dispatch`'s Service is the only
`tailscale.NewService` in the module; `gcp:ClusteredNatsHub` takes a Service name as an input
rather than declaring one.
