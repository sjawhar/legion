---
title: "A graph read skips a node kind its build cannot address, and one registry drives its lists and its counts"
category: architecture-patterns
tags:
  - dispatch
  - reference-graph
  - rolling-deploy
  - graph_edges
  - backlink-count
date: 2026-10-25
status: active
module: packages/envoy/internal/dispatch/refs
related_issues:
  - "INFRA-364"
  - "sjawhar/legion#2201"
---

# A graph read skips a node kind its build cannot address, and one registry drives its lists and its counts

- **Two builds read one database during a rolling deploy.** A row a newer build writes (a new node
  kind in `graph_edges`) is read by the older build until the rollout ends. A reader that fails on
  a value it does not know turns the newer build's first write into a 500 on every read that meets
  it. Skip what this build cannot address, as an edge whose other end no longer exists is skipped.
- **One list of node kinds, read by every reader.** In Dispatch it is `nodeLoaders` in
  `refs/graph.go` (kind → loader). `resolveNodes` looks a kind up there; `nodeKinds`, its sorted
  keys, is the SQL bind of `BacklinkCounts` (`from_kind = any($3)`) and `Outgoing`
  (`e.to_kind = any($2)`). A second list beside the loader (a `switch` plus a slice) lets the next
  kind be added to one and forgotten in the other, which silently leaves its edges uncounted or
  unlisted. Adding a node kind is one map entry: it then resolves, lists and counts at once.
- **A count beside a list filters the same rows the list shows.** The list dropped an unknown kind
  in Go (the node did not resolve); the count read SQL and kept it, so `referenced_by_count` was
  one higher than the panel it opens. When a list drops a row after the query, give the count the
  same condition in SQL.
- **Filter by the kind, never by a missing join.** `Outgoing` left-joins `artifacts` and lists an
  unresolved target on purpose (another issue, a missing document). "Join found nothing" would
  have dropped those too; `to_kind = any(nodeKinds)` drops only what this build cannot read.
- **Node kinds are not edge kinds.** `refs.Kinds`/`KnownKind` are edge types (`mentions`,
  `part_of`, …); the registry is the ends of an edge. Do not merge the two lists.
- **The fix ships before the kind does.** The build that skips must run in production before the
  build that writes the new kind merges; say so in both pull requests.

## Evidence

INFRA-364 (sjawhar/legion#2201) ran ahead of dispatch://INFRA-366, which adds `block` nodes. Before
the fix, the local harness with one `('future','x#y','issue','CORE-1')` row answered `500 graph node
kind "future" is not addressable` on `GET /api/v1/references` (`?to=` and `?from=`) and on
`GET /api/v1/artifacts/{id}/references`, `dispatch read` printed one `unavailable` row, the issue
header said `Referenced by (3)` over a panel that could list two, an ask counted 2 for one citer,
and a document's `outgoing` listed `{"kind":"future"}`. After it every read answered 200 with the
known edges and each count equalled its list. INFRA-366's tree plan said "add a `case "block"` to
`resolveNodes`"; with the registry that step became one `nodeLoaders` entry.
