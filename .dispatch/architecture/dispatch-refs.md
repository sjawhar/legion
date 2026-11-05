---
title: Reference graph
parent: dispatch-server
depends_on: [dispatch-model, dispatch-text]
paths: [packages/envoy/internal/dispatch/refs]
---
Mentions, structural containment, and typed relations are derived by `graph_edges`; identity and recorded delivery are derived by `graph_person_edges`.
`AllEdgesSQL` is their shared union for new named reads, leaving existing builds' graph reads unchanged during a rollout or rollback.
Person nodes use canonical logins from `person_aliases`; `authored_by` identifies a person or session, and `delivered_to` identifies a recorded session or role destination.
`SessionReceiptsSQL` is the one narrowed form of that delivery relation — the recorded topics and sent attempts that reached a named set of sessions since a bound, which the whole-graph union cannot answer from an index — and the person page and the delivery census both run it.
`rebuild.go` reconciles the citation rows the existing writers index, not person or delivery edges.
Like the broker, the package works through a caller's transaction rather than opening a database of its own.
