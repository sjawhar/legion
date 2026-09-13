# Changelog

## [Unreleased]

### Added

- Added the nine native Dispatch tools and automatic subscriptions to each mutation result's issue topic.
- Reviewer, implementer, and merger role prompts (and the `legion-worker` skill) name the three thread reply forms — `Accepted: fixed in <commit> — <one line>`, `Accepted: not a defect — <reason>`, `Still open: <what remains>` — and `legion threads resolve --pr <n> --repo <owner>/<repo>`, which the implementer runs before every push that answers a review and the merger before READY to resolve the threads the reviewer accepted (the review App cannot; LEGION-34).

### Changed

- `legion.daemonApiVersion` is now 2, and the daemon/plugin contract it names covers the pane contract — the environment variables the daemon sets on a pane that the extension reads or writes (`LEGION_GRANT_FILE`, `LEGION_BOOT_TOKEN_FILE`, `LEGION_CONTROLLER_SECRET_FILE`, `DISPATCH_TOKEN_FILE`, `DISPATCH_URL`, `LEGION_DAEMON_URL`, the `LEGION_*` identity variables) — as well as the daemon's HTTP request and response shapes. A daemon built from this change refuses to start against any earlier release (1.22.2 and below declare 1, or nothing before LEGION-21) with `pi-legion-envoy at <manifest> (package <version>) speaks daemon API contract 1; this daemon requires 2`, instead of starting and spawning workers that fail at their first `legion gh`/`jj git push` because the plugin never wrote the credential file (LEGION-52). Install this release into the daemon's profile before restarting the daemon on the matching code.

### Fixed

- Phase workers, the root architect, and the controller pane now receive their one-time grant through a 0600 file named by `LEGION_GRANT_FILE`, written by the extension before each bash command and read first by `legion credential`, `legion gh`, and `legion handoff complete` — replacing both the prepended command text the model imitated with stale ids (LEGION-12; `legion gh`, `legion handoff complete`, and `jj git push` no longer fail with 403 as a session goes on) and the 1.17.1 `env` delivery (the `secretsd` plugin's bash replacement discarded it, so every `legion` command failed with `LEGION_GRANT is missing`). The static `GH_CONFIG_DIR`, worker-bin `PATH`, and cleared `GH_TOKEN`/`GITHUB_TOKEN`/`GH_HOST` now come from the daemon's pane environment; the daemon strips any inherited `worker-bin` PATH entry at its own boundary, so a daemon started from a Legion pane never resolves its `gh` to the shim (LEGION-54).
- Envoy roles now survive `omp --resume` — including after stale-session cleanup — and follow `/fork`, `/branch`, or other transcript-carrying switches to the new session id until released.
- Legion agents now refresh their Envoy registration before claiming a role, so claims made after registration expiry are accepted.

### Removed

- Removed the NATS-based `{type:"shutdown"}` Legion control directive (`LegionControlDirective`, `requestShutdown`) — the daemon now gracefully stops every process, including the root architect, over its own `legion worker-shim` unix socket instead of publishing a control-subject directive.
