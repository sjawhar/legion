---
title: Contracts
parent: legion
paths: [packages/contracts, packages/envoy/internal/contracts]
---
The language-neutral source of truth for event envelopes, subjects, the Dispatch API types, and the Dispatch specs the `dispatch` command is built from. `packages/envoy/internal/contracts` is the Go package `bun run gen:go` writes from it; `cmd/listener`, `internal/bus`, `internal/cistore`, `internal/session`, `internal/store`, `internal/mcpbridge` and `internal/dispatch/outbox` import that package directly, so it belongs to this component even though it sits inside `packages/envoy`.
