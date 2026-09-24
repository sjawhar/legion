---
title: Event broker and outbound Envoy client
parent: dispatch-server
depends_on: [dispatch-model]
paths: [packages/envoy/internal/dispatch/events, packages/envoy/internal/dispatch/envoy]
---
The append-only event log every Dispatch write lands in: `Broker.Append` allocates each owner's sequence under that owner's row lock and orders commits with a global advisory lock, and `LockOwners` takes multiple owners deterministically. It writes through the transaction its caller hands it rather than importing the store, so it carries no edge to `dispatch-store`. `internal/dispatch/envoy` is the small outbound client that calls the Envoy listener's API; it exists only to carry broker-produced events onward, so the two are one component.
