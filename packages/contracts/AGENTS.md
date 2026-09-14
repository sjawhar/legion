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

## Critical conventions

- `src/dispatch-tools.ts` is the source of the fourteen native Dispatch tools:
  `dispatch_issue`, `dispatch_ask`, `dispatch_edit_ask`, `dispatch_resolve_ask`, `dispatch_comment`,
  `dispatch_suggest`, `dispatch_message`, `dispatch_doc_edit`, `dispatch_doc_read`, `dispatch_request_approval`,
  `dispatch_artifact`, `dispatch_read`, `dispatch_search`, and `dispatch_open_asks`. It defines their names,
  descriptions, and field shapes; `dispatch_open_asks` has no model-supplied session selector and
  `dispatch_message.in_reply_to` is the same-issue message-reply correlation used for a targeted agent's
  answer. Host adapters consume `dispatchToolSpecs` directly.
- `dispatch_issue` accepts optional initial labels (at most 20 labels, each at most 40 characters) and an optional coarse priority from `0` (`P0`, highest) through `3` (`P3`, lowest); project-document arguments accept the document's artifact id, slug, or filename.
- `dispatch_ask` creates a question by default and accepts only `kind: "action"` for a human to-do. Action asks have server-fixed `Done` / `Can't` options; `approval` remains server-created through `dispatch_request_approval`.
- Build field shapes through `zodSchemaApi(hostZod)` so option bags apply to the
  host's Zod. Use `dispatchToolSchema(spec, zodSchemaApi(hostZod))` when the
  host validates a call so tool-level cross-field validation also applies.
- `dispatch_doc_edit` supports `replace`, `delete`, `insert`, and `retype`. `retype` names a
  stable block id, a server-declared type, and optional client-owned attributes. Ask lifecycle
  payloads include nullable `block_id`; `block.repaired` restores server-owned attributes and
  `block.invalid` records a malformed browser-authored ask block.
- Quote anchors retain their quote display cache and inline mark while carrying nullable `block_id`;
  new quotes use their lowest complete containing block, while quotes across top-level siblings
  remain unpinned. Clients must preserve the field.
- Do not hand-edit `packages/envoy/internal/contracts/generated.go`.
- If the envelope or subject shape changes, update its schema and regenerate the
  applicable Go output.
- Prefer backward-compatible additions when extending the envelope.
- Dispatch `Message` contracts preserve nullable `issue_key`, optional `target` and `in_reply_to`, plus a `deliveries` array (empty when no attempts exist). Agent-card messages target one session without an issue and their list response is `MessageRead[]`; issue-targeted messages retain their issue key. `message.delivery` and `message.answered` event payloads carry the attempt status and correlated reply that the Conversation card coalesces; add a delivery mode in the contract before any server or adapter accepts it.
- `AnswerAskInput.expected_edited_at` is required and carries the nullable ask revision a human reviewed. Dispatch events include project/configuration events and the two-stage subscription removal (`subscription.remove_requested` with `pending: true`, then `subscription.removed` with its `request_event_id`); clients must advance their event cursor over the pending command without treating it as a completed removal.
- Keep examples synchronized with the real receiver output (Slack team IDs,
  GitHub owner/repo segments, and native Dispatch keys).
