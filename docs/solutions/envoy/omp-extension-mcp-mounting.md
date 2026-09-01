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
      "command": "./bin/dispatch-mcp-shim.ts"
    }
  }
}
```

The script needs a shebang (`#!/usr/bin/env bun`) and the executable bit; `bun build`
preserves the shebang and npm tarballs preserve file modes.

Key mechanics (from `@oh-my-pi/pi-coding-agent` `src/discovery/omp-plugins.ts` and
`src/discovery/substitute-plugin-root.ts`, verified on v18.0.10):

- **`${OMP_PLUGIN_ROOT}` is NOT substituted here.** `substitutePluginRoot()` exists but the
  `omp-plugins` MCP loader never calls it — that substitution serves the Claude marketplace
  provider. A `${OMP_PLUGIN_ROOT}` in `args` reaches the spawn verbatim and the subprocess
  dies instantly ("MCP subprocess closed stdout before responding" in `~/.omp/logs/`).
- What IS rebased (`resolvePluginStdioPaths`): a path-like `command` (`./`, `../`) resolves
  against the package root, and a relative `cwd` resolves against the package root. Bare
  commands (`bun`, `npx`) and absolute paths pass through. **`args` are never rebased.**
  Prefer the path-like `command` WITHOUT `cwd`: the subprocess keeps the session cwd, so
  cwd-scoped behavior (project `envoy.json`, `gh` identity) matches a user-config mount.
  Setting `cwd: "."` instead pins the subprocess to the package dir and silently bypasses
  project-scoped config.
- Servers are keyed by name (`capability/mcp.ts` `key: server => server.name`), so a
  same-named entry in native user config overrides the package-shipped one — collisions are
  safe.
- An "extension package root" is any directory loaded via `--extension`/`-e`, `extensions:`
  settings, or an installed plugin; for a monorepo installed whole as a plugin, the root is
  the repo root, not the subpackage.
- The provider also scans package-root `skills/`, `commands/`, `rules/`, `prompts/`,
  `hooks/`, and `tools/` — a package can ship those the same way.

Per-user gating that OpenCode did at injection time moves into the spawned server itself: the
dispatch shim resolves its endpoint from the shared `envoy.json` (`dispatch.enabled` /
`dispatch.serverUrl`, `DISPATCH_MCP_URL` env override) and exits 0 without serving when
dispatch is not enabled (`packages/envoy-client/src/dispatch-config.ts`).

## Why This Matters

Without this, "the extension should provide tool X to its sessions" dead-ends on the missing
runtime API and gets solved with per-machine hand-edited config files. Whether to actually
ship a package-root `.mcp.json` is a separate security decision: it mounts for **every**
session that loads the package. For dispatch, Legion loads `pi-envoy` for architects,
controller, and phase workers alike, and the raw `dispatch` tool is served to every one
of them deliberately — every role needs a direct channel to ask Sami a durable question,
and the asking session gets the reply regardless of role. The shim gates on exactly one
thing, whether dispatch is enabled (`packages/envoy-client/src/dispatch-config.ts`), not on
the Legion environment.

## When to Apply

Any time an OMP extension package should carry an MCP server whose audience really is "every
session that loads the package"; any time OpenCode plugin behavior built on config-hook
injection needs an OMP equivalent. When the tool must be scoped narrower than the package's
sessions, gate inside the spawned server (config opt-in, environment decline) — the static
mount itself cannot discriminate.

## Examples

Dispatch mounts this way in production: `packages/pi-envoy/.mcp.json` spawns
`bin/dispatch-mcp-shim.ts` from source in repo checkouts (the Legion daemon path) and the
release workflow rewrites the `command` to the self-contained `dist/bin/dispatch-mcp-shim.js` for
the npm tarball. Both forms verified on v18.0.10 with
`omp -p --no-extensions -e <package>` from a scratch directory — `mcp__dispatch_dispatch`
mounts with zero session-level config. Beware LLM self-reports here: a probe model once
answered "YES" while `~/.omp/logs/` showed the mount had failed. Ground truth lives in the
log ("MCP tool load failed, path: mcp:dispatch") and in the spawn argv (wrap `bun` in a
logging shim on PATH to capture it).
