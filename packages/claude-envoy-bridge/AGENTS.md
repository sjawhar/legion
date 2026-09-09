# Claude Envoy Plugin Package

Claude Code plugin package for Legion's Envoy subsystem.

## Overview

The adapter uses an always-on Claude Code Monitor process as a Tier 1 inbound bus. The monitor
subscribes directly to Envoy NATS topics, renders every envelope through
`@legion/envoy-client/delivery`, and writes one normalized TOON Monitor event to stdout. Claude
Code surfaces those events to the session, including when the session is idle. The companion send
CLI uses Envoy's local Go listener HTTP API for outbound direct messages.

## Where to look

| Task | Location | Notes |
| --- | --- | --- |
| Claude plugin manifest | `.claude-plugin/plugin.json` | Declares the Monitor manifest and the MCP server (`.mcp.json` → `bin/envoy-mcp.ts`). |
| Monitor declaration | `monitors/monitors.json` | Starts `bin/envoy-monitor.ts` for every session. |
| Inbound transport | `src/envoy-monitor.ts` | NATS subscriptions and Monitor event output. |
| MCP server + Dispatch tools | `src/envoy-mcp-server.ts` | Runs with the monitor's session id; every followed topic is forwarded onto the agent subject. |
| Topic forwarder | `src/thread-forwarder.ts` | NATS subscribe → republish on `notifications.agent.<session-id>`, deduped by event id. |
| Outbound transport | `src/envoy-client.ts` | Envoy listener HTTP client. |
| Send CLI parsing | `src/send-arguments.ts` | Validates destination and message arguments. |

## Critical conventions

- Load with `claude --plugin-dir packages/claude-envoy-bridge`; no Claude configuration-file
  changes are needed.
- The monitor subscribes to `notifications.agent.<session-id>` directly, registers that route as
  self-subscribed with port zero, refreshes it every heartbeat, and deregisters it at shutdown.
  The MCP server records registry interests for topics the session follows and forwards them onto
  that agent subject. Manual `envoy_subscribe` establishes the NATS forwarding leg before recording
  its interest; Dispatch auto-subscription remains best-effort when a broker is unavailable.
- The monitor uses `CLAUDE_CODE_SESSION_ID` for its direct route. Set `ENVOY_SESSION_ID` only
  to explicitly override that identity for controlled QA; without either identity, the monitor
  exits with an error rather than subscribing to a made-up route.
- Preserve one stdout line per inbound message because Claude Code consumes Monitor output as
  events.
- The shared renderer is tolerant of additive envelopes and parse failures. Never add an adapter-
  specific renderer or emit raw inbound envelope bytes.

## Topic reminder

- Direct Claude session: `notifications.agent.<session_id>`
