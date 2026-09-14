# Claude Envoy Plugin Package

Claude Code channel plugin package for Legion's Envoy subsystem.

## Overview

One MCP stdio process is the channel ingress. It subscribes NATS directly to the session's
`notifications.agent.<session_id>` subject and every topic the session follows, renders each
envelope through `@legion/envoy-client/delivery`, then writes a supported
`notifications/claude/channel` notification. A core-NATS direct delivery gets its empty receipt
when the server enqueues the notification; that means adapter-accepted, not processed by Claude.
There is no Monitor and no native Claude messaging socket path.

The session advertises only the `aside` capability. `btw` remains intentionally unsupported until
there is a server-owned pending-delivery/reply-ack design. Permission relay is also intentionally
absent: without a sender-authenticated reply path and reply-ack design, declaring it would allow
an Envoy input to decide Claude Code permissions. Dispatch asks stay on Dispatch.

## Where to look

| Task | Location | Notes |
| --- | --- | --- |
| Claude plugin manifest | `.claude-plugin/plugin.json` | Declares the MCP configuration only; it must not declare a Monitor. |
| Channel configuration | `.mcp.json`, `bin/envoy-channel.ts` | Launch with `bun run --cwd ${CLAUDE_PLUGIN_ROOT} --silent channel`; stdout remains pure MCP protocol. |
| Inbound channel server | `src/envoy-channel-server.ts` | MCP capability, notification queue, NATS registration, role persistence, and shared tools. |
| NATS topic consumer | `src/channel-forwarder.ts` | Dedupe by event id and drain safely at shutdown. |
| Session identity | `src/claude-session.ts` | `CLAUDE_CODE_SESSION_ID`, or explicit `ENVOY_SESSION_ID` for QA. |
| Outbound transport | `src/envoy-client.ts` | Envoy listener HTTP client. |
| Send CLI parsing | `src/send-arguments.ts` | Validates destination and message arguments. |
| Manual channel smoke | `smoke-channel.sh` | Real dev-flag smoke; documented and deliberately excluded from CI. |

## Critical conventions

- `renderInbound` from `@legion/envoy-client/delivery` is the sole inbound renderer. Never add an
  adapter-specific renderer or emit raw envelope bytes.
- Each channel meta key must contain only letters, digits, and underscores and every value must be
  a string. `source`, `topic`, `event_id`, `dedupe_key`, `urgency`, `from_session`,
  `expects_reply`, and `in_reply_to` are the current contract.
- Subscribe NATS before registering the self-subscribed session route. On shutdown drain NATS,
  deregister the session, and let the stdio process exit.
- Register `capabilities: ["aside"]`, never `btw`. The direct NATS receipt occurs immediately after
  notification enqueue; it cannot imply model processing because Claude Code does not acknowledge
  channel notifications.
- Preserve the held role at `${CLAUDE_PLUGIN_DATA}/envoy-role.json`; soft-reclaim it with its
  `previous_session_id` after registration, then reassert it on each registration heartbeat.
- Shared tool logic remains shared: use `resolveDispatchConfig`, `executeDispatchTool`, and
  `dispatchToolSpecs`; re-read Dispatch configuration on every tool call. Successful Dispatch
  mutations subscribe their returned topic; reads do not.
- `envoy_inbox` is bounded to 50 metadata-only entries. Keep full payloads in neither the tool
  output nor plugin data.
- Development uses `--dangerously-load-development-channels`; organization deployment requires
  `channelsEnabled` and `allowedChannelPlugins`. `claude -p` has no interactive plan approval,
  multiple-choice, or consent surfaces; workers need pre-approved configuration.

## Topic reminder

- Direct Claude session: `notifications.agent.<session_id>`
