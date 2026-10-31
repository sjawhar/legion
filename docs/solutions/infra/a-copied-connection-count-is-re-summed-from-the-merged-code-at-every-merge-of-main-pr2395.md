---
title: "A copied connection count is re-summed from the merged code at every merge of main"
category: infra
tags:
  - max_connections
  - go-modules
  - merge
  - connection-pool
date: 2026-10-28
status: active
module: packages/envoy/infra/gcp
related_issues:
  - "sjawhar/legion#2395"
---

# A copied connection count is re-summed from the merged code at every merge of main

Extends docs/solutions/infra/a-shared-limit-during-a-roll-counts-terminating-pods-state-the-rule-not-a-replica-count-INFRA-560.md.

- **After each merge of main, set the copy to the merged sum.** `dispatchConnectionsPerPod`
  (`infra/gcp/dispatch.go`) copies `store.ConnectionsPerProcess`. When main and the branch both
  change pools, neither side's number is right: read the merged `store/pool.go`, add every pool and
  permanent listener, and write that. Never take main's line as it is: main can itself be stale.
- **A permanent LISTEN connection is a connection.** A new `pglisten.Listener` raises
  `permanentListeners`; a pool that grows raises its own size.
- **Run the comparison test from its own module.** `TestDispatchConnectionsPerPodIsTheServers`
  lives in the `packages/envoy/infra` module, so `go test ./...` from `packages/envoy` does not run
  it: run `cd packages/envoy/infra && go test -run TestDispatchConnectionsPerPodIsTheServers ./gcp/`.
  That run can rewrite `packages/envoy/infra/go.work.sum`; restore the file before you commit.

## Evidence

- This branch's count went 85, 86, 88 across three merges of main. At one head the test failed
  with 85 against 86, because the merge added this branch's +2 to main's stale 83 instead of
  re-summing. After main added a registry pool, the merged sum was 88
  (`store/pool.go:316`, `infra/gcp/dispatch.go:97`).
- A local run of the gcp test rewrote `packages/envoy/infra/go.work.sum`, and the change went
  into commit 40ca6c2f; 7a86169c restored it.
