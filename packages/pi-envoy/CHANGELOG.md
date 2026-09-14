# Changelog

## [Unreleased]

### Added

- The Legion extension refuses `jj undo`, `jj abandon`, and `jj op restore|revert|abandon|undo` in every phase-worker pane before they run — a `bash` command in any position of a pipeline or `&&` chain, with or without `-R`, judged on each `jj` invocation's whole argument list (so `jj --repository <path> undo` and `jj operation restore` count); `eval` code; and a `hub` process start, both by a plain-text rule (the text mentions `jj` with one of the words) — from the worker's own tool calls and from any `task` subagent it spawns, the one gate that binds a subagent (it runs in the same pane, against the same log). Every Legion issue workspace is a `jj workspace` of one clone, so those commands rewrite the operation log for every tree at once (LEGION-45: on 2026-09-12 one worker's `jj undo` rewrote nine of another tree's commits). `jj restore <paths>`, `jj op log`, and `jj op show` stay allowed; the refusal names the command, says the log is shared, and gives the recovery rule. The root architect and controller panes are unaffected.
- Added the nine native Dispatch tools and automatic subscriptions to each mutation result's issue topic.
- Reviewer, implementer, and merger role prompts (and the `legion-worker` skill) name the three thread reply forms — `Accepted: fixed in <commit> — <one line>`, `Accepted: not a defect — <reason>`, `Still open: <what remains>` — and `legion threads resolve --pr <n> --repo <owner>/<repo>`, which the implementer runs before every push that answers a review and the merger before READY to resolve the threads the reviewer accepted (the review App cannot; LEGION-34).

### Changed

- `legion.daemonApiVersion` is 5. Contract 3 added the `ENVOY_TOKEN_FILE` pane contract (LEGION-25); contract 4 adds the `spawn_worker` request ID and `workerAdmission` state response (LEGION-102); contract 5 adds the interactive controller's resumable transcript, grant form, and merge-intent authority (LEGION-16). Install this release before starting a daemon that requires contract 5.

### Fixed

- The empty receipt on the direct agent subject now goes only to a role-lane frame (envelope `topic` other than the direct subject — the shape the listener forwards to a role holder). Every other publish to that subject with a reply inbox is a JetStream publish whose inbox belongs to the server's PubAck; the receipt landing there made the publisher fail with `nats: invalid jetstream publish response` (31 Dispatch outbox `publish author route` failures in 24 h in production).
- A phase worker or root architect whose registration the daemon refuses with the same-agent 409 (`Worker respawn must resume the same agent session` at `/worker/started` or `/process/started` — a resumed process that arrived as a different session, under a database session store Oh My Pi having started fresh at a path whose row is gone) now exits like a 403 boot-token refusal does, so the daemon counts the launch failure and retires the role instead of a live pod sitting unregistered under the boot watchdog forever; a 5xx or transport failure still propagates without exiting (LEGION-81).
- Phase workers, the root architect, and the controller pane now receive their one-time grant through a 0600 file named by `LEGION_GRANT_FILE`, written by the extension before each bash command and read first by `legion credential`, `legion gh`, and `legion handoff complete` — replacing both the prepended command text the model imitated with stale ids (LEGION-12; `legion gh`, `legion handoff complete`, and `jj git push` no longer fail with 403 as a session goes on) and the 1.17.1 `env` delivery (the `secretsd` plugin's bash replacement discarded it, so every `legion` command failed with `LEGION_GRANT is missing`). The static `GH_CONFIG_DIR`, worker-bin `PATH`, and cleared `GH_TOKEN`/`GITHUB_TOKEN`/`GH_HOST` now come from the daemon's pane environment; the daemon strips any inherited `worker-bin` PATH entry at its own boundary, so a daemon started from a Legion pane never resolves its `gh` to the shim (LEGION-54).
- Envoy roles now survive `omp --resume` — including after stale-session cleanup — and follow `/fork`, `/branch`, or other transcript-carrying switches to the new session id until released.
- Legion agents now refresh their Envoy registration before claiming a role, so claims made after registration expiry are accepted.

### Removed

- Removed the NATS-based `{type:"shutdown"}` Legion control directive (`LegionControlDirective`, `requestShutdown`) — the daemon now gracefully stops every process, including the root architect, over its own `legion worker-shim` unix socket instead of publishing a control-subject directive.
