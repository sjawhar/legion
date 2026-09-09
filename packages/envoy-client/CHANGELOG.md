# Changelog

## [Unreleased]

### Added

- Added the shared nine-tool native Dispatch client, typed results, and per-issue event subscription details.

### Fixed

- External issue references now create their native issue on first use, and `dispatch_read` follows
  ask and comment references to their targeted results.
- Suggestions without a rationale omit `body` from their request.
