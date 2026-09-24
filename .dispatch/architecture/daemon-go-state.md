---
title: Coordinator state and issue record
parent: daemon-go
depends_on: [daemon-go-supervision, daemon-go-identity]
paths: [packages/daemon-go/internal/store, packages/daemon-go/internal/registry]
---
Stages 1 and 2 on `main` provide Postgres migrations and boot history (`internal/store`), the local daemon registry, durable role claims, and pending task delivery (`internal/registry`). Stage 3 (LEGION-219) plans the per-issue record, admission slots, phase and gate facts, transactional outbox, and `internal/projection`; the coordinator never mirrors Dispatch or GitHub.
