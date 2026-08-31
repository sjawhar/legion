# Pi Envoy Extension

Tracked Oh My Pi (`pi-*`) extension package for Envoy messaging.

## Overview

This package owns Pi-specific tool registration, direct NATS subscriptions, steering delivery,
and optional self-subscription registration. HTTP transport, tool metadata, envelope parsing, and
subject construction come from the Envoy core packages. Role claims are routed by the listener:
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
| Dispatch MCP mount | `.mcp.json`, `bin/dispatch-mcp-shim.ts` | Package-root mount OMP discovers for every session loading the package; release rewrites args to `dist/bin/dispatch-mcp-shim.js` (see docs/solutions/envoy/omp-extension-mcp-mounting.md) |
| Root session prompts | `roles/*.md` | Daemon `--append-system-prompt` sources; NOT OMP agents — `agents/` is scanned by OMP's agent discovery, which is why these live elsewhere |

## Critical conventions

- Register schemas through the injected `pi.zod`; OMP rejects schemas built from another Zod instance.
- Keep direct NATS subscription lifecycle and Pi steering delivery adapter-local. Inbound messages deliver as `steer` so they interrupt an in-flight turn; `triggerTurn` still wakes idle sessions.
- `envoy_list` must report the union of locally live and registry-persisted topics, with each topic marked `live`, `registry`, or `both`.
- Do not alter `~/.omp` from this package. The README documents the local developer symlink.
