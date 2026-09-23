---
title: Coordinator state and issue record
parent: daemon-go
depends_on: [contracts]
paths: [packages/daemon-go/internal/store, packages/daemon-go/internal/record, packages/daemon-go/internal/projection, packages/daemon-go/internal/registry]
---
The coordinator's Postgres-owned state: migrations, the immediately queryable per-issue record, admission slots, phase and gate facts, claims, the transactional outbox, and the local daemon registry. It is not a mirror of Dispatch or GitHub.
