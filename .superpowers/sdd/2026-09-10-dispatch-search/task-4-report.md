# Task 4 report — Envoy clients, hosts, and search-first skill

## Delivered

- Added `DispatchClient.search(query, { project?, limit? })`, sending authenticated `GET /api/v1/search` requests with URL-encoded query parameters.
- Made `dispatch_search` issue-free in the executor and render a singular/plural result heading, each result's issue key, status, title, kind, artifact name, terminal-safe `snippetText()` output, and absolute deep link without a subscription topic.
- Made `dispatch_issue` forward `force` and return `POSSIBLE_DUPLICATE` candidates as a readable tool result; malformed candidates and unrelated errors rethrow.
- Pinned all three host rosters to eleven shared native Dispatch tools and verified Pi executes `dispatch_search` without an issue.
- Updated the host and contract tool documentation and added the Dispatch skill’s required Search first guidance, including citations, Websearch syntax, and duplicate handling.

## Red-green evidence

Tests were added before the client implementation. The focused client run failed because `DispatchClient.search` did not exist, `dispatch_search` still required an issue, duplicate candidates threw, and `force` was absent from the POST body. The Pi host test initially failed because `dispatch_search` returned an issue-required tool error. It passed after the issue-free executor and client request path were implemented. The one-result rendering assertion then caught the incorrect `1 results` grammar; the executor now emits `1 result`. A review-added renderer expectation failed before its fix because the output omitted the issue title and used indistinct punctuation. It now makes the search target explicit: `KEY [status] title - kind artifact: snippet -> absolute link`. The OpenCode and Claude roster pins were green immediately after their expected tables were extended because their adapters already enumerate the shared contract dynamically.

## Verification

- `env -C packages/envoy-client bun run lint && bun run typecheck && bun test`: Biome checked 24 files without fixes; TypeScript completed without diagnostics; Bun test reported 134 pass, 0 fail.
- `env -C packages/pi-envoy bun test`: 120 pass, 0 fail.
- `env -C packages/envoy-plugin bun test`: 61 pass, 0 fail.
- `env -C packages/claude-envoy-bridge bunx tsc --noEmit && bun test`: TypeScript completed without diagnostics; Bun test reported 46 pass, 0 fail.
- Direct executor smoke with a complete mocked `GET /api/v1/search` response returned `No results for "astrolabe".` and confirmed no search subscription topic.

## Hardening ledger

- `packages/envoy-client/src/dispatch-execute.ts` — The planned live OMP-to-Task-2 server smoke was not run. This task’s required `ff-search` base deliberately lacks the Go endpoint, and the Task 2 worker’s temporary smoke server was removed after its own verification. Finishing this check requires stacking the Task 2 search endpoint, starting Dispatch with an `astrolabe` fixture and duplicate `LOCAL` issue, then invoking both tools through OMP.

## Task change

`numymkoz` — `feat(envoy-client): dispatch_search tool and duplicate-aware dispatch_issue; skill says search first`
