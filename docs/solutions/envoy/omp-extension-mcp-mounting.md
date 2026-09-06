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
  - "Deciding between a native `pi.registerTool` tool and a package-shipped MCP server"
---

## Context

The OMP `ExtensionAPI` (`pi`) registers native tools imperatively (`pi.registerTool`) but has
**no runtime MCP registration method** — `omp://extensions.md` confirms MCP mounting is absent
from the imperative surface. An extension that wants every session to reach an MCP server
(as opposed to a tool it can implement in-process) has to ship the mount as a static file.

## Guidance

OMP discovers MCP servers from static files only, in priority order: native config
(`.omp/mcp.json` project, `~/.omp/agent/mcp.json` user), then **`.mcp.json`/`mcp.json` at the
root of every loaded extension package** (`omp-plugins` provider), then root-level fallbacks.
To ship an MCP server with an extension package, put `.mcp.json` next to its `package.json`:

```json
{
  "mcpServers": {
    "example-server": {
      "command": "./bin/example-server.ts"
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
- **A repo-root `package.json` `omp.extensions` manifest is inert on its own** (verified on
  v18.1.2): starting a session in a repo checkout does NOT load that repo's extension
  sources. Sessions load whatever the plugin registry installed (e.g. the published
  `@sjawhar/pi-legion-envoy` dist), so a merged pi-envoy fix reaches sessions only after an
  npm release + plugin reinstall — or when a caller passes the source path explicitly
  (`--extension`/`-e`, as the Legion daemon does). Verifying a fix against a live session
  requires confirming which dist that session actually loaded. See
  `session-id-remint-stale-transcript-identity.md` for the incident where this mattered.
- The provider also scans package-root `skills/`, `commands/`, `rules/`, `prompts/`,
  `hooks/`, and `tools/` — a package can ship those the same way.

Per-user gating lives in whatever the package mounts or registers: the dispatch tool, for
example, is registered only when `resolveDispatchConfig`
(`packages/envoy-client/src/dispatch-config.ts`) yields a service URL from `envoy.json`
(`dispatch.enabled` / `dispatch.serverUrl`) or `DISPATCH_MCP_URL`. Which GitHub repo a thread
lands in is not config either: the tool derives it from the session's cwd at call time.

## Why This Matters

Without this, "the extension should provide MCP server X to its sessions" dead-ends on the
missing runtime API and gets solved with per-machine hand-edited config files. Whether to
actually ship a package-root `.mcp.json` is a separate security decision: it mounts for
**every** session that loads the package, exactly as a native `pi.registerTool` tool does. For
dispatch, Legion loads `pi-envoy` for architects, controller, and phase workers alike, and the
raw `dispatch` tool is served to every one of them deliberately — every role needs a direct
channel to ask Sami a durable question, and the asking session gets the reply regardless of
role. The dispatch tool gates on exactly one thing, whether dispatch is enabled
(`packages/envoy-client/src/dispatch-config.ts`), not on the Legion environment.

## When to Apply

Any time an OMP extension package should carry an MCP server whose audience really is "every
session that loads the package" and the capability cannot be a native tool. When the tool must
be scoped narrower than the package's sessions, gate inside the spawned server (config opt-in,
environment decline) — the static mount itself cannot discriminate.

## Examples

Dispatch does not use this pattern: its tool is native to the extension (`pi.registerTool`
in `packages/pi-envoy/extensions/envoy.ts`), so pi-envoy ships no `.mcp.json`. The pattern
stands for any other MCP server an extension package needs every session to mount; verify a
mount with `omp -p --no-extensions -e <package>` from a scratch directory and read
`~/.omp/logs/` for `MCP tool load failed`, not the model's self-report.
