# Changelog

## [Unreleased]

### Added

- Added the nine native Dispatch tools and automatic subscriptions to each mutation result's issue topic.

## [0.3.0]

### Added

- The plugin runs from its marketplace install: `.mcp.json` launches the committed bundle `dist/envoy-channel.js` (`bun run build` / `bun run check-dist`), and the `env` block passes through `CLAUDE_PROJECT_DIR` only.
- `SessionStart` hook (`hooks/hooks.json`, `dist/open-asks-hook.js`) that injects the session's open Dispatch asks and records the current session id per Claude process.
- The channel server follows the session id across `/clear` (per-process handoff file), keeps role state per session id so `claude --resume <id>` restores the role, and rebuilds the interests a resumed id already registered.
- Rejected targeted Dispatch frames answer Dispatch (`Invalid Dispatch targeted delivery frame`); malformed ones are dropped; `envoy_list` reports the live/registry union; a newly followed issue topic is announced once.
- The smoke answers every startup dialog unattended, defaults `CLAUDE_CHANNEL_FLAG` from the entry's marketplace, and captures the pane.

### Fixed

- A plain direct send no longer gets an empty receipt (it is a JetStream publish; the receipt failed the listener's publish with `invalid jetstream publish response` after delivery).
- SIGHUP from the Claude process group deregisters the session instead of killing the server silently.
- Channel meta key `source` renamed to `producer`; it duplicated Claude Code's own `source` attribute on the `<channel>` tag.
