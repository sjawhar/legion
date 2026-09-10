# claude-envoy-bridge

Claude Code plugin for Legion's Envoy subsystem. It uses Claude Code's Tier 1 Monitor tool
integration to deliver Envoy traffic into a live session, including idle sessions.

## Architecture

- `.claude-plugin/plugin.json` declares the plugin and its always-on monitor.
- `bin/envoy-monitor.ts` subscribes directly to `notifications.agent.<session-id>` over NATS,
  registers that self-subscribed route with Envoy, and refreshes the registration every heartbeat.
  Monitor stdout is rendered by Claude Code as a Monitor event, waking the session for inbound
  Envoy traffic.
- `bin/envoy-send.ts` sends a direct message through Envoy's local Go listener HTTP API.
- `.mcp.json` mounts one MCP server for every Claude session: `envoy`, whose tools are the shared
  Envoy messaging contract plus the eleven native Dispatch tools: `dispatch_issue`,
  `dispatch_ask`, `dispatch_resolve_ask`, `dispatch_comment`, `dispatch_suggest`,
  `dispatch_message`, `dispatch_doc_edit`, `dispatch_doc_read`, `dispatch_artifact`,
  `dispatch_read`, and `dispatch_search`. The tools are offered only when `dispatch.enabled`
  resolves a server URL and bearer token in envoy.json or when `DISPATCH_URL` and
  `DISPATCH_TOKEN` provide them; with `dispatch.enabled: true` and no `dispatch.serverUrl`, the
  URL is `http://localhost:8766`. Each issue-scoped call fills the target repo from the session's
  working directory, stamps it with the Claude session id, and subscribes each successful
  mutation's `details.topic` so its Dispatch events arrive back through Envoy.

- The MCP server is also the session's topic consumer. Envoy pushes nothing to a session that
  consumes NATS itself, and the monitor listens only on `notifications.agent.<session-id>`, so
  for every topic the session follows — a Dispatch mutation or anything passed to
  `envoy_subscribe` — the server subscribes NATS (`ENVOY_NATS_URL`, the monitor's broker) and
  republishes each envelope on the session's agent subject, where the monitor renders it.
  `envoy_unsubscribe` stops the forwarding; closing the session drains the connection. A manual
  `envoy_subscribe` rejects before recording an interest if its NATS forwarder is unavailable;
  Dispatch auto-subscription remains best-effort and reports the gap on stderr.
- `skills/` symlinks the repository's shared skills tree, so a Claude session gets the
  `dispatch` skill (when to raise a question) alongside the tools.

`dispatch_artifact` accepts exactly one upload source: a local `path`, or inline `content`.
An architect can post a specification directly with
`{ issue, name: "spec.md", content: "# Design" }`.

## Inbound rendering

The monitor uses the same tolerant `@legion/envoy-client/delivery` renderer as other Envoy hosts.
It emits one TOON block containing recognized delivery fields (`to`, `from`, `at`, `id`, expiry,
reply metadata, and `summary`) and a structured payload only once as `message`. Unknown sources
and malformed frames become safe `unrecognised` fields; raw frame bytes are never emitted.

The shared MCP tool list intentionally omits `envoy_inbox`, which is Pi-specific local recovery
state. It includes `envoy_role_get` for the listener's current role holder.

## Enable

From the Legion repository root, install workspace dependencies and load the package directly:

```bash
bun install
claude --plugin-dir packages/claude-envoy-bridge
```

The monitor and the `envoy` MCP tools identify the session by Claude Code's
`CLAUDE_CODE_SESSION_ID`, so messages, `envoy_whoami`, and native Dispatch operations all name one session.
`ENVOY_SESSION_ID` is an explicit override for controlled QA. If neither is available, the
monitor exits with an actionable error rather than subscribing to a made-up address. No Claude
configuration-file changes are required.

## Send from a Claude session

```bash
bun packages/claude-envoy-bridge/bin/envoy-send.ts <target-session-id> "message"
```

Set `ENVOY_URL` to use an Envoy listener other than `http://127.0.0.1:9020`. Set
`ENVOY_NATS_URL` to use a NATS server other than `nats://example-host:4222`; `ENVOY_TOPICS` adds
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
