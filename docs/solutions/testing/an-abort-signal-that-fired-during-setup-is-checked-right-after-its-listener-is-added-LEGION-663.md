---
title: "An abort signal that fired during setup is checked right after its listener is added"
category: testing
tags:
  - AbortSignal
  - teardown
  - budget
  - mcp
  - resource-leak
  - bun-test
date: 2026-10-10
status: active
module: packages/pi-legion
related_issues:
  - "LEGION-663"
  - "sjawhar/legion#1874"
---
# An abort signal that fired during setup is checked right after its listener is added

- An `AbortSignal` listener added after the signal aborted never fires. A check that acquires a
  resource across an `await` (a client set, a child process) and then adds `abort → tearDown`
  must follow the `addEventListener` with `if (signal.aborted) void tearDown();`, or a budget
  that expired during the acquisition leaves the resource running for the process's lifetime
  while the row already reads "did not finish". Make `tearDown` idempotent (`torn ??= …`) so the
  listener, the early check and the `finally` share one run.
- The covering test must abort **before** the acquisition resolves, not after: a fake whose
  `discoverX` resolves only once another check's signal has aborted (the signals abort together
  when the report settles) reaches the window; one that aborts during the later `wait` does not.
  Assert the teardown call order (`[disconnectAll, wait]`), not only that it was called.

## Evidence

LEGION-663's MCP check opens a second, short-lived client set per configured server
(`discoverMCPServers`) and tore it down in `finally` and on `abort`; the listener was attached
after `discoverMCPServers` resolved, so an 8 s budget spent in `loadMCPConfigs`/`discoverMCPServers`
left the worked repository's own server commands spawned for the session. The reviewer's finding 3
named the window; `59dac701` added the post-listener check and a test whose
`discoverMCPServers` resolves on another check's abort, which fails without the line.
