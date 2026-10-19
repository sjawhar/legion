# Changelog

## [Unreleased]

### Fixed

- The `dispatch` command (`dist/dispatch.js`) waits for its stdout and stderr writes before it
  exits. Bun's `process.exit` dropped whatever a pipe had not yet taken, so a Bash tool lost the
  end of any output over the pipe's buffer (8 KB to 16 KB) with exit code 0.

### Added

- The MCP server lists `slack_post` beside the ten `envoy_*` tools (sjawhar/legion#1998), a reply
  in a Slack thread with the bot token the secrets broker grants (`LEGION_SLACK_BOT_TOKEN`).

### Changed

- The `envoy_unsubscribe` and `envoy_role_set` descriptions name the role-bound subscriptions
  an Oh My Pi session keeps; Claude Code holds none, so the tools behave as before.
- The `dispatch` command's `issue` and `issue-update` take `--repo <owner/name>` (`''` clears), and
  `dispatch read` prints the issue's `Repository:` line. Against a Dispatch server older than the
  repository fields the read prints `Repository: unknown (…)`, with a warning on stderr.
- `dispatch issue` names the new issue's assignee, and every `dispatch` link points at
  `DISPATCH_PUBLIC_URL` when it is set; `slack_post` refuses a reply linking a cluster-internal
  address.
- `dispatch issue` prints the new issue's dashboard link on its `Created` line.
- `envoy_sessions`' `machine` filter now matches a session's real `host` (falling back to
  `machine_id` for a session whose plugin predates the field) rather than the listener that
  registered it, so filtering by machine finds a session on its own machine again even when every
  session shares one listener.
- The `dispatch` command's approval texts, and the `dispatch` skill this plugin ships, state the
  carried approval (LEGION-573): a human's own answer to a decision block carries their approval
  to the version it writes when that answer is the whole change since the version they approved,
  so nothing is requested for it; a change outside the decision blocks makes the approval stale,
  and the next request's `--summary` names what changed since the earlier approval.
- The plugin reads one envoy.json with a `dispatch` and an `envoy` section (LEGION-707):
  `$CLAUDE_CONFIG_DIR/envoy.json`, else the file `ENVOY_CONFIG_FILE` names, else
  `~/.config/legion/envoy.json`. `~/.config/opencode/envoy.json` is not read: while it is there and
  the session's file is missing, the channel server's notice, and the `open-asks` hook's summary
  when that missing file is what turns Dispatch off, say so and name the move
  (`mkdir -p ~/.config/legion && mv ~/.config/opencode/envoy.json ~/.config/legion/`), or, for
  `$CLAUDE_CONFIG_DIR/envoy.json`, the file to copy or link it to; under a relocated
  `XDG_CONFIG_HOME` they say only that the old file is not read there. A top-level `natsUrls` is
  refused, naming `envoy.natsUrls`.
- No notice gives a `mv` across a mount: where the old file's directory is on another filesystem than the new file's nearest existing directory (an agent box's bind-mounted `~/.config/opencode`), it names the file to create and says to move the old one on the machine that owns it.
- The `open-asks` hook writes `export ENVOY_CONFIG_FILE='<path>'` to `CLAUDE_ENV_FILE` beside `DISPATCH_HOST`, and the `dispatch` command reads the file `ENVOY_CONFIG_FILE` names whenever `DISPATCH_HOST` is set, so it reads the session's own file. A Bash that lacks what that hook writes, as a subagent's can, runs `dispatch` as host `claude` (`CLAUDE_CODE_CHILD_SESSION=1` with `DISPATCH_HOST` unset) on `CLAUDE_CONFIG_DIR`'s file, where it was refused for want of `DISPATCH_HOST`.
- With Envoy off the channel server lists no tool, refuses each call with `Envoy is off for this session: <why>`, and exits 0 when stdin ends, instead of failing on `ENVOY_NATS_URL is required`.
- A `token` or `natsSeed` starting with `!` is a command whose output is the value. A checkout's
  `.envoy/envoy.json` configures Dispatch alone; its `.opencode/envoy.json` is not read, and while
  `.envoy/envoy.json` is absent the `open-asks` hook's context, the channel server's notice and the
  `dispatch` command name the rename. A checkout's file that is not valid refuses Dispatch only
  while `DISPATCH_URL` is unset, and a checkout's `dispatch.token` needs its own
  `dispatch.serverUrl` beside it.
- `DISPATCH_URL`, `ENVOY_URL` and `ENVOY_NATS_URL` take a file's token or seed only for that file's own server.
- One rule for `enabled` in both sections (envoy-client's README): `enabled: false` turns a section off; otherwise it is on when its address resolves, from the file or a variable, so an `envoy` section with `url` and `token` and no `enabled` is on with its bearer. A checkout's file is not read while `DISPATCH_URL` is set, so its token never goes to the server `DISPATCH_URL` names, and its `dispatch.enabled: true` alone turns nothing on.
- With Envoy on and no NATS server, or with its NATS seed refused, the channel server serves the Envoy tools with inbound messages off, as pi-envoy does, and says so once on stderr (`Envoy's inbound messages are off: <why>; the Envoy tools still work`, where `<why>` is `Envoy has no NATS server (envoy.natsUrls, ENVOY_NATS_URL)` or the seed's refusal); `envoy_subscribe` is refused naming why. It exited 1.
- The channel server writes one notice to stderr when a section is off: the missing file, the section's error, or what a checkout's files hold that is not read.
- `bin/envoy-send.ts` sends to the listener the envoy.json a command reads names (`envoy.url`, or `ENVOY_URL`), with its bearer (`envoy.token`, or `ENVOY_TOKEN_FILE`/`ENVOY_TOKEN`), and refuses, saying why, when Envoy is off; it sent to `ENVOY_URL` or `http://127.0.0.1:9020` with no bearer.
- `envoy-send`, a command, counts `CLAUDE_CONFIG_DIR` only under Claude Code (`CLAUDE_CODE_CHILD_SESSION=1`): run from a plain terminal whose shell exports it, it does not read `$CLAUDE_CONFIG_DIR/envoy.json`. The hook and the channel server read `CLAUDE_CONFIG_DIR` whenever it is set.

### Fixed

- `claude plugin marketplace update legion-plugins`, then `claude plugin update claude-envoy@legion-plugins`, installs main's current tree: `.claude-plugin/plugin.json` declares no `version`, so Claude Code versions the plugin by its commit. Its fixed `1.0.0` held every install at the tree it was first installed from (`claude-envoy is already at the latest version (1.0.0)`).

## [1.0.0]

### Changed

- Agents reach Dispatch through the `dispatch` command (LEGION-588): `bin/dispatch` puts it on the
  Bash tool's `PATH`, running the committed `dist/dispatch.js`, and the `open-asks` hook writes
  `export DISPATCH_HOST=claude` to `CLAUDE_ENV_FILE` once per session. The command acts as
  the session `CLAUDE_CODE_SESSION_ID` names and prints the follow notice itself, once per ask.

### Removed

- The MCP server's twenty-one `dispatch_*` tools: it lists the ten `envoy_*` tools only, and a
  `dispatch_*` call is an unknown tool. The follow announcement on the channel and the MCP image
  blocks went with them, since no MCP tool returns a Dispatch result.

## [0.6.7]

### Added

- A Dispatch tool result's `images` (`dispatch_doc_read` of a picture, the pictures `dispatch_read`
  shows) arrive as MCP image blocks after the JSON text, which leaves them out (LEGION-541). Channel
  deliveries stay text: the notification channel carries text only. Through `executeDispatchTool`
  the MCP `dispatch_read` shows a session each picture once: a later read names one it was already
  shown, and `dispatch_doc_read` shows it again, because every request carries the session's
  history and Anthropic refuses one over 32 MB. The id `/clear` replaces is forgotten, and the new
  one starts with none shown, as its conversation does. A resumed session (`--resume`) is shown
  each picture once more: the server starts with no record of it and cannot read its transcript.

## [0.6.6]

### Fixed

- `scripts/build.ts` refuses to build under a Bun other than the one `.bun-version` pins, instead
  of silently bundling with whatever `bun` resolves from PATH (LEGION-568). `bun run build` and
  `bun run check-dist` spawn a new "bun" process to run the script — package.json's script text is
  a shell command, and Bun's own `run` resolves the "bun" in it from PATH rather than reusing
  whichever binary the caller invoked `bun run` with — so a devbox whose default Bun (a version
  manager's active version) differs from the pin silently bundled with that *other Bun version*
  instead. This is a separate, compounding cause from `0.6.5`'s `minify: false` fix, not the same
  bug restated: that one was a non-determinism *within* one Bun build's own bundler, on the
  identical pinned version; this one is an entirely different Bun version running in the first
  place. Either alone could make `check-dist` fail unpredictably, and the prior investigation
  (LEGION-548, `dist/`'s round-5 note) found the first and mistakenly attributed a failure the
  second actually caused to the build machine itself — confirmed directly: the identical pinned
  Bun, same source, same lockfile, disagreed between a devbox and the CI runner, because the
  devbox's `bun run` reached a different Bun than the pin, not because the machines differ.
  `buildBundles()` now checks `Bun.version` against `.bun-version` before building, and the CLI
  entry point checks before touching the filesystem at all, so a refusal never deletes the
  committed `dist/` it was about to replace.

## [0.6.5]

### Fixed

- `scripts/build.ts` disables minification entirely (`minify: false`) rather than disabling
  `whitespace`, `identifiers` and `syntax` individually: the partial-disable form still routed
  through Bun 1.3.14's minifying code-generation path, which picked a non-deterministic CJS/ESM
  interop check on repeated builds of this package's module graph and made CI's "Check the
  committed bundle is fresh" step fail unpredictably on any change to `@legion/contracts`, this
  package's own source untouched (LEGION-548). Confirmed directly: ten rebuilds of one unmodified
  checkout, same pinned Bun, same lockfile, disagreed with each other eight times out of ten
  before the fix, and agreed every time after it.

## [0.6.4]

### Added

- The plugin ships a fourth skill, `claude-envoy:dispatch-brainstorming` (LEGION-475): in a session
  with Dispatch, a design conversation runs in its issue's spec, each question a decision block
  and nothing asked in chat, approval is requested once with nothing new in it, and the plan is the
  issue's `plan.md` document, never a repository file. It replaces superpowers' `brainstorming` and
  `writing-plans` in such a session. The `dispatch-first` skill the hook injects sends every design
  conversation and plan to it, and the `dispatch` skill's brainstorming reference is gone, its
  process now in the new skill.

## [0.6.3]

### Changed

- An approval request carries nothing new (LEGION-475). `dispatch_request_approval`'s `summary`
  says only what the human is approving, with no commentary and no question; its description, its
  refusal while a decision block is open, its two result texts and a stale approval's line say to
  request again only once the human has agreed to every point in the new version. The `dispatch`
  and `dispatch-first` skills this plugin ships say to brainstorm a design change in its issue's
  spec rather than in chat.

## [0.6.2]

### Fixed

- A malformed delivery can no longer make Claude Code accept a repeat just because one identity
  field is empty or otherwise invalid (LEGION-468, #1668). The channel read a frame's `event_id`,
  `dedupe_key`, `source` and `source_event_id` as one record, so a single invalid field discarded
  all of them and the frame passed as new. It now ignores only the invalid field and keeps a
  valid `event_id` or `dedupe_key`, so either still recognises the second copy; when neither is
  valid, the delivery remains at-least-once.

## [0.6.1]

### Fixed

- A Dispatch Retry no longer reaches a Claude Code session twice. The channel recognised a repeat
  by its `event_id`, and the listener mints a new one for every send, so a same-mode Retry of a
  message that had already reached the session arrived as a new event and was notified again, while the
  dashboard promised "Retry won't deliver it twice". The channel now recognises a repeat by its
  `dedupe_key`, which a Retry shares with the send before it, through the same
  `createDeliveryDedupe` the Oh My Pi extension uses, for the 72-hour duplicate window and only for
  a key that names its event (`dedupeKeyNamesItsEvent` in `@legion/contracts`): every Dispatch key,
  a webhook key of its delivery id, and a key the listener or the shared transport minted once for
  its message. Any other key is never dropped, so two distinct events that share one (the MCP
  bridge's, the Go daemon's outbox row id) both arrive. A second copy of one publish, which a
  session that follows overlapping topics (a pull request's whole thread and its checks) receives
  once per subscription, is still recognised by the `event_id` the copies share, whatever its key,
  so a CI settlement reaches Claude once. A send whose notification failed, or a Dispatch frame
  the channel answered with an error instead of showing it, is released, so its re-send still
  arrives; the release undoes only what that frame's own claim recorded, so a frame that merely
  carries a recorded Dispatch key cannot unclaim it. Keys live in memory, at most 250,000 of them
  with the oldest forgotten first; a restarted channel server (`claude --resume` included)
  forgets them.

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
  again; the refusal names `replace`, `move` and, for the session that asked, `dispatch_edit_ask` instead. A whole-document
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
