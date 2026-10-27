---
title: Proof document service
parent: dispatch-server
depends_on: [dispatch-events, dispatch-model, dispatch-pmdoc, dispatch-refs, dispatch-store]
paths: [packages/envoy/internal/dispatch/docs]
---
The live document surface: the collaborative websocket room, two-phase settlement that mints block ids before it persists and compares what it rendered, the agent edit operations (`replace`, `delete`, `insert`, `retype`, `move`, `delete_row`, `delete_column`), block tokens and preconditions, and ask and comment anchor refresh.

A room load or a cold read reads the document's compacted row and the rows written since in one transaction on the rooms pool, buffers them in memory up to 32 MiB, and folds them after that transaction ends (a larger log it folds as it streams it). Compaction folds only rows older than 2 minutes on the database clock and leaves younger rows as written: a per-process queue fed by every append and every load compacts a document once 16 rows after its first are older than 2 minutes or one is older than 15 minutes, and an hourly sweep under the `document-compaction` lease, whose first tick comes within 30 s of start whether or not NATS is configured, compacts what the queues missed. It runs on its own two-connection pool and holds no document lock while it folds; only its short write takes the lock, and that write drops the fold if the rows changed.
