---
title: Proof document service
parent: dispatch-server
depends_on: [dispatch-events, dispatch-model, dispatch-pmdoc, dispatch-refs, dispatch-store]
paths: [packages/envoy/internal/dispatch/docs]
---
The live document surface: the collaborative websocket room, two-phase settlement that mints block ids before it persists and compares what it rendered, the agent edit operations (`replace`, `delete`, `insert`, `retype`, `move`, `delete_row`, `delete_column`), block tokens and preconditions, and ask and comment anchor refresh. Every durable writer of a room stores the room's unstored updates through the point it read the room at, in the room's commit order, before its own (`flushThrough`), so the stored updates replay to every version; `envoy-dispatch stored-order` checks that.
