# claude-envoy

Claude Code channel plugin for Legion's Envoy subsystem. One MCP stdio server consumes Envoy NATS
events and sends them to the current Claude Code session as supported
`notifications/claude/channel` notifications.

## Architecture

- `.mcp.json` launches the committed bundle `dist/envoy-channel.js` (built from
  `bin/envoy-channel.ts`; see "The bundle" below). It declares the experimental `claude/channel`
  capability, so Claude Code accepts its event notifications; it also exposes Envoy messaging and
  native Dispatch tools through that same MCP server.
- `.omp-plugin/plugin.json` declares an empty `mcpServers`, so Oh My Pi starts no server from this
  plugin: the channel server needs Claude Code's session identity and exits without it, and an omp
  session already gets Envoy and Dispatch from `@sjawhar/pi-legion-envoy`. omp reads that manifest
  before `.claude-plugin/plugin.json` and a manifest `mcpServers` replaces `.mcp.json` instead of
  merging with it; Claude Code reads only `.claude-plugin/plugin.json`, so it still launches the
  server. Omitting the key would not work — omp would fall through to `.mcp.json`. Skills are
  unaffected: both harnesses resolve them from `.claude-plugin/plugin.json`.
- `hooks/hooks.json` runs `dist/open-asks-hook.js` on every `SessionStart` (startup, resume, clear,
  compact, fork). It records the current session id for the channel server and puts the session's
  open Dispatch asks into the model's context (`Dispatch authored-ask summary:`;
  `unavailable: <reason>` when Dispatch cannot be reached; nothing when Dispatch is not
  configured).
- The channel server subscribes directly to `notifications.agent.<session_id>` and to every topic
  followed by `envoy_subscribe` or a successful Dispatch mutation. It renders every envelope with
  the shared `@legion/envoy-client/delivery` renderer and never exposes raw envelope bytes.
- A forwarded role-lane envelope (one that still names its `notifications.role.<role>` topic while
  arriving on the direct subject) receives its empty receipt immediately after the server accepts
  the event into its ordered MCP notification queue. That confirms adapter acceptance, not that
  Claude Code or the model processed the event: the channel protocol has no processing ack. A plain
  direct send (`/v1/messages/send`) is a JetStream publish and gets no receipt: its reply subject is
  the publisher's acknowledgement inbox, and an empty receipt there fails the publish.
- The server registers the session as self-subscribed with `capabilities: ["aside"]`. It does not
  advertise `btw` or `steer` (see "Intentionally omitted").
- A targeted Dispatch delivery the shared renderer rejects is never shown to the model: when it
  names a message, the server posts `Invalid Dispatch targeted delivery frame` to
  `POST /api/v1/messages/{id}/reply` so the attempt fails visibly; otherwise it is logged and dropped.
- No Dispatch write subscribes the session to an issue. A write that makes the session follow an
  ask (`details.follows.ask`) tells the model once per ask
  `Following ask <id> on <issue>: its answer and replies reach you directly (dispatch_follow unfollow to stop). For every event on <issue>: envoy_subscribe <topic>.`
  A resumed server rebuilds the interests its session id already registered. `envoy_list` reports
  the union of live NATS subscriptions and registry interests with each topic's `source` (`live`,
  `registry`, `both`).
- `envoy_role_set` persists the held role in `${CLAUDE_PLUGIN_DATA}/roles/<session-id>.json`.
  `claude --resume <session-id>` finds that file and soft-reclaims the role with the saved
  `previous_session_id`; a plain relaunch is a new session and inherits nothing. Every healthy
  registration heartbeat checks and reasserts the role if the listener lost it. Shutdown (stdin
  end, SIGTERM, SIGINT, or the SIGHUP a closing pane sends the whole process group) drains NATS
  and deregisters the session.
- `/clear` mints a new Claude session id, but a stdio MCP server keeps the `CLAUDE_CODE_SESSION_ID`
  it was spawned with. The SessionStart hook writes the current id to
  `${CLAUDE_PLUGIN_DATA}/sessions/<claude-pid>/session-id` (keyed by the `claude` process both the
  hook and the server are children of, so concurrent sessions never share a file). The server
  polls that file every 250 ms and follows a changed id within that time, with the heartbeat as the
  fallback: it subscribes the new direct subject first, deregisters the old id, drops the old
  subject, registers the new one, and moves a held role by soft claim. The listener merges
  registered topics into the entry and never removes one, so the old subject goes before the
  registration, and the server sends its registry writes one at a time, each registration reading
  the id and topics when it runs.
  Asks authored before `/clear` carry the old Dispatch actor id and drop out of the open-asks
  summary (the same shape as OMP on fork). `ENVOY_SESSION_ID` (QA override) disables the handoff.
- `envoy_inbox` is local recovery state: the most recent 50 event summaries, with no envelope
  body, ordered newest first.
- `bin/envoy-send.ts` sends direct messages through Envoy's local listener HTTP API.

## Channel metadata

Each notification has rendered Envoy content and only identifier-safe, string-valued metadata.
Claude Code drops invalid meta keys, so the server filters them before writing to stdio.

| Key | Meaning |
| --- | --- |
| `producer` | Envoy producer, or `unknown` for a malformed envelope (`source` is Claude Code's own attribute, naming the channel). |
| `topic` | NATS subject that delivered the event. |
| `event_id` | Envoy event identity; used for channel-side deduplication. |
| `dedupe_key` | Legacy/logical identity when supplied by Envoy. |
| `urgency` | Optional Envoy priority. |
| `from_session` | Optional source session identifier. |
| `expects_reply` | Optional Envoy reply expectation. |
| `in_reply_to` | Optional correlated inbound event id. |

## Dispatch and Envoy tools

The server exposes the shared Envoy messaging contract, including `envoy_inbox` and
`envoy_role_get`. When Dispatch is configured, it additionally exposes the twenty-one native tools:
`dispatch_issue`, `dispatch_issue_update`, `dispatch_claim`, `dispatch_ask`, `dispatch_edit_ask`, `dispatch_resolve_ask`, `dispatch_resolve_comment`,
`dispatch_follow`, `dispatch_comment`, `dispatch_suggest`, `dispatch_message`, `dispatch_doc_edit`,
`dispatch_doc_read`, `dispatch_request_approval`, `dispatch_artifact`, `dispatch_read`,
`dispatch_search`, `dispatch_issues`, `dispatch_architecture_sync`, `dispatch_open_asks`, and `dispatch_whoami`.

Dispatch configuration follows `@legion/envoy-client/dispatch-config`: set `dispatch.enabled`,
`dispatch.serverUrl`, and `dispatch.token` in envoy.json, or provide `DISPATCH_URL` and
`DISPATCH_TOKEN`. The resolved configuration is re-read on every Dispatch tool call. A successful
mutation follows its event topic; reads do not add a subscription.

Dispatch asks remain on native Dispatch. Channel notifications are one-way ingress, not a remote
human approval surface.

## Skills

The plugin ships two skills, `claude-envoy:envoy` and `claude-envoy:dispatch`, which teach the
tools above. `skills/` holds one relative symlink per skill into the repository-root `skills/`,
which stays their only source; Claude Code copies each target into the plugin cache at install.

`envoy` and `dispatch` are the two a standalone Claude Code session uses. The other root skills
stay out: they belong to Legion's roles (the architect, controller, oracle and retro skills, and the
phase workers' `legion-worker` and `ce-simplify-code`) and to the reviewer's `thermonuclear-*`
pair, and those run on Oh My Pi (`skills/AGENTS.md`). The `thermonuclear-*` rubrics also ship in
the sjawhar@sjawhar plugin (sjawhar/dotfiles marketplace). Link another root skill here only when
a standalone Claude Code session needs it.

## The bundle

The marketplace installs this package's git tree into Claude Code's plugin cache with no
`node_modules`: `workspace:*` dependencies cannot resolve there, and Claude Code skips its automatic
dependency install because the package has no lockfile of its own (the monorepo's lives at the
root). So the two executables ship as committed single-file Bun bundles, `dist/envoy-channel.js`
and `dist/open-asks-hook.js`, with every dependency inlined (`@legion/contracts`,
`@legion/envoy-client`, `@modelcontextprotocol/sdk`, `nats`, `zod`, `ky`; the package version is
inlined from `package.json`, so the MCP server, `plugin.json`, and `package.json` spell one version).

- `bun run build` rebuilds `dist/` (`Bun.build`, target `bun`, whitespace + identifier minification only - syntax minification is off because Bun 1.3.14's constant folding sometimes truncates concatenated string literals in CI builds - no sourcemap).
- `bun run check-dist` rebuilds into a scratch directory and fails when it differs from the
  committed files. CI runs it on the Bun version pinned in the repo-root `.bun-version`, because
  bundler output differs across Bun releases; rebuild on that version before committing.
- pi-envoy solves the same problem with `prepack.sh` for npm; this plugin's distribution channel is
  the git repository, so its bundle lives in-tree. An npm-published plugin and a `dist` release
  branch were considered and rejected as more moving parts for the same result.

## Enable the channel

Channels are gated by managed settings on this fleet: an organization owner enables channels and
allowlists the internal marketplace plugin in `/etc/claude-code/managed-settings.json`, and the
user opts the plugin into each session with `--channels`:

```json
{
  "channelsEnabled": true,
  "allowedChannelPlugins": [
    { "marketplace": "legion-plugins", "plugin": "claude-envoy" }
  ]
}
```

```bash
claude --channels plugin:claude-envoy@legion-plugins
```

For a plugin outside the allowlist (a development marketplace, a bare `server:` entry) use
`--dangerously-load-development-channels <entry>`; it bypasses the allowlist only, so
`channelsEnabled` still applies. Do not combine it with a `--channels` entry that names the same
plugin from another marketplace: Claude Code then skips the channel (`Channel notifications
skipped: you asked for plugin:…@legion-plugins but the installed claude-envoy plugin is from
…`).

The plugin needs `ENVOY_NATS_URL` and reaches the listener at `ENVOY_URL`, which defaults to
`http://127.0.0.1:9020`. Both are inherited from the environment that launched `claude` —
`.mcp.json` passes through only `CLAUDE_PROJECT_DIR`, because an unset `${VAR}` in `.mcp.json` is
substituted as the literal text, which used to register a session as the literal id
`${ENVOY_SESSION_ID}`. Claude Code provides `CLAUDE_CODE_SESSION_ID` and `CLAUDE_PLUGIN_DATA`; set
`ENVOY_SESSION_ID` only to use a controlled identity for QA (the smoke does). Dispatch tools and
the open-asks hook read `DISPATCH_URL`/`DISPATCH_TOKEN` or `envoy.json` through
`@legion/envoy-client/dispatch-config`.

`claude -p` can run a channel session, but it disables features that need terminal input, including
multiple-choice questions and plan approval. Production Legion workers must have MCP and channel
consent preconfigured and must not depend on local dialogs.

## Intentionally omitted

- **Targeted BTW:** the channel registers only `aside`. A channel notification has no model or
  Claude Code acknowledgement, so advertising BTW would promise an automatic correlated Dispatch
  reply that does not exist. Add it only with a server-owned pending-delivery state and explicit
  reply tool design.
- **Steer:** Claude Code queues channel notifications for the model's next turn, so a steer cannot
  be honoured any differently from an aside; the session does not advertise it.
- **Permission relay:** the server does not declare `claude/channel/permission`. It has no
  sender-authenticated reply path or reply-ack design for remote permission decisions; declaring
  one would let unauthenticated Envoy payloads approve tool use. Dispatch asks stay on Dispatch.

## Manual smoke

`smoke-channel.sh` is a real manual smoke, never a CI step. It starts an isolated, interactive
Claude Code session, answers its startup dialogs unattended (folder trust, the development-channel
warning, first-use MCP consent — in whatever order they appear), waits for the Envoy registration,
sends an actual direct Envoy event, and asserts the model writes the unique payload before checking
teardown. It requires a live Envoy listener, NATS, Claude authentication, a configured model, an
organization that enables channels, and an installed plugin entry:

```bash
ENVOY_NATS_URL=nats://envoy-nats:4222 \
  CLAUDE_CHANNEL_ENTRY=plugin:claude-envoy@legion-plugins \
  bash -c 'cd packages/claude-envoy/scripts && ./smoke-channel.sh'
```

`CLAUDE_CHANNEL_FLAG` defaults to `--channels` when the entry ends in `@legion-plugins` (the
allowlisted marketplace) and to `--dangerously-load-development-channels` for every other entry;
set it explicitly to override. The `PASS` line prints the registered session row (id, dir,
capabilities); a failure dumps the pane and the full `tmux pipe-pane` capture of everything Claude
printed. Use `CLAUDE_BIN` or `ENVOY_URL` to select a different Claude binary or listener. For
source-tree diagnostics, pass a temporary bare-server config through `CLAUDE_MCP_CONFIG` and use
`CLAUDE_CHANNEL_ENTRY=server:envoy`; the marketplace command above is the plugin packaging check.

To exercise an unmerged checkout the way the marketplace would install it, register a development
marketplace with a `command` source in copy mode whose command prints a staged copy of the package
(without `node_modules`), install `claude-envoy@<dev-marketplace>`, and run the smoke against
that entry. Never `claude plugin marketplace add sjawhar/legion#<branch>`: the marketplace name comes
from the repository's `marketplace.json` and would replace the real `legion-plugins` registration.

`smoke-clear-rebind.sh` checks the `/clear` handoff against the live listener. It starts Claude
Code with user settings left out (`--setting-sources project,local`) and the build under test from
`--plugin-dir` (`CLAUDE_PLUGIN_DIR`, default: this package, which runs its committed `dist/`, so run
`bun run build` first), sends `/clear`, and passes when the registry entry has moved to the new
session id, lists the new direct subject, and still lacks the old one two heartbeats later. It also
fails when the entry moves later than the handoff poll allows; with `ENVOY_HEARTBEAT_MS=60000` that
checks the poll rather than a heartbeat that happened to fall soon after `/clear`. It needs
`tmux`, `curl`, `jq`, an authenticated `claude`, and the listener and NATS; it sends no prompt to the
model and removes both ids' registry entries when it exits. Pointed at an older build (an installed
plugin cache directory), it reproduces the stale subject that build leaves behind:

```bash
ENVOY_NATS_URL=nats://127.0.0.1:4222 packages/claude-envoy/scripts/smoke-clear-rebind.sh
```

## Local checks

```bash
bun run --cwd packages/claude-envoy lint
bun run --cwd packages/claude-envoy typecheck
bun run --cwd packages/claude-envoy test
bun run --cwd packages/claude-envoy check-dist
```
