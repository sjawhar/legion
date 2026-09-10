# Dispatch margin threads (two PRs stacked on PR 3 and PR 4) — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Every discussion in the Dispatch margin is a one-level thread card — root, chronological flat replies, inline bottom composer — collapsed to "N replies · last reply" when inactive, hidden behind `Resolved (N)` when resolved and reopenable, editable by its author, aligned with its highlight in the document, and usable on a phone as a full-height thread view; asks are thread roots; agents reply in the same threads.

**Architecture:** Two PRs. **PR T-a (server addendum)** adds three nullable columns and four rules to `api/comments.go` after PR 3's Task 5 rewrite lands: `POST /comments/{id}/reopen`, `PATCH /comments/{id}` (author only), `reply_to` must be a thread root, a reply to a resolved root reopens it — plus the contract types and two events. **PR T-b (SPA threads)** replaces PR 4 Task 4's flat `CommentCard` list with `ThreadCard`/`ThreadList`, groups comments into `Thread` objects in `useMarginItems`, gives `Composer` an inline mode and keyboard submit, aligns anchored cards with their marks through `handle.markOffsets()`/`onMarkHover` (shipped in PR 4 Task 1's fork release), and adds the phone drill-down. Proof owns highlights (marks, decorations, click/hover, offsets); Dispatch owns the thread cards (threads spec § Component Boundaries).

**Tech Stack:** Go 1.26 + pgx (PR T-a); React 19 + TanStack Query, `@sjawhar/proof-editor` 0.2.0 hooks already bound by PR 4, Bun test + happy-dom, Playwright chromium + iphone (PR T-b).

**Spec:** `docs/superpowers/specs/2026-09-10-dispatch-threads-design.md` — § Acceptance (every line), § Requirements, § Design (Component Boundaries, File Changes, Data Model Deltas, Phone Behavior), § Errors, § Testing, § Rejected. Predecessors: plan 1 (`2026-09-10-dispatch-tree-server.md`, PR 3) and the PR 4 plan (`2026-09-10-dispatch-proof-editor.md`); this plan's SPA files are PR 4 Task 4/5 files taken over after PR 4 lands (§ Rebase deltas).

---

## Decisions needed (Sami)

None. The threads spec's own "Decisions needed" is empty; the thread-UI ownership question is settled (Main's ruling 2026-09-10: Proof owns highlights via `onMarkClick`, Dispatch owns the thread cards).

## Scope boundary

**In:** PR T-a — columns `comments.resolved_by/resolved_at/edited_at`, `POST /comments/{id}/reopen`, `PATCH /comments/{id}`, root-only `reply_to`, reply-reopens-root, events `comment.reopened`/`comment.edited`, contract types, docs. PR T-b — `Thread` grouping, `ThreadCard`/`ThreadList`, inline composer + `Cmd/Ctrl+Enter`/`Esc`, `Resolved (N)` toggle + Reopen, author edit + "edited", ask as thread root, anchor-aligned cards with hover cross-highlight, "Discussion" section for unanchored threads, phone thread view, `threads.e2e.ts`.

**Out:** everything PR 4 ships (editor, selection bar, `composeForMark`, mark focus, `mark_id` anchors); suggestion mode; hard delete of comments (spec § Rejected); `unresolve` of asks (asks resolve through their own route); notifications/Envoy delivery changes (events are additive, TOON encoding is Lane A5).

**Stacking:** PR T-a branch `dispatch-doc/threads-server` is cut from PR 3's head after its Task 5 commit lands (it edits `api/comments.go`, which Task 5 rewrites) and rebased onto PR 3's head as PR 3 advances; it opens with base `dispatch-doc/tree-server`, re-targets to `main` when PR 3 merges. PR T-b branch `dispatch-doc/threads-ui` is cut from PR 4's head after PR 4 Task 5 (mark wiring) lands and merged after PR 4 (base `dispatch-doc/proof-editor`, then `main`). PR T-b requires PR T-a's routes and types (`bun run typecheck` fails without `Comment.edited_at`); order of merge: PR 3 → PR T-a → PR 4 → PR T-b (PR T-a and PR 4 are independent and may merge in either order).

**Deploy rule inherited:** the devbox is not redeployed onto a `main` containing PR 3 until PR 4 has merged (PR 4 plan). PR T-a alone on `main` is deploy-safe (additive columns, new routes) but nothing is redeployed for it.

## Rebase deltas (what PR T-b takes over from PR 4, and what it must not collide with)

| File (PR 4 owner) | PR 4's final shape | PR T-b delta |
|---|---|---|
| `features/margin/useMarginItems.ts` (Task 4) | `useMarginItems(issueKey, tab, visibleArtifact, markPositions)` returning flat `MarginItem[]` (`commentThreads` recursion with `depth`), `marginItemMarkId`, action mutation accept/reject/resolve | returns `Thread[]` + `resolvedThreads` + `needsYou`; `markPositions` → `markPlacements`; `depth` gone; `reopen`/`editComment` mutations |
| `features/margin/CommentsTab.tsx` (Task 4) | `CommentCard` (recursive, `depth * 12px` indent, Accept/Reject/Resolve/Reply, orphan link, `aria-current`) | `CommentCard` deleted; `ThreadList` rendered; **Needs you** (spec-primary) stays first |
| `features/margin/Composer.tsx` (Task 4) | `ComposerAnchor { artifact, mark_id, quote }`, `onSaved`, `ANCHOR_MISSING` copy | + `inline`, `edit`, `Cmd/Ctrl+Enter`; unchanged mark payload |
| `features/margin/Margin.tsx` (Task 4) | `composeForMark`, `focusItemForMark`, `focusRequest`, `markPositions`, `selectItem/selectedItemId`, `hoveredItemId`, `DocumentBridge`, `registerDocument` | `selectedItemId` → `expandedThreadKey`; `markPositions` → `markPlacements`; + `hoveredMarkId`, `showResolved` |
| `features/margin/MarginSheet.tsx` (Task 4) | no `SelectionMenu`; tab shell; count badge (spec-primary) | + phone thread drill-down (`ThreadView` dialog) |
| `features/margin/useMarginListeners.ts` (Task 4) | `scrollCardIntoView`, card click → `onSelectCard`, hover → `setHoveredItemId` | unchanged API; cards are thread roots |
| `features/inbox/AskCard.tsx`, `AskThread.tsx` (Task 4 / spec-primary) | `artifactSlug` orphan link; inbox card | + `mode: "thread-root"` |
| `features/doc/ProofDocument.tsx`, `marks.ts` (Task 5) | `onMarkClick`, `setMarkPositions(markPositions(doc))`, `registerDocument` | + `onMarkHover → setHoveredMarkId`; `markPositions` → `markPlacements(doc, handle.markOffsets())` republished on `ResizeObserver` |
| `api/client.ts` | `acceptComment/rejectComment/resolveComment` | + `reopenComment(id)`, `editComment(id, { body })` |
| `api/sse.ts` | `knownEventTypes` is a `Record<EventType, true>` | + `comment.reopened`, `comment.edited` → `["comments", issue]` (PR T-a's contract change makes this a compile error until done — PR T-a owns this edit) |
| e2e `margin.e2e.ts` (PR 4 Task 6) | `marginCard(page, id)`; focus scenario asserts `aria-current` | unchanged; `threads.e2e.ts` is new; `threadCard`/`replyInThread` helpers added to `e2e/editor.ts` |
| `feat/spec-primary-layout` | **Needs you** section, `useMarginItems` dedupe of open anchors, `MarginSheet` count | preserved: open asks stay in **Needs you**; a thread whose root is an open ask renders there |
| `ff-project-docs` (if landed) | margin keyed by `MarginOwner` | `useMarginItems`/`Composer` keep the owner key; the document owner has no Log, so unanchored threads still render in **Discussion** |

## Global Constraints

- Same as the PR 4 plan: jj commits per task; per-package checks; theme composites; `aria-*` contract; `{ timeout: 1000 }` peer-visibility; server settle polls ≤ 10 s; red-first tests; absolute paths; no other workspaces.
- Migration numbers are claimed at merge time: plan 1 reserves `0008`; **this plan reserves `0011_comment_threads.up.sql` (0009 = project documents, 0010 = global search; re-check `ls store/migrations` against main@origin before pushing)**; before pushing PR T-a confirm no `0009_*` landed on `main` and renumber if one did.
- `reply_to` is always the thread root: the SPA sends `reply_to: root.id`; the server rejects a `reply_to` that is itself a reply (`400 INVALID_COMMENT "reply_to must be a thread root"`); existing deeper chains render flattened under their root (`threadRootId` walks `reply_to`/`ask_id` to the root).
- No thread text renders inside the editor; Proof supplies marks, decorations, click/hover and offsets only.
- Go checks are package-scoped (`cd packages/envoy && gofmt -l ./internal/dispatch && go vet ./internal/dispatch/... && DISPATCH_TEST_DATABASE_URL=<url> go test ./internal/dispatch/api/ -run <Test> -v`); Postgres via `./scripts/dev-postgres.sh`.

## Decisions where the spec under-specifies (decided here)

1. **Resolution provenance** = `resolved_by jsonb` (an `Actor`) + `resolved_at timestamptz`; accept/reject set them like resolve (an accepted or rejected suggestion is a resolved thread — spec § Acceptance "Accept applies doc edit and resolves thread"); reopen clears them and re-projects the mark with `resolved: false`.
2. **Edit** is body-only, author-only (`403 NOT_AUTHOR`), agents may edit their own comments through the same route (no tool change in this plan — `dispatch_comment` stays create-only); `edited_at` set; the anchored root's `marks` projection re-runs so Proof's metadata carries the new text.
3. **Reopen** is `POST /comments/{id}/reopen` on a root (a reply id → `400 INVALID_COMMENT "reopen the thread root"`); event `comment.reopened`; asks are not reopened by this route (asks have their own lifecycle).
4. **Thread key** = root id (`comment.id` or `ask.id`); exactly one thread is expanded at a time (`expandedThreadKey`); expansion sources: card click, `focusItemForMark`, route `/issues/KEY/comments/:id` or `/asks/:id`; `Esc` in the inline composer with an empty draft collapses; `Esc` with a draft shows the existing discard prompt.
5. **Collapsed card** = root's first two lines (`line-clamp-2`) + `N replies · last reply <Timestamp>` (omitted when `N === 0`); expanded = root, flat replies (`marginLeft: 0`), actions, inline composer (hidden when the issue is closed).
6. **Alignment**: anchored cards are absolutely positioned inside a `position: relative` list; `top = max(placement.top, previousCardBottom + 8)` in document order; the list height = last card bottom; unanchored threads render below in a **Discussion** section (newest first); threads whose mark is absent (orphaned) render after positioned ones with their "Text changed · View original" badge. `markPlacements` are republished on document change and on `ResizeObserver` of the editor root; card heights are read with `ResizeObserver` on the list so stacking stays correct after content changes.
7. **Resolved filter**: open threads only by default; header button `Resolved (N)` (count of resolved threads for the visible artifact + unanchored) toggles a **Resolved** section under the open list with **Reopen** on each card; posting a reply from a resolved card reopens it (server rule) — the composer stays available there.
8. **Suggestion cards** keep PR 4's inline diff (`del quote / ins replace_with`) with **Accept**/**Reject** on the root only; after accept/reject the thread is resolved (decision 1).
9. **Ask thread root** = `AskCard` in `mode: "thread-root"`: question, options, answer form, resolution/answer state as today; replies (human `reply_to_ask` chains and agent replies) flat beneath; collapsed summary like comments; the open-ask copy stays in **Needs you** (spec-primary) and is not duplicated below.
10. **Phone**: the sheet lists thread summaries (root two lines + count); tapping opens `<section role="dialog" aria-label="Thread">` covering the sheet's full height with **Back**, the expanded card, and the composer pinned to the bottom (`position: sticky; bottom: 0`); Esc/Back returns to the list; focus trapped like the sheet.
11. **Keyboard**: `Cmd/Ctrl+Enter` submits any `Composer` when `canSubmitComposer` is true (root composers included — one rule); `Esc` per decision 4.
12. **Hover cross-highlight**: editor hover (`onMarkHover`) → `hoveredMarkId` → the card whose thread mark matches gets `data-hovered="true"` + a ring composite; card hover → `setActiveMarks([markId])` (PR 4 mechanism).

---

## File Structure

```
PR T-a (branch dispatch-doc/threads-server, after PR 3 Task 5)
packages/envoy/internal/dispatch/store/migrations/0011_comment_threads.up.sql   NEW     [Task A1]
packages/envoy/internal/dispatch/model/model.go                                          [Task A1] Comment.ResolvedBy/ResolvedAt/EditedAt
packages/envoy/internal/dispatch/api/comments.go, server.go, comments_test.go            [Task A1] reopen, PATCH, root-only reply_to, reply reopens, resolved_by/at on resolve/accept/reject
packages/envoy/internal/dispatch/api (event type registry the outbox uses)               [Task A1] comment.reopened, comment.edited
packages/contracts/src/dispatch-api.ts + schema source (gen:go)                          [Task A1] Comment fields, EditCommentInput, event types
packages/dispatch/web/src/api/sse.ts, web/src/__tests__/sse.test.ts                      [Task A1] knownEventTypes + invalidations (compile-forced)
packages/envoy/cmd/dispatch/README.md, skills/dispatch/SKILL.md                          [Task A1] routes, NOT_AUTHOR, reply_to root rule

PR T-b (branch dispatch-doc/threads-ui, after PR 4 Task 5)
packages/dispatch/web/src/api/client.ts                                                  [Task B1] reopenComment, editComment
packages/dispatch/web/src/features/margin/useMarginItems.ts, useMarginItems.test.ts      [Task B1] Thread grouping, resolved split, placements order
packages/dispatch/web/src/features/margin/Composer.tsx, Composer.test.tsx                [Task B1] inline, edit, Cmd/Ctrl+Enter
packages/dispatch/web/src/features/margin/ThreadCard.tsx, ThreadCard.test.tsx            NEW     [Task B2]
packages/dispatch/web/src/features/margin/ThreadList.tsx, ThreadList.test.tsx            NEW     [Task B2]
packages/dispatch/web/src/features/margin/CommentsTab.tsx                                        [Task B2] CommentCard → ThreadList
packages/dispatch/web/src/features/inbox/AskCard.tsx, AskThread.tsx                              [Task B2] thread-root mode
packages/dispatch/web/src/features/margin/Margin.tsx, Margin.test.tsx                            [Task B3] expandedThreadKey, markPlacements, hoveredMarkId, showResolved
packages/dispatch/web/src/features/margin/MarginSheet.tsx, MarginSheet.test.tsx                  [Task B3] phone ThreadView dialog
packages/dispatch/web/src/features/margin/useMarginListeners.ts                                  [Task B3] expansion scroll
packages/dispatch/web/src/features/doc/ProofDocument.tsx, ProofDocument.test.tsx, marks.ts, marks.test.ts   [Task B3] onMarkHover, markPlacements
packages/dispatch/web/src/styles.css                                                             [Task B3] card hover ring is a composite; no new literals expected
packages/dispatch/e2e/editor.ts, e2e/threads.e2e.ts (NEW), e2e/margin.e2e.ts                    [Task B4]
packages/dispatch/AGENTS.md, README.md                                                           [Task B4]
```

| Task | Runs after | Concurrent with |
|---|---|---|
| A1 server addendum | PR 3 Task 5 (Task 8 preferred, to avoid rebasing over PR 3's own `comments.go` edits) | PR 4 Tasks 1–7 |
| B1 data + composer | PR 4 Task 4; PR T-a on the branch (types) | — |
| B2 cards | B1 | B3 |
| B3 margin state, placements, phone | B1; PR 4 Task 5 | B2 |
| B4 e2e + docs + PR | B2, B3 | — |

---

### Task A1 (PR T-a): threads server addendum

**Files:** per File Structure (PR T-a block).

**Interfaces (produces):**
```sql
-- 0011_comment_threads.up.sql
alter table comments
  add column resolved_by jsonb,
  add column resolved_at timestamptz,
  add column edited_at   timestamptz;
```
```go
// model.Comment gains
ResolvedBy *Actor  `json:"resolved_by"`
ResolvedAt *string `json:"resolved_at"`
EditedAt   *string `json:"edited_at"`
// routes
mux.HandleFunc("POST /api/v1/comments/{id}/reopen", s.reopenComment)
mux.HandleFunc("PATCH /api/v1/comments/{id}", s.editComment)
// rules
// reopenComment: human only; root only (a reply → 400 INVALID_COMMENT "reopen the thread root"); resolved → open; clears resolved_by/at;
//   anchored → ProjectMark(record with Resolved:false); event comment.reopened (payload CommentEventPayload); requireOpenIssue.
// editComment: {body}; capExceeded(body) as createComment; actor must equal comment.Author (kind+id) else 403 NOT_AUTHOR; sets edited_at=now();
//   anchored root → ProjectMark with the new Text; event comment.edited.
// createComment: reply_to must name a comment whose reply_to is null and ask_id is null (or a reply_to_ask → the ask) else 400 INVALID_COMMENT
//   "reply_to must be a thread root"; if the root is resolved → root.resolved=false, resolved_by/at=null, ProjectMark(Resolved:false),
//   event comment.reopened for the root — same transaction as the reply insert.
// resolveComment / accept / reject: resolved_by = actor, resolved_at = now().
```
```ts
// contracts
export interface Comment { …; readonly resolved_by: Actor | null; readonly resolved_at: string | null; readonly edited_at: string | null }
export interface EditCommentInput { readonly body: string }
// EventType gains "comment.reopened" | "comment.edited" (payload CommentEventPayload)
```

- [ ] **Step 1: Failing tests** (`api/comments_test.go`):

```go
func TestReopenCommentClearsResolutionAndReprojects(t *testing.T) {
	// anchored comment resolved by alice → POST /comments/{id}/reopen as bob → 200; GET shows resolved=false, resolved_by/resolved_at null;
	// marks projection for the mark has resolved:false; an event of type comment.reopened exists for the issue; reopening a reply → 400 INVALID_COMMENT.
}
func TestReplyToResolvedRootReopensIt(t *testing.T) {
	// resolve root → POST /comments {reply_to: root, body} → 201; GET root shows resolved=false; events: comment.created (reply) and comment.reopened (root).
}
func TestReplyMustTargetThreadRoot(t *testing.T) {
	// reply_to = an existing reply → 400 INVALID_COMMENT "reply_to must be a thread root"; no row inserted; reply_to = root still 201.
}
func TestEditCommentIsAuthorOnly(t *testing.T) {
	// PATCH by the author → 200, body replaced, edited_at set, event comment.edited, anchored root's projection text updated;
	// PATCH by another login → 403 NOT_AUTHOR, body unchanged; agent bearer editing a human comment → 403; over-cap body → 400 as createComment.
}
func TestResolveRecordsWhoAndWhen(t *testing.T) {
	// POST /comments/{id}/resolve → resolved_by == actor, resolved_at within the last minute; same for accept/reject.
}
```
  Run: `cd packages/envoy && DISPATCH_TEST_DATABASE_URL=<url> go test ./internal/dispatch/api/ -run 'TestReopenComment|TestReplyToResolved|TestReplyMustTarget|TestEditComment|TestResolveRecords' -v`. Expected: compile errors on the new fields, then 404 on the new routes.

- [ ] **Step 2: Implement** per Interfaces: migration; `model.go`; `reopenComment`/`editComment` follow `commentAction`'s shape (issue lock → row lock → `requireOpenIssue`); `createComment`'s reply branch loads the parent, rejects a non-root, reopens a resolved root in the same tx (reuse the projection path `commentAction` uses for resolve); the contract schema source gains the fields/types/events; `cd packages/contracts && bun run gen:go && bun run build`; `sse.ts` `knownEventTypes` gains both types (compile-forced) and `eventQueryKeys` maps them to `["comments", issue]`; README route table + `NOT_AUTHOR`; `skills/dispatch/SKILL.md`: "`reply_to` is the thread root; a reply to a resolved comment reopens it; comments are edited by their author in the dashboard, `dispatch_comment` does not edit".

- [ ] **Step 3: Run** — `cd packages/envoy && gofmt -l ./internal/dispatch && go vet ./internal/dispatch/... && DISPATCH_TEST_DATABASE_URL=<url> go test ./internal/dispatch/api/ ./internal/dispatch/docs/ -count=1`; `cd packages/contracts && bun run lint && bun run typecheck && bun run test && bun run build`; `cd packages/envoy-client && bun run typecheck && bun run test`; `cd packages/pi-envoy && bun run typecheck`; `cd packages/claude-envoy-bridge && bun run typecheck`; `cd packages/dispatch && bun run typecheck && bun run test web/src/__tests__/sse.test.ts`. Smoke with curl against `e2e/run-server.sh` (header identity): create → resolve → reopen → reply → PATCH as author (200) and as another user (403); paste into the PR.

- [ ] **Step 4: Commit + PR** — `jj describe -m "feat(dispatch): comment threads — reopen, author edit, root-only replies, reply reopens a resolved thread" && jj new`; bookmark `dispatch-doc/threads-server`; `jj git push`; `gh pr create --base dispatch-doc/tree-server --title "feat(dispatch): comment thread lifecycle — reopen, edit, root-only replies"`; merge queue, stacked on PR 3; re-target to `main` when PR 3 merges. Thermo pair applies (production Go).

---

### Task B1 (PR T-b): thread data model and composer modes

**Files:** `web/src/api/client.ts`, `features/margin/useMarginItems.ts`, `useMarginItems.test.ts`, `features/margin/Composer.tsx`, `Composer.test.tsx`.

**Interfaces (produces):**
```ts
// client.ts
reopenComment(id: string): Promise<Comment>                          // POST /api/v1/comments/{id}/reopen
editComment(id: string, input: EditCommentInput): Promise<Comment>   // PATCH /api/v1/comments/{id}
// useMarginItems.ts
export interface Thread { key: string; root: { kind: "ask"; ask: Ask } | { kind: "comment"; comment: Comment }; replies: Comment[]; resolved: boolean; anchor: Anchor | null; lastReplyAt: string | undefined }
export function threadRootId(comments: readonly Comment[], comment: Comment): string      // walks reply_to; a comment with ask_id (or whose chain reaches one) roots at the ask id
export function threadMarkId(thread: Thread): string | undefined
export interface MarkPlacement { pos: number; top: number }
export function useMarginItems(issueKey, tab, visibleArtifact, markPlacements: ReadonlyMap<string, MarkPlacement>): {
  threads: Thread[];            // open, anchored by placement.pos then unplaced, then unanchored newest first
  resolvedThreads: Thread[];    // same order
  needsYou: Ask[];              // spec-primary's open asks (unchanged)
  mutateItem(input: { id: string; kind: "accept" | "reject" | "resolve" | "reopen" }): void;
  editComment(id: string, body: string): Promise<Comment>;
  …existing pending/error fields…
}
// Composer.tsx props gain
inline?: boolean;                        // bottom-of-thread: single auto-growing textarea, no title row, button "Reply"
edit?: { id: string; body: string };     // PATCH mode: prefilled, button "Save", calls api.editComment
// Cmd/Ctrl+Enter submits when canSubmitComposer(); Esc unchanged
```

- [ ] **Step 1: Failing tests.**
```ts
// useMarginItems.test.ts
test("useMarginItems groups replies under their root and flattens deeper chains", ...)        // R, A(reply_to R), B(reply_to A) → one Thread, replies [A, B] chronological, lastReplyAt = B.created_at
test("useMarginItems orders anchored threads by placement, then unplaced, then unanchored newest first", ...)
test("useMarginItems separates resolved threads and counts accepted or rejected suggestions as resolved", ...)
test("an ask with human and agent replies is one thread rooted at the ask", ...)               // replies via ask_id chain
// Composer.test.tsx
test("Cmd/Ctrl+Enter submits a ready draft and does nothing on an empty one", ...)
test("inline mode renders one textarea and a Reply button and posts reply_to", ...)
test("edit mode PATCHes the comment body and reads Save", ...)
```
  Run `bun run test web/src/features/margin` → expected failures (missing exports/props).

- [ ] **Step 2: Implement** per Interfaces (decisions 4, 5, 7, 11). `mutateItem` optimistic update covers `reopen` (`resolved: false`); `editComment` invalidates `["comments", issueKey]`.

- [ ] **Step 3: Run** — `cd packages/dispatch && bun run lint && bun run typecheck && bun run test`. Note: `CommentsTab.tsx` still consumes the old flat items until Task B2 — keep a one-line adapter `flattenThreads(threads)` in `useMarginItems.ts` for this commit only and delete it in B2.

- [ ] **Step 4: Commit** — `jj describe -m "feat(dispatch-web): thread model (one level, root-keyed), reopen/edit mutations, inline and edit composer modes with Cmd/Ctrl+Enter" && jj new`

---

### Task B2 (PR T-b): thread cards

**Files:** create `features/margin/ThreadCard.tsx`, `ThreadCard.test.tsx`, `ThreadList.tsx`, `ThreadList.test.tsx`; modify `CommentsTab.tsx`, `features/inbox/AskCard.tsx`, `AskThread.tsx`; delete `CommentCard` from `CommentsTab.tsx` and the `flattenThreads` adapter.

**Interfaces (produces):**
```ts
// ThreadCard props
{ thread: Thread; expanded: boolean; hovered: boolean; viewerLogin: string; artifactSlug: string; isClosed: boolean; pendingAction: boolean; actionError: boolean;
  onToggle(): void; onAction(kind: "accept" | "reject" | "resolve" | "reopen"): void; onEdit(id: string, body: string): Promise<unknown>; onReplySaved(): void }
// DOM: <article data-margin-item={thread.key} aria-current={expanded ? "true" : undefined} aria-expanded={expanded} data-hovered={hovered ? "true" : undefined}>
//   collapsed: root (line-clamp-2) + "N replies · last reply <Timestamp>"; expanded: root + actions (Resolve|Reopen, Accept/Reject for a pending suggestion root, Edit for the author), replies flat (marginLeft 0, each with Edit for its author and "edited" when edited_at), <Composer inline kind="comment" replyTo={root id} onSaved/> unless isClosed; orphan badge "Text changed · View original" → `?version=N&comment=|&ask=`
// ThreadList props
{ threads: Thread[]; resolvedThreads: Thread[]; showResolved: boolean; onToggleResolved(): void; markPlacements; expandedThreadKey; hoveredMarkId; onExpand(key); …ThreadCard passthroughs }
// DOM: <div role="list" aria-label="Margin review items"> anchored region (position: relative; cards absolutely placed per decision 6) → <section aria-label="Discussion"> unanchored → <button>Resolved (N)</button> → <section aria-label="Resolved"> when shown
// AskCard gains mode: "inbox" | "thread-root"
```

- [ ] **Step 1: Failing tests.**
```tsx
// ThreadCard.test.tsx
test("collapsed card shows the root preview, reply count and last reply time", ...)
test("expanded card lists flat replies with no indentation and an inline reply composer that posts reply_to root", ...)   // every reply marginLeft "0px"; createComment payload { reply_to: root.id, body }
test("Resolve and Reopen call their actions; a resolved card shows Reopen", ...)
test("Accept and Reject on a pending suggestion root; the diff shows del quote and ins replacement", ...)
test("only the author sees Edit; saving shows the edited marker", ...)
test("Esc with an empty reply draft collapses the thread", ...)
test("an ask root renders question, options and the answer form with flat replies beneath", ...)
// ThreadList.test.tsx
test("anchored cards take their mark's top offset and never overlap", ...)   // placements A top 100, B top 110, A height 80 (ResizeObserver stub) → B top ≥ 188
test("Resolved (N) toggle reveals resolved threads under a Resolved heading", ...)
test("unanchored threads render in the Discussion section below anchored ones", ...)
test("the hovered mark's card carries data-hovered", ...)
```
  Run → failures (modules missing).

- [ ] **Step 2: Implement** per Interfaces and decisions 5–9, 12; composites only (`surfaceBg`, `borderDefault`, `cardHoverBorder`, `focusVisibleRing`, `textMutedOnSurface`, `successText`, `dangerText`, `linkText`); `CommentsTab.tsx` renders **Needs you** (spec-primary) then `ThreadList`; delete `CommentCard` and `flattenThreads`.

- [ ] **Step 3: Run** — `cd packages/dispatch && bun run lint && bun run typecheck && bun run test`; `rg -n "CommentCard|flattenThreads" web/src` → nothing.

- [ ] **Step 4: Commit** — `jj describe -m "feat(dispatch-web): thread cards — flat replies, inline composer, collapse summary, resolved section, author edit, ask roots, anchor-aligned list" && jj new`

---

### Task B3 (PR T-b): margin state, placements, hover, phone thread view

**Files:** `features/margin/Margin.tsx`, `Margin.test.tsx`, `MarginSheet.tsx`, `MarginSheet.test.tsx`, `useMarginListeners.ts`, `features/doc/ProofDocument.tsx`, `ProofDocument.test.tsx`, `features/doc/marks.ts`, `marks.test.ts`, `web/src/styles.css` (only if a literal is needed; expected none).

**Interfaces (produces):**
```ts
// Margin.tsx context deltas vs PR 4
expandedThreadKey: string | undefined; setExpandedThreadKey(key: string | undefined): void;   // replaces selectItem/selectedItemId
markPlacements: ReadonlyMap<string, MarkPlacement>; setMarkPlacements(p): void;               // replaces markPositions/setMarkPositions
hoveredMarkId: string | null; setHoveredMarkId(id: string | null): void;                     // from the editor
showResolved: boolean; setShowResolved(next: boolean): void;
// focusItemForMark(markId) → expands the matching thread (setExpandedThreadKey), scrolls it, compact → sheet; phone → ThreadView open
// marks.ts
export function markPlacements(doc: ProseMirrorNode, offsets: ReadonlyMap<string, number>): Map<string, MarkPlacement>   // replaces markPositions
// ProofDocument: onMarkHover: (id) => setHoveredMarkIdRef.current(id); publish = setMarkPlacements(markPlacements(view.state.doc, handle.markOffsets())) on fragment change + ResizeObserver(view.dom)
// MarginSheet (compact): summaries list → <section role="dialog" aria-label="Thread"> full height, Back button, expanded ThreadCard, composer pinned bottom
```

- [ ] **Step 1: Failing tests.**
```tsx
// marks.test.ts: "markPlacements reports first position and offset of every record-bearing mark" (offsets c1→40, a1→40 → [["c1",{pos:5,top:40}],["a1",{pos:17,top:40}]])
// ProofDocument.test.tsx: "highlight hover reaches the margin" (onMarkHover("m-9") → hoveredMarkId "m-9"; null → null); "mark placements republish on document change and resize"
// Margin.test.tsx: "focusItemForMark expands and scrolls the matching thread and opens the sheet on compact viewports" (aria-expanded true); "hovering a card and a mark cross-highlight through the bridge"
// MarginSheet.test.tsx: "the phone sheet lists thread summaries and drills into a full-height thread view with a Back button"
```
- [ ] **Step 2: Implement** per Interfaces and decisions 4, 6, 10, 12.
- [ ] **Step 3: Run** — `cd packages/dispatch && bun run lint && bun run typecheck && bun run test`; `bun run e2e -- doc.e2e.ts margin.e2e.ts` still PASS (PR 4's scenarios: `aria-current` on the expanded card, focus both ways).
- [ ] **Step 4: Commit** — `jj describe -m "feat(dispatch-web): one expanded thread, mark placements and hover from the editor, phone thread view" && jj new`

---

### Task B4 (PR T-b): e2e, docs, PR

**Files:** `e2e/editor.ts` (+ `threadCard(page, rootId)`, `replyInThread(page, rootId, body)`), create `e2e/threads.e2e.ts`, `e2e/margin.e2e.ts` (focus scenario adds `aria-expanded`, hover `data-hovered`, and the alignment bounding-box check on chromium), `packages/dispatch/AGENTS.md` (§ Margin threads), `README.md`.

- [ ] **Step 1: `threads.e2e.ts`** (seed `"The quick brown fox"`, alice + bob, iphone opens threads through the sheet's summary → `role="dialog" name="Thread"`):
  - `"a comment thread grows flat, resolves, hides, reopens"` — alice comments on "brown" from the bar (PR 4 helpers); `replyInThread(bobPage, id, "first reply")` (Ctrl+Enter) → alice's expanded card lists it `{ timeout: 1000 }`; `replyInThread(alicePage, id, "second reply")` → two replies, both `marginLeft` `0px`, chronological; `listComments` shows both with `reply_to === id`; collapse → "2 replies · last reply"; **Resolve** → card leaves the list; `Resolved (1)` → click → card under **Resolved** with **Reopen**; **Reopen** → back; resolve again and reply from the resolved card → `GET` shows `resolved: false`.
  - `"an ask is the root of its thread"` — agent ask on "fox" with options; alice's card shows question/options/answer form; bob replies; agent reply via API appears flat beneath `{ timeout: 1000 }`; alice answers → state answered, thread and replies intact; collapsed shows "N replies · last reply".
  - `"only the author edits a comment and everyone sees the marker"` — bob's card shows no **Edit**; alice **Edit** → **Save** → both cards show the new body and "edited" `{ timeout: 1000 }`.
  - `"keyboard: Cmd/Ctrl+Enter submits and Esc collapses"` — Control+Enter posts; Esc on an empty draft → `aria-expanded="false"`; Esc on a draft → discard prompt.
  - `"the phone sheet lists thread summaries and opens a full-height thread view"` (iphone) — three summaries with counts; tap → dialog height ≥ 80 % of viewport, composer bottom within 8 px of the viewport bottom, **Back** returns; no horizontal overflow.
- [ ] **Step 2: Run** — `cd packages/dispatch && bun run lint && bun run typecheck && bun run e2e` (full lane, both projects). Docs: `AGENTS.md` § Margin threads (Thread model, one expanded thread, resolved toggle, alignment rule, phone drill-down); README application shape. Read back against the running app.
- [ ] **Step 3: Commit + PR** — `jj describe -m "test(dispatch-e2e)+docs: thread scenarios (flat replies, resolve/reopen, ask roots, edit, keyboard, phone thread view)" && jj new`; bookmark `dispatch-doc/threads-ui`; `jj git push`; `gh pr create --base dispatch-doc/proof-editor --title "feat(dispatch): margin threads — one-level cards, inline composer, resolved toggle, edit, phone thread view"`; merge queue, stacked on PR 4; re-target to `main` when PR 4 merges.

---

## Acceptance pass (threads spec § Acceptance → drivers)

| Acceptance line | Surface | Driver | Task |
|---|---|---|---|
| single card, root + flat replies (0 px indent), inline bottom composer | thread card | `threads.e2e.ts` "a comment thread grows flat…"; `ThreadCard.test.tsx` | B4, B2 |
| reply anywhere → `reply_to: root.id`, no modal | Reply in any card | same scenario; `TestReplyMustTargetThreadRoot` | B4, A1 |
| active expanded + persistent composer; inactive collapsed to 2 lines | click a card | same scenario; `ThreadCard.test.tsx` collapsed test | B4, B2 |
| resolved hidden by default; `Resolved (N)`; Reopen | margin header | same scenario; `TestReopenCommentClearsResolutionAndReprojects`, `TestReplyToResolvedRootReopensIt` | B4, A1 |
| desktop cards aligned with marks; click both ways | margin ↔ editor | `margin.e2e.ts` focus scenario (bounding-box check); `ThreadList.test.tsx` alignment; PR 4's `onMarkClick`/`focusMark` | B4, B2, B3 |
| suggestion inline diff + 1-click Accept/Reject | thread card | PR 4 `margin.e2e.ts` accept/reject scenario (unchanged) + `ThreadCard.test.tsx` | — , B2 |
| phone summaries → full-height thread view with bottom composer | iPhone | `threads.e2e.ts` phone scenario | B4 |
| `Cmd/Ctrl+Enter` submits; `Esc` cancels/unfocuses | keyboard | `threads.e2e.ts` keyboard scenario; `Composer.test.tsx` | B4, B1 |
| ask is its thread's root; replies both directions; Answer/Resolve terminal; collapsed summary | ask card | `threads.e2e.ts` "an ask is the root of its thread" | B4 |
| collapsed shows root two lines + count + last reply time | thread card | `ThreadCard.test.tsx` collapsed test; grows-flat scenario | B2, B4 |
| author edits own comment; "edited"; nobody else | thread card | `threads.e2e.ts` edit scenario; `TestEditCommentIsAuthorOnly` | B4, A1 |
| server rules (`NOT_AUTHOR`, root-only reply, reply reopens, resolved_by/at) | API | `comments_test.go` five tests + curl smoke | A1 |

What drives these today: the PR 4 e2e harness (real server, two users, iphone project), Go API tests. Gaps closed: `threadCard`/`replyInThread` helpers, `threads.e2e.ts`.

## PR body checklists

**PR T-a:** columns + migration `0009` (number confirmed free); routes `POST /comments/{id}/reopen`, `PATCH /comments/{id}`; rules root-only `reply_to`, reply reopens; events; contracts + `sse.ts`; five Go tests named above; curl smoke; docs; stacked on PR 3.

**PR T-b:** every threads-spec acceptance line → scenario (table above); `ThreadCard`/`ThreadList` unit tests; `CommentCard` deleted; PR 4 scenarios still green; screenshots `threads-desktop.png` (aligned cards, one expanded), `threads-phone.png` (thread view); stacked on PR 4.

## Self-review

- **Spec coverage:** § Acceptance — each line in the Acceptance pass ✓; § Component Boundaries (Global Constraints; B3 keeps Proof to marks/hover/offsets) ✓; § File Changes (`useMarginItems` B1, `CommentsTab`→`ThreadCard`/`ThreadList` B2, `Composer` B1, `Margin`/`MarginSheet` B3) ✓; § Data Model Deltas + § Errors (A1: columns, reopen, PATCH, `NOT_AUTHOR`, root-only `reply_to`, reply reopens, events; network/resolve failure banners reuse PR 4's `QueryError`/optimistic revert) ✓; § Phone Behavior (decision 10, B3, B4) ✓; § Testing (A1/B1/B2/B4 names) ✓; § Rejected honoured (no dimmed resolved cards, no nesting, no modal reply, no delete-on-resolve, no lock).
- **Placeholders:** none.
- **Type consistency:** `Thread`/`threadRootId`/`MarkPlacement` (B1) consumed by B2/B3; `markPlacements(doc, offsets)` (B3) feeds `useMarginItems` (B1) and `ThreadList` (B2); `Comment.resolved_by/resolved_at/edited_at` (A1) rendered by `ThreadCard` (B2); `reopenComment`/`editComment` (B1) call A1's routes; `aria-expanded`/`data-hovered`/`role="dialog" aria-label="Thread"` (B2/B3) asserted by B4; PR 4's `onMarkClick`, `focusMark`, `setActiveMarks`, `composeForMark` untouched.
- **Judgment calls the executor must not "fix":** decisions 1–12; root-keyed threads; one expanded thread; resolved hidden by default; alignment rule; `0009` reserved.

## Hardening ledger

(empty — filled by the implementer/reviewer as hardening items are found and closed)
