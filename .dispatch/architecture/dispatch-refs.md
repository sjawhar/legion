---
title: Reference graph
parent: dispatch-server
depends_on: [dispatch-model, dispatch-text]
paths: [packages/envoy/internal/dispatch/refs]
---
Mentions, structural containment, and the typed edges of `graph_edges` — `mentions`, `child_of`, `attached_to`, `anchored_to`, `owned_by`, `replies_to`, `followed_by`, plus the `part_of`, `depends_on` and `affects` edges of the architecture model itself. `rebuild.go` is the reconciliation job that rebuilds the graph from the rows it indexes. Like the broker, it works through a caller's transaction rather than importing the store.
