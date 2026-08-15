# Claude Envoy Plugin Package

Claude Code plugin package for Legion's Envoy subsystem.

## Overview

The adapter uses an always-on Claude Code Monitor process as a Tier 1 inbound bus. The monitor
subscribes directly to Envoy NATS topics and writes normalized Monitor events to stdout. Claude
Code surfaces those events to the session, including when the session is idle. The companion send
CLI uses Envoy's local Go listener HTTP API for outbound direct messages.

## Where to look

| Task | Location | Notes |
| --- | --- | --- |
| Claude plugin manifest | `.claude-plugin/plugin.json` | Declares the Monitor manifest. |
| Monitor declaration | `monitors/monitors.json` | Starts `bin/envoy-monitor.ts` for every session. |
| Inbound transport | `src/envoy-monitor.ts` | NATS subscriptions and Monitor event output. |
| Outbound transport | `src/envoy-client.ts` | Envoy listener HTTP client. |
| Send CLI parsing | `src/send-arguments.ts` | Validates destination and message arguments. |

## Critical conventions

- Load with `claude --plugin-dir packages/envoy-claude-plugin`; no Claude configuration-file
  changes are needed.
- Keep direct subscription to `notifications.agent.<session-id>`; do not add Envoy listener
  registry registration to this package.
- The monitor uses `CLAUDE_CODE_SESSION_ID` for its direct route. Set `ENVOY_SESSION_ID` only
  to explicitly override that identity for controlled QA; without either identity, the monitor
  exits with an error rather than subscribing to a made-up route.
- Preserve one stdout line per inbound message because Claude Code consumes Monitor output as
  events.

## Topic reminder

- Direct Claude session: `notifications.agent.<session_id>`
