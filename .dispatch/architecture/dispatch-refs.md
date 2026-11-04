---
title: Reference graph
parent: dispatch-server
depends_on: [dispatch-model, dispatch-text]
paths: [packages/envoy/internal/dispatch/refs]
---
Mentions, structural containment, and typed relations are derived by `graph_edges`; identity and recorded session/role delivery by `graph_person_edges`; indexed delivery to people by `graph_inbox_edges`.
`AllEdgesSQL` is their shared union for new named reads, leaving existing builds' graph reads unchanged during a rollout or rollback.
Person nodes use canonical logins from `person_aliases`; `authored_by` identifies a person or session, and `delivered_to` identifies a recorded session/role destination or a person addressed by an ask, indexed thread reply or direct reply.
`rebuild.go` reconciles the citation rows the existing writers index, not person or delivery edges.
Like the broker, the package works through a caller's transaction rather than opening a database of its own.
