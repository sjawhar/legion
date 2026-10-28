---
title: Event broker and outbound Envoy client
parent: dispatch-server
depends_on: [dispatch-model, dispatch-store]
paths: [packages/envoy/internal/dispatch/events, packages/envoy/internal/dispatch/envoy]
---
The append-only event log every Dispatch write lands in. `Broker.Append` takes the event owner's advisory lock (`store.LockOwner`, which returns at once when the transaction took it in its read phase), then the owner's row `for no key update`, which reads the sequence the append continues and is the write-phase statement that gives a transaction appending its first event its transaction id, and then a global advisory lock that orders event ids as their transactions commit. `LockOwners` takes the locks of several owners up front, sorted and deduplicated by lock key (`store.LockOwners`), so no owner lock is waited for after the commit-order lock. An append a document ledger queues runs in the transaction's write phase, in the order the transaction made it. The broker writes through the transaction its caller hands it and imports `dispatch-store` only for the owner lock. `internal/dispatch/envoy` is the small outbound client that calls the Envoy listener's API; it exists only to carry broker-produced events onward, so the two are one component.
