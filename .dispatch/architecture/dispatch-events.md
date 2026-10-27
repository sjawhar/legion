---
title: Event broker and outbound Envoy client
parent: dispatch-server
depends_on: [dispatch-model]
paths: [packages/envoy/internal/dispatch/events, packages/envoy/internal/dispatch/envoy]
---
The append-only event log every Dispatch write lands in: `Broker.Append` allocates each owner's sequence under that owner's row lock and orders commits with a global advisory lock, and `LockOwners` takes multiple owners deterministically. It writes through the transaction its caller hands it rather than importing the store, so it carries no edge to `dispatch-store`. `internal/dispatch/envoy` is the small outbound client that calls the Envoy listener's API; it exists only to carry broker-produced events onward, so the two are one component.

`Stream[T]` is the in-process fan-out: `Subscribe(buffer)` gives each subscriber a channel of the size it chooses, and a subscriber whose channel is full is dropped and its channel closed rather than waited for. `Broker` fans this process's committed events out to the outbox through one, and the API's event stream relay publishes every committed event, with its encoded frame, through another.
