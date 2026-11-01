---
title: Contracts
parent: legion
paths: [packages/contracts, packages/envoy/internal/contracts]
---
The language-neutral source of truth for event envelopes, subjects, the Dispatch API types (among them `SearchAsk`, the answer state a search hit for an ask carries), and the Dispatch specs the `dispatch` command is built from. `packages/envoy/internal/contracts` is the Go package `bun run gen:go` writes from it; `cmd/listener`, `internal/bus`, `internal/cistore`, `internal/session`, `internal/store`, `internal/mcpbridge` and `internal/dispatch/outbox` import that package directly, so it belongs to this component even though it sits inside `packages/envoy`.

It holds the related-item check's result types (`Suggestions` with its outcome and search mode, `SearchAsk`, `DecisionBlockCheck`, `RelatedChecksReport`) and the `dispatch_related` spec, which runs the check on a text without writing anything.
