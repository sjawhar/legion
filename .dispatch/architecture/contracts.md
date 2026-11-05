---
title: Contracts
parent: legion
paths: [packages/contracts, packages/envoy/internal/contracts]
---
The language-neutral source of truth for event envelopes, subjects, the Dispatch API types (among them `SearchAsk`, the answer state a search hit for an ask carries), and the Dispatch specs the `dispatch` command is built from. `packages/envoy/internal/contracts` is the Go package `bun run gen:go` writes from it; `cmd/listener`, `internal/bus`, `internal/cistore`, `internal/session`, `internal/store`, `internal/mcpbridge` and `internal/dispatch/outbox` import that package directly, so it belongs to this component even though it sits inside `packages/envoy`.

It holds the related-item check's result types (`Suggestions` with its outcome and search mode, `SearchAsk`, `DecisionBlockCheck`, `RelatedChecksReport`) and the `dispatch_related` spec, which runs the check on a text without writing anything.

It also holds the refusal codes the server and every client must spell alike, among them `DISPATCH_SIGN_IN_REQUIRED` (`SIGN_IN_REQUIRED`), the `403` for a write by an access token the secrets broker issued that names a session and a service but no person: `bun run gen:go` writes it into the Go package as `contracts.DispatchSignInRequired`, as it does `DISPATCH_LOGIN_CHANGED`, so the server's refusal and a client's reading of it cannot drift apart.

Its `fixtures/daemon-api/` goldens are the daemon's own state answer, written by `packages/daemon/internal/api`'s golden test: `settings.repository_renames` is among them, the `{from, to}` pairs a deployment tells the daemon to treat as one repository, `[]` when it configures none. `src/legion-api.ts`'s `legionSettings` stays loose, so a new settings key needs no schema change.
