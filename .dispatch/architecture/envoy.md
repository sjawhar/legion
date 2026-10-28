---
title: Envoy
parent: legion
paths: [packages/envoy]
---
The Go-based event transport: webhook ingress, NATS routing and delivery, the native Dispatch server, and the MCP bridge. Dispatch's database is PostgreSQL 18 with pgvector: `envoy-infra`'s `gcp:Dispatch` defaults a new instance to `POSTGRES_18`, and `internal/testpg`, `scripts/dev-postgres.sh` and `deploy/compose/dispatch.acceptance.compose.yml` pin one pgvector-on-18 image by digest.
