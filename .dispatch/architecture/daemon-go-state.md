---
title: Coordinator state and issue record
parent: daemon-go
depends_on: [daemon-go-supervision, daemon-go-identity]
paths: [packages/daemon-go/internal/store, packages/daemon-go/internal/record, packages/daemon-go/internal/projection, packages/daemon-go/internal/registry]
---
Stage 1 on `main` provides Postgres migrations, boot history, and the local daemon registry. Stage 2 adds durable role claims and pending task delivery. Stage 3 plans the per-issue record, admission slots, phase and gate facts, transactional outbox, and `internal/projection`; the coordinator never mirrors Dispatch or GitHub.
