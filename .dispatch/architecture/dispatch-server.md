---
title: Dispatch server
parent: envoy
depends_on: [envoy-listener, dispatch-api, dispatch-architecture, dispatch-auth, dispatch-docs, dispatch-events, dispatch-model, dispatch-outbox, dispatch-refs, dispatch-store]
paths: [packages/envoy/cmd/dispatch, packages/envoy/internal/dispatch/routes]
---
The composition root of the native Dispatch API: `cmd/dispatch` reads its boot configuration from the environment and assembles the server, and `internal/dispatch/routes` is the route table and the router wiring every handler package together. Each subsystem it composes is a child component below. Its wire types match the language-neutral contracts, which is a compatibility rule rather than a Go source dependency, so no edge is drawn for it.
