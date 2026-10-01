# Changelog

## [Unreleased]

### Changed

- `DispatchClient.requestApproval` takes `{ actor, summary }`, and `dispatch_request_approval`
  sends `summary` and quotes the question the server returned in its result (LEGION-387).
- `dispatch_request_approval` is refused, with no request sent, while the version it would name
  (the document's latest) holds a decision block open (LEGION-387): answering the block would
  write a new version and retract the request. A block is judged by its state in that version, so
  an answer not yet folded into a version still counts, and so does a block with no ask yet; every
  line that opens the block counts, so one quoting its opener (in code, say) can add an open block
  but never hide one. The refusal names each block and its ask, and tells the agent to ask the
  human to answer or waive it. It reads `GET /artifacts/{id}/blocks` first, then, only when an
  `ask` block is present, the owner's asks and that version's markdown; an approved document skips
  the reads.
- `dispatch_doc_edit` is refused, with nothing sent, when a `delete` or `retype` by block id would
  take a decision block out of the document while its ask is open, even in a batch that inserts
  markdown carrying its id (LEGION-387). The edit would write its version at once and settlement
  would retract the ask without another, so the question would leave the human's Inbox
  unanswered and an approval request sent next would find no open block. The refusal names
  `replace`, `move` and, for the session that asked, `dispatch_edit_ask` instead. It covers `dispatch_doc_edit` only: a
  whole-document replace through `dispatch_artifact` is sent with no reads and can still remove an
  open block. An edit with no `delete` or `retype` by block id reads nothing more; one with reads
  `GET /artifacts/{id}/blocks`, and the owner's asks only when it reaches an `ask` block.

### Added

- Added the shared nine-tool native Dispatch client, typed results, and per-issue event subscription details.
- `setRole` takes `soft` and `previousSessionID` and returns `{ claimed: true, interest }` or `{ claimed: false, holder }`, so a caller can recover a role without displacing a live holder.
- `resolveIssueDocumentId` resolves an issue's document reference (`spec`, or its id, slug or
  filename) to the document's id as the Dispatch tools resolve an issue's `artifact` argument.
- `activeDispatchConfig` is the one "is Dispatch configured" check every host shares: the resolved
  URL and token, null when none is configured, and a `dispatch config: <reason>` throw on a broken
  configuration.
- `DispatchClient.acceptMessageDelivery(id, attempt, {actor})` calls
  `POST /api/v1/messages/{id}/deliveries/{attempt}/accept` and returns the accepted attempt with
  the message's stored `body` (`AcceptedMessageDelivery`); a refusal throws a
  `DispatchServiceError` naming Dispatch's check. A rendered Dispatch message delivery carries the
  frame's `broadcast_id` as `DispatchDelivery.broadcastId` (LEGION-394).

### Changed

- `dispatch_read` of an issue prints an `External links:` section: each URL a person or an agent
  linked on the issue (the pull request that delivers it among them), with its kind, as the issue
  page shows them. Before, an agent had no tool that showed a linked pull request.
- A session's claim, on `dispatch_read` and on each `dispatch_issues` row, says `· not running`
  when the live agent registry loaded and does not list the holder — the judgement the
  dashboard's claim chip makes, through `claimHolds` — and `· liveness unknown` when the registry
  could not be read. Before, the line named the holder either way, with nothing to say whether its
  session still ran.
- `dispatch_issues` asks Dispatch for its page, sending `limit` and `offset` on
  `GET /api/v1/issues` through `DispatchClient.listIssuePage`, which replaces `listIssues`
  (LEGION-406). The hosts release this client when it merges while Dispatch deploys on its own
  schedule, so the client negotiates the answer's version rather than falling back: a Dispatch that
  pages answers `{issues, total, limit, offset}`, which is used as served; one that predates paging
  ignores both parameters and answers every matching issue as an array, which the client pages as
  it always has; any other answer, a page missing one of its four fields included, is refused. The
  tool's answer is the same either way, down to its `showing A-B of N`. The array arm goes once
  every Dispatch the hosts reach, the production deploy included, runs this change.
- The `envoy_subscribe` description says a `pr.<n>.checks` settlement is published for every
  commit of the pull request whose checks settle, the head or not, and names its `sha`
  (LEGION-208).

### Fixed

- `getArchitectureSource` reads both answers a server gives for a project with no architecture
  source as `null`: a current server's `200 null` and an older server's `404 SOURCE_NOT_FOUND`.
  A client meets both while a rollout mixes versions. Before, only `200 null` read as none, so
  against an older server every `dispatch_issue` create in a project without a source ended with
  "Could not check whether project … has an architecture model" and the advice to link components.
  Any other failure, a 404 with another code included, is still reported as a source the client
  could not check.

- A bare document reference (an `artifact` argument that is not a `dispatch://` reference's own
  document) that is one document's slug and another's filename on the same issue or project
  (Dispatch suffixes a slug two documents would share, so `spec-v2` can be both) is refused as
  naming two documents, with each one's id, instead of silently taking the slug's document. On a
  project the slug route's answer is now checked against the project's unlinked documents, so an id
  also outranks another project document's slug there, as it does on an issue. The document part of
  a `dispatch://<issue>/artifact/<slug>` or `dispatch://<PROJECT>/artifact/<slug>` reference, and
  of a dashboard document URL, is a slug and resolves by slug, never refused for a clash.
- Non-creation tools resolve external issue references to linked native issues without creating
  them, and `dispatch_read` follows ask and comment references to their targeted results.
- Suggestions without a rationale omit `body` from their request.
