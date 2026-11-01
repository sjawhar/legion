---
title: "A plugin reaches Oh My Pi's own modules through string-literal import() alone, and measures their shapes from a probe extension"
category: legion
tags:
  - oh-my-pi
  - pi-coding-agent
  - dynamic-import
  - extension-loader
  - getActiveTools
  - discoverAgents
  - probe-extension
  - bun-build-external
date: 2026-10-10
status: active
module: packages/pi-legion
related_issues:
  - "LEGION-663"
  - "sjawhar/legion#1874"
---
# A plugin reaches Oh My Pi's own modules through string-literal import() alone, and measures their shapes from a probe extension

- `@oh-my-pi/pi-coding-agent` and its subpaths (`task/discovery`, `task/settings`, `mcp/config`,
  `config/settings`, `extensibility/settings`, `extensibility/extensions/loader`, `web/search`)
  exist only inside the `omp` binary. Oh My Pi's loader resolves a bare specifier of them when the
  extension writes it as a **string literal**: `await import("@oh-my-pi/pi-coding-agent/mcp/config")`.
  `const spec = "…"; await import(spec)` fails with `Cannot find package`. Keep the import lazy
  (inside the function that needs it): a top-level import of a subpath fails `bun test`, which has
  no loader for the package, and runs in every session the entry loads in.
- `bun build --external @oh-my-pi/pi-coding-agent` keeps every subpath specifier as a literal in
  the bundle; confirm with `grep -o 'import("@oh-my-pi/pi-coding-agent[^"]*")' dist/*.js`.
- `pi.getActiveTools()` and the other action methods throw `Extension runtime not initialized`
  at factory time. Read the tool surface from a handler (`session_start`), and pass what a boot
  step needs into it from there.
- To learn an export's shape when no `.d.ts` is installed, write a throwaway extension and run it
  on the pinned binary: `omp models --no-extensions --extension probe.mjs --json >/dev/null` for
  factory-time imports (`Object.keys(mod)`, `String(fn)` prints the minified source, enough to read
  a signature and its return object), and `omp --mode rpc --no-ui --no-session --extension
  probe.mjs </dev/null` with a `session_start` handler for a call that needs a context or the tool
  surface. Declare what you then rely on in `packages/pi-shared/src/omp-host.d.ts`, one
  `declare module` block per specifier.
- `discoverAgents(cwd)` with no roots argument sees the agents of every `-e <plugin root>` the
  session was started with: the session's explicit extension roots are process state, so a pod's
  plugin agents (`/opt/legion/pi-legion/agents`) are found without `LEGION_PROMPT_ROOTS`.
- A workspace `.omp/config.yml` with `web_search:\n  enabled: false` removes `web_search` from
  `getActiveTools()` while `runSearchQuery` still answers, so a capability check reads the
  registration first and the call second.

## Evidence

LEGION-663's `packages/pi-legion/src/capability-report.ts` measures six live capabilities inside
the session. The first probe used `import(spec)` over a list of specifiers and every one failed
with `Cannot find package '@oh-my-pi/pi-coding-agent' imported from /tmp/omp-probe/probe-imports.mjs`;
the same list as literals resolved every module. A probe calling `pi.getActiveTools()` in the
factory failed to load with `Extension runtime not initialized. Action methods cannot be called
during…`; moved into `session_start` it listed the fifteen tools. `discoverAgents(cwd)` under
`-e /opt/legion/pi-envoy -e /opt/legion/pi-legion` returned the six plugin agents with
`searchedDirs` naming `/opt/legion/pi-legion/agents`, the same as with an explicit
`{explicit: [...], mode: "explicit-only"}` roots argument. `discoverMCPServers(cwd)` answered
`{manager, tools, errors, connectedServers, exaApiKeys}` and `runSearchQuery(...)`
`{content: [{type: "text", text}], details: {response: {provider, sources, requestId}}}`, read off
`String(fn)` and a live call, and both went into `omp-host.d.ts`.
