---
title: "A bridged provider's schema default is not the request: read the client's JSON tags"
category: infra
tags:
  - pulumi
  - terraform-provider
  - documentation
  - tailscale
date: 2026-11-02
status: active
module: packages/envoy/infra
related_issues:
  - "sjawhar/legion#2668"
---

# A bridged provider's schema default is not the request: read the client's JSON tags

- A Terraform-bridged provider's schema tells you what the state will hold. It does not tell you
  what the HTTP request carries. Follow the value from the schema into the client struct and read
  its JSON tags: an `omitempty` on a slice or a map drops the zero value, so a schema default of
  "empty" and a request field that is absent are the same thing in state and different things on
  the wire.
- A prose claim about a dependency's behaviour needs the same citation as a code claim. Cite the
  schema, the client's encoding and the vendor's reference, at the versions the module pins.
- When the vendor's reference does not define what an omitted field does to a value that already
  exists, write that it is undefined. Do not assert either outcome, and do not infer one from the
  schema's default, which never reaches the request.
- A field the schema defaults and the client omits is a drift candidate: if the API keeps the live
  value, a read writes it back into state, the default plans the empty value, the update sends no
  field, and the next refresh sees it again.

## Evidence

sjawhar/legion#2668 removed the `Tags` field from the Tailscale Service `gcp:Dispatch` declares.
Round 2 of the implementing phase wrote into `packages/envoy/infra/README.md` that a first apply
adopting a live `svc:<hostname>` "clears its tags", reasoning from the provider's schema alone. The
reviewer refused that sentence and corrected it in its own commit, `26127723b7df`.

Read at the pins. `packages/envoy/infra/go.mod:24` pins `pulumi-tailscale/sdk v0.29.2`, which
bridges `tailscale/terraform-provider-tailscale` v0.29.2:

- `tailscale/resource_service.go:101-106` declares `tags` `Computed: true, Optional: true,
  Default: setdefault.StaticValue(types.SetValueMust(types.StringType, []attr.Value{}))`, so a
  configuration that sets no tags resolves to the empty set in state.
- `resource_service.go:198-213`, `buildServiceFromResource`, turns that empty set into a
  zero-length `[]string` on the client's `VIPService`.
- That client is `tailscale.com/client/tailscale/v2 v2.9.0` (the provider's `go.mod`), whose
  `vip_services.go:23` declares ``Tags []string `json:"tags,omitempty"` `` and whose
  `CreateOrUpdate` (`vip_services.go:55`) `PUT`s that struct. A zero-length slice is omitted, so
  the request carries no `tags` key at all.
- Tailscale's reference for `PUT /tailnet/{tailnet}/services/{serviceName}` says nothing about an
  omitted field.

So the schema's "empty set" never reached the wire, and what the apply does to a live Service's
tags is not something either source settles. The README now says the request carries no tags and
that the reference leaves the live tags' fate open.

[INFERENCE] The drift loop is derived from the pinned schema and client — the resource's read
writes the API's tags into state and normalises `nil` to the empty set — and was not observed. It
is worth watching on the first live apply that adopts a Service carrying tags.
