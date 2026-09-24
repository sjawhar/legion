---
title: Proof document service
parent: dispatch-server
depends_on: [dispatch-events, dispatch-model, dispatch-pmdoc, dispatch-refs, dispatch-store]
paths: [packages/envoy/internal/dispatch/docs]
---
The live document surface: the collaborative websocket room, two-phase settlement that mints block ids before it persists and compares what it rendered, the agent edit operations (`replace`, `delete`, `insert`, `retype`, `move`, `delete_row`, `delete_column`), block tokens and preconditions, and ask and comment anchor refresh.
