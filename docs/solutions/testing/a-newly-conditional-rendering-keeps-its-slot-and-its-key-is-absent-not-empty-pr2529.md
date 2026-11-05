---
title: "A newly conditional rendering keeps its slot, and its key is absent rather than empty"
category: testing
tags:
  - golden-files
  - pulumi
  - envoy-infra
  - environment-variables
  - agreement-test
date: 2026-10-30
status: active
module: packages/envoy/infra
related_issues:
  - "sjawhar/legion#2529"
---

# A newly conditional rendering keeps its slot, and its key is absent rather than empty

Extends docs/solutions/testing/assert-the-rendering-in-the-shape-the-consumer-passes-not-the-default-INFRA-62.md.
That document is about asserting a conditionally built list; these are the authoring rules for
making an entry of such a list conditional in the first place.

- **"Renders byte for byte what it rendered before" is a property of list position.** Make the
  entry conditional in the slot it already occupies and append the new entries after it, then
  re-append whatever followed; do not move it to the end because that is where the `if` is easiest
  to write. The committed goldens are the tripwire, and a golden that changes means the rendering
  moved, never that the golden needs regenerating.
- **An optional setting is an absent key, not an empty value**, because a process reads
  `VAR=""` as a configured empty value — a proxy address of `""`, a limit of zero — and not as
  "unset". Render it through a seam that already drops the empty case rather than adding a second
  boolean beside it.
- **Assert the key's absence, not its value.** A test that compares values passes when one side
  renders nothing and the other renders `""`. Where two renderings of the same settings must
  agree, project each to the set of keys that are **present**, and assert the set's size before
  comparing the two, so two empty projections cannot pass as agreement.

## Evidence

- `packages/envoy/infra/gcp/dispatch_kube.go:575-590`: the app's unconditional environment literal
  now ends at `NATS_NKEY_SEED_FILE`; `ENVOY_NATS_SOCKS5_PROXY` is appended first, keeping the slot
  it always held, then `ENVOY_NATS_REPLICAS` and `ENVOY_STREAM_MAX_BYTES` when set, then
  `ENVOY_URL` and `ENVOY_TOKEN` as before. With the three new inputs unset the rendering is
  byte-identical, which `gcp/testdata/dispatch-*.golden` hold; moving the proxy out of that slot
  while the change was being written failed the golden comparison.
- The empty-is-not-absent seam already existed and was reused rather than duplicated:
  `gcp/tailnet_service.go:169-171` renders `TS_SOCKS5_SERVER` only for a non-empty `socks5Server`,
  so `gcp/dispatch_frontdoor.go:526-534` turns the proxy off by leaving that string empty;
  `core/listener_env.go:250-255` does the same for the two numbers.
- `dispatchNatsSettings` (`gcp/dispatch_test.go:1365`) collects only the keys that are present,
  and `TestDispatchAppAndListenerAgreeOnNats` (`:1428`) asserts `require.Len` on that projection
  (5 keys through the proxy, 4 in the in-cluster mode) before `require.Equal` compares the app's
  with the listener's. The agreement test is what holds the two renderings together — the app's
  list is hand-written while the listener's comes from `core.ListenerConfig.Env`, and the simplify
  pass of this change deliberately did not unify them — and the mode tests use
  `require.NotContains(t, env, "ENVOY_NATS_SOCKS5_PROXY")` on the key.
