# Changelog

## [0.6.0]

### Changed

- `dispatch_request_approval` requires `summary`: the proposals in the document's latest version
  the human hasn't already agreed to, in one to three sentences (LEGION-387). The Inbox shows it
  after "Approve spec.md (version N)?", and the result text quotes the question the human sees.
  It needs a Dispatch server that accepts `summary`; an older one refuses the call.
- `dispatch_request_approval` is refused, with nothing sent, while the document holds an open
  decision block, and the refusal names each block.
- `dispatch_doc_edit` is refused, with nothing sent, when a `delete` or `retype` would take a
  decision block out of the document while its ask is open, even in a batch that inserts it
  again; the refusal says to reword it with `replace` or move it with `move`. A whole-document
  replace through `dispatch_artifact` is not refused, so it can still remove an open block.
- The `dispatch_issue` and `dispatch_doc_edit` descriptions no longer list spec headings; they
  point at the dispatch skill's "Writing a spec", which describes a spec as the design
  conversation: the problem and its evidence, each open question a decision block at the end of
  the section that discusses it, and approval requested only once those are settled.
- `dispatch_search` refuses a `query` over 1,000 characters (LEGION-386) and a `project` that is
  not a project key such as CORE before any request, naming the rule. Both ride in the search URL,
  which the load balancer in front of production Dispatch answers with a bare HTML `414` when it
  is too long, so this refusal is what stops a pasted passage from becoming that error; a
  lowercased key, which used to come back as no results, is now refused by name.

## [0.5.0]

### Added

- With Dispatch configured, every Claude Code session and subagent carries the `dispatch-first`
  skill (LEGION-386): the hook command puts it into the model's context as `additionalContext` on
  every `SessionStart` (startup, resume, clear, compact, fork) and on every `SubagentStart`, each a
  hook of its own so the open-asks summary cannot push it past Claude Code's 10,000-character hook
  limit. On a resume or fork Claude Code adds it only when the transcript does not already hold the
  same text, so a session opened on 0.4.0 gets it when it is resumed on 0.5.0, and one that started
  with it keeps one copy. The plugin ships the skill as a third symlink, `skills/dispatch-first`,
  and the hook reads it from `${CLAUDE_PLUGIN_ROOT}` at run time; an install without it fails the
  hook naming the file.

### Changed

- The two hook commands are one bundle, `dist/session-hook.js` (`hooks/session-hook.ts`), whose
  argument picks the mode: `open-asks` or `dispatch-first`. `dist/open-asks-hook.js` is gone.

## [0.4.0]

### Added

- Added the nine native Dispatch tools and automatic subscriptions to each mutation result's issue topic.

### Changed

- The bundled `envoy_subscribe` description and `envoy` skill say a `pr.<n>.checks` settlement is
  published for every commit of the pull request whose checks settle and names its `sha`
  (LEGION-208).
- Committed bundles preserve whitespace and identifier names so independent source edits merge at line level; syntax minification remains disabled because Bun 1.3.14's constant folding can truncate concatenated string literals in CI builds.

- The plugin is `claude-envoy` (package `@sjawhar/claude-envoy`, directory `packages/claude-envoy`), matching `pi-envoy`. Its channel entry is `plugin:claude-envoy@legion-plugins`, its tools are `mcp__plugin_claude-envoy_envoy__*`, its skills are `claude-envoy:<skill>`, and the managed-settings allowlist names `{ "marketplace": "legion-plugins", "plugin": "claude-envoy" }`. An install of `claude-envoy-bridge@legion-plugins` is not migrated: install `claude-envoy@legion-plugins` and uninstall the old one. Claude Code keys `CLAUDE_PLUGIN_DATA` on the plugin and marketplace names (`plugins/data/claude-envoy-legion-plugins`), so a `claude --resume` of a session started before the rename does not get its held role back.
- The plugin ships only the `envoy` and `dispatch` skills (`claude-envoy:envoy`, `claude-envoy:dispatch`). The other root skills (`legion-*`, `ce-simplify-code`, `thermonuclear-*`) are Legion's Oh My Pi lane skills and no longer load from this plugin.

### Fixed

- The bundled Dispatch tools refuse a bare document reference that is one document's slug and
  another's filename, on an issue or a project, naming both ids, where they took the slug's
  document; on a project an id also outranks another document's slug. A `dispatch://` document
  reference, or a dashboard document URL, still resolves by its slug.
- Oh My Pi no longer launches the channel server. Every omp session that had this plugin installed opened with `Failed: claude-envoy-bridge:envoy [...]: MCP subprocess closed stdout before responding.`, because the server needs Claude Code's session identity and exits without it. A new `.omp-plugin/plugin.json` declares an empty `mcpServers`, which omp reads before `.claude-plugin/plugin.json` and applies instead of `.mcp.json`; Claude Code reads only `.claude-plugin/plugin.json` and still launches the server. omp gets Envoy and Dispatch from `@sjawhar/pi-legion-envoy`. Skills are unaffected in both harnesses.
- After `/clear`, the session's registry entry no longer lists the pre-`/clear` id's direct subject. The server dropped that subject only after its first registration under the new id, and the listener merges registered topics without removing any, so the entry kept it for the life of the entry and `envoy_unsubscribe` could not remove it. Every registry write now runs one at a time and a registration reads the id and topics when it runs, so a registration that was in flight when `envoy_unsubscribe` or a human in Dispatch removed a topic, or when a handoff deregistered the old id, no longer writes it back, and a handoff that fails before the id switch no longer leaves the new id's direct subject in registrations under the old id.
- `/clear` is adopted as soon as the SessionStart hook writes the handoff file instead of at the next heartbeat (up to two minutes), during which the session answered to the old id and a send to the new id was refused.
- A handoff whose first registration under the new id fails takes the held role back on a later heartbeat instead of leaving it on the old id.

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
