---
title: "An optional mode that zeroes an input inherits every refusal that input carried"
category: infra
tags:
  - pulumi
  - envoy-infra
  - component-inputs
  - validation
  - error-mapping
date: 2026-10-30
status: active
module: packages/envoy/infra
related_issues:
  - "sjawhar/legion#2529"
---

# An optional mode that zeroes an input inherits every refusal that input carried

Two rules for adding an optional mode to a component that hands a config to a shared renderer.

- **List every refusal of the callee that is conditioned on the input your mode zeroes, and decide
  each one in writing.** A refusal written as "when this field is set, also check that" vanishes
  the moment the field is empty, and it vanishes silently: the rendering succeeds. Re-express the
  ones you still need as the component's own refusal, against the callee's own predicate
  **function**, never against a re-spelling of what that predicate accepts. State in the doc
  comment which refusals you decided were unaffected and why, so the next reader does not redo
  the enumeration.
- **An error-mapping that renames a callee's refusals onto one of your inputs is a standing
  obligation of every later field you add to that callee's config.** A catch-all that turns every
  refusal of `X.Env()` into "your input `y` is wrong" is correct only while `X` refuses nothing
  else. When you add a field to `X`, either bound it yourself earlier — in the resolver, before
  anything registers, so the callee's own bound is unreachable — or narrow the catch-all. Do not
  leave the consumer a refusal that names the wrong input.

## Evidence

- `packages/envoy/infra/core/listener_env.go:236-240`: `Env` refuses a local NATS URL only in the
  `else if c.SOCKS5Proxy != ""` branch. `natsInCluster` resolves `SOCKS5Proxy` to `""`
  (`gcp/dispatch.go:435-440`), so in the new mode a loopback `natsUrl` rendered cleanly and would
  have deployed an app and a listener dialling themselves. The component now refuses it itself:
  `if s.natsInCluster && bus.LocalNATSURL(url)` at `gcp/dispatch_kube.go:280-282`, written against
  `bus.LocalNATSURL` as a function so that a change to which spellings it vouches for needs no
  edit here. Pinned by `TestDispatchRefusesABadNatsURLOnceItResolves/a loopback URL with
  natsInCluster` (`gcp/dispatch_test.go:723`).
- The same enumeration cleared the sibling case: `ENVOY_ALLOW_REMOTE_NATS=1` is decided by the
  URL's own locality (`core/listener_env.go:221,236`), not by the proxy, so it keeps rendering in
  the in-cluster mode — which the pods need, since `bus.refuseRemoteNATS` would otherwise refuse
  every connect to a Service name.
- `gcp/dispatch_kube.go:296` returns every non-`socks5Proxy` refusal of `core.ListenerConfig.Env`
  as a `core.InputError` named `natsUrl`. This change put `NATSReplicas` and `StreamMaxBytes` into
  that config, and core refuses both on its own terms (`core/listener_env.go:172-177`,
  `natsReplicas %d is outside 0-5`), so a `natsReplicas: 6` would have reached the consumer as an
  error naming `natsUrl`. It is discharged in the resolver instead (`gcp/dispatch.go:826-841`,
  which refuses `natsReplicas` outside 1 to `bus.MaxReplicas` and `streamMaxBytes` below 1 before
  anything registers), and the reason is written at `gcp/dispatch_kube.go:271-275`.
