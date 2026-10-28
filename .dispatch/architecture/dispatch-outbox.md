---
title: Event outbox
parent: dispatch-server
depends_on: [contracts, dispatch-docs, dispatch-events, dispatch-model, dispatch-store, dispatch-text]
paths: [packages/envoy/internal/dispatch/outbox]
---
The publisher that turns each appended event into deliveries and retries them with exponential backoff across its issue, route and author destinations. The one package of the server that imports the generated Go contracts, because the envelope it publishes is the contract. It runs on a one-connection pool of its own, so a full request pool never holds back publishing, and it counts the events it marks published (`Published`) and reads how long the oldest event it still owes has waited (`Lag`), which the metrics endpoint serves.
