# Changelog

## [Unreleased]

### Added

- Added the shared nine-tool native Dispatch client, typed results, and per-issue event subscription details.
- `setRole` takes `soft` and `previousSessionID` and returns `{ claimed: true, interest }` or `{ claimed: false, holder }`, so a caller can recover a role without displacing a live holder.
- `resolveIssueDocumentId` resolves an issue's document reference (`spec`, or its id, slug or
  filename) to the document's id as the Dispatch tools resolve an issue's `artifact` argument.
- `activeDispatchConfig` is the one "is Dispatch configured" check every host shares: the resolved
  URL and token, null when none is configured, and a `dispatch config: <reason>` throw on a broken
  configuration.

### Changed

- `dispatch_read` of an issue prints an `External links:` section: each URL a person or an agent
  linked on the issue (the pull request that delivers it among them), with its kind, as the issue
  page shows them. Before, an agent had no tool that showed a linked pull request.
- A session's claim, on `dispatch_read` and on each `dispatch_issues` row, says `· not running`
  when the live agent registry loaded and does not list the holder — the judgement the
  dashboard's claim chip makes, through `claimHolds` — and `· liveness unknown` when the registry
  could not be read. Before, the line named the holder either way, with nothing to say whether its
  session still ran.
- The `envoy_subscribe` description says a `pr.<n>.checks` settlement is published for every
  commit of the pull request whose checks settle, the head or not, and names its `sha`
  (LEGION-208).

### Fixed

- `createDeliveryDedupe` replaces `rememberBounded` as the one dedupe both core-NATS hosts keep: it
  recognises a repeat by `dedupe_key` alone, and only for a key that names its event
  (`dedupeKeyNamesItsEvent` in `@legion/contracts`: every Dispatch key, a webhook key of its
  delivery id, a key the listener or this package's transport minted once for its message), which
  it remembers for `DELIVERY_DUPLICATE_WINDOW_MS`. Any other key is never a repeat: the latest-1,000
  memory it replaces dropped a later event that shared a key with an earlier one (the MCP bridge's
  content hash, the Go daemon's outbox row id). A host `claim`s a frame before anything it
  awaits, which records the key and says whether the frame is a repeat in one step, and
  `release`s the claim of a frame its agent was not handed (a delivery that threw, a frame it
  answered with an error) so its re-send still arrives.
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
