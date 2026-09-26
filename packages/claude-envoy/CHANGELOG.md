# Changelog

## [Unreleased]

### Added

- Added the nine native Dispatch tools and automatic subscriptions to each mutation result's issue topic.

### Changed

- The plugin is `claude-envoy` (package `@sjawhar/claude-envoy`, directory `packages/claude-envoy`), matching `pi-envoy`. Its channel entry is `plugin:claude-envoy@legion-plugins`, its tools are `mcp__plugin_claude-envoy_envoy__*`, its skills are `claude-envoy:<skill>`, and the managed-settings allowlist names `{ "marketplace": "legion-plugins", "plugin": "claude-envoy" }`. An install of `claude-envoy-bridge@legion-plugins` is not migrated: install `claude-envoy@legion-plugins` and uninstall the old one. Claude Code keys `CLAUDE_PLUGIN_DATA` on the plugin and marketplace names (`plugins/data/claude-envoy-legion-plugins`), so a `claude --resume` of a session started before the rename does not get its held role back.
- The plugin ships only the `envoy` and `dispatch` skills (`claude-envoy:envoy`, `claude-envoy:dispatch`). The other root skills (`legion-*`, `ce-simplify-code`, `thermonuclear-*`) are Legion's Oh My Pi lane skills and no longer load from this plugin.

### Fixed

- Oh My Pi no longer launches the channel server. Every omp session that had this plugin installed opened with `Failed: claude-envoy-bridge:envoy [...]: MCP subprocess closed stdout before responding.`, because the server needs Claude Code's session identity and exits without it. A new `.omp-plugin/plugin.json` declares an empty `mcpServers`, which omp reads before `.claude-plugin/plugin.json` and applies instead of `.mcp.json`; Claude Code reads only `.claude-plugin/plugin.json` and still launches the server. omp gets Envoy and Dispatch from `@sjawhar/pi-legion-envoy`. Skills are unaffected in both harnesses.

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
