# Dispatch project documents and the reference graph — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** An artifact belongs to a project and needs no issue. A project page lists its issues (grouped by status, filterable) and its documents; a project document has the issue spec's collaboration (live text, comments, suggestions, asks, versions) and its own event stream; `dispatch://<PROJECT>/artifact/<slug>` is a first-class reference; an issue's References section is one recursive query over `refs`; the sidebar is Inbox, Pinned, Projects, Settings; the make-primary toggle is gone.

**Architecture:** One schema migration (`0009`) makes `artifacts.issue_key` nullable, adds `project_key`, a stored `ref_key` (`<owner>/<slug>`), and `last_seq`; `asks`, `comments`, and `events` gain an `artifact_id` owner alternative with an exactly-one-owner check. The Go API grows an `owner` abstraction (issue or unlinked artifact) that every collaboration handler, the event broker, the docs service, and the outbox route through; project routes reuse the issue handlers via `loadArtifactByRefKey`. A new `internal/dispatch/refs` package owns reference writing and the closure query. Contracts and the envoy-client tools accept `project` as the alternative owner. The SPA replaces the issue-list sidebar with projects, adds `/projects/:key[/documents[/:slug]]`, and keys the margin by owner.

**Tech Stack:** Go 1.26 (`github.com/sjawhar/envoy`), Postgres 16 (pgx), `reearth/ygo`, Bun + TypeScript (contracts, envoy-client, SPA React 19 + TanStack Query + React Router 6, Playwright).

**Spec:** `docs/superpowers/specs/2026-09-10-dispatch-project-documents-design.md` (binding). Reference plan shape: `docs/superpowers/plans/2026-09-10-dispatch-tree-server.md` (PR 3).

---

## Decisions needed

None. Sami settled both spec decisions on 2026-09-10 (documents live only on the project page's Documents tab; cross-project links are ordinary references). Everything the spec leaves open is decided in [Decisions where the spec under-specifies](#decisions-where-the-spec-under-specifies-decided-here-not-open-questions); none of those needs his authority.

## Scope boundary

**In:**
- Make-primary removal (spec: "leaves in the make-primary removal PR that follows #881") — Task 1, its own PR.
- Migration `0009_project_artifacts.up.sql`; owner-aware `model`, `events.Broker.Append`, `outbox`, `docs` service; project artifact routes; owner-scoped asks/comments/events on unlinked documents; inbox `document`; `GET /projects` `open_asks`; `GET /issues?pinned=true` and `IssueSummary.labels`; `refs` package with project reference parsing; `GET /issues/{key}/references` (closure + ETag) and `GET /artifacts/{id}/references`; `cmd/natstail` (NATS subject tail for acceptance 8).
- Contracts and envoy-client: `project` owner on seven tools, document topics, nullable `issue_key` types, document event rendering; `skills/dispatch/SKILL.md`.
- SPA: sidebar (Inbox, Pinned, Projects, Settings), `/projects/:key`, `/projects/:key/documents`, `/projects/:key/documents/:slug`, `components/Tabs.tsx`, project page (issue list with filters, document list, New document, Upload), document page on #881's `ArtifactHeader` + `ArtifactDocument`, margin keyed by owner, References section on the Artifacts tab, document asks in the Inbox, SSE invalidation for document events.
- Playwright: `nav.e2e.ts` rewritten, `project.e2e.ts`, `document.e2e.ts`, `phone.e2e.ts` extended; live acceptance script `packages/dispatch/e2e/acceptance/document-roundtrip.sh`.

**Out:** graph canvas; a top-level Documents page; per-user read/pinned state on documents; `dispatch_message` on documents; the Proof editor (PR 4); tree documents and mark anchors (PR 3); deploy.

### In-flight work this plan sequences against (facts as of 2026-09-10)

| Work | Head | What it changes that this plan touches | Rule |
| --- | --- | --- | --- |
| **#881** artifacts in the main column, bookmark `ff-artifacts-tab` (workspace `/home/ubuntu/.worktrees/legion/dn-an`) | `2fc27719fd9d`, moving (fix round) | adds `features/artifacts/ArtifactHeader.tsx`, `ArtifactDocument.tsx`, `ArtifactRoutePanel.tsx`; `features/refs/routes.ts` gains `kind: "artifacts"` and `issueTabForRoute`; `IssueTabs.tsx` gains the Artifacts tab; `/issues/KEY/artifacts[/<slug>[?v=N]]`; margin = Comments + Pinned; edits `ArtifactsTab.tsx`, `IssuePage.tsx`, `app.tsx`, `e2e/artifacts.e2e.ts` | Task 1 and every SPA task start on `main` **after #881 merges**. Read the branch with `jj -R /home/ubuntu/.worktrees/legion/dn-an file show -r ff-artifacts-tab <path>`. |
| **#882** spec-primary layout, bookmark `feat/spec-primary-layout`, stacked on #881 | `01bc9fbf4141` | bare `/issues/KEY` lands on Spec; deletes `BoardStrip.tsx`; open asks ("Needs you") at the top of the margin; rewrites `useMarginItems.ts`, `Margin.tsx`, `MarginSheet.tsx`, `CommentsTab.tsx`, `AskCard.tsx`, `IssuePage.tsx`, `routes.ts`, e2e `nav/phone/margin/layout/inbox/threads/writes/live/attention` | Task 6 (its SPA narrowing step) and the SPA tasks (7, 8) start on `main` **after #882 merges**. |
| **PR 3** Lane B tree server, bookmark `dispatch-doc/tree-server` (workspace `/home/ubuntu/.worktrees/legion/pr3-tree`) | plan committed `5762ab91b60d` | reserves `schema_migrations` version **8** as a Go-written marker (`docs/legacy.go` `legacyMigrationVersion = 8`, no SQL file); rewrites `docs/service.go` and `docs/mutation.go` (`SeedText`/`ReplaceText` return canonical markdown; `settleRoom`, `NamedVersion`, `writeVersionTx`, `indexDocumentReferences` all move); deletes `text/anchor.go`, `text/utf16.go`; SPA moves to quote anchors and a read-only rendered view, deletes CodeMirror, adds `e2e/preview.ts` `selectPreviewText` | This plan's migration is **0009**. The spec's `docs/service.go:299-303, 517-521` line references are main-today; this plan names functions (`settleRoom`, `issueOpen`, `NamedVersion`, `indexDocumentReferences`), never lines. Task 8 starts after PR 3 merges (both rewrite the margin). |
| **PR 4** Proof editor SPA (plan being written) | — | replaces `DocEditor` with the Proof editor; `bob sees alice's text within 1 s` and "opens in the Proof editor" (acceptance 3) exist only after it | The document page renders whatever `ArtifactDocument` renders on `main` at merge time. Acceptance 3's editor clauses are verified after PR 4 by the named scenario `documentCollaborationLive` in `document.e2e.ts` (Task 8 writes its API-edit form; PR 4 upgrades it to typing). |

Merge order: **#881 → Task 1 (PR A) → Tasks 2–5 (PR B, server) → #882 → Task 6 (PR C, contracts + tools + the SPA type narrowing) → Task 7 (PR D) → PR 3 → Task 8 (PR E) → Task 9 (live acceptance, after deploy)**. Tasks 2–5 start on `main` **now**. Task 6 develops in parallel with PR B but bases on `main` after #881, #882, and PR A: widening `issue_key` to `string | null` in `contracts` breaks the SPA typecheck (it imports the contract types directly through `web/src/api/types.ts`), so PR C carries the SPA narrowing for the sites #881 and #882 own.

## Global Constraints

- **Migration numbers are claimed at merge time.** This plan's migration is `store/migrations/0009_project_artifacts.up.sql`. On **every** rebase: `ls packages/envoy/internal/dispatch/store/migrations` against `main@origin`; if a `0009_*.up.sql` exists, renumber to the next free number and update every `9` in `store_test.go` and this plan's expectations. Version 8 is PR 3's Go marker — never create `0008_*.up.sql`. Three collisions happened on 2026-09-09; this check is not optional.
- Exactly one owner everywhere: an ask, comment, or event has `issue_key` xor `artifact_id` (database check + `events.Broker.Append` error). An artifact-owned row belongs only to an **unlinked** artifact (`issue_key is null`); a linked artifact's collaboration stays on its issue.
- Lock order: owning row first (issue row for linked, artifact row for unlinked), then artifact/comment rows — `requireOpenOwner` and `docs.lockArtifactOwner` are the only lock sites.
- `ref_key` (`coalesce(issue_key, project_key) || '/' || slug`) is the one identity for `refs.to_id`, slug uniqueness, and slug routes. `refs.to_id` for an artifact target is always a `ref_key`.
- No silent fallbacks: unknown project → 404 `PROJECT_NOT_FOUND`; owner routes on a linked artifact → 400 `ARTIFACT_LINKED`; `primary` on any upload → 400 `ARTIFACT_INPUT`; both/neither owner on a tool → tool error; malformed `refs.to_id` → boot fails naming the row.
- Event types are unchanged (`artifact.version`, `ask.opened`, …). A document event is the same event on topic `notifications.dispatch.document.<PROJECT>.<slug>.<type>`.
- Red-first: every task's tests are written and run to failure before the implementation.
- Comments describe current behaviour, never history. No TODO/placeholder/`follow-up` anywhere in the diff.
- Go checks while implementing are package-scoped: `cd packages/envoy && gofmt -l ./internal/dispatch ./cmd && go vet ./internal/dispatch/... ./cmd/... && DISPATCH_TEST_DATABASE_URL=postgres://postgres:dispatch@127.0.0.1:55432/dispatch?sslmode=disable go test ./internal/dispatch/<pkg>/ -run <Test> -v -count=1`. Postgres: `packages/envoy/scripts/dev-postgres.sh` prints the URL; the API/docs/outbox/store tests each create an isolated database (`openEmptyTestStore`/`openTestStore`).
- TS checks per package: `bun run lint && bun run typecheck && bun run test` (`contracts` also `bun run build`); `bun install --frozen-lockfile` at the repo root must pass.
- Playwright: `cd packages/dispatch && bun run e2e` boots the real server via `e2e/run-server.sh` (`DISPATCH_NATS_DISABLED=1`, header identity `alice`/`bob`, port 8777); both `chromium` and `iphone` projects must pass.
- jj, not git: one task = one commit (`jj describe -m "<msg>" && jj new`); bookmarks per PR as named in [PR strategy](#pr-strategy). Never `git`.
- File ownership: tasks that run **concurrently** (PR B ∥ PR C; Task 7 ∥ nothing) never touch the same file. Tasks inside one PR run sequentially and may revisit a file; each names the functions it changes.

## Decisions where the spec under-specifies (decided here; not open questions)

1. **`GET /projects` `open_asks`** counts open asks whose owner is an open issue of the project **or** a document of the project (`coalesce(i.project_key, ar.project_key) = p.key and (i.key is null or i.closed_at is null)`).
2. **Issue list grouping** = the lifecycle order in `packages/dispatch/AGENTS.md` and `IssuePage.tsx` `issueStatuses`: `triage, icebox, backlog, todo, in_progress, testing, needs_review, retro, done`. Empty groups are not rendered. Every group is a `<details open>` with heading `<status> (<n>)`; `done` is `<details>` (closed). Filters are client-side over `GET /issues?project=KEY` (one request): status (select), label (select over labels present), Needs you (`open_asks > 0`), Unread (`last_seq > user_state[key].last_read_seq`), search (case-insensitive substring of key or title).
3. **New document** posts `POST /projects/{key}/artifacts` JSON `{ name: <title as typed>, content: "# <title>\n" }`; the slug is the server's `artifactSlug` (the upload slugifier), so "Design notes" → `design-notes`. When a listed document already has that name the form shows "A document named <title> already exists" and posts nothing (the server would otherwise version the existing document).
4. **ETag** for `GET /issues/{key}/references` = `"` + hex(SHA-256 of `"<issue.last_seq>\n<truncated>\n"` followed by `"<artifact id>@<latest version number>\n"` per member in response order) + `"`. `If-None-Match` equal (exact string compare) → 304 with the same `ETag` header and an empty body. Member order: `depth` asc, then `ref_key` asc.
5. **Closure query**: seeds are `refs` rows with `to_kind = 'artifact'` whose source is one of the issue's asks, comments, messages, or artifacts; each step follows `refs.from_kind = 'artifact'` rows whose `from_id` is a member artifact's id (members join `artifacts` on `ref_key`); `UNION`; the recursive term stops at `depth < 9`. Members = rows with `depth ≤ 8`, one per `ref_key` (min depth; ties by `via_kind, via_id`); the issue's own artifacts and targets with no `artifacts` row are excluded. `truncated` = after the one-row-per-`ref_key` reduction, any row still has depth 9 (a target reachable only beyond the cut).
6. **`via`** = `{ kind: "ask" | "comment" | "message" | "artifact", id }`: the item (depth 1) or artifact (depth ≥ 2) through which the member was first reached.
7. **Owner routes on a linked artifact** (`GET/POST /artifacts/{id}/asks|comments`, `GET /artifacts/{id}/events`) → 400 `ARTIFACT_LINKED` naming the issue (`use /api/v1/issues/CORE-1/...`); never an empty list.
8. **`refs.Replace`** lives in the new package `internal/dispatch/refs`; `text.Extract`/`text.Ref` stay in `text/refs.go`. `Replace` stores every Dispatch target kind and **skips `url`** (external links are not graph edges; nothing reads `to_kind = 'url'` today); the one writer skips.
9. **Document item references**: `dispatch://<PROJECT>/artifact/<slug>[@vN]` ↔ `/projects/<PROJECT>/documents/<slug>[?version=N]`; `dispatch://<PROJECT>/artifact/<slug>/ask/<id>` ↔ `…?ask=<id>`; `/comment/<id>` ↔ `…?comment=<id>`. A bare `dispatch://CORE` is not a reference (no `project` ref kind). `https://<server>/projects/CORE/documents/<slug>` parses like the `dispatch://` form.
10. **`Event.project`** is derived by joining `issues`/`artifacts` at read time (`readEventRows`, SSE replay, outbox `scanBatch`) and set by `Append` from the owner lock query; `events` gains no `project_key` column (schema fidelity to the spec).
11. **Document event routing**: the outbox publishes a document event to its document topic only — no issue-route copy (there is no issue) and no ask-author agent-topic copy (spec: "`notify` on a document event routes only to that topic's subscribers"). The asking session is subscribed to the document topic by its own tool result.
12. **Acceptance 8's `version.created`** is the `artifact.version` event on the document topic (Design: "event types unchanged"); the live check asserts an envelope whose topic ends in `.artifact.version` under `notifications.dispatch.document.CORE.runbook-md.>`.
13. **Margin on a document**: Comments tab only — the Pinned tab needs `user_issue_state` (rejected for documents), and the message composer is hidden (`dispatch_message` stays issue-only). `isClosed` is always `false` for a document.
14. **Pinned sidebar rows** come from `GET /api/v1/issues?pinned=true` (new filter, human identity only, joins `user_issue_state` for the caller); the sidebar never fetches the full issue list.
15. **`IssueSummary` gains `labels`** (`listIssuesQuery` selects `i.labels`) so project rows render labels without per-issue requests.
16. **Go types**: `Artifact.IssueKey`, `Ask.IssueKey`, `Comment.IssueKey`, `Event.IssueKey` become `*string` (JSON `null` when document-owned); `Ask`, `Comment`, `Event` gain `ArtifactID *string`; `Artifact` gains `Project`, `RefKey`; `Event` gains `Project`. TS mirrors with `string | null`. `Message.IssueKey` stays `string`.
17. **`ReferencedBy`** gains artifact sources: `{ kind: "artifact", id, issue_key: string|null, project, excerpt: <artifact name>, ref_key }`; item sources keep `issue_key` (`null` for document-owned items) and gain `project`.
18. **TOON rendering** of a document event (`envoy-client/src/delivery.ts`): the `dispatch` block carries `document: "<PROJECT>/<slug>"` (from the topic's third and fourth segments) instead of `issue_key`; the non-notify skip applies to every `notifications.dispatch.` topic.
19. **`dispatch_read({ project, artifact })`** returns a document summary: name, `dispatch://<PROJECT>/artifact/<slug>`, version count, open asks, last 10 events from `GET /artifacts/{id}/events`.
20. **Tool `anchor` on a document**: `anchor.artifact` defaults to the tool's `artifact`; when both are given and differ → tool error `anchor.artifact must be the document itself`.
21. **Migration preflight** is one `DO $$ … $$` block: malformed derived `refs.to_id` artifact
    targets are deleted with a `raise notice` count because the next source write re-derives them
    under the parser grammar; an artifact whose issue has no row still raises an exception, so
    nothing is recorded on failure.
22. **`requireOpenOwner(ctx, tx, owner)`** replaces every `requireOpenIssue(ctx, tx, key)` call in collaboration handlers: linked → lock the issue row and reject closed issues (409 `ISSUE_CLOSED`); unlinked → `select 1 from artifacts where id = $1 and issue_key is null for update` (404 `ARTIFACT_NOT_FOUND` when absent). `requireOpenIssue` remains for issue-only handlers (`patchIssue` keeps its own lock).
23. **Task 1 keeps a `primary` field** in the JSON and multipart upload decoders solely to reject it with 400 `ARTIFACT_INPUT` "primary is fixed at issue creation" (the spec's error row); without it `DisallowUnknownFields` would answer `INVALID_JSON`, which tells an agent nothing.
24. **Routes naming in the SPA**: `parseIssuePath`/`buildIssuePath` stay issue-only (their ~20 callers in #881/#882/PR 3 files are untouched); `parseProjectPath`/`buildProjectPath` are the project family; `parseDispatchReference`/`buildDispatchReference` handle both. `DispatchRoute` is the union of `IssueRoute` and `ProjectRoute`.
25. **`Upload`** takes `owner: { issue: string } | { project: string }`; `uploadFile(owner, file)`.
26. **The e2e seed** (`e2e/seed.ts`) truncates the same tables (no new tables); `e2e/api.ts` gains project/document helpers.
27. **Inbox heading placement**: the `PROJECT · document name` heading for a document ask is rendered by `features/inbox/Inbox.tsx` (the component that renders the issue key + title heading today); `AskCard.tsx` changes only what it invalidates after an answer (`["artifact", artifact_id]` and `["projects"]` for a document ask).
28. **Contract widening lands with its consumers**: PR C widens `issue_key` to `string | null` on `Artifact`, `Ask`, `Comment`, and events and, in the same PR, narrows every SPA site the compiler names; until Tasks 7/8 add the project and document routes, a null owner renders as text (`Project CORE`, `CORE · Design notes`) without a link. No `?? ""` or other silent fallback anywhere.

---

## File Structure

```
packages/envoy/internal/dispatch/
├── store/migrations/0009_project_artifacts.up.sql   // NEW (Task 2)
├── store/store_test.go                              // migrateThrough helper; 0007→0009 fixture tests
├── model/model.go                                   // owner-nullable types, Project.OpenAsks, IssueSummary.Labels, reference types
├── events/broker.go, broker_test.go                 // Append by owner
├── outbox/publisher.go, publisher_test.go           // left joins, document topic, no route copies for documents
├── api/
│   ├── owner.go                                     // NEW: owner, requireOpenOwner, documentOwnerFromRequest
│   ├── project_artifacts.go, project_artifacts_test.go // NEW: GET/POST /projects/{key}/artifacts, unlinked filter
│   ├── artifact_items.go, artifact_items_test.go    // NEW: owner-scoped asks/comments/events routes
│   ├── references.go, references_test.go            // NEW: /issues/{key}/references, /artifacts/{id}/references, ETag
│   ├── inbox.go, inbox_test.go                      // left joins; document; inbox_test.go NEW
│   ├── artifacts.go, artifacts_test.go              // storeArtifact(owner), loadArtifactByRefKey, scanArtifact columns; setPrimaryArtifact deleted (Task 1)
│   ├── asks.go, comments.go, anchors.go             // createAskFor/createCommentFor(owner); resolveAnchor(owner)
│   ├── issues.go, issues_test.go                    // listIssues labels + pinned; createIssue sets project_key
│   ├── projects.go, projects_test.go                // open_asks
│   ├── events.go                                    // eventSelect with joins; readEvents(owner)
│   ├── server.go                                    // routes; error mapping unchanged
│   └── refs.go                                      // DELETED (Task 4): moved into refs package
├── refs/refs.go, closure.go, refs_test.go           // NEW package (Task 4)
├── text/refs.go, refs_test.go                       // Ref.Project; project reference parsing
└── docs/service.go, mutation.go, persistence_test.go, service_test.go
                                                     // lockArtifactOwner, issueOpen left join, settle event by owner, refs.Replace
packages/envoy/cmd/natstail/main.go, main_test.go    // NEW (Task 5): tail a NATS subject
packages/envoy/cmd/dispatch/README.md, AGENTS.md     // routes, topics, migration
packages/contracts/src/dispatch-tools.ts, dispatch-api.ts, subject.ts (+ tests)
packages/envoy-client/src/dispatch-execute.ts, dispatch-http.ts, delivery.ts (+ __tests__)
skills/dispatch/SKILL.md, skills/legion-architect/SKILL.md, packages/pi-envoy/README.md
packages/dispatch/e2e/acceptance/document-roundtrip.sh   // NEW (Task 6)
packages/dispatch/web/src/
├── api/client.ts, types.ts, sse.ts (+ __tests__)
├── components/Tabs.tsx, Tabs.test.tsx               // NEW: tablist extracted from IssueTabs
├── features/refs/routes.ts, routes.test.ts, Unfurl.tsx
├── features/sidebar/Sidebar.tsx, __tests__/sidebar.test.ts
├── features/project/ProjectPage.tsx, IssueList.tsx, DocumentList.tsx (+ tests)   // NEW
├── features/document/DocumentPage.tsx (+ test)      // NEW
├── features/artifacts/Upload.tsx, ArtifactsTab.tsx, ArtifactHeader.tsx, ArtifactDocument.tsx
├── features/margin/useMarginItems.ts, Margin.tsx, MarginSheet.tsx, Composer.tsx, CommentsTab.tsx, useAnsweredAsks.ts
├── features/inbox/Inbox.tsx, AskCard.tsx
├── features/issue/IssueTabs.tsx                     // thin wrapper over Tabs
└── app.tsx
packages/dispatch/e2e/nav.e2e.ts, project.e2e.ts, document.e2e.ts, phone.e2e.ts, artifacts.e2e.ts, api.ts
packages/dispatch/AGENTS.md
```

Parallelism: **PR B (Tasks 2→3→4→5, Go)** and **PR C (Task 6, TS tools)** run concurrently on separate files. **PR A (Task 1)** precedes both in merge order and touches files both later edit (`api/artifacts.go`, `api/server.go`, `dispatch-tools.ts`, `dispatch-execute.ts`) — PR B and PR C rebase over it (disjoint hunks). **PR D (Task 7)** and **PR E (Task 8)** are sequential SPA work; Task 8 may begin from PR D's head once Task 7 Steps 1–3 (routes, client, sse) are committed.

---

### Task 1: Make-primary removal (PR A)

Base: `main` after #881 merges (`ArtifactHeader.tsx` exists only then). Bookmark `dispatch/remove-make-primary`.

**Files:**
- Modify: `packages/envoy/internal/dispatch/api/server.go` (delete the two `…/primary` routes), `api/artifacts.go` (delete `setPrimaryArtifact`; `jsonArtifactUpload.Primary` and the multipart `primary` field become a rejection; delete every `input.primary` branch in `storeArtifact` and `uploadArtifact`'s `PRIMARY_NOT_DOC` check), `api/artifacts_test.go`, `api/server_test.go`, `api/interactions_test.go`
- Modify: `packages/contracts/src/dispatch-tools.ts` (`dispatch_artifact.primary` removed), `dispatch-api.ts` (`CreateArtifactOptions.primary` removed), `packages/envoy-client/src/dispatch-execute.ts`, `dispatch-http.ts`, `__tests__/dispatch-execute.test.ts`, `__tests__/dispatch-http.test.ts`
- Modify: `packages/dispatch/web/src/api/client.ts` (`makeArtifactPrimary` removed; `uploadArtifact` no longer sends `primary`), `__tests__/client.test.ts`, `features/artifacts/ArtifactsTab.tsx` (the `makePrimary` mutation, the `Make primary` buttons, the `Not primary` note, and the `makePrimary.isError` message go; the `Primary` badge stays), `features/artifacts/ArtifactHeader.tsx` (the `Primary`/`Not primary` span goes), `packages/dispatch/e2e/artifacts.e2e.ts`
- Modify: `packages/envoy/cmd/dispatch/README.md`, `AGENTS.md` (route tables), `skills/dispatch/SKILL.md` (`primary` example and sentence; `PRIMARY_NOT_DOC` leaves the error list), `skills/legion-architect/SKILL.md:81` (drop `primary: true`), `packages/pi-envoy/README.md:125`

**Interfaces:**
```go
// api/artifacts.go — the decoders keep the field only to name the rule.
type jsonArtifactUpload struct {
	Name    string       `json:"name"`
	Content *string      `json:"content"`
	Primary *bool        `json:"primary"` // any value → 400 ARTIFACT_INPUT "primary is fixed at issue creation"
	Summary string       `json:"summary"`
	Actor   *model.Actor `json:"actor"`
}
// storeArtifact inserts is_primary = false; only createIssue writes true.
```

- [ ] **Step 1: Failing tests.**
  - `api/artifacts_test.go`: `TestUploadArtifactRejectsPrimaryFlag` — JSON `{"name":"x.md","content":"# x","primary":true}` and multipart `primary=true` on `/api/v1/issues/{key}/artifacts` → 400, body contains `"code":"ARTIFACT_INPUT"` and `fixed at issue creation`; no artifact row. `TestPrimaryRoutesAreGone` — `POST /api/v1/artifacts/<uuid>/primary` and `POST /api/v1/issues/{key}/artifacts/spec/primary` → 404. Delete the `{name: "primary", …}` rows from the three route tables in `TestArtifactRoutesResolveUUIDsAndIssueScopedSlugs`/`TestArtifactIDRoutesValidateBeforeDatabaseUse`; change `TestUploadArtifactJSONCreatesPrimaryDocument` to `TestUploadArtifactJSONCreatesDocumentVersionOne` asserting `primary: false` and drop its `"primary": true` field; drop `"primary": "true"` from `TestUploadArtifactMultipartTrimsNameBeforeMIMEInference`.
  - `api/server_test.go`: in `TestArtifactVersionsAndPrimaryDocument` delete the `notDocument` `PRIMARY_NOT_DOC` block and the `POST …/primary` selection (rename it `TestArtifactVersions`; the spec stays primary throughout); in `TestArtifactUploadRecognizesParameterizedMarkdown` drop `"primary": "true"`; delete `TestConcurrentPrimarySelectionsSerialize` whole — its subject no longer exists.
  - `api/interactions_test.go`: delete the two `assertClosed("primary selection…")` lines.
  - `envoy-client/__tests__/dispatch-execute.test.ts`: the `dispatch_artifact` case sends no `primary` and asserts the request body has no `primary` key; `dispatch-http.test.ts`: drop `primary` from the multipart expectations.
  - SPA `__tests__/client.test.ts`: remove `makeArtifactPrimary` and the `/primary` expectation; assert the JSON upload body has no `primary` key. Add to `features/artifacts` tests (new `ArtifactsTab.test.tsx` if absent): `test("artifacts tab renders no make-primary control")` — render `ArtifactsTab` with a primary doc, a non-primary doc, and an image; `screen.queryByRole("button", { name: /make primary/i })` is null and the text `Not primary` is absent.
  - `e2e/artifacts.e2e.ts`: delete the `Make primary` steps (the disabled image button assertions, `notes.getByRole("button", { name: "Make primary" }).click()`, `toContainText("Primary")`/`"Not primary"`); assert `page.getByRole("button", { name: /make primary/i })` has count 0 on the Artifacts tab and that `notes-md`'s document opens via `/issues/${issue.key}/artifacts/notes-md` (the #881 route) with its text visible.
  - Run: `go test ./internal/dispatch/api/ -run 'TestUploadArtifactRejectsPrimaryFlag|TestPrimaryRoutesAreGone' -v` → FAIL (201 / 200); `cd packages/dispatch && bun run test` → FAIL on the new test; envoy-client tests FAIL on body shape.
- [ ] **Step 2: Implement** the deletions per Files; docs edits per Files (read each edited paragraph against the running behaviour: `curl -X POST …/primary` → 404).
- [ ] **Step 3: Run** — `go vet ./internal/dispatch/... && go test ./internal/dispatch/api/ -count=1`; `cd packages/contracts && bun run lint && bun run typecheck && bun run test && bun run build`; `cd packages/envoy-client && bun run typecheck && bun run test`; `cd packages/pi-envoy && bun run typecheck`; `cd packages/claude-envoy-bridge && bun run typecheck`; `cd packages/dispatch && bun run lint && bun run typecheck && bun run test && bun run e2e`. Expected: all green; `grep -rn 'makeArtifactPrimary\|setPrimaryArtifact\|PRIMARY_NOT_DOC\|Make primary\|Not primary' packages/ skills/` returns nothing.
- [ ] **Step 4: Commit** — `jj describe -m "feat(dispatch): the issue body is the only primary artifact — remove the make-primary routes, upload flag, and controls" && jj new`; `jj bookmark set dispatch/remove-make-primary && jj git push`; open PR A titled `feat(dispatch): remove the make-primary toggle`.

---

### Task 2: Migration 0009 and the owner-aware model, broker, and outbox (PR B, start)

Base: `main` now. Bookmark `dispatch/project-documents-server`. Nothing new is exposed by this task; every existing API test keeps passing.

**Files:**
- Create: `packages/envoy/internal/dispatch/store/migrations/0009_project_artifacts.up.sql`
- Modify: `store/store_test.go`, `model/model.go`, `events/broker.go`, `events/broker_test.go`, `outbox/publisher.go`, `outbox/publisher_test.go`
- Create: `api/owner.go` (`owner`, `issueOwner`, `documentOwner`, `ownerOf`, `owner.event`, `requireOpenOwner`)
- Modify (type-change plumbing only): `api/artifacts.go` (`scanArtifact` column list, the `insert into artifacts` in `storeArtifact` sets `project_key`), `api/issues.go` (`createIssue` insert sets `project_key`), `api/asks.go`, `api/comments.go`, `api/inbox.go`, `api/events.go` (`scanAsk`/`scanComment`/`readEventRows`/inbox scan read `issue_key` into `*string` and the new `artifact_id`; `closeAsk`, `commentAction`, `createNamedVersion`, `editArtifact` lock through `requireOpenOwner(ownerOf(...))`), `docs/service.go` (`settleRoom` builds `model.Event{IssueKey: &issueKey}`), `docs/persistence_test.go` (`createDocument` inserts `project_key`)

**Interfaces:**
```sql
-- 0009_project_artifacts.up.sql
do $$
declare bad record;
begin
  select from_kind, from_id, to_id into bad from refs
  where to_kind = 'artifact' and to_id !~ '^[A-Z][A-Z0-9]{1,9}(-[0-9]+)?/.+$' limit 1;
  if found then
    raise exception 'refs row (%, %) has malformed artifact target %', bad.from_kind, bad.from_id, bad.to_id;
  end if;
  select a.id into bad from artifacts a left join issues i on i.key = a.issue_key where i.key is null limit 1;
  if found then
    raise exception 'artifact % has no issue', bad.id;
  end if;
end $$;
alter table artifacts add column project_key text references projects(key);
update artifacts a set project_key = i.project_key from issues i where i.key = a.issue_key;
alter table artifacts alter column project_key set not null;
alter table artifacts alter column issue_key drop not null;
alter table artifacts drop constraint artifacts_issue_key_slug_key;
alter table artifacts add column ref_key text generated always as (coalesce(issue_key, project_key) || '/' || slug) stored;
alter table artifacts add constraint artifacts_ref_key_key unique (ref_key);
alter table artifacts add column last_seq integer not null default 0;
alter table artifacts add constraint artifacts_primary_has_issue check (not is_primary or issue_key is not null);
alter table asks alter column issue_key drop not null;
alter table asks add column artifact_id uuid references artifacts(id);
alter table asks add constraint asks_one_owner check ((issue_key is null) <> (artifact_id is null));
create index asks_open_artifact on asks (artifact_id) where state = 'open' and artifact_id is not null;
alter table comments alter column issue_key drop not null;
alter table comments add column artifact_id uuid references artifacts(id);
alter table comments add constraint comments_one_owner check ((issue_key is null) <> (artifact_id is null));
alter table events alter column issue_key drop not null;
alter table events add column artifact_id uuid references artifacts(id);
alter table events add constraint events_one_owner check ((issue_key is null) <> (artifact_id is null));
alter table events add constraint events_artifact_id_seq_key unique (artifact_id, seq);
```
```go
// model/model.go (changed fields only)
type Project struct { Key, Name string; OpenAsks int `json:"open_asks"`; CreatedAt time.Time }
type IssueSummary struct { …; Labels []string `json:"labels"`; … }   // after Status
type Artifact struct {
	ID string; IssueKey *string `json:"issue_key"`; Project string `json:"project"`; RefKey string `json:"ref_key"`
	Slug, Name, Kind string; Primary bool; CreatedBy Actor; CreatedAt time.Time; Versions []Version
}
type Ask     struct { ID string; IssueKey *string `json:"issue_key"`; ArtifactID *string `json:"artifact_id"`; … }
type Comment struct { ID string; IssueKey *string `json:"issue_key"`; ArtifactID *string `json:"artifact_id"`; … }
type Event   struct { ID int64; IssueKey *string `json:"issue_key"`; ArtifactID *string `json:"artifact_id"`; Project string `json:"project"`; Seq int; … }
type ReferencedBy struct { Kind, ID string; IssueKey *string `json:"issue_key"`; Project string `json:"project"`; Excerpt string; RefKey string `json:"ref_key,omitempty"` }
type ReferenceVia struct { Kind string `json:"kind"`; ID string `json:"id"` }
type ReferenceMember struct { Artifact Artifact `json:"artifact"`; Depth int `json:"depth"`; Via ReferenceVia `json:"via"` }
type IssueReferences struct { Members []ReferenceMember `json:"members"`; Truncated bool `json:"truncated"` }
type OutgoingReference struct { Kind string `json:"kind"`; ToID string `json:"to_id"`; Artifact *Artifact `json:"artifact,omitempty"` }
type ArtifactReferences struct { Outgoing []OutgoingReference `json:"outgoing"`; ReferencedBy []ReferencedBy `json:"referenced_by"` }

// events/broker.go
// Append assigns the next sequence of the event's owner — issues.last_seq when IssueKey is set,
// artifacts.last_seq when ArtifactID is set — writes e in tx, and fills Project from the owner row.
// Exactly one owner is required; an artifact owner must be unlinked (issue_key is null).
func (b *Broker) Append(ctx context.Context, tx pgx.Tx, e model.Event) (model.Event, error)

// api/owner.go
type owner struct{ IssueKey, ArtifactID *string }             // exactly one set
func issueOwner(key string) owner
func documentOwner(artifactID string) owner
func (o owner) event(eventType string, actor model.Actor, payload any) model.Event
func (s *server) requireOpenOwner(ctx context.Context, tx pgx.Tx, o owner) error   // decision 22
func ownerOf(issueKey, artifactID *string) owner

// api/events.go — the one column list every event reader uses
const eventSelect = `select e.id, e.issue_key, e.artifact_id::text, coalesce(i.project_key, ar.project_key), e.seq, e.type, e.actor, e.notify, e.created_at, e.payload
	from events e left join issues i on i.key = e.issue_key left join artifacts ar on ar.id = e.artifact_id`

// outbox/publisher.go
const documentTopicPrefix = "notifications.dispatch.document."
func envelope(event model.Event, slug string) (contracts.Envelope, error)   // issue → issue topic; document → documentTopicPrefix + project + "." + slug + "." + type
```
`scanBatch` selects through `eventSelect`'s joins plus `ar.slug, i.route`; `publish` skips `publishRoute` and `publishAskAuthorRoute` when `event.ArtifactID != nil` (decision 11); `payloadSummary` prefixes `<PROJECT>/<slug>` for document events.

- [ ] **Step 1: Failing tests.**

```go
// store/store_test.go
// migrateThrough applies the embedded migrations with version <= maxVersion, in filename order.
func migrateThrough(t *testing.T, s *Store, maxVersion int) {
	t.Helper()
	ctx := context.Background()
	if _, err := s.Pool.Exec(ctx, `create table if not exists schema_migrations (version integer primary key, applied_at timestamptz not null default now())`); err != nil { t.Fatal(err) }
	files, err := fs.Glob(migrationFiles, "migrations/*.up.sql")
	if err != nil { t.Fatal(err) }
	for _, filename := range files {
		version, err := migrationVersion(filename)
		if err != nil { t.Fatal(err) }
		if version > maxVersion { continue }
		contents, _ := migrationFiles.ReadFile(filename)
		if err := s.applyMigration(ctx, version, string(contents)); err != nil { t.Fatalf("apply %s: %v", filename, err) }
	}
}

// seedGraphAt0007 writes projects CORE and OPS; issues CORE-1, CORE-2, OPS-1 (each with a primary `spec`);
// an image `diagram-png` on CORE-2; one open ask and one comment on CORE-1; issue.created events (seq 1);
// refs: (comment, <id>, artifact, 'CORE-2/diagram-png'), (artifact, <CORE-1 spec id>, artifact, 'CORE-2/spec'), (ask, <id>, issue, 'CORE-2').
func seedGraphAt0007(t *testing.T, s *Store) (commentID string)

func TestMigrate0009BackfillsProjectAndRefKeyFrom0007(t *testing.T) {
	// acceptance 9: migrateThrough(7); seedGraphAt0007; Migrate() →
	//  select count(*) from artifacts a join issues i on i.key = a.issue_key where a.project_key <> i.project_key  == 0
	//  select count(*) from artifacts where ref_key <> issue_key || '/' || slug                                    == 0
	//  select count(*) from refs r left join artifacts a on a.ref_key = r.to_id where r.to_kind = 'artifact' and a.id is null == 0
	//  schema_migrations has 9; Migrate() again: nil error and the same applied_at for 9.
}

func TestMigrate0009DropsMalformedArtifactReferences(t *testing.T) {
	// migrateThrough(7); seedGraphAt0007; insert a malformed derived artifact reference;
	// Migrate() succeeds, deletes the malformed row, and records migration 9.
}
// TestMigrateCreatesEmptySchemaAndIsIdempotent: expectedConstraints += artifacts_project_key_fkey, artifacts_ref_key_key,
// artifacts_primary_has_issue, asks_artifact_id_fkey, asks_one_owner, comments_artifact_id_fkey, comments_one_owner,
// events_artifact_id_fkey, events_one_owner, events_artifact_id_seq_key; expectedIndexes += asks_open_artifact;
// assert artifacts_issue_key_slug_key is ABSENT (new negative helper assertDatabaseObjectsAbsent); migration count derives from embedded files.

// events/broker_test.go
func TestAppendSequencesArtifactOwnedEvents(t *testing.T) {
	// seed project P, an unlinked artifact (issue_key null, project_key 'P', slug 'notes-md'); Append twice with ArtifactID →
	// Seq 1 then 2; artifacts.last_seq == 2; Project == "P"; the row has issue_key null; a third Append on an issue owner still starts at 1.
}
func TestAppendRejectsEventWithoutExactlyOneOwner(t *testing.T) {
	// IssueKey and ArtifactID both nil → error contains "exactly one owner"; both set → same error; linked artifact id as owner → error (no unlinked row).
}

// outbox/publisher_test.go
func TestRunPublishesDocumentEventsOnTheDocumentTopicOnly(t *testing.T) {
	// seedIssue-style project 'TT'; unlinked artifact slug 'notes-md'; ask row owned by the artifact with a session author;
	// appendEvent {ArtifactID, Type: "comment.created", Actor user alice, Payload CommentEventPayload{AskID: <ask>}} (notify true) →
	// exactly ONE envelope: Topic == "notifications.dispatch.document.TT.notes-md.comment.created"; PayloadSummary starts "TT/notes-md comment created";
	// Payload decodes to an Event with issue_key null, artifact_id set, project "TT". No agent-topic copy despite the session-authored ask.
}
```
Keep every existing test: the JSON of issue-owned rows is unchanged except the added `artifact_id: null` and (artifacts) `project`, `ref_key`.

- [ ] **Step 2: Run** — `go test ./internal/dispatch/store/ -run 'TestMigrate' -v` → FAIL (no 0009: counts/constraints missing); `go test ./internal/dispatch/events/ ./internal/dispatch/outbox/ -run 'TestAppend|TestRunPublishesDocument' -v` → compile failure (`ArtifactID` undefined).
- [ ] **Step 3: Implement** the migration, model, `Append`, outbox, and the type plumbing listed under Files. `requireOpenOwner` is introduced here and used by `closeAsk`, `commentAction`, `createNamedVersion`, `editArtifact` (owners from the loaded row/artifact); `createAsk`/`createComment` keep `requireOpenIssue` until Task 3 refactors them. `readEventRows` scans through `eventSelect`.
- [ ] **Step 4: Run** — `gofmt -l ./internal/dispatch && go vet ./internal/dispatch/... && go test ./internal/dispatch/... -count=1`. Expected: PASS everywhere (api/docs regressions included). Then `cd packages/dispatch && bun run e2e` against this tree: PASS (the SPA ignores the new fields).
- [ ] **Step 5: Commit** — `jj describe -m "feat(dispatch): migration 0009 — artifacts belong to projects, collaboration rows and events have one owner" && jj new`

---

### Task 3: Project artifact routes, owner-scoped collaboration, listings, and the docs service by owner (PR B)

**Files:**
- Create: `api/project_artifacts.go`, `api/project_artifacts_test.go`, `api/artifact_items.go`, `api/artifact_items_test.go`, `api/inbox_test.go`
- Modify: `api/owner.go` (add `documentOwnerFromRequest`, `loadProject`), `api/server.go` (routes), `api/artifacts.go` (`storeArtifact(w, r, input, actor, kind, target artifactTarget)`, `findArtifactByName`/`nextArtifactSlug` by owner prefix, `loadArtifactBySlug` → `loadArtifactByRefKey`, `loadArtifactForRequest`), `api/asks.go` (`createAsk` → `createAskFor(w, r, o owner)`; `listIssueAsks*` constants gain artifact twins; `loadOwnerAsks`), `api/comments.go` (`createComment` → `createCommentFor`; `reply_to`/`ask_id` same-owner checks; `listComments` → owner), `api/anchors.go` (`resolveAnchor(ctx, tx, o owner, input, actor)`, `lockAnchorArtifact(ctx, tx, o, ref)`), `api/inbox.go`, `api/projects.go`, `api/issues.go` (`listIssues`: `pinned`, `labels`), `api/events.go` (`readEvents(ctx, o owner, options)`), `api/artifacts_test.go`, `api/projects_test.go`, `api/issues_test.go`
- Modify: `docs/service.go` (`lockArtifactOwner`, `issueOpen`, `settleRoom`), `docs/mutation.go` (`NamedVersion` uses `lockArtifactOwner`), `docs/persistence_test.go` (`createProjectDocument`), `docs/service_test.go`

**Interfaces:**
```go
// api/artifacts.go
type artifactTarget struct { IssueKey *string; Project string }     // where a new artifact is created; refPrefix() = IssueKey or Project
func (s *server) loadArtifactByRefKey(ctx context.Context, q queryer, refKey string) (model.Artifact, error) // 404 ARTIFACT_NOT_FOUND
// loadArtifactForRequest: {key}+{slug} → loadArtifactByRefKey(key + "/" + slug) — one loader for /issues/{key}/artifacts/{slug} and /projects/{key}/artifacts/{slug}
// scanArtifact columns: id::text, issue_key, project_key, ref_key, slug, name, kind, is_primary, created_by, created_at

// api/project_artifacts.go
func (s *server) listProjectArtifacts(w http.ResponseWriter, r *http.Request)  // GET /api/v1/projects/{key}/artifacts[?unlinked=true]: project_key = key and not is_primary, order by created_at, id
func (s *server) uploadProjectArtifact(w http.ResponseWriter, r *http.Request) // POST /api/v1/projects/{key}/artifacts: 404 PROJECT_NOT_FOUND; same body as the issue upload; storeArtifact(target{Project: key})

// api/owner.go
func (s *server) documentOwnerFromRequest(ctx context.Context, q queryer, r *http.Request) (model.Artifact, owner, error) // parses {id}; 404; linked → 400 ARTIFACT_LINKED "artifact belongs to issue CORE-1; use /api/v1/issues/CORE-1/..."
func (s *server) loadProject(ctx context.Context, q queryer, key string) (model.Project, error)                         // 404 PROJECT_NOT_FOUND

// api/artifact_items.go (all through documentOwnerFromRequest)
GET  /api/v1/artifacts/{id}/asks      → loadOwnerAsks(ctx, q, owner, state)   (?state=all|open|answered as the issue route)
POST /api/v1/artifacts/{id}/asks      → createAskFor(w, r, owner)
GET  /api/v1/artifacts/{id}/comments  → loadOwnerComments(ctx, q, owner, artifactFilter)
POST /api/v1/artifacts/{id}/comments  → createCommentFor(w, r, owner)
GET  /api/v1/artifacts/{id}/events    → readEvents(ctx, owner, options)  (same query params as /issues/{key}/events)

// api/anchors.go — a document ask/comment may anchor only its own document
func (s *server) resolveAnchor(ctx context.Context, tx pgx.Tx, o owner, input *model.AnchorInput, actor model.Actor) (*model.Anchor, string, *model.Version, error)
// document owner: input.Artifact must equal the artifact id or slug → else 400 INVALID_ANCHOR "anchor must target this document"

// api/inbox.go
type inboxAsk struct {
	model.Ask
	Issue    *inboxIssue    `json:"issue,omitempty"`    // {key, title}
	Document *inboxDocument `json:"document,omitempty"` // {project, slug, name}
}

// api/issues.go
const listIssuesQuery = `select i.key, i.title, i.status, i.labels, i.parent_key, i.updated_at, i.last_seq, count(a.id) filter (where i.closed_at is null) …` // unchanged predicates
const listPinnedIssuesQuery = `…the same select list and predicates, plus `join user_issue_state s on s.issue_key = i.key and s.login = $5 and s.pinned` before the where…` // its own complete constant (pgx plan stability, see the asks.go comment), never built by concatenation
// GET /api/v1/issues?pinned=true requires a human identity (403 HUMAN_ONLY for bearer callers); pinned=false or absent → the unfiltered query

// docs/service.go
type artifactOwner struct { IssueKey *string; Project, Slug, Name string }
// lockArtifactOwner loads the artifact's owner and locks the owning row: the issue row when linked
// (open = closed_at is null), the artifact row when not (open = true). Two statements, never a
// FOR UPDATE across the outer join.
func lockArtifactOwner(ctx context.Context, tx pgx.Tx, artifactID string) (artifactOwner, bool, error)
func (s *Service) issueOpen(ctx context.Context, artifactID string) (bool, error) // select coalesce(i.closed_at is null, true) from artifacts a left join issues i on i.key = a.issue_key where a.id = $1
// settleRoom: event = model.Event{IssueKey: owner.IssueKey, ArtifactID: <room when owner.IssueKey == nil>, …}
```
Routes added to `Register`:
```
GET  /api/v1/projects/{key}/artifacts            POST /api/v1/projects/{key}/artifacts
GET  /api/v1/projects/{key}/artifacts/{slug}     GET  /api/v1/projects/{key}/artifacts/{slug}/text
GET  /api/v1/projects/{key}/artifacts/{slug}/versions/{number}
POST /api/v1/projects/{key}/artifacts/{slug}/versions   POST /api/v1/projects/{key}/artifacts/{slug}/edits
GET/POST /api/v1/artifacts/{id}/asks   GET/POST /api/v1/artifacts/{id}/comments   GET /api/v1/artifacts/{id}/events
```
`/asks/{id}/answer`, `/asks/{id}/resolve`, `/comments/{id}/{resolve,accept,reject}` are unchanged routes; `closeAsk` and `commentAction` lock through `requireOpenOwner(ownerOf(row.IssueKey, row.ArtifactID))` and append events with `owner.event(...)`.

- [ ] **Step 1: Failing tests.**

```go
// api/project_artifacts_test.go
func TestCreateProjectDocumentJSONAndMultipart(t *testing.T) {
	// acceptance 5: create project CORE; POST /api/v1/projects/CORE/artifacts JSON {name:"Design notes", content:"# Design notes\n"} → 201,
	// artifact.issue_key == nil, project == "CORE", primary == false, slug == "design-notes", ref_key == "CORE/design-notes", version.number == 1;
	// multipart file "diagram.png" image/png → 201 kind "image"; POST again with name "Design notes" → version 2 of the same artifact id.
}
func TestListProjectArtifactsIncludesIssueArtifactsExceptPrimaries(t *testing.T) {
	// CORE-1 with spec + uploaded notes.md; project doc design-notes; GET /projects/CORE/artifacts → [notes-md (issue_key "CORE-1"), design-notes (issue_key null)];
	// ?unlinked=true → [design-notes]; the spec never appears; GET /projects/NOPE/artifacts → 404 PROJECT_NOT_FOUND.
}
func TestProjectArtifactSlugRoutesMirrorIssueRoutes(t *testing.T) {
	// GET /projects/CORE/artifacts/design-notes (200, referenced_by []), /text (markdown), /versions/1;
	// POST /versions {summary} → 201 named; POST /edits {ops:[replace]} → applied 1 and /text changed; GET /projects/CORE/artifacts/missing → 404 ARTIFACT_NOT_FOUND.
}
func TestProjectUploadRejectsUnknownProjectAndPrimary(t *testing.T) { /* 404 PROJECT_NOT_FOUND; {primary:true} → 400 ARTIFACT_INPUT */ }

// api/artifact_items_test.go
func TestDocumentAsksAndCommentsAreOwnerScoped(t *testing.T) {
	// POST /artifacts/{id}/asks {question, anchor:{artifact:"design-notes", quote:"Design"}} (bearer, session actor) → 201 issue_key null, artifact_id == id, anchor set;
	// GET /artifacts/{id}/asks → 1; POST /asks/{ask}/answer (alice) → 200 without any issue; POST /artifacts/{id}/comments {body, suggestion:{replace_with}, anchor:{quote:"notes"}} → 201;
	// POST /comments/{c}/accept (alice) → 200 and GET /artifacts/{id}/text contains the replacement; GET /artifacts/{id}/events → seq 1..n in order, every row issue_key null, artifact_id id, project "CORE";
	// a comment with reply_to pointing at an issue comment → 400 INVALID_COMMENT.
}
func TestOwnerRoutesRejectLinkedArtifact(t *testing.T) { /* GET/POST asks|comments|events on an issue's artifact → 400 ARTIFACT_LINKED naming the issue key */ }
func TestDocumentAnchorMustTargetTheDocument(t *testing.T) { /* anchor.artifact = another artifact's slug/id → 400 INVALID_ANCHOR */ }

// api/inbox_test.go
func TestInboxCarriesDocumentForDocumentAsks(t *testing.T) {
	// one issue ask (CORE-1) and one document ask (design-notes) → GET /inbox: the issue row has issue {key,title} and no document;
	// the document row has document {project:"CORE", slug:"design-notes", name:"Design notes"} and no issue; ?project=OPS excludes both; ?project=CORE keeps both.
}

// api/projects_test.go
func TestListProjectsCountsOpenAsksOnIssuesAndDocuments(t *testing.T) { /* 1 issue ask + 1 document ask + 1 answered → open_asks 2; a closed issue's ask is not counted */ }

// api/issues_test.go
func TestListIssuesPinnedFilterAndLabels(t *testing.T) {
	// labels ["repo:x"] round-trip in GET /issues?project=CORE; PUT /me/issues/CORE-2/state {pinned:true} (alice) →
	// GET /issues?pinned=true (alice) == [CORE-2]; (bob) == []; bearer → 403 HUMAN_ONLY.
}

// docs/service_test.go
func TestSettleWritesArtifactOwnedEventForUnlinkedDocument(t *testing.T) {
	// createProjectDocument(t, database, "before"); browser-style edit on the live doc; waitForDocumentVersion(2);
	// the events row for the version has artifact_id == id, issue_key null; artifacts.last_seq == 1.
}
func TestUnlinkedDocumentIsAlwaysOpen(t *testing.T) { /* issueOpen → true; NamedVersion succeeds; no ErrIssueClosed path */ }
```
Helper: `createProjectDocument(t, database, markdown) string` in `docs/persistence_test.go` (project `DOC`, `issue_key` null, `project_key 'DOC'`, slug `document-<rand>`).

- [ ] **Step 2: Run** — `go test ./internal/dispatch/api/ -run 'TestCreateProjectDocument|TestListProjectArtifacts|TestProjectArtifactSlugRoutes|TestDocumentAsks|TestOwnerRoutes|TestInboxCarries|TestListProjectsCounts|TestListIssuesPinned' -v` → 404s on the new routes / compile failures; `go test ./internal/dispatch/docs/ -run 'TestSettleWritesArtifactOwned|TestUnlinkedDocument' -v` → FAIL (`lock document issue: no rows` on the inner join).
- [ ] **Step 3: Implement** per Interfaces. `createAskFor`/`createCommentFor` are the existing bodies with `issueKey` replaced by `o owner` (inserts write `issue_key, artifact_id` from `o`); the issue handlers become one-line wrappers `createAskFor(w, r, issueOwner(r.PathValue("key")))`. `listComments` on the issue route keeps its `?artifact=` filter; the artifact route filters by owner. `docs`: `lockArtifactOwner` used by `settleRoom` and `NamedVersion` (rebase hazard: PR 3 rewrites both functions — re-apply by function name).
- [ ] **Step 4: Run** — `go vet ./internal/dispatch/... && go test ./internal/dispatch/... -count=1` → PASS. Smoke the real surface: `DATABASE_URL=<dev url> DISPATCH_AGENT_TOKEN=local-agent-token DISPATCH_IDENTITY=header:X-Dispatch-User DISPATCH_ALLOWED_LOGINS=sjawhar DISPATCH_NATS_DISABLED=1 go run ./cmd/dispatch`, then with `curl -H 'X-Dispatch-User: sjawhar'`: `POST /api/v1/projects/CORE/artifacts` (JSON), `GET /api/v1/projects/CORE/artifacts/design-notes/text`, `POST /api/v1/artifacts/<id>/asks`, `GET /api/v1/inbox` shows `document`, `GET /api/v1/projects` shows `open_asks`. Paste the transcript into the PR.
- [ ] **Step 5: Commit** — `jj describe -m "feat(dispatch): project documents — project artifact routes, owner-scoped asks, comments, and events, inbox and project badges" && jj new`

---

### Task 4: The `refs` package, project reference parsing, and the reference graph API (PR B)

**Files:**
- Create: `packages/envoy/internal/dispatch/refs/refs.go`, `refs/closure.go`, `refs/refs_test.go`, `api/references.go`, `api/references_test.go`
- Modify: `text/refs.go`, `text/refs_test.go`, `api/server.go` (two routes), `api/artifacts.go` (`getArtifact` uses `refs.ReferencedBy(artifact.RefKey)`; `storeArtifact` calls `refs.Replace`), `api/issues.go`, `api/asks.go`, `api/comments.go`, `api/messages.go` (`s.replaceRefs(...)` → `refs.Replace(ctx, tx, kind, id, body, s.deps.ServerURL)`), `docs/mutation.go` (`indexDocumentReferences` body → `refs.Replace(ctx, tx, "artifact", artifactID, markdown, s.serverURL)`), `api/interactions_test.go`/`api/server_test.go` (targets now exclude `url` rows)
- Delete: `api/refs.go`

**Interfaces:**
```go
// text/refs.go
type Ref struct { Kind, IssueKey, Project, ID string } // Project set (IssueKey empty) for dispatch://<PROJECT>/artifact/<slug>[/ask|comment/<id>]
var projectKeyPattern = regexp.MustCompile(`^[A-Z][A-Z0-9]{1,9}$`)
// parseDispatch: key matches issueKeyPattern → existing rules; key matches projectKeyPattern → tail must be
//   artifact/<slug>[@vN]            → Ref{Kind:"artifact", Project:key, ID:slug}
//   artifact/<slug>/ask/<id>        → Ref{Kind:"ask", Project:key, ID:id}
//   artifact/<slug>/comment/<id>    → Ref{Kind:"comment", Project:key, ID:id}
//   anything else (bare key, spec, log) → not a reference
// parseServer: /projects/<KEY>/documents/<slug>[?version=n][&ask=|&comment=] → the same refs

// refs/refs.go
type Queryer interface { QueryRow(context.Context, string, ...any) pgx.Row; Query(context.Context, string, ...any) (pgx.Rows, error) }
// ToID is the refs.to_id of a parsed reference: the target's ref_key for artifacts, the item id otherwise.
func ToID(ref text.Ref) string          // artifact: coalesce(IssueKey, Project) + "/" + ID
// Replace deletes the (fromKind, fromID) rows and writes one row per Dispatch reference in body; url refs are skipped.
func Replace(ctx context.Context, tx pgx.Tx, fromKind, fromID, body, serverURL string) error
// ReferencedBy lists the asks, comments, messages, and artifacts whose text links the artifact with refKey.
func ReferencedBy(ctx context.Context, q Queryer, refKey string) ([]model.ReferencedBy, error)
// Outgoing lists the references written by artifact fromID.
func Outgoing(ctx context.Context, q Queryer, artifactID string) ([]model.OutgoingReference, error) // Artifact filled for artifact targets that exist

// refs/closure.go
type Member struct { RefKey string; Depth int; Via model.ReferenceVia }
// Closure returns the artifacts issueKey reaches (decision 5), excluding its own, ordered by depth then ref_key, and whether depth 8 cut the graph.
func Closure(ctx context.Context, q Queryer, issueKey string) ([]Member, bool, error)
```
```sql
-- refs/closure.go query (parameters: $1 issue key)
with recursive closure as (
  select r.to_id as ref_key, 1 as depth, r.from_kind as via_kind, r.from_id as via_id
  from refs r
  where r.to_kind = 'artifact' and (
        (r.from_kind = 'ask'      and r.from_id in (select id::text from asks      where issue_key = $1))
     or (r.from_kind = 'comment'  and r.from_id in (select id::text from comments  where issue_key = $1))
     or (r.from_kind = 'message'  and r.from_id in (select id::text from messages  where issue_key = $1))
     or (r.from_kind = 'artifact' and r.from_id in (select id::text from artifacts where issue_key = $1)))
  union
  select r.to_id, c.depth + 1, 'artifact', a.id::text
  from closure c
  join artifacts a on a.ref_key = c.ref_key
  join refs r on r.from_kind = 'artifact' and r.from_id = a.id::text and r.to_kind = 'artifact'
  where c.depth < 9
)
select distinct on (c.ref_key) c.ref_key, c.depth, c.via_kind, c.via_id
from closure c join artifacts a on a.ref_key = c.ref_key
where a.issue_key is distinct from $1
order by c.ref_key, c.depth, c.via_kind, c.via_id
```
`Closure` sorts the rows by `(depth, ref_key)`, returns those with `depth ≤ 8`, and reports `truncated` when any reduced row has depth 9.
```go
// api/references.go
func (s *server) getIssueReferences(w http.ResponseWriter, r *http.Request)     // GET /api/v1/issues/{key}/references → model.IssueReferences; ETag (decision 4); If-None-Match → 304
func (s *server) getArtifactReferences(w http.ResponseWriter, r *http.Request)  // GET /api/v1/artifacts/{id}/references → model.ArtifactReferences
func referencesETag(lastSeq int, members []model.ReferenceMember, truncated bool) string
func (s *server) loadArtifactsByRefKey(ctx context.Context, q queryer, refKeys []string) (map[string]model.Artifact, error) // one query, versions loaded per artifact
```

- [ ] **Step 1: Failing tests.**

```go
// text/refs_test.go
func TestExtractParsesProjectDocumentReferences(t *testing.T) {
	body := `dispatch://CORE/artifact/design-notes dispatch://CORE/artifact/design-notes@v2 dispatch://CORE/artifact/design-notes/ask/a1 dispatch://CORE/artifact/design-notes/comment/c1 dispatch://CORE dispatch://CORE/spec https://dispatch.example/projects/CORE/documents/design-notes?version=3 https://dispatch.example/projects/CORE/documents/design-notes?ask=a2`
	want := []Ref{
		{Kind: "artifact", Project: "CORE", ID: "design-notes"}, {Kind: "artifact", Project: "CORE", ID: "design-notes"},
		{Kind: "ask", Project: "CORE", ID: "a1"}, {Kind: "comment", Project: "CORE", ID: "c1"},
		{Kind: "artifact", Project: "CORE", ID: "design-notes"}, {Kind: "ask", Project: "CORE", ID: "a2"},
	}
	// reflect.DeepEqual(Extract(body, "https://dispatch.example"), want); the bare key and /spec produce nothing
}

// refs/refs_test.go (Postgres; openTestStore copied from docs/persistence_test.go shape)
func TestReplaceWritesRefKeysAndSkipsExternalURLs(t *testing.T) {
	// Replace("comment", "c1", "see dispatch://CORE/artifact/design-notes and dispatch://CORE-2/artifact/spec and https://example.com", "https://dispatch.example")
	// → rows (comment,c1,artifact,'CORE/design-notes'), (comment,c1,artifact,'CORE-2/spec'); no url row; Replace again with "" → zero rows.
}
func TestClosureFollowsArtifactLinksToDepthEightAndReportsTruncation(t *testing.T) {
	// project CORE; CORE-1 (spec) and CORE-2 (spec, diagram-png); documents d1..d10 (unlinked) where d(n) links d(n+1);
	// a CORE-1 comment links dispatch://CORE/artifact/d1 and dispatch://CORE-2/artifact/diagram-png; d1's text also links dispatch://CORE-1/spec (own artifact) and dispatch://CORE/artifact/ghost (no row).
	// Closure(CORE-1) → members: d1 depth 1 via {comment,<id>}, diagram-png depth 1 via {comment,<id>}, d2 depth 2 via {artifact,<d1 id>}, … d8 depth 8; no CORE-1/spec; no ghost; truncated == true (d9 is beyond).
	// Ordering: depth asc then ref_key asc. A cycle (d3 links d1) changes nothing.
}
func TestReferencedByIncludesArtifactSources(t *testing.T) { /* d1 (artifact source) and a CORE-1 comment both link d2 → two rows: kind artifact (issue_key null, project CORE, ref_key CORE/d1, excerpt "d1 name") and kind comment (issue_key "CORE-1") */ }

// api/references_test.go
func TestIssueReferencesClosureDepthViaAndETag(t *testing.T) {
	// acceptance 6: CORE-1 comment links dispatch://CORE/artifact/design-notes; design-notes body links dispatch://CORE-2/artifact/diagram-png.
	// GET /api/v1/issues/CORE-1/references → 200, members[0] = design-notes depth 1 via {comment, <id>}, members[1] = diagram-png depth 2 via {artifact, <design-notes id>}, truncated false, ETag header set;
	// GET with If-None-Match: <etag> → 304, empty body, same ETag; POST /projects/CORE/artifacts {name:"Design notes", content:"no links"} (new version) → GET → one member, ETag differs, If-None-Match with the old value → 200.
}
func TestIssueReferencesExcludeOwnArtifactsAndDanglingTargets(t *testing.T) { /* own spec linked from a comment and dispatch://CORE/artifact/ghost → members [] */ }
func TestIssueReferencesTruncateBeyondDepth8(t *testing.T) { /* chain of 10 → 8 members, truncated true */ }
func TestArtifactReferencesListOutgoingAndReferencedBy(t *testing.T) { /* GET /artifacts/{design-notes}/references → outgoing [{kind artifact, to_id CORE-2/diagram-png, artifact{…}}], referenced_by [comment on CORE-1] */ }
```
`api/interactions_test.go:1050` (`len(targets) != 2 …`) and `server_test.go:1622-1654` keep passing (no url in those bodies); any test asserting a `url` ref row is deleted.

- [ ] **Step 2: Run** — `go test ./internal/dispatch/text/ -run TestExtractParsesProject -v` → FAIL (no `Project` field); `go test ./internal/dispatch/refs/ ./internal/dispatch/api/ -run 'TestReplace|TestClosure|TestReferencedBy|TestIssueReferences|TestArtifactReferences' -v` → compile failure / 404.
- [ ] **Step 3: Implement** per Interfaces; delete `api/refs.go`; every former `replaceRefs` caller calls `refs.Replace`.
- [ ] **Step 4: Run** — `go vet ./internal/dispatch/... && go test ./internal/dispatch/... -count=1` → PASS. `grep -rn 'replaceRefs\|indexDocumentReferences\|loadReferencedBy' packages/envoy` → nothing.
- [ ] **Step 5: Commit** — `jj describe -m "feat(dispatch): the reference graph — refs package, project document references, issue closure with ETag, artifact references" && jj new`

---

### Task 5: `cmd/natstail`, server docs, full verification, PR B

**Files:**
- Create: `packages/envoy/cmd/natstail/main.go`, `main_test.go`
- Modify: `packages/envoy/cmd/dispatch/README.md` (routes table: project artifact routes, owner routes, references; topics section: document topic; migration note), `packages/envoy/cmd/dispatch/AGENTS.md` (same, agent-facing), `packages/envoy/internal/dispatch/refs/refs.go` package comment (≤10 lines: what a reference is, ref_key, closure depth)

**Interfaces:**
```go
// cmd/natstail: prints every envelope received on a NATS subject as one JSON line, until -count envelopes or -timeout.
//   natstail -subject 'notifications.dispatch.document.CORE.runbook-md.>' [-count 1] [-timeout 30s]
// NATS URLs come from envoy config (config.Load, the same source cmd/dispatch uses). Exit 0 when -count reached, 1 on timeout, 2 on bad flags.
func tail(ctx context.Context, urls []string, subject string, count int, out io.Writer) error // core subscription via bus.Client.SubscribeCore
```

- [ ] **Step 1: Failing test** — `cmd/natstail/main_test.go` `TestTailPrintsEnvelopesOnTheSubject`: start NATS with `tcnats.Run(ctx, "nats:2.10")` (the `internal/bus/recovery_test.go` recipe), `bus.Connect` a publisher, publish two envelopes on `notifications.dispatch.document.T.d.artifact.version` and one on `notifications.dispatch.issue.T-1.ask.opened`; `tail(ctx, urls, "notifications.dispatch.document.T.d.>", 2, &buf)` returns nil and `buf` has exactly two lines whose `topic` fields end in `.artifact.version`. Run: `go test ./cmd/natstail/ -v` → compile failure.
- [ ] **Step 2: Implement** `main.go`; write the docs (read each table row against `curl` on the running server, not against the diff).
- [ ] **Step 3: Full verification** (the PR gate; paste outputs in the PR):
  - `cd packages/envoy && gofmt -l . && go vet ./... && DISPATCH_TEST_DATABASE_URL=<url> sh -c 'go list ./... | grep -v /integration | xargs go test -count=1'`
  - `cd packages/dispatch && bun run e2e` (unchanged SPA against the new server: PASS on both projects)
  - Smoke as in Task 3 Step 4 plus: `go run ./cmd/dispatch` with NATS enabled (`DISPATCH_NATS_DISABLED` unset, `ENVOY_NATS_URL` set) in one pane, `go run ./cmd/natstail -subject 'notifications.dispatch.document.>' -count 1 -timeout 60s` in another, then `POST /api/v1/projects/CORE/artifacts` → natstail prints the `artifact.created` envelope on `notifications.dispatch.document.CORE.<slug>.artifact.created`.
- [ ] **Step 4: Commit and open PR B** — `jj describe -m "docs(dispatch): project documents, owner routes, references, document topics; natstail" && jj new`; `jj bookmark set dispatch/project-documents-server && jj git push`; title `feat(dispatch): project documents and the reference graph — server`; register with the merge queue. Thermo pair applies (production Go). Before pushing: the migration-number re-check from Global Constraints.

---

### Task 6: Contracts, envoy-client tools, skill, and the live-acceptance script (PR C)

Base: `main` after #881, #882, and PR A merge (decision 28). Bookmark `dispatch/project-documents-tools`. Develops in parallel with PR B (no shared files); its tests mock HTTP against the API shapes defined in Tasks 2–4.

**Files:**
- Modify: `packages/contracts/src/subject.ts`, `dispatch-tools.ts`, `dispatch-tools.test.ts`, `dispatch-api.ts`, `dispatch-api.test.ts`
- Modify: `packages/envoy-client/src/dispatch-http.ts`, `dispatch-execute.ts`, `delivery.ts`, `__tests__/dispatch-http.test.ts`, `__tests__/dispatch-execute.test.ts`, `__tests__/delivery.test.ts`
- Modify: `skills/dispatch/SKILL.md` ("Your issue" → owner; References list; tool signatures gain `project`), `packages/pi-envoy/README.md` (one paragraph: project documents)
- Modify (SPA type narrowing, Step 2b — decision 28): `packages/dispatch/web/src/features/artifacts/ArtifactHeader.tsx`, `features/artifacts/ArtifactsTab.tsx`, `features/inbox/Inbox.tsx`, `features/inbox/AskCard.tsx`, `web/src/api/sse.ts`, `__tests__/ask-card.test.tsx`, `__tests__/sse.test.ts`
- Create: `packages/dispatch/e2e/acceptance/document-roundtrip.sh`

**Interfaces:**
```ts
// contracts/src/subject.ts
export const DISPATCH_TOPIC_PREFIX = "notifications.dispatch." as const;
export const DISPATCH_DOCUMENT_TOPIC_PREFIX = "notifications.dispatch.document." as const;
export type DispatchDocumentSubject<P extends string = string, S extends string = string, T extends string = string> =
  `${typeof DISPATCH_DOCUMENT_TOPIC_PREFIX}${P}.${S}.${T}`;
export function dispatchDocumentSubject(project: string, slug: string, type: string): DispatchDocumentSubject;
// Subject union gains DispatchDocumentSubject.

// contracts/src/dispatch-api.ts (changed shapes)
export interface Project { key; name; open_asks: number; created_at }
export interface IssueSummary extends Pick<Issue, …> { labels: string[]; open_asks: number }
export interface Artifact { id; issue_key: string | null; project: string; ref_key: string; slug; name; kind; primary; created_by; created_at; versions; referenced_by? }
export interface Ask { …; issue_key: string | null; artifact_id: string | null; … }      // Comment likewise
export interface InboxAsk extends Ask { issue?: { key: string; title: string }; document?: { project: string; slug: string; name: string } }
export interface ReferencedBy { kind: "ask" | "comment" | "message" | "artifact"; id; issue_key: string | null; project: string; excerpt: string; ref_key?: string }
export interface ReferenceVia { kind: "ask" | "comment" | "message" | "artifact"; id: string }
export interface ReferenceMember { artifact: Artifact; depth: number; via: ReferenceVia }
export interface IssueReferences { members: ReferenceMember[]; truncated: boolean }
export interface OutgoingReference { kind: string; to_id: string; artifact?: Artifact }
export interface ArtifactReferences { outgoing: OutgoingReference[]; referenced_by: ReferencedBy[] }
interface DispatchEventBase { id; issue_key: string | null; artifact_id: string | null; project: string; seq; actor; notify; created_at }
export const DispatchEventSchema = z.object({ issue_key: z.string().nullable(), artifact_id: z.string().nullable().optional(), project: z.string().optional(), type, actor, payload, … });
export interface ListIssuesOptions { project?; status?; parent?; updated_since?; pinned?: boolean }

// contracts/src/dispatch-tools.ts
const OWNER_REFERENCE = "Exactly one of issue and project. An issue is a native KEY or external owner/repo#n reference …(existing text)…; a project is a project key such as CORE and addresses an unlinked project document named by artifact.";
// dispatch_ask, dispatch_comment, dispatch_suggest, dispatch_doc_edit, dispatch_doc_read, dispatch_artifact, dispatch_read:
//   issue: z.string().optional(), project: z.string().optional()  (dispatch_doc_read/dispatch_read also accept ref alone)
//   validation.check: xor(issue, project) [|| ref for the two read tools]; and (project && tool !== "dispatch_artifact") → typeof artifact === "string"
//   validation.message: "Exactly one of issue and project is required; with project, artifact names the document."
// dispatch_message, dispatch_issue, dispatch_resolve_ask: unchanged.

// envoy-client/src/dispatch-http.ts (new methods)
listProjectArtifacts(project: string, unlinked?: boolean): Promise<Artifact[]>
projectArtifact(project: string, input: CreateArtifactInput): Promise<ArtifactUploadResponse>   // JSON or multipart like artifact()
getProjectArtifact(project: string, slug: string): Promise<ArtifactDetails>
getArtifactAsks(id: string, state?: "all" | "open" | "answered"): Promise<Ask[]>
artifactAsk(id: string, input: CreateAskInput): Promise<Ask>
getArtifactComments(id: string): Promise<Comment[]>
artifactComment(id: string, input: CreateCommentInput): Promise<Comment>
artifactSuggest(id: string, input: …same shape as suggest…): Promise<Comment>
getArtifactEvents(id: string, after = 0, limit = 200): Promise<Event[]>
getIssueReferences(issue: string): Promise<IssueReferences>

// envoy-client/src/dispatch-execute.ts
type Owner = { kind: "issue"; issue: string } | { kind: "project"; project: string };
interface ParsedDispatchRef { owner: Owner; kind: "issue" | "spec" | "log" | "children" | "artifact" | "ask" | "comment"; id: string; version?: number }
// parseDispatchRef also accepts dispatch://<PROJECT>/artifact/<slug>[@vN][/ask/<id>|/comment/<id>] (owner project)
async function resolveOwnerArguments(tool, args, cwd, env, exec): Promise<{ args: ToolArguments; ref: ParsedDispatchRef | null; owner: Owner | null }>
//   project string → owner project (must match /^[A-Z][A-Z0-9]{1,9}$/; issue also present → Error("exactly one of issue and project"));
//   otherwise today's issue resolution (issue arg, ref, LEGION_ISSUE)
interface ResolvedArtifact { owner: Owner; artifact: Artifact; issue?: IssueDetails }
async function resolveArtifact(client, owner, artifactReference): Promise<ResolvedArtifact>  // project: getProjectArtifact(project, slug) or getArtifact(uuid) checked for project match and issue_key === null
function ownerTopic(resolved: ResolvedArtifact | { owner: { kind: "issue"; issue: string } }): string // issue → dispatchIssueSubject(key, ">"); project → dispatchDocumentSubject(project, slug, ">")
// details for document results: { project, artifact: <id>, document: "<PROJECT>/<slug>", topic, … } (no issue key)
```
Per tool with a project owner: `dispatch_ask` → `client.artifactAsk(artifact.id, …)`; `dispatch_comment` → `client.artifactComment`; `dispatch_suggest` → `client.artifactSuggest`; `dispatch_doc_edit` → `client.docEdit(artifact.id, …)`; `dispatch_doc_read` → `client.docRead(artifact.id, version)` + marks from `getArtifactAsks`/`getArtifactComments`; `dispatch_artifact` → `client.projectArtifact(project, …)`; `dispatch_read` → document summary (decision 19). `anchor.artifact` defaulting per decision 20.

`delivery.ts`: the non-notify skip tests `startsWith(DISPATCH_TOPIC_PREFIX)`; when `frame.event.issue_key === null` the `dispatch` block emits `document: "<P>/<slug>"` parsed from the topic (`notifications.dispatch.document.<P>.<slug>.…`) instead of `issue_key`.

`document-roundtrip.sh` (same mechanics as `omp-roundtrip.sh`: `OMP_BIN`, `DISPATCH_URL`, `DISPATCH_TOKEN`, `ENVOY_NATS_URL`, tmux, the `pi-envoy` dist, persisted-session grep): starts `go run ./cmd/natstail -subject 'notifications.dispatch.document.CORE.runbook-md.>' -count 1 -timeout 120s` in the background; prompts the OMP session to run, in order, `dispatch_artifact({ project: "CORE", name: "Runbook.md", content: "# Runbook\n\nStep one.\n" })`, `dispatch_doc_read({ ref: "dispatch://CORE/artifact/runbook-md" })`, `dispatch_doc_edit({ project: "CORE", artifact: "runbook-md", ops: [{ op: "replace", find: "Step one.", with: "Step one, then two." }] })`; asserts via `curl` that `GET /api/v1/projects/CORE/artifacts/runbook-md/text` contains `then two`, that the transcript's tool result `details.topic` is `notifications.dispatch.document.CORE.runbook-md.>` (grep the `.jsonl`), and that natstail exited 0 with a topic ending `.artifact.version`. Exit non-zero with the pane capture on any miss. Takes `PROJECT`/`NAME` overrides so it is reusable for any document.

- [ ] **Step 1: Failing tests.**
  - `contracts/src/dispatch-tools.test.ts`: `"rejects both or neither of issue and project"`, `"requires artifact with project on document tools but not dispatch_artifact"`, `"accepts ref alone on dispatch_doc_read and dispatch_read"` (through `dispatchToolSchema(spec, zodSchemaApi(z), { strict: true })`).
  - `contracts/src/dispatch-api.test.ts`: `"DispatchEventSchema accepts a document event with issue_key null"`; subject test: `dispatchDocumentSubject("CORE", "runbook-md", ">") === "notifications.dispatch.document.CORE.runbook-md.>"`.
  - `envoy-client/__tests__/dispatch-execute.test.ts`: `"creates an unlinked project document with dispatch_artifact({ project })"` (POST `/api/v1/projects/CORE/artifacts`, body has no `issue`, details `{ project: "CORE", artifact, version: 1, document: "CORE/runbook-md", topic: dispatchDocumentSubject("CORE","runbook-md",">") }` and `dispatchSubscriptionTopic(details)` returns it); `"reads a project document from a dispatch:// project reference"` (GET `/api/v1/projects/CORE/artifacts/runbook-md`, `/api/v1/artifacts/<id>/text`, `/asks`, `/comments`); `"edits a project document with dispatch_doc_edit({ project, artifact })"`; `"opens a document ask on /artifacts/{id}/asks"`; `"reads a document summary with dispatch_read({ project, artifact })"`; `"rejects issue together with project, project without artifact, and a bad project key"`; the existing issue cases are unchanged.
  - `envoy-client/__tests__/delivery.test.ts`: `"renders a document event with its project and slug"` (topic `notifications.dispatch.document.CORE.runbook-md.artifact.version`, `issue_key: null` → TOON `dispatch.document === "CORE/runbook-md"`), `"skips a non-notify document frame"`.
  - `dispatch-http.test.ts`: the new methods hit the exact paths.
  - Run: `cd packages/contracts && bun run test`; `cd packages/envoy-client && bun run test` → FAIL.
- [ ] **Step 2: Implement** contracts, client, executor, delivery, skill, README, and the script.
- [ ] **Step 2b: SPA compiles against the widened types** (decision 28). Run `cd packages/dispatch && bun run typecheck` and narrow exactly the sites it names — expected on the #881/#882 tree: `features/artifacts/ArtifactHeader.tsx` (`Back to primary spec` link only when `artifact.issue_key !== null`, else the text `Project <project>`), `features/artifacts/ArtifactsTab.tsx` (the artifact-name `Link` and Referenced-by `Link` narrow on a non-null key; the tab is issue-scoped, so its rows always have one — narrow with a type guard, not a fallback), `features/inbox/Inbox.tsx` (heading: issue → today; document → text `<project> · <document.name>`), `features/inbox/AskCard.tsx` (invalidations per decision 27), `web/src/api/sse.ts` (`["issue", event.issue_key]`/`["events", …]` only when non-null; `prependEventToLog` returns early for a null key). Add `__tests__/ask-card.test.tsx` `"a document ask renders its project and document name and invalidates the artifact"` and `sse.test.ts` `"a document event never invalidates issue keys"`. `bun run lint && bun run typecheck && bun run test` → green.
- [ ] **Step 3: Run** — `cd packages/contracts && bun run lint && bun run typecheck && bun run test && bun run build`; `cd packages/envoy-client && bun run lint && bun run typecheck && bun run test`; `cd packages/pi-envoy && bun run typecheck && bun run test`; `cd packages/claude-envoy-bridge && bun run typecheck && bun run test`; `cd packages/daemon && bun run typecheck` (the daemon decodes issue topics only; it must still compile against the widened types); `cd packages/dispatch && bun run lint && bun run typecheck && bun run test && bun run e2e` (Step 2b; the e2e lane still passes on the pre-project SPA); `bun install --frozen-lockfile`. Expected: green. Then run `document-roundtrip.sh` against PR B's head (`go run ./cmd/dispatch` with NATS) — expected exit 0 with the three assertions printed; paste the output in the PR.
- [ ] **Step 4: Commit and open PR C** — `jj describe -m "feat(dispatch-tools): project documents as an owner — project on the dispatch tools, document topics, document event rendering" && jj new`; bookmark `dispatch/project-documents-tools`; title `feat(dispatch-tools): project as an owner for Dispatch tools`. PR C merges after PR B.

---

### Task 7: SPA — routes, sidebar, project page (PR D)

Base: `main` after #881, #882, PR A, PR B, PR C merge. Bookmark `dispatch/project-documents-spa-nav`.

**Files:**
- Modify: `web/src/api/client.ts`, `__tests__/client.test.ts`, `web/src/api/sse.ts`, `__tests__/sse.test.ts`, `web/src/features/refs/routes.ts`, `routes.test.ts`, `features/refs/Unfurl.tsx`, `Unfurl.test.tsx`, `web/src/app.tsx`, `app.test.tsx`, `features/sidebar/Sidebar.tsx`, `__tests__/sidebar.test.ts`, `features/issue/IssueTabs.tsx`, `features/artifacts/Upload.tsx`, `features/margin/Composer.tsx` (the single `uploadFile(issueKey, file)` call → `uploadFile({ issue: issueKey }, file)`), `features/inbox/Inbox.tsx`, `features/inbox/AskCard.tsx`, `__tests__/ask-card.test.tsx`, `packages/dispatch/AGENTS.md`
- Create: `web/src/components/Tabs.tsx`, `Tabs.test.tsx`, `features/project/ProjectPage.tsx`, `ProjectPage.test.tsx`, `features/project/IssueList.tsx`, `IssueList.test.tsx`, `features/project/DocumentList.tsx`, `DocumentList.test.tsx`
- e2e: rewrite `e2e/nav.e2e.ts`; create `e2e/project.e2e.ts`; extend `e2e/phone.e2e.ts` (project page); `e2e/api.ts` helpers `createProjectDocument(project, input)`, `listProjectArtifacts(project)`, `getProjectArtifact(project, slug)`

**Interfaces:**
```ts
// features/refs/routes.ts
export type IssueRoute = /* today's DispatchRoute variants (issue, spec, log, children, artifacts, artifact, ask, comment) */;
export type ProjectRoute =
  | { kind: "project"; project: string }
  | { kind: "documents"; project: string }
  | { kind: "document"; project: string; slug: string; version?: number; item?: { kind: "ask" | "comment"; id: string } };
export type DispatchRoute = IssueRoute | ProjectRoute;
export function parseProjectPath(pathname: string, search = ""): ProjectRoute | undefined; // /projects/CORE, /projects/CORE/documents, /projects/CORE/documents/<slug>[?version=n][&ask=|&comment=]
export function buildProjectPath(route: ProjectRoute): string;
// parseIssuePath/buildIssuePath: unchanged signatures, IssueRoute only.
// parseDispatchReference/buildDispatchReference: dispatch://<PROJECT>/artifact/<slug>[@vN] ↔ document route; …/ask/<id> ↔ item ask; …/comment/<id> ↔ item comment. A project key is ^[A-Z][A-Z0-9]{1,9}$ (no dash); an issue key has the dash.
export function isProjectRoute(route: DispatchRoute): route is ProjectRoute;

// components/Tabs.tsx — the WAI-ARIA tablist from IssueTabs, generic
export interface TabDefinition<Id extends string> { id: Id; label: string }
export function Tabs<Id extends string>(props: { activeTab: Id; ariaLabel: string; idPrefix: string; onBeforeTabChange?: (current: Id) => void; onSelect: (tab: Id) => void; tabs: readonly TabDefinition<Id>[] }): ReactNode;
// ids: `${idPrefix}-${id}-tab`, panels `${idPrefix}-${id}-panel` (IssuePage's `issue-spec-panel` etc. keep working with idPrefix "issue").
// IssueTabs becomes: <Tabs ariaLabel="Issue detail" idPrefix="issue" tabs={issueTabs} activeTab onBeforeTabChange onSelect={(tab) => navigate(buildIssuePath({ key, kind: tab }))} />

// api/client.ts (new/changed)
listProjects(): Promise<Project[]>                                            // Project.open_asks
listIssues(options: ListIssuesOptions): …                                     // options.pinned → "pinned=true"
listProjectArtifacts(key: string, unlinked?: boolean): Promise<Artifact[]>
uploadArtifact(owner: ArtifactOwner, input: CreateArtifactInput): Promise<ArtifactUploadResponse> // ArtifactOwner = { issue: string } | { project: string }
getProjectArtifact(key: string, slug: string): Promise<ArtifactDetails>
listArtifactAsks(id: string, state?: "all" | "open" | "answered"): Promise<Ask[]>
createArtifactAsk(id: string, input: CreateAskInput): Promise<Ask>
listArtifactComments(id: string): Promise<Comment[]>
createArtifactComment(id: string, input: CreateCommentInput): Promise<Comment>
getArtifactEvents(id: string, options?: ListEventsOptions): Promise<Event[]>
getIssueReferences(key: string): Promise<IssueReferences>
getInbox(project?: string): Promise<InboxAsk[]>

// api/sse.ts eventQueryKeys — a document event (issue_key === null):
//   ["artifact", event.artifact_id], ["project", event.project, "artifacts"], ["projects"], and ["inbox"] when type starts with "ask."; nothing keyed by issue.
//   Issue events additionally invalidate ["projects"] on ask.* (sidebar badges) and ["issues"] as today.

// features/sidebar/Sidebar.tsx — arrangeIssues deleted
// Inbox row: badge = inbox.data.length (["inbox"]). Pinned section: api.listIssues({ pinned: true }) key ["issues", "pinned"], rows key · title · open_asks badge, aria-current on the current issue.
// Projects section: api.listProjects() key ["projects"], rows key · name · open_asks badge, aria-current when pathname starts with /projects/<key>. Settings link (users only).

// features/project/ProjectPage.tsx — /projects/:key and /projects/:key/documents
// header: key (linkText), name from ["projects"]; Tabs ariaLabel "Project" idPrefix "project" tabs Issues | Documents; useDocumentTitle(`${key} · ${name} · Dispatch`); unknown key → NotFoundPage.
// IssueList: api.listIssues({ project }) key ["issues", "project", key] + api.getMyState() ["user-state"]; filters per decision 2; rows per spec; row link buildIssuePath({ key, kind: "issue" }); parent chip links to the parent.
// DocumentList: api.listProjectArtifacts(key) key ["project", key, "artifacts"]; rows name · kind · issue chip (link) or blank · updated (latest version created_at, <Timestamp>); row link: linked → buildIssuePath({ key: issue_key, kind: "artifact", slug }), unlinked → buildProjectPath({ kind: "document", project, slug });
//   New document form (label "New document", input "Title", submit "Create") → api.uploadArtifact({ project }, { name: title, content: `# ${title}\n` }) → navigate to the document path; duplicate-name refusal per decision 3; Upload owner={{ project }}.

// features/inbox/Inbox.tsx — a document ask's heading: `${document.project} · ${document.name}` linking to buildProjectPath({ kind: "document", project, slug, item: { kind: "ask", id } });
// AskCard onSuccess: issue ask → invalidate ["issue", issue_key], ["issues"]; document ask → ["artifact", artifact_id], ["projects"].
```

- [ ] **Step 1: Failing unit tests.**
  - `routes.test.ts`: `"parses and builds project, documents, and document routes (version and item query)"`, `"maps dispatch:// project artifact references onto the document path and back"`, `"a dashed key is never a project route and a bare project is never an issue route"`.
  - `Tabs.test.tsx`: `"tablist moves focus and selection with arrow, Home, and End keys and marks aria-selected"`; `IssueTabs.test.tsx` (from #881) keeps passing unchanged.
  - `__tests__/sidebar.test.ts` (rewrite): `"sidebar lists Inbox, Pinned, Projects, and Settings with open-ask badges and renders no issue rows"` (mock `listProjects` → two projects with `open_asks` 3 and 0, `getInbox` → 2, `listIssues({ pinned: true })` → one issue), `"sidebar marks the current project"`, `"sidebar never requests the full issue list"` (spy `listIssues` called once with `{ pinned: true }`, `getIssue` never).
  - `IssueList.test.tsx`: `"groups issues by status in board order with done collapsed and empty groups hidden"`, `"Needs you keeps only issues with open asks"`, `"Unread keeps only issues with events past last_read_seq"`, `"label filter and title search narrow the list"`, `"a child row shows its parent chip"`.
  - `DocumentList.test.tsx`: `"lists documents with kind, issue chip or blank, and updated time"`, `"New document posts the title as an H1 and navigates to the document"`, `"New document refuses a name that already exists"`.
  - `ProjectPage.test.tsx`: `"renders header, tabs, and the Issues panel; /documents selects Documents"`, `"unknown project shows the not-found view"`.
  - `__tests__/ask-card.test.tsx` + Inbox: `"a document ask shows PROJECT · document name and links to the document page"`, `"answering a document ask invalidates the document and projects, not an issue"`.
  - `__tests__/sse.test.ts`: `"a document event invalidates the artifact, its project's documents, projects, and the inbox for asks"`.
  - `client.test.ts`: the new methods hit their paths; `uploadArtifact({ project: "CORE" }, …)` posts to `/api/v1/projects/CORE/artifacts`.
  - `app.test.tsx`: `"/projects/CORE and /projects/CORE/documents render the project page; /projects/CORE/documents/x is not found until the document route exists"` — this task registers only `/projects/:key` and `/projects/:key/documents`; Task 8 registers the document route and changes this assertion.
  - Run `cd packages/dispatch && bun run typecheck; bun run test` → FAIL (missing modules, `arrangeIssues` export gone).
- [ ] **Step 2: Implement** per Interfaces. `Sidebar` deletes `arrangeIssues`, `SidebarEntry`, `SidebarGroup`, `IssueLink`'s child nesting. `AppShell`'s `pageIdentity` becomes `parseIssuePath(pathname)?.key ?? parseProjectPath(pathname)?.project ?? pathname`. `packages/dispatch/AGENTS.md` Layout: the sidebar sentence, the project routes, the document route (Task 8 completes it).
- [ ] **Step 3: Run** — `cd packages/dispatch && bun run lint && bun run typecheck && bun run test` → PASS.
- [ ] **Step 4: e2e.**
  - `nav.e2e.ts` (rewrite both tests): `"sidebar shows Inbox, Pinned, one row per project with its open-ask badge, and Settings — never an issue row — on desktop and in the phone drawer"` (acceptance 1): `createProject(CORE)`, `createProject(OPS)`, 20 issues (12 CORE, 8 OPS), 2 open asks on CORE issues, 1 on OPS; `page.goto("/")`; open the drawer on iphone; assert links `Inbox` (badge 3), `Pinned` heading absent (nothing pinned), `CORE` row badge `2`, `OPS` row badge `1`, `Settings`; `page.getByRole("link", { name: /^(CORE|OPS)-\d+/ })` count 0; record requests: no `/api/v1/issues/<key>` and the only `/api/v1/issues?` request is `pinned=true`. Keep the issue-tabs/title/not-found assertions from the old first test in a second test navigating directly by URL.
  - `project.e2e.ts` new: `"project page groups issues by status in board order with done collapsed; Needs you, Unread, label, and search filters narrow the list; a child row shows its parent chip"` (acceptance 2): 8 issues across statuses (`patchIssue` status), one `done`, one child, labels via `patchIssue({ labels })`, one open ask, alice reads one issue (`putIssueState(last_read_seq)`); assertions on group headings order, `done` `<details>` closed, filter results, parent chip link.
  - `phone.e2e.ts`: on iphone, `/projects/CORE` and `/projects/CORE/documents`: `document.documentElement.scrollWidth <= clientWidth`; the tabs, filter toggles, `Create`, and Upload controls have `boundingBox().height >= 44`.
  - Run `cd packages/dispatch && bun run e2e` → PASS on both projects; save `test-results/` screenshots of the sidebar and the project page for the PR.
- [ ] **Step 5: Commit and open PR D** — `jj describe -m "feat(dispatch-web): projects navigation — sidebar of projects, project page with grouped issues, filters, and documents" && jj new`; bookmark `dispatch/project-documents-spa-nav`.

---

### Task 8: SPA — document page, margin by owner, References section (PR E)

Base: `main` after PR D and PR 3 merge (PR 3's margin is quote-anchored and its SPA has no CodeMirror; `e2e/preview.ts` `selectPreviewText` exists). Bookmark `dispatch/project-documents-spa-doc`. If PR 4 has merged first, the page renders its `ArtifactDocument`; the margin owner interface below is what PR 4's planner was told to keep (`owner`, not `issueKey`).

**Files:**
- Create: `features/document/DocumentPage.tsx`, `DocumentPage.test.tsx`
- Modify: `web/src/app.tsx` (route `/projects/:key/documents/:slug` → `DocumentPage`), `features/artifacts/ArtifactDocument.tsx` (`issueKey` → `owner`), `features/artifacts/ArtifactHeader.tsx` (project back-link; version picker navigates with `buildProjectPath` for a document owner), `features/artifacts/ArtifactsTab.tsx` (References section; Referenced by renders artifact sources and null `issue_key`), `features/margin/useMarginItems.ts`, `useMarginItems.test.ts`, `Margin.tsx`, `Margin.test.tsx`, `MarginSheet.tsx`, `MarginSheet.test.tsx`, `Composer.tsx`, `Composer.test.tsx`, `CommentsTab.tsx`, `useAnsweredAsks.ts`, `features/refs/Unfurl.tsx` (document unfurl: name via `getProjectArtifact`)
- e2e: create `document.e2e.ts`; append to `project.e2e.ts` (acceptance 7) and `phone.e2e.ts` (document page); `e2e/api.ts` helpers `createArtifactAsk(id, input)`, `createArtifactComment(id, input)`, `getIssueReferences(key)`

**Interfaces:**
```ts
// features/margin/useMarginItems.ts
export type MarginOwner =
  | { kind: "issue"; key: string }
  | { kind: "document"; artifactId: string; project: string; slug: string };
export function useMarginOwner(): MarginOwner | undefined; // parseIssuePath → issue; parseProjectPath document route → runs the ["artifact-ref", `${project}/${slug}`] query (below) and yields the document owner once the artifact id is known
// document owner: asks api.listArtifactAsks(id) key ["artifact", id, "asks"]; comments api.listArtifactComments(id) key ["artifact", id, "comments"];
//   needsYou = inbox asks with artifact_id === id; isClosed false; pinned/pinnedIds empty (tab hidden); routeItemId from ?ask=|?comment=; visibleArtifact = the document.
// The artifact id for a document route comes from useQuery(["artifact-ref", `${project}/${slug}`], () => api.getProjectArtifact(project, slug)) — the same query DocumentPage uses, so one request serves both.
// MarginSheetModel.items: issueKey → owner: MarginOwner | undefined; MarginSheet hides the Pinned tab and the message composer for a document owner.
// Composer: prop owner: MarginOwner; ask → issue ? api.createAsk(key) : api.createArtifactAsk(id); comment/suggest likewise; attach → uploadFile(owner.kind === "issue" ? { issue: key } : { project }, file) and inserts buildDispatchReference of the result.
// CommentsTab: prop owner; version links: issue → buildIssuePath, document → buildProjectPath({ kind: "document", …, version }).
// useAnsweredAsks(owner, artifactID): issue → api.listIssueAsks(key, "all") ["asks", key]; document → ["artifact", id, "asks"].

// features/artifacts/ArtifactDocument.tsx
export function ArtifactDocument(props: { artifact: Artifact; highlight?; isClosed: boolean; owner: MarginOwner; user: AuthenticatedUser; version: number | undefined }): ReactNode
// ArtifactVersionView loads comments by owner (issue: api.listComments(key, artifactId); document: api.listArtifactComments(artifactId)).

// features/artifacts/ArtifactHeader.tsx
// artifact.issue_key === null → the header's link reads `Project ${artifact.project}` → buildProjectPath({ kind: "documents", project }); the version <select> navigates with buildProjectPath({ kind: "document", project, slug, version }).

// features/document/DocumentPage.tsx — /projects/:key/documents/:slug[?version=n]
// useQuery(["artifact-ref", `${key}/${slug}`], () => api.getProjectArtifact(key, slug)); 404 → "Document not found" heading + link "Back to CORE documents";
// renders <ArtifactHeader artifact highlight={false} showVersionPicker version>{artifact.kind === "doc" ? <ArtifactDocument owner={{ kind: "document", … }} isClosed={false} …/> : <ArtifactBlobView …/>}</ArtifactHeader>
// then <section aria-label="Referenced by"> from artifact.referenced_by (item rows link the owning issue or document; artifact rows link the source document/artifact page); useDocumentTitle(`${artifact.name} · ${key} · Dispatch`).

// features/artifacts/ArtifactsTab.tsx — References section
// useQuery(["issue", issueKey, "references"], () => api.getIssueReferences(issueKey)); <section aria-label="References"> rows: name (link: unlinked → document path, linked → issue artifact path), chip (project key or issue key), `via ${via.kind}`, `depth ${depth}`; truncated → "more references beyond 8 hops"; empty → no section.
```

- [ ] **Step 1: Failing unit tests.**
  - `useMarginItems.test.ts`: `"a document owner loads asks and comments from the artifact routes, hides pinned, and is never closed"`, `"an issue owner behaves as before"` (existing cases re-pointed to the owner shape).
  - `Composer.test.tsx`: `"posts a document ask and comment to the artifact routes"`; `MarginSheet.test.tsx`: `"a document owner shows the Comments tab only and no message composer"`.
  - `DocumentPage.test.tsx`: `"renders the header, the document, versions, and Referenced by for an unlinked document"` (mock `getProjectArtifact`), `"shows Document not found with a way back for an unknown slug"`.
  - `ArtifactsTab.test.tsx`: `"References lists closure members with chip, via, and depth, and names truncation"`, `"Referenced by renders an artifact source without an issue key"`.
  - `Unfurl.test.tsx`: `"unfurls a dispatch:// project document reference with its name and document link"`.
  - Run `bun run typecheck; bun run test` → FAIL.
- [ ] **Step 2: Implement** per Interfaces; `app.tsx` registers the document route; `ArtifactDocument`'s `issueKey` prop is gone from every caller (`IssuePage.tsx` passes `owner={{ kind: "issue", key: issueKey }}`).
- [ ] **Step 3: Run** — `cd packages/dispatch && bun run lint && bun run typecheck && bun run test` → PASS.
- [ ] **Step 4: e2e** (alice and bob contexts via `asUser`; chromium and iphone):
  - `document.e2e.ts` test `documentCollaborationLive` (acceptance 3): alice on `/projects/CORE/documents` → New document "Design notes" → URL `/projects/CORE/documents/design-notes`; `getProjectArtifact("CORE", "design-notes")` succeeds (`ref_key CORE/design-notes`); bob opens the same URL and sees `# Design notes`; an agent `editArtifact(id, { ops: [{ op: "insert", markdown: "Alice wrote this.", after: "end" }] })` (the API stands in for typing until PR 4) → bob's `article` shows `Alice wrote this.` within 1 s (`expect(...).toContainText(..., { timeout: 1000 })`); alice reloads → content kept; the version `<select>` lists one more unnamed version. **PR 4 upgrade (recorded here, executed in PR 4's plan):** alice types in the Proof editor instead of the API edit; the version appears after the 2 s settle.
  - `document.e2e.ts` test `"comment, suggest, and ask anchor marks on a project document; accept edits the text; the ask flows through the Inbox"` (acceptance 4): `selectPreviewText(page, "Design")` → Comment (body) / Suggest (replace_with) / Ask (question) from the margin selection actions; both users' margins show the three items with the quote; alice accepts the suggestion → `article` text changes and the versions list gains a named version; the Inbox (bob) lists `CORE · Design notes` linking to the document page; answering it removes it from the Inbox and the document margin shows the answer.
  - `project.e2e.ts` append test `"an issue's Artifacts tab lists its reference closure; a document's page lists who references it; the issue tabs are Spec, Log, Children, Artifacts with no make-primary control"` (acceptance 7): comment on CORE-1 links `dispatch://CORE/artifact/design-notes`; design-notes text links `dispatch://CORE-2/artifact/diagram-png`; `/issues/CORE-1/artifacts` References shows both with depth 1 and 2; clicking design-notes lands on `/projects/CORE/documents/design-notes`; Referenced by lists the CORE-1 comment; `getByRole("tab")` names are exactly Spec, Log, Children, Artifacts; no `Make primary` button.
  - `phone.e2e.ts`: iphone on the document page: no horizontal overflow; controls ≥ 44 px; the margin bottom sheet (`button /review panel/i`) opens and shows the Comments tab.
  - Run `cd packages/dispatch && bun run e2e` → PASS on both projects; keep the screenshots.
- [ ] **Step 5: Commit and open PR E** — `jj describe -m "feat(dispatch-web): project document page, margin by owner, issue reference closure" && jj new`; bookmark `dispatch/project-documents-spa-doc`; `packages/dispatch/AGENTS.md` gains the document page paragraph (routes, margin owner, References).

---

### Task 9: Live acceptance and the phone check

No code. Runs after PR E merges and the server is deployed (or against the dev stack with NATS as the cheapest real substitute).

- [ ] **Step 1:** `OMP_BIN=<omp> DISPATCH_URL=<url> DISPATCH_TOKEN=<token> ENVOY_NATS_URL=<nats> bash packages/dispatch/e2e/acceptance/document-roundtrip.sh` → exit 0; the output shows the `dispatch_artifact` result topic `notifications.dispatch.document.CORE.runbook-md.>`, the `then two` text, and natstail's `artifact.version` envelope (acceptance 8, with decision 12).
- [ ] **Step 2:** With the same OMP session's browser open on `/projects/CORE/documents/runbook-md`, run the `dispatch_doc_edit` again and watch the text change without reload (acceptance 8 "lands in an open browser").
- [ ] **Step 3:** Manual phone check per `packages/dispatch/AGENTS.md` → Phone acceptance: open the project page and a document from a phone on the tailnet, open the margin sheet, answer an open document ask from the Inbox.
- [ ] **Step 4:** Record results in PR E's body (link the run outputs); close any hardening-ledger rows opened during review.

---

## Acceptance pass (surfaces, drivers, tooling)

| Spec line | Named proof | Surface a human or operator touches | What drives it | Tooling added by this plan |
|---|---|---|---|---|
| 1 | `nav.e2e.ts` "sidebar shows Inbox, Pinned, one row per project…"; `sidebar.test.ts` | the dashboard sidebar at `/`, desktop and phone drawer | Playwright via `e2e/run-server.sh`, header identity | — |
| 2 | `project.e2e.ts` "project page groups issues by status…"; `IssueList.test.tsx` | `/projects/CORE` with the filter bar | Playwright; `e2e/api.ts` `patchIssue`, `putIssueState`, `createAsk` | — |
| 3 | `document.e2e.ts` `documentCollaborationLive` (API-edit form now; typing form after PR 4) | `/projects/CORE/documents` New document → `/projects/CORE/documents/design-notes` in two browsers | Playwright alice+bob contexts | `e2e/api.ts` `createProjectDocument`, `getProjectArtifact` |
| 4 | `document.e2e.ts` "comment, suggest, and ask anchor marks on a project document…" | the document margin and the Inbox | Playwright; PR 3's `selectPreviewText` | `e2e/api.ts` `createArtifactAsk`, `createArtifactComment` |
| 5 | `TestCreateProjectDocumentJSONAndMultipart`, `TestListProjectArtifactsIncludesIssueArtifactsExceptPrimaries` | `curl -X POST /api/v1/projects/CORE/artifacts` with a real cookie/header identity | Go API tests on an isolated Postgres | — |
| 6 | `TestIssueReferencesClosureDepthViaAndETag`, `TestClosureFollowsArtifactLinksToDepthEightAndReportsTruncation` | `curl -H 'If-None-Match: …' /api/v1/issues/CORE-1/references` | Go API + refs tests | `refs` package tests double as the closure oracle |
| 7 | `project.e2e.ts` "an issue's Artifacts tab lists its reference closure…"; `ArtifactsTab.test.tsx` | `/issues/CORE-1/artifacts` → click → document page | Playwright | `e2e/api.ts` `getIssueReferences` |
| 8 | `dispatch-execute.test.ts` project cases + `document-roundtrip.sh` (Task 9) | a real OMP session calling `dispatch_artifact`/`dispatch_doc_read`/`dispatch_doc_edit`; a NATS subscriber | Bun tests with mocked HTTP; the live script over tmux + OMP + `natstail` | **`cmd/natstail`** (Task 5) and **`document-roundtrip.sh`** (Task 6) — no NATS tail existed on this machine (`which nats` → none) |
| 9 | `TestMigrate0009BackfillsProjectAndRefKeyFrom0007`, `TestMigrate0009DropsMalformedArtifactReferences` | booting `go run ./cmd/dispatch` against an existing database | Go store tests on a database migrated through 0007 | `migrateThrough` test helper (store package) |
| 10 | `phone.e2e.ts` (project page + document page) + Task 9 Step 3 | a phone on the tailnet | Playwright `iphone` project; the manual check AGENTS.md requires | — |

Restricted real paths and their cheapest substitutes: production boot of the migration → a local `go run ./cmd/dispatch` against a `pg_dump`/restore of production (migration 0009 removes malformed derived artifact refs with a notice before applying); the deployed NATS → the dev stack with `ENVOY_NATS_URL` and `natstail`; a real GitHub login → header identity exactly as `run-server.sh` configures it (never a minted cookie).

Prose deliverables (READMEs, AGENTS.md, `skills/dispatch/SKILL.md`) are verified by reading each changed sentence against the running behaviour it describes (a `curl`, a tool call, a page), never by grepping for the new wording.

## PR strategy

| PR | Bookmark | Tasks | Base | Merges after |
|---|---|---|---|---|
| A `feat(dispatch): remove the make-primary toggle` | `dispatch/remove-make-primary` | 1 | `main` post-#881 | #881 |
| B `feat(dispatch): project documents and the reference graph — server` | `dispatch/project-documents-server` | 2, 3, 4, 5 | `main` now | A (rebase over A's `api/artifacts.go`, `api/server.go` deletions) |
| C `feat(dispatch-tools): project as an owner for Dispatch tools` | `dispatch/project-documents-tools` | 6 | `main` post-#881, #882, A | B |
| D `feat(dispatch-web): projects navigation and project page` | `dispatch/project-documents-spa-nav` | 7 | `main` post-C (which implies #881, #882, A, B) | C |
| E `feat(dispatch-web): project document page, margin by owner, references` | `dispatch/project-documents-spa-doc` | 8 (+9 evidence) | `main` post-D and PR 3 | D, PR 3 |

Each PR: commit per task, `jj bookmark set <name> && jj git push`, register with the merge queue, Thermo pair for B (production Go). PR bodies carry the acceptance-pass rows they close with the test names above and the pasted verification output; nothing is left as "remaining work".

## Self-review

- **Spec coverage:** Schema table → Task 2 (every column, constraint, index, preflight; version 0009 not 0008 because PR 3 owns 8 as a Go marker) ✓. API table → Task 3 (`GET /projects` `open_asks`, project artifact list/create/slug routes, owner-scoped asks/comments/events, inbox `document`, `requireOpenOwner`/docs left joins) and Task 4 (`/issues/{key}/references` with the CTE, ETag, 304; `/artifacts/{id}/references`; `text/refs.go` parsing; one `refs.Replace`) ✓. Events and agents table → Task 2 (`Append` by owner, document topic, document notify routing), Task 6 (tool `project` owner, auto-subscribe topic) and Task 7 (SSE keys) ✓. SPA table → Task 7 (Sidebar, routes, ProjectPage/IssueList/DocumentList, `components/Tabs.tsx`, Inbox/AskCard) and Task 8 (DocumentPage, ArtifactsTab References, margin) ✓. Errors table → `PROJECT_NOT_FOUND` (T3), `ARTIFACT_INPUT` on `primary` (T1), `ISSUE_CLOSED` unchanged and unreachable for unlinked (T3 `requireOpenOwner`), dangling link stored/absent (T4 tests), tool owner errors (T6), depth-8 truncation (T4, T8 text), migration preflight (T2) ✓. Testing table → every named file exists in a task ✓. Rejected → nothing here reintroduces a `KB` project, a Documents sidebar entry, a Related panel, per-user document state, or messages on documents ✓.
- **Placeholders:** none — every task names files, functions, tests, commands, and expected red/green.
- **Type consistency:** `owner{IssueKey, ArtifactID *string}` (T2) is what T3's handlers and `documentOwnerFromRequest` use; `artifactOwner`/`lockArtifactOwner` (T3) is used by `settleRoom` and `NamedVersion`; `refs.Member{RefKey, Depth, Via}` (T4) is hydrated to `model.ReferenceMember` (T2 types) in `api/references.go`; `MarginOwner` (T8) is the same shape `ArtifactDocument`, `Composer`, `CommentsTab`, `useAnsweredAsks` receive; `ArtifactOwner`/`uploadFile(owner, file)` (T7) is what `Composer` calls in T7 and T8; `ProjectRoute` (T7) is what `Inbox`, `Unfurl`, `DocumentPage`, `CommentsTab` build; the `string | null` owner keys widened in T6 are narrowed in T6 (text) and given their links in T7/T8 (routes exist), never defaulted.
- **Judgment calls the executor must not "fix":** decisions 1–26 above; `url` refs are not stored; document events never get route or ask-author copies; the Pinned tab is hidden on documents; `parseIssuePath`/`buildIssuePath` keep their names; `primary` is rejected with `ARTIFACT_INPUT`, not dropped silently; migration is 0009 (re-checked on every rebase).

## Hardening ledger

Rule: a hardening item found while implementing or reviewing any task is appended here as `| # | found by | item | closed in (commit) |` and closed in this plan's PRs — never deferred to a follow-up.

(empty — filled by the implementer/reviewer as hardening items are found and closed)
