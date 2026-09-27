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

### Fixed

- A document reference that is one document's slug and another's filename on the same issue or
  project (Dispatch suffixes a slug two documents would share, so `spec-v2` can be both) is refused
  as naming two documents, with each one's id, instead of silently taking the slug's document.
- Non-creation tools resolve external issue references to linked native issues without creating
  them, and `dispatch_read` follows ask and comment references to their targeted results.
- Suggestions without a rationale omit `body` from their request.
