# Dispatch search — design

## Decisions needed

None. Closed and done issues are included in results (dimmed by status) and in the duplicate gate; `force: true` is the escape. Decided by Main 2026-09-10 17:58Z on the plan's recommendation: re-filing finished work is exactly the duplicate the gate exists to catch.

## Acceptance

1. Browser (chromium): signed in at `/`, `Ctrl/Cmd+K` opens the palette; typing `astrolabe` lists hits grouped by issue with the term in `<mark>`; `ArrowDown`+`Enter` on the document hit lands on `/issues/<KEY>/spec?q=astrolabe` (non-primary: `/issues/<KEY>/artifacts/<slug>?q=astrolabe`) with the first occurrence highlighted and scrolled into view. `e2e/search.e2e.ts`.
2. Phone (iphone project): drawer → `Search` button → same palette; every result row is ≥ 44 px tall; tapping navigates and closes the palette. `e2e/search.e2e.ts`.
3. The `Search` control sits at the top of the navigation rail on both layouts; `Esc` closes the palette and returns focus to the control that opened it.
4. API: `curl -H 'X-Dispatch-User: alice' '$URL/api/v1/search?q=astrolabe'` → 200 `{results:[{kind,issue:{key,title,status},artifact?,id,snippet,rank,href}],took_ms}`; the same with `Authorization: Bearer $DISPATCH_AGENT_TOKEN`. Go `api/search_test.go`.
5. Agent: `dispatch_search({query:"astrolabe"})` from an OMP session returns the document/comment containing it with the issue key and an absolute link. envoy-client + pi-envoy tests; live: `omp` in the worktree against the dev server.
6. Agent: `dispatch_issue({project,title})` whose title near-duplicates an existing issue in that project returns the candidates and creates nothing (server `409 POSSIBLE_DUPLICATE`); the same call with `force: true` creates it. Go `api/duplicates_test.go`, envoy-client tests.
7. Latency: p95 < 100 ms for `GET /api/v1/search` over the live corpus copy (`scripts/restore-dispatch-dump.sh ~/tmp/dispatch-preflight/live-*.dump`, then `TestSearchLatencyOnCorpus`).
8. `q` under 2 characters, or stop words only, → `400 INVALID_QUERY`; `limit` outside 1..50 → `400 INVALID_LIMIT`.
9. `skills/dispatch/SKILL.md` tells agents to search before creating an issue or a design document; read against the behaviour in 5–6.

## Requirements

| Requirement | Provenance |
| --- | --- |
| Keyword search across every issue and document, fast, from anywhere in the app | Sami 2026-09-10 17:35Z: "I want some way to search for a keyword across all documents and issues. Like, I know I've used the word astrolabe somewhere in one of these issues. I just don't know where to find it. So I need some way to search. We need a fast global search." |
| Agents get the same search as a tool, and creation is gated on it | Sami 2026-09-10 17:45Z: "agents need a way to search too, otherwise many duplicate issues" |
| Corpus = issues (key, title), latest version of every `kind='doc'` artifact, comments, asks (question + answer text), messages | inferred: these are the places a word is written in Dispatch; refs/events restate them |
| Postgres full-text search in the existing database; no second service | inferred: ~100 documents, one Postgres already backed up; standing rule "boring standard mechanism" |
| Results carry `href` deep links so a hit is one click from the text | inferred: "I just don't know where to find it" is a navigation problem |
| Live document text reaches the index at settle (2 s idle, `docs.Deps.Settle`); the palette shows the last settled text | fact: `artifact_versions` is written by `writeVersionTx` on settle; the search reads versions |

## Design

- Migration `store/migrations/0010_search.up.sql` (0008 is PR 3's Go marker, 0009 is the project-documents plan; re-check `ls store/migrations` against `main@origin` at every rebase): `issues.search` = `setweight(to_tsvector('english', key),'A') || setweight(to_tsvector('english', title),'A')`; `artifact_versions.search` = `to_tsvector('english', coalesce(markdown,''))` (stored, no index: the query reads exactly one latest row per document through `artifact_versions_artifact_id_number_key`); `comments.search` (body), `asks.search` (question + `answer->>'text'`), `messages.search` (body); GIN on the four indexed columns. All generated columns; no application code writes them.
- `GET /api/v1/search?q=&project=&limit=` (`api/search.go`, `requireAuthenticated`): `websearch_to_tsquery('english', q)`; five `union all` branches ranked by `ts_rank_cd`; `order by rank desc, issue updated_at desc`; `limit` default 20, max 50. Snippet: `ts_headline` over a 4 000-char window around the first raw-term occurrence (fallback: document start) with private-use sentinels `\uE000`/`\uE001`; Go HTML-escapes the text and turns sentinels into `<mark>`/`</mark>` — `<mark>` is the only markup. Measured on the live copy: 20 document headlines over full text 143 ms, over the window 33 ms; ranking alone < 3 ms.
- `href`: issue `/issues/KEY`; document `/issues/KEY/spec?q=` (primary) or `/issues/KEY/artifacts/<slug>?q=`; comment `/issues/KEY/comments/<id>`; ask `/issues/KEY/asks/<id>`; message `/issues/KEY/log`. `q` in `href` is the raw query.
- Duplicate gate (`api/duplicates.go`, called by `createIssue` before its transaction; skipped for `external` refs and `force: true`): `A` = stemmed non-stopword lexemes of the new title minus the declared parent's title lexemes; for every issue in the same project except the parent, `B` likewise; `shared = |A ∩ B|`, `shorter = min(|A|,|B|)`; candidate when `shared ≥ 3 and 2·shared ≥ shorter`, or `shared = shorter ≥ 1`. Any candidate → `409 POSSIBLE_DUPLICATE {candidates:[{key,title,status,snippet,shared_terms,href}]}` (≤ 5, by `shared` then `updated_at`). On the live corpus this flags 12 of 820 native title pairs, all re-runs (smoke attempts, `v2`/`v3`, the same program filed twice).
- Contracts: `dispatch_search({query, project?, limit?})` (`dispatch-tools.ts`); `dispatch_issue` gains `force`; `SearchResult`, `SearchResponse`, `DuplicateCandidate`, `snippetSegments()` (`dispatch-snippet.ts`, shared by SPA and envoy-client). envoy-client `DispatchClient.search`, `executeDispatchTool` case `dispatch_search` (no issue required, like `dispatch_issue`), and `dispatch_issue` renders `POSSIBLE_DUPLICATE` candidates as a result instead of throwing.
- SPA `features/search/`: `SearchPalette` (combobox + listbox, `useDialog`, 150 ms debounce, grouped by issue, kind badge, `<mark>` via `snippetSegments`, `status === "done"` dimmed), `SearchButton` in `NavigationContents`, `useSearchShortcut` (`Ctrl/Cmd+K`), `search-model.ts`; `DocView` gains `highlightTerm` (`?q=`, first text-node occurrence wrapped in `mark.dispatch-anchor-history`, `scrollIntoView`); after PR 4 the Proof editor host implements the same `?q=` contract as a ProseMirror inline decoration. Colours only from `theme/classes.ts` (`SEARCH_HIT_BG/TEXT` added there).

## Errors

| Condition | Behaviour |
| --- | --- |
| `q` missing, < 2 chars after trim, or `numnode(websearch_to_tsquery) = 0` | `400 INVALID_QUERY` with the reason |
| `limit` not an integer in 1..50 | `400 INVALID_LIMIT` |
| Unauthenticated | `401` via `requireAuthenticated`, as every list route |
| Postgres error | `500 INTERNAL` through `writeHandlerError`; the SPA shows `QueryError` with Retry; the tool throws `DispatchServiceError` |
| Title near-duplicates an issue in the project | `409 POSSIBLE_DUPLICATE` + candidates; nothing written, no issue number consumed |
| `dispatch_search` query under 2 characters | Zod rejects before any request |
| `?q=` term absent from the rendered document | no mark, status line "`<term>` is not in this version" (same slot as the version-highlight notice) |

## Testing

| Acceptance | Proof |
| --- | --- |
| 1, 2, 3 | `packages/dispatch/e2e/search.e2e.ts` (chromium + iphone); `SearchPalette.test.tsx`, `search-model.test.ts`, `DocView.test.tsx` |
| 4, 8 | `api/search_test.go`: every kind, latest version only, project filter, limits, escaping, ranking, auth |
| 5 | `envoy-client/src/__tests__/dispatch-execute.test.ts`, `dispatch-http.test.ts`; `pi-envoy/extensions/envoy.test.ts` registration and execution |
| 6 | `api/duplicates_test.go`; envoy-client `dispatch_issue` 409 rendering and `force` forwarding |
| 7 | `api/search_bench_test.go` `TestSearchLatencyOnCorpus` (env-gated) over `scripts/restore-dispatch-dump.sh` |
| 9 | read of `skills/dispatch/SKILL.md` § Search first against 5–6 |

## Rejected

- Meilisearch / Typesense / Elasticsearch: a second service to run, back up, and keep consistent for ~100 documents.
- `LIKE`/`ILIKE` scans: no stemming, no ranking, no snippets, sequential scans over 2.8 MB of markdown per keystroke.
- Duplicate threshold `ts_rank_cd ≥ 0.1`: one shared word already scores 0.1 (measured), and the scale moves with proximity and length.
- Matching the spec's first 200 chars against documents for the gate: two design documents always share ≥ 3 stemmed terms; the title is the identity being duplicated.
- Gate across all projects: LEGSMOKE deliberately re-files LEGION titles; a project is a separate line of work.
- Results list inside the rail: a second results UI to keep in sync with the palette; the rail control opens the palette instead.
- GIN on `artifact_versions.search`: the latest-version join reads one row per document; an index over every version could only surface stale versions.
