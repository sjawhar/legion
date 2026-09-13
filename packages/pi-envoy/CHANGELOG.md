# Changelog

## [Unreleased]

### Added

- Added the nine native Dispatch tools and automatic subscriptions to each mutation result's issue topic.

### Fixed

- Phase workers now receive their one-time `LEGION_GRANT` through the bash tool's per-command environment instead of prepended command text, so `legion gh`, `legion handoff complete`, and `jj git push` no longer fail with 403 as a session goes on (LEGION-12).
- Envoy roles now survive `omp --resume` — including after stale-session cleanup — and follow `/fork`, `/branch`, or other transcript-carrying switches to the new session id until released.
- Legion agents now refresh their Envoy registration before claiming a role, so claims made after registration expiry are accepted.

### Removed

- Removed the NATS-based `{type:"shutdown"}` Legion control directive (`LegionControlDirective`, `requestShutdown`) — the daemon now gracefully stops every process, including the root architect, over its own `legion worker-shim` unix socket instead of publishing a control-subject directive.
