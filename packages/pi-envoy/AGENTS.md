# Pi Envoy Extension

Tracked Oh My Pi (`pi-*`) extension package for Envoy messaging.

## Overview

This package owns Pi-specific tool registration, direct NATS subscriptions, steering delivery,
and self-subscription registration for every session. HTTP transport, tool metadata, and subject
construction come from the Envoy core packages. `@legion/envoy-client/delivery` is the sole
inbound renderer: it produces a tolerant TOON block and never exposes raw envelope bytes. Role
claims are routed by the listener:
this extension receives a receipt-backed request on its direct agent subject instead of subscribing
to a role subject itself. The agent pump replies after it accepts the envelope, so the listener can
turn a claimed-but-deaf holder into a `delivery_failed` exception after two seconds.

## Where to look

| Task | Location | Notes |
| --- | --- | --- |
| OMP extension entries | `extensions/envoy.ts`, `extensions/legion.ts` | Load the package with OMP's `--extension` flag |
| Legion lifecycle modules | `src/legion/` | Budgets, spawn parsing, control directives, tools |
| Extension unit tests | `extensions/envoy.test.ts`, `extensions/legion.test.ts` | Mocked Pi and NATS surface |
| Shared HTTP/tool behavior | `../envoy-client/src/` | Do not duplicate it here |
| Event subjects | `../contracts/src/subject.ts` | Canonical subject construction |
| Dispatch tool | `extensions/envoy.ts` (the `registerTool` block), `@legion/envoy-client/dispatch-call` (`executeDispatch`), `@legion/envoy-client/dispatch-contract` (`DISPATCH_TOOL_JSON_SCHEMA`) | Native tool, gated on `resolveDispatchConfig`; reads session id/title from the tool context on every call and hands them to `executeDispatch`; `tool_result` auto-subscribes the session to the thread |
| Root session prompts | `roles/*.md` | Daemon `--append-system-prompt` sources; NOT OMP agents — `agents/` is scanned by OMP's agent discovery, which is why these live elsewhere |

## Critical conventions

- Register schemas through the injected `pi.zod`, or as plain JSON Schema (the path OMP's MCP tools take — `dispatch` serialises the shared contract's zod shape this way). Every field counts, not just the outer object: OMP's converter reads internals (`.ir`) only its own Zod produces, and a field from another Zod instance fails the whole extension load (`undefined is not an object (evaluating 'e.ir.desc')`). The shared tool contract therefore exposes argument shapes as builders over the caller's Zod (`spec.arguments(pi.zod)`), never as prebuilt schemas; `envoy.test.ts` proves every registered field came from the injected instance.
- Keep direct NATS subscription lifecycle and Pi steering delivery adapter-local. Inbound messages deliver as `steer` so they interrupt an in-flight turn; `triggerTurn` still wakes idle sessions.
- Render every inbound envelope through `renderInbound`. Keep its bounded 50-item `envoy_inbox` metadata-only; use the shared `envoy_role_get` transport operation for current role holders.
- `envoy_list` must report the union of locally live and registry-persisted topics, with each topic marked `live`, `registry`, or `both`.
- Do not alter `~/.omp` from this package. The README documents the local developer symlink.
