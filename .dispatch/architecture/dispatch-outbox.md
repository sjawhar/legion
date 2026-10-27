---
title: Event outbox
parent: dispatch-server
depends_on: [contracts, dispatch-docs, dispatch-events, dispatch-model, dispatch-store, dispatch-text]
paths: [packages/envoy/internal/dispatch/outbox]
---
The publisher that turns each appended event into deliveries and retries them with exponential backoff across its issue, route and author destinations. It imports the generated Go contracts because the envelope it publishes is the contract. Each destination it reaches is appended to the event's `published_destinations`, and that record is a contract of its own now: besides the retry, which publishes only the destinations not yet recorded, `GET /api/v1/agents/{session_id}/answers` (the store's `SessionAnswers`) lists a person's event for a session exactly when that session's own topic is recorded there. A change to which routes reach a session, or to what is recorded, changes what every agent is shown on its next model request.
