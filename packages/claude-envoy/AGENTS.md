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

The session advertises only the `aside` capability - never `btw`, never `steer`. `btw` remains
intentionally unsupported until there is a server-owned pending-delivery/reply-ack design; `steer`
cannot be honoured because Claude Code queues channel notifications for the model's next turn. Permission relay is also intentionally
absent: without a sender-authenticated reply path and reply-ack design, declaring it would allow
an Envoy input to decide Claude Code permissions. Dispatch asks stay on Dispatch.

## Where to look

| Task | Location | Notes |
| --- | --- | --- |
| Claude plugin manifest | `.claude-plugin/plugin.json` | Declares the MCP configuration only; it must not declare a Monitor. `hooks/hooks.json` is auto-discovered. It declares no `version`, and neither does the plugin's entry in the root `.claude-plugin/marketplace.json` (tested), so Claude Code versions the plugin by its commit: `claude plugin marketplace update legion-plugins`, then `claude plugin update claude-envoy@legion-plugins`, installs main's current tree (README "Updates"); a declared `version` holds every install until someone changes it. |
| Oh My Pi plugin manifest | `.omp-plugin/plugin.json` | Empty `mcpServers`, which is how omp is told this plugin has no MCP server: the channel server exits without Claude Code's session identity, and omp gets Envoy and Dispatch from `@sjawhar/pi-envoy`. omp reads this file before `.claude-plugin/plugin.json` and a manifest `mcpServers` replaces `.mcp.json` rather than merging; omitting the key falls through to `.mcp.json` and relaunches the server. Claude Code never reads it. Its `version` equals `package.json`'s (tested); omp reads only the file's `mcpServers` and that it exists, never its `version`. |
| Skills | `skills/envoy`, `skills/dispatch`, `skills/dispatch-first`, `skills/dispatch-brainstorming` | Relative symlinks into the root `skills/`; the plugin ships only these four, the ones a standalone Claude Code session uses (the rest belong to Legion roles on Oh My Pi; README "Skills"). Claude Code copies the targets into its cache at install. |
| Committed bundle | `dist/`, `scripts/build.ts` | `bun run build` / `bun run check-dist`. The plugin cache has no `node_modules`, so the three executables (the channel server, the hook command and the `dispatch` CLI, `BUNDLE_ENTRYPOINTS`) ship bundled, with `dist/THIRD_PARTY_NOTICES` (the inlined packages' licenses, from Bun's metafile through the root `scripts/third-party-notices.ts`); CI compares a fresh build on the pinned Bun (`.bun-version`). |
| The `dispatch` CLI | `bin/dispatch` (the shim, mode 755), `dist/dispatch.js` (built from `../envoy-client/bin/dispatch.ts`), `hooks/session-hook.ts` | Claude Code puts the plugin's `bin/` on the Bash tool's `PATH`. The `open-asks` hook writes `export DISPATCH_HOST=claude` and `export ENVOY_CONFIG_FILE='<path>'` (the envoy.json Claude Code's rule picks, `$CLAUDE_CONFIG_DIR/envoy.json` when that is set; single-quoted, the form Claude Code's PowerShell bridge also reads) to `CLAUDE_ENV_FILE`, each unless the file already holds that line, since every `SessionStart` of a session runs it; with no workable file location (a relative `CLAUDE_CONFIG_DIR`, `ENVOY_CONFIG_FILE` or `XDG_CONFIG_HOME`) it writes no `ENVOY_CONFIG_FILE` line. The command reads the file `ENVOY_CONFIG_FILE` names whenever `DISPATCH_HOST` is set. A Bash that takes nothing from that file, as a subagent's can (anthropics/claude-code#46696), has `DISPATCH_HOST` unset and the `CLAUDE_CODE_CHILD_SESSION=1` Claude Code sets in every Bash, so its `dispatch` command takes host `claude` and reads `CLAUDE_CONFIG_DIR` itself, by the plugin's own rule. The command reads the session id from `CLAUDE_CODE_SESSION_ID`, which Claude Code sets fresh in every Bash command and moves on `/clear`. The command prints the follow notice itself, once per ask. |
| Channel configuration | `.mcp.json`, `bin/envoy-channel.ts` | Launch is `bun ${CLAUDE_PLUGIN_ROOT}/dist/envoy-channel.js`; stdout remains pure MCP protocol. The `env` block passes through `CLAUDE_PROJECT_DIR` only — an unset `${VAR}` is substituted literally, so optional inputs are inherited, never listed. |
| Hook command | `hooks/hooks.json`, `hooks/session-hook.ts` | One bundle, `dist/session-hook.js`, whose argument picks the mode; each mode is a hook of its own, because Claude Code caps each hook's output at 10,000 characters. `open-asks` (every `SessionStart`) names the `dispatch` command's host and hands on the session's envoy.json in `CLAUDE_ENV_FILE`, records the current session id per Claude process and injects the open-asks summary; always exits 0. `dispatch-first` (every `SessionStart`, and every `SubagentStart`, since a subagent runs the same `dispatch` command) injects the `dispatch-first` skill as `hookSpecificOutput.additionalContext` under the input's `hook_event_name` when Dispatch is configured, reading `${CLAUDE_PLUGIN_ROOT}/skills/dispatch-first/SKILL.md` at run time and exiting non-zero naming it when it is missing. On a resume or fork Claude Code adds a SessionStart hook's context only when the transcript does not already hold the same text (measured on 2.1.283), so a resumed session keeps one copy and one opened without the skill gets it; the hook itself checks nothing. |
| Inbound channel server | `src/envoy-channel-server.ts`, `src/envoy-off.ts` | MCP capability, notification queue, NATS registration, session-id handoff, role persistence, rejected-frame replies, and the Envoy tools, on the envoy.json Claude Code's rule picks (`@legion/envoy-client/session-settings`); why a section is off, and that inbound messages are off, goes to stderr once as `envoy-channel: <notice>`. `runEnvoyChannelServer` chooses between that server and the two reduced modes of `src/envoy-off.ts`. With Envoy off it runs `serveEnvoyOff` instead: an MCP server that lists no tool, refuses every call with `Envoy is off for this session: <why>` (the section's notice or error), opens no NATS connection and calls no listener, and exits 0 when stdin ends or on SIGTERM, SIGINT or SIGHUP. With Envoy on and inbound messages off (`inbound.off`: no NATS server, from `envoy.natsUrls` or `ENVOY_NATS_URL`, or its seed refused), it serves the tools on `inboundOffChannelSession`: no NATS connection, nothing registered with the listener, an empty inbox, `envoy_subscribe` refused naming why, and a role `envoy_role_set` claims still persisted. |
| Session identity | `src/session-identity.ts`, `src/claude-session.ts` | `SessionIdentity` is the one mutable id holder; handoff file `sessions/<claude-pid>/session-id`; role file `roles/<session-id>.json` (`readPersistedRole`, `writePersistedRole`). `CLAUDE_CODE_SESSION_ID`, or explicit `ENVOY_SESSION_ID` for QA. |
| NATS topic consumer | `src/channel-forwarder.ts` | Dedupe through the shared `createDeliveryDedupe`: a re-send by its `dedupe_key` (never by `event_id` alone, which the listener mints fresh for every send, so a Dispatch Retry would pass as new), and a second copy of one publish arriving on overlapping subscriptions by its `event_id`; surface the envelope's topic, and drain safely at shutdown. The promise the dedupe keeps is stated on `DELIVERY_DUPLICATE_WINDOW_MS` in `packages/contracts`. |
| Outbound transport | `src/envoy-client.ts`, `bin/envoy-send.ts` | Envoy listener HTTP client for `envoy-send`, which sends to the listener and with the bearer `envoySettings` resolves over the envoy.json `envoyConfigPath("command")` chooses. |
| Send CLI parsing | `src/send-arguments.ts` | Validates destination and message arguments. |
| Manual channel smoke | `scripts/smoke-channel.sh` | Real smoke, unattended (answers every startup dialog, captures the pane); documented and deliberately excluded from CI. |
| Manual `/clear` smoke | `scripts/smoke-clear-rebind.sh` | Real `/clear` against the live listener with the build in `CLAUDE_PLUGIN_DIR` (default: this package); checks the registry entry moves to the new id without the old direct subject. Sends no prompt to the model; excluded from CI. |

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
- Register `capabilities: ["aside"]`, never `btw` and never `steer`: Claude Code channel
  notifications queue for the next turn, so a steer at the next tool boundary cannot be honoured.
  The direct NATS receipt occurs immediately after notification enqueue; it cannot imply model
  processing because Claude Code does not acknowledge channel notifications.
- Preserve the held role at `${CLAUDE_PLUGIN_DATA}/roles/<session-id>.json` (keyed by session id so
  `claude --resume <id>` finds it); soft-reclaim it with its `previous_session_id` after
  registration, then reassert it on each registration heartbeat. The handoff directory
  `sessions/<claude-pid>/` is pruned at startup when its pid is gone — never deleted on shutdown,
  because Claude Code restarts the server inside the same `claude` process.
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
  entries outside the allowlist and cannot be combined with a `--channels` entry naming the same
  plugin from another marketplace. `claude -p` has no interactive plan approval, multiple-choice,
  or consent surfaces; workers need pre-approved configuration.
- Rebuild `dist/` (`bun run build`, on the Bun in `.bun-version`) in the same commit as any source
  change, including a change to `@legion/contracts` or `@legion/envoy-client`; `check-dist` in CI
  compares bytes.

## Topic reminder

- Direct Claude session: `notifications.agent.<session_id>`
