# Contracts Package

Shared event contract surface for Legion/Envoy.

## Overview

This package is the language-neutral source of truth for Envoy event shapes and
the native Dispatch tool suite:

- envelope schema/type and subject helpers
- model-facing `dispatchToolSpecs` built over an injected Zod surface and
  `zodSchemaApi`
- Go output in `packages/envoy/internal/contracts/generated.go`
- Dispatch `Actor` session values may include an `owner` GitHub login when a personal agent token authenticated the request; consumers render it as `(for <owner>)` and persist it wherever they preserve actor JSON.

## Where to look

| Task | Location | Notes |
| --- | --- | --- |
| Envelope and subject contracts | `src/envelope.ts`, `src/subject.ts` | canonical TypeScript surface |
| Dispatch tool specifications | `src/dispatch-tools.ts`, `src/tool-schema.ts` | builders over `zodSchemaApi(hostZod)` |
| Go generation | `scripts/gen-go.ts` | writes the Envelope Go contract |
| Generated Go contract | `packages/envoy/internal/contracts/generated.go` | generated; do not hand-edit |
| Contract tests | `src/*.test.ts` | validation and schema drift coverage |
| Document block offsets and anchors | `src/dispatch-api.ts` | `ArtifactBlock` maps stable block IDs and canonical markdown offsets, including per-block comment/ask reference counts; `Anchor.block_id` is nullable for legacy rows. |
| Delivery capabilities | `src/dispatch-api.ts` | `DELIVERY_CAPABILITIES` (`aside`, `btw`, `steer`) is the one closed list; `MessageDeliveryMode`, the delivery event payload schema, and the envoy-client targeted-frame schema derive from it. `Agent.capabilities` stays an open `string[]` on the wire. |

## Critical conventions

- `src/tool-schema.ts` — `zodSchemaApi(z).string({ max })` emits `is N characters over the M-character limit (L/M)` (the field name is prepended by `formatZodIssues` in `@legion/envoy-client/tool-input-errors`); the OMP `pi.zod` facade ignores the message, which is why hosts register every tool with `lenientArgValidation` and let `executeDispatchTool` refuse a bad call once with every problem listed.
- `src/dispatch-tools.ts` is the source of the seventeen native Dispatch tools:
  `dispatch_issue`, `dispatch_ask`, `dispatch_edit_ask`, `dispatch_resolve_ask`, `dispatch_resolve_comment`, `dispatch_follow`,
  `dispatch_comment`, `dispatch_suggest`, `dispatch_message`, `dispatch_doc_edit`, `dispatch_doc_read`, `dispatch_request_approval`,
  `dispatch_artifact`, `dispatch_read`, `dispatch_search`, `dispatch_open_asks`, and `dispatch_whoami`. It defines their names,
  descriptions, and field shapes; `dispatch_open_asks` and `dispatch_whoami` have no model-supplied session selector (`dispatch_whoami` takes no arguments at all: a strict empty object) and
  `dispatch_message.in_reply_to` is the same-issue message-reply correlation used for a targeted agent's
  answer. Host adapters consume `dispatchToolSpecs` directly.
- `dispatch_issue` accepts optional initial labels (at most 20 labels, each at most 40 characters), an optional coarse priority from `0` (`P0`, highest) through `3` (`P3`, lowest), and an optional `assignee` (a GitHub login on the server's sign-in allowlist; the server lowercases it, and absent it defaults to the caller's owner, then the parent's assignee, then unassigned); project-document arguments accept the document's artifact id, slug, or filename. `dispatch-api.ts` carries `Issue.assignee: string | null` (also on `IssueSummary` and the inbox row's `Ask.issue`), `CreateIssueInput.assignee?: string`, `UpdateIssueInput.assignee?: string | null` (`null` clears), `DispatchUser` / `ListUsersResponse` for `GET /api/v1/users`, and `WhoamiResponse` for `GET /api/v1/whoami` (`{kind: "user", login}` or `{kind: "agent", owner}`).
- `dispatch_ask` creates a question by default and accepts only `kind: "action"` for a human to-do. Action asks have server-fixed `Done` / `Can't` options; `approval` remains server-created through `dispatch_request_approval`.
- `dispatch_comment.turn` (`"agent" | "human"`, `AskTurn`) is valid only with `reply_to_ask`; the tool-level validation rejects it otherwise. It maps to `CreateCommentInput.turn`. `Comment.turn` is that recorded turn (null under a closed ask and off ask replies), `Ask.waiting_on` is the open ask's derived state on every ask read, and `CommentEventPayload.ask_waiting_on` carries it on a `comment.created` that replies to an open ask.
- `dispatch_follow` takes `{ ask, action: "follow" | "unfollow" }` — the full ask uuid or a `dispatch://KEY/ask/<id>` reference, no owner fields — and drives `PUT`/`DELETE /api/v1/asks/{id}/followers/{session}` with the caller's own session in the `actor` body. `AskRead.followers` (`AskFollower {session_id, since}`) lists who an ask's answer and replies reach; `ask.follower_added` / `ask.follower_removed` events carry `AskFollowerEventPayload {ask_id, session_id, by}`. No tool result carries a subscription topic: write results say what the session follows (`details.follows.ask`) and name the `envoy_subscribe` line for the whole owner.
- `dispatch_read` ends every render (issue, project document, ask, comment, message) with `Referenced by:` (edges pointing at the node — mentions and structure alike) and `Links:` (edges it writes), read from `GET /api/v1/references?to=|from=` (`GraphReferences` / `GraphEdge` / `GraphNode` / `GraphExcerpt` in `dispatch-api.ts`; `GraphEdgeKind` names the seven edge types). Each row is `- <edge kind> <node kind> <dispatch:// ref> (<excerpt> · <created_at>)`; a graph the server cannot serve degrades to one `- unavailable` row like the closure section. The description keeps the phrase "artifact id, slug, or filename" that `dispatch-tools.test.ts` pins.
- `dispatch_resolve_comment` takes `{ comment }` — a comment uuid, or a `dispatch://KEY/comment/<id>` / `dispatch://PROJECT/artifact/<slug>/comment/<id>` reference whose id may be an 8+ character prefix unique on its owner — and drives `POST /api/v1/comments/{id}/resolve` with the caller's session as the `actor` body (the only field the server reads; there is no reason). The server lets any authenticated actor resolve any comment on an open owner; `/reopen` is `authHuman`, so a session cannot undo a resolution.
- Build field shapes through `zodSchemaApi(hostZod)` so option bags apply to the
  host's Zod. Use `dispatchToolSchema(spec, zodSchemaApi(hostZod))` when the
  host validates a call so tool-level cross-field validation also applies.
- `dispatch_doc_edit` supports `replace`, `delete`, `insert`, `retype`, and `move`. `retype` names a
  stable block id, a server-declared type, and optional client-owned attributes; `delete` and `move`
  also address a whole block by id, and insert/move anchors accept `block:<id>` beside quotes,
  `start`, `end`, and `heading:<title>`. Ask lifecycle
  payloads include nullable `block_id`; `block.repaired` restores server-owned attributes and
  `block.invalid` records a malformed browser-authored ask block.
- Quote anchors retain their quote display cache and inline mark while carrying nullable `block_id`;
  new quotes use their lowest complete containing block, while quotes across top-level siblings
  remain unpinned. Clients must preserve the field.
- Do not hand-edit `packages/envoy/internal/contracts/generated.go`.
- If the envelope or subject shape changes, update its schema and regenerate the
  applicable Go output.
- Prefer backward-compatible additions when extending the envelope.
- Dispatch `Message` contracts preserve nullable `issue_key`, optional `target` and `in_reply_to`, plus a `deliveries` array (empty when no attempts exist). Agent-card messages target one session without an issue and their list response is `MessageRead[]`; issue-targeted messages retain their issue key. `message.delivery` and `message.answered` event payloads carry the attempt status and correlated reply that the Conversation card coalesces. A delivery mode is a capability a session advertises to the Envoy listener: add it to `DELIVERY_CAPABILITIES` before any server, adapter, or UI accepts it — never spell the list a second time.
- `AnswerAskInput.expected_edited_at` is required and carries the nullable ask revision a human reviewed. Dispatch events include project/configuration events and the two-stage subscription removal (`subscription.remove_requested` with `pending: true`, then `subscription.removed` with its `request_event_id`); clients must advance their event cursor over the pending command without treating it as a completed removal.
- `SearchResult.owner` is always present: an issue owner carries `key`, `title`, and `status`; a standalone project-document owner carries `project`, `slug`, `artifact_id`, and `name`. Clients render from `owner`; the optional `issue` block on issue-owned hits only mirrors the owner's `key`/`title`/`status` for agent clients built before the owner-only shape (#1119) and goes away once no installed pi-legion-envoy / opencode-legion-envoy / claude-envoy-bridge predates it. A message hit's `href` is `/issues/KEY/conversation`. Ask event payloads carry `options` as an array, never `null`.
- Keep examples synchronized with the real receiver output (Slack team IDs,
  GitHub owner/repo segments, and native Dispatch keys).
