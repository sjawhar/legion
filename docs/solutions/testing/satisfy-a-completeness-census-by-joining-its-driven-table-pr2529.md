---
title: "Satisfy a completeness census by joining its driven table, never by extending its exemption list"
category: testing
tags:
  - reflection-census
  - table-tests
  - pulumi
  - envoy-infra
date: 2026-10-30
status: active
module: packages/envoy/infra
related_issues:
  - "sjawhar/legion#2529"
---

# Satisfy a completeness census by joining its driven table, never by extending its exemption list

A completeness census enumerates a type by reflection and compares it against what the suite
covers. Its `covered` set is usually the union of a **driven table**, whose every row some test
executes, and a hand-written **exemption list**, which asserts nothing. That makes the census
green under two opposite resolutions, and the cheap one is wrong.

- **When a census fails on a member you added, join the driven table.** Adding the member's name
  to the exemption literal turns the suite green in one line and proves nothing: the member is
  then accepted and silently ignored, which is the defect the census exists to catch.
- **Exempt a member only with a test that drives it**, and say in the exemption's comment which
  test that is. An exemption whose behaviour no test asserts is a hole the census cannot see.
- **Writing such a census: make both halves cost the same.** An exemption list of
  `{name, assert}` pairs, each row executed, leaves no one-line escape; a list of bare strings
  does.

## Evidence

- `packages/envoy/infra/gcp/dispatch_cluster_only_test.go`:
  `TestDispatchEveryInputIsLiveOrRefusedUnderClusterOnly` compares every `pulumi` tag of
  `gcp.DispatchArgs` against the union of `dispatchClusterOnlyRefusals`' rows and the literal
  `[]string{"clusterOnly", "region", "hostname", "deletionProtection"}`. Each refusal row is
  driven by `TestDispatchClusterOnlyRefusesEveryDispatchInput`, which runs it through
  `gcp.ResolveDispatch` and requires the refusal by name. The literal is driven by nothing.
- sjawhar/legion#2529 merged `main` and the census failed, naming `natsInCluster`, `natsReplicas`
  and `streamMaxBytes`: 53 inputs against 50 covered. Either resolution was one line. Adding the
  three names to the literal would have shipped a `clusterOnly` mode that accepts all three and
  builds nothing that reads them — `newDispatchClusterOnlySettings` resolves `Hostname`, `Region`
  and `DeletionProtection` and nothing else. The merge commit `54644c3d1fa0` took the other
  branch: three rows in `dispatchClusterOnlyRefusals` and three in its test table, each then
  driven, and one of them driven through the released plugin as well — a cluster-only program
  setting `natsInCluster` alone is refused `natsInCluster configures Dispatch, which clusterOnly
  does not build: unset it, or unset clusterOnly`.
- The exemption list in that test is sound today because its author gave each of its four names a
  test by hand (`TestDispatchClusterOnlyRegistersTheClusterAlone`,
  `TestDispatchClusterOnlyPassesDeletionProtectionToTheCluster`). Nothing in the test's shape
  required that, which is what this rule is for.
