# claude-envoy-bridge

Claude Code plugin for Legion's Envoy subsystem. It uses Claude Code's Tier 1 Monitor tool
integration to deliver Envoy traffic into a live session, including idle sessions.

## Architecture

- `.claude-plugin/plugin.json` declares the plugin and its always-on monitor.
- `bin/envoy-monitor.ts` subscribes directly to `notifications.agent.<session-id>` over NATS.
- Monitor stdout is rendered by Claude Code as a Monitor event, waking the session for inbound
  Envoy traffic.
- `bin/envoy-send.ts` sends a direct message through Envoy's local Go listener HTTP API.
- `.mcp.json` mounts two MCP servers for every Claude session: `envoy` (messaging tools) and
  `dispatch`, the shared shim from `@legion/envoy-client` that raises a durable question to
  Sami as a GitHub issue thread. The shim serves only when `dispatch.enabled` is set in
  `envoy.json`, and fills the target repo from the session's working directory.
- `skills/` symlinks the repository's shared skills tree, so a Claude session gets the
  `dispatch` skill (when to raise a question) alongside the tool itself.

The adapter intentionally does not register a listener with Envoy's session registry. It consumes
the direct NATS topic itself.

## Enable

From the Legion repository root, install workspace dependencies and load the package directly:

```bash
bun install
claude --plugin-dir packages/claude-envoy-bridge
```

The monitor subscribes using Claude Code's `CLAUDE_CODE_SESSION_ID`. `ENVOY_SESSION_ID` is an
explicit override for controlled QA. If neither is available, the monitor exits with an actionable
error rather than subscribing to a made-up address. No Claude configuration-file changes are
required.

## Send from a Claude session

```bash
bun packages/claude-envoy-bridge/bin/envoy-send.ts <target-session-id> "message"
```

Set `ENVOY_URL` to use an Envoy listener other than `http://127.0.0.1:9020`. Set
`ENVOY_NATS_URL` to use a NATS server other than `nats://envoy-nats:4222`; `ENVOY_TOPICS` adds
comma-separated NATS subscriptions.

## Local checks

```bash
bun run --cwd packages/claude-envoy-bridge lint
bun run --cwd packages/claude-envoy-bridge typecheck
bun run --cwd packages/claude-envoy-bridge test
```

## Caveats

Claude Code owns Monitor stdout semantics, including any truncation of unusually long event lines.
The monitor needs a real session identity, supplied by Claude Code or explicitly through
`ENVOY_SESSION_ID` for controlled QA.
