# claude-envoy-bridge

Claude Code channel plugin for Legion's Envoy subsystem. One MCP stdio server consumes Envoy NATS
events and sends them to the current Claude Code session as supported
`notifications/claude/channel` notifications.

## Architecture

- `.mcp.json` launches `bin/envoy-channel.ts`. It declares the experimental `claude/channel`
  capability, so Claude Code accepts its event notifications; it also exposes Envoy messaging and
  native Dispatch tools through that same MCP server.
- The channel server subscribes directly to `notifications.agent.<session_id>` and to every topic
  followed by `envoy_subscribe` or a successful Dispatch mutation. It renders every envelope with
  the shared `@legion/envoy-client/delivery` renderer and never exposes raw envelope bytes.
- Direct NATS request-reply delivery receives its empty receipt immediately after the server
  accepts the event into its ordered MCP notification queue. That confirms adapter acceptance, not
  that Claude Code or the model processed the event: the channel protocol has no processing ack.
- The server registers the session as self-subscribed with `capabilities: ["aside"]`. It does not
  advertise `btw`.
- `envoy_role_set` persists the held role in `${CLAUDE_PLUGIN_DATA}/envoy-role.json`. A new server
  process soft-reclaims it with the saved `previous_session_id`; every healthy registration
  heartbeat checks and reasserts the role if the listener lost it. Shutdown drains NATS before
  deregistering the session.
- `envoy_inbox` is local recovery state: the most recent 50 event summaries, with no envelope
  body, ordered newest first.
- `bin/envoy-send.ts` sends direct messages through Envoy's local listener HTTP API.

## Channel metadata

Each notification has rendered Envoy content and only identifier-safe, string-valued metadata.
Claude Code drops invalid meta keys, so the server filters them before writing to stdio.

| Key | Meaning |
| --- | --- |
| `source` | Envoy producer, or `unknown` for a malformed envelope. |
| `topic` | NATS subject that delivered the event. |
| `event_id` | Envoy event identity; used for channel-side deduplication. |
| `dedupe_key` | Legacy/logical identity when supplied by Envoy. |
| `urgency` | Optional Envoy priority. |
| `from_session` | Optional source session identifier. |
| `expects_reply` | Optional Envoy reply expectation. |
| `in_reply_to` | Optional correlated inbound event id. |

## Dispatch and Envoy tools

The server exposes the shared Envoy messaging contract, including `envoy_inbox` and
`envoy_role_get`. When Dispatch is configured, it additionally exposes the fourteen native tools:
`dispatch_issue`, `dispatch_ask`, `dispatch_edit_ask`, `dispatch_resolve_ask`,
`dispatch_comment`, `dispatch_suggest`, `dispatch_message`, `dispatch_doc_edit`,
`dispatch_doc_read`, `dispatch_request_approval`, `dispatch_artifact`, `dispatch_read`,
`dispatch_search`, and `dispatch_open_asks`.

Dispatch configuration follows `@legion/envoy-client/dispatch-config`: set `dispatch.enabled`,
`dispatch.serverUrl`, and `dispatch.token` in envoy.json, or provide `DISPATCH_URL` and
`DISPATCH_TOKEN`. The resolved configuration is re-read on every Dispatch tool call. A successful
mutation follows its event topic; reads do not add a subscription.

Dispatch asks remain on native Dispatch. Channel notifications are one-way ingress, not a remote
human approval surface.

## Enable the channel

Install the plugin from its marketplace, then opt it into a local development session during the
research preview:

```bash
claude --dangerously-load-development-channels plugin:claude-envoy-bridge@<marketplace>
```

For a Team or Enterprise deployment, an organization owner must enable channels and allow the
internal marketplace plugin in managed settings. The user still opts the plugin into each session
with `--channels`:

```json
{
  "channelsEnabled": true,
  "allowedChannelPlugins": [
    { "marketplace": "<marketplace>", "plugin": "claude-envoy-bridge" }
  ]
}
```

The plugin needs `ENVOY_NATS_URL` and reaches the listener at `ENVOY_URL`, which defaults to
`http://127.0.0.1:9020`. Claude Code provides `CLAUDE_CODE_SESSION_ID` and
`CLAUDE_PLUGIN_DATA`; set `ENVOY_SESSION_ID` only to use a controlled identity for QA.

`claude -p` can run a channel session, but it disables features that need terminal input, including
multiple-choice questions and plan approval. Production Legion workers must have MCP and channel
consent preconfigured and must not depend on local dialogs.

## Intentionally omitted

- **Targeted BTW:** the channel registers only `aside`. A channel notification has no model or
  Claude Code acknowledgement, so advertising BTW would promise an automatic correlated Dispatch
  reply that does not exist. Add it only with a server-owned pending-delivery state and explicit
  reply tool design.
- **Permission relay:** the server does not declare `claude/channel/permission`. It has no
  sender-authenticated reply path or reply-ack design for remote permission decisions; declaring
  one would let unauthenticated Envoy payloads approve tool use. Dispatch asks stay on Dispatch.

## Manual smoke

`smoke-channel.sh` is a real manual smoke, never a CI step. It starts an isolated,
interactive Claude Code session with the development-channel flag, waits for its Envoy
registration, sends an actual direct Envoy event, and asserts the model writes the unique payload
before checking teardown. It requires a live Envoy listener, NATS, Claude authentication, a
configured model, an organization that enables channels, and an installed plugin entry:

```bash
ENVOY_NATS_URL=nats://envoy-nats:4222 \
  CLAUDE_CHANNEL_ENTRY=plugin:claude-envoy-bridge@<marketplace> \
  bash -c 'cd packages/claude-envoy-bridge/scripts && ./smoke-channel.sh'
```

Use `CLAUDE_BIN` or `ENVOY_URL` to select a different Claude binary or listener. For source-tree
diagnostics, pass a temporary bare-server config through `CLAUDE_MCP_CONFIG` and use
`CLAUDE_CHANNEL_ENTRY=server:envoy`; the marketplace command above is the plugin packaging check.

## Local checks

```bash
bun run --cwd packages/claude-envoy-bridge lint
bun run --cwd packages/claude-envoy-bridge typecheck
bun run --cwd packages/claude-envoy-bridge test
```
