# @sjawhar/claude-legion-envoy

Claude Code plugin for Legion's Envoy subsystem. It uses Claude Code's Tier 1 Monitor tool
integration to deliver Envoy traffic into a live session, including idle sessions.

## Architecture

- `.claude-plugin/plugin.json` declares the plugin and its always-on monitor.
- `bin/envoy-monitor.ts` subscribes directly to `notifications.agent.<session-id>` over NATS.
- Monitor stdout is rendered by Claude Code as a Monitor event, waking the session for inbound
  Envoy traffic.
- `bin/envoy-send.ts` sends a direct message through Envoy's local Go listener HTTP API.

The adapter intentionally does not register a listener with Envoy's session registry. It consumes
the direct NATS topic itself.

## Enable

From the Legion repository root, install workspace dependencies and load the package directly:

```bash
bun install
ENVOY_SESSION_ID=my-claude-session claude --plugin-dir packages/envoy-claude-plugin
```

`ENVOY_SESSION_ID` gives the monitor a stable direct address. Without it, the monitor uses
`CLAUDE_SESSION_ID`, then a local `claude-<parent-pid>` fallback. No Claude configuration-file
changes are required.

## Send from a Claude session

```bash
bun packages/envoy-claude-plugin/bin/envoy-send.ts <target-session-id> "message"
```

Set `ENVOY_URL` to use an Envoy listener other than `http://127.0.0.1:9020`. Set
`ENVOY_NATS_URL` to use a NATS server other than `nats://envoy-nats:4222`; `ENVOY_TOPICS` adds
comma-separated NATS subscriptions.

## Local checks

```bash
bun run --cwd packages/envoy-claude-plugin lint
bun run --cwd packages/envoy-claude-plugin typecheck
bun run --cwd packages/envoy-claude-plugin test
```

## Caveats

Claude Code owns Monitor stdout semantics, including any truncation of unusually long event lines.
The stable session identity is caller-supplied through `ENVOY_SESSION_ID`; the process-ID fallback
changes when Claude restarts.
