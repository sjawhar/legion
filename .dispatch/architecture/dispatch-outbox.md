---
title: Event outbox
parent: dispatch-server
depends_on: [contracts, dispatch-docs, dispatch-events, dispatch-model, dispatch-store, dispatch-text]
paths: [packages/envoy/internal/dispatch/outbox]
---
The publisher that turns each appended event into deliveries and retries them with exponential backoff across its issue, route and author destinations. The one package of the server that imports the generated Go contracts, because the envelope it publishes is the contract.
