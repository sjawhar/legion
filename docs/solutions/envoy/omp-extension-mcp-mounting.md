---
title: "OMP extensions mount MCP servers via package-root .mcp.json, not a runtime API"
category: envoy
tags:
  - omp
  - mcp
  - pi-envoy
  - dispatch
  - extension-loading
date: 2026-08-30
status: active
module: pi-envoy
problem_type: architecture_pattern
applies_when:
  - "An OMP extension package needs to expose an MCP server to every session that loads it"
  - "Porting an OpenCode plugin capability that injected MCP entries via a config hook"
---

## Context

OpenCode plugins can inject MCP entries into session config at load time (the envoy-plugin
does this for the dispatch MCP server, gated by `envoy.json` `dispatch.enabled`). When Legion
moved to Oh My Pi, that injection path disappeared: the OMP `ExtensionAPI` (`pi`) has **no
runtime MCP registration method** — `omp://extensions.md` confirms MCP mounting is absent from
the imperative surface.

## Guidance

OMP discovers MCP servers from static files only, in priority order: native config
(`.omp/mcp.json` project, `~/.omp/agent/mcp.json` user), then **`.mcp.json`/`mcp.json` at the
root of every loaded extension package** (`omp-plugins` provider), then root-level fallbacks.
To ship an MCP server with an extension package, put `.mcp.json` next to its `package.json`:

```json
{
  "mcpServers": {
    "dispatch": {
      "command": "bun",
      "args": ["${OMP_PLUGIN_ROOT}/../envoy-client/bin/dispatch-mcp-shim.ts"]
    }
  }
}
```

Key mechanics (from `@oh-my-pi/pi-coding-agent` `src/discovery/omp-plugins.ts` and
`substitute-plugin-root.ts`):

- `${OMP_PLUGIN_ROOT}` is substituted recursively in all string values, including `args`.
- Relative `command`/`cwd` are rooted at the package directory, not the session cwd.
- Servers are keyed by name (`capability/mcp.ts` `key: server => server.name`), so a
  same-named entry in native user config overrides the package-shipped one — collisions are
  safe.
- An "extension package root" is any directory loaded via `--extension`/`-e`, `extensions:`
  settings, or an installed plugin; for a monorepo installed whole as a plugin, the root is
  the repo root, not the subpackage.

Per-user gating that OpenCode did at injection time moves into the spawned server itself: the
dispatch shim resolves its endpoint from the shared `envoy.json` (`dispatch.enabled` /
`dispatch.serverUrl`, `DISPATCH_MCP_URL` env override) and exits 0 without serving when
dispatch is not enabled (`packages/envoy-client/src/dispatch-config.ts`).

## Why This Matters

Without this, "the extension should provide tool X to its sessions" dead-ends on the missing
runtime API and gets solved with per-machine hand-edited config files. Whether to actually
ship a package-root `.mcp.json` is a separate security decision: it mounts for **every**
session that loads the package. For dispatch, Legion loads `pi-envoy` for architects,
controller, and phase workers alike, and workers must not get a raw gh-authority
thread-creation tool — so the dispatch shim is mounted per-user instead, and it declines to
serve in Legion environments (`LEGION_TREE`/`LEGION_CONTROLLER`) as defense in depth
(`packages/envoy-client/src/dispatch-mcp-shim.ts`).

## When to Apply

Any time an OMP extension package should carry an MCP server whose audience really is "every
session that loads the package"; any time OpenCode plugin behavior built on config-hook
injection needs an OMP equivalent. When the tool must be scoped narrower than the package's
sessions, gate inside the spawned server (config opt-in, environment decline) — the static
mount itself cannot discriminate.

## Examples

A package-root `.mcp.json` mounting the dispatch shim was verified end to end with
`omp -p --no-extensions -e packages/pi-envoy` from a scratch directory: the `dispatch` tool
mounted through the shim → gh bearer → HTTP `/mcp` chain with zero session-level config. It
was then removed in favor of user-level `~/.omp/agent/mcp.json` mounting precisely because the
package's session audience (all Legion roles) was broader than the tool's intended audience
(humans and their interactive sessions).
