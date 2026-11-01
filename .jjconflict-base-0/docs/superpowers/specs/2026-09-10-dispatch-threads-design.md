# Dispatch Margin Threaded Discussion Design

## Decisions needed

None. The two candidate questions (one-level threads; resolved threads hidden by default) converge across Google Docs, GitHub, Notion, Slack, and Linear; both are decided in Design and the alternatives sit in Rejected. Sami 2026-09-10: "Study best practices and most well-loved UX's out there and do what they do."

## Acceptance

- Every comment thread renders as a single card with root item, chronological flat replies (0px incremental indent), and an inline bottom composer.
- Replying to any comment/reply attaches `reply_to: root.id` and appends to the active thread without modal dialogs.
- Active/selected thread expands with replies and persistent composer; inactive threads collapse to a 2-line preview.
- Resolved threads hide by default; header toggle "Resolved (N)" reveals them with a "Reopen" action.
- Desktop margin cards vertically align with document anchor marks; clicking mark focuses card, clicking card highlights mark.
- Suggestion comments display inline diff (`<del>`/`<ins>`) and 1-click "Accept" (applies doc edit and resolves thread) / "Reject".
- Phone bottom sheet displays thread summaries; tapping navigates to a dedicated full-height thread view with bottom-pinned composer.
- Keyboard shortcuts: `Cmd/Ctrl+Enter` submits comment/reply; `Esc` cancels draft or unfocuses active thread.
- An ask is the root of its thread: the card shows question, options, and answer form; replies (human or agent) are flat beneath it; Answer and Resolve stay the ask's terminal actions; a collapsed ask thread shows "N replies · last reply <time>".
- A collapsed thread shows the root's first two lines, the reply count, and the last reply time (Slack/Linear).
- The author of a comment can edit it within the thread; the card shows "edited"; nobody else can.

## Requirements w/ provenance

| Requirement | Provenance (Product & Observed Behavior) |
| --- | --- |
| 1-level flat threading | Google Docs / GitHub PR / Slack — root comment + chronological flat replies prevents margin horizontal staircasing. |
| Inline bottom reply composer | Google Docs / GitHub PR / Linear — composer permanently visible at bottom of expanded thread; eliminates detached modals. |
| Card-level expand/collapse | Google Docs / Figma — active thread expands full conversation; inactive threads collapse to compact previews. |
| Resolved thread filter & reopen | Google Docs / Notion / GitHub — resolved threads hidden by default behind toggle; reopen restores thread to open state. |
| Spatial anchor alignment | Google Docs / Notion — card top aligns vertically with highlighted document text mark. |
| Suggestion diff & 1-click apply | GitHub PR reviews — replacement rendered as sidecar diff; Accept applies doc patch and marks thread resolved. |
| Keyboard navigation (Cmd+Enter, Esc) | Linear / GitHub / Slack — Cmd+Enter submits comment/reply; Esc closes composer or unfocuses active thread. |
| Unanchored discussion placement | Notion / Google Docs — general issue/doc comments placed in dedicated section below anchored threads. |
| Phone full-height thread view | Google Docs / Notion Mobile — mobile sheet lists threads; tap opens dedicated full-height screen with bottom input. |
| Collapsed thread summary "N replies · last reply" | Slack / Linear — collapsed threads show reply count and last-reply time instead of the whole conversation. |
| Edit own comment, "edited" marker | Google Docs / GitHub / Linear — authors edit their own comments in place; others see an edited marker. |
| Reply to a resolved thread reopens it | Google Docs — a reply on a resolved comment re-opens it. |
| Asks are threads with replies in both directions | Dispatch #848 (Sami: answering stays first-class) — the ask card is the thread root; Answer/Resolve are its terminal actions. |

## Design

### Component Boundaries (Dispatch Margin vs Proof PR 4)

| Layer | Responsibilities |
| --- | --- |
| Proof (`plugins/{comments,suggestions}.ts`) | ProseMirror mark decorations, quote/range selector resolution, mark click/hover events, anchor position offset reporting. |
| Dispatch Margin (`features/margin/`) | Thread data queries, 1-level grouping, card rendering, inline composer, resolve/reopen mutations, ask cards, phone drawer. |

### File Changes

| File | Changes |
| --- | --- |
| `useMarginItems.ts` | Group comments into `Thread` objects `(root, replies[])` with `reply_to = root.id`; sort anchored by doc offset, unanchored at bottom; filter resolved items. |
| `CommentsTab.tsx` | Replace recursive `CommentCard` indentation with `ThreadCard` (root + flat replies + bottom composer); add "Resolved (N)" toggle; add "Reopen" button. |
| `Composer.tsx` | Add inline mode for thread bottom; bind `Cmd+Enter` submit and `Esc` cancel; support auto-growing textarea. |
| `Margin.tsx` / `MarginSheet.tsx` | Align active thread to Proof anchor top offset; render mobile full-height thread drill-down. |

### Data Model Deltas

- `Comment` additions (`packages/contracts/src/dispatch-api.ts`, Postgres `comments` table): `resolved_by?: Actor`, `resolved_at?: string`, `edited_at?: string`; new `PATCH /api/v1/comments/{id}` (author only, body) and `POST /api/v1/comments/{id}/reopen`; `POST /comments` with `reply_to` on a resolved root sets `resolved = false` on the root.
- `reply_to` is always the thread root: the SPA sends `reply_to: root.id`; the server rejects a `reply_to` that is itself a reply with 400 `INVALID_COMMENT`; existing deeper chains render flattened under their root (`useMarginItems` walks `reply_to` to the root).
- State cost: 3 nullable additive columns; one migration (number re-checked against `main@origin` at rebase); `POST /comments` payload unchanged.

### Phone Behavior

- `MarginSheet.tsx` lists thread preview cards with reply counts; tapping a card transitions sheet to a full-height thread view with bottom-pinned composer and back button.

## Errors

| Condition | Handling |
| --- | --- |
| `POST /comments` network failure | Render inline retry banner on thread composer; draft text preserved in local component state. |
| `POST /comments/{id}/resolve` failure | Revert optimistic resolution; show inline retry banner on thread card. |
| Orphaned anchor (text edited) | Display "Text changed · View original" warning badge linking to versioned doc diff. |
| Reply to a resolved thread | Server accepts the reply, sets the root `resolved = false`, broadcasts `comment.created` and `comment.reopened`. |
| `PATCH /comments/{id}` by a non-author | 403 `NOT_AUTHOR`; the edit control is not rendered for non-authors. |
| `reply_to` pointing at a reply | 400 `INVALID_COMMENT` "reply_to must be a thread root". |

## Testing

- `useMarginItems.test.ts`: Verify 1-level thread grouping where replies to replies map to root; verify document-order sorting for anchored and timestamp sorting for unanchored; verify resolved filter toggle.
- `ThreadCard.test.tsx`: Verify rendering of root + replies without indentation; verify inline composer submission with `Cmd+Enter`; verify Resolve/Reopen actions.
- `Margin.test.tsx`: Verify active card alignment to anchor; verify mobile drill-down navigation in sheet.
- E2E smoke test: Select text in document → create comment → reply twice → verify flat thread with 2 replies → resolve → verify hidden → toggle "Show resolved" → click Reopen → thread restores.

## Rejected

| Alternative | Reason rejected |
| --- | --- |
| Resolved threads shown inline as dimmed cards | Google Docs, Notion, and GitHub hide resolved threads behind a toggle; inline dimmed cards clutter a 320px margin. |
| Arbitrary N-level recursive comment nesting (Reddit/HN style) | Horizontal indentation collapses text width in 320px margin; breaks readability; rejected across all modern doc tools (Docs, Notion, GitHub). |
| Reply button opening detached modal or separate top composer | Increases cognitive friction and loses visual context of thread history; inline bottom composer is standard. |
| Hard-deleting comments on resolve | Destroys decision audit trail; resolve must toggle state and remain retrievable via history. |
| Global document lock during comment editing | Blocks collaborative review; Proof decorations handle concurrent document remapping. |
