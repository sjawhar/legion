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

- The `envoy_subscribe` description says a `pr.<n>.checks` settlement is published for every
  commit of the pull request whose checks settle, the head or not, and names its `sha`
  (LEGION-208).

### Fixed

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
