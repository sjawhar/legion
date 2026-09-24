---
title: Architecture importer
parent: dispatch-server
depends_on: [dispatch-auth, dispatch-events, dispatch-model, dispatch-store, dispatch-text]
paths: [packages/envoy/internal/dispatch/architecture]
---
The importer that reads `.dispatch/architecture/*.md` from a project's source repository, validates the file set whole (`Parse` — one error naming every problem, so an invalid set is refused and the previous model stays up), and projects it into immutable snapshots and the components tables. A five-minute ticker and a manual sync route drive it.
