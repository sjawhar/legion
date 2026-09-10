# Changelog

## [Unreleased]

### Added

- Added the nine native Dispatch tools and automatic subscriptions to each mutation result's issue topic.

### Fixed

- Envoy roles now survive `omp --resume` — including after stale-session cleanup — and follow `/fork`, `/branch`, or other transcript-carrying switches to the new session id until released.
- Legion agents now refresh their Envoy registration before claiming a role, so claims made after registration expiry are accepted.
