# Pi Envoy Extension

Tracked Oh My Pi extension for Envoy messaging. It shares the Envoy HTTP client, tool
contract, envelope parsing, and subject helpers with the other Legion adapters while keeping
OMP's direct NATS subscriptions and Pi steering delivery local (inbound messages steer an
in-flight turn instead of queueing behind it).

Normal topic subscriptions are direct NATS subscriptions owned by this extension. A role claim is
different: the listener arbitrates the core-NATS role lane for the current live holder, then sends
a receipt-backed request with the original role topic to the holder's direct agent subject. The
agent pump replies after accepting the envelope; without a receipt within two seconds the listener
emits a `delivery_failed` exception. Role messages are live only; they are not retained for a later
claimant.

`envoy_list()` shows the union of the local subscriptions and the listener's persisted interest
registry. Each reported interest identifies whether it is `live`, `registry`, or `both`, so
temporary registration drift does not hide the extension's actual delivery state.

## Session identity

Run `/whoami` to copy the active session ID to the clipboard. OMP copies through its host
clipboard API, which sends OSC 52
first for tmux and SSH sessions. The notification shows the session ID even if the copy fails.

For tmux to accept OSC 52 clipboard writes, enable clipboard support in the tmux server:

```tmux
set -g set-clipboard on
```

## Development install

This package declares two OMP extension entries in `package.json`: `extensions/envoy.ts`
(Envoy messaging, subscriptions, and steering delivery) and `extensions/legion.ts` (the
Legion lifecycle: root bootstrap, worker spawning, budgets, and daemon capabilities).
Loading the package directory with OMP's `--extension` flag — as the Legion daemon does
when it launches trees and workers — loads both.

For local development of the messaging extension alone, link the entry into OMP:

```sh
ln -sfn "$PWD/packages/pi-envoy/extensions/envoy.ts" \
  ~/.omp/agent/extensions/envoy.ts
```

The repository root `package.json` likewise loads only `extensions/envoy.ts` for dev
sessions inside this repo: the Legion extension is meant to be loaded by the daemon with
its environment prepared, not by ambient dev sessions.

Released installs package the extension and its `nats` dependency. The symlink is only for local
development.

## Dispatch

Legion sessions use the `envoy_dispatch` tool from `extensions/legion.ts`: it routes through
the Legion daemon's architect-only, tree-scoped `/legion/v1/dispatch-threads` endpoint, which
also registers the thread so replies route back to the tree. The raw dispatch MCP tool is
deliberately not served to Legion sessions — the shared shim exits without serving when it
sees a Legion environment (`LEGION_TREE`/`LEGION_CONTROLLER`), so phase workers cannot bypass
the architect gate with ambient GitHub authority.

Interactive OMP sessions get the `dispatch` MCP tool the way OpenCode sessions do: the shared
`@legion/envoy-client` shim mounts as a stdio MCP server, and it serves only when
`dispatch.enabled` is true in the shared envoy.json (`~/.config/opencode/envoy.json`,
shallow-merged with `<cwd>/.opencode/envoy.json`) or `DISPATCH_MCP_URL` is set explicitly.
The server URL comes from `dispatch.serverUrl` (default `http://localhost:8766`).

Mount it in `~/.omp/agent/mcp.json` (user-wide) or `.omp/mcp.json` (per project):

```json
{
  "mcpServers": {
    "dispatch": {
      "command": "bun",
      "args": ["<checkout>/packages/envoy-client/bin/dispatch-mcp-shim.ts"]
    }
  }
}
```

The shim forwards newline-delimited JSON-RPC from stdin to the dispatch server's Streamable
HTTP `/mcp` endpoint with a cached GitHub bearer from the user's `gh` shim. The
bearer refreshes before expiry and retries once immediately after a 401 response.
