# Dispatch project documents and the reference graph

## Decisions needed

None. Settled 2026-09-10 15:20Z by Sami on LEGION-2 asks 092d6fa7 and da653f4c: project documents appear only on the project page's Documents tab (no top-level Documents page); a cross-project `dispatch://B/artifact/x` link is recorded like any reference and shows in References, Referenced by, and the closure.

## Acceptance

1. Playwright `nav.e2e.ts`: with 20 issues in two projects, the sidebar shows Inbox, Pinned, one row per project with its open-ask badge, and Settings; no issue row is rendered; the drawer on iPhone shows the same.
2. Playwright `project.e2e.ts`: `/projects/CORE` lists issues grouped by status in board order with `done` collapsed; the Needs you filter leaves only issues with open asks; Unread leaves only issues with events past the user's `last_read_seq`; a label filter and title search narrow the list; a child row shows its parent chip.
3. Playwright `document.e2e.ts` (alice + bob contexts): New document "Design notes" on `/projects/CORE/documents` creates `dispatch://CORE/artifact/design-notes` and opens `/projects/CORE/documents/design-notes` in the Proof editor; bob sees alice's text within 1 s; reload keeps the content; the version list gains one unnamed version after 2 s idle.
4. Playwright `document.e2e.ts`: on that document, select text → Comment, Suggest, Ask each anchor a mark visible to both users; Accept changes the text and writes a named version; the ask appears in the Inbox as `CORE · Design notes`; answering it removes it from the Inbox and shows the answer in the margin.
5. API `artifacts_test.go`: `POST /api/v1/projects/CORE/artifacts` (multipart file and JSON `content`) returns 201 with `issue_key: null`, `project: "CORE"`, `primary: false`; `GET /api/v1/projects/CORE/artifacts` lists it plus every non-primary artifact of CORE issues with `issue_key`; `?unlinked=true` returns only project documents.
6. API `references_test.go`: a CORE-1 comment links `dispatch://CORE/artifact/design-notes`, whose body links `dispatch://CORE-2/artifact/diagram-png`; `GET /api/v1/issues/CORE-1/references` returns both members with `depth` 1 and 2 and `via`; the same `If-None-Match` returns 304; a new design-notes version without the link drops the depth-2 member and changes the ETag.
7. Playwright `project.e2e.ts`: `/issues/CORE-1/artifacts` shows a References section with those two members; clicking design-notes opens its document page, whose Referenced by lists the CORE-1 comment; the issue tabs remain Spec, Log, Children, Artifacts and no make-primary control exists.
8. Envoy-client `dispatch-execute.test.ts` + one live OMP session: `dispatch_artifact({ project: "CORE", name: "Runbook.md", content })` creates an unlinked document; `dispatch_doc_read({ ref: "dispatch://CORE/artifact/runbook-md" })` returns its markdown; `dispatch_doc_edit({ project, artifact, ops })` lands in an open browser; the tool result subscribes the session to `notifications.dispatch.document.CORE.runbook-md.>` and a NATS subscriber receives `version.created` there.
9. Go `store_test.go`: a database at `0007` with issues, artifacts, asks, comments, events, and `refs` boots on `0009`: every artifact has `project_key` equal to its issue's project and `ref_key = issue_key || '/' || slug`; every pre-existing `refs` artifact target joins a `ref_key`; a second boot applies nothing; a fixture with a malformed `refs.to_id` fails boot naming the row.
10. Playwright `phone.e2e.ts`: project page and document page have no horizontal overflow, every control is ≥ 44 px, and the margin bottom sheet opens on a document.

## Requirements

| Requirement | Provenance |
| --- | --- |
| Every document belongs to one project and is listed on that project's page. | verbatim: "documents in the knowledge graph should be scoped to a project so that you can easily see all the documents in a project" |
| Cross-project references are the eventual graph; documents themselves live in one project. | verbatim: "We'll also want the ability to reference documents in other projects, so that you have a cross-project graph. But to start, they should be within a project." |
| No graph canvas in this release. | verbatim: "The first release does not need a graph canvas ... I don't think that will be very useful." |
| A document has the issue spec's collaboration: live multiplayer editing, comments, suggestions, asks, versions. | verbatim: "Knowledge pages also have the same full collaboration functionality." |
| The unit is the existing artifact; an artifact needs no issue; no new entity kind. | verbatim: "I said documents or artifacts. Artifacts should not have to be linked to an issue." |
| No dedicated knowledge project. | verbatim: "I don't like this idea of a dedicated knowledge-based project." |
| Navigation is projects → issues and documents, in the shape of Linear and GitHub, not one list of every issue. | verbatim: "It looks like it's going to turn into just an infinite ever-scrolling list of issues ... We should probably take some inspiration from Linear and GitHub's UIs for how to organize projects and issues." |
| The artifacts an issue reaches, directly or through its artifacts, are one cheap query. | verbatim: "make it cheap to compute all of the artifacts that are referenced by this issue or by artifacts in this issue." |
| The issue body is the only primary artifact, fixed at creation; nothing toggles it. | verbatim: "there has always been and only ever is one primary artifact for the issue, and that is the issue body itself, the issue spec" |
| References are graph edges, readable in both directions. | inferred: to "The aim is not a graph database" Sami answered "I'm not sure that's true, honestly." |

## Design

Borrowed: Linear's sidebar (Inbox, favorites, projects) and project page (issues grouped by status with a filter bar, a Documents tab); GitHub's issue-list filter chips (status, label, search) and per-repo docs, here per project.

| Schema (`store/migrations/0009_project_artifacts.up.sql`, one transaction under `Store.Migrate`) | Detail |
| --- | --- |
| `artifacts.project_key text not null references projects(key)` | backfilled from `issues.project_key`; `issue_key` becomes nullable; `unique (issue_key, slug)` dropped |
| `artifacts.ref_key text generated always as (coalesce(issue_key, project_key) \|\| '/' \|\| slug) stored`, `unique (ref_key)` | one identity for `refs.to_id` and slug uniqueness: per issue when linked (`CORE-1/spec`), per project when not (`CORE/design-notes`); project keys have no `-`, so the two never collide |
| `artifacts.last_seq integer not null default 0`; `check (not is_primary or issue_key is not null)` | an unlinked document owns its own event sequence and is never primary; `is_primary` is written once by `api/issues.go` issue creation; `POST .../primary` and the `primary` upload flag leave in the make-primary removal PR that follows #881 |
| `asks`, `comments`, `events`: `issue_key` nullable, `artifact_id uuid references artifacts(id)`, `check ((issue_key is null) <> (artifact_id is null))` | exactly one owner; `events` gains `unique (artifact_id, seq)`; `asks_open_artifact` partial index; `messages`, `user_issue_state` unchanged |
| Preflight in the same transaction | `raise exception` naming any `refs` artifact target not matching `^[A-Z][A-Z0-9]{1,9}(-[0-9]+)?/.+$`; the migration is skipped when `schema_migrations` has version 9 |

| API (`api/server.go` routes) | Behaviour |
| --- | --- |
| `GET /api/v1/projects`; `GET/POST /api/v1/projects/{key}/artifacts` | projects gain `open_asks` (sidebar badge); list every artifact with `project_key = key` except primaries, each with `issue_key` or null, `?unlinked=true` filter; create an unlinked artifact with the same body as `POST /issues/{key}/artifacts` minus `primary` |
| `GET /api/v1/projects/{key}/artifacts/{slug}` + `/text`, `/versions/{n}`, `POST .../versions`, `.../edits`; `GET/POST /api/v1/artifacts/{id}/asks`, `/comments`; `GET /api/v1/artifacts/{id}/events` | slug routes mirror the issue ones through one `loadArtifactByRefKey`; owner-scoped collaboration for unlinked documents; `/asks/{id}/*` and `/comments/{id}/*` unchanged |
| `GET /api/v1/issues/{key}/references` | one recursive CTE over `refs`: seeds are targets of the issue's asks, comments, messages, and artifacts; each step follows `refs.from_kind = 'artifact'` from members joined on `ref_key`; `UNION`, depth ≤ 8; result `{ members: [{ artifact, depth, via }], truncated }` excluding the issue's own artifacts; `ETag` = hash of `issue.last_seq`, member ids, member latest version numbers; 304 on match |
| `GET /api/v1/artifacts/{id}/references` | `outgoing` (this artifact's `refs`) and `referenced_by` (`refs_to`, now including artifact sources) |
| `GET /api/v1/inbox` | `left join issues`, `left join artifacts`; a document ask carries `document: { project, slug, name }` instead of `issue` |
| `requireOpenIssue`, `docs/service.go:299-303, 517-521` | `left join issues`; an artifact without an issue is always open |
| `text/refs.go`, `api/refs.go`, `docs/service.go` reference indexing | `dispatch://<PROJECT>/artifact/<slug>[/ask/<id>|/comment/<id>]` parses when the first segment matches the project-key pattern; `to_id` is the `ref_key`; one `refs.Replace` writer replaces the two current ones |

| Events and agents | Behaviour |
| --- | --- |
| `events/broker.go` `Append` | locks `issues.last_seq` or `artifacts.last_seq` by owner; event types unchanged |
| `outbox/publisher.go` | topic `notifications.dispatch.document.<PROJECT>.<slug>.<type>` for artifact-owned events; issue topics unchanged; `notify` on a document event routes only to that topic's subscribers |
| `GET /api/v1/events` (SSE) and `web/src/api/sse.ts` | events carry `issue_key \| null`, `artifact_id \| null`, `project`; document events invalidate `["artifact", id]`, `["project", key, "artifacts"]`, and `["inbox"]` for ask events |
| `contracts/src/dispatch-tools.ts`, `envoy-client/src/dispatch-execute.ts` | `dispatch_artifact`, `dispatch_comment`, `dispatch_suggest`, `dispatch_ask`, `dispatch_doc_edit`, `dispatch_doc_read`, `dispatch_read` accept `project` as the alternative to `issue` (exactly one); with `project`, `artifact` is required except for `dispatch_artifact`; `resolveIssueArguments` resolves either owner; tool results auto-subscribe to the document topic; `dispatch_message` and `dispatch_issue` unchanged |

| SPA (`packages/dispatch/web/src`; layout per `packages/dispatch/AGENTS.md`: three columns at `xl`, drawer and margin sheet below, colours from `theme/classes.ts`) | Behaviour |
| --- | --- |
| `features/sidebar/Sidebar.tsx` | Inbox (open-ask badge), Pinned (`user_issue_state.pinned` issues), Projects (one row each, open-ask badge), Settings; `arrangeIssues` deleted |
| `app.tsx`, `features/refs/routes.ts` | routes `/projects/:key`, `/projects/:key/documents`, `/projects/:key/documents/:slug[?version=n]`; `DispatchRoute` gains `project`, `documents`, `document`; `dispatch://<PROJECT>/artifact/<slug>` ↔ the document path |
| `features/project/ProjectPage.tsx`, `IssueList.tsx`, `DocumentList.tsx` | header (key, name), tabs Issues \| Documents using the `IssueTabs.tsx` tablist extracted to `components/Tabs.tsx`; issue rows: key, title, labels, open-ask badge, unread dot, updated, parent chip; grouped by status in board order, `done` collapsed; filters: status, label, Needs you, Unread, search; document rows: name, kind, issue chip or blank, updated; actions New document (title → slug) and Upload (`features/artifacts/Upload.tsx`) |
| `features/document/DocumentPage.tsx` | #881's `ArtifactHeader` + `ArtifactDocument` for a project document with the Margin (Comments, Pinned), asks, versions, and a Referenced by list; read-only never applies |
| `features/artifacts/ArtifactsTab.tsx` | adds a References section from `/issues/{key}/references`: name, project or issue chip, `via`, depth; issue tabs otherwise unchanged |
| `features/inbox/AskCard.tsx` | a document ask shows `PROJECT · document name` and links to the document page |

## Errors

| Condition | Behaviour |
| --- | --- |
| `POST /projects/{key}/artifacts` for an unknown project | 404 `PROJECT_NOT_FOUND` |
| `primary: true` on a project upload, or `is_primary` on a row without `issue_key` | 400 `ARTIFACT_INPUT`; the check constraint rejects it at the row |
| Mutation of an artifact whose issue is closed | 409 `ISSUE_CLOSED` (unchanged); unlinked artifacts never reach it |
| A `dispatch://` artifact link whose target does not exist | stored in `refs` as written; absent from References and Referenced by until the target exists; `dispatch_doc_read` of it returns 404 `ARTIFACT_NOT_FOUND` |
| Tool call with both or neither of `issue` and `project`; `project` without `artifact` on a document tool | tool error naming the rule; nothing is created |
| Closure exceeds depth 8 | members to depth 8 with `truncated: true`; the References section says "more references beyond 8 hops" |
| Migration preflight finds a malformed `refs.to_id` or an artifact whose issue has no project | boot fails naming the row; `0009` is not recorded |

## Testing

| Acceptance | Proof |
| --- | --- |
| 1, 2, 7, 10 | Playwright `nav.e2e.ts` (rewritten), new `project.e2e.ts`, `phone.e2e.ts` extended; `Sidebar` and `IssueList` unit tests for grouping and filters |
| 3, 4 | new `document.e2e.ts` with alice and bob contexts, chromium and iphone projects |
| 5, 6, 8, 9 | Go `api/artifacts_test.go`, new `api/references_test.go`, `api/inbox` test for document asks, `text/refs_test.go`, `store/store_test.go` migration fixture at `0007`; `routes.test.ts`; `envoy-client/src/__tests__/dispatch-execute.test.ts` plus a live OMP session against the deployed server with a NATS subscriber |

## Rejected

| Alternative | Reason |
| --- | --- |
| `knowledge_nodes` or a `containers` spine with issue and knowledge kinds | "It's weird that you invented this new knowledge type ... I said documents or artifacts." |
| A dedicated `KB` project for knowledge pages | "I don't like this idea of a dedicated knowledge-based project." |
| A sidebar Knowledge entry over all knowledge pages | rejected with the draft; "The sidebar is currently kind of unusable ... an infinite ever-scrolling list" |
| A Related knowledge links panel on the issue page | replaced by References; "make it cheap to compute all of the artifacts that are referenced by this issue or by artifacts in this issue." |
| Graph canvas | "The first release does not need a graph canvas ... I don't think that will be very useful." |
| Make-primary toggle | "seems like the only reason that exists is as a hack ... the only way to actually look at an artifact is to make it primary"; removed after #881 |
| Approaches, second opinions, and rationale sections in this spec | "these are also just way too long for me to actually read." |
| "The aim is not a graph database" as the framing | "I'm not sure that's true, honestly." |
| Slug uniqueness per project for every artifact | inferred: every issue's `spec` slug collides |
| Artifact UUIDs as `refs.to_id` | inferred: slugs are immutable and there is no move API; `ref_key` is stable, indexed, and a link written before its target exists still resolves |
| Per-user unread and pinned state, and messages (Log updates), on documents | inferred: no navigation surface in this release reads document read state; a document's log is its events; `dispatch_message` stays issue-only |
