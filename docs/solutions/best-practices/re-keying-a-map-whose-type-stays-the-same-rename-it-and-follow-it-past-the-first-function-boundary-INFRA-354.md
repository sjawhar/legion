---
title: "Re-keying a map whose type stays the same: rename it, and follow it past the first function boundary"
category: best-practices
tags:
  - go
  - refactoring
  - rename
  - map
  - compile-errors
date: 2026-10-25
status: active
module: packages/envoy
related_issues:
  - "INFRA-354"
  - "sjawhar/legion#2185"
---

# Re-keying a map whose type stays the same: rename it, and follow it past the first function boundary

- When you reverse what a map is keyed by and its type does not change (`map[string]string` before
  and after), **rename the field**. Every site that names the field then fails to compile until
  someone reads it again. This is the only mechanical check you get. Without the rename, each
  literal and each assertion keeps compiling with its meaning reversed, and a test can still pass
  for the wrong reason.
- The rename stops at the first function boundary. A parameter or local that receives the map
  (`func requester(…, services map[string]string)`) keeps its old name and still compiles.
  Rename or audit each of them, and read each lookup inside them, because a lookup with the old
  direction compiles too.
- When the relation goes from one-to-one to many-to-one, two more things change:
  - Every consumer of the values (`maps.Values`, a `range` over values) now sees duplicates.
    Deduplicate them where the consumer needs a set.
  - A uniqueness refusal moves to the other side of the map. The case where both entries are
    equal becomes reachable, so give it its own message. Without one it reads
    `for both "a" and "a"`.

## Evidence

INFRA-354 (sjawhar/legion#2185) re-keyed the secrets broker's `BROKER_SERVICES` map from
service → account to account → service, so one service may have several accounts.

- The plan's gap analysis (finding F3) counted six construction and assertion sites that would keep
  compiling with the meaning reversed. One of them was
  `TestAServiceGrantStopsWhenItsServiceAccountChanges` in
  `packages/envoy/internal/broker/requests/machine_test.go`: read in the new direction, its
  replacement map would have kept its assertion passing for a different reason. Renaming
  `ServiceAccounts` to `ServicesByAccount` on `config.Config` and `requests.Machine` made all nine
  sites fail until they were read, and the replacement map now names a different account.
- `requester(ctx, q, services map[string]string)` in
  `packages/envoy/internal/broker/requests/machine_state.go` kept its parameter name. The
  implementer's negative control put the old service-keyed lookup back inside that function, and
  it compiled. Only tests caught it.
- `cmd/broker/main.go` hands the policy loader the distinct services:
  `slices.Compact(slices.Sorted(maps.Values(cfg.ServicesByAccount)))`.
- `parseServiceAccounts` in `packages/envoy/internal/broker/config/config.go` gained a same-service
  wording, `names the service account X twice for "a"`, for the entry written twice. Before the
  change that case never reached it, because the duplicate-service refusal caught it first.
