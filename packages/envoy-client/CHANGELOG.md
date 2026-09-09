# Changelog

## [Unreleased]

### Added

- Added the shared nine-tool native Dispatch client, typed results, and per-issue event subscription details.
- `setRole` takes `soft` and `previousSessionID` and returns `{ claimed: true, interest }` or `{ claimed: false, holder }`, so a caller can recover a role without displacing a live holder.

### Fixed

- External issue references now create their native issue on first use, and `dispatch_read` follows
  ask and comment references to their targeted results.
- Suggestions without a rationale omit `body` from their request.
