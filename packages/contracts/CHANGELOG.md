# Changelog

## [Unreleased]

### Changed

- `dispatch_request_approval`'s `summary` says only what the human is approving, with no commentary
  and no question, and an approval is requested only once the human has agreed to every point in
  the document (LEGION-475).
- Added `AskApproval.requested_version`. An open approval ask now follows a document's latest
  version in the same row, and `requested_version < version` marks the interval while the agent
  is revising before handing that row back to a human (LEGION-470).
- Added the `ask.handed_back` event (`AskEventPayload`): an agent handed its approval request back
  to the human, at the version `approval.requested_version` names. `ask.edited` stays a rewording:
  a hand-back that changes no wording records only `ask.handed_back` and leaves `edited_at` as it
  was, and one with a new summary records `ask.edited` and then `ask.handed_back` (LEGION-470).
- Added `AskEditEventPayload.quiet`: `true` on a version move of an approval request that an
  earlier version already moved since it was opened or last handed back (`requested_version <
  version`), which carries `notify: false` and reaches no follower, as a human's unnamed
  `artifact.version` does; only the first move after an opening or a hand-back wakes anyone
  (LEGION-470).
- Added `ArtifactApproval.waiting_on`, whose move an `awaiting` approval's request waits on, and
  `ApprovalRequestResponse`, the answer of `POST /api/v1/artifacts/{id}/approval-requests`
  (LEGION-470).
- `dispatch_request_approval`'s description and `summary` say an approval request carries nothing
  new: the summary says only what the human is approving, a later version leaves the request
  waiting on the agent until it hands the request back once the human has agreed to every point,
  and a call while the request already waits on the human hands nothing back, refusing a different
  summary (LEGION-470).
- `dispatch_ask`'s description scopes it to a to-do or permission only a human can give, or a
  decision with no document to live in: a question about the design an issue's document records is
  a decision block in that document, at every phase and whether or not it was approved, and an ask
  never carries an Approve option. `dispatch_message`'s description sends a design decision to a
  decision block and a human to-do to `dispatch_ask` (LEGION-470).
- `dispatch_request_approval` requires `summary`, the proposals in the document's latest version
  the human hasn't already agreed to (LEGION-387). `SPEC_SECTIONS` is removed: `dispatch_issue`'s
  `spec` and `dispatch_doc_edit` point at the dispatch skill's "Writing a spec" instead of listing
  headings.
- `dispatch_request_approval`'s description says the call is refused while the document holds an
  open decision block, even when a human asked for approval (LEGION-387); its `summary`
  description no longer repeats that rule. `dispatch_doc_edit`'s description says a `delete` or
  `retype` that would take an ask block out of the document while its ask is open is refused.
- `itemFromSearch` answers `null` for an item id that decodes to a control character, as the
  Dispatch server's reference reader names nothing for one (LEGION-463). No item has such an id,
  and a NUL in one failed the write that cited it.
- `AnchorPosition.anchor_block_error` can be `DOCUMENT_UNLOADABLE`: the anchor's document has a
  stored history that cannot load, the state a rebuild from its latest saved version repairs, which
  the API answers `409 DOCUMENT_UNLOADABLE` where it answered `503 DOC_SERVICE_UNAVAILABLE`.
  `dispatch_read`'s description names it (LEGION-469).

### Added

- `ArtifactRebuildReport`, the answer of `POST /api/v1/artifacts/{id}/rebuild`: what the rebuild
  removed, the head it wrote, the validation error the history failed with, and `source_version`,
  the version the rebuilt document holds (its latest saved version, or the version supplied
  markdown wrote) (LEGION-469).
- `DOCUMENT_SCHEMA_CLOSE_CODE` (`4409`) and `DOCUMENT_SCHEMA_CLOSE_REASON` (`"DOC_SCHEMA"`), the
  close the document websocket refuses a room outside the Proof schema with, before any sync.
  `gen:go` generates them into Go as `contracts.DocumentSchemaCloseCode` and
  `contracts.DocumentSchemaCloseReason`, so the server's close and the dashboard's reading of it
  cannot drift apart (LEGION-469).
- `UserAgentStateInput.read_replies`: the ids of a session's own replies to mark read, those
  alone, for a view that shows only some of a session's replies (LEGION-485).
- `DISPATCH_TEXT_REFERENCES`: markdown bodies with the `dispatch://` references each one cites,
  read against `https://dispatch.test` or the row's `origin`. The dashboard's
  `composerReferences` and the Go reader `text.Extract`, through its JSON copy, are both tested
  against it, so neither where a reference ends nor what it names changes on one side only
  (LEGION-463).
- `BlockPath`, `BlockPathEntry` and `TablePosition`, the shape of
  `GET /api/v1/artifacts/{id}/blocks/{block_id}`: a block's path from the top-level block down
  and, in a table, the row and column indexes `delete_row` and `delete_column` take, the text of
  the header cell drawn above the cell and the row's cells. `AnchorPosition`, which `Comment` and
  `Ask` extend, carries the same for the anchor's block on the single-record reads as
  `anchor_block`, and `anchor_block_error` says why `anchor_block` is absent when the read could
  not read the anchor's document, as the code the API answers that error with elsewhere
  (`DOC_SERVICE_UNAVAILABLE`, `DOC_SCHEMA`, `INTERNAL`). `dispatch_read`'s description names the
  `Position:` line it prints (LEGION-460).
- `CreateBroadcastInput.idempotency_key` (required): names one send, so the server answers a
  repeat of it with the broadcast the first request made and refuses the key's reuse for a
  different request with `409 BROADCAST_KEY_REUSED` (LEGION-446).
- Added the plan handoff's two plan checks (LEGION-421): `gapAnalysis` (`findings`, each a
  `finding` with the plan's `answer`, or the failed call's `error`) and `planReview` (`verdict`
  `approved`, `rejected` or `failed`, `rounds`, `remainingIssues` of `{issue, evidence}`, `error`),
  with `PLAN_REVIEW_MAX_ROUNDS` (3) and `PLAN_REVIEW_VERDICTS`. `describePhaseHandoffWriteProblems`
  refuses a plan missing either, a rejection recorded before the last round or without the issues
  it named, an approval with issues standing, and a failure without its error. The record is the
  planner's own report: the write checks its shape, not that the checks ran. Reading stays
  tolerant of a plan committed before the checks, and refuses a record that is there without the
  fields its type requires (a review's `verdict` and `rounds`, each finding's `finding` and
  `answer`, each remaining issue's `issue` and `evidence`).
- `LegionGoControllerSecretResponse` carries `designGate` (`root-issues` or `off`): the Go daemon
  tells `legion controller start` its design gate policy, which the controller's take comment
  reads before it promises a design approval.
- Added `claimHolds(claim, titles)` and its `ClaimHolding` answer (`holds`, `lapsed`, `unknown`):
  whether a claim still holds its issue against the live agent registry. A person's always holds;
  a session's holds while the loaded registry lists it; with no registry it is `unknown`. The
  dashboard's claim chip and the agent tools' claim lines both judge a claim with it.
- Added shared schemas and descriptions for the nine native Dispatch tools.
- Added the `Agent` row type behind Dispatch's `GET /api/v1/agents`.
- Added required `Ask.opened_event_id`, the canonical event ID for an ask's opening turn.
- Added `MAX_BROADCAST_RECIPIENTS`, the most sessions one `POST /api/v1/broadcasts` sends to, generated into Go as `contracts.MaxBroadcastRecipients`.
- Added `SEARCH_QUERY_MAX`, the longest `dispatch_search` query (`GET /api/v1/search`'s `q`), generated into Go as `contracts.SearchQueryMax`; `dispatch_search`'s `query` now refuses a longer one before any request (LEGION-386).
- Added `SEARCH_QUERY_HINT`, the sentence a refusal over `SEARCH_QUERY_MAX` adds, generated into Go as `contracts.SearchQueryHint`.
- Added `dedupeKeyNamesItsEvent`, the one rule for whether an envelope's dedupe key names its event
  (every Dispatch key, a webhook key of its delivery id, and a key minted once for its message,
  `MINTED_DEDUPE_KEY_PATTERN`), which both core-NATS hosts' dedupe asks; the pattern is generated
  into Go as `contracts.MintedDedupeKeyPattern` for the stream's MsgId rule.
- Added `PROJECT_KEY_PATTERN`, a whole project key as the Dispatch server creates them; `dispatch_search`'s `project` must now be empty or match it (`project must be a project key such as CORE`), where any other value was sent and answered with no results.
- Added `maxHint` to `SchemaApi.string`'s options: text appended to the over-cap message, saying what to send instead.
- Added `LegionGoChildRequest`, the body of the Go daemon's `POST /legion/v1/children/park` and `/rerun` (an architect's `park_child` and `rerun_child`), whose answers are `LegionGoEmptyResponse`.
- Added optional `legionAppLogins` to `LegionGoGitHubTokenResponse` and to `LegionDaemonApi.GitHubToken`'s response: each Legion role App's login keyed by its App role (`{implement, review}`), on `/legion/v1/gh-token`, which `legion threads resolve` keeps out of its bot-thread rule and whose `review` login's `Accepted:` closes a bot's thread (LEGION-208).
- Added optional `Message.broadcast_id`, the broadcast a message is one recipient's copy of: null
  for every other message, absent from a Dispatch older than the field. Added optional
  `AgentStreamMessage.dispatchMessageId`, the Dispatch message a streamed user message delivered,
  set when the session matched the user message to a person's direct message it sent in as its
  user's own turn (LEGION-394). Added optional
  `MessageDelivery.requested_by`, `accepted_as` and `accepted_at`, `AcceptedMessageDelivery` (the
  accept route's answer: the attempt with the message's stored `body`), the `message.accepted`
  event (`MessageAcceptedEventPayload`), and `broadcast_id` on
  `DispatchTargetedMessagePayloadSchema` (LEGION-394).
- Added `IssueSummaryPage` (`{issues, total, limit, offset}`), the answer of
  `GET /api/v1/issues?limit=&offset=`, and `MAX_ISSUE_PAGE_LIMIT` (250) and
  `DEFAULT_ISSUE_PAGE_LIMIT` (50), generated into Go as `contracts.MaxIssuePageLimit` and
  `contracts.DefaultIssuePageLimit`, so the server's bounds and the `dispatch_issues` tool's `limit`
  are one pair of numbers (LEGION-406).

### Changed

- The `dispatch_issues` description says Dispatch pages the listing and the answer names how many
  issues match, where it said the rows were paged after the server returned the full response.

### Removed

- Removed `HandoffMessage`, `validateHandoffMessage`, and `MESSAGES_DIR_NAME`: the `legion handoff message|messages` commands they served are gone, and nothing else read `.legion/messages/`.
- Removed `legionNoticeSubject`: the Go daemon publishes no notice on an issue's topic any more, and its one caller, `legionControllerNoticeSubject`, now builds the controller topic itself.
- Removed the handoff schema: `validatePhaseHandoff`, `describePhaseHandoffProblems`,
  `describePhaseHandoffWriteProblems`, `isHandoffPhase`, the phase handoff interfaces,
  `PHASE_FILE_NAMES`, `LEGION_DIR_NAME`, `HANDOFF_SCHEMA_VERSION`, `PLAN_REVIEW_MAX_ROUNDS` and
  `PLAN_REVIEW_VERDICTS`. The Go `legion handoff write` holds each phase's handoff to the same
  rules and names every field at fault; `HANDOFF_PHASES`, the `legion` tool's phase words, stays.
