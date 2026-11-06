---
title: "A component child's explicit Protect overrides what it inherits, so tie it to an input"
category: infra
tags:
  - pulumi
  - component
  - protect
  - envoy-infra
date: 2026-10-31
status: active
module: packages/envoy/infra
related_issues:
  - "sjawhar/legion#2584"
---

# A component child's explicit Protect overrides what it inherits, so tie it to an input

- **Never hard-code `pulumi.Protect(true)` on a remote component's child.** A child that names its
  own `Protect` keeps it: the SDK takes the parent's only when the child names none. So a fixed
  one overrides whatever the consumer sets on the component, and the consumer is left with a
  resource only a resource transform or a state edit can unprotect.
- **Tie the child's `Protect` to an input the component already has** — the one that guards the
  rest of the deployment's stateful resources — so unprotecting the component unprotects the
  child with it, and a deliberate teardown is one input away.
- **Pin it in both directions.** A test that asserts `GetProtect()` is true with the input unset
  passes just as well against a hard-coded `Protect(true)`. Assert the input's false value too.
- **Open the SDK function before you cite its line numbers.** A plausible-looking range can belong
  to a different function with the same shape of code; the reader who checks it finds the citation
  false and the rule unproven, whatever the rule's merit.

## Evidence

sjawhar/legion#2584, `envoy-infra`'s `gcp:Dispatch`, which declares its own Tailscale Service.

- `packages/envoy/infra/gcp/dispatch_frontdoor.go:499` registers the Service with
  `pulumi.Protect(s.deletionProtection)`, the input that already guards the cluster and the
  database. `TestDispatchProtectsItsTailscaleServiceWithDeletionProtection`
  (`gcp/dispatch_test.go:1555`) pins it over unset, true and false; the negative control replaced
  the line with a hard-coded `pulumi.Protect(true)` and only the false row failed, which is why
  the false row exists.
- The rule behind it is `registerResource` in `pulumi/sdk/v3` v3.268.0's `go/pulumi/context.go`:
  `protect := options.Protect; if parent != nil && protect == nil { protect = parent.getProtect() }`.
- The *reason* written in the comment was wrong twice before it was right, over three review
  rounds. "One the consumer has no way to unprotect" is false, because `pulumi state unprotect
  <urn>` reaches a child. "No resource option of the consumer's own program reaches a component's
  child" is false too, because a resource transform on the component does, as that repository's
  own README says. The third attempt cited `context.go:1450-1453`, which is `readPackageResource`
  and not `registerResource`; the reviewer opened the file and moved the citation.
