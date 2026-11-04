---
title: Reference graph
parent: dispatch-server
depends_on: [dispatch-model, dispatch-text]
paths: [packages/envoy/internal/dispatch/refs, packages/envoy/internal/dispatch/mentiongraph]
---
Citation mentions and structural relations are derived by `graph_edges`; recorded session/role delivery by `graph_person_edges`, indexed delivery to people by `graph_inbox_edges`, and identity mentions by `graph_mention_edges`.
`AllEdgesSQL` is their shared union for new named reads, filtering raw identity storage kinds so no API answer carries a `*_mention` kind.
Identity addresses use the existing reference extraction and indexer; explicit mentions derive from recorded comment/message targets and resolution attempts.
Person nodes use canonical logins from `person_aliases`, and a mentioned session expands to a person only through its verified alias.
`mentiongraph.Recorded` reads those records independently of the graph; `census --mentions` compares sources active in the last 30 days, including empty source sets.
`rebuild.go` reconciles reference rows including identity addresses; it restores unknown records an old build drops, and marks document blocks owed their existing reconciliation.
Like the broker, the package works through a caller's transaction rather than opening a database of its own.
