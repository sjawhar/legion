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

- `dispatch_read` of an anchored comment or ask prints `Position:` after the quote, where the
  quote's block stands: `table[3] › row 5 (Red-teamer loop), column Due` for a table cell (the
  row's index, 0 the header, labelled by its cells before the anchored one, and the column's
  header), or the path of types and child indexes outside a table (`positionText`), and
  `Position: unavailable (<code>)` when the read carries `anchor_block_error` instead. An ask's
  summary also prints its `> quote`, which it did not before (LEGION-460).
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
  (LEGION-406), and uses the page Dispatch serves, `{issues, total, limit, offset}`, as it is.
  Any other answer, a page missing one of its four fields included, is refused, naming the
  Dispatch and the request it was sent and what arrived. A JSON answer that is not a page comes
  back the same, so the refusal says a retry will not help; text in its place looks like a proxy or
  gateway page, so it says a retry may succeed. A bare array answered to the paged request, which a
  Dispatch older than #1612 sends, was paged by the client itself until every Dispatch the hosts
  reach, the production deploy included, ran #1612. It is now refused, naming a Dispatch older than
  that change or a regression of it.
- The `envoy_subscribe` description says a `pr.<n>.checks` settlement is published for every
  commit of the pull request whose checks settle, the head or not, and names its `sha`
  (LEGION-208).

### Fixed

- `dispatch_doc_edit` given `lost_ops: null` says the live document is being reloaded or holds a
  tree too deep to read, where it said only that it was being reloaded: a document past the
  schema's depth bound reaches no verdict either, and a re-read answers it `DOC_SCHEMA`
  (LEGION-465).
- A non-2xx answer whose body is not Dispatch's `{code, error}` JSON — a gateway's or proxy's HTML
  page, an empty body, JSON of another shape — reached every `dispatch_*` tool as its raw body
  (LEGION-457). `DispatchClient` now throws it as a `DispatchGatewayError` (a
  `DispatchServiceError` whose `code` stays `HTTP_<status>` and whose `fromDispatch` is false)
  reading `<METHOD> <url with query> answered <status> [<reason>] with <a body that is not
  Dispatch's error JSON ("<excerpt>") | a body of <N> bytes and no readable text [in its first 64
  KiB] | an empty body>, which looks like a proxy or gateway page rather than Dispatch's own
  answer, so <advice>.` The advice fits the method and the status: a status that cannot clear gets
  "a retry gets the same answer until the Dispatch URL, or whatever answers in its place, is
  fixed"; a 5xx, 408 or 429 gets "a retry may succeed" on a GET; on a write, a 408 or 429 (the
  gateway's own timeout or rate limit, sent before it forwards anything) gets "the write did not
  reach Dispatch, and a retry may succeed", and a 5xx, which can come after Dispatch applied the
  write, gets "the write may or may not have reached Dispatch: check whether it took effect before
  retrying it". The error carries `answer` (the message without the advice), `advice`,
  `transient` and `mayHaveReachedDispatch`. `dispatch_issue_update`'s close path reads one rule for
  whether its reason's post or its close may have taken effect: only Dispatch's own 4xx or a
  gateway's answer that never reached Dispatch proves it did not, while a 5xx (Dispatch's own
  included, since it can fail after it committed), a timeout or a transport error says the reason,
  or the close, may or may not have landed, in place of the client's advice; after an error
  message that ends a sentence (`The operation timed out.`), that account starts one of its own. A
  gateway's 408 or 429 on the close says the issue did not close and how to retry without posting
  the reason twice, with nothing to fix. A status is read as Dispatch's meaning only from
  Dispatch's own answer: a gateway's 404 page is reported as such, not as no issue linked to an
  external reference (with the advice to create one), no such project document, no comments on a
  document, or a reference section the server does not serve, and a gateway's 500 on
  `external_links` gets no link-clash hint. The excerpt and the reason phrase are each one line of
  at most 120 characters with scripts, styles and tags dropped, and with the value after
  `Authorization:` or `Bearer` and every run of 8 or more of the client's own bearer's characters
  (trimmed; a shorter bearer only whole) redacted first, so a copy cut short, split by markup or
  overlapping another leaves no such piece. The excerpt reads at most the first 64 KiB of the body,
  redacting the bearer within that slice and taking whole a piece that runs past its end, and every
  pattern it runs is linear, so a body of any size or shape cannot hold the host's event loop. The
  URL never carries the bearer, which travels in a header. Dispatch's own JSON errors render as
  before: their `error` text under their `code`.

- `createDeliveryDedupe` replaces `rememberBounded` as the one dedupe both core-NATS hosts keep: it
  recognises a re-send by its `dedupe_key`, and only for a key that names its event
  (`dedupeKeyNamesItsEvent` in `@legion/contracts`: every Dispatch key, a webhook key of its
  delivery id, a key the listener or this package's transport minted once for its message), which
  it remembers for `DELIVERY_DUPLICATE_WINDOW_MS`. Any other key is never a repeat: the latest-1,000
  memory it replaces dropped a later event that shared a key with an earlier one (the MCP bridge's
  content hash, the Go daemon's outbox row id). It also recognises a second copy of one publish,
  arriving on an overlapping subscription, by the `event_id` the copies share, whatever their key,
  holding the last 5,000 to 10,000 event ids apart from the keys. A host `claim`s a frame before
  anything it awaits, which records it and says whether it is a repeat in one step: `undefined`
  for a repeat, otherwise a `DeliveryClaim` whose `release` undoes exactly what that claim
  recorded, once, for a frame its agent was not handed (a delivery that threw, a frame it answered
  with an error), so its re-send still arrives. `DedupeIdentitySchema` is the identity a host
  parses a frame into for the claim. The record holds at most `DELIVERY_DEDUPE_KEY_LIMIT`
  (250,000) keys and forgets the oldest past it, so a producer that floods a followed topic with
  fresh keys cannot grow a host's memory without bound.
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
