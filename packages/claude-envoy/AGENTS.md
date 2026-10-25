# Claude Envoy Plugin Package

Claude Code channel plugin package for Legion's Envoy subsystem.

## Overview

One MCP stdio process is the channel ingress. It subscribes NATS directly to the session's
`notifications.agent.<session_id>` subject and every topic the session follows, renders each
envelope through `@legion/envoy-client/delivery`, then writes a supported
`notifications/claude/channel` notification. A core-NATS direct delivery gets its empty receipt
when the server enqueues the notification; that means adapter-accepted, not processed by Claude.
There is no Monitor and no native Claude messaging socket path. The server's tools are the
`envoyToolSpecs` alone, the ten `envoy_*` tools and `slack_post`: agents reach Dispatch through the
`dispatch` command, which `bin/dispatch` puts on the Bash tool's `PATH`.

A session advertises the delivery modes its deliveries' path can honour, never `btw`, and none at
all when its deliveries are channel notifications Claude Code did not load (`src/claude-channel.ts`;
a Legion pane's go to the bridge's inbox instead, so the gate does not apply to it). One whose
deliveries are channel notifications advertises `aside` alone (`CHANNEL_CAPABILITIES`): Claude Code
queues a notification for the model's next turn, so a `steer` cannot be honoured. A Legion pane
whose deliveries go to `legion claude-bridge`'s inbox advertises `aside` and `steer`
(`INBOX_CAPABILITIES` in `src/legion-inbox.ts`): the bridge starts a turn with a delivery when
Claude Code is idle and sends it as the next user message of the running turn otherwise. `btw`
remains intentionally unsupported until there is a server-owned pending-delivery/reply-ack design.
Permission relay is also intentionally absent: without a sender-authenticated reply path and
reply-ack design, declaring it would allow an Envoy input to decide Claude Code permissions.
Dispatch asks stay on Dispatch.

## Where to look

| Task | Location | Notes |
| --- | --- | --- |
| Claude plugin manifest | `.claude-plugin/plugin.json` | Declares the MCP configuration only; it must not declare a Monitor. `hooks/hooks.json` is auto-discovered. It declares no `version`, and neither does the plugin's entry in the root `.claude-plugin/marketplace.json` (tested), so Claude Code versions the plugin by its commit: `claude plugin marketplace update legion-plugins`, then `claude plugin update claude-envoy@legion-plugins`, installs main's current tree (README "Updates"); a declared `version` holds every install until someone changes it. |
| Oh My Pi plugin manifest | `.omp-plugin/plugin.json` | Empty `mcpServers`, which is how omp is told this plugin has no MCP server: the channel server exits without Claude Code's session identity, and omp gets Envoy and Dispatch from `@sjawhar/pi-envoy`. omp reads this file before `.claude-plugin/plugin.json` and a manifest `mcpServers` replaces `.mcp.json` rather than merging; omitting the key falls through to `.mcp.json` and relaunches the server. Claude Code never reads it. Its `version` equals `package.json`'s (tested); omp reads only the file's `mcpServers` and that it exists, never its `version`. |
| Skills | `skills/envoy`, `skills/dispatch`, `skills/dispatch-first`, `skills/dispatch-brainstorming` | Relative symlinks into the root `skills/`; the plugin ships only these four, the ones a standalone Claude Code session uses (the rest belong to Legion roles on Oh My Pi; README "Skills"). Claude Code copies the targets into its cache at install. |
| Committed bundle | `dist/`, `scripts/build.ts` | `bun run build` / `bun run check-dist`. The plugin cache has no `node_modules`, so the four executables (the channel server, the session hook, the title hook and the `dispatch` CLI, `BUNDLE_ENTRYPOINTS`) ship bundled, with `dist/THIRD_PARTY_NOTICES` (the inlined packages' licenses, from Bun's metafile through the root `scripts/third-party-notices.ts`); CI compares a fresh build on the pinned Bun (`.bun-version`). A mode folded into an existing bundle is paid for by that bundle's every other mode: an ESM entrypoint evaluates its whole module graph before it reads `argv`, which is why the title hook has an entrypoint of its own and why a hook on a blocking event is weighed by what its bundle imports, not by what its mode runs. |
| The `dispatch` CLI | `bin/dispatch` (the shim, mode 755), `dist/dispatch.js` (built from `../envoy-client/bin/dispatch.ts`), `hooks/session-hook.ts` | Claude Code puts the plugin's `bin/` on the Bash tool's `PATH`. The `open-asks` hook writes `export DISPATCH_HOST=claude` and `export ENVOY_CONFIG_FILE='<path>'` (the envoy.json Claude Code's rule picks, `$CLAUDE_CONFIG_DIR/envoy.json` when that is set; single-quoted, the form Claude Code's PowerShell bridge also reads) to `CLAUDE_ENV_FILE`, each unless the file already holds that line, since every `SessionStart` of a session runs it; with no workable file location (a relative `CLAUDE_CONFIG_DIR`, `ENVOY_CONFIG_FILE` or `XDG_CONFIG_HOME`) it writes no `ENVOY_CONFIG_FILE` line. The command reads the file `ENVOY_CONFIG_FILE` names whenever `DISPATCH_HOST` is set. A Bash that takes nothing from that file, as a subagent's can (anthropics/claude-code#46696), has `DISPATCH_HOST` unset and the `CLAUDE_CODE_CHILD_SESSION=1` Claude Code sets in every Bash, so its `dispatch` command takes host `claude` and reads `CLAUDE_CONFIG_DIR` itself, by the plugin's own rule. The command reads the session id from `CLAUDE_CODE_SESSION_ID`, which Claude Code sets fresh in every Bash command and moves on `/clear`. The command prints the follow notice itself, once per ask. |
| Channel configuration | `.mcp.json`, `bin/envoy-channel.ts` | Launch is `bun ${CLAUDE_PLUGIN_ROOT}/dist/envoy-channel.js`; stdout remains pure MCP protocol. The `env` block passes through `CLAUDE_PROJECT_DIR` only — an unset `${VAR}` is substituted literally, so optional inputs are inherited, never listed. |
| Hook command | `hooks/hooks.json`, `hooks/session-hook.ts`, `hooks/session-title-hook.ts` | Two bundles. `dist/session-hook.js` takes its mode as an argument — `open-asks` or `dispatch-first`, and an argument naming neither throws — and each mode is a hook of its own, because Claude Code caps each hook's output at 10,000 characters. `open-asks` (every `SessionStart`) names the `dispatch` command's host and hands on the session's envoy.json in `CLAUDE_ENV_FILE`, records the current session id per Claude process, records the session's chosen title and injects the open-asks summary; always exits 0. `dispatch-first` (every `SessionStart`, and every `SubagentStart`, since a subagent runs the same `dispatch` command) injects the `dispatch-first` skill as `hookSpecificOutput.additionalContext` under the input's `hook_event_name` when Dispatch is configured, reading `${CLAUDE_PLUGIN_ROOT}/skills/dispatch-first/SKILL.md` at run time and exiting non-zero naming it when it is missing. On a resume or fork Claude Code adds a SessionStart hook's context only when the transcript does not already hold the same text (measured on 2.1.283), so a resumed session keeps one copy and one opened without the skill gets it; the hook itself checks nothing. `dist/session-title-hook.js` (every `UserPromptSubmit`, no argument) records the payload's `session_title` — the name a person gave the session, which Claude Code hands a hook and the plugin nowhere else — under `${CLAUDE_PLUGIN_DATA}/titles/`, and removes that file when the field is absent or blank, which is how a rename is undone. It writes nothing to stdout and exits 0 whatever happens, a payload that is not JSON included (the two `session-hook` modes still fail loudly on one), because Claude Code adds a `UserPromptSubmit` hook's stdout to the model's context and reads its exit code 2 as a block of the prompt, and a cosmetic title may do neither; a failure goes to stderr. Claude Code blocks the user's prompt on it, so it is a bundle apart from the other two and keeps zod, the Dispatch client and `session-settings` out of its import graph (`src/session-state.ts` holds the file helpers for that reason, `src/session-identity.ts` imports zod and is not reachable from here): 5 kB and ~68 ms against the shared bundle's 811 kB and ~165 ms, the latter all but 3 ms of it bare `bun` start-up. It checks the payload itself — a non-empty string `session_id`, an optional string `session_title` — rather than carry a schema library. Its `timeout` is 5, not the 20 the two `SessionStart` hooks carry. |
| Inbound channel server | `src/envoy-channel-server.ts`, `src/envoy-off.ts` | MCP capability, notification queue, NATS registration, session-id handoff, role persistence, rejected-frame replies, and the Envoy tools, on the envoy.json Claude Code's rule picks (`@legion/envoy-client/session-settings`); why a section is off, and that inbound messages are off, goes to stderr once as `envoy-channel: <notice>`. `runEnvoyChannelServer` chooses between that server and the two reduced modes of `src/envoy-off.ts`. With Envoy off it runs `serveEnvoyOff` instead: an MCP server that lists no tool, refuses every call with `Envoy is off for this session: <why>` (the section's notice or error), opens no NATS connection and calls no listener, and exits 0 when stdin ends or on SIGTERM, SIGINT or SIGHUP. With Envoy on and inbound messages off (`inbound.off`: no NATS server, from `envoy.natsUrls` or `ENVOY_NATS_URL`, or its seed refused), it serves the tools on `inboundOffChannelSession`: no NATS connection, nothing registered with the listener, an empty inbox, `envoy_subscribe` refused naming why, and a role `envoy_role_set` claims still persisted. |
| Channel gate | `src/claude-channel.ts` | Whether Claude Code loaded this server as a channel, which it never tells the server: the command line of the nearest ancestor whose program is `claude` (`claudeCommandLine`; Claude Code may run the server through a shell prefix or a `bun` shim), read from `/proc` (exact, with its stdout's terminal and its directory) or `ps` (split on whitespace), against Claude Code's own gate as of 2.1.301: a `plugin:claude-envoy@…` entry when `CLAUDE_PLUGIN_ROOT` says it runs as a plugin, any `server:` entry from a bare config; `--channels` entries before development ones; `--dangerously-load-development-channels` only in an interactive session (`asksForPrintMode`, the arguments skipped as Claude Code's scanner skips them, and a terminal on stdout where `/proc` shows it); the marketplace it is loaded from (`loadedMarketplace`: `inline` for `--plugin-dir`, else the cache's, compared after Claude Code's cache-name sanitizing); for `--channels`, `allowedChannelPlugins` (object or `plugin@marketplace` entries, each invalid one dropped alone) from the managed source that owns it (`remote-settings.json`, else `managed-settings.json` with its non-dot drop-ins; under `managedSourcesBehavior: "merge"` the highest that sets it). Not loaded, the session registers `capabilities: []`. `ENVOY_CHANNEL_LOADED=1`/`0` overrides it. A command line it cannot read, or no `claude` ancestor, counts as loaded, and so does one Node overwrote with `process.title = "claude"` (on Linux, the title alone followed by NUL padding, whatever the executable's name; elsewhere one word from a program named `node…`). |
| Session identity | `src/session-identity.ts`, `src/session-state.ts`, `src/claude-session.ts` | `SessionIdentity` is the one mutable id holder; handoff file `sessions/<claude-pid>/session-id`; role file `roles/<url-encoded session id>.json` (`readPersistedRole`, `writePersistedRole`; the suffix is what keeps a session id of `.` or `..` inside `roles/`, since `encodeURIComponent` leaves a dot alone). `CLAUDE_CODE_SESSION_ID`, or explicit `ENVOY_SESSION_ID` for QA. `src/session-state.ts` holds what the state files share and nothing that imports zod: `writeAtomicStateFile`, and `pruneStateDirectory(directory, isStale)`, the one ENOENT-tolerant sweep behind all three of them — handoffs by liveness (a `sessions/<pid>` whose Claude process is gone, anything else there left alone), titles and roles by age (`writtenLongAgo`, `STATE_FILE_MAX_AGE_MS`, thirty days). The role sweep exists because exactly one path removes a role file by hand (a soft `setRole` that comes back unclaimed) and the handoff sweep cannot reach one, keyed as it is by session id with no process left to ask about. All three run at startup in `startChannelSession`, each under its own `.catch` that logs and swallows: the heartbeat timer is not armed yet, so housekeeping must never cost a session its channel. |
| Session title | `src/session-title.ts`, `src/session-state.ts` | The title every registration sends, which Dispatch's Agents page shows in place of the id: `ENVOY_SESSION_TITLE` on one line when it holds a character that shows; else the name the person running the session gave it (`claude -n`, `--name`, `/rename`), alone, with no short id after it, since a name a person chose needs nothing to tell it apart; else a name and the current id's first eight characters (a function of the id, so it follows a `/clear` handoff, and keeps two sessions in one tmux window apart). The chosen name is read, through `readChosenTitleSync`, from the `titles/<url-encoded session id>.txt` file the hooks write under `CLAUDE_PLUGIN_DATA` (keyed by id, like the role file, so `claude --resume <id>` finds it and a `/clear`'s new id starts without one; the suffix keeps a session id of `.` or `..` inside `titles/`, since `encodeURIComponent` leaves a dot alone), and every writer and reader goes through `sessionTitleFile` so the file the hook writes is the file the server reads; that read is synchronous and swallows every error, because a cosmetic title must leave the session on its fallback rather than fail the registration carrying it, and the name goes through the same `oneLine`/`visible` pair the other sources do, so a blank or invisible one falls through to the fallback while a multi-line one is put on one line and kept. The fallback's name is the pane's tmux name (`TMUX` and `TMUX_PANE`, one bounded `tmux -u -S <socket> display-message` at startup, used only when the answering server's pid is the one in `TMUX` and the pane's process is one of this process's ancestors (`isAncestor`); `session:window` when the window's `automatic-rename` is off, else the session, never a window index; for grouped sessions the group's name), else the project directory's name (outside tmux, a refused or missing answer, or a session named only by digits in a window nobody named). That name is worked out once as the server starts; the chosen name is asked for on every call instead, so a rename reaches the page on the session's next prompt plus a heartbeat, with no watcher anywhere. `pruneStaleSessionTitles` drops title files untouched for thirty days at startup, through the sweep the handoff and role prunes share and under the same best-effort guard, since nothing else deletes a file keyed by a session id. The listener replaces the stored title on every registration, and `register` also stamps it (`stampTitle`: `writeSessionTitle` in the session's Dispatch state directory) once per id, where the `dispatch` command reads it for the `session_title` on its writes; with inbound messages or Envoy off, nothing registers, so `runEnvoyChannelServer` stamps it once for the starting id (a `/clear` there leaves the new id unstamped); nothing is stamped when the session starts with Dispatch off or under `ENVOY_SESSION_ID`. |
| NATS topic consumer | `src/channel-forwarder.ts` | Dedupe through the shared `createDeliveryDedupe`: a re-send by its `dedupe_key` (never by `event_id` alone, which the listener mints fresh for every send, so a Dispatch Retry would pass as new), and a second copy of one publish arriving on overlapping subscriptions by its `event_id`; surface the envelope's topic, and drain safely at shutdown. The promise the dedupe keeps is stated on `DELIVERY_DUPLICATE_WINDOW_MS` in `packages/contracts`. |
| Outbound transport | `src/envoy-client.ts`, `bin/envoy-send.ts` | Envoy listener HTTP client for `envoy-send`, which sends to the listener and with the bearer `envoySettings` resolves over the envoy.json `envoyConfigPath("command")` chooses. |
| Send CLI parsing | `src/send-arguments.ts` | Validates destination and message arguments. |
| Manual channel smoke | `scripts/smoke-channel.sh` | Real smoke, unattended (answers every startup dialog, captures the pane); documented and deliberately excluded from CI. |
| Manual `/clear` smoke | `scripts/smoke-clear-rebind.sh` | Real `/clear` against the live listener with the build in `CLAUDE_PLUGIN_DIR` (default: this package); checks the registry entry moves to the new id without the old direct subject. Sends no prompt to the model; excluded from CI. |
| Manual session-title smoke | `scripts/smoke-session-title.sh` | Real nats-server, a listener built from this tree, and the committed `dist/` for both the hook and the channel server: a chosen name registers, a rename replaces it, taking the name away restores the folder-and-short-id fallback, and the `dispatch` command's stamp carries the same name, each read back through the listener's `GET /v1/sessions`; the base revision's bundle is the negative control. Claude Code cannot run in a pod, so the two hook payloads are written to its documented shape; excluded from CI. |
| Legion phase worker on Claude Code | `src/legion-claim.ts`, `src/legion-tool.ts`, `src/legion-inbox.ts` | Active only when `legion claude-bridge` launched the pane (`LEGION_BOOT_TOKEN_FILE`, `LEGION_DAEMON_URL`, `CLAUDE_CONFIG_DIR` set). The claim waits for the session transcript, then boots through the claim boot `@sjawhar/pi-legion` shares (`bootLegionClaim`, `@legion/pi-shared/legion-claim-core`): registers with the daemon, claims the issue's Envoy role soft and calls ready, again on every `onRoleRegained`. The `legion` MCP tool is the phase ops `@sjawhar/pi-legion`'s tool serves too (`@legion/pi-shared/legion-phase-ops`: one schema, one executor, and the shared description template with its `read_record` and `request_backward_move` sentences, naming `mcp__legion__envoy_publish`). With `LEGION_CLAUDE_INBOX` set, deliveries go to the bridge's socket instead of channel notifications. `package.json` `legion.daemonApiVersion` is pinned by `tests/daemon-api-version.test.ts`. |
| Generated Legion plugin | `legion/`, `scripts/build.ts` | Never edit by hand: `scripts/build.ts` stages pi-legion's skills and converts `packages/pi-legion/agents` (tool names mapped to Claude Code's, `glob`/`grep`/`todo` dropped). `scripts/build.ts --check` and `tests/legion-plugin-fresh.test.ts` fail on drift. `tests/legion-pane-mcp.test.ts` runs a Legion pane's server end to end against stand-ins. |

## Critical conventions

- `renderInbound` from `@legion/envoy-client/delivery` is the sole inbound renderer. Never add an
  adapter-specific renderer or emit raw envelope bytes.
- Each channel meta key must contain only letters, digits, and underscores and every value must be
  a string. `producer`, `topic`, `event_id`, `dedupe_key`, `urgency`, `from_session`,
  `expects_reply`, and `in_reply_to` are the current contract. Never emit a `source` key: Claude
  Code stamps its own `source="plugin:claude-envoy:envoy"` attribute on the `<channel>` tag.
- Subscribe NATS before registering the self-subscribed session route — on startup, on a session-id
  handoff (new direct subject first, then deregister the old id, drop the old subject, register,
  move the role by soft claim; a handoff that fails before the id switch drops the new subject
  again), and when rebuilding the interests a resumed id already registered.
  A handoff is adopted within 250 ms of the SessionStart hook writing the handoff file: the server
  polls the file by path (`fs.watchFile`), because a directory watch reports only the hook's
  temporary file, not the rename onto `session-id`. The heartbeat is the fallback. On shutdown
  (stdin end, SIGTERM, SIGINT, SIGHUP) drain NATS, deregister the session, close the transport, and
  let the stdio process exit.
- The listener merges each registration's topics into the entry and never removes one. So every
  registry write (register, unsubscribe, deregister, the handoff) goes through the session's one
  `serialized` chain, a registration reads the id and topics when it runs, and a topic leaves the
  forwarder's topic list (`unfollow` removes it before its first await) before the listener is
  told or the next registration is sent. A human Dispatch subscription removal is repeated to the
  listener through the same chain. Nothing inside the chain may wait without a bound (the
  handoff's NATS flush has a timeout), because shutdown's deregistration queues behind it.
- Publish the empty receipt only for a forwarded lane (envelope topic differs from the direct
  subject: the listener's role lane, which waits for it). A plain direct send is a JetStream publish
  whose reply inbox expects the server's acknowledgement; an empty receipt there makes the listener
  report `invalid jetstream publish response` for an event that was delivered. The rule is
  `expectsLaneReceipt` from `@legion/envoy-client/delivery`, shared with the OMP extension.
- Every capture of the session id goes through the shared `SessionIdentity` (`identity.id` at the
  moment of use), never a copied string: after a `/clear` handoff the model posts to Dispatch,
  registers, unfollows, remembers roles, and deregisters under the current id.
- Pass a `ChannelInboundMessage` built field by field; a nats.js `Msg` exposes `subject`, `data`, and
  `reply` through prototype getters that an object spread silently drops. The forwarder decodes
  `data` once into `raw`; downstream code reads `raw`, never re-decodes.
- Register the capabilities the delivery path honours, never `btw`: `CHANNEL_CAPABILITIES`
  (`["aside"]`) for channel notifications, which queue for the next turn, so a steer at the next
  tool boundary cannot be honoured, and none when Claude Code did not load the channel;
  `INBOX_CAPABILITIES` (`["aside", "steer"]`) when `LEGION_CLAUDE_INBOX` routes deliveries to the
  bridge, which needs no channel. Dispatch refuses a targeted delivery in a mode the session does
  not advertise, and a message with no command goes in the mode the addressed sessions take
  (`plainDelivery`), as a broadcast does (`broadcastDelivery`).
  The direct NATS receipt occurs immediately after notification enqueue; it cannot imply model
  processing because Claude Code does not acknowledge channel notifications.
- Preserve the held role at `${CLAUDE_PLUGIN_DATA}/roles/<session-id>.json` (keyed by session id so
  `claude --resume <id>` finds it); soft-reclaim it with its `previous_session_id` after
  registration, then reassert it on each registration heartbeat. The handoff directory
  `sessions/<claude-pid>/` is pruned at startup when its pid is gone — never deleted on shutdown,
  because Claude Code restarts the server inside the same `claude` process — and the two
  id-keyed directories, `roles/` and `titles/`, are pruned there too at thirty days, since a
  session that ends while it still holds its role deletes neither file itself.
- Shared logic remains shared: use `sessionDispatchSettings`, `formatOpenAsksSummary`,
  `postDeliveryReply`, `mergeInterestSources`, and `sendConfirmationText`. No Dispatch call goes
  through the MCP server; the `dispatch` command runs the shared executor, and a `dispatch_*` tool
  name is an unknown tool here. Rejected targeted frames answer Dispatch with
  `Invalid Dispatch targeted delivery frame`; malformed ones without a reply address are logged and
  dropped — never shown to the model.
- `envoy_inbox` is bounded to 50 metadata-only entries. Keep full payloads in neither the tool
  output nor plugin data.
- Deployment is `--channels plugin:claude-envoy@legion-plugins` under managed settings
  (`channelsEnabled`, `allowedChannelPlugins`); `--dangerously-load-development-channels` is for
  entries outside the allowlist and cannot be combined with any `--channels` entry naming the same
  plugin: Claude Code matches the `--channels` entry first, which then needs the allowlist and its
  own marketplace. `claude -p` has no interactive plan approval, multiple-choice,
  or consent surfaces; workers need pre-approved configuration.
- Rebuild `dist/` (`bun run build`, on the Bun in `.bun-version`) in the same commit as any source
  change, including a change to `@legion/contracts` or `@legion/envoy-client`; `check-dist` in CI
  compares bytes.

## Topic reminder

- Direct Claude session: `notifications.agent.<session_id>`
