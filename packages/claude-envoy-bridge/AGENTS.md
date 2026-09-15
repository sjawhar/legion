# Claude Envoy Plugin Package

Claude Code channel plugin package for Legion's Envoy subsystem.

## Overview

One MCP stdio process is the channel ingress. It subscribes NATS directly to the session's
`notifications.agent.<session_id>` subject and every topic the session follows, renders each
envelope through `@legion/envoy-client/delivery`, then writes a supported
`notifications/claude/channel` notification. A core-NATS direct delivery gets its empty receipt
when the server enqueues the notification; that means adapter-accepted, not processed by Claude.
There is no Monitor and no native Claude messaging socket path.

The session advertises only the `aside` capability - never `btw`, never `steer`. `btw` remains
intentionally unsupported until there is a server-owned pending-delivery/reply-ack design; `steer`
cannot be honoured because Claude Code queues channel notifications for the model's next turn. Permission relay is also intentionally
absent: without a sender-authenticated reply path and reply-ack design, declaring it would allow
an Envoy input to decide Claude Code permissions. Dispatch asks stay on Dispatch.

## Where to look

| Task | Location | Notes |
| --- | --- | --- |
| Claude plugin manifest | `.claude-plugin/plugin.json` | Declares the MCP configuration only; it must not declare a Monitor. `hooks/hooks.json` is auto-discovered. Its `version` equals `package.json`'s (tested). |
| Committed bundle | `dist/`, `scripts/build.ts` | `bun run build` / `bun run check-dist`. The plugin cache has no `node_modules`, so both executables ship bundled; CI compares a fresh build on the pinned Bun (`.bun-version`). |
| Channel configuration | `.mcp.json`, `bin/envoy-channel.ts` | Launch is `bun ${CLAUDE_PLUGIN_ROOT}/dist/envoy-channel.js`; stdout remains pure MCP protocol. The `env` block passes through `CLAUDE_PROJECT_DIR` only — an unset `${VAR}` is substituted literally, so optional inputs are inherited, never listed. |
| SessionStart hook | `hooks/hooks.json`, `hooks/open-asks-hook.ts` | Records the current session id per Claude process and injects the open-asks summary; always exits 0. |
| Inbound channel server | `src/envoy-channel-server.ts` | MCP capability, notification queue, NATS registration, session-id handoff, role persistence, rejected-frame replies, and shared tools. |
| Session identity | `src/session-identity.ts`, `src/claude-session.ts` | `SessionIdentity` is the one mutable id holder; handoff file `sessions/<claude-pid>/session-id`; role file `roles/<session-id>.json`. `CLAUDE_CODE_SESSION_ID`, or explicit `ENVOY_SESSION_ID` for QA. |
| NATS topic consumer | `src/channel-forwarder.ts` | Dedupe by event id, surface the envelope's topic, and drain safely at shutdown. |
| Outbound transport | `src/envoy-client.ts` | Envoy listener HTTP client. |
| Send CLI parsing | `src/send-arguments.ts` | Validates destination and message arguments. |
| Manual channel smoke | `scripts/smoke-channel.sh` | Real smoke, unattended (answers every startup dialog, captures the pane); documented and deliberately excluded from CI. |

## Critical conventions

- `renderInbound` from `@legion/envoy-client/delivery` is the sole inbound renderer. Never add an
  adapter-specific renderer or emit raw envelope bytes.
- Each channel meta key must contain only letters, digits, and underscores and every value must be
  a string. `producer`, `topic`, `event_id`, `dedupe_key`, `urgency`, `from_session`,
  `expects_reply`, and `in_reply_to` are the current contract. Never emit a `source` key: Claude
  Code stamps its own `source="plugin:claude-envoy-bridge:envoy"` attribute on the `<channel>` tag.
- Subscribe NATS before registering the self-subscribed session route — on startup, on a session-id
  handoff (new direct subject first, then deregister the old id, register, drop the old subject,
  move the role by soft claim), and when rebuilding the interests a resumed id already registered.
  On shutdown (stdin end, SIGTERM, SIGINT, SIGHUP) drain NATS, deregister the session, close the
  transport, and let the stdio process exit.
- Publish the empty receipt only for a forwarded lane (envelope topic differs from the direct
  subject: the listener's role lane, which waits for it). A plain direct send is a JetStream publish
  whose reply inbox expects the server's acknowledgement; an empty receipt there makes the listener
  report `invalid jetstream publish response` for an event that was delivered.
- Every capture of the session id goes through the shared `SessionIdentity` (`identity.id` at the
  moment of use), never a copied string: after a `/clear` handoff the model posts to Dispatch,
  registers, unfollows, remembers roles, and deregisters under the current id.
- Pass a `ChannelInboundMessage` built field by field; a nats.js `Msg` exposes `subject`, `data`, and
  `reply` through prototype getters that an object spread silently drops.
- Register `capabilities: ["aside"]`, never `btw` and never `steer`: Claude Code channel
  notifications queue for the next turn, so a steer at the next tool boundary cannot be honoured.
  The direct NATS receipt occurs immediately after notification enqueue; it cannot imply model
  processing because Claude Code does not acknowledge channel notifications.
- Preserve the held role at `${CLAUDE_PLUGIN_DATA}/roles/<session-id>.json` (keyed by session id so
  `claude --resume <id>` finds it); soft-reclaim it with its `previous_session_id` after
  registration, then reassert it on each registration heartbeat. The handoff directory
  `sessions/<claude-pid>/` is pruned at startup when its pid is gone — never deleted on shutdown,
  because Claude Code restarts the server inside the same `claude` process.
- Shared tool logic remains shared: use `resolveDispatchConfig`, `executeDispatchTool`,
  `dispatchToolSpecs`, `formatOpenAsksSummary`, and `dispatchTopicLabel`; re-read Dispatch
  configuration on every tool call. Successful Dispatch mutations subscribe their returned topic and
  announce a newly followed one once; reads do not. Rejected targeted frames answer Dispatch with
  `Invalid Dispatch targeted delivery frame`; malformed ones without a message id are logged and
  dropped — never shown to the model.
- `envoy_inbox` is bounded to 50 metadata-only entries. Keep full payloads in neither the tool
  output nor plugin data.
- Deployment is `--channels plugin:claude-envoy-bridge@legion-plugins` under managed settings
  (`channelsEnabled`, `allowedChannelPlugins`); `--dangerously-load-development-channels` is for
  entries outside the allowlist and cannot be combined with a `--channels` entry naming the same
  plugin from another marketplace. `claude -p` has no interactive plan approval, multiple-choice,
  or consent surfaces; workers need pre-approved configuration.
- Rebuild `dist/` (`bun run build`, on the Bun in `.bun-version`) in the same commit as any source
  change, including a change to `@legion/contracts` or `@legion/envoy-client`; `check-dist` in CI
  compares bytes.

## Topic reminder

- Direct Claude session: `notifications.agent.<session_id>`
